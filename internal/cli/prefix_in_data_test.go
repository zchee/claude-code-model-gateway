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

//go:build darwin || linux

package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli"
)

// prefixInPath is a directory name that holds the program prefix as part of its own text.
const prefixInPath = "x: model-gateway: y"

// runServeWorker runs serve-worker through cli.Run with the state directory in both environments the command could
// read, so that no wiring of the command reads another one.
func runServeWorker(t *testing.T, state string) (status int, stdout, stderr string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("MODEL_GATEWAY_STATE_DIR", state)
	env := map[string]string{"HOME": home, "MODEL_GATEWAY_STATE_DIR": state}
	lookup := func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
	var out, errOut bytes.Buffer
	status = cli.Run([]string{"model-gateway", "serve-worker"}, strings.NewReader(""), &out, &errOut, lookup, func() (string, error) { return home, nil })
	return status, out.String(), errOut.String()
}

// TestServeCommandsPrintPathsUnchanged runs serve-shim and serve-worker with a state directory whose path holds the
// text ": model-gateway: ", up to a refusal that prints the path or the token path below it. The refusal reaches
// stderr as one line that starts with one "model-gateway: " prefix and carries the path exactly as it is: only a
// prefix at the start of a wrapped cause's own message is dropped, never text inside a path. A cause that starts with
// the prefix, printed from the same state directory, still carries it once.
func TestServeCommandsPrintPathsUnchanged(t *testing.T) {
	commands := map[string]func(t *testing.T, state string) (int, string, string){
		"serve-shim": func(t *testing.T, state string) (int, string, string) {
			return runServeShim(t, map[string]string{"MODEL_GATEWAY_STATE_DIR": state})
		},
		"serve-worker": runServeWorker,
	}
	tests := map[string]struct {
		// prepare returns the state directory to run with, under dir, and the line the refusal must print.
		prepare func(t *testing.T, dir string) (state, want string)
	}{
		"error: state directory that is not clean is printed unchanged": {
			prepare: func(t *testing.T, dir string) (string, string) {
				state := dir + "/" + prefixInPath + "/../state"
				return state, `model-gateway: the gateway state directory "` + state + `" must be a clean absolute path`
			},
		},
		"error: control token path that cannot be read is printed unchanged": {
			prepare: func(t *testing.T, dir string) (string, string) {
				state := filepath.Join(dir, prefixInPath)
				token := filepath.Join(state, "control-token")
				// A directory where the token file belongs cannot be read as one, and the error names its path.
				if err := os.MkdirAll(token, 0o700); err != nil {
					t.Fatal(err)
				}
				return state, "model-gateway: could not load the local control token: read " + token + ": is a directory"
			},
		},
		"error: cause that starts with the prefix carries it once": {
			prepare: func(t *testing.T, dir string) (string, string) {
				state := filepath.Join(dir, prefixInPath)
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, "control-token"), []byte("not-a-control-token\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return state, "model-gateway: could not load the local control token: invalid local control token; refusing lifecycle requests"
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			for command, run := range commands {
				t.Run(command, func(t *testing.T) {
					state, want := tc.prepare(t, t.TempDir())
					status, stdout, stderr := run(t, state)
					if diff := gocmp.Diff(1, status); diff != "" {
						t.Errorf("exit status mismatch (-want +got):\n%s", diff)
					}
					if diff := gocmp.Diff("", stdout); diff != "" {
						t.Errorf("stdout mismatch (-want +got):\n%s", diff)
					}
					if diff := gocmp.Diff(want+"\n", stderr); diff != "" {
						t.Errorf("stderr mismatch (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}
