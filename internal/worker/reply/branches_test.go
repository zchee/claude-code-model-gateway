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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestSuccessfulJSON(t *testing.T) {
	const inclusive = `{"input_tokens":3013,"cache_read_input_tokens":2497,"output_tokens":8}`
	const exclusive = `{"input_tokens":516,"cache_read_input_tokens":2497,"output_tokens":8}`
	const raw = `{"type":"message","model":"engine","stop_reason":"tool_use","content":[{"type":"tool_use","name":"ExitPlanMode"}],"usage":` + inclusive + `,"nested":[{"model":"gpt-inner"},{"model":"unrelated"}]}`
	const filtered = `{"type":"message","model":"shown","stop_reason":"end_turn","content":[],"usage":` + inclusive + `,"nested":[{"model":"shown"},{"model":"unrelated"}]}`
	tests := map[string]struct {
		provider, body, want, observed, override string
		filter                                   bool
		usage                                    float64
	}{
		"LM Studio corrects before observation and rewrite": {"lmstudio", raw, strings.ReplaceAll(strings.ReplaceAll(raw, `"engine"`, `"shown"`), `"gpt-inner"`, `"shown"`), strings.Replace(raw, inclusive, exclusive, 1), "", false, 3013},
		"Codex removes only plan tool":                      {"codex", raw, filtered, raw, "", true, 5510},
		"real tool keeps tool use stop":                     {"codex", `{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","name":"EnterPlanMode"},{"type":"tool_use","name":"Bash"}]}`, `{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash"}]}`, "", "", true, 0},
		"error envelope not completed":                      {"codex", `{"type":"message","error":{"message":"failure"}}`, `{"type":"message","error":{"message":"failure"}}`, "", "upstream_error", false, 0},
		"malformed bytes not committed as success":          {"codex", " \xff {", " \xff {", "", "upstream_error", false, 0},
		"wire order and duplicate model":                    {"kimi", `{ "z":1e400, "12":"twelve", "2":"two", "model":"ignored", "model":"engine", "s":"\ud800" }`, `{"2":"two","12":"twelve","z":null,"model":"shown","s":"\ud800"}`, "", "upstream_error", false, 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var observed []string
			state := new(sentry.State)
			target := sentry.Target{Key: "engine", Policy: &policy.Sentry{BackendWindow: 3000, CompactTrigger: 2900, Source: "derived"}, Headroom: 100}
			r := reply.Request{Provider: tc.provider, AdvertisedModel: "shown", BackendModel: "engine", FilterPlanTools: tc.filter, Session: "session", Body: []byte(`{"messages":[]}`), Target: target, Sentry: state, SentryEnabled: true, ObserveJSON: func(b []byte) { observed = append(observed, string(b)) }}
			w, result := handle(t, r, answer(t, 200, tc.body).forward)
			want := tc.want
			if tc.provider == "lmstudio" {
				want = strings.Replace(want, inclusive, exclusive, 1)
			}
			equal(t, 200, w.Code)
			equal(t, want, w.Body.String())
			equal(t, strconv.Itoa(len(want)), w.Header().Get("Content-Length"))
			wantObserved := tc.observed
			if wantObserved == "" {
				wantObserved = tc.body
			}
			equal(t, []string{wantObserved}, observed)
			equal(t, tc.override, result.StatusOverride)
			equal(t, 200, result.Status)
			crossed, ok := state.Check("session", target, r.Body)
			equal(t, tc.usage > 0, ok)
			equal(t, tc.usage, crossed.Usage)
		})
	}
}

