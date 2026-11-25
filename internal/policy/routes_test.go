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

package policy_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

func TestListedRoutes(t *testing.T) {
	tests := map[string]struct{ id, provider string }{
		"success: Kimi":                         {"kimi-k3", "kimi"},
		"success: OpenCode":                     {"opencode-go/kimi-k2.6", "opencode"},
		"success: Cursor":                       {"cursor-agent", "cursor"},
		"success: Grok only reachable by proxy": {"grok-composer-2.5-fast", "grok"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := policy.Resolver{Routes: proxymodels.NewTable([]proxymodels.Route{{ID: tt.id, Provider: tt.provider}})}
			alias := "claude-" + tt.id
			want := policy.Model{Backend: tt.provider, Transport: "proxy", BackendID: tt.id, Sentry: "none", AdvertisedWindow: 200000, Measurement: "unmeasured default for claude-code-proxy " + tt.provider + " rows absent from this table", PickerAlias: alias + "[1m]", PickerAliasTemplate: aliasTemplate}
			equal(t, want, modelFor(t, r, alias))
			equal(t, int64(200000), r.AdvertisedWindow(alias))
			equal(t, alias, r.ClientID(alias))
			equal(t, alias, r.ClientID(tt.id))
		})
	}
}

func TestNativeRoutes(t *testing.T) {
	r := policy.Resolver{Routes: proxymodels.NewTable([]proxymodels.Route{{ID: "grok-4.7", Provider: "grok"}, {ID: "gpt-6-astra", Provider: "codex"}, {ID: "gpt-6-sol-fast", Provider: "codex"}, {ID: "claude-opus-5-5", Provider: "codex"}})}
	tests := map[string]struct{ id, backend, transport string }{
		"success: native Grok":    {"claude-grok-4.7[1m]", "grok", "grok"},
		"success: measured Codex": {"claude-gpt-6-astra[1m]", "codex", "proxy"},
		"success: default Codex":  {"claude-gpt-6-sol-fast[1m]", "codex", "proxy"},
		"success: native Claude":  {"claude-opus-5-5[1m]", "anthropic", "anthropic"},
		"success: reserved key":   {"claude-default", "anthropic", "anthropic"},
		"success: native Opus":    {"claude-opus-4-8[1m]", "anthropic", "anthropic"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := modelFor(t, r, tt.id)
			equal(t, tt.backend, got.Backend)
			equal(t, tt.transport, got.Transport)
			if tt.backend == "anthropic" {
				equal(t, "none", got.Sentry)
			}
		})
	}
}

func TestPickerSpelling(t *testing.T) {
	const bare = "vertex/anthropic/claude-fable-5-2"
	const renamed = "vertex/anthropic/fable-5-2"
	const client = "claude-" + renamed + "[1m]"
	listed := []proxymodels.Route{{ID: bare, Provider: "vertex"}, {ID: "opencode-go/claude-fable-5", Provider: "opencode"}}
	collision := []proxymodels.Route{{ID: bare, Provider: "vertex"}, {ID: renamed, Provider: "vertex"}}
	tests := map[string]struct {
		routes                         []proxymodels.Route
		id, backend, client, transport string
	}{
		"success: unlisted known Fable":           {nil, "claude-vertex/anthropic/fable-5-1[1m]", "vertex/anthropic/claude-fable-5-1", "claude-vertex/anthropic/fable-5-1[1m]", "proxy"},
		"success: backend spelling":               {listed, bare, bare, client, "proxy"},
		"success: advertised spelling":            {listed, client, bare, client, "proxy"},
		"success: prefixed backend spelling":      {listed, "claude-" + bare + "[1m]", bare, client, "proxy"},
		"success: OpenCode Fable":                 {listed, "claude-opencode-go/fable-5", "opencode-go/claude-fable-5", "claude-opencode-go/fable-5", "proxy"},
		"success: native Fable":                   {nil, "claude-fable-5-2", "fable-5-2", "claude-fable-5-2", "anthropic"},
		"success: native Fable with suffix":       {nil, "claude-fable-5-1[1m]", "fable-5-1", "claude-fable-5-1", "anthropic"},
		"success: Vertex Opus":                    {[]proxymodels.Route{{ID: "vertex/anthropic/claude-opus-5-5", Provider: "vertex"}}, "claude-vertex/anthropic/claude-opus-5-5[1m]", "vertex/anthropic/claude-opus-5-5", "claude-vertex/anthropic/claude-opus-5-5[1m]", "proxy"},
		"success: alias collides":                 {collision, "claude-" + renamed, renamed, "claude-" + renamed, "proxy"},
		"success: preserve original in collision": {collision, bare, bare, "claude-" + bare + "[1m]", "proxy"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := policy.Resolver{Routes: proxymodels.NewTable(tt.routes)}
			equal(t, tt.backend, r.BackendID(tt.id))
			equal(t, tt.transport, modelFor(t, r, tt.id).Transport)
			equal(t, tt.client, r.ClientID(tt.id))
		})
	}
}

