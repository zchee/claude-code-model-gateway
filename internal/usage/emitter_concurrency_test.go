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

// pipeCollector serves every collector connection over an in-memory pipe. Created inside a
// synctest bubble, it runs the export bound on virtual time, so host load cannot make a post miss
// it and the counts it records are exact.
type pipeCollector struct {
	mu       sync.Mutex
	payloads [][]byte
	errs     []error
}

// dial hands the exporter one side of a new pipe and answers on the other side.
func (c *pipeCollector) dial(context.Context, string, string) (net.Conn, error) {
	peer, client := net.Pipe()
	go c.serve(peer)
	return client, nil
}

func (c *pipeCollector) serve(peer net.Conn) {
	defer func() { _ = peer.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(peer))
	var body []byte
	if err == nil {
		body, err = io.ReadAll(req.Body)
	}
	if err == nil {
		// Record the post before replying: the reply lets the exporter's Wait return.
		c.mu.Lock()
		c.payloads = append(c.payloads, body)
		c.mu.Unlock()
		_, err = io.WriteString(peer, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
	}
	if err != nil {
		c.mu.Lock()
		c.errs = append(c.errs, err)
		c.mu.Unlock()
	}
}

func (c *pipeCollector) emitter(builder *Builder) *Emitter {
	return newEmitter(func(k string) (string, bool) {
		return "http://127.0.0.1:1/v1/logs", k == "CODEX_GATEWAY_USAGE_ENDPOINT"
	}, builder, c.dial)
}

// received waits until every goroutine of the calling synctest bubble is blocked or has exited,
// reports the collector's failures to t and returns the payloads it recorded.
func (c *pipeCollector) received(t *testing.T) [][]byte {
	t.Helper()
	synctest.Wait()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, err := range c.errs {
		t.Errorf("pipe collector: %v", err)
	}
	return slices.Clone(c.payloads)
}

// runSessions runs two turns of each session concurrently through e, the second turn reporting
// the tool result of the first, waits for the exports and returns each session's two primary
// records in turn order. The goroutines touch only the capture, never t.
func runSessions(t *testing.T, e *Emitter, sessions int) [][2]*Record {
	t.Helper()
	type turn struct {
		body     jsonx.WireValue
		size     int
		response string
	}
	// Each goroutine gets its own decoded bodies, all decoded before any goroutine starts.
	turns := make([][2]turn, sessions)
	for i := range turns {
		turns[i] = [2]turn{
			{wire(t, exactBase), len(exactBase), `{"id":"first","usage":{"input_tokens":100,"output_tokens":1}}`},
			{wire(t, exactNext), len(exactNext), `{"id":"second","usage":{"input_tokens":137,"output_tokens":1}}`},
		}
	}
	records := make([][2]*Record, sessions)
	begin := make(chan struct{})
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Go(func() {
			<-begin
			for j, turn := range turns[i] {
				capture := e.Start(Request{Body: turn.body, BodyBytes: int64(turn.size), Headers: http.Header{"X-Claude-Code-Session-Id": {fmt.Sprintf("session-concurrent-%d", i)}, "X-Claude-Code-Agent-Id": {"agent-concurrent"}}})
				capture.SetResponse(http.StatusOK, nil)
				capture.ObserveJSON([]byte(turn.response))
				records[i][j] = capture.Finish()
			}
		})
	}
	close(begin)
	wg.Wait()
	e.Wait()
	return records
}

