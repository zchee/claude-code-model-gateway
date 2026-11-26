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
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

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

func TestEmitterToolResultDelivery(t *testing.T) {
	var mu sync.Mutex
	var payloads [][]byte
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		payloads = append(payloads, body)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer s.Close()
	e := NewEmitter(func(k string) (string, bool) { return s.URL + "/v1/logs", k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, NewBuilder(options(t)))
	for _, turn := range []struct{ body, response string }{
		{exactBase, `{"id":"first","usage":{"input_tokens":100,"output_tokens":1}}`},
		{exactNext, `{"id":"second","usage":{"input_tokens":137,"output_tokens":1}}`},
	} {
		c := e.Start(Request{Body: wire(t, turn.body), BodyBytes: int64(len(turn.body)), Headers: http.Header{"X-Claude-Code-Session-Id": {"session-tool-result"}, "X-Claude-Code-Agent-Id": {"agent-tool-result"}}, TicketRef: "SQ-1234"})
		c.SetResponse(http.StatusOK, nil)
		c.ObserveJSON([]byte(turn.response))
		if c.Finish() == nil {
			t.Error("capture returned no primary record")
		}
	}
	e.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 3 {
		t.Fatalf("got %d log posts, want two primary and one tool-result record", len(payloads))
	}
	counts := map[string]int{}
	for _, payload := range payloads {
		if bytes.Contains(payload, []byte("private exact result")) {
			t.Error("collector received tool result content")
		}
		attrs := logFields(t, payload)
		event, _ := attrs["event.name"].(string)
		counts[event]++
		if event == toolEvent {
			for k, want := range map[string]any{
				"event_name": toolEvent, "request_id": "second", "session_id": "session-tool-result", "agent_id": "agent-tool-result", "request_sequence": 2, "ticket_ref": "SQ-1234",
				"tool_use_id": "tool-exact", "tool_name": "mcp__sidequest__claim", "tool_result_tokens": 37, "tool_result_tokens_unit": "tokens", "tool_result_tokens_quality": "derived_exact",
			} {
				checkJSON(t, want, attrs[k])
			}
		}
	}
	checkJSON(t, map[string]int{"gateway.token.usage": 2, toolEvent: 1}, counts)
}

func TestEmitterResponseBound(t *testing.T) {
	tests := map[string]struct {
		bytes, wantPosts int
	}{
		"success: response at configured bound":              {bytes: 1024, wantPosts: 1},
		"success: response over configured bound is dropped": {bytes: 1025},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var posts int
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				posts++
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer s.Close()
			env := map[string]string{"CODEX_GATEWAY_USAGE_ENDPOINT": s.URL + "/v1/logs", "CODEX_GATEWAY_USAGE_MAX_RESPONSE_BYTES": "1024"}
			e := NewEmitter(func(k string) (string, bool) { v, ok := env[k]; return v, ok }, NewBuilder(options(t)))
			body := []byte(`{"usage":{"input_tokens":15}}`)
			body = append(body, bytes.Repeat([]byte(" "), tt.bytes-len(body))...)
			c := e.Start(Request{})
			c.SetResponse(http.StatusOK, nil)
			c.ObserveChunk(body[:800])
			c.ObserveChunk(body[800:])
			checkJSON(t, tt.wantPosts != 0, c.Finish() != nil)
			e.Wait()
			mu.Lock()
			defer mu.Unlock()
			checkJSON(t, tt.wantPosts, posts)
		})
	}
}

func TestDisabledEmitterSnapshot(t *testing.T) {
	e := NewEmitter(func(k string) (string, bool) { return "0", k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, NewBuilder(options(t)))
	if e.Enabled() {
		t.Fatal("usage exporter is enabled despite endpoint=0")
	}
	capture := e.Start(Request{Backend: "anthropic"})
	if capture == nil {
		t.Fatal("disabled emitter returned no capture")
	}
	capture.SetResponse(http.StatusOK, nil)
	capture.ObserveEvent(wire(t, `{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":1}}}`))
	capture.ObserveEvent(wire(t, `{"type":"message_delta","usage":{"output_tokens":3}}`))
	checkJSON(t, 12, capture.Snapshot()["input_tokens"])
	checkJSON(t, 3, capture.Snapshot()["output_tokens"])
	if capture.Finish() == nil {
		t.Error("disabled emitter did not produce a completed capture")
	}
	e.Wait()
}

func TestEmitterExportFailure(t *testing.T) {
	tests := map[string]struct{ stalls bool }{
		"error: collector never replies":          {stalls: true},
		"error: collector refuses TCP connection": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			// The emitter has no diagnostic sink; capture process output in this serial test.
			path := filepath.Join(t.TempDir(), "collector-output")
			output, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := output.Close(); err != nil {
					t.Error(err)
				}
			}()
			stdout, stderr, logOutput := os.Stdout, os.Stderr, log.Writer()
			os.Stdout, os.Stderr = output, output
			log.SetOutput(output)
			defer func() {
				os.Stdout, os.Stderr = stdout, stderr
				log.SetOutput(logOutput)
			}()
			builder := NewBuilder(options(t))
			export := func(t *testing.T, endpoint string, dial func(context.Context, string, string) (net.Conn, error)) {
				e := newEmitter(func(k string) (string, bool) { return endpoint, k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, builder, dial)
				c := e.Start(Request{})
				c.SetResponse(http.StatusOK, nil)
				c.ObserveJSON([]byte(`{"usage":{"input_tokens":15}}`))
				if c.Finish() == nil {
					t.Error("collector failure suppressed the primary usage record")
				}
				e.Wait()
			}
			if tt.stalls {
				synctest.Test(t, func(t *testing.T) {
					peer, client := net.Pipe()
					defer func() {
						_ = peer.Close()
						_ = client.Close()
					}()
					// Consume the HTTP request but never send a reply. The client must close the connection.
					read := make(chan int64, 1)
					go func() {
						n, _ := io.Copy(io.Discard, peer)
						read <- n
					}()
					start := time.Now()
					done := make(chan time.Duration, 1)
					go func() {
						export(t, "http://127.0.0.1:1/v1/logs", func(context.Context, string, string) (net.Conn, error) { return client, nil })
						done <- time.Since(start)
					}()
					select {
					case elapsed := <-done:
						if elapsed != 500*time.Millisecond {
							t.Errorf("Wait returned at %s, want 500ms", elapsed)
						}
					case <-time.After(501 * time.Millisecond):
						t.Error("Wait did not return after the 500ms collector timeout")
					}
					synctest.Wait()
					select {
					case n := <-read:
						if n == 0 {
							t.Error("collector received no request")
						}
					default:
						t.Error("collector connection was not closed")
					}
				})
			} else {
				l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				endpoint := "http://" + l.Addr().String() + "/v1/logs"
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
				var dialer net.Dialer
				dialResult := make(chan error, 1)
				export(t, endpoint, func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := dialer.DialContext(ctx, network, address)
					select {
					case dialResult <- err:
					default:
					}
					return conn, err
				})
				select {
				case err := <-dialResult:
					if !errors.Is(err, syscall.ECONNREFUSED) {
						t.Errorf("collector dial = %v, want connection refused", err)
					}
				default:
					t.Error("emitter never dialed collector")
				}
			}
			if raw, err := os.ReadFile(path); err != nil {
				t.Fatal(err)
			} else if len(raw) != 0 {
				t.Errorf("export failure wrote diagnostic: %q", raw)
			}
		})
	}
}

func encodedHeader(h http.Header) []byte { var b bytes.Buffer; _ = h.Write(&b); return b.Bytes() }
