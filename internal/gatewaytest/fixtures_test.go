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

//go:build process

package gatewaytest_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

func TestModelProxy(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	const event = "data: [DONE]\n\n"
	base := env.StartModelProxy(t, []string{"fixture-model"}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, event); err != nil {
			t.Error(err)
		}
	}))
	tests := map[string]struct{ method, path, contentType, body string }{
		"success: model catalog":    {"GET", "/v1/models", "application/json", `{"data":[{"id":"fixture-model"}]}`},
		"success: scripted message": {"POST", "/v1/messages", "text/event-stream", event},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tt.method, base+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if ip := net.ParseIP(req.URL.Hostname()); !ip.IsLoopback() || req.URL.Port() == "" {
				t.Fatalf("not a loopback fixture URL: %s", base)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			if diff := gocmp.Diff([]string{"200 OK", tt.contentType, tt.body}, []string{resp.Status, resp.Header.Get("Content-Type"), string(body)}); diff != "" {
				t.Errorf("response (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCountingProxy(t *testing.T) {
	tests := map[string]struct {
		destination, target string
		tunnel              bool
	}{
		"success: absolute request": {"http://example.invalid:9876/message", "example.invalid:9876", false},
		"success: CONNECT request":  {"https://example.invalid/message", "example.invalid:443", true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			proxy := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil).StartCountingProxy(t)
			addr, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(addr)}}
			defer client.CloseIdleConnections()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tt.destination, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err == nil {
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if tt.tunnel || resp.StatusCode != http.StatusBadGateway {
					t.Fatalf("unexpected proxy response: %s", resp.Status)
				}
			} else if !tt.tunnel {
				t.Fatal(err)
			}
			if diff := gocmp.Diff([]string{tt.target}, proxy.Targets()); diff != "" {
				t.Fatal(diff)
			}
			if n := proxy.Connections(); n != 1 {
				t.Fatalf("observed %d connections, want one", n)
			}
		})
	}
}

func TestRunFixtureRefusesExternalExecutable(t *testing.T) {
	env, outside := guardFixture(t, sandbox)
	if err := os.WriteFile(outside, []byte(launchCanary), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(env["HOME"], "renamed-helper")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ path string }{
		"error: explicit external helper":           {outside},
		"error: sandbox symlink to external helper": {link},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), tt.path)
			if err := env.RunFixture(cmd); !errors.Is(err, gatewaytest.ErrRealClaude) {
				t.Fatalf("RunFixture: %v", err)
			}
			wantAbsent(t, filepath.Join(filepath.Dir(outside), "launched"))
			wantAbsent(t, filepath.Join(env["HOME"], "launched"))
		})
	}
}

func TestFixtureDispatch(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	outside := filepath.Join(t.TempDir(), "fixture-command")
	if err := os.WriteFile(outside, []byte(launchCanary), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		env  gatewaytest.Environment
		path string
		args []string
	}{
		"error: empty environment outside executable": {nil, outside, nil},
		"error: no test selector":                     {env, self, nil},
		"error: unrelated test":                       {nil, self, []string{"-test.run=^TestFakeClaudeProcess$", "--", "--gatewaytest-helper=claude"}},
		"error: no child mode":                        {env, self, []string{helperTest, "--"}},
		"error: unknown child mode":                   {env, self, []string{helperTest, "--", helperPrefix + "unknown"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// A cancelled command cannot recursively launch the suite even if validation regresses.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cmd := exec.CommandContext(ctx, tt.path, tt.args...)
			cmd.Dir = env["HOME"]
			if err := tt.env.RunFixture(cmd); !errors.Is(err, gatewaytest.ErrRealClaude) {
				t.Fatalf("RunFixture returned %v, want refusal", err)
			}
		})
	}
	if cmd, err := env.HelperCommand(t, "TestFakeClaudeProcess"); err == nil || cmd != nil {
		t.Errorf("unregistered helper: cmd=%v err=%v", cmd, err)
	}
}

func TestHelperDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
		cmd, err := env.HelperCommand(t, "claude", "--version")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(6 * time.Second)
		if err := env.RunFixture(cmd); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired helper: %v", err)
		}
	})
}

func TestProbeEndpointRefusal(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	bin := filepath.Join(env["HOME"], "fixture-claude")
	if err := os.WriteFile(bin, []byte(launchCanary), 0o700); err != nil {
		t.Fatal(err)
	}
	env["CODEX_GATEWAY_CLAUDE_BIN"] = bin
	tests := map[string]struct{ endpoint string }{
		"error: hostname":                      {"http://localhost:9"},
		"error: non-loopback IP":               {"http://192.0.2.1:9"},
		"error: implicit port":                 {"http://127.0.0.1"},
		"error: gateway port":                  {"http://127.0.0.1:18764"},
		"error: proxy port":                    {"http://[::1]:18765"},
		"error: HTTP port":                     {"http://127.0.0.1:80"},
		"error: HTTPS":                         {"https://127.0.0.1:9"},
		"error: mapped loopback reserved port": {"http://[::ffff:127.0.0.1]:18764"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			model, err := env.ProbeAlias(t.Context(), "sonnet", tt.endpoint)
			if !errors.Is(err, gatewaytest.ErrProbeEndpoint) || model != nil {
				t.Fatalf("model=%v err=%v", model, err)
			}
			wantAbsent(t, filepath.Join(env["HOME"], "launched"))
		})
	}
}

