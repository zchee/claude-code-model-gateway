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

// sameInstallRoot calls install.SameInstallRoot and turns a panic into a test failure, so that an unimplemented
// function cannot abort the package's other tests.
func sameInstallRoot(t *testing.T, a, b string) (same bool) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("SameInstallRoot panicked: %v", p)
		}
	}()
	return install.SameInstallRoot(a, b)
}

// TestSameInstallRoot decides the install root comparison: both roots are cleaned, resolved to their physical path,
// their separators folded and their letters lowercased before they are compared, as the package's comparison key is
// built; an empty or relative root is never the same as anything.
func TestSameInstallRoot(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(home, ".claude", "plugins", "cache", "eigenwise-toolshed", "model-gateway")
	current, sibling := filepath.Join(plugin, "0.50.0"), filepath.Join(plugin, "0.48.0")
	standalone := filepath.Join(home, "dev", "model-gateway")
	recased := filepath.Join(home, "dev", "Model-Gateway")
	elsewhere := filepath.Join(home, "dev2", "model-gateway")
	for _, dir := range []string{current, sibling, standalone, recased, elsewhere} {
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(home, "linked-install")
	if err := os.Symlink(current, link); err != nil {
		t.Fatal(err)
	}
	unborn := filepath.Join(home, "not-yet", "model-gateway")
	tests := map[string]struct {
		a, b string
		want bool
	}{
		"success: identical roots":                                    {a: current, b: current, want: true},
		"success: the same root with a trailing separator":            {a: current + string(filepath.Separator), b: current, want: true},
		"success: the same root with a doubled separator":             {a: plugin + string(filepath.Separator) + string(filepath.Separator) + "0.50.0", b: current, want: true},
		"success: the same root through a symlink":                    {a: link, b: current, want: true},
		"success: a standalone root that differs only in letter case": {a: recased, b: standalone, want: true},
		"success: a cache root that differs only in letter case": {
			a: filepath.Join(home, ".claude", "plugins", "cache", "Eigenwise-Toolshed", "Model-Gateway", "0.50.0"), b: current, want: true,
		},
		"success: identical roots that are not on disk yet": {a: unborn, b: unborn, want: true},
		"error: a sibling version is another root":          {a: sibling, b: current},
		"error: a root elsewhere with the same name":        {a: elsewhere, b: standalone},
		"error: a root and its parent":                      {a: plugin, b: current},
		"error: an empty first root":                        {a: "", b: current},
		"error: an empty second root":                       {a: current, b: ""},
		"error: two empty roots":                            {a: "", b: ""},
		"error: a relative first root":                      {a: "dev/model-gateway", b: standalone},
		"error: two identical relative roots":               {a: "dev/model-gateway", b: "dev/model-gateway"},
		"error: two spellings of one relative root":         {a: filepath.Join(".", "dev", "model-gateway"), b: "dev/model-gateway"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := sameInstallRoot(t, tt.a, tt.b); got != tt.want {
				t.Errorf("SameInstallRoot(%q, %q) = %v; want %v", tt.a, tt.b, got, tt.want)
			}
			if got := sameInstallRoot(t, tt.b, tt.a); got != tt.want {
				t.Errorf("SameInstallRoot(%q, %q) = %v; want %v (the comparison is symmetric)", tt.b, tt.a, got, tt.want)
			}
		})
	}
}
