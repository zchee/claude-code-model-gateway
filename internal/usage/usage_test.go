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

package usage

import (
	"bytes"
	json "encoding/json/v2"
	"net/http"
	"os"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

const compositionBody = `{
 "model":"claude-codex-auto", "system":[{"type":"text","text":"private system"}],
 "tools":[{"name":"Read","description":"private native schema"},{"name":"mcp__sidequest__claim","description":"private MCP schema"}],
 "messages":[{"role":"user","content":"private first message"},{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"private":true}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"private result"}]}]
}`

func wire(t *testing.T, raw string) jsonx.WireValue {
	t.Helper()
	v, err := jsonx.DecodeWire([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func encoded(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func checkJSON(t *testing.T, want, got any) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(encoded(t, want), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded(t, got), &b); err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(a, b); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func options(t *testing.T) Options {
	t.Helper()
	return Options{ProjectsDir: t.TempDir(), Now: func() time.Time { return time.Unix(123, 0) }, Random: bytes.NewReader(make([]byte, 8192))}
}

func onlyEvent(t *testing.T, records []Record, event string) Record {
	t.Helper()
	matches := events(records, event)
	if len(matches) != 1 {
		t.Fatalf("got %d %q records, want 1", len(matches), event)
	}
	return matches[0]
}

func events(records []Record, event string) []Record {
	var selected []Record
	for _, r := range records {
		if r.EventName == event {
			selected = append(selected, r)
		}
	}
	return selected
}

func TestInputComposition(t *testing.T) {
	got := inputComposition(wire(t, compositionBody), 9876)
	want := map[string]int64{"request_body_bytes": 9876, "input_system_bytes": 41, "input_tools_bytes": 123, "input_native_tools_bytes": 53, "input_mcp_tools_bytes": 67, "input_messages_bytes": 258, "input_first_message_bytes": 49, "input_history_bytes": 205, "input_tool_results_bytes": 72}
	checkJSON(t, want, got)
	if bytes.Contains(encoded(t, got), []byte("private")) {
		t.Error("request content retained in composition")
	}
}

func TestLargestRemainder(t *testing.T) {
	if diff := gocmp.Diff([]int64{3, 2, 2}, allocateLargestRemainder(7, []int64{1, 1, 1})); diff != "" {
		t.Errorf("allocation (-want +got):\n%s", diff)
	}
}

func TestInputAttribution(t *testing.T) {
	tests := map[string]struct {
		usage map[string]any
		want  map[string]any
	}{
		"success: measured":    {map[string]any{"input_tokens": 100, "output_tokens": 11, "cache_read_input_tokens": 40, "cache_creation_input_tokens": 20}, map[string]any{"context_tokens": 160, "input_tools_tokens": 47, "input_native_tools_tokens": 21, "input_mcp_tools_tokens": 26, "input_system_tokens": 16, "input_first_message_tokens": 19, "input_history_tokens": 78, "cache_read_tools_tokens": 40, "cache_read_system_tokens": 0, "cache_read_first_message_tokens": 0, "cache_read_history_tokens": 0, "fresh_tools_tokens": 7, "fresh_system_tokens": 16, "fresh_first_message_tokens": 19, "fresh_history_tokens": 78}},
		"success: output only": {map[string]any{"output_tokens": 5}, map[string]any{"context_tokens": nil, "input_history_tokens": 52}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := onlyEvent(t, NewBuilder(options(t)).Build(Request{Body: wire(t, compositionBody), StatusCode: 200, Usage: tt.usage}), "gateway.token.usage")
			for key, want := range tt.want {
				got, ok := r.Attributes[key]
				if !ok {
					t.Errorf("missing %q", key)
					continue
				}
				t.Run(key, func(t *testing.T) { checkJSON(t, want, got) })
			}
		})
	}
}

func TestExactUsage(t *testing.T) {
	tests := map[string]struct {
		raw, want map[string]any
		subset    bool
	}{
		"success: cache thinking and server tools": {raw: map[string]any{"input_tokens": 9, "output_tokens": 7, "cache_read_input_tokens": 5, "cache_creation_input_tokens": 3, "cache_creation": map[string]any{"ephemeral_5m_input_tokens": 1, "ephemeral_1h_input_tokens": 2}, "output_tokens_details": map[string]any{"reasoning_tokens": 4}, "server_tool_use": map[string]any{"web_search_requests": 2, "web_fetch_requests": 1, "code_execution_requests": 3}}, want: map[string]any{"input_tokens": 9, "output_tokens": 7, "cache_read_tokens": 5, "cache_creation_tokens": 3, "cache_creation_5m_tokens": 1, "cache_creation_1h_tokens": 2, "thinking_tokens": 4, "server_tool_use_count": 6, "web_search_requests": 2, "web_fetch_requests": 1, "code_execution_requests": 3, "tool_search_requests": nil}},
		"success: zero writes":                     {raw: map[string]any{"cache_creation_input_tokens": 0}, want: map[string]any{"cache_creation_5m_tokens": 0, "cache_creation_1h_tokens": 0}, subset: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := exactUsage(tt.raw)
			if !tt.subset {
				checkJSON(t, tt.want, got)
				return
			}
			for k, want := range tt.want {
				checkJSON(t, want, got[k])
			}
		})
	}
}

func TestMergeUsage(t *testing.T) {
	start := map[string]any{"input_tokens": 100, "cache_creation": map[string]any{"ephemeral_5m_input_tokens": 10}}
	end := map[string]any{"output_tokens": 20, "input_tokens": 101, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": 4}}
	checkJSON(t, map[string]any{"input_tokens": 101, "output_tokens": 20, "cache_creation": map[string]any{"ephemeral_5m_input_tokens": 10, "ephemeral_1h_input_tokens": 4}}, MergeUsage(start, end))
}

func TestLimitEvidence(t *testing.T) {
	h := http.Header{}
	for k, v := range map[string]string{"anthropic-ratelimit-input-tokens-limit": "100000", "anthropic-ratelimit-input-tokens-remaining": "25000", "anthropic-ratelimit-input-tokens-reset": "2026-07-19T20:00:00Z", "retry-after": "2.5", "x-codex-primary-used-percent": "72%", "x-codex-secondary-used-percent": "64", "x-private-header": "private value"} {
		h.Set(k, v)
	}
	r := onlyEvent(t, NewBuilder(options(t)).Build(Request{Body: wire(t, `{}`), StatusCode: 429, ResponseHeaders: h}), "gateway.limit.signal")
	for k, want := range map[string]int64{"rate_limit_input_tokens_limit": 100000, "rate_limit_input_tokens_remaining": 25000, "rate_limit_input_tokens_reset_at_ms": 1784491200000, "retry_after_ms": 2500, "codex_throttle_used_percent": 72} {
		t.Run(k, func(t *testing.T) { checkJSON(t, want, r.Attributes[k]) })
	}
	if bytes.Contains(encoded(t, r), []byte("private")) {
		t.Error("private header in limit record")
	}
}

func TestUsageEndpoint(t *testing.T) {
	tests := map[string]struct{ gateway, logs, base, want string }{
		"success: default":                {want: "http://127.0.0.1:4318/v1/logs"},
		"success: generic base":           {base: "http://localhost:4318/base/", want: "http://127.0.0.1:4318/base/v1/logs"},
		"success: logs overrides base":    {logs: "http://localhost:4321/log", base: "http://localhost:4318/base", want: "http://127.0.0.1:4321/log"},
		"success: gateway overrides logs": {gateway: "http://127.0.0.1:1234/custom", logs: "http://localhost:4321/log", want: "http://127.0.0.1:1234/custom"},
		"success: disabled":               {gateway: "0", logs: "http://localhost:4321/log"},
		"error: external":                 {gateway: "https://telemetry.example.com/v1/logs", base: "http://localhost:4318/base"},
		"error: invalid IPv4":             {gateway: "http://127.999.999.999/v1/logs"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{"CODEX_GATEWAY_USAGE_ENDPOINT": tt.gateway, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": tt.logs, "OTEL_EXPORTER_OTLP_ENDPOINT": tt.base}
			endpoint := config.UsageURL(func(k string) (string, bool) { v := values[k]; return v, v != "" })
			var got string
			if endpoint != nil {
				got = endpoint.String()
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("URL (-want +got):\n%s", diff)
			}
		})
	}
}
