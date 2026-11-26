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
	"bytes"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// deepModelBody returns an object with the given model whose last member opens containers until the document holds
// depth containers in total, the top-level object included.
func deepModelBody(model string, depth int) string {
	return `{"model":"` + model + `","x":` + strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1) + `}`
}

// deepMemberBody returns an object with the given model whose last member opens containers until an inner object sits
// at depth containers in total, the top-level object included, and places member where a member name is required.
func deepMemberBody(model string, depth int, member string) string {
	return `{"model":"` + model + `","x":` + strings.Repeat("[", depth-2) + `{` + member + `:1}` + strings.Repeat("]", depth-2) + `}`
}

// TestClassifyRefusals pins the two conditions the port refuses locally because passing the body through could change
// the recipient, and the neighbouring bodies that still route as the source routes them.
func TestClassifyRefusals(t *testing.T) {
	s := route.NewSnapshot(policy.Resolver{}, "fallback", nil, []string{"grok-4.7"})
	anthropic := route.Decision{Branch: "anthropic"}
	codex := route.Decision{Branch: "proxy", Provider: "codex", Model: "gpt-9"}
	grok := route.Decision{Branch: "grok", Provider: "grok", Model: "grok-4.7"}
	tooDeep := route.Decision{Branch: "refused", Reason: "nesting-depth"}
	illFormed := route.Decision{Branch: "refused", Reason: "ill-formed-model"}
	const limit = 10000
	tests := map[string]struct {
		method, target, body string
		want                 route.Decision
	}{
		"success: proxy model at the nesting limit":               {"POST", "/v1/messages", deepModelBody("claude-gpt-9", limit), codex},
		"success: Grok model at the nesting limit":                {"POST", "/v1/messages", deepModelBody("claude-grok-4.7", limit), grok},
		"success: default model at the nesting limit":             {"POST", "/v1/messages", deepModelBody("claude-opus-5", limit), anthropic},
		"error: proxy model past the nesting limit":               {"POST", "/v1/messages", deepModelBody("claude-gpt-9", limit+1), tooDeep},
		"error: Grok model past the nesting limit":                {"POST", "/v1/messages", deepModelBody("claude-grok-4.7", limit+1), tooDeep},
		"error: default model past the nesting limit":             {"POST", "/v1/messages", deepModelBody("claude-opus-5", limit+1), tooDeep},
		"error: array past the nesting limit":                     {"POST", "/v1/messages", strings.Repeat("[", limit+1) + strings.Repeat("]", limit+1), tooDeep},
		"error: truncated right after crossing the nesting limit": {"POST", "/v1/messages", `{"model":"claude-gpt-9","x":` + strings.Repeat("[", limit), tooDeep},
		"success: truncated at the nesting limit":                 {"POST", "/v1/messages", `{"model":"claude-gpt-9","x":` + strings.Repeat("[", limit-1), anthropic},
		"error: past the nesting limit for another method":        {"DELETE", "/v1/messages/count_tokens", deepModelBody("claude-opus-5", limit+1), tooDeep},
		"success: array member name at the nesting limit":         {"POST", "/v1/messages", deepMemberBody("claude-gpt-9", limit, "[0]"), anthropic},
		"success: object member name at the nesting limit":        {"POST", "/v1/messages", deepMemberBody("claude-gpt-9", limit, `{"k":0}`), anthropic},
		"success: numeric member name at the nesting limit":       {"POST", "/v1/messages", deepMemberBody("claude-gpt-9", limit, "0"), anthropic},
		"success: literal member name at the nesting limit":       {"POST", "/v1/messages", deepMemberBody("claude-gpt-9", limit, "true"), anthropic},
		"error: array member name past the nesting limit":         {"POST", "/v1/messages", deepMemberBody("claude-gpt-9", limit+1, "[0]"), tooDeep},
		"error: object member name past the nesting limit":        {"POST", "/v1/messages", deepMemberBody("claude-gpt-9", limit+1, `{"k":0}`), tooDeep},
		"error: numeric member name past the nesting limit":       {"POST", "/v1/messages", deepMemberBody("claude-gpt-9", limit+1, "0"), tooDeep},
		"error: literal member name past the nesting limit":       {"POST", "/v1/messages", deepMemberBody("claude-gpt-9", limit+1, "true"), tooDeep},
		"error: lone high surrogate in the model":                 {"POST", "/v1/messages", "{\"model\":\"claude-gpt-\\ud800\"}", illFormed},
		"error: lone low surrogate in the model":                  {"POST", "/v1/messages", "{\"model\":\"claude-grok-\\udc00\"}", illFormed},
		"error: reversed surrogate pair in the model":             {"POST", "/v1/messages", "{\"model\":\"claude-opus-\\udc00\\ud800\"}", illFormed},
		"error: lone surrogate in an escaped model name":          {"POST", "/v1/messages", "{\"mod\\u0065l\":\"claude-gpt-\\ud800\"}", illFormed},
		"success: lone high surrogate in a nested model":          {"POST", "/v1/messages", "{\"model\":\"claude-gpt-9\",\"x\":{\"model\":\"claude-gpt-\\ud800\"}}", codex},
		"success: lone low surrogate in a nested model":           {"POST", "/v1/messages", "{\"model\":\"claude-gpt-9\",\"x\":{\"model\":\"claude-grok-\\udc00\"}}", codex},
		"success: reversed pair in a nested model":                {"POST", "/v1/messages", "{\"model\":\"claude-gpt-9\",\"x\":{\"model\":\"claude-opus-\\udc00\\ud800\"}}", codex},
		"success: nested ill-formed model alone":                  {"POST", "/v1/messages", "{\"x\":{\"model\":\"claude-gpt-\\ud800\"}}", anthropic},
		"success: lone surrogate outside the model":               {"POST", "/v1/messages", "{\"model\":\"claude-grok-4.7\",\"system\":\"\\ud800\"}", grok},
		"success: lone surrogate in another member name":          {"POST", "/v1/messages", "{\"model\\ud800\":\"claude-gpt-9\"}", anthropic},
		"success: duplicate model whose last is well-formed":      {"POST", "/v1/messages", "{\"model\":\"claude-gpt-\\ud800\",\"model\":\"claude-gpt-9\"}", codex},
		"error: duplicate model whose last is ill-formed":         {"POST", "/v1/messages", "{\"model\":\"claude-gpt-9\",\"model\":\"claude-gpt-\\ud800\"}", illFormed},
		"success: body that is not JSON":                          {"POST", "/v1/messages", "{\"model\":\"claude-gpt-9\"", anthropic},
		"success: two JSON documents":                             {"POST", "/v1/messages", "{\"model\":\"claude-gpt-9\"} {}", anthropic},
		"success: ill-formed byte in a proxy model":               {"POST", "/v1/messages", "{\"model\":\"claude-gpt-\xff\"}", route.Decision{Branch: "proxy", Provider: "codex", Model: "gpt-\uFFFD"}},
		"success: ill-formed byte in a Grok model":                {"POST", "/v1/messages", "{\"model\":\"claude-grok-\xff\"}", route.Decision{Branch: "grok", Provider: "grok", Model: "grok-\uFFFD"}},
		"success: missing model":                                  {"POST", "/v1/messages", "{\"messages\":[]}", anthropic},
		"success: numeric model":                                  {"POST", "/v1/messages", "{\"model\":9}", anthropic},
		"success: null model":                                     {"POST", "/v1/messages", "{\"model\":null}", anthropic},
		"success: drain before a deep body":                       {"POST", "/drain", deepModelBody("claude-gpt-9", limit+1), route.Decision{Branch: "drain"}},
		"success: health before an ill-formed model":              {"POST", "/healthz", "{\"model\":\"claude-gpt-\\ud800\"}", route.Decision{Branch: "health"}},
		"success: catalog before a deep body":                     {"GET", "/v1/models", deepModelBody("claude-gpt-9", limit+1), route.Decision{Branch: "models"}},
		"success: HEAD before an ill-formed model":                {"HEAD", "/v1/messages", "{\"model\":\"claude-gpt-\\ud800\"}", route.Decision{Branch: "head"}},
		"success: deep body outside the messages prefix":          {"POST", "/v1/message", deepModelBody("claude-gpt-9", limit+1), anthropic},
		"success: ill-formed model outside the prefix":            {"POST", "/v2/messages", "{\"model\":\"claude-gpt-\\ud800\"}", anthropic},
		"success: ill-formed model in an absolute target":         {"POST", "http://example.test/v1/messages", "{\"model\":\"claude-gpt-\\ud800\"}", anthropic},
		"success: empty body":                                     {"POST", "/v1/messages", "", anthropic},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := []byte(tt.body)
			original := bytes.Clone(body)
			if diff := gocmp.Diff(tt.want, s.Classify(tt.method, tt.target, body)); diff != "" {
				t.Errorf("Classify(%q, %q) (-want +got):\n%s", tt.method, tt.target, diff)
			}
			if !bytes.Equal(original, body) {
				t.Error("request body changed")
			}
		})
	}
}
