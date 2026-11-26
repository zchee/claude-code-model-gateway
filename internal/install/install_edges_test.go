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
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/install"
)

func TestCacheIdentityThroughAlias(t *testing.T) {
	tests := map[string]struct {
		version string
		exists  bool
	}{
		"success: existing sibling": {version: "1.2.3", exists: true},
		"success: missing sibling":  {version: "1.2.4"},
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(root, "configured")
	alias := filepath.Join(root, "alias")
	relative := filepath.Join("cache", "market", "model-gateway")
	pluginRoot := filepath.Join(configured, relative)
	if err := os.MkdirAll(pluginRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(configured, alias); err != nil {
		t.Fatal(err)
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.exists {
				if err := os.Mkdir(filepath.Join(pluginRoot, tt.version), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			want := strings.ToLower(filepath.ToSlash(pluginRoot))
			if diff := gocmp.Diff(want, install.CacheIdentity(filepath.Join(alias, relative, tt.version), []string{configured})); diff != "" {
				t.Errorf("CacheIdentity (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCacheIdentityCaseHandling(t *testing.T) {
	tests := map[string]struct {
		parent, market, wantSuffix string
	}{
		"success: ASCII plugin directory name":         {parent: "PLUGINS", market: "MARKET", wantSuffix: "plugins/cache/market/model-gateway"},
		"error: non-ASCII fold of plugins":             {parent: "pluginſ", market: "market"},
		"success: dotted I expands and sigma is final": {parent: "plugins", market: "İΟΣ", wantSuffix: "plugins/cache/i̇ος/model-gateway"},
		"success: sigma before a letter is not final":  {parent: "plugins", market: "ΟΣΑ", wantSuffix: "plugins/cache/οσα/model-gateway"},
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			version := filepath.Join(root, tt.parent, "CACHE", tt.market, "MODEL-GATEWAY", "1.2.3")
			want := ""
			if tt.wantSuffix != "" {
				want = strings.ToLower(filepath.ToSlash(root)) + "/" + tt.wantSuffix
			}
			if diff := gocmp.Diff(want, install.CacheIdentity(version, nil)); diff != "" {
				t.Errorf("CacheIdentity (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReadEntriesMalformedNamesAndMembers(t *testing.T) {
	tests := map[string]struct {
		first, second, pluginID string
		readError               bool
		want                    []map[string]any
	}{
		"success: drop unpaired names, retain literal replacement": {
			first: `{"plugins":{
				"model-gateway@\ud800":[{"scope":"not kept"}],
				"model-gateway@�":[{"�":"kept","\ud800":"not kept","\udc01":"not kept","extra":{"�":"kept","\ud800":"not kept"}}],
				"model-gateway@\udc01":[{"scope":"not kept"}]}}`,
			pluginID: "model-gateway",
			want:     []map[string]any{{"�": "kept", "extra": map[string]any{"�": "kept"}}},
		},
		"success: exact plugin ID does not match lone surrogate": {
			first:    `{"plugins":{"model-gateway@\ud800":[{"scope":"not kept"}]}}`,
			pluginID: "model-gateway@�",
			want:     []map[string]any{},
		},
		"success: drop invalid install path and keep literal replacement": {
			first: `{"plugins":{"model-gateway@market":[
				{"version":"9.0.0","installPath":"/cache/\ud800"},
				{"version":"1.0.0","installPath":"/cache/�"}]}}`,
			pluginID: "model-gateway",
			want:     []map[string]any{{"version": "1.0.0", "installPath": "/cache/�"}},
		},
		"success: replaced raw bytes match a literal replacement plugin ID": {
			first: `{"plugins":{"model-gateway@` + "\xff" + `":[{"scope":"not kept"}],
				"model-gateway@�":[{"scope":"kept"}]}}`,
			pluginID: "model-gateway@�",
			want:     []map[string]any{{"scope": "kept"}},
		},
		"success: last duplicate wins without moving a plugin": {
			first: `{"plugins":false,"plugins":{
				"model-gateway@z":false,
				"model-gateway@a":[{"scope":"second"}],
				"model-gateway@z":[{"scope":"ignored","scope":"first"}]}}`,
			pluginID: "model-gateway",
			want:     []map[string]any{{"scope": "first"}, {"scope": "second"}},
		},
		"error: directory at first registry path": {
			second:    `{"plugins":{"model-gateway@market":[{"scope":"kept"}]}}`,
			pluginID:  "model-gateway",
			readError: true,
			want:      []map[string]any{{"scope": "kept"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			first := filepath.Join(root, "first.json")
			second := filepath.Join(root, "second.json")
			if tt.readError {
				if err := os.Mkdir(first, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			writeRegistry(t, first, tt.first)
			writeRegistry(t, second, tt.second)
			for _, entry := range tt.want {
				entry["registry"] = first
				if tt.readError {
					entry["registry"] = second
				}
			}
			if diff := gocmp.Diff(tt.want, install.ReadEntries([]string{first, second}, tt.pluginID)); diff != "" {
				t.Errorf("ReadEntries (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStableOrderingOfRegistryEntries(t *testing.T) {
	overflow := strings.Repeat("9", 310)
	nonCallableToString := map[string]any{"toString": true}
	tests := map[string]struct {
		entries []map[string]any
		want    []string
	}{
		"success: equal overflowing parts preserve registry order": {
			entries: []map[string]any{registryEntry(overflow+".1", "b"), registryEntry(overflow+".2", "a"), registryEntry(overflow+".3", "c")},
			want:    []string{"b", "a", "c"},
		},
		"success: non-callable toString keeps comparator deterministic": {
			entries: []map[string]any{{"version": nonCallableToString, "lastUpdated": "b"}, registryEntry("1", "a"), registryEntry("", "c")},
			want:    []string{"a", "c", "b"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			slices.SortStableFunc(tt.entries, install.CompareEntries)
			var got []string
			for _, entry := range tt.entries {
				got = append(got, entry["lastUpdated"].(string))
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("stable registry order (-want +got):\n%s", diff)
			}
		})
	}
}
