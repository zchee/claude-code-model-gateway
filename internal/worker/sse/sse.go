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
	"io"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	sseframe "github.com/zchee/claude-code-model-gateway/internal/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
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

	// advertised and backend hold the model IDs as the UTF-16 units decoded strings compare against.
	advertised, backend []uint16

	scanner sseframe.Scanner

	// dropped holds the original indexes of filtered plan-tool blocks; droppedCount shifts later indexes.
	dropped      map[float64]struct{}
	droppedCount int
	keptToolUse  bool
}

// EventObserver parses SSE frames on a side branch without altering relayed bytes.
// When pending decoded UTF-8 bytes exceed maxPendingBytes, it clears its buffer, calls onOverflow
// once, and ignores all subsequent input. Both observe and onOverflow may be nil.
type EventObserver struct {
	maxPendingBytes int
	observe         func(jsonx.WireValue)
	onOverflow      func()

	// carry holds a trailing UTF-8 sequence still waiting for its continuation bytes.
	carry []byte
	// pending holds decoded text that has not yet reached a blank-line separator.
	pending []byte
	// scanFrom is where the next separator search may start: earlier offsets cannot begin one.
	scanFrom   int
	overflowed bool
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

// ProxyOptions configures streaming relay of a 2xx proxy text/event-stream response.
type ProxyOptions struct {
	Provider          string
	AdvertisedModel   string
	BackendModel      string
	FilterPlanTools   bool
	IsCompaction      bool
	HeadWritten       bool
	HeartbeatInterval time.Duration   // Zero uses DefaultHeartbeatInterval; negative disables heartbeats.
	FirstEventWait    time.Duration   // Zero uses DefaultFirstEventWait; negative times out immediately.
	ClientDone        <-chan struct{} // Optional signal closed when the client disconnects (nil means never).
	SessionKey        string
	Sentry            *sentry.State
	SentryTarget      sentry.Target
	Usage             UsageCapture
	OnOverflow        OverflowResponder
	OnPeek            func() // Optional hook invoked when LM Studio first-event peeking starts.
}

// PassthroughOptions configures untouched streaming relay for Anthropic responses
// and non-2xx or non-rewritten responses.
type PassthroughOptions struct {
	ProxyProvider     string          // Empty for Anthropic passthrough; non-empty enables proxy heartbeat and eligible sentry observation on 2xx SSE.
	HeartbeatInterval time.Duration   // Used only when ProxyProvider is nonempty; zero uses DefaultHeartbeatInterval; negative disables.
	MaxObserveBytes   int             // Pending-frame byte budget for side-branch SSE observation; non-positive uses DefaultMaxObserveBytes.
	ClientDone        <-chan struct{} // Optional signal closed when the client disconnects (nil means never).
	SessionKey        string          // Used only when ProxyProvider is nonempty; Anthropic passthrough never touches Sentry.
	Sentry            *sentry.State   // Used only when ProxyProvider is nonempty; Anthropic passthrough never touches Sentry.
	SentryTarget      sentry.Target   // Used only when ProxyProvider is nonempty; Anthropic passthrough never touches Sentry.
	Usage             UsageCapture
}