func TestProbeProxyEnvironment(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	const endpoint = "http://[::1]:9"
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy", "ANTHROPIC_UNIX_SOCKET", "CODEX_GATEWAY_SOCKET_PATH"} {
		env[key] = "fixture-override"
		script.WriteString("[ -z \"${")
		script.WriteString(key)
		script.WriteString("+x}\" ] || exit 1\n")
	}
	script.WriteString("[ \"$ANTHROPIC_BASE_URL\" = '" + endpoint + "' ] || exit 1\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"model\":\"claude-sonnet-fixture\"}'\n")
	bin := filepath.Join(env["HOME"], "fixture-claude")
	if err := os.WriteFile(bin, []byte(script.String()), 0o700); err != nil {
		t.Fatal(err)
	}
	env["CODEX_GATEWAY_CLAUDE_BIN"] = bin
	got, err := env.ProbeAlias(t.Context(), "sonnet", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(new("claude-sonnet-fixture[1m]"), got); diff != "" {
		t.Fatal(diff)
	}
}

func TestHelperDirectory(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(home, "other")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(home, "inside", "child")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	localLink := filepath.Join(home, "local-link")
	if err := os.Symlink(inside, localLink); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ path string }{
		"error: in-sandbox symlink followed by parent segment": {localLink + "/.."},
		"error: relative home":                                 {"relative"},
		"error: unclean home":                                  {home + "/."},
		"error: parent segment":                                {home + "/unused/.."},
		"error: external directory":                            {outside},
		"error: external directory link":                       {link},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			env["HOME"] = tt.path
			if cmd, err := env.HelperCommand(t, "claude", "--version"); cmd != nil || !errors.Is(err, gatewaytest.ErrOutsideSandbox) {
				t.Fatalf("HelperCommand: cmd=%v err=%v", cmd, err)
			}
			for root, want := range map[string][]string{home: {".", "inside", "inside/child", "local-link", "other"}, outside: {"."}} {
				var got []string
				if err := fs.WalkDir(os.DirFS(root), ".", func(path string, _ fs.DirEntry, err error) error {
					got = append(got, path)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("entries in %s (-want +got):\n%s", root, diff)
				}
			}
		})
	}
}

func TestHelperChangedWorkdir(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	outside := t.TempDir()
	link := filepath.Join(env["HOME"], "other")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ path string }{
		"error: relative working directory":            {"."},
		"error: external working directory":            {outside},
		"error: symlink to external working directory": {link},
		"error: noncanonical working directory":        {env["HOME"] + "/."},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cmd, err := env.HelperCommand(t, "claude", "--version")
			if err != nil {
				t.Fatal(err)
			}
			cmd.Dir = tt.path
			if err := env.RunFixture(cmd); !errors.Is(err, gatewaytest.ErrOutsideSandbox) || cmd.Process != nil {
				t.Fatalf("RunFixture: process=%v err=%v", cmd.Process, err)
			}
		})
	}
}

func TestUnrepresentableEnvironment(t *testing.T) {
	tests := map[string]struct{ key, value string }{
		"error: empty key":     {"", "value"},
		"error: equals in key": {"NAME=extra", "value"},
		"error: NUL in key":    {"NA\x00ME", "value"},
		"error: NUL in value":  {"NAME", "v\x00alue"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			env["NAME"] = "original"
			env[tt.key] = tt.value
			cmd, err := env.HelperCommand(t, "claude", "--version")
			if err != nil {
				t.Fatal(err)
			}
			if err := env.RunFixture(cmd); !errors.Is(err, gatewaytest.ErrInvalidEnvironment) || cmd.Process != nil {
				t.Fatalf("RunFixture: process=%v err=%v", cmd.Process, err)
			}
		})
	}
}

func TestEmptyFixtureRun(t *testing.T) {
	dir := privateWorkdir(t)
	cmd := helperCommand(t, dir, "empty")
	if err := (gatewaytest.Environment{}).RunFixture(cmd); err != nil {
		t.Fatal(err)
	}
	var checked bool
	readReport(t, dir, &checked)
	if !checked {
		t.Fatal("empty-environment helper did not report success")
	}
}

func TestGatewayFixtureHelper(t *testing.T) {
	if endpoint := gatewaytest.DispatchHelper(t); endpoint != "" {
		postFixtureUsage(t, endpoint)
		return
	}
	if want, ok := os.LookupEnv("GATEWAYTEST_EXPECTED_CWD"); ok {
		current, err := os.Stat(".")
		if err != nil {
			t.Fatal(err)
		}
		expected, err := os.Stat(want)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(current, expected) {
			t.Fatal("child did not enter the prepared working directory")
		}
	}
}

func TestFakeClaudeProcess(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	tests := map[string]struct {
		args []string
		want string
	}{
		"success: version":     {[]string{"--version"}, "fake-claude 2.0.0"},
		"success: opus init":   {[]string{"--model", "opus"}, "claude-opus-5-5"},
		"success: sonnet init": {[]string{"--model", "sonnet"}, "claude-sonnet-5"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cmd, err := env.HelperCommand(t, "claude", tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			insideDirectory(t, env[sandbox], cmd.Dir)
			info, err := os.Stat(cmd.Dir)
			if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || cmd.WaitDelay <= 0 {
				t.Fatalf("unsafe child directory or wait delay: mode=%v delay=%v err=%v", info, cmd.WaitDelay, err)
			}
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			env["GATEWAYTEST_EXPECTED_CWD"] = cmd.Dir
			if err := env.RunFixture(cmd); err != nil {
				t.Fatalf("fixture: %v: %s", err, &out)
			}
			line, _, _ := strings.Cut(out.String(), "\n")
			if tt.args[0] == "--version" {
				if diff := gocmp.Diff(tt.want, line); diff != "" {
					t.Fatal(diff)
				}
				return
			}
			var init map[string]string
			if err := json.Unmarshal([]byte(line), &init); err != nil {
				t.Fatalf("fixture output %q: %v", line, err)
			}
			if diff := gocmp.Diff(map[string]string{"type": "system", "subtype": "init", "model": tt.want}, init); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
