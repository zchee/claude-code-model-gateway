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
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
)

// traceCollector is a loopback OTLP collector that records every request body it receives.
type traceCollector struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newTraceCollector(t *testing.T) *traceCollector {
	t.Helper()
	c := &traceCollector{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(c.server.Close)
	return c
}

// env enables route spans towards this collector.
func (c *traceCollector) env() map[string]string {
	return map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": c.server.URL + "/v1/traces", "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"}
}

func (c *traceCollector) received() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.bodies)
}

// pipeTraceCollector serves every collector connection over an in-memory pipe. Created inside a
// synctest bubble, it runs the export bound on virtual time, so host load cannot make a post miss
// it and the counts it records are exact.
type pipeTraceCollector struct {
	mu     sync.Mutex
	bodies [][]byte
	errs   []error
}

// dial hands the exporter one side of a new pipe and answers on the other side.
func (c *pipeTraceCollector) dial(context.Context, string, string) (net.Conn, error) {
	peer, client := net.Pipe()
	go c.serve(peer)
	return client, nil
}

func (c *pipeTraceCollector) serve(peer net.Conn) {
	defer func() { _ = peer.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(peer))
	var body []byte
	if err == nil {
		body, err = io.ReadAll(req.Body)
	}
	if err == nil {
		// Record the post before replying: the reply lets the exporter's Wait return.
		c.mu.Lock()
		c.bodies = append(c.bodies, body)
		c.mu.Unlock()
		_, err = io.WriteString(peer, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
	}
	if err != nil {
		c.mu.Lock()
		c.errs = append(c.errs, err)
		c.mu.Unlock()
	}
}

// recorder creates a recorder that exports route spans to this collector.
func (c *pipeTraceCollector) recorder(opts Options) *Recorder { return newRecorder(opts, c.dial) }

// env enables route spans; the address is never dialed.
func (c *pipeTraceCollector) env() map[string]string {
	return map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "http://127.0.0.1:1/v1/traces", "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"}
}

// received waits until every goroutine of the calling synctest bubble is blocked or has exited,
// reports the collector's failures to t and returns the bodies it recorded.
func (c *pipeTraceCollector) received(t *testing.T) [][]byte {
	t.Helper()
	synctest.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, err := range c.errs {
		t.Errorf("pipe collector: %v", err)
	}
	return slices.Clone(c.bodies)
}

// blockIdentity returns the identifiers an unlinked span draws from 40 random bytes of value v:
// the trace identifier, the span identifier and the version 4 route identifier, in that order.
func blockIdentity(v byte) spanIdentity {
	route := bytes.Repeat([]byte{v}, 16)
	route[6] = route[6]&0x0f | 0x40
	route[8] = route[8]&0x3f | 0x80
	h := hex.EncodeToString(route)
	return spanIdentity{
		TraceID: strings.Repeat(hex.EncodeToString([]byte{v}), 16),
		SpanID:  strings.Repeat(hex.EncodeToString([]byte{v}), 8),
		RouteID: h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:],
	}
}

// checkBlockIdentities requires every span's trace, span and route identifiers to come whole from
// one 40-byte block of the source, each block at most once, and returns the block values seen.
func checkBlockIdentities(t *testing.T, ids []spanIdentity) map[byte]bool {
	t.Helper()
	blocks := map[byte]bool{}
	for _, id := range ids {
		raw, err := hex.DecodeString(id.SpanID)
		if err != nil || len(raw) == 0 {
			t.Errorf("span identifier %q is not hexadecimal", id.SpanID)
			continue
		}
		want := blockIdentity(raw[0])
		want.SessionID = id.SessionID
		if d := gocmp.Diff(want, id); d != "" {
			t.Errorf("span identifiers are not one whole draw of block %#02x (-want +got):\n%s", raw[0], d)
		}
		if blocks[raw[0]] {
			t.Errorf("block %#02x was drawn by more than one span", raw[0])
		}
		blocks[raw[0]] = true
	}
	return blocks
}

// spanIdentity holds the identifiers of one exported route span.
type spanIdentity struct {
	TraceID, SpanID, RouteID, SessionID string
}

