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
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

type envCheck struct {
	name  string
	check func(*testing.T)
}

func checkKey[T any](key Key[T], valid, invalid string, want [4]T) envCheck {
	name := key.variable.Name
	return envCheck{name: name, check: func(t *testing.T) {
		t.Helper()
		tests := map[string]struct {
			value string
			set   bool
			want  T
		}{
			"success: unset":                {want: want[0]},
			"success: empty":                {set: true, want: want[1]},
			"success: valid":                {value: valid, set: true, want: want[2]},
			"error: invalid or unvalidated": {value: invalid, set: true, want: want[3]},
		}
		for label, tt := range tests {
			t.Run(label, func(t *testing.T) {
				got := key.Get(lookup(name, tt.value, tt.set))
				if diff := gocmp.Diff(tt.want, got, cmpopts.EquateNaNs()); diff != "" {
					t.Errorf("lookup %s = %q, present %v (-want +got):\n%s", name, tt.value, tt.set, diff)
				}
			})
		}
	}}
}

func lookup(name, value string, set bool) LookupEnv {
	return func(key string) (string, bool) { return value, set && key == name }
}

func stringCheck(key Key[string], fallback string) envCheck {
	return checkKey(key, "  example  ", "not validated", [4]string{fallback, fallback, "  example  ", "not validated"})
}

func trimCheck(key Key[string]) envCheck {
	return checkKey(key, "  example  ", " \t", [4]string{"", "", "example", ""})
}

func rawCheck(key Key[*string]) envCheck {
	return checkKey(key, "  example  ", "not validated", [4]*string{nil, new(""), new("  example  "), new("not validated")})
}

func toggledCheck(key Key[bool], defaultOn bool) envCheck {
	if defaultOn {
		return checkKey(key, "0", " 0 ", [4]bool{true, true, false, true})
	}
	return checkKey(key, "1", "true", [4]bool{false, false, true, false})
}

func endpointCheck(key Key[Endpoint]) envCheck {
	return checkKey(key, "http://LOCALHOST:4318/v1/otlp?x=1#a", "https://example.test/otlp", [4]Endpoint{
		{}, {}, {Set: true, URL: wantURL("http", "127.0.0.1:4318", "/v1/otlp")}, {Set: true},
	})
}

