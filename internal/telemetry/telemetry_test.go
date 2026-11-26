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

package telemetry

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) { venue.Require(); goleak.VerifyTestMain(m) }

func recorderOptions(env map[string]string) Options {
	return Options{Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Now: func() time.Time { return time.Unix(1, 0) }, Random: bytes.NewReader(bytes.Repeat([]byte{0x11}, 256))}
}

func TestRequestRouteLog(t *testing.T) {
	tests := map[string]struct{ disabled, broken bool }{"success: default metadata only": {}, "success: disabled creates no file": {disabled: true}, "error: log write is diagnostic only": {broken: true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			if tt.disabled {
				env["CODEX_GATEWAY_REQUEST_LOG"] = "0"
			}
			opt := recorderOptions(env)
			root := t.TempDir()
			opt.LogPath = filepath.Join(root, "logs", "request-routes.jsonl")
			if tt.broken {
				opt.LogPath = root
			}
			var diag bytes.Buffer
			opt.Diagnostics = &diag
			r := NewRecorder(opt)
			req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/messages?key=query-sentinel", strings.NewReader(`{"messages":["prompt-sentinel"]}`))
			req.Header.Set("X-Claude-Code-Session-Id", "header-session")
			req.Header.Set("Authorization", "Bearer token-sentinel")
			req.Header.Set("X-Private", "header-sentinel")
			r.LogRoute(req, LogEntry{Backend: "anthropic", Model: new("claude-fable-5")})
			r.LogRoute(req, LogEntry{Backend: "codex", Via: "dispatch-inherited", Effort: "xhigh", SessionID: "dispatch-session", AgentID: "child", ParentAgentID: "parent", InheritedFromAgentID: "parent", SessionSource: "metadata", MarkersLength: new(0)})
			if tt.broken {
				if !strings.Contains(diag.String(), "model-gateway: could not write request route log:") || strings.Contains(diag.String(), "sentinel") {
					t.Errorf("unexpected diagnostic: %q", diag.String())
				}
				return
			}
			raw, err := os.ReadFile(opt.LogPath)
			if tt.disabled {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("disabled logger created file: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diag.Len() != 0 {
				t.Errorf("unexpected diagnostic: %s", &diag)
			}
			if !bytes.HasSuffix(raw, []byte("\n")) {
				t.Fatal("last record not newline terminated")
			}
			lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			want := []map[string]any{{"at": "1970-01-01T00:00:01.000Z", "backend": "anthropic", "model": "claude-fable-5", "path": "/v1/messages", "sessionId": "header-session"}, {"at": "1970-01-01T00:00:01.000Z", "backend": "codex", "model": nil, "path": "/v1/messages", "sessionId": "dispatch-session", "agentId": "child", "parentAgentId": "parent", "inheritedFromAgentId": "parent", "sessionSource": "metadata", "markersLength": float64(0), "effort": "xhigh", "via": "dispatch-inherited"}}
			var got []map[string]any
			for _, line := range lines {
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatal(err)
				}
				got = append(got, row)
			}
			if d := gocmp.Diff(want, got); d != "" {
				t.Errorf("audit schema (-want +got):\n%s", d)
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(opt.LogPath)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Errorf("mode=%o, want 0600", info.Mode().Perm())
				}
			}
		})
	}
}

