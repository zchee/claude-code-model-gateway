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

package reply_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestMain(m *testing.M) { venue.Require(); goleak.VerifyTestMain(m) }

func equal[T any](t *testing.T, want, got T) {
	t.Helper()
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestOpenAIRejectionEvidence(t *testing.T) {
	tests := map[string]struct {
		status  int
		headers http.Header
		body    string
		want    bool
	}{
		"header": {403, http.Header{"X-Openai-Request-Id": {"req_1"}}, "", true}, "body": {429, nil, "OpenAI rejected this request", true}, "unattributed": {403, nil, "forbidden", false}, "server_error": {500, http.Header{"X-Openai-Request-Id": {"req_1"}}, "", false}, "word_boundary": {401, nil, "notopenai", false}, "processing_header": {401, http.Header{"OPENAI-PROCESSING-MS": {""}}, "", true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			equal(t, tc.want, reply.HasOpenAIRejectionEvidence(tc.status, tc.headers, []byte(tc.body)))
		})
	}
}

func TestWriteSentry(t *testing.T) {
	original := []byte(" {\"messages\":[{\"role\":\"assistant\",\"content\":\"🎵\"}]}\n")
	tests := map[string]struct{ provider, label string }{
		"error: Codex proxy": {"codex", "Codex"},
		"error: local proxy": {"local", "Local GGUF"},
		"error: Grok":        {"grok", "Grok"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r, err := reply.WriteSentry(w, original, tc.provider, sentry.Crossing{Usage: 91, Trigger: 80})
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf(`{"type":"error","error":{"type":"request_too_large","message":"Prompt is too long for the %s context window; compact and retry. (91 tokens > 80 tokens) [model-gateway: phase continuation, refused by gateway_sentry; request %d bytes, 91 tokens from previous_turn_usage, limit 80 tokens; recovery: compact the conversation and retry]","overflow":{"phase":"continuation","refused_by":"gateway_sentry","request_bytes":%d,"tokens":91,"tokens_from":"previous_turn_usage","limit_tokens":80}}}`, tc.label, len(original), len(original))
			equal(t, want, w.Body.String())
			equal(t, http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(want))}}, w.Result().Header)
			equal(t, 413, w.Code)
			equal(t, 413, r.Status)
			equal(t, false, r.Contacted)
			equal(t, &outcome.Overflow{Phase: "continuation", RefusedBy: "gateway_sentry", RequestBytes: int64(len(original)), Tokens: new(91.0), TokensFrom: "previous_turn_usage", LimitTokens: new(80.0)}, r.Route.Overflow)
		})
	}
}

func TestWriteEngineOverflow(t *testing.T) {
	prepared := []byte(`{"messages":[{"role":"assistant"}],"model":"engine"}`)
	tests := map[string]struct {
		status                                        int
		enabled                                       bool
		prior                                         int
		counts                                        sentry.Counts
		tokens                                        *float64
		limit                                         float64
		from, numbers, measured, wireTokens, wireFrom string
		learns                                        bool
	}{
		"error: first SSE event with exact limit":      {200, true, 80, sentry.Counts{Tokens: new(118.0), Limit: new(95.0)}, new(118.0), 95, "upstream", " (118 tokens > 95 tokens)", "118 tokens from upstream", "118", `"upstream"`, false},
		"error: engine omitted both counts":            {400, true, 80, sentry.Counts{}, new(80.0), 110, "previous_turn_usage", " (80 tokens > 110 tokens)", "80 tokens from previous_turn_usage", "80", `"previous_turn_usage"`, true},
		"error: enabled without counts or prior usage": {400, true, 0, sentry.Counts{}, nil, 110, "", " (111 tokens > 110 tokens)", "tokens UNVERIFIED", "null", "null", false},
		"error: disabled sentry":                       {500, false, 80, sentry.Counts{}, nil, 110, "", "", "tokens UNVERIFIED", "null", "null", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			state := new(sentry.State)
			target := sentry.Target{Key: "model", Policy: &policy.Sentry{BackendWindow: 110, CompactTrigger: 100, Source: "derived"}, Headroom: 10}
			if tc.prior > 0 {
				state.Observe("session", target, fmt.Appendf(nil, `{"type":"message_delta","usage":{"input_tokens":%d}}`, tc.prior), false)
			}
			w := httptest.NewRecorder()
			r, err := reply.WriteEngineOverflow(w, reply.Request{Body: prepared, Provider: "lmstudio", Window: 110, Sentry: state, Session: "session", Target: target, SentryEnabled: tc.enabled}, tc.status, tc.counts)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf(`{"type":"error","error":{"type":"request_too_large","message":"Input exceeds the model context window; compact and retry.%s [model-gateway: phase continuation, refused by upstream; request %d bytes, %s, limit %g tokens; recovery: compact the conversation and retry]","overflow":{"phase":"continuation","refused_by":"upstream","request_bytes":%d,"tokens":%s,"tokens_from":%s,"limit_tokens":%g}}}`, tc.numbers, len(prepared), tc.measured, tc.limit, len(prepared), tc.wireTokens, tc.wireFrom, tc.limit)
			equal(t, want, w.Body.String())
			equal(t, http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(want))}, "X-Model-Gateway-Upstream-Status": {strconv.Itoa(tc.status)}}, w.Result().Header)
			equal(t, 413, w.Code)
			equal(t, 413, r.Status)
			equal(t, true, r.Contacted)
			equal(t, &outcome.Overflow{Phase: "continuation", RefusedBy: "upstream", RequestBytes: int64(len(prepared)), Tokens: tc.tokens, TokensFrom: tc.from, LimitTokens: new(tc.limit)}, r.Route.Overflow)
			state.Observe("session", target, []byte(`{"type":"message_delta","usage":{"input_tokens":101}}`), false)
			_, crossed := state.Check("session", target, prepared)
			equal(t, !tc.enabled, crossed)
			state.Observe("other", target, []byte(`{"type":"message_delta","usage":{"input_tokens":75}}`), false)
			cross, crossed := state.Check("other", target, prepared)
			equal(t, tc.learns, crossed)
			if tc.learns {
				equal(t, 70.0, cross.Trigger)
			}
		})
	}
}
