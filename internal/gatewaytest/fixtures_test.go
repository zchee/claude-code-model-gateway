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
	json "encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func TestFakeClaudeProcess(t *testing.T) {
	if i := slices.Index(os.Args, "--"); i >= 0 && len(os.Args) > i+1 && os.Args[i+1] == "fixture-claude" {
		if err := gatewaytest.FakeClaude(os.Args[i+2:], os.Stdout); err != nil {
			t.Fatal(err)
		}
		return
	}
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
			cmd := gatewaytest.HelperCommand(t, "TestFakeClaudeProcess", append([]string{"fixture-claude"}, tt.args...)...)
			cmd.Dir = env["HOME"]
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
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
