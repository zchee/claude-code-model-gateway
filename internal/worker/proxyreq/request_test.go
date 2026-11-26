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

package proxyreq_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestMain(m *testing.M) {
	venue.Require()
	goleak.VerifyTestMain(m)
}

func newHandler(t *testing.T, dir string, env map[string]string, state *sentry.State) *proxyreq.Handler {
	t.Helper()
	h, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: "routes.json", Now: func() time.Time { return time.UnixMilli(1234) }, Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Sentry: state})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func prepare(t testing.TB, h *proxyreq.Handler, s route.Snapshot, path, raw string, headers http.Header) (proxyreq.Result, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
	r.Header = headers.Clone()
	d := s.Classify(r.Method, r.RequestURI, []byte(raw))
	if d.Branch != "proxy" {
		t.Fatalf("fixture classified %q as %+v", raw, d)
	}
	original := []byte(raw)
	w := httptest.NewRecorder()
	out, err := h.Prepare(w, r, s, d, original)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, []byte(raw)) {
		t.Fatal("original request bytes were modified")
	}
	return out, w
}

const (
	threadRefusal   = `{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: beta_header:message-threads-2026-08-12; the model-gateway proxy and Grok backends hold no conversation state. Resend this turn with the full message history.","details":{"error_code":"thread_unsupported_request"}}}`
	schema          = `{"not":{"pattern":"[^\\p{Cc}a]"}}`
	propertyQuirk   = `{"model":"claude-gpt-5.6-sol[1m]","tools":[{"name":"artifact_tool","input_schema":{"pattern":"[^\\p{Ō}x]"}}]}`
	propertyRefusal = `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway: Codex tool schema compatibility refused tool artifact_tool at /tools/0/input_schema/pattern; reason=invalid-regex. Nothing was forwarded or rerouted."}}`
)

func TestPrepare(t *testing.T) {
	const model = `"model":"claude-gpt-5.6-sol[1m]"`
	const adapted = `"model":"gpt-5.6-sol"`
	const path = "/v1/messages"
	const additions = `[{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"New","defer_loading":true}}},{"type":"tool_addition","tool":{"type":"tool_reference","name":"Client"}},{"type":"tool_addition","tool":{"type":"tool_reference","name":"advisor"}}]},{"role":"user","content":[{"type":"tool_result","content":[{"type":"tool_reference","tool_name":"Nested"}]}]}]`
	const tools = `[{"name":"EnterPlanMode"},{"name":"ExitPlanMode"},{"name":"Client","defer_loading":true,"input_schema":{}},{"name":"advisor","defer_loading":true},{"name":"Nested","defer_loading":true},{"name":"Unused","defer_loading":true}]`
	const resolved = `"messages":[{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"Tool reference: Nested"}]}]}],"tools":[{"name":"Client","input_schema":{}},{"name":"Nested"},{"name":"New"}]`
	tests := map[string]struct {
		path, raw, wantBody, reason, refusal string
		keep                                 bool
	}{
		"success: Codex adapts a schema hoisted from tool additions": {
			path:     path,
			raw:      `{` + model + `,"messages":[{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"File","input_schema":{"type":"string","pattern":"[^\\p{Cc}a]"}}}}]}]}`,
			wantBody: `{` + adapted + `,"messages":[],"tools":[{"name":"File","input_schema":{"type":"string","pattern":"[^a]"}}]}`,
		},
		"success: count tokens preserves thread but resolves tools":            {path: path + "/count_tokens?beta=true", raw: `{` + model + `,"thread":null,"messages":` + additions + `,"tools":` + tools + `}`, wantBody: `{` + adapted + `,"thread":null,` + resolved + `}`},
		"success: plan tools escape hatch":                                     {path: path, keep: true, raw: `{` + model + `,"tools":[{"name":"ExitPlanMode","defer_loading":false}]}`, wantBody: `{` + adapted + `,"tools":[{"name":"ExitPlanMode"}]}`},
		"success: self contained thread and full history":                      {path: path, raw: `{` + model + `,"thread":{"type":"create"},"messages":[{"role":"assistant","content":"history"}]}`, wantBody: `{` + adapted + `,"thread":{"type":"create"},"messages":[{"role":"assistant","content":"history"}]}`},
		"success: non Codex schema stays unchanged but plan tools are removed": {path: path, raw: `{"model":"claude-lmstudio/tiny","tools":[{"name":"ExitPlanMode"},{"name":"X","input_schema":` + schema + `}]}`, wantBody: `{"model":"lmstudio/tiny","tools":[{"name":"X","input_schema":` + schema + `}]}`},
		"error: non ASCII property name is not narrowed":                       {path: path, raw: propertyQuirk, reason: "invalid_tool_schema", refusal: propertyRefusal},
		"error: thread precedes dispatch and schema":                           {path: path + "?beta=true", raw: `{"model":"claude-codex-auto","thread":null,"tools":[{"name":"X","input_schema":` + schema + `}]}`, reason: "thread_unsupported", refusal: threadRefusal},
		"error: schema precedes plan removal even on count tokens":             {path: path + "/count_tokens", raw: `{` + model + `,"tools":[{"name":"ExitPlanMode","input_schema":` + schema + `}]}`, reason: "invalid_tool_schema", refusal: `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway: Codex tool schema compatibility refused tool ExitPlanMode at /tools/0/input_schema/not; reason=forbidden-schema-applicator. Nothing was forwarded or rerouted."}}`},
	}
	s := route.NewSnapshot(policy.NewResolver(policy.ResolverInputs{Routes: proxymodels.NewTable([]proxymodels.Route{{ID: "lmstudio/tiny", Provider: "lmstudio"}})}), "", nil, nil)
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			if tc.keep {
				env["CODEX_GATEWAY_KEEP_PLAN_TOOLS"] = "1"
			}
			out, w := prepare(t, newHandler(t, t.TempDir(), env, new(sentry.State)), s, tc.path, tc.raw, nil)
			if diff := gocmp.Diff(tc.wantBody, string(out.Body)); diff != "" {
				t.Error(diff)
			}
			if tc.reason != "" {
				if out.LocalStatus != 400 || out.Reason != tc.reason || w.Code != 400 || w.Header().Get("Content-Type") != "application/json" {
					t.Errorf("local result %+v, HTTP %d %v", out, w.Code, w.Header())
				}
				if diff := gocmp.Diff(tc.refusal, w.Body.String()); diff != "" {
					t.Error(diff)
				}
			} else {
				selected, effective, provider := "claude-gpt-5.6-sol[1m]", "gpt-5.6-sol", "codex"
				if strings.Contains(tc.raw, `"claude-lmstudio/tiny"`) {
					selected, effective, provider = "claude-lmstudio/tiny", "lmstudio/tiny", "lmstudio"
				}
				if out.LocalStatus != 0 || out.Via != "direct" || out.SelectedModel != selected || out.EffectiveModel != effective || out.Provider != provider {
					t.Errorf("unexpected direct route: %+v", out)
				}
			}
		})
	}
}

