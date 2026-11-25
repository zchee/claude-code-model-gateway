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

package pins_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/pins"
)

const (
	shipped  = "claude-fable-5-1[1m]"
	detected = "claude-fable-5-2[1m]"
	catalog  = "claude-fable-5-3[1m]"
	newest   = "claude-fable-6[1m]"
)

func TestEffective(t *testing.T) {
	tests := map[string]struct {
		cli, forVersion, detection, saved, override string
		want                                        pins.Pin
	}{
		"success: no cache": {
			"",
			"",
			"",
			"",
			"",
			pins.Pin{
				Default: shipped,
				Shipped: shipped,
				Newest:  newest,
				Value:   shipped,
				Source:  "shipped",
			},
		},
		"success: stale detection": {
			"v2",
			"v1",
			detected,
			"",
			"",
			pins.Pin{
				Default: shipped,
				Shipped: shipped,
				Newest:  newest,
				Value:   shipped,
				Source:  "shipped",
			},
		},
		"success: missing CLI version": {
			"",
			"",
			detected,
			"",
			"",
			pins.Pin{
				Default: shipped,
				Shipped: shipped,
				Newest:  newest,
				Value:   shipped,
				Source:  "shipped",
			},
		},
		"success: lagging detection visible but not selected": {
			"v",
			"v",
			"claude-fable-5[1m]",
			"",
			"",
			pins.Pin{
				Default:  shipped,
				Shipped:  shipped,
				Detected: "claude-fable-5[1m]",
				Newest:   newest,
				Value:    shipped,
				Source:   "shipped",
			},
		},
		"success: incomparable detection wins": {
			"v",
			"v",
			"claude-fable-preview[1m]",
			catalog,
			"",
			pins.Pin{
				Default:  "claude-fable-preview[1m]",
				Shipped:  shipped,
				Detected: "claude-fable-preview[1m]",
				Newest:   newest,
				Value:    "claude-fable-preview[1m]",
				Source:   "detected",
			},
		},
		"success: saved catalog older": {
			"v",
			"v",
			catalog,
			detected,
			"",
			pins.Pin{
				Default:  catalog,
				Shipped:  shipped,
				Detected: catalog,
				Newest:   newest,
				Value:    catalog,
				Source:   "detected",
			},
		},
		"success: saved catalog newer": {
			"v",
			"v",
			detected,
			catalog,
			"",
			pins.Pin{
				Default:  catalog,
				Shipped:  shipped,
				Detected: detected,
				Newest:   newest,
				Value:    catalog,
				Source:   "catalog",
			},
		},
		"success: identical text credits catalog": {
			"v",
			"v",
			detected,
			detected,
			"",
			pins.Pin{
				Default:  detected,
				Shipped:  shipped,
				Detected: detected,
				Newest:   newest,
				Value:    detected,
				Source:   "catalog",
			},
		},
		"success: equivalent versions retain detected spelling": {
			"v",
			"v",
			"claude-fable-6-0[1m]",
			newest,
			"",
			pins.Pin{
				Default:  "claude-fable-6-0[1m]",
				Shipped:  shipped,
				Detected: "claude-fable-6-0[1m]",
				Newest:   newest,
				Value:    "claude-fable-6-0[1m]",
				Source:   "detected",
			},
		},
		"success: explicit overrides all sources": {
			"v",
			"v",
			detected,
			catalog,
			"custom",
			pins.Pin{
				Default:  catalog,
				Shipped:  shipped,
				Detected: detected,
				Override: "custom",
				Newest:   newest,
				Value:    "custom",
				Source:   "override",
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := pins.Inputs{
				Cache: pins.Cache{
					CLIVersion:  tt.cli,
					Detected:    map[string]string{"fable": tt.detection},
					DetectedFor: map[string]string{"fable": tt.forVersion},
					CatalogPins: map[string]string{"fable": tt.saved},
				},
				Overrides: map[string]string{"fable": tt.override},
				Catalog:   []pins.Candidate{{ID: "claude-fable-6"}},
			}
			equal(t, tt.want, in.Effective()["fable"])
		})
	}
}