func TestVertexListing(t *testing.T) {
	const unmeasured = `vertex/anthropic/claude-opus-4-5@20251101 vertex/anthropic/claude-haiku-4-5@20251001 vertex/xai/grok-4.3 vertex/mistralai/mistral-medium-3 vertex/mistralai/mistral-small-2503 vertex/mistralai/codestral-2 vertex/openai/gpt-oss-120b-maas vertex/deepseek-ai/deepseek-v4.1-flash-maas`
	const registered = `gemini-3-flash-preview gemini-3.1-flash-lite gemini-3.1-pro-preview gemini-3.5-flash gemini-3.5-flash-lite gemini-3.7-flash gemini-3.8-flash gemini-4-argon gemini-4-carbon gemini-4-hydrogen gemini-4-neon vertex/anthropic/claude-fable-5 vertex/anthropic/claude-fable-5-1 vertex/anthropic/claude-fable-5-2 vertex/anthropic/claude-haiku-4-5@20251001 vertex/anthropic/claude-haiku-5-5 vertex/anthropic/claude-opus-4-5@20251101 vertex/anthropic/claude-opus-4-6 vertex/anthropic/claude-opus-4-7 vertex/anthropic/claude-opus-4-8 vertex/anthropic/claude-opus-5 vertex/anthropic/claude-opus-5-5 vertex/anthropic/claude-sonnet-4-6 vertex/anthropic/claude-sonnet-5 vertex/anthropic/claude-sonnet-5-5 vertex/deepseek-ai/deepseek-v4.1-flash-maas vertex/gemini-3-flash-preview vertex/gemini-3.1-flash-lite vertex/gemini-3.1-pro-preview vertex/gemini-3.5-flash vertex/gemini-3.5-flash-lite vertex/gemini-3.7-flash vertex/gemini-3.8-flash vertex/gemini-4-argon vertex/gemini-4-carbon vertex/gemini-4-hydrogen vertex/gemini-4-neon vertex/google/gemma-4-26b-a4b-it-maas vertex/meta/llama-4-maverick-17b-128e-instruct-maas vertex/meta/llama-4-scout-17b-16e-instruct-maas vertex/meta/muse-ember-1.0 vertex/meta/muse-spark-1.3 vertex/meta/muse-wonder-1.0 vertex/mistralai/codestral-2 vertex/mistralai/mistral-medium-3 vertex/mistralai/mistral-small-2503 vertex/openai/gpt-6-astra vertex/openai/gpt-6-luna vertex/openai/gpt-6.1-sol vertex/openai/gpt-6.1-terra vertex/openai/gpt-oss-120b-maas vertex/xai/grok-4.20-non-reasoning vertex/xai/grok-4.20-reasoning vertex/xai/grok-4.3 vertex/xai/grok-4.6 vertex/xai/grok-4.7 vertex/zai-org/glm-5.2-maas`
	rows := map[string]policy.Model{}
	bare := 0
	names := strings.Fields(registered)
	for _, p := range policy.Rows() {
		if p.Backend != "vertex" {
			continue
		}
		rows[p.BackendID] = p
		if !slices.Contains(names, p.BackendID) {
			t.Errorf("%q not registered", p.BackendID)
		}
		if !strings.Contains(p.BackendID, "/") {
			bare++
		}
	}
	equal(t, 49, len(rows))
	equal(t, 11, bare)
	for id, p := range rows {
		if strings.Contains(id, "/") {
			continue
		}
		p.BackendID = "vertex/" + id
		p.PickerAlias = "claude-vertex/" + id + "[1m]"
		equal(t, p, rows[p.BackendID])
	}
	var missing []string
	for _, id := range names {
		if _, ok := rows[id]; !ok {
			missing = append(missing, id)
		}
	}
	slices.Sort(missing)
	equal(t, slices.Sorted(strings.FieldsSeq(unmeasured)), missing)
	for id := range strings.FieldsSeq(unmeasured + " vertex/endpoint/my-llm") {
		t.Run(id, func(t *testing.T) {
			_, ok := rows[id]
			equal(t, false, ok)
			alias := "claude-" + id
			equal(t, "anthropic", modelFor(t, policy.Resolver{}, alias).Transport)
			r := policy.Resolver{Routes: proxymodels.NewTable([]proxymodels.Route{{ID: id, Provider: "vertex"}})}
			p := modelFor(t, r, alias)
			equal(t, "vertex", p.Backend)
			equal(t, "proxy", p.Transport)
			equal(t, "none", p.Sentry)
			equal(t, "unmeasured default for claude-code-proxy vertex rows absent from this table", p.Measurement)
			equal(t, alias, r.ClientID(id))
			equal(t, int64(200000), r.AdvertisedWindow(id))
		})
	}
}

