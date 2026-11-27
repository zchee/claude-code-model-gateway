// Copyright 2026 The claude-code-model-gateway Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sse

import (
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// earlyEndBody answers an LM Studio stream that broke before its first data event.
const earlyEndBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`

var (
	errNilResponse          = errors.New("sse: nil upstream response")
	errNoOverflowResponder  = errors.New("sse: engine context overflow without an overflow responder")
	errSideBranchDone       = errors.New("sse: side-branch observation ended")
	errSideBranchAbandoned  = errors.New("sse: side-branch observation abandoned")
	errUnsupportedEncoding  = errors.New("sse: unsupported content encoding")
	messageDeltaType        = jsonx.WireValue{Kind: '"', Text: units("message_delta")}
	typeName, usageName     = units("type"), units("usage")
	hopByHopResponseHeaders = []string{"Transfer-Encoding", "Connection", "Keep-Alive"}
)

// copyRelayHeaders copies the client-visible upstream headers into dst without mutating either
// upstream view. With no Content-Type upstream, automatic content sniffing is suppressed so the
// client sees what the upstream sent.
func copyRelayHeaders(dst http.Header, resp *upstream.Response, dropContentLength bool) {
	src := resp.RelayHeader
	if src == nil {
		src = resp.Header
	}
	for name, values := range src {
		key := http.CanonicalHeaderKey(name)
		if slices.Contains(hopByHopResponseHeaders, key) || dropContentLength && key == "Content-Length" {
			continue
		}
		dst[key] = slices.Clone(values)
	}
	if _, ok := dst["Content-Type"]; !ok {
		dst["Content-Type"] = nil
	}
}

// relayHeader returns a response header value as the source runtime's parsed header object
// holds it. A given RelayHeader is already normalized and is read as it is, every value it holds
// in order. On the raw Header fallback the source runtime's rule for repeated fields applies:
// Content-Type keeps its first value and other fields, such as Content-Encoding, are joined
// with ", ".
func relayHeader(resp *upstream.Response, name string) string {
	if resp.RelayHeader != nil {
		return strings.Join(resp.RelayHeader.Values(name), ", ")
	}
	if http.CanonicalHeaderKey(name) == "Content-Type" {
		return resp.Header.Get(name)
	}
	return strings.Join(resp.Header.Values(name), ", ")
}

// clientWriter is the only path from a relay to the client. Each WriteHeader, Write and flush
// checks, without blocking, that the client is still there immediately before it runs, so no output
// races a disconnect that a caller has not yet seen; an operation already running is not retracted.
// Once the client is gone, by its done signal or by a failed write or flush, it keeps nothing:
// Header returns a map of its own, WriteHeader and Flush do nothing, and Write reports the bytes
// as written. It deliberately has no Unwrap, so a ResponseController cannot reach past it.
type clientWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	done    <-chan struct{}
	gone    bool
	discard http.Header
}

// newClientWriter gates w on done; a nil done never reports a departure by itself.
func newClientWriter(w http.ResponseWriter, done <-chan struct{}) *clientWriter {
	return &clientWriter{w: w, rc: http.NewResponseController(w), done: done}
}

// nothingWriter returns a writer that keeps nothing and reaches no client.
func nothingWriter() *clientWriter {
	return &clientWriter{gone: true}
}

// clientGone reports whether the client is gone, latching a closed done signal.
func (c *clientWriter) clientGone() bool {
	if !c.gone && closed(c.done) {
		c.gone = true
	}
	return c.gone
}

// Header returns the client's header map while the client is there, and a private map after.
func (c *clientWriter) Header() http.Header {
	if c.clientGone() {
		if c.discard == nil {
			c.discard = make(http.Header)
		}
		return c.discard
	}
	return c.w.Header()
}

// WriteHeader sends the status line unless the client is gone.
func (c *clientWriter) WriteHeader(code int) {
	if c.clientGone() {
		return
	}
	c.w.WriteHeader(code)
}

// Write sends b unless the client is gone. A failed write marks the client gone, except one
// refused because the response status allows no body.
func (c *clientWriter) Write(b []byte) (int, error) {
	if c.clientGone() {
		return len(b), nil
	}
	n, err := c.w.Write(b)
	if err != nil && !errors.Is(err, http.ErrBodyNotAllowed) {
		c.gone = true
	}
	return n, err
}

// FlushError pushes written bytes to the client unless it is gone. A failed flush marks the client
// gone; a client writer that cannot flush reports http.ErrNotSupported and stays usable.
func (c *clientWriter) FlushError() error {
	if c.clientGone() {
		return nil
	}
	err := c.rc.Flush()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.gone = true
	}
	return err
}

// Flush is FlushError for callers that assert http.Flusher.
func (c *clientWriter) Flush() { _ = c.FlushError() }

// heartbeatInterval resolves a configured interval: zero is the default, negative disables.
func heartbeatInterval(d time.Duration) time.Duration {
	switch {
	case d == 0:
		return DefaultHeartbeatInterval
	case d < 0:
		return 0
	}
	return d
}

// streamer writes one relayed response through its client writer and keeps its heartbeat. Once
// the client is gone the writer keeps nothing and the heartbeat stays stopped.
type streamer struct {
	out        *clientWriter
	clientDone <-chan struct{}
	unflushed  bool
	interval   time.Duration
	heartbeat  *time.Timer
}

// newStreamer starts the heartbeat only for a client that is still there.
func newStreamer(w http.ResponseWriter, clientDone <-chan struct{}, interval time.Duration) *streamer {
	s := &streamer{out: newClientWriter(w, clientDone), clientDone: clientDone, interval: interval}
	if s.clientGone() {
		return s
	}
	if interval > 0 {
		s.heartbeat = time.NewTimer(interval)
	}
	return s
}

// heartbeatC returns the heartbeat timer's channel, or nil when no heartbeat is armed.
func (s *streamer) heartbeatC() <-chan time.Time {
	if s.heartbeat == nil {
		return nil
	}
	return s.heartbeat.C
}

// arm restarts the idle interval after upstream activity or a heartbeat.
func (s *streamer) arm() {
	if s.heartbeat != nil {
		s.heartbeat.Reset(s.interval)
	}
}

// stop releases the heartbeat timer.
func (s *streamer) stop() {
	if s.heartbeat != nil {
		s.heartbeat.Stop()
		s.heartbeat = nil
	}
}

// markGone records that the client can no longer be written to.
func (s *streamer) markGone() {
	s.out.gone = true
	s.clientDone = nil
	s.stop()
}

// closed reports, without blocking, whether done has been closed; a nil channel never is.
func closed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// clientGone reports whether the client is gone and, if so, stops the heartbeat and the wait on
// ClientDone.
func (s *streamer) clientGone() bool {
	if !s.out.clientGone() {
		return false
	}
	s.markGone()
	return true
}

// head copies the relay headers and writes the status line. Nothing is prepared for a client
// that is already gone; each step stays gated for a client that leaves during the copy.
func (s *streamer) head(resp *upstream.Response, status int, dropContentLength bool) {
	if s.clientGone() {
		return
	}
	copyRelayHeaders(s.out.Header(), resp, dropContentLength)
	s.out.WriteHeader(status)
	s.clientGone()
}

// write sends b to the client unless the client is gone. A response status that allows no body
// discards it without marking the client gone.
func (s *streamer) write(b []byte) {
	if len(b) == 0 || s.clientGone() {
		return
	}
	_, err := s.out.Write(b)
	if !s.clientGone() && err == nil {
		s.unflushed = true
	}
}

// flush pushes written bytes to the client; nothing is flushed before the first write.
func (s *streamer) flush() {
	if !s.unflushed {
		return
	}
	s.unflushed = false
	_ = s.out.FlushError()
	s.clientGone()
}

// beat writes one heartbeat comment and re-arms the interval while the client is there.
func (s *streamer) beat() {
	if s.clientGone() {
		return
	}
	s.write([]byte(HeartbeatComment))
	s.flush()
	if !s.clientGone() {
		s.arm()
	}
}

// shutdownBeforeHead reports a service shutdown seen before the relay wrote the response head:
// nothing reached the client, so the context's error is returned unless the client is gone.
func shutdownBeforeHead(ctx context.Context, status int, clientGone bool) (outcome.Result, error) {
	res := outcome.Result{Status: status, StatusOverride: overrideAborted}
	if clientGone {
		return res, nil
	}
	return res, ctx.Err()
}

// abortResult reports a relay ended by service shutdown: nil error once the client is gone,
// otherwise an abort of the committed response carrying the shutdown cause.
func abortResult(ctx context.Context, status int, clientGone bool) (outcome.Result, error) {
	res := outcome.Result{Status: status, StatusOverride: overrideAborted}
	if clientGone {
		return res, nil
	}
	return res, fmt.Errorf("%w: %w", http.ErrAbortHandler, context.Cause(ctx))
}

// brokenResult reports an upstream body that failed after headers were committed.
func brokenResult(status int, clientGone bool) (outcome.Result, error) {
	res := outcome.Result{Status: status, StatusOverride: overrideAborted}
	if clientGone {
		return res, nil
	}
	return res, http.ErrAbortHandler
}

// RelayProxy relays a 2xx proxy SSE response to w with model rewriting, plan-tool filtering,
// heartbeats, sentry observation, and (for eligible LM Studio streams) first-event overflow peeking.
// The ctx parameter is the service-owned lifetime context, not the client request context.
// When opts.ClientDone closes or a write to w fails, RelayProxy stops heartbeats and writes nothing
// more to w, then drains the finite upstream tail (observing usage and sentry) until EOF or ctx
// cancellation. When ctx ends after the client is gone, RelayProxy closes resp.Body and returns a
// nil error (neither ctx.Err() nor http.ErrAbortHandler). It never sets outcome.Result.Cancelled.
// It copies relay headers from resp.RelayHeader (or resp.Header if RelayHeader is nil) minus
// hop-by-hop and Content-Length headers without mutating resp.Header or resp.RelayHeader.
// It takes ownership of resp.Body and closes it before returning.
// Every WriteHeader, Write and Flush on w, including the early-end 502 and those of OnOverflow,
// first checks ClientDone without blocking and is skipped once the client is gone; one already
// running is not retracted. A client that is already gone when the head would be written gets
// nothing, not even the head (no early-end 502 either), and no heartbeat. A ctx that has ended
// before RelayProxy writes the head, including one that ended while the first-event peek decided,
// closes resp.Body and returns ctx.Err() (nil once the client is gone); with HeadWritten it returns
// the abort described next. ctx is checked again before every upstream read, and once more at the
// clean end of the body before the stream is finalized: a ctx that ended while the last chunk was
// written gets no additional closing events and no synthesized stop, only the shutdown abort; a stop
// the upstream sent in that last chunk has already been forwarded.
// If the upstream breaks after response headers are committed, RelayProxy returns the terminal
// outcome facts alongside http.ErrAbortHandler rather than panicking.
// When the first-event peek finds an engine overflow, OnOverflow is called even if the client is
// already gone or ctx has ended, because the refusal must still reach the sentry. Its writer gates
// each operation on the client as above, so for a client already gone no head, body byte or flush
// reaches w. After a ctx that ended during the peek, OnOverflow gets a writer that keeps nothing,
// its result is dropped, and RelayProxy returns as for any shutdown before the head.
func RelayProxy(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, opts ProxyOptions) (outcome.Result, error) {
	if resp == nil || resp.Body == nil {
		return outcome.Result{}, errNilResponse
	}
	status := resp.StatusCode
	body := newAsyncReader(resp.Body)
	defer func() { _ = body.close() }()

	var prefix []byte
	var termErr error
	if opts.Provider == LMStudioProvider && !opts.HeadWritten {
		if opts.OnPeek != nil {
			opts.OnPeek()
		}
		peeked, err := peekFirst(ctx, body, opts.FirstEventWait)
		if err != nil {
			return shutdownBeforeHead(ctx, status, closed(opts.ClientDone))
		}
		// The service may have ended while the peek decided, for example by closing the body.
		if ctx.Err() != nil {
			_ = body.close()
			if peeked.overflow != nil && opts.OnOverflow != nil {
				_, _ = opts.OnOverflow(nothingWriter(), status, *peeked.overflow)
			}
			return shutdownBeforeHead(ctx, status, closed(opts.ClientDone))
		}
		switch {
		case peeked.overflow != nil:
			if opts.OnOverflow == nil {
				return outcome.Result{Status: status}, errNoOverflowResponder
			}
			return opts.OnOverflow(newClientWriter(w, opts.ClientDone), status, *peeked.overflow)
		case peeked.earlyEnd != "":
			out := newClientWriter(w, opts.ClientDone)
			out.Header().Set("Content-Type", "application/json")
			out.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(out, earlyEndBody)
			return outcome.Result{Status: status, StatusOverride: peeked.earlyEnd}, nil
		}
		prefix, termErr = peeked.prefix, peeked.termErr
	}

	s := newStreamer(w, opts.ClientDone, heartbeatInterval(opts.HeartbeatInterval))
	defer s.stop()
	if ctx.Err() != nil {
		_ = body.close()
		if opts.HeadWritten {
			return abortResult(ctx, status, s.clientGone())
		}
		return shutdownBeforeHead(ctx, status, s.clientGone())
	}
	if !opts.HeadWritten {
		s.head(resp, status, true)
	}

	var attempt Attempt
	transformer := NewTransformer(TransformerConfig{
		AdvertisedModel: opts.AdvertisedModel,
		BackendModel:    opts.BackendModel,
		Provider:        opts.Provider,
		FilterPlanTools: opts.FilterPlanTools,
		Observe: func(event jsonx.WireValue) {
			attempt.NoteEvent(event)
			if opts.Sentry != nil {
				opts.Sentry.ObserveWire(opts.SessionKey, opts.SentryTarget, event, false)
			}
			if opts.Usage != nil {
				opts.Usage.ObserveEvent(event)
			}
		},
	})
	var out []byte
	emit := func(frame []byte) error {
		out = append(out, frame...)
		return nil
	}
	relay := func(chunk []byte) {
		if opts.Usage != nil {
			opts.Usage.NoteResponseBytes(int64(len(chunk)))
		}
		s.arm()
		out = out[:0]
		_ = transformer.Feed(chunk, emit)
		s.write(out)
		s.flush()
	}

	if len(prefix) > 0 {
		relay(prefix)
	}
	buf := make([]byte, readBufferSize)
	for termErr == nil {
		if ctx.Err() != nil {
			_ = body.close()
			return abortResult(ctx, status, s.clientGone())
		}
		body.request(buf)
		select {
		case res := <-body.results:
			body.got(res)
			if len(res.data) > 0 {
				relay(res.data)
			}
			termErr = res.err
		case <-s.heartbeatC():
			s.beat()
		case <-s.clientDone:
			s.markGone()
		case <-ctx.Done():
			_ = body.close()
			return abortResult(ctx, status, s.clientGone())
		}
	}
	if !errors.Is(termErr, io.EOF) {
		_ = body.close()
		if ctx.Err() != nil {
			return abortResult(ctx, status, s.clientGone())
		}
		return brokenResult(status, s.clientGone())
	}
	// The last read may have carried data and the end of the body together, and the service may
	// have ended while that data was written: such a stream is not finalized as a clean one.
	if ctx.Err() != nil {
		_ = body.close()
		return abortResult(ctx, status, s.clientGone())
	}

	out = out[:0]
	_ = transformer.End(emit)
	s.write(out)
	deliverable := attempt.Deliverable()
	if deliverable && opts.Sentry != nil {
		completed := jsonx.WireValue{Kind: '{', Members: []jsonx.Member{{Name: typeName, Value: messageDeltaType}}}
		if attempt.MergedUsage.Kind != 0 {
			completed.Members = append(completed.Members, jsonx.Member{Name: usageName, Value: attempt.MergedUsage})
		}
		opts.Sentry.ObserveWire(opts.SessionKey, opts.SentryTarget, completed, !opts.IsCompaction)
	}
	if deliverable && !attempt.Terminal {
		s.write([]byte(SynthesizedMessageStop))
	}
	s.flush()
	res := outcome.Result{Status: status}
	if !deliverable {
		res.StatusOverride = "upstream_error"
	}
	return res, nil
}

// passthroughObserver observes an untouched response on the side without changing its bytes.
type passthroughObserver struct {
	usage  UsageCapture
	mode   observeMode
	events *EventObserver
	side   *sideDecoder
	ended  bool
}

type observeMode int

const (
	observeNothing observeMode = iota
	observeEvents              // 2xx SSE: count raw bytes, decode events on the side.
	observeChunks              // other 2xx: the capture keeps and counts the body.
	observeBytes               // non-2xx: count raw bytes only.
)

func newPassthroughObserver(resp *upstream.Response, opts PassthroughOptions) *passthroughObserver {
	o := &passthroughObserver{usage: opts.Usage}
	successful := resp.StatusCode >= 200 && resp.StatusCode < 300
	observeSentry := opts.ProxyProvider != "" && opts.Sentry != nil && opts.SessionKey != ""
	switch {
	case isEventStream(resp) && (observeSentry || opts.Usage != nil):
		o.mode = observeEvents
		o.events = NewEventObserver(opts.MaxObserveBytes, func(event jsonx.WireValue) {
			if observeSentry {
				opts.Sentry.ObserveWire(opts.SessionKey, opts.SentryTarget, event, false)
			}
			if opts.Usage != nil {
				opts.Usage.ObserveEvent(event)
			}
		}, o.markOverflow)
		encoding := strings.ToLower(strings.TrimSpace(relayHeader(resp, "Content-Encoding")))
		if encoding == "gzip" || encoding == "deflate" || encoding == "br" {
			o.side = startSideDecoder(encoding, o.events, o.markOverflow)
		}
	case successful && opts.Usage != nil:
		o.mode = observeChunks
	case opts.Usage != nil:
		o.mode = observeBytes
	}
	return o
}

func (o *passthroughObserver) markOverflow() {
	if o.usage != nil {
		o.usage.MarkOverflow()
	}
}

// observe accounts one raw chunk before it is written to the client.
func (o *passthroughObserver) observe(chunk []byte) {
	switch o.mode {
	case observeEvents:
		if o.usage != nil {
			o.usage.NoteResponseBytes(int64(len(chunk)))
		}
		if o.side != nil {
			o.side.write(chunk)
		} else {
			o.events.Write(chunk)
		}
	case observeChunks:
		o.usage.ObserveChunk(chunk)
	case observeBytes:
		o.usage.NoteResponseBytes(int64(len(chunk)))
	}
}

// finish completes observation of a body that ended cleanly and waits for the side branch.
func (o *passthroughObserver) finish() {
	if o.ended {
		return
	}
	o.ended = true
	switch {
	case o.side != nil:
		o.side.finish()
	case o.events != nil:
		o.events.End()
	}
}

// abandon stops observation of a body that did not end cleanly, reporting nothing further.
func (o *passthroughObserver) abandon() {
	if o.ended {
		return
	}
	o.ended = true
	if o.side != nil {
		o.side.abandon()
	}
}

// sideDecoder decompresses a copy of the relayed bytes on its own goroutine for observation.
type sideDecoder struct {
	pw        *io.PipeWriter
	done      chan struct{}
	abandoned atomic.Bool
}

func startSideDecoder(encoding string, events *EventObserver, onFailure func()) *sideDecoder {
	pr, pw := io.Pipe()
	d := &sideDecoder{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		err := decodeInto(encoding, pr, events)
		switch {
		case err == nil:
			events.End()
		case !d.abandoned.Load():
			onFailure()
		}
		// Later writes fail at once instead of waiting for a reader that has gone.
		_ = pr.CloseWithError(errSideBranchDone)
	}()
	return d
}

// decodeInto feeds the decompressed stream to events until the compressed stream ends. An empty
// compressed body is a decode failure for every encoding, as the source runtime's decompressors
// report it ("unexpected end of file"), so it marks an overflow like any other failure.
func decodeInto(encoding string, r io.Reader, events *EventObserver) error {
	var decoded io.Reader
	switch encoding {
	case "gzip":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		decoded = zr
	case "deflate":
		zr, err := zlib.NewReader(r)
		if err != nil {
			return err
		}
		defer func() { _ = zr.Close() }()
		decoded = zr
	case "br":
		decoded = brotli.NewReader(r)
	default:
		return errUnsupportedEncoding
	}
	buf := make([]byte, readBufferSize)
	for {
		n, err := decoded.Read(buf)
		if n > 0 {
			events.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// write hands one raw chunk to the decoder; after a decode failure it is discarded.
func (d *sideDecoder) write(chunk []byte) { _, _ = d.pw.Write(chunk) }

// finish ends the compressed input and waits for the decoder to report its outcome.
func (d *sideDecoder) finish() {
	_ = d.pw.Close()
	<-d.done
}

// abandon stops the decoder without reporting a failure and waits for it to exit.
func (d *sideDecoder) abandon() {
	d.abandoned.Store(true)
	_ = d.pw.CloseWithError(errSideBranchAbandoned)
	<-d.done
}

// RelayPassthrough forwards upstream status, relay headers minus hop-by-hop headers, and raw body bytes
// unchanged, flushing each chunk. The ctx parameter is the service-owned lifetime context, not the
// client request context. It copies headers from resp.RelayHeader (or resp.Header if RelayHeader is nil)
// without mutating resp.Header or resp.RelayHeader, takes ownership of resp.Body, and closes it before
// returning. For 2xx SSE responses with Usage or eligible proxy Sentry set, it decodes gzip, deflate,
// or br on a side branch subject to MaxObserveBytes without altering delivered bytes. When
// opts.ClientDone closes or a write to w fails, RelayPassthrough stops heartbeats (if enabled) and
// writes nothing more to w (each WriteHeader, Write and Flush first checks ClientDone without
// blocking); Anthropic passthrough (ProxyProvider == "") never sends heartbeats, never
// updates sentry state, and pauses reading resp.Body for back-pressure without eagerly closing the
// provider until ctx is canceled. When ctx ends after the client is gone, RelayPassthrough closes
// resp.Body and returns a nil error (neither ctx.Err() nor http.ErrAbortHandler). RelayPassthrough
// never sets outcome.Result.Cancelled. A client that is already gone on entry gets nothing, not even
// the head, no heartbeat and no upstream read. A ctx that has ended before the head is written closes
// resp.Body and returns ctx.Err() (nil once the client is gone); ctx is checked again before every
// upstream read, and once more when the body has ended, before anything else is decided: a ctx
// that ended by then (for example while the last chunk was written) abandons observation and
// returns the shutdown abort, never a clean status, also when a chunk was discarded. With ctx
// live, at a clean end of the body observation finishes whenever every chunk read was observed,
// even when the client left after the last one; it is abandoned only when a chunk that arrived
// after the client left was discarded. If the upstream breaks after response headers are
// committed, RelayPassthrough returns the terminal outcome facts alongside http.ErrAbortHandler
// rather than panicking.
func RelayPassthrough(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, opts PassthroughOptions) (outcome.Result, error) {
	if resp == nil || resp.Body == nil {
		return outcome.Result{}, errNilResponse
	}
	status := resp.StatusCode
	body := newAsyncReader(resp.Body)
	defer func() { _ = body.close() }()

	var interval time.Duration
	if opts.ProxyProvider != "" && isEventStream(resp) {
		interval = heartbeatInterval(opts.HeartbeatInterval)
	}
	s := newStreamer(w, opts.ClientDone, interval)
	defer s.stop()
	if ctx.Err() != nil {
		_ = body.close()
		return shutdownBeforeHead(ctx, status, s.clientGone())
	}
	s.head(resp, status, false)

	observer := newPassthroughObserver(resp, opts)
	defer observer.abandon()

	buf := make([]byte, readBufferSize)
	var termErr error
	// discarded records a chunk that arrived after the client left and was never observed.
	discarded := false
	for termErr == nil {
		if ctx.Err() != nil {
			observer.abandon()
			_ = body.close()
			return abortResult(ctx, status, s.clientGone())
		}
		// A client that is gone gets no further reads: the provider is held by back-pressure.
		if !s.clientGone() {
			body.request(buf)
		}
		select {
		case res := <-body.results:
			body.got(res)
			termErr = res.err
			if len(res.data) == 0 {
				continue
			}
			if s.clientGone() {
				discarded = true
				continue
			}
			s.arm()
			observer.observe(res.data)
			s.write(res.data)
			s.flush()
		case <-s.heartbeatC():
			s.beat()
		case <-s.clientDone:
			s.markGone()
		case <-ctx.Done():
			observer.abandon()
			_ = body.close()
			return abortResult(ctx, status, s.clientGone())
		}
	}
	// The last read may have carried data and the end of the body together, and the service may
	// have ended while that data was written or discarded: such a stream does not end as a clean one.
	if ctx.Err() != nil {
		observer.abandon()
		_ = body.close()
		return abortResult(ctx, status, s.clientGone())
	}
	if !errors.Is(termErr, io.EOF) || discarded {
		observer.abandon()
		_ = body.close()
		if errors.Is(termErr, io.EOF) {
			return outcome.Result{Status: status}, nil
		}
		return brokenResult(status, s.clientGone())
	}
	observer.finish()
	return outcome.Result{Status: status}, nil
}

// isEventStream reports whether resp is a successful SSE response.
func isEventStream(resp *upstream.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		strings.Contains(strings.ToLower(relayHeader(resp, "Content-Type")), "text/event-stream")
}
