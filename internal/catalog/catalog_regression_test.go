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
	"slices"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
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
		ids     []string
		windows policy.Windows
		want    []catalog.Model
		wantErr string
	}{
		"error: unmeasured listing and saved direct maximum": {
			ids: []string{"claude-" + unmeasured}, windows: policy.Windows{"grok": {CompactAt: 120000}},
			wantErr: "cannot format a direct compaction maximum without a synthetic sentry",
		},
		"error: invalid label before an earlier model's window note": {
			ids: []string{"claude-" + unmeasured, "claude-gpt-1--fast"}, windows: policy.Windows{"grok": {CompactAt: 120000}},
			wantErr: "Cannot read properties of undefined (reading 'toUpperCase')",
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
