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

package proxymodels_test

import (
	"encoding/json/jsontext"
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestParseListing(t *testing.T) {
	tests := map[string]struct {
		rows string
		want []proxymodels.Route
	}{
		"success: provider listing": {
			rows: `[
				{"id":"claude-opus-5-5","display_name":"claude-opus-5-5 (codex)"},
				{"id":"haiku","display_name":"haiku (codex)"},
				{"id":"fable","display_name":"fable (codex)"},
				{"id":"gpt-6-sol","display_name":"gpt-6-sol (codex)"},
				{"id":"gpt-6-sol-fast","display_name":"gpt-6-sol-fast (codex)"},
				{"id":"cursor-agent","display_name":"cursor-agent (cursor)"},
				{"id":"composer-2.5","display_name":"composer-2.5 (cursor)"},
				{"id":"grok-4.7","display_name":"grok-4.7 (grok)"},
				{"id":"grok-composer-2.5-fast","display_name":"grok-composer-2.5-fast (grok)"},
				{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},
				{"id":"k3","display_name":"k3 (kimi)"},
				{"id":"glm-5.3","display_name":"glm-5.3 (opencode)"},
				{"id":"opencode-go/kimi-k2.6","display_name":"opencode-go/kimi-k2.6 (opencode)"}
			]`,
			want: []proxymodels.Route{
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
			},
		},
		"error: non-array listing":                            {rows: `{"data":[]}`, want: []proxymodels.Route{}},
		"error: missing id":                                   {rows: `[{"display_name":"x (codex)"}]`, want: []proxymodels.Route{}},
		"error: display names a different id":                 {rows: `[{"id":"glm-5","display_name":"glm-6 (opencode)"}]`, want: []proxymodels.Route{}},
		"error: unattributed non-Codex row":                   {rows: `[{"id":"glm-5","display_name":"glm-5"}]`, want: []proxymodels.Route{}},
		"success: legacy Codex":                               {rows: `[{"id":"gpt-5.4","display_name":"gpt-5.4"}]`, want: []proxymodels.Route{{ID: "gpt-5.4", Provider: "codex"}}},
		"success: first attributed duplicate wins":            {rows: `[{"id":"k3","display_name":"k3 (kimi)"},{"id":"k3","display_name":"k3 (opencode)"}]`, want: []proxymodels.Route{{ID: "k3", Provider: "kimi"}}},
		"success: unattributed duplicate does not reserve id": {rows: `[{"id":"k3"},{"id":"k3","display_name":"k3 (new-provider)"}]`, want: []proxymodels.Route{{ID: "k3", Provider: "new-provider"}}},
		"error: invalid ids and native aliases":               {rows: `[null,7,{"id":1},{"id":""},{"id":"sonnet","display_name":"sonnet (kimi)"},{"id":"opus","display_name":"opus (kimi)"},{"id":"anthropic-alias","display_name":"anthropic-alias (kimi)"}]`, want: []proxymodels.Route{}},
		"success: malformed rows do not discard valid neighbors": {
			rows: `[
				{"id":"k3","display_name":"k3 (kimi)"},
				null, 7, "x", {"id":42}, {"id":false},
				{"id":"gpt-legacy"}, {"id":"gpt-other","display_name":42},
				{"id":"untagged","display_name":42},
				{"id":"glm-5","display_name":"glm-5 (opencode)"}
			]`,
			want: []proxymodels.Route{
				{ID: "k3", Provider: "kimi"},
				{ID: "gpt-legacy", Provider: "codex"},
				{ID: "gpt-other", Provider: "codex"},
				{ID: "glm-5", Provider: "opencode"},
			},
		},
		"success: only a well-formed suffix attributes a provider": {
			rows: `[{"id":"a","display_name":"a (MixedCase)"},{"id":"b","display_name":"b (-x)"},{"id":"c","display_name":"c (x.y)"},{"id":"d","display_name":"d (x) trailing"},{"id":"gpt-other","display_name":"gpt-other (7-new)"}]`,
			want: []proxymodels.Route{{ID: "gpt-other", Provider: "7-new"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, proxymodels.ParseListing(jsontext.Value(tt.rows))); diff != "" {
				t.Errorf("ParseListing (-want +got):\n%s", diff)
			}
		})
	}
}

func TestModelsFileRoutes(t *testing.T) {
	tests := map[string]struct {
		value string
		want  []proxymodels.Route
	}{
		"success: unique Codex ids": {`["gpt-5.4","glm-5","auto",7,"gpt-5.4","haiku"]`, []proxymodels.Route{{ID: "gpt-5.4", Provider: "codex"}}},
		"error: scalar":             {`"gpt-5.4"`, []proxymodels.Route{}},
		"success: file order":       {`["gpt-z","gpt-a","gpt-z"]`, []proxymodels.Route{{ID: "gpt-z", Provider: "codex"}, {ID: "gpt-a", Provider: "codex"}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, proxymodels.ModelsFileRoutes(jsontext.Value(tt.value))); diff != "" {
				t.Errorf("ModelsFileRoutes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestProviderLabel(t *testing.T) {
	tests := map[string]struct{ provider, want string }{
		"success: opencode": {"opencode", "OpenCode"}, "success: kimi": {"kimi", "Kimi"}, "success: unknown": {"mistral", "Mistral"},
		"success: codex": {"codex", "Codex"}, "success: cursor": {"cursor", "Cursor"}, "success: grok": {"grok", "Grok"},
		"success: lmstudio": {"lmstudio", "LM Studio"}, "success: local": {"local", "Local GGUF"}, "success: muse": {"muse", "Muse"},
		"success: vertex": {"vertex", "Vertex AI"}, "success: empty": {"", "Proxy"},
		"success: capitalize constructor provider": {"constructor", "Constructor"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, proxymodels.ProviderLabel(tt.provider)); diff != "" {
				t.Errorf("ProviderLabel (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDisplayName(t *testing.T) {
	tests := map[string]struct{ id, provider, want string }{"success: opencode": {"glm-5.3", "opencode", "glm-5.3 (OpenCode)"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, proxymodels.DisplayName(tt.id, tt.provider)); diff != "" {
				t.Errorf("DisplayName (-want +got):\n%s", diff)
			}
		})
	}
}
