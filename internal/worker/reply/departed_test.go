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

//go:build process

package reply_test

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// watchedBody reports the first bytes Handle reads from an upstream body and records how the
// reads ended, so a test can act while a buffered read is blocked and check that it finished.
type watchedBody struct {
	io.ReadCloser
	firstBytes func()
	read       atomic.Int64
	ended      atomic.Pointer[error]
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.read.Add(int64(n))
		b.firstBytes()
	}
	if err != nil {
		b.ended.CompareAndSwap(nil, &err)
	}
	return n, err
}

// TestClientLeavesBeforeTerminalEffect checks that a client which leaves while the upstream
// attempt is being sent, or while a buffered upstream body is being read, receives nothing: the
// reply writes no status line, no body and no in-band frame, returns no error, hands no stream on,
// reports the upstream as contacted with status zero and leaves Cancelled to the caller. The
// attempt itself is not cancelled by the departure: the upstream request runs to its own end,
// a read in flight finishes, and the upstream body is closed exactly once.
func TestClientLeavesBeforeTerminalEffect(t *testing.T) {
	const (
		final     = `{"type":"message","model":"engine","stop_reason":"end_turn","content":[],"usage":{"input_tokens":3,"output_tokens":1}}`
		failure   = `{"type":"error","error":{"type":"api_error","message":"upstream failed"}}`
		splitAt   = 9
		committed = 200
	)
	tests := map[string]struct {
		// duringRead leaves after Handle has read the first bytes of the body; otherwise the
		// client leaves while the upstream still holds the request without a response.
		duringRead bool
		// hijack closes the connection without a response once the attempt is released.
		hijack bool
		// truncate declares a longer body than the upstream sends, so the read ends early.
		truncate    bool
		status      int
		contentType string
		body        string
		headWritten bool
	}{
		"error: client leaves during the send of a final 200": {
			status: 200, contentType: "application/json", body: final,
		},
		"error: client leaves during a send that fails in transport": {
			hijack: true,
		},
		"error: client leaves during the send of a committed retry": {
			status: 500, contentType: "application/json", body: failure, headWritten: true,
		},
		"error: client leaves during the read of a final 200": {
			duringRead: true, status: 200, contentType: "application/json", body: final,
		},
		"error: client leaves during a read of a final 200 that fails in transport": {
			duringRead: true, truncate: true, status: 200, contentType: "application/json", body: final,
		},
		"error: client leaves during a read of a proxy error that fails in transport": {
			duringRead: true, truncate: true, status: 500, contentType: "application/json", body: failure,
		},
		"error: client leaves during the read of a committed retry": {
			duringRead: true, status: 500, contentType: "application/json", body: failure, headWritten: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var attempts atomic.Int32
			upstreamHeld, release, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			defer releaseOnce()
			var upstreamCancelled atomic.Bool
			var upstreamWrote atomic.Int64
			// hold keeps the upstream attempt open until the test releases it, and records whether
			// the attempt was cancelled while it waited or by the time it resumed.
			hold := func(r *http.Request) bool {
				select {
				case <-release:
				case <-r.Context().Done():
					upstreamCancelled.Store(true)
					return false
				}
				if r.Context().Err() != nil {
					upstreamCancelled.Store(true)
				}
				return true
			}
			fixture := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				attempts.Add(1)
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Errorf("read upstream request body: %v", err)
				}
				if !tc.duringRead {
					close(upstreamHeld)
					if !hold(r) {
						return
					}
				}
				if tc.hijack {
					conn, _, err := http.NewResponseController(w).Hijack()
					if err != nil {
						t.Errorf("hijack upstream connection: %v", err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", tc.contentType)
				size := len(tc.body)
				if tc.truncate {
					size++
				}
				w.Header().Set("Content-Length", strconv.Itoa(size))
				w.WriteHeader(tc.status)
				n, _ := io.WriteString(w, tc.body[:splitAt])
				upstreamWrote.Add(int64(n))
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("flush first upstream bytes: %v", err)
				}
				if tc.duringRead && !hold(r) {
					return
				}
				n, _ = io.WriteString(w, tc.body[splitAt:])
				upstreamWrote.Add(int64(n))
			})

			var (
				forwardCtx context.Context
				firstBody  *observedBody
				watched    *watchedBody
				sendErr    error
			)
			readStarted := make(chan struct{})
			readOnce := sync.OnceFunc(func() { close(readStarted) })
			req := reply.Request{Provider: "codex", AdvertisedModel: "shown", BackendModel: "engine", Body: []byte(`{"messages":[]}`), Compaction: tc.headWritten, HeadWritten: tc.headWritten}
			base := fixture.forward(req.Body)
			send := func(callCtx context.Context) (*upstream.Response, error) {
				forwardCtx = callCtx
				res, err := base(callCtx)
				sendErr = err
				if err == nil {
					firstBody, _ = res.Body.(*observedBody)
					watched = &watchedBody{ReadCloser: res.Body, firstBytes: readOnce}
					res.Body = watched
				}
				return res, err
			}

			w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
			var pre []int
			if tc.headWritten {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(committed)
				pre = []int{committed}
			}
			var result reply.Result
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				result, err = reply.Handle(ctx, w, req, send)
			}()
			defer func() {
				releaseOnce()
				<-done
			}()

			leaveAt := upstreamHeld
			if tc.duringRead {
				leaveAt = readStarted
			}
			select {
			case <-leaveAt:
			case <-done:
				t.Fatalf("Handle returned before the client could leave mid-attempt: err=%v result=%+v", err, result)
			}
			cancel()
			releaseOnce()
			<-done
			<-handlerDone

			if forwardCtx != nil && forwardCtx.Err() != nil {
				t.Errorf("attempt context ended by the client leaving: %v", forwardCtx.Err())
			}
			equal(t, false, upstreamCancelled.Load())
			equal(t, int32(1), attempts.Load())
			if err != nil {
				t.Errorf("Handle error = %v, want nil", err)
			}
			equal(t, pre, w.heads)
			equal(t, "", w.Body.String())
			if result.Stream != nil {
				_ = result.Stream.Body.Close()
				t.Error("a departed client's reply handed a stream on")
			}
			equal(t, 0, result.Status)
			equal(t, "", result.StatusOverride)
			equal(t, true, result.Contacted)
			equal(t, false, result.Cancelled)

			if tc.hijack {
				if sendErr == nil {
					t.Fatal("hijacked upstream connection produced a response")
				}
				return
			}
			if sendErr != nil {
				t.Fatalf("upstream attempt failed: %v", sendErr)
			}
			if firstBody == nil {
				t.Fatal("upstream body was not observed")
			}
			equal(t, int32(1), firstBody.closes.Load())
			equal(t, int64(len(tc.body)), upstreamWrote.Load())
			if tc.duringRead {
				// The read that was blocked when the client left ran to the upstream's end.
				equal(t, int64(len(tc.body)), watched.read.Load())
				ended := watched.ended.Load()
				if ended == nil {
					t.Fatal("buffered read did not reach the end of the upstream body")
				}
				if tc.truncate {
					if errors.Is(*ended, io.EOF) {
						t.Errorf("truncated upstream body ended cleanly")
					}
				} else if !errors.Is(*ended, io.EOF) {
					t.Errorf("buffered read ended with %v, want io.EOF", *ended)
				}
			}
		})
	}
}

