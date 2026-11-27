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

package usage

import (
	"bytes"
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// exportTimeout bounds one collector post, including draining its reply. Refusing a reply that
// switched protocols closes its connection outside this bound (see upstream.RefuseSwitch).
const exportTimeout = 500 * time.Millisecond

// Emitter sends counts-only records to the configured loopback collector.
type Emitter struct {
	builder  *Builder
	endpoint *url.URL
	maxBytes int
	client   *http.Client
	// toolTTL (milliseconds) and toolLimit bound the emitter's own tool-result context snapshots,
	// which are independent of the builder's snapshots for direct Build calls, as in the source.
	toolTTL   float64
	toolLimit int

	mu       sync.Mutex // guards sequence and tools
	sequence uint64
	tools    recent[identity, snapshot]
	pending  sync.WaitGroup

	queueMu sync.Mutex // guards queue and sending
	queue   []queuedPost
	sending bool // a sender is draining queue
}

// queuedPost is one record waiting for the sender. Its export bound started when it was submitted.
type queuedPost struct {
	ctx     context.Context
	cancel  context.CancelFunc
	payload []byte
}

// NewEmitter resolves log settings and shares one builder across captures.
func NewEmitter(lookup config.LookupEnv, builder *Builder) *Emitter {
	return newEmitter(lookup, builder, nil)
}

// NewEmitterWithTransport is NewEmitter whose collector posts go through transport, so that their
// connections belong to the caller. The transport should keep no idle connection, consult no proxy and
// ask for no compression, as the package's own transport does. A nil transport is NewEmitter.
func NewEmitterWithTransport(lookup config.LookupEnv, builder *Builder, transport http.RoundTripper) *Emitter {
	e := newEmitter(lookup, builder, nil)
	if e.client != nil && transport != nil {
		e.client.Transport = transport
	}
	return e
}

// A nil dial function uses the standard network dialer.
func newEmitter(lookup config.LookupEnv, builder *Builder, dialContext func(context.Context, string, string) (net.Conn, error)) *Emitter {
	e := &Emitter{
		builder:   builder,
		endpoint:  config.UsageURL(lookup),
		maxBytes:  intBound(config.UsageMaxResponse.Get(lookup)),
		toolTTL:   config.DispatchTTL.Get(lookup),
		toolLimit: intBound(config.DispatchMax.Get(lookup)),
	}
	if e.endpoint != nil {
		e.client = exportClient(dialContext)
	}
	return e
}

// intBound converts a configured positive bound, possibly infinite, to an int.
func intBound(n float64) int {
	if n >= math.MaxInt {
		return math.MaxInt
	}
	return int(n)
}

// exportClient makes one connection per post, as the source's agentless requests do: no proxy,
// no redirects, no compression negotiation and no idle connections left behind.
func exportClient(dialContext func(context.Context, string, string) (net.Conn, error)) *http.Client {
	if dialContext == nil {
		dialContext = (&net.Dialer{}).DialContext
	}
	return &http.Client{
		Transport:     &http.Transport{DialContext: dialContext, DisableKeepAlives: true, DisableCompression: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Enabled reports whether records have a local export destination.
func (e *Emitter) Enabled() bool { return e.endpoint != nil }

// Start allocates a request sequence in arrival order, not completion order, and fixes the
// request's tool-block evidence. The capture works whether or not export is enabled. Its one
// eligible Finish exports the primary record first; for token usage it then exports the MCP
// footprint records and advances the emitter's tool-result snapshots exactly once. A panic in
// that handling is not recovered: it reaches the caller of Finish.
func (e *Emitter) Start(req Request) *Capture {
	e.mu.Lock()
	e.sequence++
	sequence := e.sequence
	e.mu.Unlock()
	req.Sequence = &sequence
	key := identity{identifierHeader(req.Headers, "x-claude-code-session-id"), identifierHeader(req.Headers, "x-claude-code-agent-id"), identifierHeader(req.Headers, "x-claude-code-parent-agent-id")}
	var blocks []block
	if key.session != "" {
		blocks = messageBlocks(req.Body)
	}
	c := NewCapture(e.builder, req, e.maxBytes, nil)
	c.handle = func(primary Record) { e.emit(primary, key, blocks) }
	return c
}

// emit schedules the primary record and the records derived from it without waiting for the
// collector. Nothing on this path recovers: the capture runs emit outside the recovery it keeps
// for caller-supplied callbacks, so only a panic on the calling goroutine, while emit encodes or
// derives a record, reaches the caller of Finish instead of being hidden; a panic in an export
// goroutine that schedule starts cannot reach that caller. Each lock it takes is released by a
// deferred unlock, so the emitter and its builder stay usable after such a panic.
func (e *Emitter) emit(primary Record, key identity, blocks []block) {
	e.schedule(primary)
	if primary.EventName != tokenUsageEvent {
		return
	}
	for _, record := range e.builder.emitterRecords(primary, e.advanceTools(primary, key, blocks)) {
		e.schedule(record)
	}
}

func (e *Emitter) advanceTools(primary Record, key identity, blocks []block) []toolAttribution {
	e.mu.Lock()
	defer e.mu.Unlock()
	return advanceTools(&e.tools, e.toolTTL, e.toolLimit, key, func() []block { return blocks }, primary.Attributes["context_tokens"], primary.ObservedAt)
}

var footprintKey = regexp.MustCompile(`^input_mcp_tools_([a-z0-9_]{1,64})_tokens$`)

// emitterRecords derives the MCP footprint records from the primary record's per-server
// attributes, in attribute order, followed by the tool-result records.
func (b *Builder) emitterRecords(primary Record, added []toolAttribution) []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	var records []Record
	for _, key := range primary.order {
		m := footprintKey.FindStringSubmatch(key)
		tokens, ok := numeric(primary.Attributes[key])
		if m == nil || !ok {
			continue
		}
		foot := b.derived(primary, "gateway.mcp.footprint", "gateway-mcp-footprint-")
		foot.add("mcp_server", m[1])
		foot.add("input_mcp_tools_tokens", tokens)
		records = append(records, foot)
	}
	return append(records, b.toolResultRecords(primary, added)...)
}

// schedule submits one record for export in the background and returns at once. Records reach the
// collector in submission order, as the source starts its posts in order: one sender starts each
// post and waits only until its request is written, or the post ended, before starting the next.
// Replies are read concurrently, so a slow collector delays no later post. Each post's bound starts
// at submission, and a record still queued when its bound expires is dropped. Delivery failures,
// including timeouts and refused connections, are silent and never affect the request.
func (e *Emitter) schedule(record Record) {
	if e.client == nil {
		return
	}
	payload, err := record.OTLP()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	e.queueMu.Lock()
	e.queue = append(e.queue, queuedPost{ctx: ctx, cancel: cancel, payload: payload})
	start := !e.sending
	e.sending = true
	e.queueMu.Unlock()
	if start {
		e.pending.Go(e.send)
	}
}

// send starts the queued posts one at a time, in queue order, until the queue is empty. It is
// counted in pending, so every post it starts is added to pending before Wait can return.
func (e *Emitter) send() {
	for {
		e.queueMu.Lock()
		if len(e.queue) == 0 {
			e.queue, e.sending = nil, false
			e.queueMu.Unlock()
			return
		}
		next := e.queue[0]
		e.queue[0] = queuedPost{}
		e.queue = e.queue[1:]
		e.queueMu.Unlock()
		if next.ctx.Err() != nil {
			next.cancel()
			continue
		}
		written := make(chan struct{})
		e.pending.Go(func() {
			defer next.cancel()
			e.post(next.ctx, next.payload, sync.OnceFunc(func() { close(written) }))
		})
		<-written
	}
}

// post sends one record under ctx and calls written once: when the whole request is on the
// connection, or when the post ends without that.
//
// The body is an opaque reader of known length, not a *bytes.Reader. This relies on net/http
// behaviour that is implemented but not documented: for a body it does not recognise as in-memory,
// the transport flushes the headers before the body and copies the body straight onto the
// connection, so WroteRequest fires only once the request has left the transport's write buffer.
// With a *bytes.Reader it fires while the request is still in that buffer, and the next post can
// start before this one reached the collector. TestEmitterSubmissionOrder fails if that changes.
func (e *Emitter) post(ctx context.Context, payload []byte, written func()) {
	defer written()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { written() }})
	opaque := func() io.Reader { return struct{ io.Reader }{bytes.NewReader(payload)} }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint.String(), opaque())
	if err != nil {
		return
	}
	// NewRequest sets neither for a body type it does not know: keep the fixed length (no chunked
	// encoding), and keep the request rewindable as a *bytes.Reader body would be.
	req.ContentLength = int64(len(payload))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(opaque()), nil }
	req.Header.Set("Content-Type", "application/json")
	// An empty value suppresses Go's default User-Agent; the source sends none.
	req.Header.Set("User-Agent", "")
	resp, err := e.client.Do(req)
	if err != nil {
		return
	}
	// The deadline would not end the drain of a connection that switched protocols.
	if upstream.RefuseSwitch(resp) != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// Wait waits for scheduled exports after the last request has finished.
func (e *Emitter) Wait() { e.pending.Wait() }
