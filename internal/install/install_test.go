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
	"cmp"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/install"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestProxyVersionFloor(t *testing.T) {
	if diff := gocmp.Diff("0.1.14", install.MinProxyVersion); diff != "" {
		t.Errorf("minimum proxy version (-want +got):\n%s", diff)
	}
	tests := map[string]struct {
		text         string
		valid, older bool
	}{
		"success: single digit patch": {"0.1.9", true, true},
		"success: previous patch":     {"0.1.13", true, true},
		"success: at floor":           {"0.1.14", true, false},
		"success: later patch":        {"0.1.15", true, false},
		"success: prefixed minor":     {"v0.2.0", true, false},
		"error: no version":           {"not a version", false, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, valid := install.ParseVersion(tt.text)
			if valid != tt.valid {
				t.Fatalf("ParseVersion(%q) valid = %t, want %t", tt.text, valid, tt.valid)
			}
			if !tt.valid {
				return
			}
			floor, ok := install.ParseVersion(install.MinProxyVersion)
			if !ok {
				t.Fatalf("cannot parse configured floor %q", install.MinProxyVersion)
			}
			if diff := gocmp.Diff(tt.older, install.Older(got, floor)); diff != "" {
				t.Errorf("Older(%q, %q) (-want +got):\n%s", tt.text, install.MinProxyVersion, diff)
			}
		})
	}
}

func TestRegistryLocations(t *testing.T) {
	root := t.TempDir()
	conf := filepath.Join(root, "config")
	legacy := filepath.Join(conf, "plugins")
	cache := filepath.Join(root, "agent-plugins")
	tests := map[string]struct {
		cache string
		dirs  []string
	}{
		"success: cache first": {cache, []string{cache, legacy}},
		"success: legacy only": {"", []string{legacy}},
		"success: duplicate":   {legacy, []string{legacy}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"CLAUDE_CONFIG_DIR": conf}
			if tt.cache != "" {
				env["CLAUDE_CODE_PLUGIN_CACHE_DIR"] = tt.cache
			}
			r := paths.New(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, nil, nil)
			registries, err := r.PluginRegistryPaths()
			if err != nil {
				t.Fatal(err)
			}
			var files []string
			for _, dir := range tt.dirs {
				files = append(files, filepath.Join(dir, "installed_plugins.json"))
			}
			if diff := gocmp.Diff(files, registries); diff != "" {
				t.Errorf("registry files (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.dirs, install.RegistryDirectories(registries)); diff != "" {
				t.Errorf("registry directories (-want +got):\n%s", diff)
			}
		})
	}
}

func writeRegistry(t *testing.T, path, data string) {
	t.Helper()
	if data == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadEntries(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "agent-plugins", "installed_plugins.json")
	legacy := filepath.Join(root, "config", "plugins", "installed_plugins.json")
	const pluginID = "model-gateway@eigenwise-toolshed"
	const current = `{"plugins":{"model-gateway@eigenwise-toolshed":[{"version":"0.52.0","scope":"user","installPath":"/installs/0.52.0"}]}}`
	const old = `{"plugins":{"model-gateway@eigenwise-toolshed":[{"version":"0.40.0","scope":"project","installPath":"/installs/0.40.0"}]}}`
	const survivor = `{"plugins":{"model-gateway@eigenwise-toolshed":[{"version":"0.40.0","scope":"user","installPath":"/installs/0.40.0"}]}}`
	tests := map[string]struct {
		first, second, id string
		want              []map[string]any
	}{
		"success: both registries":        {current, old, pluginID, []map[string]any{{"version": "0.52.0", "scope": "user", "installPath": "/installs/0.52.0", "registry": cache}, {"version": "0.40.0", "scope": "project", "installPath": "/installs/0.40.0", "registry": legacy}}},
		"error: malformed first registry": {"{ not json", survivor, pluginID, []map[string]any{{"version": "0.40.0", "scope": "user", "installPath": "/installs/0.40.0", "registry": legacy}}},
		"error: missing registries":       {"", "", pluginID, []map[string]any{}},
		"success: exact marketplace":      {`{"plugins":{"model-gateway@fork":[{"scope":"user"}],"model-gateway@eigenwise-toolshed":[null,false,{}, {"registry":"other","extra":"kept"}]}}`, "", pluginID, []map[string]any{{"registry": cache}, {"registry": cache, "extra": "kept"}}},
		"success: bare plugin name":       {`{"plugins":{"model-gateway@z":[{"scope":"project"}],"model-gateway@bad":{},"model-gateway@a":[{"scope":"user"}],"model-gateway-extra@z":[{}]}}`, "", "model-gateway", []map[string]any{{"scope": "project", "registry": cache}, {"scope": "user", "registry": cache}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for file, body := range map[string]string{cache: tt.first, legacy: tt.second} {
				writeRegistry(t, file, body)
				if body != "" {
					t.Cleanup(func() {
						if err := os.Remove(file); err != nil {
							t.Error(err)
						}
					})
				}
			}
			got := install.ReadEntries([]string{cache, legacy}, tt.id)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ReadEntries(%q) (-want +got):\n%s", tt.id, diff)
			}
		})
	}
}

