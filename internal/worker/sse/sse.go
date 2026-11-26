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

// Package sse relays proxy and passthrough SSE responses on top of internal/sse.
package sse

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const (
	// LMStudioProvider identifies the proxy provider that uses inclusive cache
	// accounting and first-event overflow peeking before committing headers.
	LMStudioProvider = "lmstudio"

	// DefaultHeartbeatInterval is the default idle interval before writing a
	// heartbeat comment on a proxy SSE stream.
	DefaultHeartbeatInterval = 20 * time.Second

	// DefaultFirstEventWait is the default deadline for peeking the first data
	// event of an eligible LM Studio stream before committing HTTP 200 headers.
	DefaultFirstEventWait = 10 * time.Second

	// DefaultMaxObserveBytes is the default pending-frame byte budget for
	// side-branch SSE observation on untouched passthrough responses.
	DefaultMaxObserveBytes = 4 * 1024 * 1024

	// HeartbeatComment is the SSE comment frame emitted while a proxy stream is idle.
	HeartbeatComment = ": ping\n\n"

	// SynthesizedMessageStop is emitted at EOF when a proxy generation finished
	// cleanly without sending a message_stop frame.
	SynthesizedMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

var errNotImplemented = errors.New("not implemented")

// UsageCapture records response-side usage observations for one request.
// Implementations must be safe for concurrent calls from the relay and its side-branch observer.
type UsageCapture interface {
	ObserveEvent(event jsonx.WireValue)
	ObserveChunk(chunk []byte)
	NoteResponseBytes(n int64)
	MarkOverflow()
}

// OverflowResponder writes a normalized 413 response when LM Studio first-event peeking
// detects an engine context overflow before headers are committed.
type OverflowResponder func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error)

// Attempt tracks SSE turn structure, upstream errors, and cumulative usage for one proxy stream attempt.
// Its zero value is ready to use.
type Attempt struct {
	Terminal            bool
	TerminalCode        string
	EndOfGenerationStop bool
	ContentAfterStop    bool
	OpenBlocks          int
	HasVisibleText      bool
	SawError            bool
	ErrorCode           string
	Fatal               bool
	MergedUsage         jsonx.WireValue
}

// NoteEvent updates attempt structure, error state, and merged usage from one decoded SSE event.
// Non-object or absent events have no effect.
func (a *Attempt) NoteEvent(event jsonx.WireValue) {
	_ = a
	_ = event
}

// Finished reports whether the stream reached message_stop or a clean end-of-generation stop with no open blocks.
func (a *Attempt) Finished() bool {
	_ = a
	return false
}

// Deliverable reports whether the attempt finished cleanly without seeing an upstream error event.
func (a *Attempt) Deliverable() bool {
	_ = a
	return false
}

// TransformerConfig configures per-frame proxy SSE rewriting and observation.
// Observe may be nil when no per-event observation is needed.
type TransformerConfig struct {
	AdvertisedModel string
	BackendModel    string
	Provider        string
	FilterPlanTools bool
	Observe         func(event jsonx.WireValue)
}

// Transformer rewrites proxy SSE frames in wire order on top of internal/sse.Scanner.
// Untouched frames (comments, empty data, [DONE], or unparseable JSON) preserve their raw bytes,
// including ill-formed UTF-8. Rewritten frames decode the entire frame with Node's UTF-8 semantics,
// apply LM Studio exclusive cache accounting when configured, invoke Observe once with the decoded
// wire value, rewrite model IDs, filter plan tools, and re-encode with the wire JSON policy.
type Transformer struct {
	cfg TransformerConfig
}

// NewTransformer creates a proxy SSE frame transformer.
func NewTransformer(cfg TransformerConfig) *Transformer {
	return &Transformer{cfg: cfg}
}

// Feed processes one upstream chunk and passes each emitted wire frame to emit.
func (t *Transformer) Feed(chunk []byte, emit func([]byte) error) error {
	_ = t
	_ = chunk
	_ = emit
	return errNotImplemented
}

// End flushes any remaining unterminated frame and passes it to emit.
func (t *Transformer) End(emit func([]byte) error) error {
	_ = t
	_ = emit
	return errNotImplemented
}

// EventObserver parses SSE frames on a side branch without altering relayed bytes.
// When pending decoded UTF-8 bytes exceed maxPendingBytes, it clears its buffer, calls onOverflow
// once, and ignores all subsequent input. Both observe and onOverflow may be nil.
type EventObserver struct {
	maxPendingBytes int
	observe         func(jsonx.WireValue)
	onOverflow      func()
}

// NewEventObserver creates a side-branch SSE observer with the given pending byte limit.
// A non-positive maxPendingBytes defaults to DefaultMaxObserveBytes.
func NewEventObserver(maxPendingBytes int, observe func(jsonx.WireValue), onOverflow func()) *EventObserver {
	return &EventObserver{maxPendingBytes: maxPendingBytes, observe: observe, onOverflow: onOverflow}
}

// Write feeds raw or decompressed stream bytes into the observer.
func (o *EventObserver) Write(chunk []byte) {
	_ = o
	_ = chunk
}

// End flushes any remaining unterminated frame if the observer has not overflowed.
func (o *EventObserver) End() {
	_ = o
}

