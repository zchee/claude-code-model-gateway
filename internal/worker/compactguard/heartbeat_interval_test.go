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

package compactguard_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// gatedBeatWriter is a concurrency-safe response writer that reports the virtual time of the first
// heartbeat written to it and then holds the writing goroutine until gate is closed.
type gatedBeatWriter struct {
	mu     sync.Mutex
	header http.Header
	heads  []int
	body   bytes.Buffer
	start  time.Time
	beats  chan time.Duration
	gate   chan struct{}
}

var (
	_ http.ResponseWriter = (*gatedBeatWriter)(nil)
	_ http.Flusher        = (*gatedBeatWriter)(nil)
)

func (w *gatedBeatWriter) Header() http.Header { return w.header }

func (w *gatedBeatWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.heads = append(w.heads, code)
}

func (w *gatedBeatWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	if len(w.heads) == 0 {
		w.heads = append(w.heads, http.StatusOK)
	}
	w.body.Write(b)
	w.mu.Unlock()
	if bytes.Contains(b, []byte(sse.HeartbeatComment)) {
		select {
		case w.beats <- time.Since(w.start):
		default:
		}
		<-w.gate
	}
	return len(b), nil
}

func (w *gatedBeatWriter) Flush() {}

func (w *gatedBeatWriter) snapshot() (heads []int, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.heads), w.body.String()
}

// TestGuardHeartbeatIntervalTakenAsGiven checks that the guard uses its configured heartbeat
// interval as a Go duration, on its own and not through the stream relay: a fraction of a
// millisecond is kept and an interval beyond the largest delay a JavaScript timer accepts is kept.
// The source's timers would fire one millisecond early for the first and after one millisecond for
// the second. The first heartbeat is timed by the bubble's virtual clock as the guard writes it.
//
// The writer holds the guard inside its first heartbeat write until the test lets it go, so a
// guard that armed a much shorter timer writes one heartbeat, not one per millisecond of a virtual
// wait that lasts weeks. A virtual watchdog ends the wait when no heartbeat comes at all.
func TestGuardHeartbeatIntervalTakenAsGiven(t *testing.T) {
	// literalHeartbeat restates sse.HeartbeatComment.
	const literalHeartbeat = ": ping\n\n"
	// watchdogSlack is how long after its interval a heartbeat may still be awaited before the
	// test reports that none came.
	const watchdogSlack = time.Second
	// javascriptTimerMax restates the guard's maxTimerMillis, the largest delay in milliseconds
	// a JavaScript timer accepts.
	const javascriptTimerMax = 1<<31 - 1
	summary := completeStream("summary after a heartbeat")

	tests := map[string]struct {
		interval time.Duration
	}{
		"success: the guard keeps the sub-millisecond part of its heartbeat interval": {
			interval: 1500 * time.Microsecond,
		},
		"success: the guard keeps a heartbeat interval beyond the JavaScript timer maximum": {
			interval: time.Duration(javascriptTimerMax+1)*time.Millisecond + 500*time.Microsecond,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := &gatedBeatWriter{header: http.Header{}, start: time.Now(), beats: make(chan time.Duration, 1), gate: make(chan struct{})}
				gateOpen := false
				openGate := func() {
					if !gateOpen {
						gateOpen = true
						close(w.gate)
					}
				}
				rd, wr := io.Pipe()
				body := &countedBody{ReadCloser: rd}
				req := guardRequest()
				req.Stream.HeartbeatInterval = tt.interval
				dispatches := 0
				req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
					dispatches++
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				type serveResult struct {
					res outcome.Result
					err error
				}
				done := make(chan serveResult, 1)
				go func() {
					res, err := guardRunner(t.Context(), 1).Serve(t.Context(), w, req)
					done <- serveResult{res: res, err: err}
				}()
				joined := false
				defer func() {
					openGate()
					if !joined {
						_ = wr.CloseWithError(io.ErrUnexpectedEOF)
						<-done
					}
				}()

				watchdog := time.NewTimer(tt.interval + watchdogSlack)
				select {
				case elapsed := <-w.beats:
					watchdog.Stop()
					if elapsed != tt.interval {
						t.Errorf("first heartbeat written at %v, want exactly its interval %v", elapsed, tt.interval)
					}
				case <-watchdog.C:
					t.Fatalf("no heartbeat written by %v, its interval %v", time.Since(w.start), tt.interval)
				}
				if _, got := w.snapshot(); got != literalHeartbeat {
					t.Errorf("client body at the first heartbeat = %q, want one heartbeat %q", got, literalHeartbeat)
				}

				openGate()
				if _, err := io.WriteString(wr, summary); err != nil {
					t.Fatalf("write upstream summary: %v", err)
				}
				if err := wr.Close(); err != nil {
					t.Fatalf("close upstream pipe: %v", err)
				}
				synctest.Wait()
				var got serveResult
				select {
				case got = <-done:
					joined = true
				default:
					t.Fatal("Serve did not return after the upstream ended")
				}
				if got.err != nil {
					t.Errorf("Serve error = %v, want nil", got.err)
				}
				if got.res.Status != http.StatusOK || got.res.StatusOverride != "" {
					t.Errorf("result status=%d override=%q, want 200 and none", got.res.Status, got.res.StatusOverride)
				}
				heads, gotBody := w.snapshot()
				if diff := gocmp.Diff([]int{http.StatusOK}, heads); diff != "" {
					t.Errorf("response heads (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(literalHeartbeat+delivered(summary), gotBody); diff != "" {
					t.Errorf("client body (-want +got):\n%s", diff)
				}
				if dispatches != 1 {
					t.Errorf("attempts dispatched = %d, want 1", dispatches)
				}
				if n := body.closes.Load(); n != 1 {
					t.Errorf("body closed %d times, want 1", n)
				}
			})
		})
	}
	if diff := gocmp.Diff(literalHeartbeat, sse.HeartbeatComment); diff != "" {
		t.Errorf("sse.HeartbeatComment (-restated +got):\n%s", diff)
	}
}
