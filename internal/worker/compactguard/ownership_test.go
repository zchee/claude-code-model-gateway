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
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// overflowFirstEvent is an LM Studio stream whose first data event is an engine context overflow.
const overflowFirstEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":6017,\"n_ctx\":4096}}\n\n"

// TestGuardReportsContactAfterRetry checks that once an attempt has contacted the upstream,
// every result the guard returns says so, whatever the later dispatch reports.
func TestGuardReportsContactAfterRetry(t *testing.T) {
	errDispatch := errors.New("retry dispatch failed")
	tests := map[string]struct {
		// leave cancels the client context inside the retry dispatch, as a client that
		// disconnects while the reply handler starts.
		leave bool
		// stream makes the retry dispatch hand back an open response together with err.
		stream  bool
		verdict outcome.Result
		err     error
		want    outcome.Result
	}{
		"success: client leaves as the retry is dispatched": {
			leave: true,
			want:  outcome.Result{Contacted: true},
		},
		"error: retry verdict settled without contact": {
			verdict: outcome.Result{Status: http.StatusServiceUnavailable, StatusOverride: "upstream_error"},
			want:    outcome.Result{Status: http.StatusServiceUnavailable, StatusOverride: "upstream_error", Contacted: true},
		},
		"error: retry response returned with an error": {
			stream:  true,
			verdict: outcome.Result{Status: http.StatusBadGateway},
			err:     errDispatch,
			want:    outcome.Result{Status: http.StatusBadGateway, Contacted: true},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, disconnect := context.WithCancel(t.Context())
				defer disconnect()
				req := guardRequest()
				w := &headRecorder{rec: httptest.NewRecorder()}
				var bodies []*countedBody
				var heads []bool
				req.Next = func(_ context.Context, head bool) (*upstream.Response, outcome.Result, error) {
					heads = append(heads, head)
					if len(heads) == 1 {
						first := &countedBody{ReadCloser: io.NopCloser(strings.NewReader(truncatedStream()))}
						bodies = append(bodies, first)
						return sseResponse(first), outcome.Result{Contacted: true}, nil
					}
					if tt.leave {
						disconnect()
						return nil, outcome.Result{}, nil
					}
					if tt.stream {
						retry := &countedBody{ReadCloser: io.NopCloser(strings.NewReader(completeStream("never relayed")))}
						bodies = append(bodies, retry)
						return sseResponse(retry), tt.verdict, tt.err
					}
					return nil, tt.verdict, tt.err
				}
				got, err := guardRunner(t.Context(), 1).Serve(client, w, req)
				if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
					t.Fatalf("Serve error = %v, want %v", err, tt.err)
				}
				if diff := gocmp.Diff(tt.want, got); diff != "" {
					t.Errorf("Serve result (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]bool{false, true}, heads); diff != "" {
					t.Errorf("Next head arguments (-want +got):\n%s", diff)
				}
				for i, b := range bodies {
					if n := b.closes.Load(); n != 1 {
						t.Errorf("attempt %d body closed %d times, want 1", i, n)
					}
				}
				if diff := gocmp.Diff([]int{http.StatusOK}, w.heads); diff != "" {
					t.Errorf("client heads (-want +got):\n%s", diff)
				}
				if body := w.bodyString(); body != "" {
					t.Errorf("guard relayed a discarded attempt: %q", body)
				}
			})
		})
	}
}

// overflowCall records one call of the overflow responder.
type overflowCall struct {
	Status int
	Counts sentry.Counts
}