// TestNegativeRetrySettingsUseDefaults checks that a negative retry count or retry delay falls
// back to the default of two retries separated by 250 milliseconds instead of being clamped.
func TestNegativeRetrySettingsUseDefaults(t *testing.T) {
	const rejection = `{"error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`
	const exhausted = `{"type":"error","error":{"type":"api_error","message":"model-gateway: Codex WebSocket upgrade was temporarily rejected after 3 attempts; retry the request."}}`
	defaults := []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond}
	tests := map[string]struct {
		retries *int
		delay   *time.Duration
	}{
		"success: negative retry count":          {retries: new(-1)},
		"success: negative retry delay":          {delay: new(-time.Millisecond)},
		"success: negative count and delay":      {retries: new(-3), delay: new(-250 * time.Millisecond)},
		"success: most negative count and delay": {retries: new(math.MinInt), delay: new(time.Duration(math.MinInt64))},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := reply.Request{Provider: "codex", AdvertisedModel: "public", Body: []byte(`{}`), Retries: tc.retries, RetryDelay: tc.delay}
			var attempts atomic.Int32
			fixture := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, rejection)
			})
			send := outsideClock(t, fixture.forward(req.Body))
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var calls []time.Duration
				w := httptest.NewRecorder()
				res, err := reply.Handle(t.Context(), w, req, func(ctx context.Context) (*upstream.Response, error) {
					calls = append(calls, time.Since(start))
					if len(calls) > len(defaults) {
						return nil, errors.New("unexpected extra attempt")
					}
					return send(ctx)
				})
				if err != nil {
					t.Fatal(err)
				}
				equal(t, defaults, calls)
				equal(t, int32(len(defaults)), attempts.Load())
				equal(t, http.StatusServiceUnavailable, w.Code)
				equal(t, exhausted, w.Body.String())
				equal(t, http.StatusServiceUnavailable, res.Status)
				equal(t, "upstream_error", res.StatusOverride)
				equal(t, true, res.Contacted)
				equal(t, true, res.Stream == nil)
			})
		})
	}
}
