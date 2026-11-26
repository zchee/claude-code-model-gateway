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

package compose_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
)

const upgradeRejection = `{"type":"error","error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`

type departureWriter struct {
	mu           sync.Mutex
	rec          *httptest.ResponseRecorder
	departed     bool
	writes, late int
}

func newDepartureWriter() *departureWriter {
	return &departureWriter{rec: httptest.NewRecorder()}
}

func (w *departureWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rec.Header()
}

func (w *departureWriter) recordLocked() {
	w.writes++
	if w.departed {
		w.late++
	}
}

func (w *departureWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recordLocked()
	w.rec.WriteHeader(code)
}

func (w *departureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recordLocked()
	if w.departed {
		return 0, io.ErrClosedPipe
	}
	return w.rec.Write(p)
}

func (w *departureWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func (w *departureWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recordLocked()
	w.rec.Flush()
}

func (w *departureWriter) markDeparted() {
	w.mu.Lock()
	w.departed = true
	w.mu.Unlock()
}

func (w *departureWriter) snapshot() (writes, late, code int, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes, w.late, w.rec.Code, w.rec.Body.String()
}

func beginRequest(t *testing.T, h *harness, body string) (*departureWriter, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	w := newDepartureWriter()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(body))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if p := recover(); p != nil {
				if err, ok := p.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
			}
		}()
		h.workerHandler.ServeHTTP(w, req)
	}()
	t.Cleanup(func() { cancel(); h.cancelService(); waitSignal(t, "handler cleanup", done, time.Second) })
	return w, cancel, done
}

func assertRunning(t *testing.T, name string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s already returned", name)
	default:
	}
}

func TestRetryWaitVirtualClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var attempts atomic.Int32
		h := newHarness(t, harnessConfig{inMemory: true, disableUsage: true, disableTrace: true, replyRetries: new(1), replyRetryDelay: new(time.Second), proxyHandler: func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, upgradeRejection)
		}})
		w, cancel, done := beginRequest(t, h, messageRequestJSON("claude-gpt-6.1-sol[1m]", false))
		synctest.Wait()
		mustDiff(t, "first rejection", int32(1), attempts.Load())
		assertRunning(t, "retrying handler", done)
		writes, _, _, _ := w.snapshot()
		mustDiff(t, "writes before retry", 0, writes)
		w.markDeparted()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("cancellation did not interrupt the retry wait")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		mustDiff(t, "no later retry", int32(1), attempts.Load())
		writes, _, _, _ = w.snapshot()
		mustDiff(t, "no response to departed client", 0, writes)

		// With the client still present the configured delay expires and the sole retry runs.
		live, _, finished := beginRequest(t, h, messageRequestJSON("claude-gpt-6.1-sol[1m]", false))
		synctest.Wait()
		time.Sleep(time.Second - time.Nanosecond)
		synctest.Wait()
		mustDiff(t, "no early retry", int32(2), attempts.Load())
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case <-finished:
		default:
			t.Fatal("configured retry limit did not end the request")
		}
		mustDiff(t, "one retry for live client", int32(3), attempts.Load())
		_, _, code, _ := live.snapshot()
		mustDiff(t, "upgrade exhaustion status", http.StatusServiceUnavailable, code)
	})
}

const (
	proxyStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":10}}}\n\n"
	grokStart  = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"model\":\"grok-4.7\"}}\n\n"
)

