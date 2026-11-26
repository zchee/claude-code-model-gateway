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
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Read actual OTLP attributes, not the pre-serialization Go map.
func logFields(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var body struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []struct {
						Key   string
						Value map[string]any
					}
				}
			}
		}
	}
	if err := json.Unmarshal(payload, &body, json.MatchCaseInsensitiveNames(true)); err != nil {
		t.Fatal(err)
	}
	if len(body.ResourceLogs) != 1 || len(body.ResourceLogs[0].ScopeLogs) != 1 || len(body.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("invalid OTLP log envelope: %s", payload)
	}
	fields := map[string]any{}
	for _, attr := range body.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes {
		if len(attr.Value) != 1 {
			t.Fatalf("invalid attribute %q: %v", attr.Key, attr.Value)
		}
		for typ, v := range attr.Value {
			if typ == "intValue" {
				s, ok := v.(string)
				if !ok {
					t.Fatalf("intValue %v not a string", v)
				}
				n, err := strconv.Atoi(s)
				if err != nil {
					t.Fatal(err)
				}
				v = n
			}
			fields[attr.Key] = v
		}
	}
	return fields
}

func captureLog(t *testing.T, r *Record) ([]byte, map[string]any) {
	t.Helper()
	if r == nil {
		t.Fatal("capture returned no usage record")
	}
	b, err := r.OTLP()
	if err != nil {
		t.Fatal(err)
	}
	return b, logFields(t, b)
}

func TestJSONCapture(t *testing.T) {
	tests := map[string]struct{ via, ticket string }{"success: originating dispatch": {"dispatch", "SQ-1234"}, "success: dispatch without marker": {"dispatch", ""}, "success: inherited route": {"dispatch-inherited", ""}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := Request{Body: wire(t, compositionBody), BodyBytes: int64(len(compositionBody)), Headers: http.Header{"X-Claude-Code-Session-Id": {"session-1"}, "X-Claude-Code-Agent-Id": {"agent-1"}, "X-Claude-Code-Parent-Agent-Id": {"parent-1"}, "X-Request-Id": {"client-request-1"}, "Authorization": {"Bearer private-credential"}, "X-Private-Header": {"private-header-value"}}, RequestedModel: "claude-codex-auto", EffectiveModel: "gpt-5.6-terra", Backend: "codex", Effort: "xhigh", Via: tt.via, TicketRef: tt.ticket, Sequence: new(uint64(3))}
			var emitted []Record
			c := NewCapture(NewBuilder(options(t)), req, 0, func(r Record) { emitted = append(emitted, r) })
			c.SetResponse(200, http.Header{"Request-Id": {"provider-request-1"}, "Anthropic-Ratelimit-Input-Tokens-Remaining": {"999"}})
			c.ObserveJSON([]byte(`{"id":"msg-private-id","model":"virtual-wrong-model","content":[{"type":"text","text":"private response content"}],"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":20,"cache_creation_input_tokens":5}}`))
			r := c.Finish()
			raw, flat := captureLog(t, r)
			delivered := onlyEvent(t, emitted, "gateway.token.usage")
			checkJSON(t, r, delivered)
			for k, want := range map[string]any{"request_id": "provider-request-1", "session_id": "session-1", "agent_id": "agent-1", "parent_agent_id": "parent-1", "model": "gpt-5.6-terra", "requested_model": "claude-codex-auto", "context_tokens": 35, "rate_limit_input_tokens_remaining": 999, "request_sequence": 3, "input_tokens": 10, "output_tokens": 4, "event.name": "gateway.token.usage", "event_name": "gateway.token.usage"} {
				checkJSON(t, want, flat[k])
			}
			if tt.ticket == "" {
				v, ok := r.Attributes["ticket_ref"]
				if !ok || v != nil {
					t.Errorf("missing marker: got (%v,%v), want present null", v, ok)
				}
			} else {
				checkJSON(t, tt.ticket, r.Attributes["ticket_ref"])
			}
			if !regexp.MustCompile(`^(claude_code|agent_sdk|gateway)\.|^(mcp_server_connection|hook_execution_(start|complete))$`).MatchString(fmt.Sprint(flat["event.name"])) {
				t.Error("collector filter did not match event.name")
			}
			native, nok := flat["input_native_tools_tokens"].(int)
			mcp, mok := flat["input_mcp_tools_tokens"].(int)
			if !nok || !mok {
				t.Fatal("missing OTLP integer tool measurements")
			}
			checkJSON(t, native+mcp, flat["input_tools_tokens"])
			for _, secret := range []string{"private system", "private native schema", "private MCP schema", "private first message", "private result", "private response content", "private-credential", "private-header-value", "authorization", "x-private-header"} {
				if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(secret)) {
					t.Errorf("OTLP contains %q", secret)
				}
			}
		})
	}
}

