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

package gatewaytest_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const (
	sandbox      = "MODEL_GATEWAY_TEST_SANDBOX"
	runner       = "NODE_TEST_CONTEXT"
	launchCanary = "#!/bin/sh\n: > \"${0%/*}/launched\"\nprintf '9.9.9\\n'\n"
)

// Both sides of the boundary belong to the test, even if the guard regresses.
func guardFixture(t *testing.T, trigger string) (gatewaytest.Environment, string) {
	t.Helper()
	root, outside := t.TempDir(), filepath.Join(t.TempDir(), "untouched")
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	env := gatewaytest.Environment{"HOME": home, "USERPROFILE": home, "TMPDIR": root}
	if trigger == runner {
		env[runner] = "child"
	} else {
		env[sandbox] = root
	}
	return env, outside
}

func wantAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lstat(%q)=%v, want no entry", path, err)
	}
}

func TestUserSettingsPath(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			env, _ := guardFixture(t, key)
			file, err := env.UserSettingsPath()
			if err != nil {
				t.Errorf("UserSettingsPath: %v", err)
			}
			if want := filepath.Join(env["HOME"], ".claude", "settings.json"); file != want {
				t.Errorf("settings path=%q, want %q", file, want)
			}
			insideDirectory(t, env["TMPDIR"], filepath.Dir(filepath.Dir(file)))
		})
	}
}

func TestRefuseOutsideUserHome(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			env, outside := guardFixture(t, key)
			env["HOME"], env["USERPROFILE"] = outside, outside
			_, err := env.UserSettingsPath()
			if !errors.Is(err, gatewaytest.ErrOutsideSandbox) {
				t.Errorf("UserSettingsPath: got %v, want ErrOutsideSandbox", err)
			}
			wantAbsent(t, outside)
		})
	}
}

func refusedWrite(env gatewaytest.Environment, path string) (int, error) {
	calls := 0
	err := env.WriteSettings(path, map[string]any{"env": map[string]string{"SHOULD_NOT": "land"}}, func(string, map[string]any) error { calls++; return nil })
	return calls, err
}

func TestRefuseOutsideSettingsWrite(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			env, outside := guardFixture(t, key)
			calls, err := refusedWrite(env, filepath.Join(outside, ".claude", "settings.json"))
			if !errors.Is(err, gatewaytest.ErrOutsideSandbox) || calls != 0 {
				t.Errorf("outside write: error=%v calls=%d, want ErrOutsideSandbox and zero calls", err, calls)
			}
			wantAbsent(t, outside)
		})
	}
}

func TestRefuseSymlinkSettingsWrite(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			env, outside := guardFixture(t, key)
			dest := filepath.Dir(outside)
			link := filepath.Join(env["HOME"], ".claude")
			if err := os.Symlink(dest, link); err != nil {
				t.Fatal(err)
			}
			calls, err := refusedWrite(env, filepath.Join(link, "settings.json"))
			if !errors.Is(err, gatewaytest.ErrOutsideSandbox) || calls != 0 {
				t.Errorf("symlink write: error=%v calls=%d, want ErrOutsideSandbox and zero calls", err, calls)
			}
			wantAbsent(t, filepath.Join(dest, "settings.json"))
		})
	}
}

