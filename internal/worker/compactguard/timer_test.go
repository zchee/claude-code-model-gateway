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
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func TestGuardRequiresServiceContext(t *testing.T) {
	req := guardRequest()
	req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
		t.Error("contacted upstream without a service context")
		return nil, outcome.Result{}, nil
	}
	r := guardRunner(t.Context(), 1)
	r.ServiceContext = nil
	if _, err := r.Serve(t.Context(), httptest.NewRecorder(), req); err == nil {
		t.Error("missing service context accepted")
	}
}

type peekReadBody struct {
	*io.PipeReader
	started chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (b *peekReadBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.PipeReader.Read(p)
}
func (b *peekReadBody) Close() error { b.closes.Add(1); return b.PipeReader.Close() }

func TestGuardPeekLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stop := context.WithCancel(t.Context())
		defer stop()
		client, disconnect := context.WithCancel(t.Context())
		defer disconnect()
		rd, wr := io.Pipe()
		defer func() { _ = wr.Close() }()
		body := &peekReadBody{PipeReader: rd, started: make(chan struct{})}
		req := guardRequest()
		req.ProxyProvider = "lmstudio"
		req.Stream.Provider = "lmstudio"
		req.Stream.FirstEventWait = time.Hour
		calls := 0
		req.Next = func(ctx context.Context, head bool) (*upstream.Response, outcome.Result, error) {
			calls++
			if head || ctx.Done() != client.Done() {
				t.Error("first dispatch must receive the client lifetime before commitment")
			}
			return sseResponse(body), outcome.Result{Contacted: true}, nil
		}
		done := make(chan error, 1)
		go func() { _, err := guardRunner(service, 1).Serve(client, httptest.NewRecorder(), req); done <- err }()
		select {
		case <-body.started:
		case err := <-done:
			t.Fatalf("Serve ended before peek started: %v", err)
		case <-time.After(time.Second):
			_ = wr.Close()
			t.Fatal("peek did not start")
		}
		disconnect()
		synctest.Wait()
		if n := body.closes.Load(); n != 0 {
			t.Errorf("client disconnect closed the in-flight peek %d times", n)
		}
		stop()
		synctest.Wait()
		select {
		case <-done:
		case <-time.After(time.Second):
			_ = wr.Close()
			t.Fatal("worker shutdown did not stop the peek")
		}
		if n := body.closes.Load(); n != 1 || calls != 1 {
			t.Errorf("body closes=%d, attempts=%d; want one each", n, calls)
		}
	})
}

func TestGuardRetryDelaySynctest(t *testing.T) {
	tests := map[string]struct {
		millis float64
		delay  time.Duration
		cancel bool
	}{
		"success: configured delay":                                    {250, 250 * time.Millisecond, false},
		"success: fractional milliseconds truncate for the timer only": {12.5, 12 * time.Millisecond, false},
		"success: out of range timeout uses minimum timer":             {1e30, time.Millisecond, false},
		"success: zero delay uses minimum timer":                       {0, time.Millisecond, false},
		"success: client loss stops pending retry":                     {250, 100 * time.Millisecond, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, cancel := context.WithCancel(t.Context())
				defer cancel()
				r := guardRunner(t.Context(), 1)
				r.Settings.RetryDelayMillis = tt.millis
				req := guardRequest()
				start := time.Now()
				var times []time.Duration
				var heads []bool
				req.Next = func(ctx context.Context, head bool) (*upstream.Response, outcome.Result, error) {
					if ctx.Done() != client.Done() {
						t.Error("attempt did not receive the client context")
					}
					heads = append(heads, head)
					times = append(times, time.Since(start))
					stream := truncatedStream()
					if head {
						stream = completeStream("delayed ok")
					}
					return sseResponse(io.NopCloser(strings.NewReader(stream))), outcome.Result{Contacted: true}, nil
				}
				var timer *time.Timer
				if tt.cancel {
					timer = time.AfterFunc(tt.delay, cancel)
					defer timer.Stop()
				}
				w := httptest.NewRecorder()
				_, err := r.Serve(client, w, req)
				if err != nil && (!tt.cancel || !errors.Is(err, context.Canceled)) {
					t.Fatalf("Serve: %v", err)
				}
				wantTimes := []time.Duration{0}
				wantHeads := []bool{false}
				if !tt.cancel {
					wantTimes = append(wantTimes, tt.delay)
					wantHeads = append(wantHeads, true)
					if diff := gocmp.Diff(delivered(completeStream("delayed ok")), w.Body.String()); diff != "" {
						t.Error(diff)
					}
				}
				if diff := gocmp.Diff(wantTimes, times); diff != "" {
					t.Error(diff)
				}
				if diff := gocmp.Diff(wantHeads, heads); diff != "" {
					t.Error(diff)
				}
				if elapsed := time.Since(start); elapsed != tt.delay {
					t.Errorf("Serve elapsed %s, want %s", elapsed, tt.delay)
				}
			})
		})
	}
}

type heartbeatRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	start   time.Time
	flushed []time.Duration
}

func (r *heartbeatRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ResponseRecorder.Write(b)
}

func (r *heartbeatRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ResponseRecorder.Flush()
	for n := bytes.Count(r.Body.Bytes(), []byte(": ping\n\n")); len(r.flushed) < n; {
		r.flushed = append(r.flushed, time.Since(r.start))
	}
}

func TestGuardHeartbeatSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := guardRunner(t.Context(), 1)
		r.Settings.RetryDelayMillis = 250
		req := guardRequest()
		req.Stream.HeartbeatInterval = 20 * time.Second
		w := &heartbeatRecorder{ResponseRecorder: httptest.NewRecorder(), start: time.Now()}
		var wg sync.WaitGroup
		var readers []*io.PipeReader
		var heads []bool
		req.Next = func(_ context.Context, head bool) (*upstream.Response, outcome.Result, error) {
			heads = append(heads, head)
			rd, wr := io.Pipe()
			readers = append(readers, rd)
			wg.Go(func() {
				defer func() { _ = wr.Close() }()
				time.Sleep(30 * time.Second)
				if _, err := io.WriteString(wr, frameMessageStart); err != nil {
					return
				}
				time.Sleep(25 * time.Second)
				if head {
					_, _ = io.WriteString(wr, frameTextDelta("still alive")+frameMessageStop)
				}
			})
			return sseResponse(rd), outcome.Result{Contacted: true}, nil
		}
		_, err := r.Serve(t.Context(), w, req)
		for _, rd := range readers {
			_ = rd.Close()
		}
		wg.Wait()
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
		if diff := gocmp.Diff([]bool{false, true}, heads); diff != "" {
			t.Error(diff)
		}
		if diff := gocmp.Diff([]time.Duration{20 * time.Second, 50 * time.Second, 75250 * time.Millisecond, 105250 * time.Millisecond}, w.flushed); diff != "" {
			t.Error(diff)
		}
		if diff := gocmp.Diff(strings.Repeat(": ping\n\n", 4)+advertisedStart+frameTextDelta("still alive")+frameMessageStop, w.Body.String()); diff != "" {
			t.Error(diff)
		}
	})
}
