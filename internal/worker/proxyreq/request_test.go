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

func prepare(t *testing.T, h *proxyreq.Handler, s route.Snapshot, path, raw string, headers http.Header) (proxyreq.Result, *httptest.ResponseRecorder) {
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
	threadRefusal = `{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: beta_header:message-threads-2026-08-12; the model-gateway proxy and Grok backends hold no conversation state. Resend this turn with the full message history.","details":{"error_code":"thread_unsupported_request"}}}`
	schema        = `{"not":{"pattern":"[^\\p{Cc}a]"}}`
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
			} else if out.LocalStatus != 0 {
				t.Errorf("unexpected local refusal: %+v", out)
			}
		})
	}
}

func TestDispatchLifetime(t *testing.T) {
	const marker = "[sidequest-route model=gpt-5.6-sol effort=high ticket=SQ-123]"
	const auto = "claude-codex-auto"
	dir := t.TempDir()
	h := newHandler(t, dir, nil, new(sentry.State))
	turn := func(content, agent, parent, via string) {
		t.Helper()
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}],"output_config":{"effort":"low","other":1}}`, auto, content)
		headers := make(http.Header)
		headers.Set("X-Claude-Code-Session-Id", "s")
		headers.Set("X-Claude-Code-Agent-Id", agent)
		if parent != "" {
			headers.Set("X-Claude-Code-Parent-Agent-Id", parent)
		}
		out, w := prepare(t, h, route.Snapshot{}, "/v1/messages", body, headers)
		if via == "" {
			if out.LocalStatus != 400 || out.Reason != "invalid_route" || w.Code != 400 || len(out.Body) != 0 {
				t.Fatalf("unbound route forwarded or not refused: %+v", out)
			}
			return
		}
		want := fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":%q}],"output_config":{"effort":"high","other":1}}`, content)
		if diff := gocmp.Diff(want, string(out.Body)); diff != "" {
			t.Error(diff)
		}
		if out.Via != via || out.SelectedModel != auto || out.EffectiveModel != "gpt-5.6-sol" || out.Provider != "codex" || out.Effort == nil || *out.Effort != "high" {
			t.Errorf("route = %+v", out)
		}
	}
	turn(marker, "parent", "", "dispatch")
	cache := filepath.Join(dir, "routes.json")
	if err := os.Remove(cache); err != nil {
		t.Fatalf("cache not written under startup directory: %v", err)
	}
	turn("follow up", "parent", "", "dispatch-cached")
	turn(marker+marker, "parent", "", "")
	turn("new child", "child", "parent", "dispatch-inherited")
	turn("later", "child", "", "dispatch-cached")
	turn("other", "sibling", "", "")
	h = newHandler(t, dir, nil, new(sentry.State))
	turn("after restart", "child", "", "dispatch-cached")
}

func TestSentrySessionIdentity(t *testing.T) {
	tests := map[string]struct {
		session  string
		agent    *string
		disabled bool
		want     string
	}{
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
