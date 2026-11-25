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

package config

import (
	"math"
	"net/url"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func wantURL(scheme, host, path string) *url.URL {
	return &url.URL{Scheme: scheme, Host: host, Path: path, RawPath: path}
}

func TestReadVariants(t *testing.T) {
	tests := map[string]struct {
		env        map[string]string
		wantWorker float64
		wantSuper  float64
		wantPort   string
		wantProbe  float64
		wantSSE    float64
		wantZip    *string
		wantBypass string
	}{
		"success: defaults": {wantWorker: 18764, wantSuper: 0, wantPort: "18764", wantProbe: 8000, wantSSE: 20000, wantBypass: "127.0.0.1,localhost"},
		"success: separate listen and recovery port conversions": {
			env:        map[string]string{"CODEX_GATEWAY_PORT": "0x50", "CODEX_GATEWAY_WORKER_PORT": "", "CODEX_GATEWAY_PROBE_TIMEOUT_MS": "-4", "CODEX_GATEWAY_SSE_HEARTBEAT_S": ".5", "CODEX_GATEWAY_REQUEST_COMPRESSION": "", "NO_PROXY": "a,b", "no_proxy": " c "},
			wantWorker: 80, wantSuper: 0, wantPort: "0x50", wantProbe: 1, wantSSE: 500, wantZip: new(""), wantBypass: "a,b, c ,127.0.0.1,localhost",
		},
		"success: defined proxy value wins": {
			env:        map[string]string{"CODEX_GATEWAY_PORT": "18000", "CODEX_GATEWAY_WORKER_PORT": " 1.9e4 ", "CCP_CODEX_REQUEST_COMPRESSION": "", "CODEX_GATEWAY_REQUEST_COMPRESSION": "zstd"},
			wantWorker: 19000, wantSuper: 19000, wantPort: "18000", wantProbe: 8000, wantSSE: 20000, wantZip: new(""), wantBypass: "127.0.0.1,localhost",
		},
		"error: invalid numeric listen port is not defaulted": {
			env:        map[string]string{"CODEX_GATEWAY_WORKER_PORT": "123x", "CODEX_GATEWAY_PROBE_TIMEOUT_MS": "NaN"},
			wantWorker: math.NaN(), wantSuper: math.NaN(), wantPort: "18764", wantProbe: 8000, wantSSE: 20000, wantBypass: "127.0.0.1,localhost",
		},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			lookup := func(name string) (string, bool) { v, ok := tt.env[name]; return v, ok }
			if diff := gocmp.Diff([]float64{tt.wantWorker, tt.wantSuper, tt.wantProbe, tt.wantSSE}, []float64{WorkerPort(lookup), SupervisorWorkerPort(lookup), ProbeTimeoutFor(lookup, "windows"), SSEHeartbeatMilliseconds(lookup)}, cmpopts.EquateNaNs()); diff != "" {
				t.Errorf("worker, supervisor, probe, SSE (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff([]string{tt.wantPort, tt.wantBypass}, []string{RecoveryPublicPort(lookup), NoProxyForPinProbe(lookup)}); diff != "" {
				t.Errorf("recovery and bypass (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantZip, ProxyRequestCompression(lookup)); diff != "" {
				t.Errorf("compression (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSignalEndpoints(t *testing.T) {
	tests := map[string]struct {
		env   map[string]string
		trace string
		usage string
	}{
		"success: defaults": {usage: "http://127.0.0.1:4318/v1/logs"},
		"success: trace opt-in": {
			env: map[string]string{"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"}, trace: "http://127.0.0.1:4318/v1/traces", usage: "http://127.0.0.1:4318/v1/logs",
		},
		"success: generic base": {
			env:   map[string]string{"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1", "OTEL_EXPORTER_OTLP_ENDPOINT": "HTTP:0177.1:080/base/%2E./v2/?q=1#x"},
			trace: "http://127.0.0.1/v2/v1/traces", usage: "http://127.0.0.1/v2/v1/logs",
		},
		"success: signal override before generic": {
			env:   map[string]string{"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://localhost/trace", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "https://[0:0:0:0:0:0:0:1]:9443/log", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"},
			trace: "http://127.0.0.1/trace", usage: "https://[::1]:9443/log",
		},
		"success: private disable and override": {
			env:   map[string]string{"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1", "CODEX_GATEWAY_USAGE_ENDPOINT": "0", "CODEX_GATEWAY_TELEMETRY_ENDPOINT": "http://0x7f000001:99/", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"},
			trace: "http://127.0.0.1:99/",
		},
		"success: explicit trace disable and explicit usage precedence": {
			env: map[string]string{
				"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1", "CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:4318/trace",
				"CODEX_GATEWAY_USAGE_ENDPOINT":       "http://localhost:8000/ours?gone=1",
				"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT":   "http://127.0.0.1:4318/ignored",
			},
			usage: "http://127.0.0.1:8000/ours",
		},
		"error: invalid explicit and whitespace signal do not fall back": {
			env: map[string]string{"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1", "CODEX_GATEWAY_TELEMETRY_ENDPOINT": "http://localhost.test/", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": " ", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318"},
		},
		"error: IPv4-mapped IPv6 is not an accepted loopback spelling": {
			env: map[string]string{"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://[::ffff:127.0.0.1]:4318"},
		},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			lookup := func(name string) (string, bool) { v, ok := tt.env[name]; return v, ok }
			var trace, usage string
			if endpoint := TraceURL(lookup); endpoint != nil {
				trace = endpoint.String()
			}
			if endpoint := UsageURL(lookup); endpoint != nil {
				usage = endpoint.String()
			}
			if diff := gocmp.Diff([]string{tt.trace, tt.usage}, []string{trace, usage}); diff != "" {
				t.Errorf("trace and usage (-want +got):\n%s", diff)
			}
		})
	}
}

func TestProbeFilters(t *testing.T) {
	tests := map[string]struct {
		env         map[string]string
		gateway     map[string]string
		pin, picker map[string]string
	}{
		"success: case and precedence": {
			env:     map[string]string{"ANTHROPIC_BASE_URL": "", "anthropic_base_url": "preserved by picker", "anthropic_api_key": "test", "ANTHROPIC_DEFAULT_FABLE_MODEL": "old", "CLAUDECODE": "", "claudecode": "preserved", "UNRELATED": "x"},
			gateway: map[string]string{"ANTHROPIC_BASE_URL": "replacement", "ANTHROPIC_DEFAULT_FABLE_MODEL": "new"},
			pin:     map[string]string{"CLAUDECODE": "", "claudecode": "preserved", "UNRELATED": "x"},
			picker:  map[string]string{"anthropic_base_url": "preserved by picker", "anthropic_api_key": "test", "claudecode": "preserved", "UNRELATED": "x"},
		},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			if diff := gocmp.Diff(tt.pin, PinProbeEnvironment(tt.env)); diff != "" {
				t.Errorf("pin environment (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.picker, NativePickerEnvironment(tt.env, tt.gateway)); diff != "" {
				t.Errorf("picker environment (-want +got):\n%s", diff)
			}
		})
	}
}