// checkFinishedSequences checks the primary records Finish returned, which host load cannot lose:
// every request has one, the sequences are allocated once each across all goroutines, and within
// a session the first request's sequence is lower than the second's.
func checkFinishedSequences(t *testing.T, records [][2]*Record) {
	t.Helper()
	var all []uint64
	for i, session := range records {
		var sequences [2]uint64
		for j, record := range session {
			if record == nil {
				t.Fatalf("session %d turn %d: Finish returned no primary record", i, j)
			}
			sequence, ok := record.Attributes["request_sequence"].(uint64)
			if !ok {
				t.Fatalf("session %d turn %d: request_sequence = %#v, want a uint64", i, j, record.Attributes["request_sequence"])
			}
			sequences[j] = sequence
			all = append(all, sequence)
		}
		if sequences[0] >= sequences[1] {
			t.Errorf("session %d: first request sequence %d is not lower than the second's %d", i, sequences[0], sequences[1])
		}
	}
	slices.Sort(all)
	want := make([]uint64, 2*len(records))
	for i := range want {
		want[i] = uint64(i + 1)
	}
	if d := gocmp.Diff(want, all); d != "" {
		t.Errorf("request sequences returned by Finish (-want +got):\n%s", d)
	}
}

func TestEmitterConcurrentCaptures(t *testing.T) {
	tests := map[string]struct {
		sessions int
		// loopback exports to a real loopback collector, where a loaded host may correctly drop
		// a post at the export bound; only what cannot be lost is asserted on it.
		loopback bool
	}{
		"success: two sessions share one emitter":                      {sessions: 2},
		"success: thirty-two sessions share one emitter":               {sessions: 32},
		"success: two sessions share one emitter on a loopback socket": {sessions: 2, loopback: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.loopback {
				c := newUsageCollector(t)
				checkFinishedSequences(t, runSessions(t, c.emitter(sharedBuilder(t)), tt.sessions))
				payloads := c.received()
				if len(payloads) > 3*tt.sessions {
					t.Errorf("collector received %d records, want at most two usage records and one tool-result record per session (%d)", len(payloads), 3*tt.sessions)
				}
				for _, payload := range payloads {
					attrs := logFields(t, payload)
					if event := attrs["event.name"]; event != "gateway.token.usage" && event != toolEvent {
						t.Errorf("collector received an unexpected %v record", event)
					}
				}
				return
			}
			synctest.Test(t, func(t *testing.T) {
				c := &pipeCollector{}
				checkFinishedSequences(t, runSessions(t, c.emitter(sharedBuilder(t)), tt.sessions))
				payloads := c.received(t)
				if len(payloads) != 3*tt.sessions {
					t.Fatalf("collector received %d records, want two usage records and one tool-result record per session (%d)", len(payloads), 3*tt.sessions)
				}
				counts := map[string]int{}
				var sequences []int
				turns := map[string]map[string]int{}
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
						request, _ := attrs["request_id"].(string)
						if turns[session] == nil {
							turns[session] = map[string]int{}
						}
						turns[session][request] = sequence
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
					t.Errorf("exported request sequences (-want +got):\n%s", d)
				}
				for i := range tt.sessions {
					session := fmt.Sprintf("session-concurrent-%d", i)
					first, okFirst := turns[session]["first"]
					second, okSecond := turns[session]["second"]
					if len(turns[session]) != 2 || !okFirst || !okSecond || tools[session] != 1 {
						t.Errorf("session %s: usage records %v and %d tool-result records, want the first and second requests and 1", session, turns[session], tools[session])
						continue
					}
					if first >= second {
						t.Errorf("session %s: exported first request sequence %d is not lower than the second's %d", session, first, second)
					}
				}
			})
		})
	}
}

func TestCaptureConcurrentFinish(t *testing.T) {
	tests := map[string]struct{ goroutines int }{
		"success: one capture finished from many goroutines exports once": {goroutines: 32},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := &pipeCollector{}
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
				if got := len(c.received(t)); got != 1 {
					t.Errorf("collector received %d records, want 1", got)
				}
			})
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
					// The handler's own limit lets the server close even if the exporter never
					// abandons the connection; it matches the test's watchdog.
					select {
					case <-r.Context().Done():
						abandoned <- struct{}{}
					case <-time.After(30 * time.Second):
					}
				}))
				defer s.Close()
				endpoint := s.URL + "/v1/logs"
				start := time.Now()
				export(t, NewEmitter(func(k string) (string, bool) { return endpoint, k == "CODEX_GATEWAY_USAGE_ENDPOINT" }, NewBuilder(options(t))))
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