// PeekResult describes the outcome of inspecting the first data event of an eligible LM Studio SSE body.
//
// On a non-overflow first event, wait timeout, or clean EOF before any event, Body is a non-nil
// owned io.ReadCloser that replays the exact buffered prefix followed by the unread tail, and
// Overflow and EarlyEndOverride are zero. On a recognized engine overflow, PeekFirstEvent closes
// the original body, sets Body to nil, and sets Overflow to a non-nil pointer (which may hold zero
// Counts when the overflow marker carried neither usable count). On an abnormal read error before
// a first complete data event, PeekFirstEvent closes the original body, sets Body to nil, and sets
// EarlyEndOverride to "upstream_error" or "upstream_aborted".
type PeekResult struct {
	Body             io.ReadCloser
	Overflow         *sentry.Counts
	EarlyEndOverride string
}

// PeekFirstEvent inspects body up to its first complete SSE data event or wait deadline.
// PeekFirstEvent takes ownership of body on entry. A zero wait uses DefaultFirstEventWait;
// a negative wait times out immediately.
//
// A clean EOF before a first data event is not an error and returns a non-nil replay Body.
// An abnormal upstream read error returns a nil Body, a non-empty EarlyEndOverride, and a nil error.
// On wait expiration, PeekFirstEvent does not close the usable stream and does not leave a goroutine
// racing a second reader; the single reader transfers to the returned Body whose Close unblocks and
// joins it. On ctx cancellation, PeekFirstEvent closes body, joins its reader goroutine, and returns
// ctx.Err().
func PeekFirstEvent(ctx context.Context, body io.ReadCloser, wait time.Duration) (PeekResult, error) {
	_ = ctx
	_ = body
	_ = wait
	return PeekResult{}, errNotImplemented
}

// ProxyOptions configures streaming relay of a 2xx proxy text/event-stream response.
type ProxyOptions struct {
	Provider          string
	AdvertisedModel   string
	BackendModel      string
	FilterPlanTools   bool
	IsCompaction      bool
	HeadWritten       bool
	HeartbeatInterval time.Duration // Zero uses DefaultHeartbeatInterval; negative disables heartbeats.
	FirstEventWait    time.Duration // Zero uses DefaultFirstEventWait; negative times out immediately.
	SessionKey        string
	Sentry            *sentry.State
	SentryTarget      sentry.Target
	Usage             UsageCapture
	OnOverflow        OverflowResponder
	OnPeek            func() // Optional hook invoked when LM Studio first-event peeking starts.
}

// RelayProxy relays a 2xx proxy SSE response to w with model rewriting, plan-tool filtering,
// heartbeats, sentry observation, and (for eligible LM Studio streams) first-event overflow peeking.
// It copies relay headers from resp.RelayHeader (or resp.Header if RelayHeader is nil) minus
// hop-by-hop and Content-Length headers without mutating resp.Header or resp.RelayHeader.
// It takes ownership of resp.Body and closes it before returning.
// If the upstream breaks after response headers are committed, RelayProxy returns the terminal
// outcome facts alongside http.ErrAbortHandler rather than panicking.
func RelayProxy(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, opts ProxyOptions) (outcome.Result, error) {
	_ = ctx
	_ = w
	_ = resp
	_ = opts
	return outcome.Result{}, errNotImplemented
}

// PassthroughOptions configures untouched streaming relay for Anthropic responses
// and non-2xx or non-rewritten responses.
type PassthroughOptions struct {
	ProxyProvider     string        // Empty for Anthropic passthrough; non-empty enables proxy heartbeat and eligible sentry observation on 2xx SSE.
	HeartbeatInterval time.Duration // Used only when ProxyProvider is nonempty; zero uses DefaultHeartbeatInterval; negative disables.
	MaxObserveBytes   int           // Pending-frame byte budget for side-branch SSE observation; non-positive uses DefaultMaxObserveBytes.
	SessionKey        string        // Used only when ProxyProvider is nonempty; Anthropic passthrough never touches Sentry.
	Sentry            *sentry.State // Used only when ProxyProvider is nonempty; Anthropic passthrough never touches Sentry.
	SentryTarget      sentry.Target // Used only when ProxyProvider is nonempty; Anthropic passthrough never touches Sentry.
	Usage             UsageCapture
}

// RelayPassthrough forwards upstream status, relay headers minus hop-by-hop headers, and raw body bytes
// unchanged, flushing each chunk. It copies headers from resp.RelayHeader (or resp.Header if RelayHeader
// is nil) without mutating resp.Header or resp.RelayHeader, and owns resp.Body. For 2xx SSE responses
// with Usage or eligible proxy Sentry set, it decodes gzip, deflate, or br on a side branch subject to
// MaxObserveBytes without altering delivered bytes. Anthropic passthrough (ProxyProvider == "") never
// sends heartbeats and never updates sentry state; on client write failure it pauses reading resp.Body
// for back-pressure without eagerly closing the provider until ctx (the service-owned lifetime context)
// is canceled. If the upstream breaks after response headers are committed, RelayPassthrough returns
// the terminal outcome facts alongside http.ErrAbortHandler rather than panicking.
func RelayPassthrough(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, opts PassthroughOptions) (outcome.Result, error) {
	_ = ctx
	_ = w
	_ = resp
	_ = opts
	return outcome.Result{}, errNotImplemented
}