func TestEmptyCompletion(t *testing.T) {
	const input = `{"message":"Codex completed without producing output","ignored":"context window"}`
	const id = "msg_gateway_765432101234456789ab0123456789ab"
	const prefix = `{"id":"` + id + `","type":"message","role":"assistant","model":"shown","content":[],"stop_reason":`
	const suffix = `,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	const stream = "event: message_start\ndata: " + `{"type":"message_start","message":` + prefix + `null` + suffix + "}\n\nevent: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":0}}` + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	tests := map[string]struct {
		provider, request, up string
		status                int
		contentType, body     string
	}{
		"JSON before legacy normalization": {"codex", `{"stream":"true"}`, input, 200, "application/json", prefix + `"end_turn"` + suffix},
		"requested SSE":                    {"codex", `{"stream":true}`, input, 200, "text/event-stream", stream},
		"ordinary 503":                     {"codex", `{}`, `{"message":"upstream overloaded"}`, 503, "application/json", `{"message":"upstream overloaded"}`},
		"other provider":                   {"kimi", `{}`, `{"message":"Codex completed without producing output"}`, 503, "application/json", `{"message":"Codex completed without producing output"}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w, result := handle(t, reply.Request{Provider: tc.provider, AdvertisedModel: "shown", Body: []byte(tc.request), NewMessageID: func() string { return id }}, answer(t, 503, tc.up).forward)
			equal(t, tc.status, w.Code)
			equal(t, tc.status, result.Status)
			equal(t, tc.contentType, w.Header().Get("Content-Type"))
			equal(t, tc.body, w.Body.String())
			equal(t, strconv.Itoa(len(tc.body)), w.Header().Get("Content-Length"))
		})
	}
}

func TestRejectionAttribution(t *testing.T) {
	tests := map[string]struct {
		provider string
		want     bool
	}{"attributed Codex": {"codex", true}, "other provider": {"kimi", false}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			store := &upstreamstate.Store{Dir: t.TempDir(), Now: func() time.Time { return time.Unix(1000, 0) }}
			const body = `{"error":{"message":"provider rejection"}}`
			w, result := handle(t, reply.Request{Provider: tc.provider, AdvertisedModel: "shown", Body: []byte(`{}`), Upstream: store}, upstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-OpenAI-Request-Id", "private-id")
				w.Header().Add("Retry-After", "1")
				w.Header().Add("Retry-After", "60")
				w.WriteHeader(429)
				_, _ = io.WriteString(w, body)
			}).forward)
			equal(t, 429, w.Code)
			equal(t, body, w.Body.String())
			equal(t, 429, result.CaptureStatus)
			equal(t, []string{"1", "60"}, result.CaptureHeaders.Values("Retry-After"))
			equal(t, int64(len(body)), result.ResponseBytes)
			block := store.ReadBlocked()
			if (block != nil) != tc.want {
				t.Fatalf("ReadBlocked = %+v, want present=%v", block, tc.want)
			}
			if block != nil {
				equal(t, 429, block.StatusCode)
				equal(t, new(float64(1001000)), block.ExpiresAtMs)
				if strings.Contains(block.Evidence, "private-id") {
					t.Error("evidence contains a header value")
				}
			}
		})
	}
}

type countedWriter struct {
	*httptest.ResponseRecorder
	heads []int
}

func (w *countedWriter) WriteHeader(status int) {
	w.heads = append(w.heads, status)
	w.ResponseRecorder.WriteHeader(status)
}

func TestCommittedCompaction(t *testing.T) {
	tests := map[string]struct {
		status       int
		body, detail string
	}{"forbidden before ordinary rejection": {403, `{"error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`, "model-gateway: upstream returned 403: WebSocket upgrade was rejected"}, "successful JSON cannot change committed stream": {200, `{}`, "model-gateway: upstream returned 200 with no readable error body"}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			r, e := reply.Handle(t.Context(), w, reply.Request{Provider: "codex", AdvertisedModel: "shown", Compaction: true, HeadWritten: true, Body: []byte(`{}`)}, answer(t, tc.status, tc.body).forward)
			if e != nil {
				t.Fatal(e)
			}
			equal(t, []int{200}, w.heads)
			equal(t, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\""+tc.detail+"\"}}\n\n", w.Body.String())
			equal(t, tc.status, r.Status)
			equal(t, true, r.Incomplete)
			equal(t, "upstream_error", r.CompactionOutcome)
		})
	}
}

func TestBufferedReadFailure(t *testing.T) {
	tests := map[string]struct {
		status         int
		enabled, abort bool
	}{"enabled genuine": {413, true, false}, "other proxy failure": {500, true, false}, "forbidden": {403, true, true}, "sentry off": {413, false, true}, "JSON success": {200, true, true}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			result, e := reply.Handle(t.Context(), w, reply.Request{Provider: "codex", AdvertisedModel: "shown", Compaction: true, SentryEnabled: tc.enabled, Sentry: new(sentry.State), Body: []byte(`{}`)}, upstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "truncated")
			}).forward)
			if tc.abort {
				if !errors.Is(e, http.ErrAbortHandler) || w.Body.Len() != 0 {
					t.Errorf("must abort without a clean partial reply: err=%v body=%q", e, w.Body.String())
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			equal(t, 502, w.Code)
			equal(t, "application/json", w.Header().Get("Content-Type"))
			equal(t, `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`, w.Body.String())
			equal(t, tc.status, result.Status)
			if result.StatusOverride != "upstream_error" && result.StatusOverride != "upstream_aborted" {
				t.Errorf("unexpected incomplete status %q", result.StatusOverride)
			}
			equal(t, "upstream_error", result.CompactionOutcome)
			equal(t, true, result.Incomplete)
		})
	}
}

func TestTransportFailure(t *testing.T) {
	w, r := handle(t, reply.Request{Origin: "https://proxy.invalid", Compaction: true}, func(context.Context) (*http.Response, error) { return nil, errors.New("connection refused") })
	equal(t, 502, w.Code)
	equal(t, `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream https://proxy.invalid failed: connection refused"}}`, w.Body.String())
	equal(t, 502, r.Status)
	equal(t, "upstream_error", r.StatusOverride)
	equal(t, true, r.Incomplete)
}

func TestUnbufferedOwnership(t *testing.T) {
	tests := map[string]struct {
		provider, model   string
		status            int
		contentType, body string
		committed         bool
	}{"Anthropic genuine-looking failure": {"", "", 413, "application/json", ` { "error": {"type":"request_too_large", "message":"context window"} } `, false}, "proxy redirect": {"codex", "shown", 307, "application/octet-stream", "\x00\xff", false}, "proxy event stream": {"codex", "shown", 200, "text/event-stream; charset=utf-8", ":keep\n\n", false}, "committed successful stream": {"codex", "shown", 200, "text/event-stream", "data: {}\n\n", true}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w, r := handle(t, reply.Request{Provider: tc.provider, AdvertisedModel: tc.model, Compaction: tc.committed, HeadWritten: tc.committed, SentryEnabled: true}, upstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}).forward)
			if r.Stream == nil {
				t.Fatal("response was consumed instead of handed to the relay")
			}
			equal(t, 0, w.Body.Len())
			equal(t, tc.status, r.Stream.StatusCode)
			b, e := io.ReadAll(r.Stream.Body)
			if e != nil {
				t.Fatal(e)
			}
			equal(t, tc.body, string(b))
		})
	}
}
