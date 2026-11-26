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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

func TestMissingVersionWritesString(t *testing.T) {
	tests := map[string]struct {
		inputs catalog.Inputs
		want   string
	}{
		"success: no writer version":           {catalog.Inputs{}, `"0.0.0-dev"`},
		"success: keep explicit writer string": {catalog.Inputs{Version: " build "}, `" build "`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			data, err := jsonx.EncodeState(catalog.Build(t.Context(), nil, tc.inputs))
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				WrittenBy jsontext.Value `json:"writtenBy"`
			}
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tc.want, string(document.WrittenBy)); diff != "" {
				t.Errorf("writtenBy (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPrepare(t *testing.T) {
	const unmeasured = "grok-composer-unmeasured"
	tests := map[string]struct {
		ids       []string
		windows   policy.Windows
		want      []catalog.Model
		wantErr   string
		labelBase string
	}{
		"error: unmeasured listing and saved direct maximum": {
			ids: []string{"claude-" + unmeasured}, windows: policy.Windows{"grok": {CompactAt: 120000}},
			wantErr: "cannot format a direct compaction maximum without a synthetic sentry",
		},
		"error: invalid label before an earlier model's window note": {
			ids: []string{"claude-" + unmeasured, "claude-gpt-1--fast"}, windows: policy.Windows{"grok": {CompactAt: 120000}},
			wantErr: `cannot label Codex model "gpt-1--fast": empty suffix word`, labelBase: "gpt-1--fast",
		},
		"success: unmeasured listing without a direct maximum": {
			ids:  []string{"claude-" + unmeasured},
			want: []catalog.Model{{Slug: unmeasured, ID: "claude-" + unmeasured, Label: "Grok composer unmeasured", Provider: "grok", ContextWindow: 200000}},
		},
		"success: measured model with a direct maximum": {
			ids: []string{"claude-grok-4.5[1m]"}, windows: policy.Windows{"grok": {CompactAt: 120000}},
			want: []catalog.Model{{
				Slug: "grok-4-5", ID: "claude-grok-4.5[1m]", Label: "Grok 4.5", Provider: "grok", ContextWindow: 500000,
				ContextWindowNote: "requested compact-at 120000; backend window 500000 (grok-4.5); effective maximum 120000 (compact-at); compacts past 120000",
			}},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			probe := new(observations)
			ids := slices.Clone(tc.ids)
			prepared, err := catalog.Prepare(ids, catalog.Inputs{Fallback: probe}, policy.ResolverInputs{
				Routes: proxymodels.NewTable([]proxymodels.Route{{ID: unmeasured, Provider: "grok"}}), Windows: tc.windows,
			}, nil)
			if len(probe.probes) != 0 {
				t.Fatalf("readiness probed before Build: %v", probe.probes)
			}
			if tc.wantErr != "" {
				if prepared != nil || err == nil || err.Error() != tc.wantErr {
					t.Fatalf("Prepare returned buildable=%t, error=%v; want no prepared inputs and %q", prepared != nil, err, tc.wantErr)
				}
				if tc.labelBase != "" {
					labelErr, ok := errors.AsType[*catalog.LabelError](err)
					if !ok || labelErr.Base != tc.labelBase {
						t.Fatalf("error = %T %v; want LabelError for %q", err, err, tc.labelBase)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ids[0] = "claude-gpt-1--late-change"
			if diff := gocmp.Diff(tc.want, prepared.Build(t.Context()).Models); diff != "" {
				t.Errorf("prepared models (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseIDs(t *testing.T) {
	tests := map[string]struct {
		wire string
		want []string
	}{
		"success: isolated escaped surrogates are not replacement characters": {
			`[{"id":"claude-grok-\ud800"},{"id":"claude-grok-�"},{"id":"claude-grok-\udfff"},{"id":"claude-grok-😀"}]`,
			[]string{"claude-grok-�", "claude-grok-😀"},
		},
		"success: ill-formed UTF-8 is not a replacement character": {
			"[{\"id\":\"claude-grok-\xff\"},{\"id\":\"claude-grok-�\"}]",
			[]string{"claude-grok-�"},
		},
		"success: last exact id field wins; non-string ids are ignored": {
			`[null,12,{"id":true},{"ID":"claude-grok-ignored"},{"id":"claude-grok-ignored","id":"claude-grok-kept"},{"id":"claude-grok-ignored","id":"\ud800"}]`,
			[]string{"claude-grok-kept"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ids, err := catalog.ParseIDs(jsontext.Value(tc.wire))
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tc.want, ids); diff != "" {
				t.Errorf("parsed IDs (-want +got):\n%s", diff)
			}
			ids = append([]string{"claude-grok-\xed\xa0\x80"}, ids...)
			models := catalog.Build(t.Context(), ids, catalog.Inputs{}).Models
			got := make([]string, 0, len(models))
			for _, m := range models {
				got = append(got, m.ID)
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("published IDs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSlugCasingAndCollisionOrder(t *testing.T) {
	tests := map[string]struct {
		ids  []string
		want []string
	}{
		"success: full lowercase before stripping punctuation": {
			[]string{"claude-grok-İD", "claude-grok-Key[1m]", "claude-grok-ΟΣ"}, []string{"grok-i-d", "grok-key", "grok"},
		},
		"success: the window suffix matches only the actual end": {
			[]string{"claude-grok-a[1M]", "claude-grok-a[1m]\n"}, []string{"grok-a", "grok-a-1m"},
		},
		"success: an existing literal suffix occupies that collision slot": {
			[]string{"claude-grok-a", "claude-grok-a-2", "claude-grok-a"}, []string{"grok-a", "grok-a-2", "grok-a-3"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rows := catalog.Build(t.Context(), tc.ids, catalog.Inputs{}).Models
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.Slug)
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("slugs (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildInvalidLabel(t *testing.T) {
	const invalid = "claude-codex-gpt-1--fast"
	tests := map[string]struct{ ids []string }{
		"error: invalid legacy label without a policy": {[]string{invalid}},
		"error: discard earlier valid rows too":        {[]string{"claude-grok-4.5", invalid}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := catalog.Build(t.Context(), tc.ids, catalog.Inputs{})
			if diff := gocmp.Diff(catalog.Catalog{}, got); diff != "" {
				t.Errorf("catalog (-want +got):\n%s", diff)
			}
			if data, err := jsonx.EncodeState(got); err == nil {
				t.Errorf("invalid catalog serialized: %s", data)
			}
		})
	}
}

func TestPreparedReadinessSnapshot(t *testing.T) {
	tests := map[string]struct {
		ids  []string
		want map[string]catalog.Readiness
	}{
		"success: supplied readings are owned and fallback is probed on each build": {
			ids: []string{"claude-cursor-agent", "claude-gpt-1", "claude-kimi-k3", "claude-grok-4.5"},
			want: map[string]catalog.Readiness{
				"cursor": {Ready: true, State: "ready", Message: "cursor observed"},
				"codex":  {Ready: true, State: "ready", Message: "codex observed"},
				"kimi":   {State: "proxy-down", Message: "proxy-down"},
				"grok":   {State: "auth-missing", Message: "not signed in"},
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			probe := new(observations)
			inputs := catalog.Inputs{
				Providers:    map[string]catalog.Readiness{"cursor": {Ready: true, State: "ready", Message: "cursor observed"}},
				Codex:        new(catalog.Readiness{Ready: true, State: "ready", Message: "codex observed"}),
				SharedChecks: new(catalog.Checks{ProxyBinary: true}),
				Fallback:     probe,
			}
			routes := proxymodels.NewTable([]proxymodels.Route{{ID: "cursor-agent", Provider: "cursor"}, {ID: "kimi-k3", Provider: "kimi"}})
			prepared, err := catalog.Prepare(tc.ids, inputs, policy.ResolverInputs{Routes: routes}, nil)
			if err != nil {
				t.Fatal(err)
			}
			delete(inputs.Providers, "cursor")
			*inputs.Codex = catalog.Readiness{}
			*inputs.SharedChecks = catalog.Checks{}
			first := prepared.Build(t.Context())
			if diff := gocmp.Diff(tc.want, first.Providers); diff != "" {
				t.Fatalf("provider snapshot (-want +got):\n%s", diff)
			}
			delete(first.Providers, "cursor")
			first.Models[0].ID = "changed"
			*first.CodexReadiness = catalog.Readiness{}
			second := prepared.Build(t.Context())
			if diff := gocmp.Diff(tc.want, second.Providers); diff != "" {
				t.Errorf("second provider snapshot (-want +got):\n%s", diff)
			}
			if second.Models[0].ID != tc.ids[0] {
				t.Errorf("second model ID = %q, want %q", second.Models[0].ID, tc.ids[0])
			}
			checkCodexMirror(t, second)
			if diff := gocmp.Diff([]string{"grok", "grok"}, probe.probes); diff != "" {
				t.Errorf("probes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTypedNilFallback(t *testing.T) {
	tests := map[string]struct{ prepared bool }{
		"success: direct build ignores typed nil":   {},
		"success: prepared build ignores typed nil": {prepared: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ids := []string{"claude-kimi-k3", "claude-grok-4.5"}
			inputs := catalog.Inputs{Fallback: (*observations)(nil), SharedChecks: &catalog.Checks{}, Policy: modelPolicy}
			var got catalog.Catalog
			if tc.prepared {
				routes := proxymodels.NewTable([]proxymodels.Route{{ID: "kimi-k3", Provider: "kimi"}})
				prepared, err := catalog.Prepare(ids, inputs, policy.ResolverInputs{Routes: routes}, nil)
				if err != nil {
					t.Fatal(err)
				}
				got = prepared.Build(t.Context())
			} else {
				got = catalog.Build(t.Context(), ids, inputs)
			}
			want := map[string]catalog.Readiness{
				"kimi": {State: "binary-missing", Message: "Readiness is unavailable."},
				"grok": {State: "unavailable", Message: "grok readiness is unavailable."},
			}
			if diff := gocmp.Diff(want, got.Providers); diff != "" {
				t.Errorf("providers (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPrepareWindowSnapshot(t *testing.T) {
	tests := map[string]struct {
		ids  []string
		want []int
	}{
		"success: lookup cannot change a later model's cap or note": {
			ids: []string{"claude-grok-4.5", "claude-grok-4.7"}, want: []int{300000, 300000},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			windows := policy.Windows{"grok": {Value: config.ContextWindow{Tokens: 300000}, CompactAt: 120000}}
			var calls int
			lookup := func(string) (string, bool) {
				calls++
				if calls == 1 {
					// Change both values before the second model's window and note are read.
					windows["grok"] = policy.Setting{Value: config.ContextWindow{Tokens: 200000}, CompactAt: 195000}
				}
				return "", false
			}
			prepared, err := catalog.Prepare(tc.ids, catalog.Inputs{}, policy.ResolverInputs{Windows: windows}, lookup)
			if err != nil {
				t.Fatal(err)
			}
			delete(windows, "grok")
			models := prepared.Build(t.Context()).Models
			if calls != len(tc.ids) || len(models) != len(tc.ids) {
				t.Fatalf("lookup calls = %d, models = %d; want %d each", calls, len(models), len(tc.ids))
			}
			for i, row := range models {
				wantNote := fmt.Sprintf("requested compact-at 120000; backend window 500000 (%s); effective maximum 120000 (compact-at); compacts past 120000", tc.ids[i][len("claude-"):])
				if row.ContextWindow != tc.want[i] || row.ContextWindowNote != wantNote {
					t.Errorf("%s: window %d, note %q; want window %d, note %q", row.ID, row.ContextWindow, row.ContextWindowNote, tc.want[i], wantNote)
				}
			}
		})
	}
}

func TestProviderPropertyNames(t *testing.T) {
	tests := map[string]struct{ provider string }{
		"success: checks is an ordinary provider":      {"checks"},
		"success: constructor is an ordinary provider": {"constructor"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The JavaScript property collisions select the flat checks or inherited constructor,
			// so both providers get state "unavailable" instead of the shared check's failure.
			probe := new(observations)
			got := catalog.Build(t.Context(), []string{"claude-example"}, catalog.Inputs{
				SharedChecks: &catalog.Checks{}, Fallback: probe,
				Policy: func(string) catalog.Policy {
					return catalog.Policy{Provider: tc.provider, BackendID: "example", Transport: "proxy"}
				},
			})
			want := map[string]catalog.Readiness{tc.provider: {State: "binary-missing", Message: "binary-missing"}}
			if diff := gocmp.Diff(want, got.Providers); diff != "" {
				t.Errorf("providers (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCodexLabelUnicode(t *testing.T) {
	tests := map[string]struct{ suffix, want string }{
		"success: initial outside BMP is unchanged": {"𐐨-fast", "GPT-1 𐐨 Fast"},
		"success: LF prevents suffix capture":       {"before\n-after", "GPT-1 before\n after"},
		"success: CR prevents suffix capture":       {"before\r-after", "GPT-1 before\r after"},
		"success: LS prevents suffix capture":       {"before -after", "GPT-1 before  after"},
		"success: PS prevents suffix capture":       {"before -after", "GPT-1 before  after"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			models := catalog.Build(t.Context(), []string{"claude-codex-gpt-1-" + tc.suffix}, catalog.Inputs{}).Models
			if len(models) != 1 || models[0].Label != tc.want {
				t.Errorf("models = %+v; want one with label %q", models, tc.want)
			}
		})
	}
}

func TestShortSlugs(t *testing.T) {
	tests := map[string]struct{ provider, base, want string }{
		"success: empty normalized slug becomes x": {"é", "!", "x"},
		"success: one character stays one":         {"1", "1/", "1"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			models := catalog.Build(t.Context(), []string{"claude-example"}, catalog.Inputs{
				Policy: func(string) catalog.Policy {
					return catalog.Policy{Provider: tc.provider, BackendID: tc.base, Transport: "proxy"}
				},
			}).Models
			if len(models) != 1 || models[0].Slug != tc.want {
				t.Errorf("models = %+v; want one with slug %q", models, tc.want)
			}
		})
	}
}

func TestCatalogWireOrder(t *testing.T) {
	const want = `{"schemaVersion":4,"source":"model-gateway","updatedAt":"1970-01-01T00:00:00.000Z","writtenBy":"v","providers":{"2":{"ready":false,"state":"unavailable","message":"2 readiness is unavailable."},"10":{"ready":false,"state":"unavailable","message":"10 readiness is unavailable."},"01":{"ready":false,"state":"unavailable","message":"01 readiness is unavailable."}},"codexReadiness":null,"models":[{"slug":"10-a","id":"claude-a","label":"a (10)","provider":"10","contextWindow":200000,"contextWindowNote":"note"},{"slug":"01-b","id":"claude-b","label":"b (01)","provider":"01"},{"slug":"2-c","id":"claude-c","label":"c (2)","provider":"2"}]}`
	tests := map[string]struct {
		ids  []string
		want string
	}{
		"success: array-index providers sort before other source-ordered members": {[]string{"claude-a", "claude-b", "claude-c"}, want},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			inputs := catalog.Inputs{
				Version: "v", UpdatedAt: time.Unix(0, 0), Policy: func(id string) catalog.Policy {
					switch id {
					case "claude-a":
						return catalog.Policy{Provider: "10", BackendID: "a", Transport: "proxy", Window: 200000, WindowNote: "note"}
					case "claude-b":
						return catalog.Policy{Provider: "01", BackendID: "b", Transport: "proxy", WindowNote: "not published without a window"}
					default:
						return catalog.Policy{Provider: "2", BackendID: "c", Transport: "proxy"}
					}
				},
			}
			data, err := jsonx.EncodeState(catalog.Build(t.Context(), tc.ids, inputs))
			if err != nil {
				t.Fatal(err)
			}
			wire := jsontext.Value(data)
			if err := wire.Compact(); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tc.want, string(wire)); diff != "" {
				t.Errorf("wire (-want +got):\n%s", diff)
			}
		})
	}
}