func TestFollowCatalog(t *testing.T) {
	const prior = "claude-opus-6[1m]"
	history := []string{"claude-opus-8[1m]", prior}
	for i := range 14 {
		history = append(history, fmt.Sprintf("claude-opus-4-%d[1m]", i))
	}
	after := append([]string{prior, history[0]}, history[2:]...)
	full := append(slices.Clone(history[2:]), "claude-opus-3-9[1m]", "claude-opus-3-8[1m]")
	tests := map[string]struct {
		saved                 string
		rows                  []pins.Candidate
		history, wantHistory  []string
		wantNewest, wantSaved string
		write                 bool
	}{
		"success: filter candidates keep first tie retire previous": {
			prior,
			[]pins.Candidate{
				{ID: "claude-opus-7"},
				{ID: "claude-opus-7-0"},
				{ID: "claude-opus-10", Lifecycle: "deprecated"},
				{ID: "claude-opus-20", Lifecycle: "retired"},
				{ID: "claude-opus-8"},
				{ID: "claude-opus-6-9"},
				{ID: "claude-opus-5"},
				{ID: "claude-opus-9-1-20251101"},
				{ID: "claude-haiku-99"},
			},
			history,
			after,
			"claude-opus-7[1m]",
			"claude-opus-7[1m]",
			true,
		},
		"success: bound retirement history": {
			prior,
			[]pins.Candidate{{ID: "claude-opus-7"}},
			full,
			append([]string{prior}, full[:15]...),
			"claude-opus-7[1m]",
			"claude-opus-7[1m]",
			true,
		},
		"success: equal default reporting only": {
			"",
			[]pins.Candidate{{ID: "claude-opus-5-5"}},
			nil,
			nil,
			"claude-opus-5-5[1m]",
			"",
			false,
		},
		"success: no acceptable candidates": {
			"",
			[]pins.Candidate{
				{ID: "claude-opus-4-9"},
				{ID: "claude-opus-5"},
				{ID: "claude-opus-preview"},
				{ID: "claude-opus-7", Lifecycle: "deprecated"},
			},
			nil,
			nil,
			"",
			"",
			false,
		},
		"success: persist even without advancement": {
			prior,
			[]pins.Candidate{{ID: "claude-opus-5-6"}},
			nil,
			nil,
			"claude-opus-5-6[1m]",
			prior,
			true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := pins.Inputs{
				Cache: pins.Cache{
					CatalogPins: map[string]string{},
					Retired:     map[string][]string{"opus": slices.Clone(tt.history)},
				},
				Catalog:   tt.rows,
				Overrides: map[string]string{"opus": "custom"},
			}
			if tt.saved != "" {
				in.Cache.CatalogPins["opus"] = tt.saved
			}
			effective := in.Effective()
			equal(t, tt.wantNewest, effective["opus"].Newest)
			_, hasHaiku := effective["haiku"]
			equal(t, false, hasHaiku)
			got, write := in.FollowCatalog()
			equal(t, tt.write, write)
			_, savedHaiku := got.CatalogPins["haiku"]
			equal(t, false, savedHaiku)
			equal(t, tt.wantSaved, got.CatalogPins["opus"])
			if len(tt.wantHistory) == 0 {
				equal(t, 0, len(got.Retired["opus"]))
			} else {
				equal(t, tt.wantHistory, got.Retired["opus"])
			}
		})
	}
}

const (
	opusKey     = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	sonnetKey   = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	fableKey    = "ANTHROPIC_DEFAULT_FABLE_MODEL"
	option      = "ANTHROPIC_CUSTOM_MODEL_OPTION"
	optionName  = "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"
	description = "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION"
)

func TestFableOption(t *testing.T) {
	in := pins.Inputs{Overrides: map[string]string{"fable": catalog}}
	tests := map[string]struct {
		key, value string
		owned      bool
	}{
		"success: picker model": {option, catalog, false},
		"success: picker name":  {optionName, "Fable", false},
		"success: picker description": {
			description,
			"Fable 5.3 · current Fable, through the gateway",
			false,
		},
		"success: owns override description": {
			description,
			"Fable 5.3 · current Fable, through the gateway",
			true,
		},
		"success: owns shipped model": {option, shipped, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.owned {
				equal(t, true, in.Owned()[tt.key][tt.value])
				return
			}
			for _, e := range in.PinEnv() {
				if e.Key == tt.key {
					equal(t, tt.value, e.Value)
					return
				}
			}
			t.Fatalf("missing %s", tt.key)
		})
	}
}