// exportedSpan decodes the single span of one OTLP trace export.
func exportedSpan(t *testing.T, body []byte) spanIdentity {
	t.Helper()
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID    string `json:"traceId"`
					SpanID     string `json:"spanId"`
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.ResourceSpans) != 1 || len(doc.ResourceSpans[0].ScopeSpans) != 1 || len(doc.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("invalid OTLP span envelope: %s", body)
	}
	span := doc.ResourceSpans[0].ScopeSpans[0].Spans[0]
	id := spanIdentity{TraceID: span.TraceID, SpanID: span.SpanID}
	for _, attr := range span.Attributes {
		switch attr.Key {
		case "route_id":
			id.RouteID = attr.Value.StringValue
		case "session_id":
			id.SessionID = attr.Value.StringValue
		}
	}
	return id
}

// routeLogRows decodes a complete route log, one JSON object per newline-terminated line.
func routeLogRows(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Fatalf("route log does not end with a complete line: %q", raw)
	}
	var rows []map[string]any
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("route log line %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// panicAtOffsetReader fills every buffer with 0x22. Once, on the read that would deliver the byte
// at panicAt of its stream, it panics instead and delivers nothing.
type panicAtOffsetReader struct {
	offset, panicAt int
	fired           bool
}

func (r *panicAtOffsetReader) Read(p []byte) (int, error) {
	if !r.fired && r.panicAt >= r.offset && r.panicAt < r.offset+len(p) {
		r.fired = true
		panic("random source failure")
	}
	for i := range p {
		p[i] = 0x22
	}
	r.offset += len(p)
	return len(p), nil
}

func TestStartAfterRandomSourcePanic(t *testing.T) {
	// An unlinked Start draws the trace identifier from bytes 0 to 15, the span identifier from
	// bytes 16 to 23 and the route identifier from bytes 24 to 39.
	tests := map[string]struct{ panicAt int }{
		"success: start completes after a panic while reading the trace identifier": {panicAt: 0},
		"success: start completes after a panic while reading the span identifier":  {panicAt: 16},
		"success: start completes after a panic while reading the route identifier": {panicAt: 24},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := newTraceCollector(t)
			opts := recorderOptions(c.env())
			random := &panicAtOffsetReader{panicAt: tt.panicAt}
			opts.Random = random
			r := NewRecorder(opts)
			recovered := func() (v any) {
				defer func() { v = recover() }()
				r.Start(http.Header{})
				return nil
			}()
			if recovered == nil {
				t.Fatal("the random source's panic did not reach the caller of Start")
			}
			if random.offset != tt.panicAt {
				t.Fatalf("the panic came after %d bytes were drawn, want it on the read of byte %d", random.offset, tt.panicAt)
			}
			// A lock left held by the panic would block this Start forever; bound the wait
			// generously so that only a lock that is never released trips it.
			started := make(chan *Span, 1)
			go func() { started <- r.Start(http.Header{}) }()
			var span *Span
			select {
			case span = <-started:
			case <-time.After(30 * time.Second):
				t.Fatal("Start blocked after the caller recovered a panic of the random source")
			}
			span.Finish(Route{Backend: "codex"}, http.StatusOK, "")
			r.Wait()
			bodies := c.received()
			if len(bodies) != 1 {
				t.Fatalf("collector received %d spans, want the one span started after the panic", len(bodies))
			}
			want := spanIdentity{TraceID: strings.Repeat("22", 16), SpanID: strings.Repeat("22", 8), RouteID: "22222222-2222-4222-a222-222222222222"}
			if d := gocmp.Diff(want, exportedSpan(t, bodies[0])); d != "" {
				t.Errorf("span started after the panic (-want +got):\n%s", d)
			}
		})
	}
}

