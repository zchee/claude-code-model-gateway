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

import "encoding/json/jsontext"

// Outcome classifies the terminal state of a guarded compaction request for route telemetry.
type Outcome string

const (
	// OutcomeCompleted reports a deliverable generation with visible summary text.
	OutcomeCompleted Outcome = "completed"
	// OutcomeAborted reports an undeliverable stream whose upstream connection aborted.
	OutcomeAborted Outcome = "aborted"
	// OutcomeIncomplete reports an undeliverable stream with a known or absent error code.
	OutcomeIncomplete Outcome = "incomplete"
	// OutcomeEmptySummary reports a deliverable generation with no visible text delta.
	OutcomeEmptySummary Outcome = "empty_summary"
	// OutcomeUpstreamError reports a non-2xx upstream status or upstream transport failure.
	OutcomeUpstreamError Outcome = "upstream_error"
	// OutcomeUnknownError reports an undeliverable stream carrying an unclassified upstream error.
	OutcomeUnknownError Outcome = "unknown_error"
)

// ErrorCode identifies a safe, closed-set error classification for compaction route telemetry.
type ErrorCode string

const (
	// ErrorInvalidRequest classifies an invalid_request_error event.
	ErrorInvalidRequest ErrorCode = "invalid_request_error"
	// ErrorAuthentication classifies an authentication_error event.
	ErrorAuthentication ErrorCode = "authentication_error"
	// ErrorPermission classifies a permission_error event.
	ErrorPermission ErrorCode = "permission_error"
	// ErrorNotFound classifies a not_found_error event.
	ErrorNotFound ErrorCode = "not_found_error"
	// ErrorRequestTooLarge classifies a request_too_large event.
	ErrorRequestTooLarge ErrorCode = "request_too_large"
	// ErrorRateLimit classifies a rate_limit_error event.
	ErrorRateLimit ErrorCode = "rate_limit_error"
	// ErrorBilling classifies a billing_error event.
	ErrorBilling ErrorCode = "billing_error"
	// ErrorWebSocketMissingTerminal classifies an api_error carrying the websocket_missing_terminal slug.
	ErrorWebSocketMissingTerminal ErrorCode = "websocket_missing_terminal"
	// ErrorUpstreamHTTP classifies a non-2xx upstream status, non-SSE retry reply, or transport error.
	ErrorUpstreamHTTP ErrorCode = "upstream_http_error"
	// ErrorUnknown classifies any other upstream SSE error payload.
	ErrorUnknown ErrorCode = "unknown_error"
)

// Usage holds the four nonnegative token counts recorded in compaction route diagnostics.
type Usage struct {
	InputTokens         *float64 `json:"input_tokens"`
	OutputTokens        *float64 `json:"output_tokens"`
	CacheReadTokens     *float64 `json:"cache_read_tokens"`
	CacheCreationTokens *float64 `json:"cache_creation_tokens"`
}

// Diagnostics holds the content-free compaction fields reported to route telemetry.
type Diagnostics struct {
	Outcome        Outcome   `json:"outcome"`
	UpstreamStatus *int      `json:"upstreamStatus"`
	TerminalCode   string    `json:"terminalCode"`
	ErrorCode      ErrorCode `json:"errorCode"`
	Usage          Usage     `json:"usage"`
}

// Attempt tracks stream structure, completion, and error state for one upstream SSE attempt.
// Its zero value is ready to use.
type Attempt struct {
	Terminal            bool
	TerminalCode        string
	ErrorCode           ErrorCode
	HasVisibleText      bool
	Fatal               bool
	SawError            bool
	Degraded            bool
	EndOfGenerationStop bool
	ContentAfterStop    bool
	OpenBlocks          int
}

// NoteEvent updates the attempt state from one parsed SSE JSON payload.
func (a *Attempt) NoteEvent(event jsontext.Value) {}

// GenerationFinished reports whether the stream reached message_stop or an uncontradicted end-of-generation stop.
func (a *Attempt) GenerationFinished() bool {
	return false
}

// Deliverable reports whether the attempt finished generation without any typed upstream error event.
func (a *Attempt) Deliverable() bool {
	return false
}

// Outcome classifies the settled attempt from the upstream HTTP status and any transport override.
func (a *Attempt) Outcome(statusCode int, statusOverride string) Outcome {
	return ""
}

// IsFatalErrorType reports whether errorType is a request-shaped error that must not be retried.
func IsFatalErrorType(errorType string) bool {
	return false
}

// SnapshotUsage retains only finite nonnegative token counts and clears invalid fields to nil.
func SnapshotUsage(usage Usage) Usage {
	return Usage{}
}

// MergeUsage prefers non-nil fields from next and falls back to previous.
func MergeUsage(previous, next Usage) Usage {
	return Usage{}
}

// NormalizeDiagnostics clamps outcome, terminal code, error code, and usage to the allowed telemetry domain.
func NormalizeDiagnostics(diag Diagnostics) Diagnostics {
	return Diagnostics{}
}

// SSEErrorFrame formats one Anthropic-style SSE error frame without HTML escaping.
func SSEErrorFrame(errType, message string) string {
	return ""
}

// UpstreamErrorMessage formats the in-band message for a non-2xx or non-SSE retry response.
func UpstreamErrorMessage(body []byte, statusCode int) string {
	return ""
}

// IncompleteErrorMessage formats the fallback SSE error message when retries are exhausted without an upstream error event.
func IncompleteErrorMessage(provider string, attempts int) string {
	return ""
}