func TestEnvBlock(t *testing.T) {
	tests := map[string]struct{ mode, url string }{
		"success: default":      {"default", "http://127.0.0.1:31234"},
		"success: compat":       {"compat", "http://api.anthropic.com"},
		"success: unknown mode": {"other", "http://127.0.0.1:31234"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := pins.Inputs{}
			pinEntries := []pins.Entry{
				{Key: opusKey, Value: "claude-opus-5-5[1m]"},
				{Key: sonnetKey, Value: "claude-sonnet-5-5[1m]"},
				{Key: fableKey, Value: shipped},
				{Key: option, Value: shipped},
				{Key: optionName, Value: "Fable"},
				{Key: description, Value: "Fable 5.1 · current Fable, through the gateway"},
			}
			equal(t, pinEntries, in.PinEnv())
			prefix := []pins.Entry{
				{Key: "ANTHROPIC_BASE_URL", Value: tt.url},
				{Key: "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", Value: "1"},
				{Key: "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK", Value: "1"},
				{Key: "ENABLE_TOOL_SEARCH", Value: "true"},
				{Key: "CLAUDE_CODE_MAX_OUTPUT_TOKENS", Value: "64000"},
			}
			equal(t, append(prefix, pinEntries...), in.EnvBlock(tt.mode, "http://127.0.0.1:31234"))
		})
	}
}

func TestOwnership(t *testing.T) {
	in := pins.Inputs{
		Cache: pins.Cache{
			CLIVersion:  "new",
			Detected:    map[string]string{"fable": detected},
			DetectedFor: map[string]string{"fable": "old"},
			CatalogPins: map[string]string{"fable": catalog},
			Retired:     map[string][]string{"fable": {"claude-fable-4[1m]"}},
		},
		Overrides: map[string]string{"fable": "custom"},
	}
	fables := map[string]bool{
		shipped:              true,
		detected:             true,
		catalog:              true,
		"claude-fable-4[1m]": true,
		"custom":             true,
	}
	want := map[string]map[string]bool{
		opusKey: {
			"claude-opus-5-5[1m]": true,
			"claude-opus-5[1m]":   true,
			"claude-opus-4-8[1m]": true,
		},
		sonnetKey:  {"claude-sonnet-5-5[1m]": true, "claude-sonnet-5[1m]": true},
		fableKey:   fables,
		option:     fables,
		optionName: {"Fable": true},
		description: {
			"Fable 5.1 · current Fable, through the gateway": true,
			"Fable 5.2 · current Fable, through the gateway": true,
			"Fable 5.3 · current Fable, through the gateway": true,
			"Fable 4 · current Fable, through the gateway":   true,
			"custom · current Fable, through the gateway":    true,
		},
	}
	equal(t, want, in.Owned())
}

func TestStale(t *testing.T) {
	in := pins.Inputs{Overrides: map[string]string{"fable": catalog}}
	tests := map[string]struct {
		env  map[string]any
		want []pins.Update
	}{"success: change owned values only": {map[string]any{description: "Fable 5.1 · current Fable, through the gateway", optionName: "mine", option: 32, fableKey: shipped, sonnetKey: "claude-sonnet-5-5[1m]", opusKey: "claude-opus-4-8[1m]", "OTHER": "old"}, []pins.Update{{Key: opusKey, From: "claude-opus-4-8[1m]", To: "claude-opus-5-5[1m]"}, {Key: fableKey, From: shipped, To: catalog}, {Key: description, From: "Fable 5.1 · current Fable, through the gateway", To: "Fable 5.3 · current Fable, through the gateway"}}}, "success: user set values survive": {map[string]any{option: "mine", sonnetKey: shipped}, nil}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := in.Stale(tt.env)
			if len(tt.want) == 0 {
				equal(t, 0, len(got))
				return
			}
			equal(t, tt.want, got)
		})
	}
}
