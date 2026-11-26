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
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
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
	ObserveEvent(event jsontext.Value)
	ObserveChunk(chunk []byte)
	NoteResponseBytes(n int)
	MarkOverflow()
}

// Telemetry records the terminal HTTP status and optional failure override for a relayed response.
type Telemetry interface {
	Finish(status int, override string)
}

// OverflowResponder writes a normalized 413 response when LM Studio first-event peeking
// detects an engine context overflow before headers are committed.
type OverflowResponder func(w http.ResponseWriter, counts sentry.Counts, upstreamStatus int)

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
	MergedUsage         jsontext.Value
}

// NoteEvent updates attempt structure, error state, and merged usage from one parsed SSE event.
func (a *Attempt) NoteEvent(event jsontext.Value) {
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
type TransformerConfig struct {
	AdvertisedModel string
	BackendModel    string
	Provider        string
	FilterPlanTools bool
	Observe         func(event jsontext.Value)
}

// Transformer rewrites proxy SSE frames in wire order on top of internal/sse.Scanner.
// Untouched frames (comments, empty data, [DONE], or unparseable JSON) preserve their raw bytes,
// including ill-formed UTF-8. Rewritten frames decode the entire frame with Node's UTF-8 semantics,
// apply LM Studio exclusive cache accounting when configured, invoke Observe, rewrite model IDs,
// filter plan tools, and re-encode with the wire JSON policy.
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
// When pending unparsed bytes exceed maxPendingBytes, it clears its buffer, calls onOverflow
// once, and ignores all subsequent input.
type EventObserver struct {
	maxPendingBytes int
	observe         func(jsontext.Value)
	onOverflow      func()
}

// NewEventObserver creates a side-branch SSE observer with the given pending byte limit.
// A non-positive maxPendingBytes defaults to DefaultMaxObserveBytes.
func NewEventObserver(maxPendingBytes int, observe func(jsontext.Value), onOverflow func()) *EventObserver {
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
	Telemetry         Telemetry
	OnOverflow        OverflowResponder
	OnPeek            func() // Optional hook invoked when LM Studio first-event peeking starts.
}

// RelayProxy relays a 2xx proxy SSE response to w with model rewriting, plan-tool filtering,
// heartbeats, sentry observation, and (for eligible LM Studio streams) first-event overflow peeking.
// If the upstream breaks after response headers are committed, RelayProxy returns http.ErrAbortHandler.
func RelayProxy(ctx context.Context, w http.ResponseWriter, upstream *http.Response, opts ProxyOptions) error {
	_ = ctx
	_ = w
	_ = upstream
	_ = opts
	return errNotImplemented
}

// PassthroughOptions configures untouched streaming relay for Anthropic responses
// and non-2xx or non-rewritten responses.
type PassthroughOptions struct {
	ProxyProvider     string        // Empty for Anthropic passthrough; non-empty enables proxy heartbeat on 2xx SSE.
	HeartbeatInterval time.Duration // Used only when ProxyProvider is nonempty; zero uses DefaultHeartbeatInterval; negative disables.
	MaxObserveBytes   int           // Pending-frame byte budget for side-branch SSE observation; non-positive uses DefaultMaxObserveBytes.
	Usage             UsageCapture
	Telemetry         Telemetry
}

// RelayPassthrough forwards upstream status, headers minus hop-by-hop headers, and raw body bytes
// unchanged, flushing each chunk. For 2xx SSE responses with Usage set, it decodes gzip, deflate,
// or br on a side branch subject to MaxObserveBytes without altering delivered bytes.
// Anthropic passthrough never sends heartbeats and never updates sentry state.
// If the upstream breaks after response headers are committed, RelayPassthrough returns http.ErrAbortHandler.
func RelayPassthrough(ctx context.Context, w http.ResponseWriter, upstream *http.Response, opts PassthroughOptions) error {
	_ = ctx
	_ = w
	_ = upstream
	_ = opts
	return errNotImplemented
}
