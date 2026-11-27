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

package upstream_test

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// replayContact is one request the upstream read in TestReplayAfterWrittenRequestOnReusedConnection.
type replayContact struct {
	// Conn counts the connections in the order the upstream accepted them, from zero.
	Conn            int
	Method          string
	URI             string
	BodyLen         int
	IdempotencyKey  string
	XIdempotencyKey string
}

// TestReplayAfterWrittenRequestOnReusedConnection checks which requests the transport sends a second
// time when a reused connection fails after the whole request was written to it. A bodyless GET,
// HEAD, OPTIONS or TRACE is sent again on a fresh connection, and so is a bodyless request of any
// other method that carries an Idempotency-Key or X-Idempotency-Key header, however the client
// spelled the name. A bodyless request of another method without such a header, and a request with
// a body whatever its headers, reach the upstream once and fail with the fixed response error.
// net/http documents which requests are eligible for a retry (an idempotent method or an idempotency
// key header, and no body or a GetBody); that the Go 1.27.4 transport does retry each eligible
// request on this simulated failure of a reused connection is observed behaviour, not documented.
func TestReplayAfterWrittenRequestOnReusedConnection(t *testing.T) {
	const (
		probeURI   = "/v1/messages/batches/probe"
		probeReply = "HTTP/1.1 204 No Content\r\n\r\n"
	)

	tests := map[string]struct {
		method string
		header http.Header
		body   []byte
		// wantKey and wantXKey are the Idempotency-Key and X-Idempotency-Key values the upstream
		// must read; empty means the header is absent.
		wantKey, wantXKey string
		wantRetry         bool
	}{
		"success: a bodyless GET is sent again on a fresh connection": {
			method:    http.MethodGet,
			wantRetry: true,
		},
		"success: a bodyless HEAD is sent again on a fresh connection": {
			method:    http.MethodHead,
			wantRetry: true,
		},
		"success: a bodyless OPTIONS is sent again on a fresh connection": {
			method:    http.MethodOptions,
			wantRetry: true,
		},
		"success: a bodyless TRACE is sent again on a fresh connection": {
			method:    http.MethodTrace,
			wantRetry: true,
		},
		"success: a bodyless DELETE carrying Idempotency-Key is sent again on a fresh connection": {
			method:    http.MethodDelete,
			header:    http.Header{"Idempotency-Key": {"delete-key-1"}},
			wantKey:   "delete-key-1",
			wantRetry: true,
		},
		"success: a bodyless DELETE carrying X-Idempotency-Key is sent again on a fresh connection": {
			method:    http.MethodDelete,
			header:    http.Header{"X-Idempotency-Key": {"delete-key-2"}},
			wantXKey:  "delete-key-2",
			wantRetry: true,
		},
		"success: a bodyless PUT whose client spelled idempotency-key in lowercase is sent again on a fresh connection": {
			method:    http.MethodPut,
			header:    http.Header{"idempotency-key": {"put-key-1"}},
			wantKey:   "put-key-1",
			wantRetry: true,
		},
		"error: a bodyless DELETE without an idempotency key reaches the upstream once": {
			method: http.MethodDelete,
		},
		"error: a bodyless PUT without an idempotency key reaches the upstream once": {
			method: http.MethodPut,
		},
		"error: a bodyless POST without an idempotency key reaches the upstream once": {
			method: http.MethodPost,
		},
		"error: a POST with a body and Idempotency-Key reaches the upstream once": {
			method:  http.MethodPost,
			header:  http.Header{"Idempotency-Key": {"post-key-1"}, "Content-Type": {"application/json"}},
			body:    []byte(`{"model":"claude-sonnet-4-6"}`),
			wantKey: "post-key-1",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var (
					accepted atomic.Int32
					mu       sync.Mutex
					contacts []replayContact
				)
				up := &pipeUpstream{mode: faultNone}
				up.serve = func(peer net.Conn, _ *faultConn) {
					conn := int(accepted.Add(1) - 1)
					defer func() { _ = peer.Close() }()
					br := bufio.NewReader(peer)
					for {
						req, err := http.ReadRequest(br)
						if err != nil {
							return
						}
						body, err := io.ReadAll(req.Body)
						_ = req.Body.Close()
						if err != nil {
							return
						}
						if req.URL.Path == "/warmup" {
							if _, err := io.WriteString(peer, okReply); err != nil {
								return
							}
							continue
						}
						mu.Lock()
						contacts = append(contacts, replayContact{
							Conn:            conn,
							Method:          req.Method,
							URI:             req.RequestURI,
							BodyLen:         len(body),
							IdempotencyKey:  req.Header.Get("Idempotency-Key"),
							XIdempotencyKey: req.Header.Get("X-Idempotency-Key"),
						})
						mu.Unlock()
						if conn == 0 {
							// The whole request was read, so the transport has written it; closing
							// now fails the reused connection before any response byte.
							return
						}
						if _, err := io.WriteString(peer, probeReply); err != nil {
							return
						}
					}
				}
				client := upstream.NewClient(upstream.Options{DialContext: up.DialContext})
				defer func() {
					_ = client.Close()
					up.peers.Wait()
				}()

				body, err := doAndDrain(t.Context(), client, upstream.Request{
					Target:     upstream.TargetAnthropic,
					BaseURL:    fakeUpstreamBase,
					Method:     http.MethodGet,
					RequestURI: "/warmup",
				})
				if err != nil {
					t.Fatalf("warm-up request failed: %v", err)
				}
				if diff := gocmp.Diff(okReplyBody, body); diff != "" {
					t.Fatalf("warm-up response body (-want +got):\n%s", diff)
				}
				// Let the transport park the warm connection, so the probe reuses it.
				synctest.Wait()

				resp, err := client.Do(t.Context(), upstream.Request{
					Target:        upstream.TargetAnthropic,
					BaseURL:       fakeUpstreamBase,
					Method:        tt.method,
					RequestURI:    probeURI,
					ClientHeaders: tt.header,
					Body:          tt.body,
				})
				if resp != nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				mu.Lock()
				got := slices.Clone(contacts)
				mu.Unlock()

				first := replayContact{
					Conn:            0,
					Method:          tt.method,
					URI:             probeURI,
					BodyLen:         len(tt.body),
					IdempotencyKey:  tt.wantKey,
					XIdempotencyKey: tt.wantXKey,
				}
				want := []replayContact{first}
				wantDials := int32(1)
				if tt.wantRetry {
					second := first
					second.Conn = 1
					want = append(want, second)
					wantDials = 2
					if err != nil {
						t.Fatalf("Do = %v, want the request sent again on a fresh connection and answered", err)
					}
					if diff := gocmp.Diff(http.StatusNoContent, resp.StatusCode); diff != "" {
						t.Errorf("status of the request sent again (-want +got):\n%s", diff)
					}
				} else {
					if resp != nil {
						t.Fatalf("Do returned status %d, want the failure of the only attempt", resp.StatusCode)
					}
					if err == nil {
						t.Fatal("Do returned neither a response nor an error")
					}
					if diff := gocmp.Diff([]string{responseFailureText}, errorTexts(err)); diff != "" {
						t.Errorf("texts of the error of the only attempt (-want +got):\n%s", diff)
					}
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("requests the upstream read (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(wantDials, up.dials.Load()); diff != "" {
					t.Errorf("dials, the warm-up connection included (-want +got):\n%s", diff)
				}
			})
		})
	}
}
