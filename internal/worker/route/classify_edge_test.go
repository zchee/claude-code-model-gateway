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

// nestedBody returns an object with a Codex model whose deepest member opens containers until the document holds
// depth containers in total, the top-level object included.
func nestedBody(depth int) string {
	return `{"model":"claude-gpt-9","x":` + strings.Repeat("[", depth-1) + strings.Repeat("]", depth-1) + `}`
}

func TestClassifyBodyEdges(t *testing.T) {
	s := route.NewSnapshot(policy.Resolver{}, "fallback", nil, []string{"grok-a.1", "grok-a-1"})
	anthropic := route.Decision{Branch: "anthropic"}
	codex := func(model string) route.Decision {
		return route.Decision{Branch: "proxy", Provider: "codex", Model: model}
	}
	grok := func(model string) route.Decision {
		return route.Decision{Branch: "grok", Provider: "grok", Model: model}
	}
	tests := map[string]struct {
		target, body string
		want         route.Decision
	}{
		"success: deepest accepted nesting is classified":                 {"/v1/messages", nestedBody(10000), codex("gpt-9")},
		"success: nesting past the limit passes through":                  {"/v1/messages", nestedBody(10001), anthropic},
		"success: escaped lone surrogate passes through":                  {"/v1/messages", `{"model":"claude-gpt-\ud800"}`, anthropic},
		"success: lone low surrogate passes through":                      {"/v1/messages", `{"model":"claude-grok-\udc00x"}`, anthropic},
		"success: reversed surrogate pair passes through":                 {"/v1/messages", `{"model":"claude-gpt-\udc00\ud800"}`, anthropic},
		"success: escaped surrogate pair is one code point":               {"/v1/messages", "{\"model\":\"claude-gpt-\\ud83d\\ude00\"}", codex("gpt-\U0001F600")},
		"success: ill-formed byte decodes as Node does":                   {"/v1/messages", "{\"model\":\"claude-gpt-\xff\"}", codex("gpt-\uFFFD")},
		"success: escaped member name":                                    {"/v1/messages", "{\"mod\\u0065l\":\"claude-gpt-9\"}", codex("gpt-9")},
		"success: nested model ignored":                                   {"/v1/messages", `{"x":{"model":"claude-gpt-9"}}`, anthropic},
		"success: one extended suffix per strip":                          {"/v1/messages", `{"model":"claude-gpt-9[1m][1m]"}`, codex("gpt-9")},
		"success: empty query":                                            {"/v1/messages?", `{"model":"claude-gpt-9"}`, codex("gpt-9")},
		"success: absolute-form target goes to the default upstream":      {"http://example.test/v1/messages", `{"model":"claude-gpt-9"}`, anthropic},
		"success: doubled suffix on the Grok path strips once":            {"/v1/messages", `{"model":"claude-grok-4.7[1m][1m]"}`, grok("grok-4.7[1m]")},
		"success: doubled suffix on the legacy ID strips once":            {"/v1/messages", `{"model":"claude-codex-gpt-5.4[1m][1m]"}`, codex("gpt-5.4[1m]")},
		"success: duplicate model with escaped first spelling, last wins": {"/v1/messages", "{\"mod\\u0065l\":\"claude-opus-5\",\"model\":\"claude-gpt-9\"}", codex("gpt-9")},
		"success: empty legacy Codex":                                     {"/v1/messages", `{"model":"claude-codex-"}`, anthropic},
		"success: legacy dispatch spelling":                               {"/v1/messages", `{"model":"claude-codex-gpt-auto"}`, codex("gpt-auto")},
		"success: empty Grok picker":                                      {"/v1/messages", `{"model":"claude-grok-[1m]"}`, grok("grok-")},
		"success: first flattened Grok ID wins":                           {"/v1/messages", `{"model":"claude-grok-a-1"}`, grok("grok-a.1")},
		"success: Grok prefix is case-sensitive":                          {"/v1/messages", `{"model":"Claude-grok-4.7"}`, anthropic},
		"success: object string form is not ported":                       {"/v1/messages", `{"model":"claude-grok-[object Object]"}`, grok("grok-[object Object]")},
		"success: fragment is part of the path":                           {"/v1/messages#x?y", `{"model":"claude-gpt-9"}`, codex("gpt-9")},
		"success: path without the messages prefix":                       {"/v1/message", `{"model":"claude-gpt-9"}`, anthropic},
		"success: boolean body":                                           {"/v1/messages", `true`, anthropic},
		"success: whitespace around one document":                         {"/v1/messages", " \r\n\t" + `{"model":"claude-gpt-9"}` + "\n", codex("gpt-9")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := []byte(tt.body)
			original := bytes.Clone(body)
			if diff := gocmp.Diff(tt.want, s.Classify("POST", tt.target, body)); diff != "" {
				t.Errorf("Classify(%q) (-want +got):\n%s", tt.target, diff)
			}
			if !bytes.Equal(original, body) {
				t.Error("request body changed")
			}
		})
	}
}