func TestRouteLogWithoutURL(t *testing.T) {
	header := func() http.Header {
		h := http.Header{}
		h.Set("X-Claude-Code-Session-Id", "header-session")
		return h
	}
	tests := map[string]struct {
		request  *http.Request
		wantPath string
	}{
		"success: request target is used when the URL is nil": {
			request:  &http.Request{Method: http.MethodPost, RequestURI: "/v1/messages?key=query-sentinel", Header: header()},
			wantPath: "/v1/messages",
		},
		"success: empty request target and nil URL log an empty path": {
			request: &http.Request{Method: http.MethodPost, Header: header()},
		},
		"success: URL is used when the request target is empty": {
			request:  &http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/v1/messages", RawQuery: "key=query-sentinel"}, Header: header()},
			wantPath: "/v1/messages",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			opts := recorderOptions(nil)
			opts.LogPath = filepath.Join(t.TempDir(), "logs", "request-routes.jsonl")
			var diagnostics bytes.Buffer
			opts.Diagnostics = &diagnostics
			r := NewRecorder(opts)
			func() {
				defer func() {
					if v := recover(); v != nil {
						t.Errorf("LogRoute panicked: %v", v)
					}
				}()
				r.LogRoute(tt.request, LogEntry{Backend: "anthropic", Model: new("claude-fable-5")})
			}()
			raw, err := os.ReadFile(opts.LogPath)
			if err != nil {
				t.Fatalf("route log was not written: %v", err)
			}
			want := []map[string]any{{"at": "1970-01-01T00:00:01.000Z", "backend": "anthropic", "model": "claude-fable-5", "path": tt.wantPath, "sessionId": "header-session"}}
			if d := gocmp.Diff(want, routeLogRows(t, raw)); d != "" {
				t.Errorf("route log (-want +got):\n%s", d)
			}
			if diagnostics.Len() != 0 {
				t.Errorf("unexpected diagnostic: %q", diagnostics.String())
			}
		})
	}
}

func TestTraceExportFailureIsSilent(t *testing.T) {
	tests := map[string]struct{ reply string }{
		"error: collector closes the connection without a reply": {},
		"error: collector replies with a malformed status line":  {reply: "not an HTTP reply\r\n\r\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			var mu sync.Mutex
			var requests int
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				requests++
				mu.Unlock()
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				if tt.reply != "" {
					_, _ = io.WriteString(conn, tt.reply)
				}
				_ = conn.Close()
			}))
			defer s.Close()
			opts := recorderOptions(map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": s.URL + "/v1/traces", "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"})
			var diagnostics bytes.Buffer
			opts.Diagnostics = &diagnostics
			r := NewRecorder(opts)
			r.Start(nil).Finish(Route{Backend: "codex"}, http.StatusOK, "")
			r.Wait()
			mu.Lock()
			defer mu.Unlock()
			if requests != 1 {
				t.Errorf("collector received %d requests, want 1", requests)
			}
			if diagnostics.Len() != 0 {
				t.Errorf("export failure wrote diagnostic: %q", diagnostics.String())
			}
		})
	}
}

