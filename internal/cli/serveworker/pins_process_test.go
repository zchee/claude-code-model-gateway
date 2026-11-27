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

//go:build process && (darwin || linux)

package serveworker_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestCommandEffectivePinsHonourCatalogEligibility decides which saved catalog pin can become the
// effective pin of an alias. The source drops a saved catalog pin that does not outrank the shipped
// pin, or that is a retired shipped pin, when it reads the detection cache, and only then compares
// it with the detected default (lib/pins.js:35-51,119-126,155-159).
//
// The effective opus pin is observed through the previous-row labels of /v1/models: a server row is
// labelled previous when it is older than the effective pin of its family. With a detected
// claude-opus-NaN-6 (a major that is not a number, so versions compare on the minor), the two server
// rows below are labelled differently for each candidate pin:
//
//	effective pin          claude-opus-4-6   claude-opus-5-1
//	claude-opus-NaN-6      current           previous
//	claude-opus-4-7        previous          current
//	claude-opus-4-8        previous          current
//	claude-opus-6-7        previous          previous
//	claude-opus-5-5        previous          previous
func TestCommandEffectivePinsHonourCatalogEligibility(t *testing.T) {
	tests := map[string]struct {
		cache        string
		wantPrevious []string
	}{
		"success: a catalog pin that does not outrank the shipped pin never replaces the detected pin": {
			cache:        `{"cliVersion":"2.0.0","pins":{"opus":"claude-opus-NaN-6"},"catalogPins":{"opus":"claude-opus-4-7"}}`,
			wantPrevious: []string{"Opus 5.1 (previous)"},
		},
		"success: a retired shipped pin saved as the catalog pin never replaces the detected pin": {
			cache:        `{"cliVersion":"2.0.0","pins":{"opus":"claude-opus-NaN-6"},"catalogPins":{"opus":"claude-opus-4-8"}}`,
			wantPrevious: []string{"Opus 5.1 (previous)"},
		},
		"success: a catalog pin that outranks the shipped pin and the detected pin is used": {
			cache:        `{"cliVersion":"2.0.0","pins":{"opus":"claude-opus-NaN-6"},"catalogPins":{"opus":"claude-opus-6-7"}}`,
			wantPrevious: []string{"Opus 4.6 (previous)", "Opus 5.1 (previous)"},
		},
		"success: a catalog pin below the shipped pin leaves the shipped pin when nothing was detected": {
			cache:        `{"cliVersion":"2.0.0","catalogPins":{"opus":"claude-opus-4-7"}}`,
			wantPrevious: []string{"Opus 4.6 (previous)", "Opus 5.1 (previous)"},
		},
		"success: the detected pin is used when no catalog pin was saved": {
			cache:        `{"cliVersion":"2.0.0","pins":{"opus":"claude-opus-NaN-6"}}`,
			wantPrevious: []string{"Opus 5.1 (previous)"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, state := sandbox(t)
			if err := os.WriteFile(filepath.Join(state, "detected-pins.json"), []byte(tt.cache), 0o600); err != nil {
				t.Fatal(err)
			}
			global := filepath.Join(env["CLAUDE_CONFIG_DIR"], ".claude.json")
			if err := os.WriteFile(global, []byte(`{"additionalModelOptionsCache":[{"value":"claude-opus-4-6","label":"Opus 4.6"},{"value":"claude-opus-5-1","label":"Opus 5.1"}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)
			doc := getJSON(t, "http://"+w.public+"/v1/models")
			rows, _ := doc["data"].([]any)
			var previous []string
			for _, row := range rows {
				m, _ := row.(map[string]any)
				if label, _ := m["display_name"].(string); strings.HasSuffix(label, " (previous)") {
					previous = append(previous, label)
				}
			}
			if diff := gocmp.Diff(tt.wantPrevious, previous); diff != "" {
				t.Errorf("previous server rows in /v1/models for detection cache %s (-want +got):\n%s", tt.cache, diff)
			}
		})
	}
}
