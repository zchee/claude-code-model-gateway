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

package compactguard_test

import (
	"encoding/json/jsontext"
	"math"
	"net/http"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
)

func TestAttemptStopReasons(t *testing.T) {
	t.Parallel()

	// Matches every stop_reason value covered by the pinned compaction stream matrix.
	tests := map[string]struct {
		stopReasonJSON string
		wantFinished   bool
		wantOutcome    compactguard.Outcome
	}{
		"success: end_turn finishes generation": {
			stopReasonJSON: `"end_turn"`,
			wantFinished:   true,
			wantOutcome:    compactguard.OutcomeCompleted,
		},
		"success: stop_sequence finishes generation": {
			stopReasonJSON: `"stop_sequence"`,
			wantFinished:   true,
			wantOutcome:    compactguard.OutcomeCompleted,
		},
		"error: max_tokens does not finish generation": {
			stopReasonJSON: `"max_tokens"`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
		"error: refusal does not finish generation": {
			stopReasonJSON: `"refusal"`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
		"error: pause_turn does not finish generation": {
			stopReasonJSON: `"pause_turn"`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
		"error: tool_use does not finish generation": {
			stopReasonJSON: `"tool_use"`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
		"error: vendor_specific_stop does not finish generation": {
			stopReasonJSON: `"vendor_specific_stop"`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
		"error: numeric stop_reason does not finish generation": {
			stopReasonJSON: `7`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
		"error: empty string stop_reason does not finish generation": {
			stopReasonJSON: `""`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
		"error: null stop_reason does not finish generation": {
			stopReasonJSON: `null`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
		"error: boolean false stop_reason does not finish generation": {
			stopReasonJSON: `false`,
			wantFinished:   false,
			wantOutcome:    compactguard.OutcomeIncomplete,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var attempt compactguard.Attempt
			attempt.NoteEvent(jsontext.Value(`{"type":"message_start","message":{"id":"msg_1","role":"assistant"}}`))
			attempt.NoteEvent(jsontext.Value(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
			attempt.NoteEvent(jsontext.Value(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"summary"}}`))
			attempt.NoteEvent(jsontext.Value(`{"type":"content_block_stop","index":0}`))
			attempt.NoteEvent(jsontext.Value(`{"type":"message_delta","delta":{"stop_reason":` + tt.stopReasonJSON + `},"usage":{"output_tokens":12}}`))

			if diff := gocmp.Diff(tt.wantFinished, attempt.GenerationFinished()); diff != "" {
				t.Fatalf("GenerationFinished mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantFinished, attempt.Deliverable()); diff != "" {
				t.Fatalf("Deliverable mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantOutcome, attempt.Outcome(http.StatusOK, "")); diff != "" {
				t.Fatalf("Outcome mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAttemptContradictionsAndErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		events          []string
		statusCode      int
		statusOverride  string
		wantFinished    bool
		wantDeliverable bool
		wantFatal       bool
		wantSawError    bool
		wantTerminal    bool
		wantTermCode    string
		wantErrorCode   compactguard.ErrorCode
		wantVisibleText bool
		wantOutcome     compactguard.Outcome
	}{
		"success: explicit message_stop with visible text": {
			events: []string{
				`{"type":"message_start","message":{"id":"msg_1"}}`,
				`{"type":"content_block_start","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_stop"}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    true,
			wantDeliverable: true,
			wantTerminal:    true,
			wantTermCode:    compactguard.TerminalMessageStop,
			wantVisibleText: true,
			wantOutcome:     compactguard.OutcomeCompleted,
		},
		"success: finished generation with no text blocks is empty_summary": {
			events: []string{
				`{"type":"message_start","message":{"id":"msg_1"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    true,
			wantDeliverable: true,
			wantVisibleText: false,
			wantOutcome:     compactguard.OutcomeEmptySummary,
		},
		"success: whitespace-only text delta including NBSP and BOM is empty_summary": {
			events: []string{
				`{"type":"content_block_start","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" \t\r\n` + "\\u00a0\\ufeff" + ` "}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_stop"}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    true,
			wantDeliverable: true,
			wantTerminal:    true,
			wantTermCode:    compactguard.TerminalMessageStop,
			wantVisibleText: false,
			wantOutcome:     compactguard.OutcomeEmptySummary,
		},
		"success: thinking delta does not count as visible text": {
			events: []string{
				`{"type":"content_block_start","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_stop"}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    true,
			wantDeliverable: true,
			wantTerminal:    true,
			wantTermCode:    compactguard.TerminalMessageStop,
			wantVisibleText: false,
			wantOutcome:     compactguard.OutcomeEmptySummary,
		},
		"success: untyped error payload leaves finished generation deliverable": {
			events: []string{
				`{"type":"content_block_start","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"summary"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"error":{"type":"api_error","message":"websocket_missing_terminal"}}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    true,
			wantDeliverable: true,
			wantVisibleText: true,
			wantOutcome:     compactguard.OutcomeCompleted,
		},
		"error: open content block contradicts end_turn": {
			events: []string{
				`{"type":"content_block_start","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"open"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    false,
			wantDeliverable: false,
			wantVisibleText: true,
			wantOutcome:     compactguard.OutcomeIncomplete,
		},
		"error: content_block_stop without start leaves non-zero open blocks": {
			events: []string{
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    false,
			wantDeliverable: false,
			wantOutcome:     compactguard.OutcomeIncomplete,
		},
		"error: content after end_turn contradicts stop": {
			events: []string{
				`{"type":"content_block_start","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"before"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"after"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":null}}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    false,
			wantDeliverable: false,
			wantVisibleText: true,
			wantOutcome:     compactguard.OutcomeIncomplete,
		},
		"error: typed websocket_missing_terminal after end_turn is not deliverable": {
			events: []string{
				`{"type":"content_block_start","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"error","error":{"type":"api_error","message":"websocket_missing_terminal"}}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    true,
			wantDeliverable: false,
			wantSawError:    true,
			wantErrorCode:   compactguard.ErrorWebSocketMissingTerminal,
			wantVisibleText: true,
			wantOutcome:     compactguard.OutcomeIncomplete,
		},
		"error: typed error after message_stop is not deliverable": {
			events: []string{
				`{"type":"message_stop"}`,
				`{"type":"error","error":{"type":"api_error","message":"unexpected"}}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    true,
			wantDeliverable: false,
			wantSawError:    true,
			wantTerminal:    true,
			wantTermCode:    compactguard.TerminalMessageStop,
			wantErrorCode:   compactguard.ErrorUnknown,
			wantOutcome:     compactguard.OutcomeUnknownError,
		},
		"error: null error field classifies as unknown_error and not fatal": {
			events: []string{
				`{"type":"error","error":null}`,
			},
			statusCode:      http.StatusOK,
			wantFinished:    false,
			wantDeliverable: false,
			wantSawError:    true,
			wantErrorCode:   compactguard.ErrorUnknown,
			wantOutcome:     compactguard.OutcomeUnknownError,
		},
		"error: aborted after completion remains completed": {
			events: []string{
				`{"type":"content_block_start","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_stop"}`,
			},
			statusCode:      http.StatusOK,
			statusOverride:  "upstream_aborted",
			wantFinished:    true,
			wantDeliverable: true,
			wantTerminal:    true,
			wantTermCode:    compactguard.TerminalMessageStop,
			wantVisibleText: true,
			wantOutcome:     compactguard.OutcomeCompleted,
		},
		"error: aborted before completion yields aborted": {
			events: []string{
				`{"type":"content_block_start","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"continued"}}`,
			},
			statusCode:      http.StatusOK,
			statusOverride:  "upstream_aborted",
			wantFinished:    false,
			wantDeliverable: false,
			wantVisibleText: true,
			wantOutcome:     compactguard.OutcomeAborted,
		},
		"error: non-2xx status always yields upstream_error": {
			events: []string{
				`{"type":"message_stop"}`,
			},
			statusCode:      http.StatusServiceUnavailable,
			wantFinished:    true,
			wantDeliverable: true,
			wantTerminal:    true,
			wantTermCode:    compactguard.TerminalMessageStop,
			wantOutcome:     compactguard.OutcomeUpstreamError,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var attempt compactguard.Attempt
			for _, raw := range tt.events {
				attempt.NoteEvent(jsontext.Value(raw))
			}

			if diff := gocmp.Diff(tt.wantFinished, attempt.GenerationFinished()); diff != "" {
				t.Fatalf("GenerationFinished mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantDeliverable, attempt.Deliverable()); diff != "" {
				t.Fatalf("Deliverable mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantFatal, attempt.Fatal); diff != "" {
				t.Fatalf("Fatal mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantSawError, attempt.SawError); diff != "" {
				t.Fatalf("SawError mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantTerminal, attempt.Terminal); diff != "" {
				t.Fatalf("Terminal mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantTermCode, attempt.TerminalCode); diff != "" {
				t.Fatalf("TerminalCode mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantErrorCode, attempt.ErrorCode); diff != "" {
				t.Fatalf("ErrorCode mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantVisibleText, attempt.HasVisibleText); diff != "" {
				t.Fatalf("HasVisibleText mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantOutcome, attempt.Outcome(tt.statusCode, tt.statusOverride)); diff != "" {
				t.Fatalf("Outcome mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFatalErrorTypes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		errType   string
		wantFatal bool
		wantCode  compactguard.ErrorCode
	}{
		"success: invalid_request_error is fatal": {
			errType:   "invalid_request_error",
			wantFatal: true,
			wantCode:  compactguard.ErrorInvalidRequest,
		},
		"success: authentication_error is fatal": {
			errType:   "authentication_error",
			wantFatal: true,
			wantCode:  compactguard.ErrorAuthentication,
		},
		"success: permission_error is fatal": {
			errType:   "permission_error",
			wantFatal: true,
			wantCode:  compactguard.ErrorPermission,
		},
		"success: not_found_error is fatal": {
			errType:   "not_found_error",
			wantFatal: true,
			wantCode:  compactguard.ErrorNotFound,
		},
		"success: request_too_large is fatal": {
			errType:   "request_too_large",
			wantFatal: true,
			wantCode:  compactguard.ErrorRequestTooLarge,
		},
		"success: rate_limit_error is fatal": {
			errType:   "rate_limit_error",
			wantFatal: true,
			wantCode:  compactguard.ErrorRateLimit,
		},
		"success: billing_error is fatal": {
			errType:   "billing_error",
			wantFatal: true,
			wantCode:  compactguard.ErrorBilling,
		},
		"error: api_error is not fatal": {
			errType:   "api_error",
			wantFatal: false,
			wantCode:  compactguard.ErrorUnknown,
		},
		"error: overloaded_error is not fatal": {
			errType:   "overloaded_error",
			wantFatal: false,
			wantCode:  compactguard.ErrorUnknown,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.wantFatal, compactguard.IsFatalErrorType(tt.errType)); diff != "" {
				t.Fatalf("IsFatalErrorType mismatch (-want +got):\n%s", diff)
			}
			var attempt compactguard.Attempt
			attempt.NoteEvent(jsontext.Value(`{"type":"error","error":{"type":"` + tt.errType + `","message":"detail"}}`))
			if diff := gocmp.Diff(tt.wantFatal, attempt.Fatal); diff != "" {
				t.Fatalf("Attempt.Fatal mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantCode, attempt.ErrorCode); diff != "" {
				t.Fatalf("Attempt.ErrorCode mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUsageSnapshotMergeAndDiagnostics(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		snapshotIn compactguard.Usage
		snapshotOk compactguard.Usage
		mergePrev  compactguard.Usage
		mergeNext  compactguard.Usage
		mergeWant  compactguard.Usage
	}{
		"success: keeps zero and positive finite values and drops invalid ones": {
			snapshotIn: compactguard.Usage{
				InputTokens:         new(0.0),
				OutputTokens:        new(12.0),
				CacheReadTokens:     new(-1.0),
				CacheCreationTokens: new(math.Inf(1)),
			},
			snapshotOk: compactguard.Usage{
				InputTokens:  new(0.0),
				OutputTokens: new(12.0),
			},
			mergePrev: compactguard.Usage{
				InputTokens:     new(17.0),
				OutputTokens:    new(12.0),
				CacheReadTokens: new(3.0),
			},
			mergeNext: compactguard.Usage{
				OutputTokens:        new(15.0),
				CacheCreationTokens: new(4.0),
			},
			mergeWant: compactguard.Usage{
				InputTokens:         new(17.0),
				OutputTokens:        new(15.0),
				CacheReadTokens:     new(3.0),
				CacheCreationTokens: new(4.0),
			},
		},
		"error: NaN and negative infinity become nil": {
			snapshotIn: compactguard.Usage{
				InputTokens:  new(math.NaN()),
				OutputTokens: new(math.Inf(-1)),
			},
			snapshotOk: compactguard.Usage{},
			mergePrev:  compactguard.Usage{InputTokens: new(5.0)},
			mergeNext:  compactguard.Usage{},
			mergeWant:  compactguard.Usage{InputTokens: new(5.0)},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.snapshotOk, compactguard.SnapshotUsage(tt.snapshotIn)); diff != "" {
				t.Fatalf("SnapshotUsage mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.mergeWant, compactguard.MergeUsage(tt.mergePrev, tt.mergeNext)); diff != "" {
				t.Fatalf("MergeUsage mismatch (-want +got):\n%s", diff)
			}
		})
	}

	diagTests := map[string]struct {
		in   compactguard.Diagnostics
		want compactguard.Diagnostics
	}{
		"success: valid diagnostics preserved": {
			in: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeCompleted,
				UpstreamStatus: new(200),
				TerminalCode:   compactguard.TerminalMessageStop,
				ErrorCode:      compactguard.ErrorWebSocketMissingTerminal,
				Usage:          compactguard.Usage{InputTokens: new(19.0), OutputTokens: new(13.0)},
			},
			want: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeCompleted,
				UpstreamStatus: new(200),
				TerminalCode:   compactguard.TerminalMessageStop,
				ErrorCode:      compactguard.ErrorWebSocketMissingTerminal,
				Usage:          compactguard.Usage{InputTokens: new(19.0), OutputTokens: new(13.0)},
			},
		},
		"error: disallowed outcome terminal and error code clamped": {
			in: compactguard.Diagnostics{
				Outcome:        compactguard.Outcome("raw-secret-outcome"),
				UpstreamStatus: new(503),
				TerminalCode:   "other_terminal",
				ErrorCode:      compactguard.ErrorCode("raw-secret-error"),
				Usage:          compactguard.Usage{InputTokens: new(-5.0), OutputTokens: new(0.0)},
			},
			want: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeUnknownError,
				UpstreamStatus: new(503),
				TerminalCode:   "",
				ErrorCode:      "",
				Usage:          compactguard.Usage{OutputTokens: new(0.0)},
			},
		},
	}

	for name, tt := range diagTests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diff := gocmp.Diff(tt.want, compactguard.NormalizeDiagnostics(tt.in)); diff != "" {
				t.Fatalf("NormalizeDiagnostics mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestErrorFormatting(t *testing.T) {
	t.Parallel()

	t.Run("success: SSEErrorFrame preserves angle brackets without HTML escaping", func(t *testing.T) {
		t.Parallel()
		got := compactguard.SSEErrorFrame("api_error", "a < b & c > d")
		want := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"a < b & c > d\"}}\n\n"
		if diff := gocmp.Diff(want, got); diff != "" {
			t.Fatalf("SSEErrorFrame mismatch (-want +got):\n%s", diff)
		}
	})

	msgTests := map[string]struct {
		body       string
		statusCode int
		want       string
	}{
		"success: nested error.message extracted": {
			body:       `{"type":"error","error":{"type":"api_error","message":"upstream exploded"}}`,
			statusCode: 500,
			want:       "model-gateway: upstream returned 500: upstream exploded",
		},
		"success: top-level message extracted": {
			body:       `{"message":"rate limited"}`,
			statusCode: 429,
			want:       "model-gateway: upstream returned 429: rate limited",
		},
		"error: empty message falls back to unreadable body text": {
			body:       `{"error":{"message":""}}`,
			statusCode: 502,
			want:       "model-gateway: upstream returned 502 with no readable error body",
		},
		"error: non-JSON body falls back to unreadable body text": {
			body:       `plain text failure`,
			statusCode: 503,
			want:       "model-gateway: upstream returned 503 with no readable error body",
		},
	}

	for name, tt := range msgTests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := compactguard.UpstreamErrorMessage([]byte(tt.body), tt.statusCode)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("UpstreamErrorMessage mismatch (-want +got):\n%s", diff)
			}
		})
	}

	incompleteTests := map[string]struct {
		provider string
		attempts int
		want     string
	}{
		"success: codex label": {
			provider: "codex",
			attempts: 2,
			want:     "model-gateway: the Codex compaction stream ended without a completed generation after 2 attempt(s); the summary above is incomplete",
		},
		"success: lmstudio label": {
			provider: "lmstudio",
			attempts: 3,
			want:     "model-gateway: the LM Studio compaction stream ended without a completed generation after 3 attempt(s); the summary above is incomplete",
		},
		"success: empty provider uses Proxy label": {
			provider: "",
			attempts: 1,
			want:     "model-gateway: the Proxy compaction stream ended without a completed generation after 1 attempt(s); the summary above is incomplete",
		},
	}

	for name, tt := range incompleteTests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := compactguard.IncompleteErrorMessage(tt.provider, tt.attempts)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("IncompleteErrorMessage mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
