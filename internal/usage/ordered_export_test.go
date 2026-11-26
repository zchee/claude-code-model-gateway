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
	"net/http"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestOrderedExportBytes(t *testing.T) {
	// lib/usage-observability.js:641-671: the only variable bytes in this envelope
	// are the event name and its ordered attribute array. Compare bytes, not parsed JSON.
	envelope := func(event string, fields []string) string {
		return `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"codex-gateway"}}]},"scopeLogs":[{"scope":{"name":"eigenwise.codex-gateway.usage","version":"1"},"logRecords":[{"timeUnixNano":"123000000000","observedTimeUnixNano":"123000000000","eventName":"` + event + `","body":{"stringValue":"` + event + `"},"attributes":[` + strings.Join(fields, ",") + `]}]}]}]}`
	}
	s := func(k, v string) string { return `{"key":"` + k + `","value":{"stringValue":"` + v + `"}}` }
	n := func(k, v string) string { return `{"key":"` + k + `","value":{"intValue":"` + v + `"}}` }
	const primary = "gateway.token.usage"
	const derived = "gateway.mcp.footprint"
	const id = "00000000-0000-4000-8000-000000000000"
	tests := map[string]struct {
		index  int
		event  string
		fields []string
	}{
		"success: primary includes ranked server keys between tool and system measurements": {index: 0, event: primary, fields: []string{
			// lib/usage-observability.js:835-864: source identity, request identity, status, quality and sequence.
			s("source", "codex_gateway"), s("source_event_id", "gateway-usage-"+id), s("source_schema", "gateway-usage-v1"), s("event.name", primary), s("event_name", primary),
			s("request_id", "req"), s("request_id_source", "response_header"), s("agent_role", "orchestrator"), s("status", "ok"), n("status_code", "200"), s("response_mode", "sse"), s("token_estimator", "utf8_bytes_div_4_normalized"), s("cache_attribution", "prefix_order_estimate"), n("request_sequence", "1"),
			// lib/usage-observability.js:825-832,569-594: elapsed time, response bytes, then available limits.
			n("duration_ms", "0"), n("response_body_bytes", "0"), n("rate_limit_requests_limit", "9"), n("retry_after_ms", "1000"),
			// lib/usage-observability.js:247-257,455-468: composition precedes exact usage; null measurements are omitted.
			n("request_body_bytes", "0"), n("input_system_bytes", "0"), n("input_tools_bytes", "44"), n("input_native_tools_bytes", "0"), n("input_mcp_tools_bytes", "41"), n("input_messages_bytes", "2"), n("input_first_message_bytes", "0"), n("input_history_bytes", "0"), n("input_tool_results_bytes", "0"), n("input_tokens", "41"),
			// lib/usage-observability.js:514-532: all server weights participate, but named fields are emitted by descending bytes.
			n("context_tokens", "41"), n("input_tools_tokens", "41"), n("input_native_tools_tokens", "0"), n("input_mcp_tools_tokens", "41"), n("input_mcp_tools_aa_tokens", "21"), n("input_mcp_tools_z_tokens", "20"),
			n("input_system_tokens", "0"), n("input_first_message_tokens", "0"), n("input_history_tokens", "0"), n("input_tool_results_tokens", "0"),
			n("cache_read_tools_tokens", "0"), n("cache_read_system_tokens", "0"), n("cache_read_first_message_tokens", "0"), n("cache_read_history_tokens", "0"),
			n("fresh_tools_tokens", "41"), n("fresh_system_tokens", "0"), n("fresh_first_message_tokens", "0"), n("fresh_history_tokens", "0"),
		}},
		"success: derived uses shared identity followed by the server and allocation": {index: 1, event: derived, fields: []string{
			// lib/usage-observability.js:942-961: derived identity and copied request metadata omit request_id_source.
			s("source", "codex_gateway"), s("source_event_id", "gateway-mcp-footprint-"+id), s("source_schema", "gateway-usage-v1"), s("event.name", derived), s("event_name", derived), s("request_id", "req"), s("agent_role", "orchestrator"), n("request_sequence", "1"),
			// lib/usage-observability.js:962-963: footprint-specific fields are last.
			s("mcp_server", "aa"), n("input_mcp_tools_tokens", "21"),
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := Request{Body: wire(t, `{"tools":[{"name":"mcp__z__t"},{"name":"mcp__aa__t"}]}`), StatusCode: 200, ResponseMode: "sse", ResponseHeaders: http.Header{"Request-Id": {"req"}, "Retry-After": {"1"}, "Anthropic-Ratelimit-Requests-Limit": {"9"}}, WireUsage: wire(t, `{"input_tokens":41}`)}
			records := NewBuilder(options(t)).Build(req)
			if len(records) != 3 {
				t.Fatalf("got %d records, want primary and two footprints", len(records))
			}
			got, err := records[tt.index].OTLP()
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(envelope(tt.event, tt.fields), string(got)); diff != "" {
				t.Error(diff)
			}
		})
	}
}
