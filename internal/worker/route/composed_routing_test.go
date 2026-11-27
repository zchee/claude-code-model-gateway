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

package route_test

import (
	json "encoding/json/v2"
	"net/http"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// toolNames returns the name of every element of a tools array, or of every tool_use block of a
// content array, in order.
func toolNames(t *testing.T, doc, member string) []string {
	t.Helper()
	type item struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	var parsed struct {
		Tools   []item `json:"tools"`
		Content []item `json:"content"`
	}
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("decode %s: %v\n%s", member, err, doc)
	}
	items := parsed.Content
	if member == "tools" {
		items = parsed.Tools
	}
	names := []string{}
	for _, it := range items {
		if member == "tools" || it.Type == "tool_use" {
			names = append(names, it.Name)
		}
	}
	return names
}

// TestRoutingKeepPlanToolsOverride checks that CODEX_GATEWAY_KEEP_PLAN_TOOLS, enabled by exactly "1", keeps the
// plan-mode tools on the proxy path: the tool definitions reach the proxy (lib/request-worker.js:2541-2550) and the
// proxy's plan-mode tool calls reach the client (the same flag is the reply filter's switch at
// lib/request-worker.js:2591). Any other value, and an unset variable, removes both, for Codex and for a listed
// non-Codex provider alike.
func TestRoutingKeepPlanToolsOverride(t *testing.T) {
	const tools = `[{"name":"EnterPlanMode","input_schema":{"type":"object"}},{"name":"Read","input_schema":{"type":"object"}},{"name":"ExitPlanMode","input_schema":{"type":"object"}}]`
	const answer = `{"id":"msg_proxy","type":"message","role":"assistant","model":"backend","stop_reason":"tool_use","content":[{"type":"text","text":"plan"},{"type":"tool_use","id":"toolu_1","name":"ExitPlanMode","input":{}},{"type":"tool_use","id":"toolu_2","name":"Read","input":{}}],"usage":{"input_tokens":1,"output_tokens":1}}`
	kept := []string{"EnterPlanMode", "Read", "ExitPlanMode"}
	removed := []string{"Read"}
	keptCalls := []string{"ExitPlanMode", "Read"}
	removedCalls := []string{"Read"}
	tests := map[string]struct {
		model        string
		value        *string
		wantTools    []string
		wantCalls    []string
		wantUpstream string
	}{
		"success: Codex keeps plan tools when the flag is 1":           {model: "claude-gpt-5.6-terra", value: new("1"), wantTools: kept, wantCalls: keptCalls, wantUpstream: "gpt-5.6-terra"},
		"success: listed provider keeps plan tools when the flag is 1": {model: "claude-kimi-k3", value: new("1"), wantTools: kept, wantCalls: keptCalls, wantUpstream: "kimi-k3"},
		"error: Codex removes plan tools when the flag is unset":       {model: "claude-gpt-5.6-terra", wantTools: removed, wantCalls: removedCalls, wantUpstream: "gpt-5.6-terra"},
		"error: listed provider removes plan tools when unset":         {model: "claude-kimi-k3", wantTools: removed, wantCalls: removedCalls, wantUpstream: "kimi-k3"},
		"error: Codex removes plan tools when the flag is 0":           {model: "claude-gpt-5.6-terra", value: new("0"), wantTools: removed, wantCalls: removedCalls, wantUpstream: "gpt-5.6-terra"},
		"error: Codex removes plan tools when the flag is true":        {model: "claude-gpt-5.6-terra", value: new("true"), wantTools: removed, wantCalls: removedCalls, wantUpstream: "gpt-5.6-terra"},
		"error: Codex removes plan tools when the flag is blank":       {model: "claude-gpt-5.6-terra", value: new(""), wantTools: removed, wantCalls: removedCalls, wantUpstream: "gpt-5.6-terra"},
		"error: listed provider removes plan tools when the flag is 1 with spaces": {
			model: "claude-kimi-k3", value: new(" 1 "), wantTools: removed, wantCalls: removedCalls, wantUpstream: "kimi-k3",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			if tt.value != nil {
				env["CODEX_GATEWAY_KEEP_PLAN_TOOLS"] = *tt.value
			}
			cw := newComposedWorker(t, env, jsonAnswer(answer))
			status, _, body := cw.post("/v1/messages", `{"model":"`+tt.model+`","max_tokens":64,"messages":[{"role":"user","content":"plan it"}],"tools":`+tools+`}`)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body:\n%s", status, body)
			}
			forwarded, _ := cw.proxy.received()
			if len(forwarded) != 1 {
				t.Fatalf("the proxy received %d requests, want 1", len(forwarded))
			}
			var upstream struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(forwarded[0]), &upstream); err != nil {
				t.Fatalf("decode the forwarded body: %v", err)
			}
			if diff := gocmp.Diff(tt.wantUpstream, upstream.Model); diff != "" {
				t.Errorf("forwarded model (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantTools, toolNames(t, forwarded[0], "tools")); diff != "" {
				t.Errorf("tools the proxy received (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantCalls, toolNames(t, body, "content")); diff != "" {
				t.Errorf("tool calls the client received (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRoutingGrokThreadNotForwarded checks that a Grok request on exactly /v1/messages whose thread member is
// present and is not a plain object of type "create" is answered with the source's local refusal and causes no
// request, and no connection, to the Grok provider (lib/request-worker.js:2599-2613). The forwarded cases are the
// positive controls of the same harness: a self-contained thread creation, a request without a thread, and a
// continuation on the token-count path, which the source does not refuse.
func TestRoutingGrokThreadNotForwarded(t *testing.T) {
	const model = "claude-grok-4.7"
	tests := map[string]struct {
		path, body string
		refused    bool
	}{
		"success: thread creation reaches Grok":                    {path: "/v1/messages", body: threadPayload(model, `{"type":"create"}`)},
		"success: request without a thread reaches Grok":           {path: "/v1/messages", body: threadPayload(model, "")},
		"success: continuation on count_tokens reaches Grok":       {path: "/v1/messages/count_tokens", body: threadPayload(model, threadShapes["continuation"])},
		"success: continuation on the extended alias is refused":   {path: "/v1/messages", body: threadPayload(model+"[1m]", threadShapes["continuation"]), refused: true},
		"success: continuation with a query string is refused":     {path: "/v1/messages?beta=true", body: threadPayload(model, threadShapes["continuation"]), refused: true},
		"success: streamed continuation is refused before sending": {path: "/v1/messages", body: `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"current turn"}],"thread":` + threadShapes["continuation"] + `}`, refused: true},
	}
	for shape, thread := range threadShapes {
		tests["success: "+shape+" is refused"] = struct {
			path, body string
			refused    bool
		}{path: "/v1/messages", body: threadPayload(model, thread), refused: true}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cw := newComposedWorker(t, nil, jsonAnswer(`{"type":"message","model":"gpt-5.6-terra","content":[]}`))
			status, contentType, body := cw.post(tt.path, tt.body)
			grokBodies, _ := cw.grok.received()
			anthropicBodies, _ := cw.anthropic.received()
			proxyBodies, _ := cw.proxy.received()
			if !tt.refused {
				if status != http.StatusOK || len(grokBodies) != 1 {
					t.Fatalf("status = %d with %d Grok requests, want 200 with 1; body:\n%s", status, len(grokBodies), body)
				}
				return
			}
			type observed struct {
				Status                                    int
				ContentType, Body                         string
				Grok, GrokDials, Issuer, Anthropic, Proxy int
			}
			issuerBodies, _ := cw.issuer.received()
			got := observed{status, contentType, body, len(grokBodies), int(cw.grokDials.Load()), len(issuerBodies), len(anthropicBodies), len(proxyBodies)}
			want := observed{Status: http.StatusBadRequest, ContentType: "application/json", Body: sourceThreadRefusal}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("refused Grok continuation (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRoutingListedProviderThreadRefusal checks that a continuation on a listed non-Codex provider's exact
// /v1/messages route is refused locally with HTTP 400 and the source's refusal body, and that the proxy receives
// nothing (lib/request-worker.js:2443-2459, which refuses for every proxy route, not only Codex). A thread creation,
// a request without a thread and a continuation on the token-count path reach the proxy under the backend ID.
func TestRoutingListedProviderThreadRefusal(t *testing.T) {
	type testCase struct {
		path, body, wantModel string
		refused               bool
	}
	tests := map[string]testCase{
		"success: Kimi thread creation is forwarded":           {path: "/v1/messages", body: threadPayload("claude-kimi-k3", `{"type":"create"}`), wantModel: "kimi-k3"},
		"success: Kimi request without a thread is forwarded":  {path: "/v1/messages", body: threadPayload("claude-kimi-k3", ""), wantModel: "kimi-k3"},
		"success: Kimi continuation on count_tokens forwarded": {path: "/v1/messages/count_tokens", body: threadPayload("claude-kimi-k3", threadShapes["continuation"]), wantModel: "kimi-k3"},
		"success: OpenCode continuation is refused":            {path: "/v1/messages", body: threadPayload("claude-glm-5.3", threadShapes["continuation"]), refused: true},
		"success: Kimi extended continuation is refused":       {path: "/v1/messages", body: threadPayload("claude-kimi-k3[1m]", threadShapes["continuation"]), refused: true},
		"success: Kimi continuation with a query is refused":   {path: "/v1/messages?beta=true", body: threadPayload("claude-kimi-k3", threadShapes["continuation"]), refused: true},
	}
	for shape, thread := range threadShapes {
		tests["success: Kimi "+shape+" is refused"] = testCase{path: "/v1/messages", body: threadPayload("claude-kimi-k3", thread), refused: true}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cw := newComposedWorker(t, nil, jsonAnswer(`{"id":"msg_proxy","type":"message","model":"backend","stop_reason":"end_turn","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
			status, contentType, body := cw.post(tt.path, tt.body)
			proxyBodies, _ := cw.proxy.received()
			anthropicBodies, _ := cw.anthropic.received()
			grokBodies, _ := cw.grok.received()
			if !tt.refused {
				if status != http.StatusOK || len(proxyBodies) != 1 {
					t.Fatalf("status = %d with %d proxy requests, want 200 with 1; body:\n%s", status, len(proxyBodies), body)
				}
				var forwarded struct {
					Model string `json:"model"`
				}
				if err := json.Unmarshal([]byte(proxyBodies[0]), &forwarded); err != nil {
					t.Fatalf("decode the forwarded body: %v", err)
				}
				if diff := gocmp.Diff(tt.wantModel, forwarded.Model); diff != "" {
					t.Errorf("forwarded model (-want +got):\n%s", diff)
				}
				return
			}
			type observed struct {
				Status                 int
				ContentType, Body      string
				Proxy, Anthropic, Grok int
			}
			got := observed{status, contentType, body, len(proxyBodies), len(anthropicBodies), len(grokBodies)}
			want := observed{Status: http.StatusBadRequest, ContentType: "application/json", Body: sourceThreadRefusal}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("refused listed-provider continuation (-want +got):\n%s", diff)
			}
		})
	}
}
