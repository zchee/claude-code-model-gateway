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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// usageCollector is a loopback OTLP log collector that records every request body it receives.
type usageCollector struct {
	server   *httptest.Server
	mu       sync.Mutex
	payloads [][]byte
}

func newUsageCollector(t *testing.T) *usageCollector {
	t.Helper()
	c := &usageCollector{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		c.mu.Lock()
		c.payloads = append(c.payloads, body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *usageCollector) emitter(builder *Builder) *Emitter {
	endpoint := c.server.URL + "/v1/logs"
	return NewEmitter(func(k string) (string, bool) { return endpoint, k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, builder)
}

func (c *usageCollector) received() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.payloads)
}

// sharedBuilder has enough entropy for every record of a concurrent run.
func sharedBuilder(t *testing.T) *Builder {
	t.Helper()
	return NewBuilder(Options{ProjectsDir: t.TempDir(), Now: func() time.Time { return time.Unix(123, 0) }, Random: bytes.NewReader(make([]byte, 1<<20))})
}

// captureOutput redirects the process's standard output, standard error and default logger to a
// file until the test ends, because the emitter has no diagnostic sink. Tests that use it must
// not run in parallel.
func captureOutput(t *testing.T) func() []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "process-output")
	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, logOutput := os.Stdout, os.Stderr, log.Writer()
	os.Stdout, os.Stderr = output, output
	log.SetOutput(output)
	t.Cleanup(func() {
		os.Stdout, os.Stderr = stdout, stderr
		log.SetOutput(logOutput)
		if err := output.Close(); err != nil {
			t.Error(err)
		}
	})
	return func() []byte {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
}

func TestEmitterConcurrentCaptures(t *testing.T) {
	tests := map[string]struct{ sessions int }{
		"success: two sessions share one emitter":        {sessions: 2},
		"success: thirty-two sessions share one emitter": {sessions: 32},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := newUsageCollector(t)
			e := c.emitter(sharedBuilder(t))
			begin := make(chan struct{})
			var wg sync.WaitGroup
			for i := range tt.sessions {
				// Two turns of one session: the second reports the tool result of the first. Each
				// goroutine gets its own decoded bodies.
				turns := []struct {
					body     jsonx.WireValue
					size     int
					response string
				}{
					{wire(t, exactBase), len(exactBase), `{"id":"first","usage":{"input_tokens":100,"output_tokens":1}}`},
					{wire(t, exactNext), len(exactNext), `{"id":"second","usage":{"input_tokens":137,"output_tokens":1}}`},
				}
				wg.Go(func() {
					<-begin
					for _, turn := range turns {
						capture := e.Start(Request{Body: turn.body, BodyBytes: int64(turn.size), Headers: http.Header{"X-Claude-Code-Session-Id": {fmt.Sprintf("session-concurrent-%d", i)}, "X-Claude-Code-Agent-Id": {"agent-concurrent"}}})
						capture.SetResponse(http.StatusOK, nil)
						capture.ObserveJSON([]byte(turn.response))
						if capture.Finish() == nil {
							t.Error("capture returned no primary record")
						}
					}
				})
			}
			close(begin)
			wg.Wait()
			e.Wait()
			payloads := c.received()
			if len(payloads) != 3*tt.sessions {
				t.Fatalf("collector received %d records, want two usage records and one tool-result record per session (%d)", len(payloads), 3*tt.sessions)
			}
			counts := map[string]int{}
			var sequences []int
			turns := map[string][]int{}
			tools := map[string]int{}
			for _, payload := range payloads {
				attrs := logFields(t, payload)
				event, _ := attrs["event.name"].(string)
				counts[event]++
				session, _ := attrs["session_id"].(string)
				sequence, _ := attrs["request_sequence"].(int)
				switch event {
				case "gateway.token.usage":
					sequences = append(sequences, sequence)
					turns[session] = append(turns[session], sequence)
				case toolEvent:
					tools[session]++
					checkJSON(t, 37, attrs["tool_result_tokens"])
					checkJSON(t, "second", attrs["request_id"])
				}
			}
			checkJSON(t, map[string]int{"gateway.token.usage": 2 * tt.sessions, toolEvent: tt.sessions}, counts)
			// Sequences are allocated once each, in arrival order, across all goroutines.
			slices.Sort(sequences)
			want := make([]int, 2*tt.sessions)
			for i := range want {
				want[i] = i + 1
			}
			if d := gocmp.Diff(want, sequences); d != "" {
				t.Errorf("request sequences (-want +got):\n%s", d)
			}
			for i := range tt.sessions {
				session := fmt.Sprintf("session-concurrent-%d", i)
				if len(turns[session]) != 2 || tools[session] != 1 {
					t.Errorf("session %s: %d usage records and %d tool-result records, want 2 and 1", session, len(turns[session]), tools[session])
				}
			}
		})
	}
}

