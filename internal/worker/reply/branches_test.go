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

//go:build process

package reply_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func TestSuccessfulJSON(t *testing.T) {
	const inclusive = `{"input_tokens":3013,"cache_read_input_tokens":2497,"output_tokens":8}`
	const exclusive = `{"input_tokens":516,"cache_read_input_tokens":2497,"output_tokens":8}`
	const raw = `{"type":"message","model":"engine","stop_reason":"tool_use","content":[{"type":"tool_use","name":"ExitPlanMode"}],"usage":` + inclusive + `,"nested":[{"model":"gpt-inner"},{"model":"unrelated"}]}`
	const alreadyExclusive = `{"type":"message","usage":` + exclusive + `}`
	const filtered = `{"type":"message","model":"shown","stop_reason":"end_turn","content":[],"usage":` + inclusive + `,"nested":[{"model":"shown"},{"model":"unrelated"}]}`
	tests := map[string]struct {
		provider, body, want, observed, override string
		filter                                   bool
		usage                                    float64
	}{
		"success: LM Studio corrects before observation and rewrite": {"lmstudio", raw, strings.ReplaceAll(strings.ReplaceAll(raw, `"engine"`, `"shown"`), `"gpt-inner"`, `"shown"`), strings.Replace(raw, inclusive, exclusive, 1), "", false, 3013},
		"success: LM Studio preserves already exclusive input":       {"lmstudio", alreadyExclusive, alreadyExclusive, alreadyExclusive, "", false, 3013},
		"success: Codex removes only plan tool":                      {"codex", raw, filtered, raw, "", true, 5510},
		"success: real tool keeps tool use stop":                     {"codex", `{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","name":"EnterPlanMode"},{"type":"tool_use","name":"Bash"}]}`, `{"type":"message","stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash"}]}`, "", "", true, 0},
		"error: error envelope not completed":                        {"codex", `{"type":"message","error":{"message":"failure"}}`, `{"type":"message","error":{"message":"failure"}}`, "", "upstream_error", false, 0},
		"error: malformed bytes not committed as success":            {"codex", " \xff {", " \xff {", "", "upstream_error", false, 0},
		"success: wire order and duplicate model":                    {"kimi", `{ "z":1e400, "12":"twelve", "2":"two", "model":"ignored", "model":"engine", "s":"\ud800" }`, `{"2":"two","12":"twelve","z":null,"model":"shown","s":"\ud800"}`, "", "upstream_error", false, 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			capture := newCapture()
			state := new(sentry.State)
			target := sentry.Target{Key: "engine", Policy: &policy.Sentry{BackendWindow: 3000, CompactTrigger: 2900, Source: "derived"}, Headroom: 100}
			r := reply.Request{Provider: tc.provider, AdvertisedModel: "shown", BackendModel: "engine", FilterPlanTools: tc.filter, Session: "session", Body: []byte(`{"messages":[]}`), Target: target, Sentry: state, SentryEnabled: true, Usage: capture}
			w, result := handle(t, r, answer(t, 200, tc.body))
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
			fields := map[string]any{}
			if tc.body == raw {
				fields["model"] = "engine"
			}
			if tc.usage > 0 {
				fields["input_tokens"] = tc.usage - 2497
				fields["cache_read_tokens"] = float64(2497)
			}
			checkCapture(t, capture, 200, len(wantObserved), fields)
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
		"success: JSON before legacy normalization": {"codex", `{"stream":"true"}`, input, 200, "application/json", prefix + `"end_turn"` + suffix},
		"success: requested SSE":                    {"codex", `{"stream":true}`, input, 200, "text/event-stream", stream},
		"error: ordinary 503":                       {"codex", `{}`, `{"message":"upstream overloaded"}`, 503, "application/json", `{"message":"upstream overloaded"}`},
		"error: other provider":                     {"kimi", `{}`, `{"message":"Codex completed without producing output"}`, 503, "application/json", `{"message":"Codex completed without producing output"}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w, result := handle(t, reply.Request{Provider: tc.provider, AdvertisedModel: "shown", Body: []byte(tc.request), NewMessageID: func() string { return id }}, answer(t, 503, tc.up))
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
		status   int
		want     bool
		expires  *float64
	}{
		"error: attributed Codex rate limit":         {"codex", 429, true, new(float64(1001000))},
		"error: other provider":                      {"kimi", 429, false, nil},
		"error: ordinary attributed Codex forbidden": {"codex", 403, true, nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			store := &upstreamstate.Store{Dir: t.TempDir(), Now: func() time.Time { return time.Unix(1000, 0) }}
			const body = `{"error":{"message":"provider rejection"}}`
			capture := newCapture()
			w, _ := handle(t, reply.Request{Provider: tc.provider, AdvertisedModel: "shown", Body: []byte(`{}`), Upstream: store, Usage: capture}, newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-OpenAI-Request-Id", "private-id")
				w.Header().Add("Retry-After", "1")
				w.Header().Add("Retry-After", "60")
				w.Header().Add("X-Codex-Primary-Used-Percent", "31")
				w.Header().Add("X-Codex-Primary-Used-Percent", "99")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, body)
			}))
			equal(t, tc.status, w.Code)
			equal(t, body, w.Body.String())
			checkCapture(t, capture, tc.status, len(body), map[string]any{"retry_after_ms": float64(1000), "codex_throttle_used_percent": float64(31)})
			block := store.ReadBlocked()
			if (block != nil) != tc.want {
				t.Fatalf("ReadBlocked = %+v, want present=%v", block, tc.want)
			}
			if block != nil {
				equal(t, tc.status, block.StatusCode)
				equal(t, tc.expires, block.ExpiresAtMs)
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
		status             int
		body, kind, detail string
		truncated          bool
	}{
		"error: forbidden before ordinary rejection":            {403, `{"error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`, "api_error", "model-gateway: upstream returned 403: WebSocket upgrade was rejected", false},
		"error: successful JSON cannot change committed stream": {200, `{}`, "api_error", "model-gateway: upstream returned 200 with no readable error body", false},
		"error: overflow latches session and learns ceiling":    {413, `{"error":{"type":"request_too_large","message":"too large"}}`, "api_error", "model-gateway: upstream returned 413: too large", false},
		"error: attributed unauthorized without detail":         {401, "", "authentication_error", "Codex dispatch refused: ChatGPT sign-in is required. Run `\"/stable gateway\" login`, finish browser OAuth, then run `\"/stable gateway\" setup` and retry. Credentials live in `~/.config/claude-code-proxy/`. Upstream detail: HTTP 401", false},
		"error: truncated response":                             {500, `{"message":"partial response"}`, "api_error", "model-gateway: upstream returned 500: partial response", true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			state := new(sentry.State)
			target := sentry.Target{Key: "model", Policy: &policy.Sentry{BackendWindow: 110, CompactTrigger: 100, Source: "derived"}, Headroom: 10}
			store := &upstreamstate.Store{Dir: t.TempDir(), Now: func() time.Time { return time.Unix(1000, 0) }}
			capture := newCapture()
			req := reply.Request{Usage: capture, Provider: "codex", AdvertisedModel: "shown", Compaction: true, HeadWritten: true, Body: []byte(`{}`), Session: "only", Target: target, SentryEnabled: true, Sentry: state, Upstream: store, CommandPath: "/stable gateway"}
			state.Observe("only", target, []byte(`{"type":"message_delta","usage":{"input_tokens":80}}`), false)
			r, e := reply.Handle(t.Context(), w, req, newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.status == 401 {
					w.Header().Set("X-Openai-Request-Id", "req_test")
				}
				if tc.truncated {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+1))
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}).forward(req.Body))
			if e != nil {
				t.Fatal(e)
			}
			equal(t, []int{200}, w.heads)
			equal(t, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":"+strconv.Quote(tc.kind)+",\"message\":"+strconv.Quote(tc.detail)+"}}\n\n", w.Body.String())
			equal(t, tc.status, r.Status)
			equal(t, true, r.Contacted && r.Stream == nil)
			checkIncomplete(t, capture)
			equal(t, &outcome.Compaction{Outcome: "upstream_error", ErrorCode: "upstream_http_error", UpstreamStatus: new(tc.status)}, r.Route.Compaction)
			if tc.truncated && r.StatusOverride != "upstream_error" && r.StatusOverride != "upstream_aborted" {
				t.Errorf("unexpected truncated response status %q", r.StatusOverride)
			}
			if tc.status == 401 {
				block := store.ReadBlocked()
				if block == nil {
					t.Fatal("missing attributed Codex rejection")
				}
				equal(t, 401, block.StatusCode)
				equal(t, (*float64)(nil), block.ExpiresAtMs)
			}
			if tc.status == 413 {
				state.Observe("only", target, []byte(`{"type":"message_delta","usage":{"input_tokens":101}}`), false)
				_, crossed := state.Check("only", target, req.Body)
				equal(t, false, crossed)
				state.Observe("other", target, []byte(`{"type":"message_delta","usage":{"input_tokens":75}}`), false)
				crossing, crossed := state.Check("other", target, req.Body)
				equal(t, true, crossed)
				equal(t, float64(70), crossing.Trigger)
			}
		})
	}
}