func TestEveryVariable(t *testing.T) {
	tests := map[string]struct{ checks []envCheck }{
		"success: raw strings": {[]envCheck{
			stringCheck(AppData, ""), stringCheck(ClaudeBin, "claude"), stringCheck(ClaudeConfig, ""),
			stringCheck(DispatchCachePath, ""), stringCheck(GrokEndpoint, "https://cli-chat-proxy.grok.com/v1/responses"),
			stringCheck(GrokHome, ""), stringCheck(HostsFile, ""), stringCheck(NoProxy, ""), stringCheck(NoProxyLower, ""),
			stringCheck(OpenCodeProxyKey, ""), stringCheck(OpenCodeKey, ""), stringCheck(PluginCache, ""),
			stringCheck(PluginRoot, ""), stringCheck(ProxyConfig, ""), stringCheck(RegistryClaudeHome, ""),
			stringCheck(RequestBodyDir, ""), stringCheck(RequestLogPath, ""), stringCheck(SocketPath, ""),
			stringCheck(StateDirectory, ""), stringCheck(SystemRoot, `C:\Windows`),
			stringCheck(Upstream, "https://api.anthropic.com"), stringCheck(UserProfile, ""), stringCheck(WorkerPortValue, ""), stringCheck(XDGConfigHome, ""),
			rawCheck(AnthropicBaseURL), rawCheck(Home), rawCheck(ProxyCompression), rawCheck(GatewayCompression), rawCheck(StartupGeneration),
		}},
		"success: trimmed strings": {[]envCheck{
			trimCheck(GitHubToken), trimCheck(GHToken), trimCheck(GoogleCloudProject), trimCheck(LMStudioAPIKey),
			trimCheck(LMToken), trimCheck(VertexProject),
		}},
		"success: switches": {[]envCheck{
			toggledCheck(CompactGuard, true), toggledCheck(KeepPlanTools, false), toggledCheck(ListDispatchModel, false),
			toggledCheck(RequestLog, true), toggledCheck(Sentry, true), toggledCheck(Traceparent, false),
			checkKey(NodeTest, "0", "not rejected", [4]bool{false, false, true, true}),
			checkKey(LMStudioEnabled, " yES ", "sometimes", [4]*bool{nil, nil, new(true), nil}),
		}},
		"success: numeric variables": {[]envCheck{
			checkKey(CompatPort, "0x50", "80x", [4]float64{80, 80, 80, math.NaN()}),
			checkKey(PublicPort, " 0x10 ", "16x", [4]float64{18764, 18764, 16, math.NaN()}),
			checkKey(ProxyPort, "1.8e4", "18k", [4]float64{18765, 18765, 18000, math.NaN()}),
			checkKey(CompactMax, "2048", "Infinity", [4]float64{16777216, 16777216, 2048, 16777216}),
			checkKey(CompactRetries, "0x4", "1.5", [4]float64{2, 0, 4, 2}),
			checkKey(CompactDelay, ".5", "-1", [4]float64{250, 0, .5, 250}),
			checkKey(CompactTrigger, "1e5", "1e5!", [4]float64{math.NaN(), 0, 100000, math.NaN()}),
			checkKey(DispatchMax, "50.0", "5.1", [4]float64{500, 500, 50, 500}),
			checkKey(DispatchTTL, "1e5", "-1", [4]float64{14400000, 14400000, 100000, 14400000}),
			checkKey(DrainTimeout, "-2", "bad", [4]float64{30000, 30000, -2, 30000}),
			checkKey(LMStudioFirstWait, "0x20", "Infinity", [4]float64{10000, 0, 32, 10000}),
			checkKey(ModelsRefresh, "Infinity", "0", [4]float64{60000, 60000, math.Inf(1), 60000}),
			checkKey(PinCacheTTL, "-40", "NaN", [4]float64{86400000, 86400000, -40, 86400000}),
			checkKey(PinProbeTimeout, "Infinity", "0", [4]float64{5000, 5000, math.Inf(1), 5000}),
			checkKey(ProbeTimeoutValue, "-1", "NaN", [4]*float64{nil, nil, new(1.0), nil}),
			checkKey(ProxyRecovery, "-0.1", "1e", [4]float64{5000, 5000, 1000, 5000}),
			checkKey(SSEHeartbeatSeconds, " .2 ", "-0.5", [4]float64{20, 0, .2, 20}),
			checkKey(UsageMaxResponse, "900", "bad", [4]float64{4194304, 4194304, 1024, 4194304}),
			checkKey(WSRetries, "5.0", "-1", [4]float64{2, 0, 5, 2}),
			checkKey(WSRetryDelay, "2e2", "Infinity", [4]float64{250, 0, 200, 250}),
			checkKey(ClaudeAutoCompactWindow, "0b11", "5.1", [4]*float64{nil, nil, new(3.0), nil}),
			checkKey(CodexContextWindow, "full", "184999", [4]ContextWindow{{}, {}, {Full: true}, {}}),
			checkKey(LlamaServerPort, "+65535", "65536", [4]LlamaPort{{}, {Configured: true}, {Configured: true, Port: 65535}, {Configured: true, Invalid: "65536"}}),
		}},
		"success: endpoints": {[]envCheck{
			endpointCheck(TraceEndpoint), endpointCheck(UsageEndpoint), endpointCheck(OTelTracesEndpoint),
			endpointCheck(OTelLogsEndpoint), endpointCheck(OTelEndpoint),
			checkKey(LMStudioBaseURL, " https://Example.test:443/ ", "http://example.test", [4]LMOrigin{
				{}, {}, {Origin: "https://example.test"}, {Reason: "must use https (plain http only for a loopback host)"},
			}),
		}},
		"success: probe environment filters": {[]envCheck{
			checkKey(anthropicAPIKey, "value", "any", [4]bool{false, true, true, true}),
			checkKey(anthropicAuth, "value", "any", [4]bool{false, true, true, true}),
			checkKey(anthropicFable, "value", "any", [4]bool{false, true, true, true}),
			checkKey(anthropicOpus, "value", "any", [4]bool{false, true, true, true}),
			checkKey(anthropicSonnet, "value", "any", [4]bool{false, true, true, true}),
			checkKey(claudeOAuth, "value", "any", [4]bool{false, true, true, true}),
			checkKey(claudeCode, "value", "any", [4]bool{false, true, true, true}),
			checkKey(claudeEntrypoint, "value", "any", [4]bool{false, true, true, true}),
			checkKey(claudeSSEPort, "value", "any", [4]bool{false, true, true, true}),
			checkKey(claudeSessionID, "value", "any", [4]bool{false, true, true, true}),
			checkKey(claudeDiscovery, "value", "any", [4]bool{false, true, true, true}),
			checkKey(claudeNoFallback, "value", "any", [4]bool{false, true, true, true}),
			checkKey(claudeMaxOutput, "value", "any", [4]bool{false, true, true, true}),
			checkKey(enableToolSearch, "value", "any", [4]bool{false, true, true, true}),
			checkKey(customModel, "value", "any", [4]bool{false, true, true, true}),
			checkKey(customModelName, "value", "any", [4]bool{false, true, true, true}),
			checkKey(customModelDesc, "value", "any", [4]bool{false, true, true, true}),
		}},
	}
	seen := make(map[string]bool)
	for group, fixture := range tests {
		t.Run(group, func(t *testing.T) {
			for _, tc := range fixture.checks {
				if seen[tc.name] {
					t.Errorf("duplicate environmental test for %s", tc.name)
				}
				seen[tc.name] = true
				t.Run(tc.name, tc.check)
			}
		})
	}
	for _, row := range Variables() {
		if row.Name == "" || row.Default == "" || row.Meaning == "" || row.Rule == "" || row.Readers == "" {
			t.Errorf("incomplete metadata: %+v", row)
		}
		if !seen[row.Name] {
			t.Errorf("untested or duplicate variable in the catalog: %s", row.Name)
		}
		delete(seen, row.Name)
	}
	if len(seen) != 0 {
		t.Errorf("tested keys absent from the catalog: %v", seen)
	}
}
