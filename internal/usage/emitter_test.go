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
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestEmitterEnabled(t *testing.T) {
	tests := map[string]struct {
		endpoint string
		enabled  bool
	}{"success: loopback": {"http://127.0.0.1:1234/custom", true}, "error: external": {"https://telemetry.example.com/v1/logs", false}, "success: explicitly disabled": {"0", false}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			e := NewEmitter(func(k string) (string, bool) { return tt.endpoint, k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, NewBuilder(options(t)))
			if got := e.Enabled(); got != tt.enabled {
				t.Errorf("Enabled()=%v, want %v", got, tt.enabled)
			}
		})
	}
}

func TestEmitterDelivery(t *testing.T) {
	tests := map[string]struct{ status int }{"success: collector accepts records": {204}, "success: collector refuses records": {503}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			var mu sync.Mutex
			var payloads [][]byte
			finished := make(chan struct{})
			release := sync.OnceFunc(func() { close(finished) })
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.RequestURI() != "/v1/logs" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected export: %s %s %v", r.Method, r.URL, r.Header)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if int64(len(body)) != r.ContentLength {
					t.Error("missing or incorrect content length")
				}
				for _, secret := range []string{"private", "Bearer", "sentinel"} {
					if bytes.Contains(body, []byte(secret)) || bytes.Contains(encodedHeader(r.Header), []byte(secret)) {
						t.Errorf("collector received %q", secret)
					}
				}
				mu.Lock()
				payloads = append(payloads, body)
				mu.Unlock()
				select {
				case <-finished:
				case <-r.Context().Done():
					t.Error("collector request expired before inference finished")
				}
				w.WriteHeader(tt.status)
			}))
			defer s.Close()
			defer release()
			env := map[string]string{"CODEX_GATEWAY_USAGE_ENDPOINT": s.URL + "/v1/logs?private=sentinel#ignored"}
			e := NewEmitter(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, NewBuilder(options(t)))
			body := wire(t, `{"tools":[{"name":"mcp__alpha__one","description":"private alpha schema"},{"name":"mcp__beta__two","description":"private beta schema"}]}`)
			first := e.Start(Request{Body: body, Headers: http.Header{"X-Claude-Code-Session-Id": {"session-footprint"}, "X-Claude-Code-Agent-Id": {"agent-footprint"}, "Authorization": {"Bearer sentinel"}}, TicketRef: "SQ-1234"})
			second := e.Start(Request{})
			second.SetResponse(200, nil)
			second.ObserveJSON([]byte(`{"id":"second","usage":{"output_tokens":1}}`))
			r2 := second.Finish()
			first.SetResponse(200, nil)
			first.ObserveJSON([]byte(`{"usage":{"input_tokens":1000,"output_tokens":1},"content":"private result"}`))
			r1 := first.Finish()
			release()
			e.Wait()
			if r1 == nil || r2 == nil {
				t.Fatal("collector outcome must not suppress completed records")
			}
			checkJSON(t, 1, r1.Attributes["request_sequence"])
			checkJSON(t, 2, r2.Attributes["request_sequence"])
			mu.Lock()
			defer mu.Unlock()
			if len(payloads) != 4 {
				t.Fatalf("got %d log posts, want two primary and two footprint records", len(payloads))
			}
			var total any
			var sum int
			servers := map[string]int{}
			for _, p := range payloads {
				attrs := logFields(t, p)
				switch attrs["event.name"] {
				case "gateway.mcp.footprint":
					name, ok := attrs["mcp_server"].(string)
					n, valid := attrs["input_mcp_tools_tokens"].(int)
					if !ok || !valid {
						t.Fatalf("invalid MCP footprint: %v", attrs)
					}
					servers[name]++
					sum += n
					checkJSON(t, "session-footprint", attrs["session_id"])
					checkJSON(t, "SQ-1234", attrs["ticket_ref"])
				case "gateway.token.usage":
					if attrs["session_id"] == "session-footprint" {
						total = attrs["input_mcp_tools_tokens"]
					}
				default:
					t.Fatalf("unexpected event: %v", attrs["event.name"])
				}
			}
			checkJSON(t, map[string]int{"alpha": 1, "beta": 1}, servers)
			checkJSON(t, sum, total)
		})
	}
}

func encodedHeader(h http.Header) []byte { var b bytes.Buffer; _ = h.Write(&b); return b.Bytes() }
