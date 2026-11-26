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
	"slices"
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
		w := httptest.NewRecorder()
		done := make(chan error, 1)
		joined := false
		go func() { _, err := guardRunner(service, 1).Serve(client, w, req); done <- err }()
		defer func() {
			if !joined {
				stop()
				_ = wr.Close()
				<-done
			}
		}()
		synctest.Wait()
		select {
		case <-body.started:
		case err := <-done:
			joined = true
			t.Fatalf("Serve ended before peek started: %v", err)
		default:
			t.Fatal("peek did not start")
		}
		disconnect()
		synctest.Wait()
		if n := body.closes.Load(); n != 0 {
			t.Errorf("client disconnect closed the in-flight peek %d times", n)
		}
		select {
		case err := <-done:
			joined = true
			t.Fatalf("client disconnect ended in-flight peek: %v", err)
		default:
		}
		stop()
		synctest.Wait()
		var err error
		select {
		case err = <-done:
			joined = true
		default:
			t.Fatal("worker shutdown did not stop the peek")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("worker shutdown error = %v, want %v", err, context.Canceled)
		}
		if n := body.closes.Load(); n != 1 || calls != 1 {
			t.Errorf("body closes=%d, attempts=%d; want one each", n, calls)
		}
		if w.Body.Len() != 0 {
			t.Errorf("wrote body on shutdown: %q", w.Body.String())
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
				res, err := r.Serve(client, w, req)
				if err != nil {
					t.Fatalf("Serve: %v", err)
				}
				if res.Cancelled {
					t.Errorf("Serve set Cancelled = true, want false")
				}
				wantTimes := []time.Duration{0}
				wantHeads := []bool{false}
				if !tt.cancel {
					wantTimes = append(wantTimes, tt.delay)
					wantHeads = append(wantHeads, true)
					if diff := gocmp.Diff(delivered(completeStream("delayed ok")), w.Body.String()); diff != "" {
						t.Error(diff)
					}
				} else if w.Body.Len() != 0 {
					t.Errorf("wrote body after client cancel: %q", w.Body.String())
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

func (r *heartbeatRecorder) snapshotFlushed() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.flushed)
}

func (r *heartbeatRecorder) bodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Body.String()
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

func TestGuardHeartbeatStopsOnClientDisconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, stop := context.WithCancel(t.Context())
		defer stop()
		client, disconnect := context.WithCancel(t.Context())
		defer disconnect()
		r := guardRunner(service, 1)
		r.Settings.RetryDelayMillis = 250
		req := guardRequest()
		req.Stream.HeartbeatInterval = 20 * time.Second
		w := &heartbeatRecorder{ResponseRecorder: httptest.NewRecorder(), start: time.Now()}
		rd, wr := io.Pipe()
		defer func() { _ = wr.Close() }()
		body := &countedBody{ReadCloser: rd}
		var calls atomic.Int32
		req.Next = func(ctx context.Context, head bool) (*upstream.Response, outcome.Result, error) {
			if ctx.Done() != client.Done() {
				t.Error("Next did not receive client liveness")
			}
			if calls.Add(1) > 1 || head {
				t.Errorf("unexpected Next call %d head=%v", calls.Load(), head)
				return nil, outcome.Result{}, errors.New("unexpected retry")
			}
			return sseResponse(body), outcome.Result{Contacted: true}, nil
		}
		type serveResult struct {
			res outcome.Result
			err error
		}
		done := make(chan serveResult, 1)
		joined := false
		go func() {
			res, err := r.Serve(client, w, req)
			done <- serveResult{res: res, err: err}
		}()
		defer func() {
			if !joined {
				stop()
				_ = wr.Close()
				<-done
			}
		}()
		time.Sleep(25 * time.Second)
		synctest.Wait()
		select {
		case got := <-done:
			joined = true
			t.Fatalf("Serve ended before client disconnect: %v", got.err)
		default:
		}
		if diff := gocmp.Diff([]time.Duration{20 * time.Second}, w.snapshotFlushed()); diff != "" {
			t.Fatalf("heartbeats before disconnect (-want +got):\n%s", diff)
		}
		disconnect()
		synctest.Wait()
		if n := body.closes.Load(); n != 0 {
			t.Errorf("client disconnect closed upstream body %d times", n)
		}
		select {
		case got := <-done:
			joined = true
			t.Fatalf("Serve returned before upstream ended: %v", got.err)
		default:
		}
		time.Sleep(35 * time.Second)
		synctest.Wait()
		if n := body.closes.Load(); n != 0 {
			t.Errorf("upstream body closed %d times while still open", n)
		}
		select {
		case got := <-done:
			joined = true
			t.Fatalf("Serve returned while upstream was still silent: %v", got.err)
		default:
		}
		if diff := gocmp.Diff([]time.Duration{20 * time.Second}, w.snapshotFlushed()); diff != "" {
			t.Errorf("heartbeats after disconnect (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(": ping\n\n", w.bodyString()); diff != "" {
			t.Errorf("body after disconnect (-want +got):\n%s", diff)
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
			t.Fatal("Serve did not return after upstream ended")
		}
		if got.err != nil {
			t.Errorf("Serve error = %v, want nil", got.err)
		}
		if got.res.Cancelled {
			t.Errorf("Serve set Cancelled = true, want false")
		}
		if n := body.closes.Load(); n != 1 || calls.Load() != 1 {
			t.Errorf("body closes=%d, attempts=%d; want one each", n, calls.Load())
		}
		if diff := gocmp.Diff([]time.Duration{20 * time.Second}, w.snapshotFlushed()); diff != "" {
			t.Errorf("final heartbeats (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(": ping\n\n", w.bodyString()); diff != "" {
			t.Errorf("final body (-want +got):\n%s", diff)
		}
	})
}