func TestCaptureToolMeasurements(t *testing.T) {
	tests := map[string]struct{ many bool }{"success: normalized server names": {}, "success: bounded server count": {true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := `{"system":"system","tools":[{"name":"Read","description":"native schema"},{"name":"mcp__sidequest__claim","description":"sidequest claim schema"},{"name":"mcp__sidequest__done","description":"sidequest done schema"},{"name":"mcp__Browser-Tools__click","description":"browser click schema"}],"messages":[{"role":"user","content":"hello"}]}`
			if tt.many {
				var tools []map[string]string
				for i := range 25 {
					tools = append(tools, map[string]string{"name": fmt.Sprintf("mcp__server_%d__tool", i), "description": strings.Repeat("schema", i+1)})
				}
				body = string(encoded(t, map[string]any{"tools": tools}))
			}
			c := NewCapture(NewBuilder(options(t)), Request{Body: wire(t, body)}, 0, nil)
			c.SetResponse(200, nil)
			c.ObserveJSON([]byte(`{"usage":{"input_tokens":1000,"output_tokens":1}}`))
			_, flat := captureLog(t, c.Finish())
			if tt.many {
				n := 0
				for k := range flat {
					if strings.HasPrefix(k, "input_mcp_tools_server_") && strings.HasSuffix(k, "_tokens") {
						n++
					}
				}
				checkJSON(t, 20, n)
				return
			}
			sum := 0
			for _, k := range []string{"input_mcp_tools_sidequest_tokens", "input_mcp_tools_browser_tools_tokens", "input_native_tools_tokens"} {
				v, ok := flat[k].(int)
				if !ok || v <= 0 {
					t.Fatalf("%s = %v, want positive OTLP integer", k, flat[k])
				}
				if k != "input_native_tools_tokens" {
					sum += v
				}
			}
			checkJSON(t, sum, flat["input_mcp_tools_tokens"])
			if len(flat) >= 128 {
				t.Errorf("%d attributes, want fewer than 128", len(flat))
			}
		})
	}
}