func TestLiveRows(t *testing.T) {
	tests := map[string]struct {
		window, trigger, advertised int64
		long                        bool
	}{
		"success: no probe":                {-1, 0, 200000, false},
		"success: unloaded or router mode": {0, 0, 200000, false},
		"success: below floor":             {8192, 0, 8192, false},
		"success: at floor":                {32768, 21846, 32768, false},
		"success: one third of 40960":      {40960, 27307, 40960, false},
		"success: one third of 65536":      {65536, 43691, 65536, false},
		"success: capped reserve":          {131072, 91072, 131072, false},
		"success: exact client threshold":  {200000, 160000, 200000, false},
		"success: first long alias":        {200001, 160001, 200001, true},
		"success: 256 Ki":                  {262144, 222144, 262144, true},
		"success: 1 Mi":                    {1048576, 1008576, 1000000, true},
		"success: 2 Mi":                    {2097152, 2057152, 1000000, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, backend := range []string{"lmstudio", "local"} {
				t.Run(backend, func(t *testing.T) {
					key := "ibm/granite-4-micro"
					id := "lmstudio/" + key
					if backend == "local" {
						id = "local/gguf"
					}
					r := policy.Resolver{Routes: proxymodels.NewTable([]proxymodels.Route{{ID: id, Provider: backend}}), LocalPort: 8080, LMStudioURL: "http://127.0.0.1:1234"}
					if tt.window >= 0 {
						r.LocalWindow = tt.window
						r.LMStudioWindows = map[string]int64{key: tt.window, "qwen3-coder-30b": 16384}
					}
					alias := "claude-" + id
					client := alias
					if tt.long {
						client += "[1m]"
					}
					for _, spelling := range []string{id, alias, alias + "[1m]"} {
						p := modelFor(t, r, spelling)
						equal(t, backend, p.Backend)
						equal(t, id, p.BackendID)
						equal(t, "proxy", p.Transport)
						wantSentry := "none"
						var want *policy.Sentry
						if tt.trigger != 0 {
							wantSentry = "synthetic-413"
							want = &policy.Sentry{BackendWindow: tt.window, CompactTrigger: float64(tt.trigger), Source: "derived"}
						}
						equal(t, wantSentry, p.Sentry)
						s, err := policy.EffectiveSentry(&p, 0, 0, 0)
						if err != nil {
							t.Fatal(err)
						}
						equal(t, want, s)
						equal(t, tt.advertised, r.AdvertisedWindow(spelling))
						equal(t, client, r.ClientID(spelling))
						if tt.window > 0 {
							note := fmt.Sprintf("llama-server /props n_ctx %d on 127.0.0.1:8080, read live", tt.window)
							if backend == "lmstudio" {
								note = fmt.Sprintf("LM Studio /api/v1/models loaded context_length %d at http://127.0.0.1:1234, read live", tt.window)
							}
							equal(t, note, p.Measurement)
						}
					}
				})
			}
		})
	}
}