func TestBufferedReadFailure(t *testing.T) {
	tests := map[string]struct {
		status         int
		enabled, abort bool
	}{
		"error: enabled genuine":     {413, true, false},
		"error: other proxy failure": {500, true, false},
		"error: forbidden":           {403, true, true},
		"error: sentry off":          {413, false, true},
		"error: JSON success":        {200, true, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			capture := newCapture()
			req := reply.Request{Usage: capture, Provider: "codex", AdvertisedModel: "shown", Compaction: true, SentryEnabled: tc.enabled, Sentry: new(sentry.State), Body: []byte(`{}`)}
			result, e := reply.Handle(t.Context(), w, req, newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "100")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "truncated")
			}).forward(req.Body))
			equal(t, true, result.Contacted && result.Stream == nil)
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
			equal(t, &outcome.Compaction{Outcome: "upstream_error", ErrorCode: "upstream_http_error", UpstreamStatus: new(tc.status)}, result.Route.Compaction)
			checkIncomplete(t, capture)
		})
	}
}

func TestTransportFailure(t *testing.T) {
	tests := map[string]struct{ committed bool }{
		"error: before response headers": {false},
		"error: after response headers":  {true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
			if tc.committed {
				w.WriteHeader(200)
			}
			fixture := answer(t, 200, "")
			fixture.server.Close()
			capture := newCapture()
			req := reply.Request{Usage: capture, Origin: fixture.server.URL, Compaction: true, HeadWritten: tc.committed}
			send := fixture.forward(req.Body)
			var refused error
			r, err := reply.Handle(t.Context(), w, req, func(ctx context.Context) (*upstream.Response, error) {
				resp, err := send(ctx)
				refused = err
				return resp, err
			})
			if !errors.Is(refused, syscall.ECONNREFUSED) {
				t.Fatalf("expected a refused loopback connection, got %v (Handle: %v)", refused, err)
			}
			if tc.committed {
				if !errors.Is(err, http.ErrAbortHandler) {
					t.Fatalf("committed response: got %v, want http.ErrAbortHandler", err)
				}
				equal(t, []int{200}, w.heads)
				equal(t, 0, w.Body.Len())
			} else {
				if err != nil {
					t.Fatal(err)
				}
				equal(t, []int{502}, w.heads)
				equal(t, `{"type":"error","error":{"type":"api_error","message":`+strconv.Quote("model-gateway shim: upstream "+req.Origin+" failed: "+refused.Error())+`}}`, w.Body.String())
			}
			equal(t, 502, r.Status)
			equal(t, "upstream_error", r.StatusOverride)
			equal(t, true, r.Contacted && r.Stream == nil)
			equal(t, &outcome.Compaction{Outcome: "upstream_error", ErrorCode: "upstream_http_error", UpstreamStatus: new(502)}, r.Route.Compaction)
			checkIncomplete(t, capture)
		})
	}
}