func TestRecorderConcurrentUse(t *testing.T) {
	tests := map[string]struct {
		goroutines int
		// loopback exports to a real loopback collector, where a loaded host may correctly drop
		// a span at the export bound; only what cannot be lost is asserted on it.
		loopback bool
	}{
		"success: two requests share one recorder":                      {goroutines: 2},
		"success: sixty-four requests share one recorder":               {goroutines: 64},
		"success: two requests share one recorder on a loopback socket": {goroutines: 2, loopback: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// run starts every request from its own goroutine and returns the route log and the
			// diagnostics once every export has finished.
			run := func(t *testing.T, makeRecorder func(Options) *Recorder, env map[string]string) ([]byte, string, map[string]bool) {
				opts := recorderOptions(env)
				// Each Start draws one 40-byte block: trace, span and route identifiers. Distinct
				// blocks expose a draw torn between two concurrent Starts; bytes.Reader is not safe
				// for concurrent use, so the race detector sees an unserialized draw.
				random := make([]byte, 0, tt.goroutines*40)
				for i := range tt.goroutines {
					random = append(random, bytes.Repeat([]byte{byte(i + 1)}, 40)...)
				}
				opts.Random = bytes.NewReader(random)
				opts.LogPath = filepath.Join(t.TempDir(), "logs", "request-routes.jsonl")
				var diagnostics bytes.Buffer
				opts.Diagnostics = &diagnostics
				r := makeRecorder(opts)
				wantSessions := map[string]bool{}
				// Every request is made here: nothing that can stop the test runs in the goroutines.
				requests := make([]*http.Request, tt.goroutines)
				for i := range requests {
					session := fmt.Sprintf("session-concurrent-%d", i)
					wantSessions[session] = true
					requests[i] = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages?key=query-sentinel", nil)
					requests[i].Header.Set("X-Claude-Code-Session-Id", session)
				}
				begin := make(chan struct{})
				var wg sync.WaitGroup
				for _, req := range requests {
					wg.Go(func() {
						<-begin
						r.LogRoute(req, LogEntry{Backend: "codex", Model: new("gpt-5.6-sol")})
						r.Start(req.Header).Finish(Route{Backend: "codex"}, http.StatusOK, "")
					})
				}
				close(begin)
				wg.Wait()
				r.Wait()
				raw, err := os.ReadFile(opts.LogPath)
				if err != nil {
					t.Fatal(err)
				}
				return raw, diagnostics.String(), wantSessions
			}
			// checkLog checks what the recorder wrote locally, which host load cannot lose.
			checkLog := func(t *testing.T, raw []byte, diagnostics string, wantSessions map[string]bool) {
				if diagnostics != "" {
					t.Errorf("unexpected diagnostic: %q", diagnostics)
				}
				rows := routeLogRows(t, raw)
				if len(rows) != tt.goroutines {
					t.Fatalf("route log has %d lines, want %d", len(rows), tt.goroutines)
				}
				logged := map[string]bool{}
				for _, row := range rows {
					session, _ := row["sessionId"].(string)
					logged[session] = true
					if row["path"] != "/v1/messages" || row["backend"] != "codex" || row["model"] != "gpt-5.6-sol" {
						t.Errorf("unexpected route log line: %v", row)
					}
				}
				if d := gocmp.Diff(wantSessions, logged); d != "" {
					t.Errorf("route log sessions (-want +got):\n%s", d)
				}
			}
			identities := func(t *testing.T, bodies [][]byte) ([]spanIdentity, map[string]bool) {
				ids := make([]spanIdentity, 0, len(bodies))
				sessions := map[string]bool{}
				for _, body := range bodies {
					id := exportedSpan(t, body)
					ids = append(ids, id)
					sessions[id.SessionID] = true
				}
				return ids, sessions
			}
			if tt.loopback {
				c := newTraceCollector(t)
				raw, diagnostics, wantSessions := run(t, NewRecorder, c.env())
				checkLog(t, raw, diagnostics, wantSessions)
				bodies := c.received()
				if len(bodies) > tt.goroutines {
					t.Errorf("collector received %d spans, want at most %d", len(bodies), tt.goroutines)
				}
				ids, sessions := identities(t, bodies)
				for block := range checkBlockIdentities(t, ids) {
					if block < 1 || int(block) > tt.goroutines {
						t.Errorf("span drawn from block %#02x, want a block of 1 to %d", block, tt.goroutines)
					}
				}
				for session := range sessions {
					if !wantSessions[session] {
						t.Errorf("span carries unexpected session %q", session)
					}
				}
				return
			}
			synctest.Test(t, func(t *testing.T) {
				c := &pipeTraceCollector{}
				raw, diagnostics, wantSessions := run(t, c.recorder, c.env())
				checkLog(t, raw, diagnostics, wantSessions)
				bodies := c.received(t)
				if len(bodies) != tt.goroutines {
					t.Fatalf("collector received %d spans, want %d", len(bodies), tt.goroutines)
				}
				ids, sessions := identities(t, bodies)
				wantBlocks := map[byte]bool{}
				for i := range tt.goroutines {
					wantBlocks[byte(i+1)] = true
				}
				if d := gocmp.Diff(wantBlocks, checkBlockIdentities(t, ids)); d != "" {
					t.Errorf("random blocks drawn by the spans (-want +got):\n%s", d)
				}
				if d := gocmp.Diff(wantSessions, sessions); d != "" {
					t.Errorf("span sessions (-want +got):\n%s", d)
				}
			})
		})
	}
}