func TestScope(t *testing.T) {
	tests := map[string]struct{ list, want string }{
		"error: no registry":       {"", "unknown"},
		"success: empty fork list": {"[]", "unknown"},
		"success: project in fork": {`[{"scope":"project","version":"0.1.0"}]`, "project-only"},
		"success: user in fork":    {`[{"scope":"project","version":"0.1.0"},{"scope":"user","version":"0.1.0"}]`, "user"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".claude", "plugins", "installed_plugins.json")
			if tt.list != "" {
				writeRegistry(t, path, `{"plugins":{"model-gateway@fork-toolshed":`+tt.list+`}}`)
			}
			if diff := gocmp.Diff(tt.want, install.Scope([]string{path})); diff != "" {
				t.Errorf("Scope (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCacheIdentity(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "agent-plugins")
	legacy := filepath.Join(root, "config", "plugins")
	tests := map[string]struct {
		parent string
		dirs   []string
		known  bool
	}{
		"success: configured cache":                  {cache, []string{cache, legacy}, true},
		"success: config plugins":                    {legacy, []string{legacy}, true},
		"success: legacy home after cache migration": {filepath.Join(root, "home", ".claude", "Plugins"), []string{cache}, true},
		"error: unrelated directory":                 {filepath.Join(root, "unrelated"), []string{cache, legacy}, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			plugin := filepath.Join(tt.parent, "cache", "eigenwise-toolshed", "model-gateway")
			installed := filepath.Join(plugin, "0.51.2")
			if err := os.MkdirAll(installed, 0o700); err != nil {
				t.Fatal(err)
			}
			id := install.CacheIdentity(installed, tt.dirs)
			if diff := gocmp.Diff(tt.known, id != ""); diff != "" {
				t.Errorf("known identity (-want +got):\n%s", diff)
			}
			if tt.known {
				other := filepath.Join(tt.parent, "cache", "another-marketplace", "model-gateway", "0.51.2")
				if err := os.MkdirAll(other, 0o700); err != nil {
					t.Fatal(err)
				}
				if otherID := install.CacheIdentity(other, tt.dirs); otherID == "" || otherID == id {
					t.Errorf("different marketplaces must have distinct nonempty identities, got %q and %q", id, otherID)
				}
				if diff := gocmp.Diff(id, install.CacheIdentity(filepath.Join(plugin, "0.52.0"), tt.dirs)); diff != "" {
					t.Errorf("sibling identity (-want +got):\n%s", diff)
				}
			}
			if diff := gocmp.Diff(tt.known, install.IsCacheParent(tt.parent, tt.dirs)); diff != "" {
				t.Errorf("cache parent (-want +got):\n%s", diff)
			}
		})
	}
}

func registryEntry(version string, updated any) map[string]any {
	return map[string]any{"version": version, "lastUpdated": updated}
}

func TestRegistryRootCollation(t *testing.T) {
	// Swedish sorts ä after z; registry updates use root order regardless of the host locale.
	tests := map[string]struct {
		left, right string
		want        int
	}{
		"success: root order rather than Swedish":     {"ä", "z", 1},
		"success: punctuation rather than byte order": {"a1b", "a_b", -1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := install.CompareEntries(registryEntry("1", tt.left), registryEntry("1", tt.right))
			if diff := gocmp.Diff(tt.want, cmp.Compare(got, 0)); diff != "" {
				t.Errorf("descending update order (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCompareEntries(t *testing.T) {
	missing := map[string]any{"version": "1"}
	tests := map[string]struct {
		left, right map[string]any
		want        int
	}{
		"success: numeric before timestamp":                         {registryEntry("1.10", "a"), registryEntry("1.9.9", "z"), -1},
		"success: later numeric part":                               {registryEntry("1-2-0", "z"), registryEntry("1.2.1", "a"), 1},
		"success: missing parts are zero":                           {registryEntry("1.2", "same"), registryEntry("1.2.0", "same"), 0},
		"success: extra trailing component":                         {registryEntry("1.2.0.4", "a"), registryEntry("1.2.0", "z"), -1},
		"success: punctuation before digits in ascending collation": {registryEntry("1.02", "a_b"), registryEntry("1.2.0", "a1b"), 1},
		"success: letters compare before case":                      {registryEntry("1", "a"), registryEntry("1", "B"), 1},
		"success: update text breaks numeric tie":                   {registryEntry("1.0", "2025-11"), registryEntry("1", "2025-09"), -1},
		"success: false and absent timestamps tie":                  {registryEntry("1", false), missing, 0},
		"success: zero and absent timestamps tie":                   {registryEntry("1", float64(0)), missing, 0},
		"success: null and absent timestamps tie":                   {registryEntry("1", nil), missing, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, cmp.Compare(install.CompareEntries(tt.left, tt.right), 0)); diff != "" {
				t.Errorf("comparison sign (-want +got):\n%s", diff)
			}
		})
	}
}
