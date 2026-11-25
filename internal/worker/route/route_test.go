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
	"encoding/json/jsontext"
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func resolverFor(routes []proxymodels.Route, windows policy.Windows, local int64, lmStudio map[string]int64) policy.Resolver {
	return policy.Resolver{Routes: proxymodels.NewTable(routes), Windows: windows, LocalWindow: local, LocalPort: 8080, LMStudioWindows: lmStudio, LMStudioURL: "http://127.0.0.1:1234"}
}

func listedSnapshot() route.Snapshot {
	routes := []proxymodels.Route{
		{ID: "gpt-6-sol", Provider: "codex"},
		{ID: "gpt-6-sol-fast", Provider: "codex"},
		{ID: "cursor-agent", Provider: "cursor"},
		{ID: "composer-2.5", Provider: "cursor"},
		{ID: "grok-4.7", Provider: "grok"},
		{ID: "grok-composer-2.5-fast", Provider: "grok"},
		{ID: "kimi-k3", Provider: "kimi"},
		{ID: "k3", Provider: "kimi"},
		{ID: "glm-5.3", Provider: "opencode"},
		{ID: "opencode-go/kimi-k2.6", Provider: "opencode"},
	}
	return route.NewSnapshot(resolverFor(routes, nil, 0, nil), "proxy", nil, []string{"grok-custom.2"})
}

func TestCodexBase(t *testing.T) {
	s := listedSnapshot()
	tests := map[string]struct {
		id   *string
		want string
	}{
		"success: current":      {new("claude-gpt-5.6-sol"), "gpt-5.6-sol"},
		"success: extended":     {new("claude-gpt-5.6-sol[1m]"), "gpt-5.6-sol"},
		"success: legacy":       {new("claude-codex-gpt-5.6-sol"), "gpt-5.6-sol"},
		"success: dispatch":     {new("claude-codex-auto"), "auto"},
		"success: listed Codex": {new("claude-gpt-6-sol[1m]"), "gpt-6-sol"},
		"error: listed Kimi":    {new("claude-kimi-k3"), ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, s.CodexBase(tt.id)); diff != "" {
				t.Error(diff)
			}
		})
	}
	t.Run("error: passthrough", func(t *testing.T) {
		for _, id := range []*string{new("claude-opus-5"), new("claude-opus-5[1m]"), new("claude-sonnet-4-5"), new("claude-fable-5"), new("claude-haiku-4-5"), new("claude-grok-4.5"), new("claude-"), new("gpt-5.6-sol"), nil} {
			if got := s.CodexBase(id); got != "" {
				t.Errorf("CodexBase(%v) = %q, want empty", id, got)
			}
		}
	})
}

func TestGatewayID(t *testing.T) {
	s := listedSnapshot()
	tests := map[string]struct {
		id   string
		want bool
	}{
		"success: native Grok":      {"claude-grok-4.5", true},
		"error: Opus":               {"claude-opus-5", false},
		"success: OpenCode":         {"claude-glm-5.3", true},
		"error: Opus listing alias": {"claude-opus-5-5", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, s.IsGatewayID(tt.id)); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestProxyID(t *testing.T) {
	s := listedSnapshot()
	tests := map[string]struct{ id, provider, base string }{
		"success: Kimi":            {"claude-kimi-k3", "kimi", "kimi-k3"},
		"success: OpenCode prefix": {"claude-opencode-go/kimi-k2.6", "opencode", "opencode-go/kimi-k2.6"},
		"success: proxy Grok":      {"claude-grok-composer-2.5-fast", "grok", "grok-composer-2.5-fast"},
		"success: extended Codex":  {"claude-gpt-6-sol[1m]", "codex", "gpt-6-sol"},
		"success: legacy Codex":    {"claude-codex-gpt-5.4", "codex", "gpt-5.4"},
		"success: dispatch":        {"claude-codex-auto", "codex", "auto"},
		"error: native Grok":       {"claude-grok-4.7[1m]", "", ""},
		"error: Claude":            {"claude-opus-5-5", "", ""},
		"error: bare":              {"kimi-k3", "", ""},
		"error: unlisted":          {"claude-glm-99", "", ""},
		"error: legacy Kimi":       {"claude-codex-kimi-k3", "", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := s.ProxyID(tt.id)
			if diff := gocmp.Diff(proxymodels.Route{ID: tt.base, Provider: tt.provider}, got); diff != "" || ok != (tt.provider != "") {
				t.Errorf("lookup %q, found=%t (-want +got): %s", tt.id, ok, diff)
			}
		})
	}
}

func TestWithoutListing(t *testing.T) {
	s := route.NewSnapshot(policy.Resolver{},
		"fallback", nil, nil)
	tests := map[string]struct{ id, provider, base string }{
		"success: Codex": {"claude-gpt-5.4", "codex", "gpt-5.4"},
		"error: Kimi":    {"claude-kimi-k3", "", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := s.ProxyID(tt.id)
			if diff := gocmp.Diff(proxymodels.Route{ID: tt.base, Provider: tt.provider}, got); diff != "" || ok != (tt.provider != "") {
				t.Errorf("found=%t (-want +got): %s", ok, diff)
			}
		})
	}
	t.Run("success: unlisted policy", func(t *testing.T) {
		m := s.Resolver().Resolve("claude-kimi-k3")
		if m == nil || m.Backend != "anthropic" {
			t.Errorf("unlisted Kimi policy = %+v, want Anthropic", m)
		}
	})
}

