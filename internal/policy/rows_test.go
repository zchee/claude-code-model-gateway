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
	"os"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func equal[T any](t *testing.T, want, got T) {
	t.Helper()
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func modelFor(t *testing.T, r policy.Resolver, id string) policy.Model {
	t.Helper()
	p := r.Resolve(id)
	if p == nil {
		t.Fatalf("Resolve(%q) returned no policy", id)
	}
	return *p
}

const (
	aliasTemplate = "claude-{backendId}[1m]"
	defaultNote   = "unmeasured default for Codex proxy rows absent from this table"
	measuredEarly = "measured 2026-09-05: 920012 accepted; 935012 refused"
	measuredLate  = "measured 2026-09-30: 920012 accepted; 935012 refused"
	fastGrokNote  = "same model as grok-4.7 per claude-code-proxy grok-4.7-fast branch; window from grok-4.7 (models.dev 2026-10-17, Grok CLI README 2026-10-16); not a live probe"
)

func expectedRows() []policy.Model {
	rows := []policy.Model{{Backend: "codex", BackendID: "default", BackendWindow: 920000, AdvertisedWindow: 920000, Measurement: defaultNote, PickerAliasTemplate: aliasTemplate, Sentry: "synthetic-413"}}
	add := func(backend, transport, id string, window, advertised int64, note string) {
		rows = append(rows, policy.Model{Backend: backend, Transport: transport, BackendID: id, BackendWindow: window, AdvertisedWindow: advertised, Measurement: note, PickerAlias: "claude-" + id + "[1m]", Sentry: "synthetic-413"})
	}
	for id := range strings.FieldsSeq("gpt-5.6-sol gpt-5.6-terra gpt-5.6-luna gpt-6-astra") {
		add("codex", "", id, 920012, 920000, measuredEarly)
	}
	add("codex", "", "gpt-6.1-sol", 920012, 920000, measuredLate)
	add("grok", "", "grok-4.7", 500000, 500000, "models.dev xai/grok-4.7 2026-10-17 (context 500000) and Grok CLI models_cache.json 2026-09-23 (500000); the Grok CLI README calls its 256000 default an auto-compact threshold with 500000 selectable, not a backend limit; not a live probe")
	for id := range strings.FieldsSeq("grok-4.7-fast grok-4.7-build-fast") {
		add("grok", "proxy", id, 500000, 500000, fastGrokNote)
	}
	add("grok", "", "grok-4.6", 500000, 500000, "measured 2026-09-15 from Grok CLI models_cache.json (context_window 500000)")
	add("grok", "", "grok-4.5", 500000, 500000, "measured 2026-09-05 from grok-backend.js GROK_MODELS")
	for id := range strings.FieldsSeq("muse-spark-1.3 muse-spark-1.2 muse-wonder-1.0 muse-wonder-full-1.0 muse-ember-1.0") {
		add("muse", "proxy", id, 1007997, 1000000, "claude-code-proxy dev 1b2e80b 2026-11-10 docs/providers/muse.md (context 1007997, max output 128000 for every Muse model); not a live probe")
	}
	source := "models.dev google-vertex 2026-11-12"
	vertex := func(window, advertised int64, prefix, ids string) {
		for id := range strings.FieldsSeq(ids) {
			add("vertex", "proxy", prefix+id, window, advertised, fmt.Sprintf("%s (context %d); not a live probe", source, window))
		}
	}
	gemini := func(ids string) {
		for id := range strings.FieldsSeq(ids) {
			vertex(1048576, 1000000, "", id)
			vertex(1048576, 1000000, "vertex/", id)
		}
	}
	gemini("gemini-4-argon gemini-4-carbon gemini-3.8-flash gemini-3.7-flash gemini-3.5-flash gemini-3.5-flash-lite gemini-3.1-flash-lite gemini-3.1-pro-preview gemini-3-flash-preview")
	vertex(1000000, 1000000, "vertex/anthropic/", "claude-fable-5-2 claude-fable-5-1 claude-fable-5 claude-opus-5-5 claude-opus-5 claude-opus-4-8 claude-opus-4-7 claude-sonnet-5-5 claude-sonnet-5 claude-haiku-5-5 claude-opus-4-6 claude-sonnet-4-6")
	vertex(524288, 524288, "vertex/xai/", "grok-4.7 grok-4.6")
	vertex(2000000, 1000000, "vertex/xai/", "grok-4.20-reasoning grok-4.20-non-reasoning")
	vertex(1000000, 1000000, "vertex/zai-org/", "glm-5.2-maas")
	vertex(262144, 262144, "vertex/google/", "gemma-4-26b-a4b-it-maas")
	vertex(524288, 524288, "vertex/meta/", "llama-4-maverick-17b-128e-instruct-maas")
	vertex(1310720, 1000000, "vertex/meta/", "llama-4-scout-17b-16e-instruct-maas")
	source = "models.dev google-vertex 2026-11-25"
	gemini("gemini-4-hydrogen gemini-4-neon")
	source = "claude-code-proxy dev c31326c 2026-11-12 docs/providers/vertex.md, quoting Google's input limit"
	vertex(922000, 922000, "vertex/openai/", "gpt-6-astra gpt-6.1-sol gpt-6.1-terra gpt-6-luna")
	vertex(1048576, 1000000, "vertex/meta/", "muse-spark-1.3 muse-wonder-1.0 muse-ember-1.0")
	return append(rows, policy.Model{Backend: "anthropic", BackendID: "anthropic", Measurement: "upstream-managed native Anthropic passthrough", PickerAlias: "claude-anthropic", Sentry: "none"})
}

func TestOrderedRows(t *testing.T) {
	want := expectedRows()
	got := policy.Rows()
	equal(t, want, got)
	got[0].BackendID = "modified"
	equal(t, want, policy.Rows())
}

func TestMeasuredRows(t *testing.T) {
	r := policy.Resolver{}
	for _, row := range expectedRows() {
		if row.Backend == "anthropic" || row.BackendID == "default" {
			continue
		}
		t.Run(row.BackendID, func(t *testing.T) {
			want := row
			if want.Transport == "" {
				if want.Backend == "grok" {
					want.Transport = "grok"
				} else {
					want.Transport = "proxy"
				}
			}
			want.PickerAlias = strings.Replace(want.PickerAlias, "/claude-fable-", "/fable-", 1)
			resolved := modelFor(t, r, want.PickerAlias)
			equal(t, want, resolved)
			equal(t, want.PickerAlias, r.ClientID(row.BackendID))
			advertised := row.AdvertisedWindow
			if row.Backend == "codex" {
				advertised = 272000
			}
			equal(t, advertised, r.AdvertisedWindow(row.BackendID))
			if row.Backend == "vertex" {
				got, err := policy.EffectiveSentry(&resolved, 0, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				equal(t, &policy.Sentry{BackendWindow: row.BackendWindow, CompactTrigger: float64(row.BackendWindow - 40000), Source: "derived"}, got)
			}
		})
	}
}

func TestAliasReplacementText(t *testing.T) {
	r := policy.Resolver{}
	tests := map[string]struct{ id, alias string }{
		"success: matched template field":  {"gpt-$&", "claude-gpt-{backendId}[1m]"},
		"success: literal dollar":          {"gpt-$$", "claude-gpt-$[1m]"},
		"success: preceding template text": {"gpt-$`", "claude-gpt-claude-[1m]"},
		"success: following template text": {"gpt-$'", "claude-gpt-[1m][1m]"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := modelFor(t, r, tt.id)
			equal(t, tt.id, p.BackendID)
			equal(t, tt.alias, p.PickerAlias)
			equal(t, tt.alias, r.ClientID(tt.id))
		})
	}
}

func TestCodexTierAndDefaults(t *testing.T) {
	r := policy.Resolver{}
	tests := map[string]struct {
		id, measurement, template string
		window                    int64
	}{
		"success: unmeasured Codex": {"gpt-5.2", defaultNote, aliasTemplate, 920000},
		"success: Astra fast":       {"gpt-6-astra-fast", measuredEarly, "", 920012},
		"success: Astra ultrafast":  {"gpt-6-astra-ultrafast", measuredEarly, "", 920012},
		"success: Sol fast":         {"gpt-6.1-sol-fast", measuredLate, "", 920012},
		"success: Sol ultrafast":    {"gpt-6.1-sol-ultrafast", measuredLate, "", 920012},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			alias := "claude-" + tt.id + "[1m]"
			want := policy.Model{Backend: "codex", Transport: "proxy", BackendID: tt.id, Measurement: tt.measurement, BackendWindow: tt.window, AdvertisedWindow: 920000, PickerAlias: alias, PickerAliasTemplate: tt.template, Sentry: "synthetic-413"}
			equal(t, want, modelFor(t, r, alias))
			equal(t, want, modelFor(t, r, tt.id))
			equal(t, alias, r.ClientID(tt.id))
			equal(t, int64(272000), r.AdvertisedWindow(tt.id))
		})
	}
}