func TestCaptureStream(t *testing.T) {
	tests := map[string]struct {
		backend, start, delta string
		blocks                bool
		want                  map[string]any
	}{
		"success: Anthropic cumulative deltas":    {"anthropic", `{"id":"msg-1","model":"claude-opus-4-8","usage":{"input_tokens":3,"cache_read_input_tokens":7,"cache_creation_input_tokens":0,"output_tokens":0}}`, `{"output_tokens":9,"thinking_tokens":2}`, false, map[string]any{"request_id": "msg-1", "response_mode": "sse", "input_tokens": 3, "output_tokens": 9, "thinking_tokens": 2, "response_body_bytes": 100}},
		"success: Codex multi-block final counts": {"codex", `{"id":"msg-blocks","model":"gpt-5.6-terra","usage":{"input_tokens":0,"output_tokens":0}}`, `{"input_tokens":64,"output_tokens":27,"cache_read_input_tokens":120000,"cache_creation_input_tokens":40}`, true, map[string]any{"input_tokens": 64, "output_tokens": 27, "cache_read_tokens": 120000, "cache_creation_tokens": 40, "context_tokens": 120104, "request_sequence": nil}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var emitted []Record
			c := NewCapture(NewBuilder(options(t)), Request{Body: wire(t, compositionBody), Backend: tt.backend}, 0, func(r Record) { emitted = append(emitted, r) })
			c.SetResponse(200, nil)
			c.NoteResponseBytes(100)
			c.ObserveEvent(wire(t, `{"type":"message_start","message":`+tt.start+`}`))
			kinds := []string{"text"}
			if tt.blocks {
				kinds = []string{"thinking", "thinking", "text", "tool_use"}
			}
			for i, kind := range kinds {
				c.ObserveEvent(wire(t, fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":%q}}`, i, kind)))
				c.ObserveEvent(wire(t, fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"text":"private streamed content"}}`, i)))
				c.ObserveEvent(wire(t, fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i)))
			}
			c.ObserveEvent(wire(t, `{"type":"message_delta","usage":`+tt.delta+`}`))
			c.ObserveEvent(wire(t, `{"type":"message_stop"}`))
			c.Finish()
			if len(emitted) != 1 {
				t.Fatalf("got %d records, want one per request", len(emitted))
			}
			r := emitted[0]
			for k, want := range tt.want {
				v, ok := r.Attributes[k]
				if !ok {
					t.Errorf("missing %s", k)
				}
				checkJSON(t, want, v)
			}
			if bytes.Contains(encoded(t, r), []byte("private streamed content")) {
				t.Error("raw stream content retained in emitted record")
			}
			if c.Finish() != nil || len(emitted) != 1 {
				t.Error("second Finish returned or emitted a record")
			}
		})
	}
}

func TestBoundedCapture(t *testing.T) {
	tests := map[string]struct {
		status                  int
		split, incomplete, mark bool
		want                    string
	}{"success: at byte ceiling": {200, true, false, false, "gateway.token.usage"}, "success: overflow suppresses usage": {200, true, false, false, ""}, "success: incomplete suppresses usage": {200, false, true, false, ""}, "success: explicit overflow suppresses usage": {200, false, false, true, ""}, "success: failed response reports limits only": {429, false, false, false, "gateway.limit.signal"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var emitted []Record
			c := NewCapture(NewBuilder(options(t)), Request{Body: wire(t, compositionBody)}, 1024, func(r Record) { emitted = append(emitted, r) })
			c.SetResponse(tt.status, http.Header{"Retry-After": {"3"}, "X-Codex-Primary-Used-Percent": {"99"}})
			body := []byte(`{"usage":{"input_tokens":15}}`)
			if tt.split {
				n := 1024
				if tt.want == "" {
					n = 1600
				}
				body = append(body, bytes.Repeat([]byte(" "), n-len(body))...)
				c.ObserveChunk(body[:800])
				c.ObserveChunk(body[800:])
			} else {
				c.ObserveJSON(body)
			}
			if tt.mark {
				c.MarkOverflow()
			}
			c.SetResponseComplete(!tt.incomplete)
			r := c.Finish()
			if tt.want == "" {
				if r != nil || len(emitted) != 0 {
					t.Error("suppressed usage emitted")
				}
				return
			}
			if r == nil || r.EventName != tt.want || len(emitted) != 1 {
				t.Fatalf("got %v and %d emitted, want one %s", r, len(emitted), tt.want)
			}
			if tt.status == 429 {
				checkJSON(t, 3000, r.Attributes["retry_after_ms"])
				checkJSON(t, 99, r.Attributes["codex_throttle_used_percent"])
				if _, ok := r.Attributes["input_tokens"]; ok {
					t.Error("error record included successful usage")
				}
			}
		})
	}
}

func TestCaptureRetrySnapshot(t *testing.T) {
	c := NewCapture(NewBuilder(options(t)), Request{}, 0, nil)
	c.SetResponse(200, nil)
	c.ObserveEvent(wire(t, `{"type":"message_start","message":{"id":"old","usage":{"input_tokens":12}}}`))
	checkJSON(t, 12, c.Snapshot()["input_tokens"])
	c.ResetUsage()
	checkJSON(t, nil, c.Snapshot()["input_tokens"])
	c.ObserveJSON([]byte(`{"id":"new","usage":{"output_tokens":3}}`))
	_, flat := captureLog(t, c.Finish())
	checkJSON(t, "new", flat["request_id"])
	checkJSON(t, "json", flat["response_mode"])
	if _, ok := flat["input_tokens"]; ok {
		t.Error("old usage survived retry")
	}
}