func TestClassifyModel(t *testing.T) {
	s := listedSnapshot()
	tests := map[string]struct{ id, branch, provider, upstream string }{
		"success: extended Codex":        {"claude-gpt-5.6-sol[1m]", "proxy", "codex", "gpt-5.6-sol"},
		"success: tiered Codex":          {"claude-gpt-5.6-sol-fast", "proxy", "codex", "gpt-5.6-sol-fast"},
		"success: unknown Codex":         {"claude-gpt-9", "proxy", "codex", "gpt-9"},
		"success: legacy Codex":          {"claude-codex-gpt-5.6-sol", "proxy", "codex", "gpt-5.6-sol"},
		"success: dispatch":              {"claude-codex-auto[1m]", "proxy", "codex", "auto"},
		"success: invalid legacy":        {"claude-codex-foo", "anthropic", "", ""},
		"success: native listed Grok":    {"claude-grok-4.7[1m]", "grok", "grok", "grok-4.7"},
		"success: native Grok middle":    {"claude-grok-4.6", "grok", "grok", "grok-4.6"},
		"success: native Grok older":     {"claude-grok-4.5", "grok", "grok", "grok-4.5"},
		"success: fast Grok":             {"claude-grok-4.7-fast", "proxy", "grok", "grok-4.7-fast"},
		"success: build Grok":            {"claude-grok-4.7-build-fast", "proxy", "grok", "grok-4.7-build-fast"},
		"success: proxy-only Grok":       {"claude-grok-composer-2.5-fast", "proxy", "grok", "grok-composer-2.5-fast"},
		"success: unknown Grok":          {"claude-grok-custom", "grok", "grok", "grok-custom"},
		"success: cached flattened Grok": {"claude-grok-custom-2[1m]", "grok", "grok", "grok-custom.2"},
		"success: Muse":                  {"claude-muse-spark-1.3", "proxy", "muse", "muse-spark-1.3"},
		"success: listed provider":       {"claude-kimi-k3", "proxy", "kimi", "kimi-k3"},
		"success: Opus":                  {"claude-opus-5-5[1m]", "anthropic", "", ""},
		"success: unknown Claude":        {"claude-anything-else", "anthropic", "", ""},
		"success: bare Codex":            {"gpt-5.6-sol", "anthropic", "", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{ "model": "` + tt.id + `" }`)
			original := bytes.Clone(body)
			got := s.Classify("POST", "/v1/messages", body)
			if diff := gocmp.Diff(route.Decision{Branch: tt.branch, Provider: tt.provider, Model: tt.upstream}, got); diff != "" {
				t.Error(diff)
			}
			if diff := gocmp.Diff(original, body); diff != "" {
				t.Errorf("request changed: %s", diff)
			}
		})
	}
}

func TestClassifyRequest(t *testing.T) {
	s := route.NewSnapshot(policy.Resolver{},
		"fallback", jsontext.Value(`[]`), nil)
	anthropic := route.Decision{Branch: "anthropic"}
	proxy := route.Decision{Branch: "proxy", Provider: "codex", Model: "gpt-9"}
	const good = `{"model":"claude-gpt-9"}`
	tests := map[string]struct {
		method, target, body string
		want                 route.Decision
	}{
		"success: count tokens with query":             {"POST", "/v1/messages/count_tokens?beta=1", good, proxy},
		"success: prefix not segment":                  {"PATCH", "/v1/messages-anything", good, proxy},
		"success: foreign path":                        {"DELETE", "/other?path=/v1/messages", good, anthropic},
		"success: arbitrary method without body":       {"OPTIONS", "/v1/messages", "", anthropic},
		"success: byte-exact arbitrary payload":        {"PUT", "/v1/messages", " \n\xffgzip", anthropic},
		"success: repeated model last wins":            {"POST", "/v1/messages", `{"model":"claude-opus","model":"claude-gpt-9"}`, proxy},
		"success: last model null":                     {"POST", "/v1/messages", `{"model":"claude-gpt-9","model":null}`, anthropic},
		"success: valid model invalid tail":            {"POST", "/v1/messages", good + `x`, anthropic},
		"success: invalid Grok tail":                   {"POST", "/v1/messages", `{"model":"claude-grok-4.7",}`, anthropic},
		"success: multiple documents":                  {"POST", "/v1/messages", good + ` {}`, anthropic},
		"success: missing":                             {"POST", "/v1/messages", `{}`, anthropic},
		"success: numeric model":                       {"POST", "/v1/messages", `{"model":5}`, anthropic},
		"success: array":                               {"POST", "/v1/messages", `[{"model":"claude-gpt-9"}]`, anthropic},
		"success: null":                                {"POST", "/v1/messages", `null`, anthropic},
		"success: string":                              {"POST", "/v1/messages", `"claude-gpt-9"`, anthropic},
		"success: cached defaults are objects not IDs": {"POST", "/v1/messages", `{"model":"claude-grok-4-5"}`, route.Decision{Branch: "grok", Provider: "grok", Model: "grok-4-5"}},
		"success: health before HEAD":                  {"HEAD", "/healthz?x", good, route.Decision{Branch: "health"}},
		"success: HEAD before body":                    {"HEAD", "/v1/messages", good, route.Decision{Branch: "head"}},
		"success: GET catalog":                         {"GET", "/v1/models?q", good, route.Decision{Branch: "models"}},
		"success: other catalog method":                {"POST", "/v1/models", good, anthropic},
		"success: drain handler":                       {"POST", "/drain", good, route.Decision{Branch: "drain"}},
		"success: other drain method":                  {"GET", "/drain", good, anthropic},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := []byte(tt.body)
			if diff := gocmp.Diff(tt.want, s.Classify(tt.method, tt.target, body)); diff != "" {
				t.Error(diff)
			}
			if diff := gocmp.Diff(tt.body, string(body)); diff != "" {
				t.Errorf("request changed: %s", diff)
			}
		})
	}
}
