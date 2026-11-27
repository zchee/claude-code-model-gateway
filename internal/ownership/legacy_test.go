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

package ownership_test

import (
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

type nodeReading struct {
	Root, Current string
	Ours          bool
}

// nodeInstalls lays out real install roots under a private home: this plugin's current and older versions in its
// cache, another marketplace's install, the same marketplace's install under a physically unrelated cache, a
// development checkout named model-gateway, the source's foreign-model-gateway checkout
// (test/gateway-process-isolation.test.js:369), a checkout whose name ends in model-gateway in mixed case, and a
// directory that is no gateway at all.
type nodeInstalls struct {
	home, current, older, otherMarket, unrelated, checkout, foreignNamed, mixedCase, tools string
}

func layoutInstalls(t *testing.T) nodeInstalls {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(home, ".claude", "plugins", "cache", "eigenwise-toolshed", "model-gateway")
	n := nodeInstalls{
		home:         home,
		current:      filepath.Join(plugin, "0.50.0"),
		older:        filepath.Join(plugin, "0.48.0"),
		otherMarket:  filepath.Join(home, ".claude", "plugins", "cache", "other-marketplace", "model-gateway", "0.49.0"),
		unrelated:    filepath.Join(home, "elsewhere", ".claude", "plugins", "cache", "eigenwise-toolshed", "model-gateway", "0.49.0"),
		checkout:     filepath.Join(home, "dev", "model-gateway"),
		foreignNamed: filepath.Join(home, "foreign-model-gateway"),
		mixedCase:    filepath.Join(home, "work", "Foreign-Model-Gateway"),
		tools:        filepath.Join(home, "tools"),
	}
	for _, dir := range []string{n.current, n.older, n.otherMarket, n.unrelated, n.checkout, n.foreignNamed, n.mixedCase, n.tools} {
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bin", "model-gateway.js"), []byte("'use strict';\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

func nodeCommand(script string) []string { return []string{"/usr/bin/node", script, "serve-shim"} }

func readNode(t *testing.T, predicate func([]string) (string, string, bool), argv []string) nodeReading {
	t.Helper()
	return noPanic(t, "the Node gateway predicate", func() nodeReading {
		root, cur, ours := predicate(argv)
		return nodeReading{root, cur, ours}
	})
}

func newNodeGateway(t *testing.T, current string, registryDirs []string) func([]string) (string, string, bool) {
	t.Helper()
	return noPanic(t, "NodeGateway", func() func([]string) (string, string, bool) { return ownership.NodeGateway(current, registryDirs) })
}

// TestNodeGatewayInstallRoot decides how the production predicate reads a Node gateway's command line, following the
// source's pattern (lib/process-supervision.js:510-514) and its install-root comparison (:530-542).
func TestNodeGatewayInstallRoot(t *testing.T) {
	n := layoutInstalls(t)
	script := func(root string) string { return root + "/bin/model-gateway.js" }
	tests := map[string]struct {
		argv []string
		want nodeReading
	}{
		"success: the current install": {argv: nodeCommand(script(n.current)), want: nodeReading{n.current, n.current, true}},
		"success: an older sibling version of this plugin": {
			argv: nodeCommand(script(n.older)), want: nodeReading{n.older, n.current, true},
		},
		"error: another marketplace's install": {
			argv: nodeCommand(script(n.otherMarket)), want: nodeReading{n.otherMarket, n.current, false},
		},
		"error: the same marketplace's install under a physically unrelated cache": {
			argv: nodeCommand(script(n.unrelated)), want: nodeReading{n.unrelated, n.current, false},
		},
		"error: a development checkout named model-gateway is another install": {
			argv: nodeCommand(script(n.checkout)), want: nodeReading{n.checkout, n.current, false},
		},
		"error: the source's foreign-model-gateway checkout is another install": {
			argv: nodeCommand(script(n.foreignNamed)), want: nodeReading{n.foreignNamed, n.current, false},
		},
		"error: a checkout whose name ends in model-gateway in mixed case is another install": {
			argv: nodeCommand(script(n.mixedCase)), want: nodeReading{n.mixedCase, n.current, false},
		},
		"success: an older sibling's script spelled in capitals matches without regard to case": {
			argv: nodeCommand(n.older + "/BIN/MODEL-GATEWAY.JS"), want: nodeReading{n.older, n.current, true},
		},
		"error: a directory that is no gateway but holds bin/model-gateway.js": {
			argv: nodeCommand(script(n.tools)), want: nodeReading{"", n.current, false},
		},
		"error: a doubled separator before bin in the script is not the source's pattern": {
			argv: nodeCommand(n.older + "//bin/model-gateway.js"), want: nodeReading{"", n.current, false},
		},
		"error: a command line that names no Node gateway": {
			argv: []string{"/usr/bin/node", filepath.Join(n.home, "server.js")}, want: nodeReading{"", n.current, false},
		},
	}
	predicate := newNodeGateway(t, n.current, nil)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, readNode(t, predicate, tt.argv)); diff != "" {
				t.Errorf("reading of %q (-want +got):\n%s", tt.argv, diff)
			}
		})
	}
	t.Run("success: the authority classifies a sibling as legacy and an unrelated root as foreign", func(t *testing.T) {
		got := map[string]ownership.Class{}
		for name, root := range map[string]string{"sibling": n.older, "unrelated": n.unrelated} {
			h := newHost(sampleOf(holderPID, procinfo.Image{Path: "/usr/bin/node", Device: 1, Inode: 7}, nodeCommand(script(root))...))
			h.listener = []listenerAnswer{identified(holderPID)}
			a := authorityOn(stateRoot(t), h)
			a.Legacy = predicate
			owner, err := resolve(t, a, t.Context(), recordName, publicPort)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			got[name] = owner.Class
		}
		if diff := gocmp.Diff(map[string]ownership.Class{"sibling": ownership.LegacyNode, "unrelated": ownership.Foreign}, got); diff != "" {
			t.Errorf("classes (-want +got):\n%s", diff)
		}
	})
	t.Run("success: the authority classifies the source's foreign-model-gateway checkout as foreign with its root", func(t *testing.T) {
		h := newHost(sampleOf(holderPID, procinfo.Image{Path: "/usr/bin/node", Device: 1, Inode: 7}, nodeCommand(script(n.foreignNamed))...))
		h.listener = []listenerAnswer{identified(holderPID)}
		a := authorityOn(stateRoot(t), h)
		a.Legacy = predicate
		owner, err := resolve(t, a, t.Context(), recordName, publicPort)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		type verdict struct {
			Class  ownership.Class
			PID    int
			Reason string
		}
		want := verdict{ownership.Foreign, holderPID, foreignReason(publicPort, holderPID, n.foreignNamed, n.current)}
		if diff := gocmp.Diff(want, verdict{owner.Class, owner.PID, owner.Reason}); diff != "" {
			t.Errorf("owner (-want +got):\n%s", diff)
		}
	})
}