// TestGuardDepartedClientPeekOverflow checks that an engine overflow reported by the
// first-event peek after the client has gone still reaches the overflow responder, so the
// sentry learns the counts, while nothing reaches the departed client.
func TestGuardDepartedClientPeekOverflow(t *testing.T) {
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
		var heads []bool
		req.Next = func(_ context.Context, head bool) (*upstream.Response, outcome.Result, error) {
			heads = append(heads, head)
			return sseResponse(body), outcome.Result{Contacted: true}, nil
		}
		var calls []overflowCall
		var writeErrs []error
		req.Stream.OnOverflow = func(rw http.ResponseWriter, status int, counts sentry.Counts) (outcome.Result, error) {
			calls = append(calls, overflowCall{Status: status, Counts: counts})
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusRequestEntityTooLarge)
			_, err := io.WriteString(rw, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`)
			writeErrs = append(writeErrs, err)
			return outcome.Result{Status: http.StatusRequestEntityTooLarge, Contacted: true, Route: outcome.Route{Overflow: &outcome.Overflow{Phase: "upstream"}}}, nil
		}
		w := &headRecorder{rec: httptest.NewRecorder()}
		type serveResult struct {
			res outcome.Result
			err error
		}
		done := make(chan serveResult, 1)
		joined := false
		go func() {
			res, err := guardRunner(service, 1).Serve(client, w, req)
			done <- serveResult{res: res, err: err}
		}()
		defer func() {
			if !joined {
				disconnect()
				stop()
				_ = wr.Close()
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Error("Serve did not exit during cleanup")
				}
			}
		}()
		synctest.Wait()
		select {
		case <-body.started:
		case got := <-done:
			joined = true
			t.Fatalf("Serve ended before the peek started: %v", got.err)
		default:
			t.Fatal("peek did not start")
		}
		disconnect()
		synctest.Wait()
		select {
		case got := <-done:
			joined = true
			t.Fatalf("client departure ended the peek in flight: %v", got.err)
		default:
		}
		go func() {
			_, _ = io.WriteString(wr, overflowFirstEvent)
			_ = wr.Close()
		}()
		synctest.Wait()
		var got serveResult
		select {
		case got = <-done:
			joined = true
		default:
			t.Fatal("Serve did not return after the peek reported an overflow")
		}
		if got.err != nil {
			t.Errorf("Serve error = %v, want nil", got.err)
		}
		if diff := gocmp.Diff(outcome.Result{Contacted: true}, got.res); diff != "" {
			t.Errorf("Serve result for a departed client (-want +got):\n%s", diff)
		}
		wantCalls := []overflowCall{{Status: http.StatusOK, Counts: sentry.Counts{Tokens: new(6017.0), Limit: new(4096.0)}}}
		if diff := gocmp.Diff(wantCalls, calls); diff != "" {
			t.Errorf("overflow responder calls (-want +got):\n%s", diff)
		}
		for i, err := range writeErrs {
			if err != nil {
				t.Errorf("overflow responder write %d failed: %v", i, err)
			}
		}
		if diff := gocmp.Diff([]bool{false}, heads); diff != "" {
			t.Errorf("Next head arguments (-want +got):\n%s", diff)
		}
		if n := body.closes.Load(); n != 1 {
			t.Errorf("upstream body closed %d times, want 1", n)
		}
		if len(w.heads) != 0 {
			t.Errorf("departed client received status %v", w.heads)
		}
		if h := w.Header(); len(h) != 0 {
			t.Errorf("departed client received headers %v", h)
		}
		if b := w.bodyString(); b != "" {
			t.Errorf("departed client received body %q", b)
		}
	})
}

// injectedPanic is the value a test hook panics with.
type injectedPanic struct{ where string }

// panickingCapture is a real usage capture whose byte observation panics.
type panickingCapture struct {
	*usage.Capture
}

func (c panickingCapture) NoteResponseBytes(int64) { panic(injectedPanic{where: "usage"}) }

// TestGuardClosesBodyOnPanic checks that a panic in a hook the worker binds into the guard
// propagates to the caller with the upstream body closed exactly once and no reader left.
func TestGuardClosesBodyOnPanic(t *testing.T) {
	tests := map[string]struct {
		provider string
		stream   string
		bind     func(*compactguard.Request)
		want     injectedPanic
	}{
		"error: first-event peek hook panics": {
			provider: "lmstudio",
			stream:   completeStream("summary"),
			bind: func(req *compactguard.Request) {
				req.Stream.OnPeek = func() { panic(injectedPanic{where: "peek"}) }
			},
			want: injectedPanic{where: "peek"},
		},
		"error: overflow responder panics": {
			provider: "lmstudio",
			stream:   overflowFirstEvent,
			bind: func(req *compactguard.Request) {
				req.Stream.OnOverflow = func(http.ResponseWriter, int, sentry.Counts) (outcome.Result, error) {
					panic(injectedPanic{where: "overflow"})
				}
			},
			want: injectedPanic{where: "overflow"},
		},
		"error: usage capture panics while relaying": {
			provider: "codex",
			stream:   completeStream("summary"),
			bind: func(req *compactguard.Request) {
				req.Usage = panickingCapture{Capture: usage.NewCapture(usage.NewBuilder(usage.Options{Now: time.Now}), usage.Request{Backend: "codex"}, 0, nil)}
			},
			want: injectedPanic{where: "usage"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := guardRequest()
			req.ProxyProvider = tt.provider
			req.Stream.Provider = tt.provider
			tt.bind(&req)
			body := &countedBody{ReadCloser: io.NopCloser(strings.NewReader(tt.stream))}
			calls := 0
			req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
				calls++
				return sseResponse(body), outcome.Result{Contacted: true}, nil
			}
			w := &headRecorder{rec: httptest.NewRecorder()}
			recovered := func() (v any) {
				defer func() { v = recover() }()
				_, err := guardRunner(t.Context(), 1).Serve(t.Context(), w, req)
				t.Errorf("Serve returned (error %v) instead of propagating the hook's panic", err)
				return nil
			}()
			if recovered != tt.want {
				t.Errorf("recovered %#v, want %#v", recovered, tt.want)
			}
			if calls != 1 {
				t.Errorf("Next called %d times, want 1", calls)
			}
			if n := body.closes.Load(); n != 1 {
				t.Errorf("upstream body closed %d times after the panic, want 1", n)
			}
		})
	}
}

// failingWriter is a client connection that breaks at its first write or flush. It records
// every call the guard makes, including the failing one.
type failingWriter struct {
	mu        sync.Mutex
	header    http.Header
	failWrite bool
	failFlush bool
	calls     []string
}

var _ http.ResponseWriter = (*failingWriter)(nil)

func (w *failingWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.header
}

func (w *failingWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, "head "+http.StatusText(code))
}

func (w *failingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, "write")
	if w.failWrite {
		return 0, errors.New("client connection reset")
	}
	return len(b), nil
}

// FlushError is what http.ResponseController.Flush calls; it is the only flush that can fail.
func (w *failingWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, "flush")
	if w.failFlush {
		return errors.New("client connection reset")
	}
	return nil
}

func (w *failingWriter) snapshot() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

// TestGuardSilentAfterFailedClientWrite checks that once a client write or flush has failed,
// the guard sends the client nothing more (no heartbeat through long upstream silence, no
// later data), keeps reading the upstream to its end, and returns the departed result.
func TestGuardSilentAfterFailedClientWrite(t *testing.T) {
	tests := map[string]struct {
		maxBuffered float64
		failWrite   bool
		failFlush   bool
		want        []string
	}{
		"error: write fails while streaming live": {
			maxBuffered: 0,
			failWrite:   true,
			want:        []string{"head OK", "write"},
		},
		"error: flush fails while streaming live": {
			maxBuffered: 0,
			failFlush:   true,
			want:        []string{"head OK", "write", "flush"},
		},
		"error: heartbeat write fails while buffering": {
			maxBuffered: 16 << 20,
			failWrite:   true,
			want:        []string{"head OK", "write"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := guardRunner(t.Context(), 1)
				r.Settings.MaxBufferedBytes = tt.maxBuffered
				req := guardRequest()
				req.Stream.HeartbeatInterval = time.Second
				rd, wr := io.Pipe()
				body := &countedBody{ReadCloser: rd}
				var wg sync.WaitGroup
				calls := 0
				req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
					calls++
					wg.Go(func() {
						defer func() { _ = wr.Close() }()
						if _, err := io.WriteString(wr, frameMessageStart); err != nil {
							return
						}
						// Silence across many heartbeat intervals, then more data.
						time.Sleep(10 * time.Second)
						_, _ = io.WriteString(wr, frameBlockStart+frameTextDelta("after the failure")+frameBlockStop+frameMessageDelta+frameMessageStop)
					})
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				w := &failingWriter{header: http.Header{}, failWrite: tt.failWrite, failFlush: tt.failFlush}
				start := time.Now()
				got, err := r.Serve(t.Context(), w, req)
				_ = rd.Close()
				wg.Wait()
				if err != nil {
					t.Fatalf("Serve error = %v, want nil", err)
				}
				if elapsed := time.Since(start); elapsed != 10*time.Second {
					t.Errorf("Serve returned after %s, want it to read the upstream to its end at 10s", elapsed)
				}
				if diff := gocmp.Diff(outcome.Result{Contacted: true}, got); diff != "" {
					t.Errorf("Serve result (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.want, w.snapshot()); diff != "" {
					t.Errorf("calls on the client connection (-want +got):\n%s", diff)
				}
				if calls != 1 {
					t.Errorf("Next called %d times, want 1", calls)
				}
				if n := body.closes.Load(); n != 1 {
					t.Errorf("upstream body closed %d times, want 1", n)
				}
			})
		})
	}
}