func TestDispatchLifetime(t *testing.T) {
	const marker = "[sidequest-route model=gpt-5.6-sol effort=high ticket=SQ-123]"
	const auto = "claude-codex-auto"
	dir := t.TempDir()
	h := newHandler(t, dir, nil, new(sentry.State))
	turn := func(content string, headers http.Header, userID, via string) {
		t.Helper()
		metadata := ""
		if userID != "" {
			metadata = fmt.Sprintf(`,"metadata":{"user_id":%q}`, userID)
		}
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%s}],"output_config":{"effort":"low","other":1}%s}`, auto, content, metadata)
		out, w := prepare(t, h, route.Snapshot{}, "/v1/messages", body, headers)
		if via == "" {
			if out.LocalStatus != 400 || out.Reason != "invalid_route" || w.Code != 400 || len(out.Body) != 0 {
				t.Fatalf("unbound route forwarded or not refused: %+v", out)
			}
			const refusal = `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway: dispatch model requires exactly one [sidequest-route model=... effort=... ticket=...] marker in the conversation; redispatch the ticket"}}`
			if diff := gocmp.Diff(refusal, w.Body.String()); diff != "" || w.Header().Get("Content-Type") != "application/json" {
				t.Errorf("unbound refusal Content-Type=%q, body (-want +got): %s", w.Header().Get("Content-Type"), diff)
			}
			return
		}
		want := fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":%s}],"output_config":{"effort":"high","other":1}%s}`, content, metadata)
		if diff := gocmp.Diff(want, string(out.Body)); diff != "" {
			t.Error(diff)
		}
		if out.Via != via || out.SelectedModel != auto || out.EffectiveModel != "gpt-5.6-sol" || out.Provider != "codex" || out.Effort == nil || *out.Effort != "high" || out.LocalStatus != 0 {
			t.Errorf("route = %+v", out)
		}
		wantTicket := new("SQ-123")
		if via == "dispatch-inherited" {
			wantTicket = nil
		}
		if diff := gocmp.Diff(wantTicket, out.Ticket); diff != "" {
			t.Errorf("%s usage ticket (-want +got): %s", via, diff)
		}
	}
	parent := http.Header{"X-Claude-Code-Session-Id": {"s"}, "X-Claude-Code-Agent-Id": {"parent"}}
	child := http.Header{"X-Claude-Code-Session-Id": {"s"}, "X-Claude-Code-Agent-Id": {"child"}, "X-Claude-Code-Parent-Agent-Id": {"parent"}}
	const follow = `"follow up"`
	briefing := fmt.Sprintf("%q", marker)
	turn(briefing, parent, "", "dispatch")
	cache := filepath.Join(dir, "routes.json")
	if err := os.Remove(cache); err != nil {
		t.Fatalf("cache not written under startup directory: %v", err)
	}
	turn(follow, parent, "", "dispatch-cached")
	turn(fmt.Sprintf("%q", marker+marker), parent, "", "")
	turn(follow, child, "", "dispatch-inherited")
	child.Del("X-Claude-Code-Parent-Agent-Id")
	turn(follow, child, "", "dispatch-cached")
	turn(follow, http.Header{"X-Claude-Code-Session-Id": {"s"}, "X-Claude-Code-Agent-Id": {"sibling"}}, "", "")
	h = newHandler(t, dir, nil, new(sentry.State))
	turn(follow, child, "", "dispatch-cached")

	child.Del("X-Claude-Code-Session-Id")
	turn(follow, child, `{"session_id":"s"}`, "dispatch-cached")
	turn(follow, child, "user_session_old_session_s", "dispatch-cached")
	turn(follow, child, `{"session_id":"other"}`, "")
	turn(follow, parent, `{"session_id":"other"}`, "dispatch-cached")
	child.Set("X-Claude-Code-Session-Id", "unsafe id")
	turn(follow, child, `{"session_id":"s"}`, "dispatch-cached")
	turn(briefing, child, `{"session_id":"unsafe id"}`, "dispatch")
	turn(follow, child, `{"session_id":"unsafe id"}`, "")

	parentOnly := http.Header{"X-Claude-Code-Session-Id": {"s"}, "X-Claude-Code-Parent-Agent-Id": {"parent"}}
	turn(briefing, parentOnly, "", "dispatch")
	turn(follow, parentOnly, "", "")
	parentOnly.Del("X-Claude-Code-Parent-Agent-Id")
	turn(follow, parentOnly, "", "")
	const echo = `{"type":"tool_result","content":"[sidequest-route model=gpt-5.6-terra]"}`
	turn(`[`+echo+`]`, nil, "", "")
	turn(`[{"type":"text","text":`+briefing+`},`+echo+`]`, nil, "", "dispatch")
}

