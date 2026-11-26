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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestOverflowNormalization(t *testing.T) {
	const first = `{"messages":[{"role":"user","content":"private-prompt-🎵"}]}`
	const genuine = `{"type":"error","error":{"type":"request_too_large","message":"upstream wording (118 tokens > 95 tokens)"}}`
	const noCounts = `{"type":"error","error":{"type":"request_too_large","message":"upstream wording"}}`
	const engine = `{"error":{"type":"exceed_context_size_error","n_prompt_tokens":118,"n_ctx":95,"message":"private-backend-detail"}}`
	const legacy = `{"error":{"message":"private-backend-detail: context length exceeded"}}`
	const normalized = "Input exceeds the model context window; compact and retry."
	tests := map[string]struct {
		code          int
		body          string
		enabled       bool
		prior         int
		lead          string
		tokens, limit int
		from          string
		learns        bool
	}{
		"error: genuine with exact counts learns":          {413, genuine, true, 80, "upstream wording (118 tokens > 95 tokens)", 118, 95, "upstream", true},
		"error: genuine without counts uses previous turn": {413, noCounts, true, 80, "upstream wording (80 tokens > 110 tokens)", 80, 110, "previous_turn_usage", true},
		"error: genuine without measurements":              {413, noCounts, true, 0, "upstream wording (111 tokens > 110 tokens)", 0, 110, "", false},
		"error: engine with exact limit does not learn":    {400, engine, true, 80, normalized + " (118 tokens > 95 tokens)", 118, 95, "upstream", false},
		"error: engine missing its limit learns":           {500, `{"error":{"type":"exceed_context_size_error","n_prompt_tokens":118,"message":"private-backend-detail"}}`, true, 80, normalized + " (118 tokens > 110 tokens)", 118, 110, "upstream", true},
		"error: engine with sentry off uses own counts":    {400, engine, false, 80, normalized + " (118 tokens > 95 tokens)", 118, 95, "upstream", false},
		"error: legacy learns from previous turn":          {502, legacy, true, 80, normalized + " (80 tokens > 110 tokens)", 80, 110, "previous_turn_usage", true},
		"error: legacy with sentry off has no numbers":     {400, legacy, false, 80, normalized, 0, 110, "", false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			state := new(sentry.State)
			target := sentry.Target{Key: "model", Policy: &policy.Sentry{BackendWindow: 110, CompactTrigger: 100, Source: "derived"}, Headroom: 10}
			request := []byte(first)
			observe := func(session string, tokens int) {
				state.Observe(session, target, jsontext.Value(fmt.Sprintf(`{"type":"message_delta","usage":{"input_tokens":%d}}`, tokens)), false)
			}
			observe("only", tc.prior)
			w, result := handle(t, reply.Request{Provider: "local", AdvertisedModel: "shown", BackendModel: "model", Window: 110, Session: "only", Body: request, Target: target, Sentry: state, SentryEnabled: tc.enabled}, answer(t, tc.code, tc.body).forward)
			equal(t, 413, w.Code)
			equal(t, 413, result.Status)
			equal(t, tc.code, result.CaptureStatus)
			equal(t, "application/json", w.Header().Get("Content-Type"))
			equal(t, strconv.Itoa(w.Body.Len()), w.Header().Get("Content-Length"))
			if tc.code == 413 {
				equal(t, "kept", w.Header().Get("X-Upstream-Test"))
				equal(t, "", w.Header().Get("X-Model-Gateway-Upstream-Status"))
			} else {
				equal(t, "", w.Header().Get("X-Upstream-Test"))
				equal(t, strconv.Itoa(tc.code), w.Header().Get("X-Model-Gateway-Upstream-Status"))
			}
			var payload struct {
				Type  string `json:"type"`
				Error struct {
					Type     string         `json:"type"`
					Message  string         `json:"message"`
					Overflow map[string]any `json:"overflow"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			equal(t, "error", payload.Type)
			equal(t, "request_too_large", payload.Error.Type)
			details := map[string]any{"phase": "first_turn", "refused_by": "upstream", "request_bytes": float64(len(request)), "limit_tokens": float64(tc.limit), "tokens": nil, "tokens_from": nil}
			measured := "tokens UNVERIFIED"
			if tc.tokens > 0 {
				details["tokens"] = float64(tc.tokens)
				details["tokens_from"] = tc.from
				measured = fmt.Sprintf("%d tokens from %s", tc.tokens, tc.from)
			}
			equal(t, details, payload.Error.Overflow)
			equal(t, fmt.Sprintf("%s [model-gateway: phase first_turn, refused by upstream; request %d bytes, %s, limit %d tokens; recovery: nothing to compact, the request has no prior turn: shrink the first prompt (a dispatched subagent's briefing) or route to a larger-window model]", tc.lead, len(request), measured, tc.limit), payload.Error.Message)
			var logged map[string]any
			if err := json.Unmarshal(result.Overflow, &logged); err != nil {
				t.Fatal(err)
			}
			equal(t, details, logged)
			if strings.Contains(w.Body.String(), "private-") {
				t.Error("normalized public body leaked private text")
			}
			observe("only", 101)
			_, crossed := state.Check("only", target, request)
			equal(t, !tc.enabled, crossed)
			observe("other", 75)
			crossing, crossed := state.Check("other", target, request)
			equal(t, tc.learns, crossed)
			if tc.learns {
				equal(t, float64(70), crossing.Trigger)
			}
		})
	}
}

func TestEarlierResponseWins(t *testing.T) {
	tests := map[string]struct {
		code                 int
		provider, body, want string
		enabled              bool
	}{
		"error: forbidden precedes legacy overflow":            {403, "codex", `{"message":"context window","model":"gpt-hidden"}`, `{"message":"context window","model":"shown"}`, true},
		"error: disabled genuine overflow only rewrites model": {413, "local", `{"error":{"type":"request_too_large","message":"context window"},"model":"engine"}`, `{"error":{"type":"request_too_large","message":"context window"},"model":"shown"}`, false},
		"error: unrecognized genuine overflow":                 {413, "local", `{"error":{"type":"api_error","message":"context window"}}`, `{"error":{"type":"api_error","message":"context window"}}`, true},
		"error: non Codex auth error":                          {401, "kimi", `{"error":{"message":"OpenAI not authenticated"}}`, `{"error":{"message":"OpenAI not authenticated"}}`, true},
		"error: upstream invalid JSON stays raw":               {502, "kimi", "{invalid\xff", "{invalid\xff", true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w, r := handle(t, reply.Request{Provider: tc.provider, AdvertisedModel: "shown", BackendModel: "engine", Window: 110, Sentry: new(sentry.State), SentryEnabled: tc.enabled, Body: []byte(`{"messages":[]}`)}, answer(t, tc.code, tc.body).forward)
			equal(t, tc.code, w.Code)
			equal(t, tc.want, w.Body.String())
			equal(t, "kept", w.Header().Get("X-Upstream-Test"))
			equal(t, 0, len(r.Overflow))
		})
	}
}
