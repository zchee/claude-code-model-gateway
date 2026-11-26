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

package grok_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
)

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func isASCIIUnsignedDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func versionMeetsGrokMinimum(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return false
	}
	var nums [3]int
	for i := range 3 {
		if !isASCIIUnsignedDigits(parts[i]) {
			return false
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return false
		}
		nums[i] = n
	}
	return nums[0] > 1 || (nums[0] == 1 && (nums[1] > 0 || (nums[1] == 0 && nums[2] >= 13)))
}

func TestVersionAndHeaders(t *testing.T) {
	tests := map[string]struct {
		versionFile string
		token       string
		model       string
		wantHeaders http.Header
	}{
		"success: reads CLI version and builds headers": {
			versionFile: `{"version":"0.2.112"}`,
			token:       "fixture-token",
			model:       "grok-4.5",
			wantHeaders: http.Header{
				"Authorization":            {"Bearer fixture-token"},
				"Content-Type":             {"application/json"},
				"User-Agent":               {"xai-grok-build/0.2.112"},
				"X-Grok-Client-Version":    {"0.2.112"},
				"X-Grok-Client-Identifier": {"xai-grok-cli"},
				"X-Grok-Model-Override":    {"grok-4.5"},
			},
		},
		"success: omitted empty version uses VersionFallback": {
			token: "fixture-token",
			model: "grok-4.7",
			wantHeaders: http.Header{
				"Authorization":            {"Bearer fixture-token"},
				"Content-Type":             {"application/json"},
				"User-Agent":               {"xai-grok-build/1.0.60"},
				"X-Grok-Client-Version":    {"1.0.60"},
				"X-Grok-Client-Identifier": {"xai-grok-cli"},
				"X-Grok-Model-Override":    {"grok-4.7"},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var version string
			if tt.versionFile != "" {
				file := filepath.Join(t.TempDir(), "version.json")
				writeFixtureFile(t, file, tt.versionFile)
				version = grok.ReadVersion(file)
			}
			got := grok.Headers(tt.token, tt.model, version)
			if diff := gocmp.Diff(tt.wantHeaders, got); diff != "" {
				t.Errorf("Headers() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReadVersion(t *testing.T) {
	const wantFallback = "1.0.60"
	tests := map[string]struct {
		contents *string
		want     string
	}{
		"success: missing version.json uses the fallback": {
			contents: nil,
			want:     wantFallback,
		},
		"success: unparseable version.json uses the fallback": {
			contents: new("{"),
			want:     wantFallback,
		},
		"success: the Grok CLI version wins over the fallback": {
			contents: new(`{"version":"1.0.61"}`),
			want:     "1.0.61",
		},
		"success: client_version is used when version is absent": {
			contents: new(`{"client_version":"1.2.3-rc1"}`),
			want:     "1.2.3-rc1",
		},
		"success: non-semver version falls back": {
			contents: new(`{"version":"dev"}`),
			want:     wantFallback,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "version.json")
			if tt.contents != nil {
				writeFixtureFile(t, file, *tt.contents)
			}
			got := grok.ReadVersion(file)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ReadVersion() (-want +got):\n%s", diff)
			}
			if tt.want == wantFallback {
				if diff := gocmp.Diff(grok.VersionFallback, got); diff != "" {
					t.Errorf("ReadVersion() vs VersionFallback (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestVersionFallbackMinimum(t *testing.T) {
	// Verify the minimum-version predicate itself rejects negative and below-minimum versions.
	predicateCases := map[string]struct {
		version string
		want    bool
	}{
		"success: exact minimum 1.0.13":      {version: "1.0.13", want: true},
		"success: current fallback 1.0.60":   {version: "1.0.60", want: true},
		"success: higher minor 1.1.0":        {version: "1.1.0", want: true},
		"success: higher major 2.0.0":        {version: "2.0.0", want: true},
		"error: below minimum 1.0.12":        {version: "1.0.12", want: false},
		"error: negative minor 1.-1.0":       {version: "1.-1.0", want: false},
		"error: negative patch 1.0.-1":       {version: "1.0.-1", want: false},
		"error: major zero 0.9.99":           {version: "0.9.99", want: false},
		"error: empty version":               {version: "", want: false},
		"error: missing patch component 1.0": {version: "1.0", want: false},
	}
	for name, tc := range predicateCases {
		t.Run(name, func(t *testing.T) {
			if got := versionMeetsGrokMinimum(tc.version); got != tc.want {
				t.Errorf("versionMeetsGrokMinimum(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}

	// The backend answers a client version older than 1.0.13 with HTTP 426.
	if !versionMeetsGrokMinimum(grok.VersionFallback) {
		t.Errorf("VersionFallback = %q, must be a valid semver at least 1.0.13", grok.VersionFallback)
	}
}

func TestCatalogCache(t *testing.T) {
	wantDefaults := []grok.Model{
		{ID: "grok-4.7", Context: 500000, Reasoning: true},
		{ID: "grok-4.6", Context: 500000, Reasoning: true},
		{ID: "grok-4.5", Context: 500000, Reasoning: true},
	}

	tests := map[string]struct {
		contents      *string
		wantModels    []grok.Model
		wantIDs       []string
		checkDefaults bool
	}{
		"success: reads current CLI cache": {
			contents: new(`{
				"models": {
					"grok-4.7": {"info": {"id": "grok-4.7", "context_window": 500000, "supports_reasoning_effort": true}},
					"grok-4.6": {"info": {"id": "grok-4.6", "context_window": 500000, "supports_reasoning_effort": true}},
					"grok-4.5": {"info": {"id": "grok-4.5", "context_window": 500000, "supports_reasoning_effort": true}}
				}
			}`),
			wantModels:    wantDefaults,
			wantIDs:       []string{"grok-4.7", "grok-4.6", "grok-4.5"},
			checkDefaults: true,
		},
		"success: accepts top-level array and string entries with fallback metadata": {
			contents: new(`["grok-4.7", {"model": "grok-4.9", "context_window": 256000, "supports_reasoning_effort": false}, "grok-custom"]`),
			wantModels: []grok.Model{
				{ID: "grok-4.7", Context: 500000, Reasoning: true},
				{ID: "grok-4.9", Context: 256000, Reasoning: false},
				{ID: "grok-custom", Context: 131072, Reasoning: false},
			},
			wantIDs: []string{"grok-4.7", "grok-4.9", "grok-custom"},
		},
		"success: accepts models array shape": {
			contents: new(`{"models": [{"id": "grok-4.8", "context_window": 300000, "supports_reasoning_effort": true}, {"id": "grok-4.8", "context_window": 999}, {"id": "not-grok"}]}`),
			wantModels: []grok.Model{
				{ID: "grok-4.8", Context: 300000, Reasoning: true},
			},
			wantIDs: []string{"grok-4.8"},
		},
		"success: accepts data array shape": {
			contents: new(`{"data": [{"id": "grok-4.6", "context_window": "bad"}, {"id": "not-grok"}, {"id": "grok-4.6"}, {"id": "grok-4.5", "supports_reasoning_effort": false}]}`),
			wantModels: []grok.Model{
				{ID: "grok-4.6", Context: 500000, Reasoning: true},
				{ID: "grok-4.5", Context: 500000, Reasoning: false},
			},
			wantIDs: []string{"grok-4.6", "grok-4.5"},
		},
		"success: preserves positive finite floating-point context and rejects non-positive values": {
			contents: new(`{
				"models": [
					{"id": "grok-frac", "context_window": 123.5, "supports_reasoning_effort": false},
					{"id": "grok-zero", "context_window": 0},
					{"id": "grok-neg", "context_window": -42.5},
					{"id": "grok-large", "context_window": 1e16}
				]
			}`),
			wantModels: []grok.Model{
				{ID: "grok-frac", Context: 123.5, Reasoning: false},
				{ID: "grok-zero", Context: 131072, Reasoning: false},
				{ID: "grok-neg", Context: 131072, Reasoning: false},
				{ID: "grok-large", Context: 1e16, Reasoning: false},
			},
			wantIDs: []string{"grok-frac", "grok-zero", "grok-neg", "grok-large"},
		},
		"error: missing cache file returns empty": {
			contents: nil,
		},
		"error: malformed cache file returns empty": {
			contents: new(`{invalid`),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "models_cache.json")
			if tt.contents != nil {
				writeFixtureFile(t, file, *tt.contents)
			}
			gotModels := grok.ModelsFromCache(file)
			gotIDs := grok.ModelIDsFromCache(file)

			if len(tt.wantModels) == 0 {
				if len(gotModels) != 0 {
					t.Errorf("ModelsFromCache() = %#v, want empty", gotModels)
				}
				if len(gotIDs) != 0 {
					t.Errorf("ModelIDsFromCache() = %#v, want empty", gotIDs)
				}
			} else {
				if diff := gocmp.Diff(tt.wantModels, gotModels); diff != "" {
					t.Errorf("ModelsFromCache() (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.wantIDs, gotIDs); diff != "" {
					t.Errorf("ModelIDsFromCache() (-want +got):\n%s", diff)
				}
			}

			if tt.checkDefaults {
				defaults := grok.DefaultModels()
				if diff := gocmp.Diff(wantDefaults, defaults); diff != "" {
					t.Errorf("DefaultModels() (-want +got):\n%s", diff)
				}
				if len(defaults) > 0 {
					defaults[0].ID = "mutated"
					if diff := gocmp.Diff(wantDefaults, grok.DefaultModels()); diff != "" {
						t.Errorf("DefaultModels() after caller mutation (-want +got):\n%s", diff)
					}
				}
			}
		})
	}
}
