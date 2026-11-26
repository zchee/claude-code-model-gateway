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

package compactguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"time"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const (
	// overrideAborted and overrideError are the route status overrides of a stream that broke
	// and of a stream that ended without a usable summary.
	overrideAborted = "upstream_aborted"
	overrideError   = "upstream_error"

	// earlyEndBody answers an LM Studio stream that broke before its first data event.
	earlyEndBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`

	// readBufferSize bounds one upstream read.
	readBufferSize = 32 << 10

	// maxTimerMillis is the largest delay a JavaScript timer accepts; anything outside
	// [1, maxTimerMillis] fires after one millisecond.
	maxTimerMillis = 1<<31 - 1
)

var (
	errNoServiceContext    = errors.New("compactguard: runner has no service context")
	errNoNext              = errors.New("compactguard: request has no attempt dispatcher")
	errNilBody             = errors.New("compactguard: nil upstream body")
	errNoOverflowResponder = errors.New("compactguard: engine context overflow without an overflow responder")

	hopByHopResponseHeaders = []string{"Transfer-Encoding", "Connection", "Keep-Alive"}
)

// UsageCapture observes raw response bytes and corrected events, independently of delivery.
type UsageCapture interface {
	NoteResponseBytes(int64)
	ObserveEvent(jsonx.WireValue)
	Snapshot() map[string]any
	ResetUsage()
	SetResponseComplete(bool)
}

// Runner buffers eligible compaction streams and reports degradation to Stderr.
type Runner struct {
	// ServiceContext is set once when the worker builds the runner, not per request.
	// It bounds peeking and relay work; a nil context is rejected before dispatch.
	ServiceContext context.Context
	Settings       Settings
	Stderr         io.Writer
}

// Request describes a prepared proxy request. Next invokes the bound reply handler;
// its bool argument says whether the client has already received response headers.
// A nil response is a settled verdict; a non-nil response transfers body ownership.
//
// Serve takes its stream settings from Stream except HeadWritten, ClientDone and Usage:
// it tracks header commitment itself, learns about the client from Serve's ctx, and
// reports usage to Usage.
type Request struct {
	ProxyProvider  string
	PathOnly       string
	UpstreamOrigin string
	Body           []byte
	Next           func(context.Context, bool) (*upstream.Response, outcome.Result, error)
	Stream         sse.ProxyOptions
	Usage          UsageCapture
}

// Serve consumes one stream per attempt and returns the terminal response facts.
// The caller dispatches unguarded requests directly to their ordinary relay.
//
// The ctx parameter tracks client liveness. Once ctx is done, Serve starts no
// new attempt (before any attempt or during a retry wait), stops heartbeats,
// and writes nothing more to w; it does not end a peek or read already in
// flight. Once in-flight upstream work finishes on its own (or if no attempt
// was in flight), Serve returns with a nil error and Result.Cancelled false.
//
// Runner.ServiceContext bounds upstream peeking and reads. When it is done,
// Serve closes any in-flight peek or stream body exactly once, writes nothing
// more to w, and returns promptly with an error matching context.Canceled (or
// the service context's cause).
func (r Runner) Serve(ctx context.Context, w http.ResponseWriter, req Request) (outcome.Result, error) {
	if r.ServiceContext == nil {
		return outcome.Result{}, errNoServiceContext
	}
	if r.ServiceContext.Err() != nil {
		return outcome.Result{}, context.Cause(r.ServiceContext)
	}
	if ctx.Err() != nil {
		return outcome.Result{}, nil
	}
	if req.Next == nil {
		return outcome.Result{}, errNoNext
	}
	g := &guard{
		Runner: r,
		req:    req,
		ctx:    ctx,
		w:      w,
		rc:     http.NewResponseController(w),
	}
	if g.Stderr == nil {
		g.Stderr = io.Discard
	}
	return g.serve()
}

// guard holds the state of one guarded request across its attempts. Only the goroutine
// running Serve touches it.
type guard struct {
	Runner
	req Request
	ctx context.Context
	w   http.ResponseWriter
	rc  *http.ResponseController

	// headWritten records that the first relayed attempt committed the response status.
	headWritten bool
	// gone records that the client can no longer be written to: its context ended or a
	// write to it failed.
	gone bool
	// unflushed records bytes written since the last flush.
	unflushed bool
	// contacted records that any attempt invoked an upstream send.
	contacted bool
	// retries counts the attempts already discarded for a retry.
	retries int
	// observed holds the token counts of discarded attempts.
	observed outcome.Compaction
}

func (g *guard) serve() (outcome.Result, error) {
	for {
		if g.clientGone() {
			return g.departed(), nil
		}
		if g.ServiceContext.Err() != nil {
			return g.shutdown(0)
		}
		resp, res, err := g.req.Next(g.ctx, g.headWritten)
		// A later attempt that never reached the upstream does not undo an earlier contact.
		g.contacted = g.contacted || res.Contacted
		res.Contacted = g.contacted
		if resp == nil {
			if g.retries > 0 && res.Route.Compaction != nil {
				merged := withUsage(*res.Route.Compaction, mergeUsage(g.observed, g.snapshot()))
				res.Route.Compaction = &merged
			}
			return res, err
		}
		if err != nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return res, err
		}
		res, retry, err := g.attempt(resp)
		if err != nil || !retry {
			return res, err
		}
		timer := time.NewTimer(retryDelay(g.Settings.RetryDelayMillis))
		select {
		case <-timer.C:
		case <-g.ctx.Done():
			timer.Stop()
			return g.departed(), nil
		case <-g.ServiceContext.Done():
			timer.Stop()
			return g.shutdown(0)
		}
	}
}

// attempt relays one upstream stream into the guard buffer and settles it. It reports
// whether the attempt was discarded for a retry; otherwise the result is terminal.
func (g *guard) attempt(resp *upstream.Response) (outcome.Result, bool, error) {
	status := resp.StatusCode
	body := resp.Body
	if body == nil {
		return outcome.Result{Contacted: g.contacted, Status: status}, false, errNilBody
	}
	// The guard owns the body until the peek or the body reader takes it over, so a panic
	// before that hand-off (in OnPeek, say) still releases the upstream connection.
	handedOff := false
	defer func(body io.Closer) {
		if !handedOff {
			_ = body.Close()
		}
	}(body)
	opts := g.req.Stream
	if g.req.ProxyProvider == sse.LMStudioProvider && !g.headWritten {
		if opts.OnPeek != nil {
			opts.OnPeek()
		}
		// The peek owns the body from here on and closes it on every path that ends it.
		handedOff = true
		peeked, err := sse.PeekFirstEvent(g.ServiceContext, body, opts.FirstEventWait)
		switch {
		case err != nil && g.ServiceContext.Err() != nil:
			res, err := g.shutdown(status)
			return res, false, err
		case err != nil:
			return outcome.Result{Contacted: g.contacted, Status: status, StatusOverride: overrideAborted}, false, err
		case peeked.Overflow != nil:
			if opts.OnOverflow == nil {
				return outcome.Result{Contacted: g.contacted, Status: status}, false, errNoOverflowResponder
			}
			if g.clientGone() {
				// The responder still runs so the sentry latches and learns the limit, but the
				// client that left receives nothing.
				res, _ := opts.OnOverflow(discardWriter{header: http.Header{}}, status, *peeked.Overflow)
				g.contacted = g.contacted || res.Contacted
				return g.departed(), false, nil
			}
			res, err := opts.OnOverflow(g.w, status, *peeked.Overflow)
			res.Contacted = res.Contacted || g.contacted
			return res, false, err
		case peeked.EarlyEndOverride != "":
			if !g.clientGone() {
				g.w.Header().Set("Content-Type", "application/json")
				g.w.WriteHeader(http.StatusBadGateway)
				g.write([]byte(earlyEndBody))
			}
			return outcome.Result{Contacted: g.contacted, Status: status, StatusOverride: peeked.EarlyEndOverride}, false, nil
		}
		body = peeked.Body
	}

	handedOff = true
	rd := newBodyReader(body)
	defer func() { _ = rd.close() }()

	if !g.headWritten && !g.clientGone() {
		copyRelayHeaders(g.w.Header(), resp)
		g.w.WriteHeader(status)
		g.headWritten = true
	}
	hb := newHeartbeat(opts.HeartbeatInterval, g.gone)
	defer hb.stop()

	var a sse.Attempt
	transformer := sse.NewTransformer(sse.TransformerConfig{
		AdvertisedModel: opts.AdvertisedModel,
		BackendModel:    opts.BackendModel,
		Provider:        opts.Provider,
		FilterPlanTools: opts.FilterPlanTools,
		Observe: func(event jsonx.WireValue) {
			a.NoteEvent(event)
			// A guarded attempt is never an ordinary completed turn, even when it finishes.
			if opts.Sentry != nil {
				opts.Sentry.ObserveWire(opts.SessionKey, opts.SentryTarget, event, false)
			}
			if g.req.Usage != nil {
				g.req.Usage.ObserveEvent(event)
			}
		},
	})
	var (
		buffered      []byte
		bufferedBytes float64
		degraded      bool
	)
	emit := func(frame []byte) error {
		if degraded {
			g.write(frame)
			return nil
		}
		buffered = append(buffered, frame...)
		bufferedBytes += float64(len(frame))
		if bufferedBytes <= g.Settings.MaxBufferedBytes {
			return nil
		}
		// Far past any real summary: keep the turn alive instead of hoarding it, and say
		// plainly that retry cover is gone.
		_, _ = fmt.Fprintf(g.Stderr, "model-gateway: compaction response passed %s buffered bytes; streaming the remainder live without retry cover\n", numberString(g.Settings.MaxBufferedBytes))
		degraded = true
		g.write(buffered)
		buffered = nil
		return nil
	}

	clientDone := g.ctx.Done()
	var termErr error
	for termErr == nil {
		rd.request()
		select {
		case res := <-rd.results:
			rd.got(res)
			if len(res.data) > 0 {
				if g.req.Usage != nil {
					g.req.Usage.NoteResponseBytes(int64(len(res.data)))
				}
				hb.arm()
				_ = transformer.Feed(res.data, emit)
				g.flush()
			}
			termErr = res.err
		case <-hb.c():
			if g.clientGone() {
				hb.stop()
				continue
			}
			g.write([]byte(sse.HeartbeatComment))
			g.flush()
			if g.gone {
				hb.stop()
			} else {
				hb.arm()
			}
		case <-clientDone:
			clientDone = nil
			g.gone = true
			hb.stop()
		case <-g.ServiceContext.Done():
			_ = rd.close()
			res, err := g.shutdown(status)
			return res, false, err
		}
		// A failed client write or flush ends heartbeats for good; stop() also keeps a later
		// upstream chunk from re-arming the timer.
		if g.gone {
			hb.stop()
		}
	}
	_ = rd.close()
	hb.stop()
	override := ""
	if !errors.Is(termErr, io.EOF) {
		if g.ServiceContext.Err() != nil {
			res, err := g.shutdown(status)
			return res, false, err
		}
		override = overrideAborted
	}

	_ = transformer.End(emit)
	if g.clientGone() {
		return g.departed(), false, nil
	}
	deliverable := a.Deliverable()
	if !deliverable && !a.Fatal && !degraded && float64(g.retries) < g.Settings.Retries {
		g.observed = mergeUsage(g.observed, g.snapshot())
		if g.req.Usage != nil {
			g.req.Usage.ResetUsage()
		}
		g.retries++
		return outcome.Result{}, true, nil
	}

	c := classify(&a, status, override)
	done := completed(c.Outcome)
	usage := g.observed
	if done {
		// A delivered summary reports only its own attempt's usage.
		usage = outcome.Compaction{}
	}
	c = withUsage(c, mergeUsage(usage, g.snapshot()))
	if g.req.Usage != nil {
		g.req.Usage.SetResponseComplete(done)
	}
	if override == "" && !done {
		override = overrideError
	}

	g.write(buffered)
	// The model finished but its terminal frame was lost in transit: close the turn the
	// client is holding open rather than re-inferring the largest request of the session.
	if deliverable && !a.Terminal {
		g.write([]byte(sse.SynthesizedMessageStop))
	}
	if !deliverable && !a.SawError {
		g.write(errorFrame("api_error", fmt.Sprintf("model-gateway: the %s compaction stream ended without a completed generation after %d attempt(s); the summary above is incomplete", proxymodels.ProviderLabel(g.req.ProxyProvider), g.retries+1)))
	}
	g.flush()
	return outcome.Result{
		Route:          outcome.Route{Compaction: &c},
		Status:         status,
		StatusOverride: override,
		Contacted:      g.contacted,
	}, false, nil
}

// snapshot returns the capture's usage for the current attempt, or nil without a capture.
func (g *guard) snapshot() map[string]any {
	if g.req.Usage == nil {
		return nil
	}
	return g.req.Usage.Snapshot()
}

// departed is the result for a client that left: no status, since the worker root decides
// the cancellation and publishes no terminal route for it.
func (g *guard) departed() outcome.Result {
	return outcome.Result{Contacted: g.contacted}
}

// shutdown is the result of service shutdown during an attempt or a retry wait. A client
// still holding a committed response gets an abort rather than a clean end.
func (g *guard) shutdown(status int) (outcome.Result, error) {
	res := outcome.Result{Contacted: g.contacted, Status: status}
	if status != 0 {
		res.StatusOverride = overrideAborted
	}
	cause := context.Cause(g.ServiceContext)
	if g.headWritten && !g.clientGone() {
		return res, fmt.Errorf("%w: %w", http.ErrAbortHandler, cause)
	}
	return res, cause
}

// clientGone reports whether the client is gone, checking its context without blocking so
// that no write races a departure the guard has not yet seen.
func (g *guard) clientGone() bool {
	if !g.gone && g.ctx.Err() != nil {
		g.gone = true
	}
	return g.gone
}

// write sends b to the client unless the client is gone. A response status that allows no
// body discards it without marking the client gone.
func (g *guard) write(b []byte) {
	if len(b) == 0 || g.clientGone() {
		return
	}
	if _, err := g.w.Write(b); err != nil {
		if !errors.Is(err, http.ErrBodyNotAllowed) {
			g.gone = true
		}
		return
	}
	g.unflushed = true
}

// flush pushes written bytes to the client; nothing is flushed when nothing was written.
func (g *guard) flush() {
	if !g.unflushed || g.gone {
		return
	}
	g.unflushed = false
	if err := g.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		g.gone = true
	}
}

// discardWriter is the response writer handed to a responder whose client has gone: it
// accepts everything and delivers nothing.
type discardWriter struct {
	header http.Header
}

func (d discardWriter) Header() http.Header { return d.header }

func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }

func (discardWriter) WriteHeader(int) {}

// retryDelay converts the configured retry delay as a JavaScript timer does: a value outside
// [1, maxTimerMillis] milliseconds fires after one millisecond, and fractions are dropped.
func retryDelay(millis float64) time.Duration {
	if !(millis >= 1 && millis <= maxTimerMillis) {
		millis = 1
	}
	return time.Duration(math.Trunc(millis)) * time.Millisecond
}

// copyRelayHeaders copies the client-visible headers of the first attempt into dst without
// mutating either upstream view: hop-by-hop fields and Content-Length are dropped, since the
// guard rewrites the body. With no upstream Content-Type, content sniffing is suppressed so
// the client sees what the upstream sent.
func copyRelayHeaders(dst http.Header, resp *upstream.Response) {
	src := resp.RelayHeader
	if src == nil {
		src = resp.Header
	}
	for name, values := range src {
		key := http.CanonicalHeaderKey(name)
		if slices.Contains(hopByHopResponseHeaders, key) || key == "Content-Length" {
			continue
		}
		dst[key] = slices.Clone(values)
	}
	if _, ok := dst["Content-Type"]; !ok {
		dst["Content-Type"] = nil
	}
}

// errorFrame is an SSE error event with JSON.stringify's encoding of the error object.
func errorFrame(typ, message string) []byte {
	event := jsonx.WireValue{Kind: '{', Members: []jsonx.Member{
		{Name: units("type"), Value: text("error")},
		{Name: units("error"), Value: jsonx.WireValue{Kind: '{', Members: []jsonx.Member{
			{Name: units("type"), Value: text(typ)},
			{Name: units("message"), Value: text(message)},
		}}},
	}}
	data, err := jsonx.EncodeWire(event)
	if err != nil {
		// Strings and objects always encode; this branch is unreachable.
		data = []byte(`{"type":"error","error":{"type":"api_error","message":"model-gateway: compaction stream failed"}}`)
	}
	frame := append([]byte("event: error\ndata: "), data...)
	return append(frame, "\n\n"...)
}

// numberString is JavaScript's String(n) for a finite number.
func numberString(n float64) string {
	b, err := jsonx.EncodeWire(jsonx.WireValue{Kind: '0', Number: n})
	if err != nil {
		return "NaN"
	}
	return string(b)
}

func units(s string) []uint16 { return utf16.Encode([]rune(s)) }

func text(s string) jsonx.WireValue { return jsonx.WireValue{Kind: '"', Text: units(s)} }

// heartbeat is one attempt's idle timer: it fires after interval without upstream data.
type heartbeat struct {
	interval time.Duration
	timer    *time.Timer
}

// newHeartbeat arms a heartbeat. A zero interval is the relay default, a negative one disables
// heartbeats, and none is armed for a client already gone.
func newHeartbeat(interval time.Duration, gone bool) *heartbeat {
	if interval == 0 {
		interval = sse.DefaultHeartbeatInterval
	}
	h := &heartbeat{interval: interval}
	if interval > 0 && !gone {
		h.timer = time.NewTimer(interval)
	}
	return h
}

// c returns the timer's channel, or nil when no heartbeat is armed.
func (h *heartbeat) c() <-chan time.Time {
	if h.timer == nil {
		return nil
	}
	return h.timer.C
}

// arm restarts the idle interval after upstream data or a heartbeat.
func (h *heartbeat) arm() {
	if h.timer != nil {
		h.timer.Reset(h.interval)
	}
}

// stop releases the timer; the heartbeat stays disarmed.
func (h *heartbeat) stop() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
}

// readResult is the outcome of one upstream read: the filled part of the reader's buffer.
type readResult struct {
	data []byte
	err  error
}

// bodyReader owns an upstream body and reads it on one goroutine, one read per request, so
// the guard can wait for data, its heartbeat, the client and the service together. Only the
// goroutine running Serve calls its methods; close joins the reading goroutine.
type bodyReader struct {
	body    io.ReadCloser
	buf     []byte
	reqs    chan struct{}
	results chan readResult
	quit    chan struct{}
	exited  chan struct{}

	// pending records a read in flight; finished records that a read returned an error and
	// the goroutine has exited; closed records that close ran.
	pending, finished, closed bool
}

func newBodyReader(body io.ReadCloser) *bodyReader {
	r := &bodyReader{
		body:    body,
		buf:     make([]byte, readBufferSize),
		reqs:    make(chan struct{}, 1),
		results: make(chan readResult),
		quit:    make(chan struct{}),
		exited:  make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *bodyReader) run() {
	defer close(r.exited)
	for {
		select {
		case <-r.reqs:
		case <-r.quit:
			return
		}
		n, err := r.body.Read(r.buf)
		select {
		case r.results <- readResult{data: r.buf[:n], err: err}:
		case <-r.quit:
			return
		}
		if err != nil {
			return
		}
	}
}

// request starts one read unless a read is in flight or the body has ended. The data of the
// previous result must not be used after this call.
func (r *bodyReader) request() {
	if r.pending || r.finished {
		return
	}
	r.reqs <- struct{}{}
	r.pending = true
}

// got records a result received from r.results.
func (r *bodyReader) got(res readResult) {
	r.pending = false
	if res.err != nil {
		r.finished = true
	}
}

// close closes the body once, which unblocks a read in flight, and waits for the reading
// goroutine to exit.
func (r *bodyReader) close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.body.Close()
	close(r.quit)
	<-r.exited
	return err
}