func TestClientDisconnectPreservesUpstreamWork(t *testing.T) {
	tests := map[string]struct{ kind string }{
		"success: client leaves during buffered attempt":              {"buffered"},
		"success: client leaves during sse relay and heartbeats stop": {"stream"},
		"success: client leaves during guarded compaction attempt":    {"guard"},
		"success: client leaves during grok inference":                {"grok"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			collector := newOTLPCollector(t, httptest.NewServer)
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				var attempts atomic.Int32
				providerDone := make(chan struct{})
				markDone := sync.OnceFunc(func() { close(providerDone) })
				var resMu sync.Mutex
				var results []outcome.Result
				snapshotResults := func() []outcome.Result {
					resMu.Lock()
					defer resMu.Unlock()
					return slices.Clone(results)
				}
				cfg := harnessConfig{
					inMemory: true, disableUsage: true, env: map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": collector.srv.URL + "/v1/traces"}, heartbeatInterval: 100 * time.Millisecond,
					observeOptions: func(o *worker.Options) {
						start := o.BeforeBody
						o.BeforeBody = func(r *http.Request) func(outcome.Result) {
							finish := start(r)
							return func(v outcome.Result) {
								finish(v)
								resMu.Lock()
								results = append(results, v)
								resMu.Unlock()
							}
						}
					},
				}
				hold := func(w http.ResponseWriter, r *http.Request) {
					defer markDone()
					attempts.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if tt.kind != "buffered" {
						w.Header().Set("Content-Type", "text/event-stream")
						frame := proxyStart
						if tt.kind == "grok" {
							frame = grokStart
						}
						_, _ = io.WriteString(w, frame)
						w.(http.Flusher).Flush()
					}
					select {
					case <-r.Context().Done():
						return
					case <-release:
					}
					switch tt.kind {
					case "buffered":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, upgradeRejection)
					case "grok":
						_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\"}}\n\n")
					case "stream":
						_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					}
				}
				body := messageRequestJSON("claude-gpt-6.1-sol[1m]", tt.kind != "buffered")
				cfg.proxyHandler = hold
				if tt.kind == "guard" {
					body = compactRequestJSON("claude-gpt-6.1-sol[1m]")
				}
				if tt.kind == "grok" {
					body = messageRequestJSON("claude-grok-4.7[1m]", true)
					cfg.grokHandler = hold
				}
				h := newHarness(t, cfg)
				w, cancel, done := beginRequest(t, h, body)
				defer unblock()
				synctest.Wait()
				mustDiff(t, "in-flight provider", int32(1), attempts.Load())
				assertRunning(t, "provider", providerDone)
				if tt.kind == "stream" {
					time.Sleep(100 * time.Millisecond)
					synctest.Wait()
					_, _, _, streamBody := w.snapshot()
					mustDiff(t, "live heartbeat", 1, strings.Count(streamBody, ": ping\n\n"))
				}
				// Start with a nonempty state so a spurious completion or error overwrite is observable.
				seed, err := h.upstreamState.SetUnavailable(http.StatusServiceUnavailable)
				if err != nil || seed == nil {
					t.Fatal("could not seed unavailable state")
				}
				w.markDeparted()
				cancel()
				synctest.Wait()
				time.Sleep(300 * time.Millisecond)
				synctest.Wait()
				assertRunning(t, "provider after departure", providerDone)
				assertRunning(t, "handler after departure", done)
				mustDiff(t, "active requests before provider EOF", any(float64(0)), h.healthz()["activeRequests"])
				midResults := snapshotResults()
				mustDiff(t, "terminal results before provider EOF", 1, len(midResults))
				mustDiff(t, "root cancellation", true, midResults[0].Cancelled)
				mustDiff(t, "attempts before release", int32(1), attempts.Load())
				_, late, _, _ := w.snapshot()
				mustDiff(t, "no writes or heartbeats after departure", 0, late)
				unblock()
				waitSignal(t, "handler after release", done, time.Second)
				synctest.Wait()
				mustDiff(t, "no new attempt", int32(1), attempts.Load())
				mustDiff(t, "terminal result count", 1, len(snapshotResults()))
				_, late, _, _ = w.snapshot()
				mustDiff(t, "no late tail", 0, late)
				mustDiff(t, "unchanged Codex state", seed, h.upstreamState.ReadUnavailable())
				h.telemetryRec.Wait()
				mustDiff(t, "no cancelled span", 0, collector.count())
			})
		})
	}
}

func TestShutdownCancelsInFlightWork(t *testing.T) {
	tests := map[string]struct {
		kind   string
		depart bool
	}{
		"success: native stream":          {"native", false},
		"success: departed native stream": {"native", true},
		"success: proxy stream":           {"proxy", false},
		"success: guarded stream":         {"guard", false},
		"success: grok stream":            {"grok", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				entered := make(chan struct{})
				providerDone := make(chan struct{})
				enteredOnce := sync.OnceFunc(func() { close(entered) })
				doneOnce := sync.OnceFunc(func() { close(providerDone) })
				hold := func(w http.ResponseWriter, r *http.Request) {
					defer doneOnce()
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					frame := proxyStart
					if tt.kind == "grok" {
						frame = grokStart
					}
					_, _ = io.WriteString(w, frame)
					w.(http.Flusher).Flush()
					enteredOnce()
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
				cfg := harnessConfig{inMemory: true, disableUsage: true, disableTrace: true, heartbeatInterval: -1}
				body := messageRequestJSON("claude-gpt-6.1-sol[1m]", true)
				switch tt.kind {
				case "native":
					cfg.anthropicHandler = hold
					body = messageRequestJSON("claude-sonnet-4-6", true)
				case "grok":
					cfg.grokHandler = hold
					body = messageRequestJSON("claude-grok-4.7[1m]", true)
				case "guard":
					cfg.proxyHandler = hold
					body = compactRequestJSON("claude-gpt-6.1-sol[1m]")
				default:
					cfg.proxyHandler = hold
				}
				h := newHarness(t, cfg)
				w, cancel, done := beginRequest(t, h, body)
				waitSignal(t, "provider entry", entered, time.Second)
				synctest.Wait()
				if tt.depart {
					_, _, _, nativeBody := w.snapshot()
					if len(nativeBody) == 0 {
						t.Fatal("native stream did not reach client")
					}
					w.markDeparted()
					cancel()
					synctest.Wait()
					assertRunning(t, "native provider", providerDone)
					assertRunning(t, "native handler", done)
				}
				h.cancelService()
				waitSignal(t, "provider shutdown", providerDone, time.Second)
				waitSignal(t, "handler shutdown", done, time.Second)
				_, late, _, _ := w.snapshot()
				mustDiff(t, "late writes", 0, late)
			})
		})
	}
}