func TestModelKeys(t *testing.T) {
	tests := map[string]struct{ id, want string }{
		"success: Codex fast":          {"claude-gpt-6.1-sol-fast[1m]", "gpt-6.1-sol"},
		"success: Codex ultrafast":     {"gpt-6-astra-ultrafast", "gpt-6-astra"},
		"success: LM Studio fast":      {"claude-lmstudio/qwen3-coder-30b-fast", "lmstudio/qwen3-coder-30b-fast"},
		"success: LM Studio ultrafast": {"lmstudio/tiny-ultrafast", "lmstudio/tiny-ultrafast"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, (policy.Resolver{}).WindowModelID(tc.id)); diff != "" {
				t.Error(diff)
			}
		})
	}
}

type overflowFunc func(http.ResponseWriter, *http.Request, []byte, proxyreq.Result, sentry.Crossing)

func (f overflowFunc) WriteSentryOverflow(w http.ResponseWriter, r *http.Request, original []byte, result proxyreq.Result, crossing sentry.Crossing) {
	f(w, r, original, result, crossing)
}

func TestPrepareArmedSentry(t *testing.T) {
	const session = `["s",null]`
	const model = "claude-gpt-6.1-sol[1m]"
	const base = "gpt-6.1-sol"
	const history = `"messages":[{"role":"assistant","content":"prior"},{"role":"user","content":"next"}]`
	const summary = `"stream":true,"system":"You are a helpful AI assistant tasked with summarizing conversations.",` + history
	target := sentry.Target{Key: base, Policy: &policy.Sentry{BackendWindow: 920012, CompactTrigger: 242000, Source: "compact-at"}, Headroom: 40000}
	tests := map[string]struct {
		path, fields     string
		disabled, refuse bool
	}{
		"error: continuation reports original bytes": {"/v1/messages?beta=true", history, false, true},
		"success: count tokens bypasses crossing":    {"/v1/messages/count_tokens?beta=true", history, false, false},
		"success: compaction bypasses crossing":      {"/v1/messages", summary, false, false},
		"success: disabled sentry bypasses crossing": {"/v1/messages", history, true, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			raw := fmt.Sprintf(`{ "model": %q, %s }`, model, tc.fields)
			state := new(sentry.State)
			state.ObserveJSON(session, target, nil, []byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":250000}}`))
			windows := policy.Windows{"codex": {CompactAt: 242000}}
			env := map[string]string{"CODEX_GATEWAY_COMPACT_TRIGGER": "320000"}
			if tc.disabled {
				env["CODEX_GATEWAY_SENTRY"] = "0"
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, nil)
			r.Header.Set("X-Claude-Code-Session-Id", "s")
			w := httptest.NewRecorder()
			calls := 0
			reply := overflowFunc(func(rw http.ResponseWriter, req *http.Request, original []byte, res proxyreq.Result, crossing sentry.Crossing) {
				calls++
				if rw != w || req != r || string(original) != raw {
					t.Errorf("overflow writer did not receive the original request, writer and bytes: %q", original)
				}
				if diff := gocmp.Diff((sentry.Crossing{Usage: 250000, Trigger: 242000}), crossing); diff != "" {
					t.Error(diff)
				}
				if res.SelectedModel != model || res.EffectiveModel != base || res.Provider != "codex" || res.Via != "direct" || res.SessionKey != session {
					t.Errorf("incorrect overflow route: %+v", res)
				}
				if diff := gocmp.Diff(target, res.Target); diff != "" {
					t.Error(diff)
				}
				rw.WriteHeader(http.StatusRequestEntityTooLarge)
			})
			h, err := proxyreq.New(proxyreq.Options{StartupDir: t.TempDir(), DispatchPath: "routes.json", Now: time.Now, Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Sentry: state, Windows: windows, Reply: reply})
			if err != nil {
				t.Fatal(err)
			}
			s := route.Snapshot{}
			original := []byte(raw)
			out, err := h.Prepare(w, r, s, s.Classify(r.Method, r.RequestURI, original), original)
			if err != nil {
				t.Fatal(err)
			}
			if tc.refuse {
				if calls != 1 || w.Code != 413 || out.LocalStatus != 413 || len(out.Body) != 0 {
					t.Errorf("overflow writer calls=%d, HTTP %d, result=%+v", calls, w.Code, out)
				}
			} else {
				want := fmt.Sprintf(`{"model":%q,%s}`, base, tc.fields)
				if diff := gocmp.Diff(want, string(out.Body)); diff != "" || calls != 0 || out.LocalStatus != 0 {
					t.Errorf("expected forward, writer calls=%d, status=%d, body diff=%s", calls, out.LocalStatus, diff)
				}
			}
			if !tc.disabled {
				if diff := gocmp.Diff(target, out.Target); diff != "" {
					t.Error(diff)
				}
			}
		})
	}
}

func TestSentrySessionIdentity(t *testing.T) {
	tests := map[string]struct {
		session  string
		agent    *string
		disabled bool
		want     string
	}{
		"success: empty agent":                {"s", new(""), false, `["s",""]`},
		"success: missing agent":              {"s", nil, false, `["s",null]`},
		"success: literal replacement":        {"�", new("child"), false, `["�","child"]`},
		"success: malformed session is inert": {"\xff", nil, false, ""},
		"success: malformed agent is inert":   {"s", new("\xff"), false, ""},
		"success: sentry switch is off":       {"s", nil, true, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			if tc.disabled {
				env["CODEX_GATEWAY_SENTRY"] = "0"
			}
			headers := make(http.Header)
			headers.Set("X-Claude-Code-Session-Id", tc.session)
			if tc.agent != nil {
				headers.Set("X-Claude-Code-Agent-Id", *tc.agent)
			}
			got, _ := prepare(t, newHandler(t, t.TempDir(), env, new(sentry.State)), route.Snapshot{}, "/v1/messages", `{"model":"claude-gpt-5.6-sol"}`, headers)
			if diff := gocmp.Diff(tc.want, got.SessionKey); diff != "" {
				t.Error(diff)
			}
		})
	}
}
