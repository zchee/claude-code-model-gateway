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

//go:build process && darwin

package worker_test

import (
	"crypto/rand"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// entryKinds maps each name in dir to the kind of file it is: "socket", "regular file", or its mode otherwise.
func entryKinds(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make(map[string]string, len(entries))
	for _, entry := range entries {
		switch kind := entry.Type(); {
		case kind&fs.ModeSocket != 0:
			kinds[entry.Name()] = "socket"
		case kind.IsRegular():
			kinds[entry.Name()] = "regular file"
		default:
			kinds[entry.Name()] = kind.String()
		}
	}
	return kinds
}

// TestAtNameSocketIsPathOnDarwin starts the socket on a relative name that starts with '@' in a working directory of
// its own. Darwin has no abstract namespace, so the name is an ordinary relative path there: the start takes the lock
// and binds like any pathname start, leaving the socket '@<id>' and its lock file '@<id>.lock' in that directory.
func TestAtNameSocketIsPathOnDarwin(t *testing.T) {
	tests := map[string]struct {
		// prefix starts the relative name the starter receives.
		prefix string
	}{
		"success: an at-sign name is bound as a relative path with its lock file beside it": {prefix: "@"},
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The name is relative and short; the starter binds it inside its own directory, so the long absolute
			// path of that directory never reaches sockaddr_un.sun_path.
			socket := tc.prefix + "mg-at-name-" + rand.Text()
			dir, result, join := startInOwnDirectory(t, env, socket)
			if !strings.HasPrefix(result, "owner ") {
				t.Errorf("the start on a free relative name: %q, want an owner", result)
			}
			want := map[string]string{socket: "socket", socket + ".lock": "regular file"}
			if diff := gocmp.Diff(want, entryKinds(t, dir)); diff != "" {
				t.Errorf("files in the starter's working directory (-want +got):\n%s", diff)
			}
			if err := join(); err != nil {
				t.Error(err)
			}
		})
	}
}
