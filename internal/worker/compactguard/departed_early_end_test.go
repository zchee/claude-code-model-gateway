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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// TestGuardDepartedClientPeekEarlyEnd checks an LM Studio stream that breaks before its first
// data event while the service stays live. A client that left while the peek was in flight
// receives no 502, no header field and no body, and the guard still returns the upstream status
// with the peek's aborted override and no error; a live client receives the 502 JSON reply with
// the same result. Neither case calls the overflow responder or retries.
func TestGuardDepartedClientPeekEarlyEnd(t *testing.T) {
	tests := map[string]struct {
		disconnect bool
		wantHeads  []int
		wantType   string
		wantBody   string
	}{
		"success: departed client gets no reply and the result keeps the peek override": {
			disconnect: true,
		},
		"error: live client receives the early-end reply": {
			wantHeads: []int{http.StatusBadGateway},
			wantType:  "application/json",
			wantBody:  peekEarlyEndBody,
		},
	}
	wantRes := outcome.Result{Contacted: true, Status: http.StatusOK, StatusOverride: "upstream_aborted"}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
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
				req.Stream.OnOverflow = func(_ http.ResponseWriter, status int, counts sentry.Counts) (outcome.Result, error) {
					calls = append(calls, overflowCall{Status: status, Counts: counts})
					return outcome.Result{}, nil
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
				if tt.disconnect {
					disconnect()
					synctest.Wait()
					select {
					case got := <-done:
						joined = true
						t.Fatalf("client departure ended the peek in flight: %v", got.err)
					default:
					}
				}
				go func() {
					_, _ = io.WriteString(wr, "event: message_start\ndata: {\"type\"")
					_ = wr.CloseWithError(errPeekReset)
				}()
				synctest.Wait()
				var got serveResult
				select {
				case got = <-done:
					joined = true
				default:
					t.Fatal("Serve did not return after the peek reported an early end")
				}
				if got.err != nil {
					t.Errorf("Serve error = %v, want nil", got.err)
				}
				if diff := gocmp.Diff(wantRes, got.res); diff != "" {
					t.Errorf("Serve result (-want +got):\n%s", diff)
				}
				if len(calls) != 0 {
					t.Errorf("overflow responder called %v, want no call for an early end", calls)
				}
				if diff := gocmp.Diff([]bool{false}, heads); diff != "" {
					t.Errorf("Next head arguments, one attempt and no retry (-want +got):\n%s", diff)
				}
				if n := body.closes.Load(); n != 1 {
					t.Errorf("upstream body closed %d times, want 1", n)
				}
				if diff := gocmp.Diff(tt.wantHeads, w.heads); diff != "" {
					t.Errorf("client heads (-want +got):\n%s", diff)
				}
				if tt.disconnect {
					if h := w.Header(); len(h) != 0 {
						t.Errorf("departed client received headers %v", h)
					}
				} else if gotType := w.Header().Get("Content-Type"); gotType != tt.wantType {
					t.Errorf("client Content-Type = %q, want %q", gotType, tt.wantType)
				}
				if diff := gocmp.Diff(tt.wantBody, w.bodyString()); diff != "" {
					t.Errorf("client body (-want +got):\n%s", diff)
				}
				if tt.disconnect && w.rec.Flushed {
					t.Error("departed client response was flushed")
				}
			})
		})
	}
}
