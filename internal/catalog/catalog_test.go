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

package catalog_test

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

const billingNote = "compacts past 187000; OpenAI bills input above 272k tokens at 2x; the crossing turn and compaction request can still exceed 272k and pay double"

// These are policy observations, not prebuilt catalog rows: the builder still chooses
// membership, order, slug, label, optional fields and provider readiness.
func modelPolicy(id string) catalog.Policy {
	base := strings.TrimSuffix(strings.TrimPrefix(id, "claude-"), "[1m]")
	p := catalog.Policy{BackendID: base, ClientID: "claude-" + base, Transport: "proxy", Window: 200000}
	switch {
	case base == "codex-auto":
		p.Provider, p.BackendID = "codex", "auto"
	case strings.HasPrefix(base, "gpt-"):
		p.Provider, p.Window, p.WindowNote = "codex", 272000, billingNote
	case base == "grok-4.5":
		p.Provider, p.Transport, p.Window = "grok", "grok", 500000
	case base == "grok-composer-2.5-fast":
		p.Provider = "grok"
	case base == "kimi-k3":
		p.Provider = "kimi"
	case base == "glm-5.3" || base == "opencode-go/kimi-k2.6":
		p.Provider = "opencode"
	case base == "cursor-agent":
		p.Provider = "cursor"
	case base == "muse-spark-1.3":
		p.Provider, p.Window = "muse", 1000000
	case base == "gemini-4-argon" || strings.HasPrefix(base, "vertex/anthropic/"):
		p.Provider, p.Window = "vertex", 1000000
	case strings.HasPrefix(base, "vertex/"):
		p.Provider = "vertex"
	default:
		p.Provider, p.Transport, p.Window = "anthropic", "anthropic", 0
	}
	if p.Window > 200000 {
		p.ClientID += "[1m]"
	}
	return p
}

func checkJSON(t *testing.T, want string, got any) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var expected, actual any
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatalf("invalid expected JSON: %v", err)
	}
	if err := json.Unmarshal(b, &actual); err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(expected, actual); diff != "" {
		t.Errorf("catalog JSON (-want +got):\n%s", diff)
	}
}

func checkCodexMirror(t *testing.T, got catalog.Catalog) {
	t.Helper()
	provider, exists := got.Providers["codex"]
	if !exists || got.CodexReadiness == nil {
		t.Fatalf("missing Codex summary: provider present=%t, mirror present=%t", exists, got.CodexReadiness != nil)
	}
	if diff := gocmp.Diff(provider, *got.CodexReadiness); diff != "" {
		t.Errorf("Codex readiness (-provider +mirror):\n%s", diff)
	}
}

func TestCatalogContract(t *testing.T) {
	inputs := catalog.Inputs{
		Policy: modelPolicy, UpdatedAt: time.Date(1970, 1, 1, 9, 0, 1, 120000000, time.FixedZone("JST", 9*60*60)), Version: "0.56.3",
		Providers: map[string]catalog.Readiness{
			"codex": {Ready: true, State: "ready", Message: "Codex is ready."},
			"grok":  {State: "auth-missing", Message: "Grok CLI auth is missing. Run `grok` and log in again."},
		},
	}
	ids := []string{"claude-codex-auto", "claude-gpt-5.6-sol", "claude-gpt-5.6-terra", "claude-gpt-5.6-luna", "claude-gpt-5.6-sol-fast", "claude-gpt-5.6-terra-fast", "claude-gpt-5.6-luna-fast", "claude-grok-4.5", "claude-grok-build"}
	got := catalog.Build(t.Context(), ids, inputs)
	t.Run("codex_mirror", func(t *testing.T) { checkCodexMirror(t, got) })
	tests := map[string]struct{ want, got any }{
		"schema": {4, got.SchemaVersion}, "source": {"model-gateway", got.Source},
		"writer": {"0.56.3", got.WrittenBy}, "timestamp": {"1970-01-01T00:00:01.120Z", got.UpdatedAt},
		"providers": {map[string]catalog.Readiness{
			"codex": {Ready: true, State: "ready", Message: "Codex is ready."},
			"grok":  {State: "auth-missing", Message: "Grok CLI auth is missing. Run `grok` and log in again."},
		}, got.Providers},
		"models": {[]catalog.Model{
			{Slug: "codex-gpt-5-6-sol", ID: "claude-gpt-5.6-sol[1m]", Label: "GPT-5.6 Sol", Provider: "codex", ContextWindow: 272000, ContextWindowNote: billingNote},
			{Slug: "codex-gpt-5-6-terra", ID: "claude-gpt-5.6-terra[1m]", Label: "GPT-5.6 Terra", Provider: "codex", ContextWindow: 272000, ContextWindowNote: billingNote},
			{Slug: "codex-gpt-5-6-luna", ID: "claude-gpt-5.6-luna[1m]", Label: "GPT-5.6 Luna", Provider: "codex", ContextWindow: 272000, ContextWindowNote: billingNote},
			{Slug: "codex-gpt-5-6-sol-fast", ID: "claude-gpt-5.6-sol-fast[1m]", Label: "GPT-5.6 Sol Fast", Provider: "codex", ContextWindow: 272000, ContextWindowNote: billingNote},
			{Slug: "codex-gpt-5-6-terra-fast", ID: "claude-gpt-5.6-terra-fast[1m]", Label: "GPT-5.6 Terra Fast", Provider: "codex", ContextWindow: 272000, ContextWindowNote: billingNote},
			{Slug: "codex-gpt-5-6-luna-fast", ID: "claude-gpt-5.6-luna-fast[1m]", Label: "GPT-5.6 Luna Fast", Provider: "codex", ContextWindow: 272000, ContextWindowNote: billingNote},
			{Slug: "grok-4-5", ID: "claude-grok-4.5[1m]", Label: "Grok 4.5", Provider: "grok", ContextWindow: 500000},
			{Slug: "grok-build", ID: "claude-grok-build", Label: "Grok build", Provider: "grok"},
		}, got.Models},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, tc.got); diff != "" {
				t.Errorf("%s (-want +got):\n%s", name, diff)
			}
		})
	}
}

