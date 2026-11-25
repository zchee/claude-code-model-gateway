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
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const fixtureUsageBody = `{"resourceLogs":[]}`

func postFixtureUsage(t *testing.T, expected string) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(name), "OTEL_EXPORTER_OTLP") {
			t.Fatalf("unexpected inherited OTLP setting %q", name)
		}
	}
	endpoint := os.Getenv(fixtureUsageKey)
	if endpoint != expected {
		t.Fatal("usage endpoint is not the expected fixture destination")
	}
	if endpoint == "0" {
		return
	}
	client := &http.Client{
		Transport:     &http.Transport{},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(fixtureUsageBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("collector returned %s, want 204", resp.Status)
	}
}

func TestUsageFixtureProcess(t *testing.T) {
	tests := map[string]struct {
		override, isolated bool
	}{
		"success: default disables reporting":  {},
		"success: explicit collector":          {override: true},
		"success: isolated collector override": {isolated: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			collector := env.StartUsageCollector(t)
			copied := *collector
			parent := map[string]string{
				fixtureUsageKey:                      collector.URL,
				"OTEL_EXPORTER_OTLP_ENDPOINT":        collector.URL,
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":   collector.URL,
				"otel_exporter_otlp_traces_endpoint": collector.URL,
			}
			expected := "0"
			var normal, isolated map[string]string
			if tt.override || tt.isolated {
				expected = collector.URL
				delete(parent, fixtureUsageKey)
			}
			if tt.override {
				normal = map[string]string{fixtureUsageKey: collector.URL}
			}
			if tt.isolated {
				normal = map[string]string{fixtureUsageKey: "0"}
				isolated = map[string]string{fixtureUsageKey: collector.URL}
			}
			env = gatewaytest.NewEnv(home, parent, normal, isolated)
			cmd, err := env.HelperCommand(t, "usage", expected)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			var want []gatewaytest.UsageRequest
			if tt.override || tt.isolated {
				err = copied.RunFixture(env, cmd)
				want = []gatewaytest.UsageRequest{{Method: "POST", RequestURI: "/v1/logs", ContentType: "application/json", Body: fixtureUsageBody}}
			} else {
				err = env.RunFixture(cmd)
			}
			if err != nil {
				t.Fatalf("usage helper: %v\n%s", err, &output)
			}
			if diff := gocmp.Diff(want, collector.Requests()); diff != "" {
				t.Errorf("collector requests (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(want, copied.Requests()); diff != "" {
				t.Errorf("copied collector requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUsageCollectorChildLifetime(t *testing.T) {
	tests := map[string]struct{ callerDeadline bool }{
		"success: cleanup cancels a command without a context": {},
		"success: caller deadline still cancels a command":     {callerDeadline: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			bin := filepath.Join(home, "fixture-wait")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'ready\\n'\nexec /bin/sleep 30\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			var endpoint string
			var cmd *exec.Cmd
			var started time.Time
			var runErr error
			finished := make(chan struct{})
			if !t.Run("launch child", func(t *testing.T) {
				collector := env.StartUsageCollector(t)
				endpoint = collector.URL
				env[fixtureUsageKey] = endpoint
				if tt.callerDeadline {
					ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
					defer cancel()
					cmd = exec.CommandContext(ctx, bin)
				} else {
					// No caller context: only the fixture can stop this command.
					cmd = &exec.Cmd{Path: bin, Args: []string{bin}}
				}
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = stdout.Close() }()
				ready := make(chan string, 1)
				go func() {
					line, _ := bufio.NewReader(stdout).ReadString('\n')
					ready <- line
				}()
				started = time.Now()
				go func() {
					runErr = collector.RunFixture(env, cmd)
					close(finished)
				}()
				select {
				case line := <-ready:
					if line != "ready\n" {
						t.Fatalf("child ready line = %q", line)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("child did not start before cleanup")
				}
				if tt.callerDeadline {
					select {
					case <-finished:
					case <-time.After(3 * time.Second):
						t.Fatal("caller deadline did not stop the child before cleanup")
					}
				}
			}) {
				t.FailNow()
			}
			select {
			case <-finished:
				if _, ok := errors.AsType[*exec.ExitError](runErr); !ok || cmd.ProcessState == nil || cmd.ProcessState.Success() {
					t.Fatalf("child not reaped with unsuccessful exit: state=%v err=%v", cmd.ProcessState, runErr)
				}
			case <-time.After(time.Until(started.Add(4 * time.Second))):
				t.Fatal("child was not reaped within 4s")
			}
			if elapsed := time.Since(started); elapsed >= 4*time.Second {
				t.Errorf("child exit and cleanup took %v; want under 4s", elapsed)
			}
			address, err := url.Parse(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			// Rebind to prove cleanup released the socket without dialing a replacement listener.
			var cfg net.ListenConfig
			listener, err := env.Listen(t.Context(), "tcp4", address.Host, cfg.Listen)
			if err != nil {
				t.Fatalf("collector listener still occupied: %v", err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUsageCollectorAuthorization(t *testing.T) {
	home := t.TempDir()
	base := gatewaytest.NewEnv(home, nil, nil, nil)
	first, second := base.StartUsageCollector(t), base.StartUsageCollector(t)
	var stopped *gatewaytest.UsageCollector
	t.Run("start then clean up collector", func(t *testing.T) { stopped = base.StartUsageCollector(t) })
	bin := filepath.Join(home, "fixture-launch-canary")
	if err := os.WriteFile(bin, []byte(launchCanary), 0o700); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		url, helperURL string
		authorize      *gatewaytest.UsageCollector
	}{
		"error: arbitrary loopback URL":                          {url: "http://127.0.0.1:9000/v1/logs"},
		"error: existing URL without collector":                  {url: first.URL},
		"error: constructed collector without lease":             {url: first.URL, authorize: &gatewaytest.UsageCollector{URL: first.URL}},
		"error: changed collector path":                          {url: first.URL + "/other", authorize: first},
		"error: changed collector query":                         {url: first.URL + "?fixture=other", authorize: first},
		"error: different live collector for sandbox executable": {url: second.URL, authorize: first},
		"error: previously cleaned up collector":                 {url: stopped.URL, authorize: stopped},
		"error: helper aimed at another live collector":          {url: second.URL, helperURL: first.URL, authorize: second},
		"error: helper strings alone are not authority":          {url: first.URL, helperURL: first.URL},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(home, nil, map[string]string{fixtureUsageKey: tt.url}, nil)
			var cmd *exec.Cmd
			if tt.helperURL != "" {
				var err error
				cmd, err = env.HelperCommand(t, "usage", tt.helperURL)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				cmd = exec.CommandContext(t.Context(), bin)
			}
			launch := env.RunFixture
			if tt.authorize != nil {
				launch = func(cmd *exec.Cmd) error { return tt.authorize.RunFixture(env, cmd) }
			}
			if err := launch(cmd); !errors.Is(err, gatewaytest.ErrUsageEndpoint) || cmd.Process != nil {
				t.Fatalf("RunFixture: process=%v err=%v; want ErrUsageEndpoint before launch", cmd.Process, err)
			}
			wantAbsent(t, filepath.Join(home, "launched"))
			if got := len(first.Requests()) + len(second.Requests()) + len(stopped.Requests()); got != 0 {
				t.Errorf("collectors received %d requests, want zero", got)
			}
		})
	}
}

func TestUsagePolicyRequiredAtLaunch(t *testing.T) {
	tests := map[string]struct {
		raw, helper, empty  bool
		legacy, exporterKey string
	}{
		"error: missing on a sandbox executable":            {},
		"error: raw map missing on a sandbox executable":    {raw: true},
		"error: missing on a usage helper":                  {helper: true},
		"error: nonempty legacy environment missing policy": {legacy: "empty"},
		"error: legacy fixture mode cannot use empty map":   {legacy: "fixture", empty: true},
		"error: exporter in raw map":                        {raw: true, exporterKey: "OTel_Exporter_OTLP_Logs_Endpoint"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			var cmd *exec.Cmd
			switch {
			case tt.helper:
				var err error
				cmd, err = env.HelperCommand(t, "usage", "0")
				if err != nil {
					t.Fatal(err)
				}
			case tt.legacy != "":
				cmd = helperCommand(t, privateWorkdir(t), tt.legacy)
			default:
				bin := filepath.Join(home, "fixture-launch-canary")
				if err := os.WriteFile(bin, []byte(launchCanary), 0o700); err != nil {
					t.Fatal(err)
				}
				cmd = exec.CommandContext(t.Context(), bin)
			}
			if tt.empty {
				clear(env)
			}
			if tt.raw {
				env = gatewaytest.Environment{sandbox: home}
			}
			if tt.exporterKey != "" {
				env[fixtureUsageKey] = "0"
				env[tt.exporterKey] = "http://127.0.0.1:9000/v1/logs"
			} else {
				delete(env, fixtureUsageKey)
			}
			if err := env.RunFixture(cmd); !errors.Is(err, gatewaytest.ErrUsageEndpoint) || cmd.Process != nil {
				t.Fatalf("RunFixture: process=%v err=%v; want ErrUsageEndpoint before launch", cmd.Process, err)
			}
			wantAbsent(t, filepath.Join(home, "launched"))
		})
	}
}

func TestUsageHelperPolicyError(t *testing.T) {
	tests := map[string]struct{ endpoint string }{
		"error: empty endpoint argument":  {""},
		"error: named host argument":      {"http://localhost:9000/v1/logs"},
		"error: NUL in endpoint argument": {"http://127.0.0.1:9000/v1/logs\x00"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			if cmd, err := env.HelperCommand(t, "usage", tt.endpoint); cmd != nil || !errors.Is(err, gatewaytest.ErrUsageEndpoint) {
				t.Fatalf("HelperCommand: cmd=%v err=%v; want ErrUsageEndpoint", cmd, err)
			}
			cmd, err := env.HelperCommand(t, "usage", "0")
			if err != nil {
				t.Fatal(err)
			}
			cmd.Args[len(cmd.Args)-1] = tt.endpoint
			env[fixtureUsageKey] = tt.endpoint
			if err := env.RunFixture(cmd); !errors.Is(err, gatewaytest.ErrUsageEndpoint) || cmd.Process != nil {
				t.Fatalf("direct dispatch: process=%v err=%v; want ErrUsageEndpoint before launch", cmd.Process, err)
			}
		})
	}
}

func TestProbeUsagePolicyError(t *testing.T) {
	tests := map[string]struct {
		key, value string
		wantErr    error
	}{
		"error: unowned usage endpoint": {fixtureUsageKey, "http://127.0.0.1:9000/v1/logs", gatewaytest.ErrUsageEndpoint},
		"error: raw exporter setting":   {"OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:9000", gatewaytest.ErrUsageEndpoint},
		"success: ordinary probe miss":  {fixtureUsageKey, "0", nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, "fixture-launch-canary")
			if err := os.WriteFile(bin, []byte(launchCanary), 0o700); err != nil {
				t.Fatal(err)
			}
			env := gatewaytest.NewEnv(home, nil, map[string]string{"CODEX_GATEWAY_CLAUDE_BIN": bin}, nil)
			env[tt.key] = tt.value
			model, err := env.ProbeAlias(t.Context(), "sonnet", "http://127.0.0.1:9")
			if model != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("ProbeAlias: model=%v err=%v, want nil and %v", model, err, tt.wantErr)
			}
			if tt.wantErr != nil {
				wantAbsent(t, filepath.Join(home, "launched"))
			} else if _, err := os.Stat(filepath.Join(home, "launched")); err != nil {
				t.Fatalf("ordinary miss did not run the canary: %v", err)
			}
		})
	}
}

func TestUsageHelperRejectsInvalidEndpointSyntax(t *testing.T) {
	tests := map[string]struct{ endpoint string }{
		"error: empty would fall back":         {""},
		"error: whitespace is not disabled":    {" 0 "},
		"error: relative URL":                  {"/v1/logs"},
		"error: HTTPS":                         {"https://127.0.0.1:9999/v1/logs"},
		"error: non-loopback IP":               {"http://192.0.2.1:9999/v1/logs"},
		"error: hostname":                      {"http://localhost:9999/v1/logs"},
		"error: encoded host":                  {"http://%31%32%37.0.0.1:9999/v1/logs"},
		"error: credentials":                   {"http://user@127.0.0.1:9999/v1/logs"},
		"error: zoned IP":                      {"http://[::1%25lo]:9999/v1/logs"},
		"error: implicit port":                 {"http://127.0.0.1/v1/logs"},
		"error: zero port":                     {"http://127.0.0.1:0/v1/logs"},
		"error: nonnumeric port":               {"http://127.0.0.1:http/v1/logs"},
		"error: out-of-range port":             {"http://127.0.0.1:65536/v1/logs"},
		"error: default collector port":        {"http://127.0.0.1:4318/v1/logs"},
		"error: HTTP port with leading zeros":  {"http://127.0.0.1:00080/v1/logs"},
		"error: gateway port":                  {"http://127.0.0.1:18764/v1/logs"},
		"error: proxy port on IPv6 loopback":   {"http://[::1]:18765/v1/logs"},
		"error: mapped loopback reserved port": {"http://[::ffff:127.0.0.1]:4318/v1/logs"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			if cmd, err := env.HelperCommand(t, "usage", tt.endpoint); cmd != nil || !errors.Is(err, gatewaytest.ErrUsageEndpoint) {
				t.Fatalf("HelperCommand: endpoint=%q cmd=%v err=%v; want ErrUsageEndpoint before launch", tt.endpoint, cmd, err)
			}
		})
	}
}