func TestSpanConcurrentFinish(t *testing.T) {
	tests := map[string]struct{ traceparent string }{
		"success: unlinked span finished from many goroutines exports once": {},
		"success: linked span finished from many goroutines exports once":   {traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := &pipeTraceCollector{}
				r := c.recorder(recorderOptions(c.env()))
				h := http.Header{}
				if tt.traceparent != "" {
					h.Set("Traceparent", tt.traceparent)
				}
				span := r.Start(h)
				begin := make(chan struct{})
				var wg sync.WaitGroup
				for i := range 32 {
					wg.Go(func() {
						<-begin
						span.Finish(Route{Backend: "codex"}, http.StatusOK+i, "")
					})
				}
				close(begin)
				wg.Wait()
				r.Wait()
				if got := len(c.received(t)); got != 1 {
					t.Errorf("collector received %d spans for one request, want 1", got)
				}
			})
		})
	}
}

func TestSpanIntegerAttributeExact(t *testing.T) {
	tests := map[string]struct {
		requestBytes int64
		want         string
	}{
		"success: small request byte count is an exact decimal":         {requestBytes: 150, want: "150"},
		"success: request byte count above two to the 53 is exact":      {requestBytes: 1<<53 + 1, want: "9007199254740993"},
		"success: largest int64 request byte count is an exact decimal": {requestBytes: math.MaxInt64, want: "9223372036854775807"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := &pipeTraceCollector{}
				r := c.recorder(recorderOptions(c.env()))
				r.Start(nil).Finish(Route{Backend: "codex", Overflow: &Overflow{Phase: "first_turn", RefusedBy: "upstream", RequestBytes: tt.requestBytes}}, http.StatusRequestEntityTooLarge, "")
				r.Wait()
				bodies := c.received(t)
				if len(bodies) != 1 {
					t.Fatalf("collector received %d spans, want 1", len(bodies))
				}
				var doc struct {
					ResourceSpans []struct {
						ScopeSpans []struct {
							Spans []struct {
								Attributes []struct {
									Key   string         `json:"key"`
									Value map[string]any `json:"value"`
								} `json:"attributes"`
							} `json:"spans"`
						} `json:"scopeSpans"`
					} `json:"resourceSpans"`
				}
				if err := json.Unmarshal(bodies[0], &doc); err != nil {
					t.Fatal(err)
				}
				if len(doc.ResourceSpans) != 1 || len(doc.ResourceSpans[0].ScopeSpans) != 1 || len(doc.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
					t.Fatalf("invalid OTLP span envelope: %s", bodies[0])
				}
				var got []map[string]any
				for _, attr := range doc.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
					if attr.Key == "overflow_request_bytes" {
						got = append(got, attr.Value)
					}
				}
				if d := gocmp.Diff([]map[string]any{{"intValue": tt.want}}, got); d != "" {
					t.Errorf("exported overflow_request_bytes (-want +got):\n%s", d)
				}
			})
		})
	}
}