func TestDiscoveredWindows(t *testing.T) {
	got := catalog.Build(t.Context(), []string{"claude-gpt-5.6-sol[1m]", "claude-grok-4.5[1m]"}, catalog.Inputs{Policy: modelPolicy})
	checkJSON(t, `[{"id":"claude-gpt-5.6-sol[1m]","contextWindow":272000,"contextWindowNote":"`+billingNote+`","label":"GPT-5.6 Sol","provider":"codex","slug":"codex-gpt-5-6-sol"},{"id":"claude-grok-4.5[1m]","contextWindow":500000,"label":"Grok 4.5","provider":"grok","slug":"grok-4-5"}]`, got.Models)
}

func TestFlatCodexReadiness(t *testing.T) {
	got := catalog.Build(t.Context(), []string{"claude-gpt-5.6-terra"}, catalog.Inputs{
		Policy: modelPolicy, Codex: &catalog.Readiness{State: "auth-missing", Message: "sign in"}, SharedChecks: &catalog.Checks{},
	})
	const summary = `{"ready":false,"state":"auth-missing","message":"sign in"}`
	t.Run("public_fields", func(t *testing.T) { checkJSON(t, `{"codex":`+summary+`}`, got.Providers) })
	t.Run("codex_mirror", func(t *testing.T) { checkCodexMirror(t, got) })
}

type observations struct{ probes []string }

func (s *observations) Probe(_ context.Context, provider string) catalog.Readiness {
	s.probes = append(s.probes, provider)
	return catalog.Readiness{State: "auth-missing", Message: "not signed in"}
}

func (observations) SharedFailureMessage(state string) string { return state }