func TestTraceDelivery(t *testing.T) {
	const trace = "4bf92f3577b34da6a3ce929d0e0e4736"
	const parent = "00f067aa0ba902b7"
	const fresh = "1111111111111111"
	const routeID = "11111111-1111-4111-9111-111111111111"
	tests := map[string]struct {
		traceparent, override, status, endpoint string
		code, flags                             int
		linked, enabled, compact, overflow      bool
		filtered, retried                       bool
	}{
		"success: linked route":                            {traceparent: "00-" + trace + "-" + parent + "-01", status: "ok", code: 201, flags: 1, linked: true, enabled: true},
		"success: future version with suffix":              {traceparent: "01-" + trace + "-" + parent + "-01-ab", status: "ok", code: 202, flags: 1, linked: true, enabled: true},
		"error: invalid current version suffix":            {traceparent: "00-" + trace + "-" + parent + "-01-ab", status: "client_error", code: 413, enabled: true, overflow: true},
		"error: forbidden version":                         {traceparent: "ff-" + trace + "-" + parent + "-01", status: "server_error", code: 503, enabled: true},
		"error: zero parent":                               {traceparent: "00-" + trace + "-0000000000000000-01", override: "upstream_error", status: "upstream_error", code: 200, enabled: true, compact: true},
		"error: zero trace":                                {traceparent: "00-00000000000000000000000000000000-" + parent + "-01", status: "upstream_error", enabled: true},
		"success: tracing disabled":                        {},
		"success: disabled endpoint overrides propagation": {traceparent: "00-" + trace + "-" + parent + "-01", endpoint: "0"},
		"error: external endpoint overrides propagation":   {traceparent: "00-" + trace + "-" + parent + "-01", endpoint: "https://collector.example.invalid/v1/traces"},
		"success: linked zero flags and filtered metadata": {traceparent: "00-" + trace + "-" + parent + "-00", status: "ok", code: 200, linked: true, enabled: true, filtered: true},
		"success: terminal compaction result after retry":  {status: "ok", code: 200, enabled: true, retried: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies [][]byte
			finished := make(chan struct{})
			release := sync.OnceFunc(func() { close(finished) })
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RequestURI() != "/v1/traces" || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected collector request: %s %s %v", r.Method, r.URL, r.Header)
				}
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if int64(len(b)) != r.ContentLength {
					t.Error("wrong content length")
				}
				for _, key := range []string{"Authorization", "X-Api-Key", "Cookie", "Traceparent", "Tracestate", "Baggage", "X-Private"} {
					if _, ok := r.Header[key]; ok {
						t.Errorf("collector received %s", key)
					}
				}
				if bytes.Contains(b, []byte("sentinel")) {
					t.Error("collector received private data")
				}
				mu.Lock()
				bodies = append(bodies, b)
				mu.Unlock()
				select {
				case <-finished:
				case <-r.Context().Done():
					t.Error("collector timed out before Finish returned")
				}
				w.WriteHeader(503)
			}))
			defer s.Close()
			defer release()
			env := map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": s.URL + "/v1/traces?query-sentinel#secret"}
			if tt.enabled || tt.endpoint != "" {
				env["CLAUDE_CODE_PROPAGATE_TRACEPARENT"] = "1"
			}
			if tt.endpoint != "" {
				env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = s.URL + "/v1/traces"
				env["CODEX_GATEWAY_TELEMETRY_ENDPOINT"] = tt.endpoint
			}
			opts := recorderOptions(env)
			var diagnostics bytes.Buffer
			opts.Diagnostics = &diagnostics
			instant := time.Unix(1, 0)
			opts.Now = func() time.Time { return instant }
			r := NewRecorder(opts)
			if got := r.Enabled(); got != tt.enabled {
				t.Fatalf("Enabled()=%v, want %v", got, tt.enabled)
			}
			h := http.Header{}
			h.Set("Traceparent", tt.traceparent)
			h.Set("X-Claude-Code-Session-Id", "session-telemetry-1")
			for _, k := range []string{"Authorization", "X-Api-Key", "Cookie", "Tracestate", "Baggage", "X-Private"} {
				h.Set(k, "header-sentinel")
			}
			if tt.filtered {
				h.Set("X-Claude-Code-Session-Id", "invalid session")
			}
			span := r.Start(h)
			instant = instant.Add(1500 * time.Microsecond)
			route := Route{SelectedModel: "claude-gpt-5.6-sol", EffectiveModel: "gpt-5.6-sol", Backend: "codex", Effort: "high", Via: "direct"}
			if tt.filtered {
				route = Route{SelectedModel: "invalid selected", EffectiveModel: "invalid effective", Backend: "Codex", Effort: "ultra", Via: "dispatch-inherited"}
			}
			if tt.retried {
				route.Compaction = &Compaction{Outcome: "empty_summary", UpstreamStatus: new(200), TerminalCode: "message_stop"}
			}
			if tt.compact {
				route.Compaction = &Compaction{Outcome: "incomplete", UpstreamStatus: new(200), ErrorCode: "websocket_missing_terminal", TerminalCode: "terminal-sentinel", InputTokens: new(17.0), OutputTokens: new(12.0)}
			}
			if tt.overflow {
				route.Overflow = &Overflow{Phase: "first_turn", RefusedBy: "upstream", RequestBytes: 150, Tokens: new(120.0), TokensFrom: "upstream", LimitTokens: new(100.0)}
			}
			span.Finish(route, tt.code, tt.override)
			if !tt.retried {
				span.Finish(route, 500, "")
			}
			release()
			r.Wait()
			if diagnostics.Len() != 0 {
				t.Errorf("export failure wrote diagnostic: %s", &diagnostics)
			}
			mu.Lock()
			defer mu.Unlock()
			if !tt.enabled {
				if len(bodies) != 0 {
					t.Error("disabled tracing exported a span")
				}
				return
			}
			if len(bodies) != 1 {
				t.Fatalf("received %d spans for one finished request", len(bodies))
			}
			// Assemble the expectation using OTLP's typed JSON values, including their wire-level types.
			str := func(k, v string) map[string]any {
				return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
			}
			val := func(k, typ string, v any) map[string]any {
				return map[string]any{"key": k, "value": map[string]any{typ: v}}
			}
			id := fresh + fresh
			if tt.linked {
				id = trace
			}
			attrs := []map[string]any{str("source", "codex-gateway"), str("source_event_id", routeID), str("source_schema", "1"), str("event_name", "codex_gateway.route"), str("route_id", routeID), str("trace_id", id), str("span_id", fresh)}
			if tt.linked {
				attrs = append(attrs, str("parent_span_id", parent))
			}
			attrs = append(attrs, val("trace_linked", "boolValue", tt.linked))
			if !tt.filtered {
				attrs = append(attrs, str("session_id", "session-telemetry-1"), str("selected_model", route.SelectedModel), str("effective_model", route.EffectiveModel), str("backend", "codex"), str("effort", "high"))
			}
			attrs = append(attrs, val("fallback", "boolValue", false))
			if !tt.filtered {
				attrs = append(attrs, str("via", "direct"))
			}
			attrs = append(attrs, str("status", tt.status))
			if tt.code != 0 {
				attrs = append(attrs, val("status_code", "intValue", strconv.Itoa(tt.code)))
			}
			if tt.compact {
				attrs = append(attrs, str("compaction_outcome", "incomplete"), val("upstream_status_code", "intValue", "200"), str("compaction_error_code", "websocket_missing_terminal"), val("compaction_input_tokens", "intValue", "17"), val("compaction_output_tokens", "intValue", "12"))
			}
			if tt.retried {
				attrs = append(attrs, str("compaction_outcome", "empty_summary"), val("upstream_status_code", "intValue", "200"), str("compaction_terminal_code", "message_stop"))
			}
			if tt.overflow {
				attrs = append(attrs, str("overflow_phase", "first_turn"), str("overflow_refused_by", "upstream"), val("overflow_request_bytes", "intValue", "150"), val("overflow_tokens", "intValue", "120"), str("overflow_tokens_from", "upstream"), val("overflow_limit_tokens", "intValue", "100"))
			}
			attrs = append(attrs, val("duration_ms", "doubleValue", 1.5))
			code := 2
			if tt.status == "ok" {
				code = 1
			}
			wantSpan := map[string]any{"traceId": id, "spanId": fresh, "name": "codex_gateway.route", "kind": 2, "startTimeUnixNano": "1000000000", "endTimeUnixNano": "1001500000", "attributes": attrs, "events": []any{map[string]any{"name": "codex_gateway.route", "timeUnixNano": "1001500000", "attributes": attrs}}, "status": map[string]int{"code": code}}
			if tt.linked {
				wantSpan["flags"] = tt.flags
				wantSpan["parentSpanId"] = parent
			}
			want := map[string]any{"resourceSpans": []any{map[string]any{"resource": map[string]any{"attributes": []any{str("service.name", "model-gateway")}}, "scopeSpans": []any{map[string]any{"scope": map[string]string{"name": "eigenwise.codex-gateway"}, "spans": []any{wantSpan}}}}}}
			expected, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			var a, b any
			if err := json.Unmarshal(expected, &a); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(bodies[0], &b); err != nil {
				t.Fatal(err)
			}
			if d := gocmp.Diff(a, b); d != "" {
				t.Errorf("OTLP span (-want +got):\n%s", d)
			}
		})
	}
}

