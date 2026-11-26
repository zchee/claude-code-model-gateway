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
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
)

func sseResponse(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       body,
	}
}

func TestGuardRetryDelaySynctest(t *testing.T) {
	t.Parallel()

	t.Run("success: retry waits configured delay before issuing second attempt", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			const delay = 250 * time.Millisecond
			var attemptTimes []time.Time

			runner := compactguard.Runner{
				Settings: compactguard.Settings{
					StreamGuard:      true,
					Retries:          1,
					MaxBufferedBytes: 16 * 1024 * 1024,
					RetryDelay:       delay,
				},
			}
			rec := httptest.NewRecorder()
			req := compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       "/v1/messages",
				UpstreamOrigin: "http://127.0.0.1:18765",
				Body:           compactRequestBody("claude-gpt-5.6-sol", "Summarize."),
				Upstream: compactguard.UpstreamFunc(func(_ context.Context) (*http.Response, error) {
					attemptTimes = append(attemptTimes, time.Now())
					if len(attemptTimes) == 1 {
						return sseResponse(io.NopCloser(strings.NewReader(truncatedStream()))), nil
					}
					return sseResponse(io.NopCloser(strings.NewReader(completeStream("delayed ok")))), nil
				}),
			}

			if err := runner.Serve(t.Context(), rec, req); err != nil {
				t.Fatalf("Serve returned error: %v", err)
			}
			if diff := gocmp.Diff(2, len(attemptTimes)); diff != "" {
				t.Fatalf("attempt count mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(delay, attemptTimes[1].Sub(attemptTimes[0])); diff != "" {
				t.Fatalf("retry delay mismatch (-want +got):\n%s", diff)
			}
			if !strings.Contains(rec.Body.String(), "delayed ok") {
				t.Fatalf("response body missing retried summary: %q", rec.Body.String())
			}
		})
	})

	t.Run("success: client cancellation during retry delay prevents second attempt", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			const delay = 250 * time.Millisecond
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			attempt1Done := make(chan struct{})
			var attempts atomic.Int32
			runner := compactguard.Runner{
				Settings: compactguard.Settings{
					StreamGuard:      true,
					Retries:          1,
					MaxBufferedBytes: 16 * 1024 * 1024,
					RetryDelay:       delay,
				},
			}
			rec := httptest.NewRecorder()
			req := compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       "/v1/messages",
				UpstreamOrigin: "http://127.0.0.1:18765",
				Body:           compactRequestBody("claude-gpt-5.6-sol", "Summarize."),
				Upstream: compactguard.UpstreamFunc(func(_ context.Context) (*http.Response, error) {
					if attempts.Add(1) == 1 {
						close(attempt1Done)
						return sseResponse(io.NopCloser(strings.NewReader(truncatedStream()))), nil
					}
					return sseResponse(io.NopCloser(strings.NewReader(completeStream("must not run")))), nil
				}),
			}

			start := time.Now()
			errCh := make(chan error, 1)
			go func() {
				errCh <- runner.Serve(ctx, rec, req)
			}()

			select {
			case <-attempt1Done:
			case err := <-errCh:
				t.Fatalf("Serve exited before first attempt completed: %v", err)
			}

			synctest.Wait()
			time.Sleep(100 * time.Millisecond)
			cancel()
			synctest.Wait()

			if err := <-errCh; err != nil {
				t.Fatalf("Serve returned unexpected error on cancel during delay: %v", err)
			}
			if diff := gocmp.Diff(int32(1), attempts.Load()); diff != "" {
				t.Fatalf("canceled retry must not issue second attempt (-want +got):\n%s", diff)
			}
			if elapsed := time.Since(start); elapsed != 100*time.Millisecond {
				t.Fatalf("elapsed = %v, want 100ms", elapsed)
			}
		})
	})
}

type heartbeatRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	buf     bytes.Buffer
	pingCh  chan time.Time
	flushes int
}

func newHeartbeatRecorder() *heartbeatRecorder {
	return &heartbeatRecorder{
		header: make(http.Header),
		pingCh: make(chan time.Time, 8),
	}
}

func (r *heartbeatRecorder) Header() http.Header { return r.header }

func (r *heartbeatRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = code
	}
}

func (r *heartbeatRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.buf.Write(p)
	if bytes.Contains(p, []byte(": ping\n\n")) {
		r.pingCh <- time.Now()
	}
	return n, err
}

func (r *heartbeatRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
}

func (r *heartbeatRecorder) bodyString() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func TestGuardHeartbeatSynctest(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const interval = 20 * time.Second
		pr, pw := io.Pipe()
		defer func() { _ = pw.Close() }()

		rw := newHeartbeatRecorder()
		runner := compactguard.Runner{
			Settings: compactguard.Settings{
				StreamGuard:      true,
				Retries:          0,
				MaxBufferedBytes: 16 * 1024 * 1024,
			},
			HeartbeatInterval: interval,
		}
		req := compactguard.Request{
			ProxyProvider:  "codex",
			PathOnly:       "/v1/messages",
			UpstreamOrigin: "http://127.0.0.1:18765",
			Body:           compactRequestBody("claude-gpt-5.6-sol", "Summarize."),
			Upstream: compactguard.UpstreamFunc(func(_ context.Context) (*http.Response, error) {
				return sseResponse(pr), nil
			}),
		}

		start := time.Now()
		errCh := make(chan error, 1)
		go func() {
			errCh <- runner.Serve(t.Context(), rw, req)
		}()

		waitPing := func(step string) time.Time {
			select {
			case at := <-rw.pingCh:
				return at
			case err := <-errCh:
				_ = pw.Close()
				t.Fatalf("Serve exited before %s heartbeat: %v", step, err)
				return time.Time{}
			}
		}

		firstPing := waitPing("first")
		if diff := gocmp.Diff(interval, firstPing.Sub(start)); diff != "" {
			_ = pw.Close()
			t.Fatalf("first heartbeat time mismatch (-want +got):\n%s", diff)
		}

		// Upstream activity at t=30s resets the 20s inactivity timer so the next ping arrives at t=50s.
		time.Sleep(10 * time.Second)
		if _, err := io.WriteString(pw, frameMessageStart+frameBlockStart+frameTextDelta("still alive")); err != nil {
			t.Fatalf("write active chunk: %v", err)
		}

		secondPing := waitPing("second")
		if diff := gocmp.Diff(50*time.Second, secondPing.Sub(start)); diff != "" {
			_ = pw.Close()
			t.Fatalf("second heartbeat time mismatch (-want +got):\n%s", diff)
		}

		time.Sleep(5 * time.Second)
		if _, err := io.WriteString(pw, frameBlockStop+frameMessageDelta+frameMessageStop); err != nil {
			t.Fatalf("write terminal frames: %v", err)
		}
		if err := pw.Close(); err != nil {
			t.Fatalf("close upstream pipe: %v", err)
		}

		if err := <-errCh; err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
		synctest.Wait()

		body := rw.bodyString()
		if got := strings.Count(body, ": ping\n\n"); got != 2 {
			t.Fatalf("heartbeat ping count = %d, want 2 (body=%q)", got, body)
		}
		if !strings.Contains(body, "still alive") || !strings.Contains(body, `"type":"message_stop"`) {
			t.Fatalf("response missing buffered summary after heartbeats: %q", body)
		}
	})
}