func TestAllowSandboxSettingsWrite(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			env, _ := guardFixture(t, key)
			file := filepath.Join(env["HOME"], ".claude", "settings.json")
			calls := 0
			// The content assertion covers the complete operation only with the product settings writer as the leaf.
			err := env.WriteSettings(file, map[string]any{"env": map[string]any{"OK": "1"}}, func(path string, settings map[string]any) error {
				calls++
				if path != file {
					t.Fatalf("writer received %q, want %q", path, file)
				}
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					return err
				}
				data, err := json.Marshal(settings)
				if err != nil {
					return err
				}
				return os.WriteFile(file, data, 0o600)
			})
			if err != nil || calls != 1 {
				t.Errorf("allowed write: error=%v calls=%d, want nil and one call", err, calls)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(map[string]any{"env": map[string]any{"OK": "1"}}, got); diff != "" {
				t.Errorf("settings on disk (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSettingsWriterError(t *testing.T) {
	env, _ := guardFixture(t, sandbox)
	path := filepath.Join(env["HOME"], ".claude", "settings.json")
	want := &os.PathError{Op: "write", Path: path, Err: fs.ErrPermission}
	calls := 0
	got := env.WriteSettings(path, map[string]any{}, func(string, map[string]any) error { calls++; return want })
	if cause, ok := got.(*os.PathError); !ok || cause != want || calls != 1 { //nolint:errorlint // The caller needs the original error, not a wrapper.
		t.Errorf("WriteSettings returned %v after %d calls, want the original writer error after one call", got, calls)
	}
}

func TestClaudeLaunchGuard(t *testing.T) {
	for _, key := range []string{sandbox, runner} {
		t.Run(key, func(t *testing.T) {
			env, outside := guardFixture(t, key)
			fixture := filepath.Join(env["HOME"], "claude")
			for _, name := range []string{fixture, outside} {
				if err := os.WriteFile(name, []byte(launchCanary), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(env["HOME"], "linked-claude")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			env["PATH"] = env["HOME"]
			t.Setenv("PATH", env["HOME"])
			t.Setenv("CODEX_GATEWAY_CLAUDE_BIN", fixture)
			tests := map[string]struct {
				path  string
				calls int
				err   error
			}{
				"named_fixture":   {fixture, 1, nil},
				"missing_fixture": {filepath.Join(env["HOME"], "missing-claude"), 1, nil},
				"real_claude":     {"", 0, gatewaytest.ErrRealClaude},
				"bare_name":       {"claude", 0, gatewaytest.ErrRealClaude},
				"outside_fixture": {outside, 0, gatewaytest.ErrRealClaude},
				"symlink_outside": {link, 0, gatewaytest.ErrRealClaude},
			}
			for name, tt := range tests {
				t.Run(name, func(t *testing.T) {
					delete(env, "CODEX_GATEWAY_CLAUDE_BIN")
					if tt.path != "" {
						env["CODEX_GATEWAY_CLAUDE_BIN"] = tt.path
					}
					calls := 0
					err := env.StartClaude(t.Context(), func(_ context.Context, path string) error {
						calls++
						if tt.calls == 0 || path != tt.path {
							t.Errorf("unexpected launcher call with path %q", path)
						}
						return nil
					})
					wantAbsent(t, filepath.Join(env["HOME"], "launched"))
					wantAbsent(t, filepath.Join(filepath.Dir(outside), "launched"))
					if calls != tt.calls || !errors.Is(err, tt.err) {
						t.Errorf("StartClaude: error=%v calls=%d, want %v and %d calls", err, calls, tt.err, tt.calls)
					}
				})
			}
		})
	}
}

func TestPortGuard(t *testing.T) {
	for _, key := range []string{sandbox, runner, "unguarded"} {
		t.Run(key, func(t *testing.T) {
			env := gatewaytest.Environment{}
			wantCalls := 1
			var wantErr error
			if key != "unguarded" {
				env, _ = guardFixture(t, key)
				wantCalls = 0
				wantErr = gatewaytest.ErrReservedPort
			}
			for _, port := range []int{18764, 18765, 80} {
				t.Run(strconv.Itoa(port), func(t *testing.T) {
					calls := 0
					address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
					_, err := env.Listen(t.Context(), "tcp", address, func(_ context.Context, network, addr string) (net.Listener, error) {
						calls++
						if network != "tcp" || addr != address {
							t.Errorf("listen leaf received %q, %q, want tcp, %q", network, addr, address)
						}
						return nil, nil
					})
					if calls != wantCalls || !errors.Is(err, wantErr) {
						t.Errorf("Listen(%q): error=%v calls=%d, want %v and %d calls", address, err, calls, wantErr, wantCalls)
					}
				})
			}
		})
	}
}