func TestTraceExportFailure(t *testing.T) {
	tests := map[string]struct{ stalls bool }{
		"error: collector never replies":          {stalls: true},
		"error: collector refuses TCP connection": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			var diagnostics bytes.Buffer
			newTrace := func(endpoint string, dial func(context.Context, string, string) (net.Conn, error)) *Recorder {
				opt := recorderOptions(map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": endpoint, "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"})
				opt.Diagnostics = &diagnostics
				return newRecorder(opt, dial)
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
					r := newTrace("http://127.0.0.1:1/v1/traces", func(context.Context, string, string) (net.Conn, error) { return client, nil })
					start := time.Now()
					done := make(chan time.Duration, 1)
					go func() {
						r.Start(nil).Finish(Route{Backend: "codex"}, 200, "")
						r.Wait()
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
				endpoint := "http://" + l.Addr().String() + "/v1/traces"
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
				var dialer net.Dialer
				dialResult := make(chan error, 1)
				r := newTrace(endpoint, func(ctx context.Context, network, address string) (net.Conn, error) {
					conn, err := dialer.DialContext(ctx, network, address)
					select {
					case dialResult <- err:
					default:
					}
					return conn, err
				})
				r.Start(nil).Finish(Route{Backend: "codex"}, 200, "")
				r.Wait()
				select {
				case err := <-dialResult:
					if !errors.Is(err, syscall.ECONNREFUSED) {
						t.Errorf("collector dial = %v, want connection refused", err)
					}
				default:
					t.Error("recorder never dialed collector")
				}
			}
			if diagnostics.Len() != 0 {
				t.Errorf("export failure wrote diagnostic: %s", &diagnostics)
			}
		})
	}
}