func TestClassifyMethodsAreCaseSensitive(t *testing.T) {
	s := route.NewSnapshot(policy.Resolver{}, "fallback", nil, nil)
	tests := map[string]struct {
		method, target string
		want           route.Decision
	}{
		"success: lower-case drain method":    {"post", "/drain", route.Decision{Branch: "anthropic"}},
		"success: drain with query":           {"POST", "/drain?x=1", route.Decision{Branch: "drain"}},
		"success: lower-case catalog method":  {"get", "/v1/models", route.Decision{Branch: "anthropic"}},
		"success: lower-case HEAD method":     {"head", "/v1/messages", route.Decision{Branch: "proxy", Provider: "codex", Model: "gpt-9"}},
		"success: health for any method":      {"DELETE", "/healthz", route.Decision{Branch: "health"}},
		"success: health path is exact":       {"GET", "/healthz/", route.Decision{Branch: "anthropic"}},
		"success: catalog path is exact":      {"GET", "/v1/models/claude-gpt-9", route.Decision{Branch: "anthropic"}},
		"success: HEAD on the drain endpoint": {"HEAD", "/drain", route.Decision{Branch: "head"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, s.Classify(tt.method, tt.target, []byte(`{"model":"claude-gpt-9"}`))); diff != "" {
				t.Errorf("Classify(%q, %q) (-want +got):\n%s", tt.method, tt.target, diff)
			}
		})
	}
}

func TestGatewayIDPrefixes(t *testing.T) {
	s := route.NewSnapshot(policy.Resolver{}, "fallback", nil, nil)
	tests := map[string]struct {
		id   string
		want bool
	}{
		"success: unknown Grok picker":    {"claude-grok-anything", true},
		"success: dispatch":               {"claude-codex-auto[1m]", true},
		"success: legacy Codex":           {"claude-codex-gpt-5.4", true},
		"error: legacy non-Codex":         {"claude-codex-foo", false},
		"error: bare Grok backend ID":     {"grok-4.7", false},
		"error: unlisted provider picker": {"claude-kimi-k3", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, s.IsGatewayID(tt.id)); diff != "" {
				t.Errorf("IsGatewayID(%q) (-want +got):\n%s", tt.id, diff)
			}
		})
	}
}

func TestGrokEmptyIDMatchFallsBack(t *testing.T) {
	tests := map[string]struct {
		ids  []string
		want string
	}{
		"success: empty first match yields the picker spelling": {[]string{"", "grok-"}, "grok-"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := route.NewSnapshot(policy.Resolver{}, "fallback", nil, tt.ids)
			got := s.Classify("POST", "/v1/messages", []byte(`{"model":"claude-grok-[1m]"}`))
			if diff := gocmp.Diff(route.Decision{Branch: "grok", Provider: "grok", Model: tt.want}, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}