// TestNodeGatewayComparedRoot decides the comparison itself: both roots handed to it are cleaned first. The source
// spells the candidate with a trailing separator (installBelongsToThisPlugin(`${olderSibling}${path.sep}`, ...)); in
// this port the candidate comes only from a command line, and the source's pattern puts exactly one separator between
// that root and bin, so the unclean candidate spellings a command line can carry are the ones the pattern admits
// before the plugin directory: a doubled separator and a dot segment. Cleaning makes either spelling the same
// candidate. The current root is also given with a trailing separator.
func TestNodeGatewayComparedRoot(t *testing.T) {
	n := layoutInstalls(t)
	unclean := func(root string) string {
		rel, err := filepath.Rel(n.home, root)
		if err != nil {
			t.Fatal(err)
		}
		return n.home + "//./" + rel + "/bin/model-gateway.js"
	}
	t.Run("candidate", func(t *testing.T) {
		predicate := newNodeGateway(t, n.current, nil)
		tests := map[string]struct {
			script string
			want   nodeReading
		}{
			"success: an older sibling's candidate root spelled with a doubled separator and a dot segment is cleaned before the comparison": {
				script: unclean(n.older), want: nodeReading{n.older, n.current, true},
			},
			"error: another marketplace's candidate root spelled the same way stays another install": {
				script: unclean(n.otherMarket), want: nodeReading{n.otherMarket, n.current, false},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				if diff := gocmp.Diff(tt.want, readNode(t, predicate, nodeCommand(tt.script))); diff != "" {
					t.Errorf("reading of %q (-want +got):\n%s", tt.script, diff)
				}
			})
		}
	})
	predicate := newNodeGateway(t, n.current+string(filepath.Separator), nil)
	tests := map[string]struct {
		root string
		want nodeReading
	}{
		"success: an older sibling of a root spelled with a trailing separator": {root: n.older, want: nodeReading{n.older, n.current, true}},
		"success: the current install against its own root with a trailing separator": {
			root: n.current, want: nodeReading{n.current, n.current, true},
		},
		"error: another marketplace against a root with a trailing separator": {
			root: n.otherMarket, want: nodeReading{n.otherMarket, n.current, false},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, readNode(t, predicate, nodeCommand(tt.root+"/bin/model-gateway.js"))); diff != "" {
				t.Errorf("reading (-want +got):\n%s", diff)
			}
		})
	}
}

// TestNodeGatewayRegistryDirectories decides that a configured cache parent with a name other than plugins makes its
// versions siblings, and that without it they are only installs elsewhere.
func TestNodeGatewayRegistryDirectories(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(home, "agent-plugins")
	plugin := filepath.Join(registry, "cache", "eigenwise-toolshed", "model-gateway")
	current, older := filepath.Join(plugin, "0.50.0"), filepath.Join(plugin, "0.48.0")
	for _, dir := range []string{current, older} {
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tests := map[string]struct {
		dirs []string
		want nodeReading
	}{
		"success: a configured registry directory makes the older version a sibling": {dirs: []string{registry}, want: nodeReading{older, current, true}},
		"error: without it the older version is another install":                     {want: nodeReading{older, current, false}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			predicate := newNodeGateway(t, current, tt.dirs)
			if diff := gocmp.Diff(tt.want, readNode(t, predicate, nodeCommand(older+"/bin/model-gateway.js"))); diff != "" {
				t.Errorf("reading (-want +got):\n%s", diff)
			}
		})
	}
}