func TestProxyCatalog(t *testing.T) {
	ids := []string{"claude-gpt-6-sol[1m]", "claude-kimi-k3", "claude-glm-5.3", "claude-opencode-go/kimi-k2.6", "claude-cursor-agent", "claude-grok-composer-2.5-fast", "claude-muse-spark-1.3[1m]", "claude-gemini-4-argon[1m]", "claude-vertex/anthropic/claude-opus-5-5[1m]", "claude-vertex/anthropic/claude-sonnet-5-5[1m]", "claude-vertex/mistralai/codestral-2", "claude-vertex/endpoint/my-llm", "claude-opus-5-5"}
	probe := new(observations)
	inputs := catalog.Inputs{Policy: modelPolicy, Codex: &catalog.Readiness{State: "binary-missing", Message: "Codex dispatch refused: claude-code-proxy is missing."}, SharedChecks: &catalog.Checks{}, Fallback: probe}
	got := catalog.Build(t.Context(), ids, inputs)
	t.Run("windows", func(t *testing.T) {
		want := map[string]int{
			"claude-gpt-6-sol[1m]": 272000, "claude-kimi-k3": 200000, "claude-glm-5.3": 200000,
			"claude-opencode-go/kimi-k2.6": 200000, "claude-cursor-agent": 200000, "claude-grok-composer-2.5-fast": 200000,
			"claude-muse-spark-1.3[1m]": 1000000, "claude-gemini-4-argon[1m]": 1000000, "claude-vertex/anthropic/claude-opus-5-5[1m]": 1000000,
			"claude-vertex/anthropic/claude-sonnet-5-5[1m]": 1000000, "claude-vertex/mistralai/codestral-2": 200000, "claude-vertex/endpoint/my-llm": 200000,
		}
		check := func(rows []catalog.Model) {
			t.Helper()
			windows := make(map[string]int, len(rows))
			for _, row := range rows {
				if _, duplicate := windows[row.ID]; duplicate {
					t.Errorf("duplicate model ID %q", row.ID)
				}
				windows[row.ID] = row.ContextWindow
			}
			if diff := gocmp.Diff(want, windows); diff != "" {
				t.Errorf("windows by model ID (-want +got):\n%s", diff)
			}
		}
		check(got.Models)
		capped := inputs
		capped.Fallback = new(observations)
		capped.Policy = func(id string) catalog.Policy {
			p := modelPolicy(id)
			switch p.Provider {
			case "codex":
				p.Window, p.WindowNote, p.ClientID = 200000, strings.Replace(billingNote, "187000", "115000", 1), strings.TrimSuffix(p.ClientID, "[1m]")
			case "grok":
				p.Window, p.WindowNote = 190000, "compacts past 105000"
			}
			return p
		}
		delete(want, "claude-gpt-6-sol[1m]")
		want["claude-gpt-6-sol"], want["claude-grok-composer-2.5-fast"] = 200000, 190000
		check(catalog.Build(t.Context(), ids, capped).Models)
	})
	t.Run("codex_note", func(t *testing.T) {
		if len(got.Models) == 0 {
			t.Fatal("missing Codex model")
		}
		if diff := gocmp.Diff(billingNote, got.Models[0].ContextWindowNote); diff != "" {
			t.Error(diff)
		}
	})
	t.Run("rows", func(t *testing.T) {
		var rows [][4]string
		for _, m := range got.Models {
			rows = append(rows, [4]string{m.Slug, m.ID, m.Label, m.Provider})
		}
		want := [][4]string{
			{"codex-gpt-6-sol", "claude-gpt-6-sol[1m]", "GPT-6 Sol", "codex"},
			{"kimi-k3", "claude-kimi-k3", "kimi-k3 (Kimi)", "kimi"},
			{"opencode-glm-5-3", "claude-glm-5.3", "glm-5.3 (OpenCode)", "opencode"},
			{"opencode-go-kimi-k2-6", "claude-opencode-go/kimi-k2.6", "opencode-go/kimi-k2.6 (OpenCode)", "opencode"},
			{"cursor-agent", "claude-cursor-agent", "cursor-agent (Cursor)", "cursor"},
			{"grok-composer-2-5-fast", "claude-grok-composer-2.5-fast", "Grok composer 2.5 fast", "grok"},
			{"muse-spark-1-3", "claude-muse-spark-1.3[1m]", "muse-spark-1.3 (Muse)", "muse"},
			{"vertex-gemini-4-argon", "claude-gemini-4-argon[1m]", "gemini-4-argon (Vertex AI)", "vertex"},
			{"vertex-anthropic-claude-opus-5-5", "claude-vertex/anthropic/claude-opus-5-5[1m]", "vertex/anthropic/claude-opus-5-5 (Vertex AI)", "vertex"},
			{"vertex-anthropic-claude-s-b52873", "claude-vertex/anthropic/claude-sonnet-5-5[1m]", "vertex/anthropic/claude-sonnet-5-5 (Vertex AI)", "vertex"},
			{"vertex-mistralai-codestral-2", "claude-vertex/mistralai/codestral-2", "vertex/mistralai/codestral-2 (Vertex AI)", "vertex"},
			{"vertex-endpoint-my-llm", "claude-vertex/endpoint/my-llm", "vertex/endpoint/my-llm (Vertex AI)", "vertex"},
		}
		if diff := gocmp.Diff(want, rows); diff != "" {
			t.Error(diff)
		}
	})
	t.Run("readiness", func(t *testing.T) {
		if diff := gocmp.Diff([]string{"grok"}, probe.probes); diff != "" {
			t.Errorf("provider probes (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(catalog.Readiness{State: "auth-missing", Message: "not signed in"}, got.Providers["grok"]); diff != "" {
			t.Errorf("independent Grok readiness (-want +got):\n%s", diff)
		}
		for _, p := range []string{"kimi", "opencode", "cursor", "muse", "vertex", "codex"} {
			r, ok := got.Providers[p]
			if !ok {
				t.Errorf("provider %q missing", p)
				continue
			}
			if r.State != "binary-missing" || r.Ready {
				t.Errorf("%s readiness = %+v, want binary-missing and false", p, r)
			}
		}
	})
}

func TestSlugCollisions(t *testing.T) {
	tests := map[string]struct{ ids, want []string }{
		"success: punctuation and numeric suffixes": {[]string{"claude-vertex/A__b!", "claude-vertex/a-b", "claude-vertex/a.b"}, []string{"vertex-a-b", "vertex-a-b-2", "vertex-a-b-3"}},
		"success: exact length then collision":      {[]string{"claude-vertex/anthropic/claude-opus-5-5[1m]", "claude-vertex/anthropic/claude-opus-5-5[1m]"}, []string{"vertex-anthropic-claude-opus-5-5", "vertex-anthropic-claude-opus-5-2"}},
		"success: hash then collision":              {[]string{"claude-vertex/anthropic/claude-sonnet-5-5[1m]", "claude-vertex/anthropic/claude-sonnet-5-5[1m]"}, []string{"vertex-anthropic-claude-s-b52873", "vertex-anthropic-claude-s-b528-2"}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := catalog.Build(t.Context(), tc.ids, catalog.Inputs{Policy: modelPolicy})
			var slugs []string
			for _, m := range got.Models {
				slugs = append(slugs, m.Slug)
			}
			if diff := gocmp.Diff(tc.want, slugs); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestSharedReadinessPrecedence(t *testing.T) {
	tests := map[string]struct {
		checks catalog.Checks
		want   string
	}{
		"error: binary before proxy":   {catalog.Checks{}, "binary-missing"},
		"error: proxy before shim":     {catalog.Checks{ProxyBinary: true}, "proxy-down"},
		"error: shim before version":   {catalog.Checks{ProxyBinary: true, ProxyModels: true}, "shim-down"},
		"error: stale serving version": {catalog.Checks{ProxyBinary: true, ProxyModels: true, ShimRunning: true}, "serving-version-mismatch"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			probe := new(observations)
			got := catalog.Build(t.Context(), []string{"claude-kimi-k3", "claude-grok-4.5", "claude-grok-build"}, catalog.Inputs{Policy: modelPolicy, SharedChecks: &tc.checks, Fallback: probe})
			want := map[string]catalog.Readiness{"kimi": {State: tc.want, Message: tc.want}, "grok": {State: "auth-missing", Message: "not signed in"}}
			if diff := gocmp.Diff(want, got.Providers); diff != "" {
				t.Error(diff)
			}
			if diff := gocmp.Diff([]string{"grok"}, probe.probes); diff != "" {
				t.Errorf("provider probes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCatalogMembershipWithoutProxyPolicy(t *testing.T) {
	tests := map[string]struct {
		id     string
		policy catalog.Policy
		want   string
	}{
		"success: unresolved legacy Codex":           {"claude-codex-gpt-5.6-sol", catalog.Policy{}, `[{"slug":"codex-gpt-5-6-sol","id":"claude-codex-gpt-5.6-sol","label":"GPT-5.6 Sol","provider":"codex"}]`},
		"success: Grok prefix despite native policy": {"claude-grok-build", catalog.Policy{BackendID: "grok-build", Provider: "anthropic", Transport: "anthropic", ClientID: "claude-grok-build"}, `[{"slug":"grok-build","id":"claude-grok-build","label":"Grok build","provider":"grok"}]`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := catalog.Build(t.Context(), []string{tc.id}, catalog.Inputs{Policy: func(string) catalog.Policy { return tc.policy }})
			checkJSON(t, tc.want, got.Models)
		})
	}
}

func TestProviderPublicationOrder(t *testing.T) {
	// Native and dispatch rows are ignored; the repeated Grok row must not move its provider.
	ids := []string{"claude-codex-auto", "claude-opus-5-5", "claude-muse-spark-1.3[1m]", "claude-grok-4.5", "claude-cursor-agent", "claude-grok-build", "claude-gpt-5.6-sol"}
	got := catalog.Build(t.Context(), ids, catalog.Inputs{Policy: modelPolicy})
	data, err := jsonx.EncodeState(got)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Providers jsontext.Value `json:"providers"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	providers, err := jsonx.DecodeWire(document.Providers)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, member := range providers.Members {
		names = append(names, string(utf16.Decode(member.Name)))
	}
	if diff := gocmp.Diff([]string{"muse", "grok", "cursor", "codex"}, names); diff != "" {
		t.Errorf("published provider keys (-want +got):\n%s", diff)
	}
}

func TestEmptyCatalog(t *testing.T) {
	got := catalog.Build(t.Context(), nil, catalog.Inputs{Policy: modelPolicy, Version: "custom-build", UpdatedAt: time.Unix(1, 999999)})
	checkJSON(t, `{"schemaVersion":4,"source":"model-gateway","updatedAt":"1970-01-01T00:00:01.000Z","writtenBy":"custom-build","providers":{},"codexReadiness":null,"models":[]}`, got)
}
