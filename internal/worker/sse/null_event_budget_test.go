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

package sse_test

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

// TestTransformerNullDataEvent checks that a transformed "data: null" frame is re-encoded on the
// wire and that the JSON null is ignored by attempt, sentry and usage accounting, so the valid
// events around it are rewritten, filtered and accounted for exactly as without it. The accounting
// of the observed events is compared with the accounting of the valid events alone, and the
// sentry's recorded usage is also compared with a literal crossing.
func TestTransformerNullDataEvent(t *testing.T) {
	const (
		advertised = "claude-gpt-5.6-sol"
		backend    = "gpt-5.6-sol"
		// sentryTrigger is the environment trigger of the sentry target every case observes into.
		sentryTrigger = 100
	)
	ordinaryReq := []byte(`{"model":"claude-gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	session := sentry.SessionKey(true, "session-null-frame", nil)
	target, err := sentry.ResolveTarget(policy.NewResolver(policy.ResolverInputs{}), advertised, sentryTrigger, 0, 0)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	tests := map[string]struct {
		cfg     sse.TransformerConfig
		chunks  []string
		wantOut string
		// wantValid are the non-null events Observe must receive, in order.
		wantValid []string
		// wantCrossing is what the sentry's check of the next ordinary request reports after the
		// observed events; nil means no crossing.
		wantCrossing *sentry.Crossing
	}{
		"success: null frame is re-encoded and the following event is rewritten and observed": {
			cfg: sse.TransformerConfig{AdvertisedModel: advertised, BackendModel: backend, Provider: "codex", FilterPlanTools: true},
			chunks: []string{
				"event: ping\r\ndata:   null\r\n\r\n" +
					"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"model\":\"gpt-5.6-sol\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
			wantOut: "event: ping\ndata: null\r\n\r\n" +
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"model\":\"claude-gpt-5.6-sol\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			wantValid: []string{
				`{"type":"message_start","message":{"id":"msg-1","model":"gpt-5.6-sol","usage":{"input_tokens":5,"output_tokens":0}}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
				`{"type":"message_stop"}`,
			},
		},
		"success: null frame between plan-tool blocks leaves the index shift intact": {
			cfg: sse.TransformerConfig{AdvertisedModel: advertised, BackendModel: backend, Provider: "codex", FilterPlanTools: true},
			chunks: []string{
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"plan\",\"name\":\"EnterPlanMode\",\"input\":{}}}\n\n" +
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"data: null\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"bash\",\"name\":\"Bash\",\"input\":{}}}\n\n" +
					"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
					"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\n",
			},
			wantOut: "data: null\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"bash\",\"name\":\"Bash\",\"input\":{}}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\n",
			wantValid: []string{
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"plan","name":"EnterPlanMode","input":{}}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"bash","name":"Bash","input":{}}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`,
			},
		},
		"success: null frame split across chunks before an LM Studio usage event": {
			cfg: sse.TransformerConfig{AdvertisedModel: "claude-lmstudio/qwen3-coder-30b", BackendModel: "lmstudio/qwen3-coder-30b", Provider: sse.LMStudioProvider},
			chunks: []string{
				"data: nu",
				"ll\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":30000,\"output_tokens\":2,\"cache_read_input_tokens\":29990}}\n\n",
			},
			wantOut: "data: null\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_input_tokens\":29990}}\n\n",
			wantValid: []string{
				`{"type":"message_delta","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":29990}}`,
			},
			// 10 exclusive input tokens plus 29990 cache reads.
			wantCrossing: &sentry.Crossing{Usage: 30000, Trigger: sentryTrigger},
		},
		"success: null frame after a crossing usage event leaves the sentry's recorded usage": {
			cfg: sse.TransformerConfig{AdvertisedModel: advertised, BackendModel: backend, Provider: "codex"},
			chunks: []string{
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":150,\"output_tokens\":4}}\n\n" +
					"data: null\n\n" +
					"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
			wantOut: "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":150,\"output_tokens\":4}}\n\n" +
				"data: null\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			wantValid: []string{
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":150,"output_tokens":4}}`,
				`{"type":"message_stop"}`,
			},
			wantCrossing: &sentry.Crossing{Usage: 150, Trigger: sentryTrigger},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var attempt sse.Attempt
			capture := nullEventCapture(t)
			var state sentry.State
			var valid []string
			cfg := tt.cfg
			cfg.Observe = func(event jsonx.WireValue) {
				attempt.NoteEvent(event)
				// As the proxy relay observes a frame: never as an ordinary completed turn.
				state.ObserveWire(session, target, event, false)
				capture.ObserveEvent(event)
				if event.Kind != 'n' {
					valid = append(valid, mustEncodeWire(t, event))
				}
			}
			tr := sse.NewTransformer(cfg)
			var out bytes.Buffer
			emit := func(frame []byte) error {
				out.Write(frame)
				return nil
			}
			for _, chunk := range tt.chunks {
				if err := tr.Feed([]byte(chunk), emit); err != nil {
					t.Fatalf("Feed() error = %v", err)
				}
			}
			if err := tr.End(emit); err != nil {
				t.Fatalf("End() error = %v", err)
			}
			if diff := gocmp.Diff(tt.wantOut, out.String()); diff != "" {
				t.Errorf("wire output (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantValid, valid); diff != "" {
				t.Errorf("observed non-null events (-want +got):\n%s", diff)
			}

			var wantAttempt sse.Attempt
			wantCapture := nullEventCapture(t)
			var wantState sentry.State
			for _, raw := range tt.wantValid {
				event := mustDecodeWire(t, raw)
				wantAttempt.NoteEvent(event)
				wantState.ObserveWire(session, target, event, false)
				wantCapture.ObserveEvent(event)
			}
			if diff := gocmp.Diff(wantAttempt, attempt); diff != "" {
				t.Errorf("attempt accounting with the null event (-valid events only +observed):\n%s", diff)
			}
			crossing, fired := state.Check(session, target, ordinaryReq)
			var gotCrossing *sentry.Crossing
			if fired {
				gotCrossing = &crossing
			}
			if diff := gocmp.Diff(tt.wantCrossing, gotCrossing); diff != "" {
				t.Errorf("sentry check after the observed events (-want +got):\n%s", diff)
			}
			validCrossing, validFired := wantState.Check(session, target, ordinaryReq)
			if fired != validFired || crossing != validCrossing {
				t.Errorf("sentry check with the null event = (%+v, %t), valid events only = (%+v, %t)", crossing, fired, validCrossing, validFired)
			}
			if diff := gocmp.Diff(wantCapture.Snapshot(), capture.Snapshot()); diff != "" {
				t.Errorf("usage accounting with the null event (-valid events only +observed):\n%s", diff)
			}
			got, want := capture.Finish(), wantCapture.Finish()
			if (got == nil) != (want == nil) {
				t.Fatalf("usage record present = %t, want %t", got != nil, want != nil)
			}
			if got != nil {
				for _, key := range []string{"response_mode", "input_tokens", "output_tokens", "cache_read_tokens", "request_id"} {
					if diff := gocmp.Diff(want.Attributes[key], got.Attributes[key]); diff != "" {
						t.Errorf("usage record %s (-valid events only +observed):\n%s", key, diff)
					}
				}
			}
		})
	}
}

