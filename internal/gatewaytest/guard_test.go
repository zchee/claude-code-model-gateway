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
	json "encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// Both sides of the boundary belong to the test, even if the guard regresses.
func guardFixture(t *testing.T, trigger string) (gatewaytest.Environment, string) {
	t.Helper()
	root, outside := t.TempDir(), filepath.Join(t.TempDir(), "untouched")
	env := gatewaytest.Environment{"HOME": root, "USERPROFILE": root}
	if trigger == "NODE_TEST_CONTEXT" {
		t.Setenv("TMPDIR", root)
		env["TMPDIR"], env[trigger] = root, "child"
	} else {
		env[trigger] = root
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
	env, _ := guardFixture(t, "MODEL_GATEWAY_TEST_SANDBOX")
	file, err := env.UserSettingsPath()
	if err != nil {
		t.Errorf("UserSettingsPath: %v", err)
	}
	if got, want := file, filepath.Join(env["HOME"], ".claude", "settings.json"); got != want {
		t.Errorf("settings path=%q, want %q", got, want)
	}
	insideDirectory(t, os.TempDir(), filepath.Dir(filepath.Dir(file)))
}

func TestRefuseOutsideUserHome(t *testing.T) {
	for _, key := range []string{"MODEL_GATEWAY_TEST_SANDBOX", "NODE_TEST_CONTEXT"} {
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

func TestRefuseOutsideSettingsWrite(t *testing.T) {
	t.Run("fixture_guard_enabled", func(t *testing.T) {
		home := t.TempDir()
		if got := gatewaytest.NewEnv(home, nil, nil, nil)["MODEL_GATEWAY_TEST_SANDBOX"]; got != home {
			t.Errorf("fixture guard=%q, want %q", got, home)
		}
	})
	for _, key := range []string{"MODEL_GATEWAY_TEST_SANDBOX", "NODE_TEST_CONTEXT"} {
		t.Run(key, func(t *testing.T) {
			env, outside := guardFixture(t, key)
			err := env.WriteSettings(filepath.Join(outside, ".claude", "settings.json"), map[string]any{"env": map[string]string{"SHOULD_NOT": "land"}})
			if !errors.Is(err, gatewaytest.ErrOutsideSandbox) {
				t.Errorf("WriteSettings: got %v, want ErrOutsideSandbox", err)
			}
			wantAbsent(t, outside)
		})
	}
}

func TestRefuseSymlinkSettingsWrite(t *testing.T) {
	for _, key := range []string{"MODEL_GATEWAY_TEST_SANDBOX", "NODE_TEST_CONTEXT"} {
		t.Run(key, func(t *testing.T) {
			env, outside := guardFixture(t, key)
			dest := filepath.Dir(outside)
			link := filepath.Join(env["HOME"], ".claude")
			if err := os.Symlink(dest, link); err != nil {
				t.Fatal(err)
			}
			err := env.WriteSettings(filepath.Join(link, "settings.json"), map[string]any{})
			if !errors.Is(err, gatewaytest.ErrOutsideSandbox) {
				t.Errorf("WriteSettings through symlink: got %v, want ErrOutsideSandbox", err)
			}
			wantAbsent(t, filepath.Join(dest, "settings.json"))
		})
	}
}

func TestAllowSandboxSettingsWrite(t *testing.T) {
	env, _ := guardFixture(t, "MODEL_GATEWAY_TEST_SANDBOX")
	file := filepath.Join(env["HOME"], ".claude", "settings.json")
	want := map[string]any{"env": map[string]any{"OK": "1"}}
	if err := env.WriteSettings(file, want); err != nil {
		t.Errorf("WriteSettings: %v", err)
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
}

func TestClaudeLaunchGuard(t *testing.T) {
	for _, key := range []string{"MODEL_GATEWAY_TEST_SANDBOX", "NODE_TEST_CONTEXT"} {
		t.Run(key, func(t *testing.T) {
			env, _ := guardFixture(t, key)
			t.Run("named_fixture", func(t *testing.T) {
				env["CODEX_GATEWAY_CLAUDE_BIN"] = filepath.Join(env["HOME"], "missing-claude")
				if err := env.CheckClaude(); err != nil {
					t.Errorf("named fixture executable refused: %v", err)
				}
			})
			t.Run("real_claude", func(t *testing.T) {
				delete(env, "CODEX_GATEWAY_CLAUDE_BIN")
				if err := env.CheckClaude(); !errors.Is(err, gatewaytest.ErrRealClaude) {
					t.Errorf("implicit claude: got %v, want ErrRealClaude", err)
				}
			})
		})
	}
}

func TestPortGuard(t *testing.T) {
	tests := map[string]struct {
		ports []int
		want  error
	}{
		"MODEL_GATEWAY_TEST_SANDBOX": {[]int{18764, 18765, 80}, gatewaytest.ErrReservedPort},
		"NODE_TEST_CONTEXT":          {[]int{18764, 18765, 80}, gatewaytest.ErrReservedPort},
		"unguarded":                  {[]int{18764, 18765, 80}, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.Environment{}
			if name != "unguarded" {
				env, _ = guardFixture(t, name)
			}
			for _, port := range tt.ports {
				t.Run(strconv.Itoa(port), func(t *testing.T) {
					if err := env.CheckBind(port); !errors.Is(err, tt.want) {
						t.Errorf("CheckBind(%d): got %v, want %v", port, err, tt.want)
					}
				})
			}
			if name != "unguarded" {
				t.Run("ephemeral", func(t *testing.T) {
					if err := env.CheckBind(0); err != nil {
						t.Errorf("ephemeral port refused: %v", err)
					}
				})
			}
		})
	}
}
