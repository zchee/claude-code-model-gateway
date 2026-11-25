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
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
		override, isolated, wrongCollector bool
	}{
		"success: default disables reporting":  {},
		"success: explicit collector":          {override: true},
		"success: isolated collector override": {isolated: true},
		"error: unexpected loopback collector": {override: true, wrongCollector: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			collector := env.StartUsageCollector(t)
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
			if tt.wrongCollector {
				other := env.StartUsageCollector(t)
				normal[fixtureUsageKey] = other.URL
				defer func() {
					if got := other.Requests(); len(got) != 0 {
						t.Errorf("unexpected collector received %v", got)
					}
				}()
			}
			env = gatewaytest.NewEnv(home, parent, normal, isolated)
			cmd, err := env.HelperCommand(t, "usage", expected)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			err = env.RunFixture(cmd)
			var want []gatewaytest.UsageRequest
			if tt.wrongCollector {
				if _, ok := errors.AsType[*exec.ExitError](err); !ok || !strings.Contains(output.String(), "usage endpoint is not the expected fixture destination") {
					t.Fatalf("expected child to refuse the endpoint: %v\n%s", err, &output)
				}
			} else if err != nil {
				t.Fatalf("usage helper: %v\n%s", err, &output)
			} else if tt.override || tt.isolated {
				want = []gatewaytest.UsageRequest{{Method: "POST", RequestURI: "/v1/logs", ContentType: "application/json", Body: fixtureUsageBody}}
			}
			if diff := gocmp.Diff(want, collector.Requests()); diff != "" {
				t.Errorf("collector requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUsageOverrideRefusedBeforeLaunch(t *testing.T) {
	tests := map[string]struct{ endpoint string }{
		"error: empty would fall back":         {""},
		"error: whitespace is not disabled":    {" 0 "},
		"error: relative URL":                  {"/v1/logs"},
		"error: HTTPS":                         {"https://127.0.0.1:9999/v1/logs"},
		"error: non-loopback IP":               {"http://192.0.2.1:9999/v1/logs"},
		"error: hostname":                      {"http://localhost:9999/v1/logs"},
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
			home := t.TempDir()
			bin := filepath.Join(home, "fixture-launch-canary")
			if err := os.WriteFile(bin, []byte(launchCanary), 0o700); err != nil {
				t.Fatal(err)
			}
			env := gatewaytest.NewEnv(home, nil, map[string]string{fixtureUsageKey: tt.endpoint}, nil)
			cmd := exec.CommandContext(t.Context(), bin)
			if err := env.RunFixture(cmd); !errors.Is(err, gatewaytest.ErrUsageEndpoint) || cmd.Process != nil {
				t.Fatalf("RunFixture: endpoint=%q process=%v err=%v; want ErrUsageEndpoint before launch", tt.endpoint, cmd.Process, err)
			}
			wantAbsent(t, filepath.Join(home, "launched"))
		})
	}
}
