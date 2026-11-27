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

package ownership

import (
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestInstallRootComparison decides the production install-root comparison the way the source calls it
// (test/gateway-process-isolation.test.js:581): the older sibling's directory with a final separator against the
// clean current directory is a sibling of this plugin, exactly as its clean spelling is.
func TestInstallRootComparison(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(home, ".claude", "plugins", "cache")
	older := filepath.Join(cache, "eigenwise-toolshed", "model-gateway", "0.48.0")
	current := filepath.Join(cache, "eigenwise-toolshed", "model-gateway", "0.50.0")
	otherMarket := filepath.Join(cache, "other-marketplace", "model-gateway", "0.49.0")
	for _, dir := range []string{older, current, otherMarket} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sep := string(filepath.Separator)
	tests := map[string]struct {
		root string
		want bool
	}{
		"success: the older sibling with a final separator belongs to the clean current install": {root: older + sep, want: true},
		"success: the older sibling spelled cleanly belongs to the current install":              {root: older, want: true},
		"success: the current install with a final separator is the current install":             {root: current + sep, want: true},
		"error: another marketplace's install with a final separator does not belong":            {root: otherMarket + sep, want: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := belongsToThisPlugin(tt.root, current, nil)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("belongsToThisPlugin(%q, %q) (-want +got):\n%s", tt.root, current, diff)
			}
		})
	}
}
