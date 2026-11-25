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
	"errors"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

func TestSandboxRootOverrides(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	override := map[string]string{sandbox: outside, "CODEX_HOME": filepath.Join(home, "custom-codex")}
	tests := map[string]struct{ normal, isolated map[string]string }{
		"error: normal override cannot move sandbox":   {override, nil},
		"error: isolated override cannot move sandbox": {nil, override},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(home, nil, tt.normal, tt.isolated)
			if diff := gocmp.Diff([]string{home, override["CODEX_HOME"]}, []string{env[sandbox], env["CODEX_HOME"]}); diff != "" {
				t.Fatal(diff)
			}
			calls, err := refusedWrite(env, filepath.Join(outside, "settings.json"))
			if calls != 0 || !errors.Is(err, gatewaytest.ErrOutsideSandbox) {
				t.Fatalf("write calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestClaudeExecutableType(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	dir := filepath.Join(env["HOME"], "bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "claude")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ path string }{
		"error: directory":           {dir},
		"error: non-executable file": {file},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env["CODEX_GATEWAY_CLAUDE_BIN"] = tt.path
			err := env.StartClaude(t.Context(), func(context.Context, string) error { t.Fatal("unexpected launch"); return nil })
			if !errors.Is(err, gatewaytest.ErrRealClaude) {
				t.Fatalf("StartClaude: %v", err)
			}
		})
	}
}
