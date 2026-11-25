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
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

// insideDirectory checks the actual filesystem location, not a string prefix.
func insideDirectory(t *testing.T, root, dir string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %q: %v", dir, err)
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		t.Errorf("%q does not resolve inside %q: relative=%q err=%v", dir, root, rel, err)
	}
}

func TestThrowawayHome(t *testing.T) {
	home := t.TempDir()
	env := gatewaytest.NewEnv(home, map[string]string{"HOME": "/inherited-home", "USERPROFILE": "/inherited-profile"}, nil, nil)
	t.Run("home", func(t *testing.T) {
		if env["HOME"] != home || env["USERPROFILE"] != home {
			t.Errorf("HOME=%q USERPROFILE=%q; want %q", env["HOME"], env["USERPROFILE"], home)
		}
		insideDirectory(t, os.TempDir(), env["HOME"])
	})
	t.Run("config", func(t *testing.T) {
		if got, want := env["CLAUDE_CONFIG_DIR"], filepath.Join(home, ".claude"); got != want {
			t.Errorf("CLAUDE_CONFIG_DIR=%q, want %q", got, want)
		}
	})
}

func TestFixtureClaudeBinary(t *testing.T) {
	home := t.TempDir()
	env := gatewaytest.NewEnv(home, map[string]string{"CODEX_GATEWAY_CLAUDE_BIN": "claude"}, nil, nil)
	bin := env["CODEX_GATEWAY_CLAUDE_BIN"]
	if bin != filepath.Join(home, "missing-claude") {
		t.Errorf("probe executable=%q, want the nonexistent fixture executable", bin)
	}
	insideDirectory(t, os.TempDir(), filepath.Dir(bin))
}

func TestFixtureEnvironment(t *testing.T) {
	home := t.TempDir()
	want := gatewaytest.Environment{
		"HOME": home, "USERPROFILE": home, "CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"), "CODEX_HOME": filepath.Join(home, ".codex"),
		"MODEL_GATEWAY_TEST_SANDBOX": home, "MODEL_GATEWAY_REQUEST_BODY_DIR": filepath.Join(home, ".claude", "model-gateway", "request-body"),
		"HTTP_PROXY": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "http_proxy": "", "https_proxy": "", "all_proxy": "", "NO_PROXY": "*", "no_proxy": "*",
		"CODEX_GATEWAY_PORT": "0", "CODEX_GATEWAY_WORKER_PORT": "0", "CODEX_GATEWAY_PROXY_PORT": "0", "CODEX_GATEWAY_COMPAT_PORT": "0",
		"CODEX_GATEWAY_SOCKET_PATH": filepath.Join(home, "gateway.sock"), "CODEX_GATEWAY_GROK_HOME": filepath.Join(home, ".grok"),
		"CODEX_GATEWAY_GROK_ENDPOINT": "http://127.0.0.1:9/v1/responses", "CODEX_GATEWAY_ANTHROPIC_UPSTREAM": "http://127.0.0.1:9",
		"CODEX_GATEWAY_CLAUDE_BIN": filepath.Join(home, "missing-claude"), "CODEX_GATEWAY_HOSTS_FILE": filepath.Join(home, "hosts"),
		"CODEX_GATEWAY_DISPATCH_CACHE_PATH": filepath.Join(home, "dispatch-routes.json"), "CODEX_GATEWAY_REQUEST_LOG_PATH": filepath.Join(home, "request-routes.jsonl"),
		"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "http://127.0.0.1:9", "KEEP_ME": "parent",
	}
	parent := map[string]string{"ANTHROPIC_BASE_URL": "inherited", "ANTHROPIC_UNIX_SOCKET": "inherited", "CLAUDE_CODE_PLUGIN_CACHE_DIR": "inherited", "CODEX_GATEWAY_UNKNOWN": "inherited", "MODEL_GATEWAY_UNKNOWN": "inherited", "MODEL_GATEWAY_STATE_DIR": "inherited"}
	for key := range want {
		parent[key] = "inherited"
	}
	parent["KEEP_ME"] = "parent"
	if diff := gocmp.Diff(want, gatewaytest.NewEnv(home, parent, nil, nil)); diff != "" {
		t.Errorf("fixture environment (-want +got):\n%s", diff)
	}
}

func TestFixtureOverrides(t *testing.T) {
	tests := map[string]struct {
		override, isolated map[string]string
		want               string
	}{
		"inherited_copy":         {map[string]string{"CODEX_GATEWAY_PORT": "18764"}, nil, "0"},
		"explicit_override":      {map[string]string{"CODEX_GATEWAY_PORT": "23456"}, nil, "23456"},
		"isolated_override_wins": {map[string]string{"CODEX_GATEWAY_PORT": "23456"}, map[string]string{"CODEX_GATEWAY_PORT": "18764"}, "18764"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(t.TempDir(), map[string]string{"CODEX_GATEWAY_PORT": "18764"}, tt.override, tt.isolated)
			if got := env["CODEX_GATEWAY_PORT"]; got != tt.want {
				t.Errorf("port override=%q, want %q", got, tt.want)
			}
		})
	}
}
