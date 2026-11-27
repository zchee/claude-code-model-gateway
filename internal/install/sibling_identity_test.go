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

package install_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/install"
)

// TestCacheIdentityOfLinkedVersion decides that a version directory in this plugin's cache is a sibling by its
// physical location, not by its name: a link that points outside the plugin's cache is not a sibling, while a
// real version directory beside the current one is.
func TestCacheIdentityOfLinkedVersion(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(home, ".claude", "plugins", "cache", "eigenwise-toolshed", "model-gateway")
	current := filepath.Join(plugin, "0.50.0")
	older := filepath.Join(plugin, "0.48.0")
	foreignRoot := filepath.Join(home, "dev", "foreign-model-gateway")
	otherMarketplace := filepath.Join(home, ".claude", "plugins", "cache", "other-marketplace", "model-gateway", "0.49.0")
	for _, dir := range []string{current, older, foreignRoot, otherMarketplace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, target, version string) string {
		t.Helper()
		path := filepath.Join(plugin, version)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	want := install.CacheIdentity(current, nil)
	if want == "" {
		t.Fatalf("the current install %s has no cache identity", current)
	}
	tests := map[string]struct {
		path    func(t *testing.T) string
		sibling bool
	}{
		"success: a real version directory beside the current one is a sibling": {
			path:    func(*testing.T) string { return older },
			sibling: true,
		},
		"error: a version name linked to a directory outside the cache is not a sibling": {
			path: func(t *testing.T) string { return link(t, foreignRoot, "0.49.0") },
		},
		"error: a version name linked to another marketplace's install is not a sibling": {
			path: func(t *testing.T) string { return link(t, otherMarketplace, "0.49.1") },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := tt.path(t)
			got := install.CacheIdentity(path, nil)
			if sibling := got != "" && got == want; sibling != tt.sibling {
				t.Errorf("CacheIdentity(%s) = %q, current %q: sibling %v; want %v", path, got, want, sibling, tt.sibling)
			}
		})
	}
}