func TestCaptureConcurrentFinish(t *testing.T) {
	tests := map[string]struct{ goroutines int }{
		"success: one capture finished from many goroutines exports once": {goroutines: 32},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := newUsageCollector(t)
			e := c.emitter(sharedBuilder(t))
			capture := e.Start(Request{})
			capture.SetResponse(http.StatusOK, nil)
			capture.ObserveJSON([]byte(`{"usage":{"input_tokens":15,"output_tokens":1}}`))
			begin := make(chan struct{})
			var wg sync.WaitGroup
			var mu sync.Mutex
			var records int
			for range tt.goroutines {
				wg.Go(func() {
					<-begin
					if capture.Finish() != nil {
						mu.Lock()
						records++
						mu.Unlock()
					}
				})
			}
			close(begin)
			wg.Wait()
			e.Wait()
			if records != 1 {
				t.Errorf("Finish returned %d records, want 1", records)
			}
			if got := len(c.received()); got != 1 {
				t.Errorf("collector received %d records, want 1", got)
			}
		})
	}
}

func TestEmitterStalledReplyBody(t *testing.T) {
	const reply = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{"
	tests := map[string]struct{ loopback bool }{
		"error: collector on a pipe stalls after its response headers":   {},
		"error: collector on loopback stalls after its response headers": {loopback: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			output := captureOutput(t)
			export := func(t *testing.T, e *Emitter) {
				capture := e.Start(Request{})
				capture.SetResponse(http.StatusOK, nil)
				capture.ObserveJSON([]byte(`{"usage":{"input_tokens":15}}`))
				if capture.Finish() == nil {
					t.Error("a stalled collector suppressed the primary usage record")
				}
				e.Wait()
			}
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
					e := newEmitter(func(k string) (string, bool) {
						return "http://127.0.0.1:1/v1/logs", k == "CODEX_GATEWAY_USAGE_ENDPOINT"
					}, NewBuilder(options(t)), func(context.Context, string, string) (net.Conn, error) { return client, nil })
					start := time.Now()
					done := make(chan time.Duration, 1)
					go func() {
						export(t, e)
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
					select {
					case <-r.Context().Done():
						abandoned <- struct{}{}
					case <-time.After(10 * time.Second):
					}
				}))
				defer s.Close()
				endpoint := s.URL + "/v1/logs"
				start := time.Now()
				export(t, NewEmitter(func(k string) (string, bool) { return endpoint, k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, NewBuilder(options(t))))
				// The deadline starts after Finish, so the wait lasts at least the bound; the upper
				// limit leaves room for a loaded test machine.
				if elapsed := time.Since(start); elapsed < 500*time.Millisecond || elapsed > 5*time.Second {
					t.Errorf("Wait returned after %s, want the 500ms export bound", elapsed)
				}
				select {
				case <-abandoned:
				case <-time.After(5 * time.Second):
					t.Error("exporter did not close the stalled collector connection")
				}
			}
			if raw := output(); len(raw) != 0 {
				t.Errorf("export failure wrote output: %q", raw)
			}
		})
	}
}

func TestEmitterExportFailureIsSilent(t *testing.T) {
	tests := map[string]struct{ reply string }{
		"error: collector closes the connection without a reply": {},
		"error: collector replies with a malformed status line":  {reply: "not an HTTP reply\r\n\r\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			output := captureOutput(t)
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
			endpoint := s.URL + "/v1/logs"
			e := NewEmitter(func(k string) (string, bool) { return endpoint, k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, NewBuilder(options(t)))
			capture := e.Start(Request{})
			capture.SetResponse(http.StatusOK, nil)
			capture.ObserveJSON([]byte(`{"usage":{"input_tokens":15}}`))
			if capture.Finish() == nil {
				t.Error("a failing collector suppressed the primary usage record")
			}
			e.Wait()
			mu.Lock()
			defer mu.Unlock()
			if requests != 1 {
				t.Errorf("collector received %d requests, want 1", requests)
			}
			if raw := output(); len(raw) != 0 {
				t.Errorf("export failure wrote output: %q", raw)
			}
		})
	}
}