func TestTraceStalledReplyBody(t *testing.T) {
	const reply = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{"
	tests := map[string]struct{ loopback bool }{
		"error: collector on a pipe stalls after its response headers":   {},
		"error: collector on loopback stalls after its response headers": {loopback: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			var diagnostics bytes.Buffer
			env := map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "http://127.0.0.1:1/v1/traces", "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"}
			if !tt.loopback {
				// Virtual time pins the bound exactly; the pipe stands in for the collector's socket.
				synctest.Test(t, func(t *testing.T) {
					peer, client := net.Pipe()
					defer func() {
						_ = peer.Close()
						_ = client.Close()
					}()
					headers := make(chan error, 1)
					abandoned := make(chan struct{})
					go func() {
						defer close(abandoned)
						req, err := http.ReadRequest(bufio.NewReader(peer))
						if err == nil {
							_, err = io.Copy(io.Discard, req.Body)
						}
						if err == nil {
							_, err = io.WriteString(peer, reply)
						}
						headers <- err
						// Send nothing more; this read ends only when the exporter closes the connection.
						_, _ = io.Copy(io.Discard, peer)
					}()
					opts := recorderOptions(env)
					opts.Diagnostics = &diagnostics
					r := newRecorder(opts, func(context.Context, string, string) (net.Conn, error) { return client, nil })
					start := time.Now()
					done := make(chan time.Duration, 1)
					go func() {
						r.Start(nil).Finish(Route{Backend: "codex"}, http.StatusOK, "")
						r.Wait()
						done <- time.Since(start)
					}()
					select {
					case elapsed := <-done:
						if elapsed != 500*time.Millisecond {
							t.Errorf("Wait returned at %s, want 500ms", elapsed)
						}
					case <-time.After(501 * time.Millisecond):
						t.Error("Wait did not return within the 500ms export bound")
					}
					synctest.Wait()
					select {
					case err := <-headers:
						if err != nil {
							t.Errorf("collector: %v", err)
						}
					default:
						t.Error("collector never sent its response headers")
					}
					select {
					case <-abandoned:
					default:
						t.Error("exporter did not close the stalled collector connection")
					}
				})
			} else {
				abandoned := make(chan struct{}, 1)
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Length", "64")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "{")
					if err := http.NewResponseController(w).Flush(); err != nil {
						t.Error(err)
						return
					}
					// The handler's own limit lets the server close even if the exporter never
					// abandons the connection; it matches the test's watchdog.
					select {
					case <-r.Context().Done():
						abandoned <- struct{}{}
					case <-time.After(30 * time.Second):
					}
				}))
				defer s.Close()
				env["CODEX_GATEWAY_TELEMETRY_ENDPOINT"] = s.URL + "/v1/traces"
				opts := recorderOptions(env)
				opts.Diagnostics = &diagnostics
				r := NewRecorder(opts)
				start := time.Now()
				r.Start(nil).Finish(Route{Backend: "codex"}, http.StatusOK, "")
				r.Wait()
				// The deadline starts after Finish, so the wait lasts at least the bound. A loaded
				// host can only make it longer; the exact bound is checked on the pipe.
				if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
					t.Errorf("Wait returned after %s, before the 500ms export bound", elapsed)
				}
				select {
				case <-abandoned:
				case <-time.After(30 * time.Second):
					t.Error("exporter did not close the stalled collector connection")
				}
			}
			if diagnostics.Len() != 0 {
				t.Errorf("export failure wrote diagnostic: %q", diagnostics.String())
			}
		})
	}
}

func TestStartWithShortRandomSource(t *testing.T) {
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	tests := map[string]struct {
		random      int
		traceparent string
	}{
		"error: empty random source yields an inert span":                          {},
		"error: unlinked span whose route identifier draw ends early is inert":     {random: 39},
		"error: unlinked span whose span identifier draw ends early is inert":      {random: 20},
		"error: linked span whose route identifier draw ends early is inert":       {random: 23, traceparent: traceparent},
		"error: linked span whose span identifier draw ends early is inert":        {random: 7, traceparent: traceparent},
		"error: unlinked span whose trace identifier draw ends early is inert":     {random: 15},
		"error: linked span with an empty random source is inert despite the link": {traceparent: traceparent},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := newTraceCollector(t)
			opts := recorderOptions(c.env())
			opts.Random = bytes.NewReader(bytes.Repeat([]byte{0x33}, tt.random))
			var diagnostics bytes.Buffer
			opts.Diagnostics = &diagnostics
			r := NewRecorder(opts)
			h := http.Header{}
			if tt.traceparent != "" {
				h.Set("Traceparent", tt.traceparent)
			}
			span := r.Start(h)
			span.Finish(Route{Backend: "codex"}, http.StatusOK, "")
			span.Finish(Route{Backend: "codex"}, http.StatusOK, "")
			r.Wait()
			if got := len(c.received()); got != 0 {
				t.Errorf("collector received %d spans from a span without identifiers, want 0", got)
			}
			if diagnostics.Len() != 0 {
				t.Errorf("unexpected diagnostic: %q", diagnostics.String())
			}
		})
	}
}