// nullEventCapture is a usage capture of a successful response with a fixed clock and identifiers.
func nullEventCapture(t *testing.T) *usage.Capture {
	t.Helper()
	c := usage.NewCapture(usage.NewBuilder(usage.Options{Now: func() time.Time { return time.Unix(1, 0) }, Random: bytes.NewReader(bytes.Repeat([]byte{0x33}, 1024))}), usage.Request{}, 0, nil)
	c.SetResponse(http.StatusOK, nil)
	return c
}

// TestEventObserverUTF8Budget checks the observer's pending-byte budget at a chunk boundary that
// splits a UTF-8 sequence. A trailing prefix that can still become a valid sequence is withheld
// and not counted until the next chunk; a trailing sequence that can no longer be completed is
// decoded at once, so its replacement characters count against the budget on the chunk that
// carries it. The decoded event text at completion is the same either way.
func TestEventObserverUTF8Budget(t *testing.T) {
	const (
		prefix = `data: {"type":"m","x":"`
		suffix = "\"}\n\n"
	)
	p := len(prefix)
	tests := map[string]struct {
		maxBytes int
		chunks   []string
		// wantOverflows is the cumulative overflow count after each Write and then after End.
		wantOverflows []int
		wantEvents    []string
	}{
		"error: malformed incomplete sequence ending the first chunk counts against the budget at once": {
			maxBytes:      p + 5, // the raw chunk is p+2 bytes; its two replacement characters are 6
			chunks:        []string{prefix + "\xe0\x80", suffix},
			wantOverflows: []int{1, 1, 1},
		},
		"error: surrogate lead with an impossible second byte counts against the budget at once": {
			maxBytes:      p + 5,
			chunks:        []string{prefix + "\xed\xa0", suffix},
			wantOverflows: []int{1, 1, 1},
		},
		"success: malformed split fits when the budget covers the decoded frame": {
			maxBytes:      p + 6 + len(suffix),
			chunks:        []string{prefix + "\xe0\x80", suffix},
			wantOverflows: []int{0, 0, 0},
			wantEvents:    []string{"{\"type\":\"m\",\"x\":\"��\"}"},
		},
		"error: malformed split one byte short of the decoded frame overflows on the completing chunk": {
			maxBytes:      p + 6 + len(suffix) - 1,
			chunks:        []string{prefix + "\xe0\x80", suffix},
			wantOverflows: []int{0, 1, 1},
		},
		"success: valid incomplete prefix at the budget boundary is withheld from the first chunk": {
			maxBytes:      p,
			chunks:        []string{prefix + "\xe0\xa0", "\x80" + suffix},
			wantOverflows: []int{0, 1, 1},
		},
		"success: valid split sequence completes into one character within the budget": {
			maxBytes:      p + 3 + len(suffix),
			chunks:        []string{prefix + "\xe0\xa0", "\x80" + suffix},
			wantOverflows: []int{0, 0, 0},
			wantEvents:    []string{"{\"type\":\"m\",\"x\":\"ࠀ\"}"},
		},
		"success: malformed sequence ending the stream is counted once at its decoded size": {
			maxBytes:      p + 6,
			chunks:        []string{prefix + "\xe0\x80"},
			wantOverflows: []int{0, 0},
		},
		"error: valid prefix left at the end of the stream counts when End decodes it": {
			maxBytes:      p + 2,
			chunks:        []string{prefix + "\xf0\x9f\x8e"},
			wantOverflows: []int{0, 1},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var events []string
			overflows := 0
			obs := sse.NewEventObserver(tt.maxBytes, func(event jsonx.WireValue) {
				events = append(events, mustEncodeWire(t, event))
			}, func() { overflows++ })
			var counts []int
			for _, chunk := range tt.chunks {
				obs.Write([]byte(chunk))
				counts = append(counts, overflows)
			}
			obs.End()
			counts = append(counts, overflows)
			if diff := gocmp.Diff(tt.wantOverflows, counts); diff != "" {
				t.Errorf("cumulative overflows after each Write and End (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantEvents, events); diff != "" {
				t.Errorf("observed events (-want +got):\n%s", diff)
			}
		})
	}
}