func TestUnbufferedOwnership(t *testing.T) {
	tests := map[string]struct {
		provider, model   string
		status            int
		contentType, body string
		committed         bool
	}{
		"error: Anthropic genuine-looking failure": {"", "", 413, "application/json", ` { "error": {"type":"request_too_large", "message":"context window"} } `, false},
		"success: proxy redirect":                  {"codex", "shown", 307, "application/octet-stream", "\x00\xff", false},
		"success: proxy event stream":              {"codex", "shown", 200, "text/event-stream; charset=utf-8", ":keep\n\n", false},
		"success: committed successful stream":     {"codex", "shown", 200, "text/event-stream", "data: {}\n\n", true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			capture := newCapture()
			req := reply.Request{Provider: tc.provider, AdvertisedModel: tc.model, Compaction: tc.committed, HeadWritten: tc.committed, SentryEnabled: true, Usage: capture}
			w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
			r, err := reply.Handle(t.Context(), w, req, newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}).forward(req.Body))
			if err != nil {
				t.Fatal(err)
			}
			equal(t, true, r.Contacted)
			equal(t, 0, len(w.heads))
			if r.Stream == nil {
				t.Fatal("response was consumed instead of handed to the relay")
			}
			t.Cleanup(func() { _ = r.Stream.Body.Close() })
			checkCapture(t, capture, tc.status, 0, map[string]any{"retry_after_ms": float64(2000)})
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
