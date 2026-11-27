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
	"context"
	"crypto/tls"
	"errors"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// wireMarker is text only the fake upstream sends; an error that contains it carries upstream
// bytes.
const wireMarker = "upstream-wire-marker-5e1f"

// errorTexts returns the message of err and of every error it wraps.
func errorTexts(err error) []string {
	var texts []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		texts = append(texts, e.Error())
		switch u := e.(type) { //nolint:errorlint // The walk visits each wrapped error itself, one level at a time.
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	return texts
}

// requireNoWireText fails t when err or any error it wraps contains wireMarker.
func requireNoWireText(t *testing.T, err error) {
	t.Helper()
	for _, text := range errorTexts(err) {
		if strings.Contains(text, wireMarker) {
			t.Errorf("error carries upstream bytes %q: %q", wireMarker, text)
		}
	}
}

func TestDoHidesMalformedUpstreamText(t *testing.T) {
	tests := map[string]struct {
		reply string
	}{
		"error: a reply that is not an HTTP status line": {
			reply: wireMarker + "\r\n\r\n",
		},
		"error: a status line with a malformed status code": {
			reply: "HTTP/1.1 2" + wireMarker + " OK\r\n\r\n",
		},
		"error: a status line with a malformed protocol version": {
			reply: "HTTP/1." + wireMarker + " 200 OK\r\n\r\n",
		},
		"error: a header line without a colon": {
			reply: "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n" + wireMarker + "\r\nContent-Length: 2\r\n\r\n{}",
		},
		"error: a first header line that starts with whitespace": {
			reply: "HTTP/1.1 200 OK\r\n " + wireMarker + "\r\nContent-Length: 2\r\n\r\n{}",
		},
		"error: an invalid Content-Length value": {
			reply: "HTTP/1.1 200 OK\r\nContent-Length: " + wireMarker + "\r\n\r\n{}",
		},
		"error: conflicting Content-Length values": {
			reply: "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Length: 3" + wireMarker + "\r\n\r\n{}",
		},
		"error: an unsupported Transfer-Encoding value": {
			reply: "HTTP/1.1 200 OK\r\nTransfer-Encoding: " + wireMarker + "\r\n\r\n{}",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				up := &pipeUpstream{mode: faultNone}
				up.serve = func(peer net.Conn, _ *faultConn) { replyAfterFirstByte(peer, tt.reply) }
				client := upstream.NewClient(upstream.Options{DialContext: up.DialContext})
				defer func() {
					_ = client.Close()
					up.peers.Wait()
				}()

				resp, err := client.Do(t.Context(), messagesRequest())
				if resp != nil {
					_ = resp.Body.Close()
					t.Fatalf("Do returned status %d for a malformed reply, want an error", resp.StatusCode)
				}
				if err == nil {
					t.Fatal("Do returned neither a response nor an error")
				}
				requireNoWireText(t, err)
				// The fixed sentence alone, wrapping nothing that could carry the reply.
				if diff := gocmp.Diff([]string{responseFailureText}, errorTexts(err)); diff != "" {
					t.Errorf("texts of the error for a malformed reply (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func TestDoKeepsErrorIdentity(t *testing.T) {
	tests := map[string]struct {
		baseURL string
		// reply is what the upstream sends after the first byte of the request; empty is silence.
		reply   string
		dialErr error
		timeout time.Duration
		// abort runs once the request is outstanding.
		abort func(cancel context.CancelFunc, client *upstream.Client)
		check func(t *testing.T, err error, client *upstream.Client)
	}{
		"error: one hour of upstream silence keeps ErrTimeout": {
			abort: func(context.CancelFunc, *upstream.Client) { time.Sleep(time.Hour) },
			check: func(t *testing.T, err error, _ *upstream.Client) {
				if !errors.Is(err, upstream.ErrTimeout) {
					t.Errorf("Do after one hour of silence = %v, want ErrTimeout", err)
				}
			},
		},
		"error: a canceled request context keeps context.Canceled": {
			abort: func(cancel context.CancelFunc, _ *upstream.Client) { cancel() },
			check: func(t *testing.T, err error, _ *upstream.Client) {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Do with a canceled context = %v, want context.Canceled", err)
				}
			},
		},
		"error: an expired request deadline keeps context.DeadlineExceeded": {
			timeout: time.Minute,
			abort:   func(context.CancelFunc, *upstream.Client) { time.Sleep(time.Minute) },
			check: func(t *testing.T, err error, _ *upstream.Client) {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Do past its deadline = %v, want context.DeadlineExceeded", err)
				}
			},
		},
		"error: Client.Close during a request keeps the closed-client error": {
			abort: func(_ context.CancelFunc, client *upstream.Client) { _ = client.Close() },
			check: func(t *testing.T, err error, client *upstream.Client) {
				resp, closedErr := client.Do(t.Context(), messagesRequest())
				if resp != nil {
					_ = resp.Body.Close()
				}
				if closedErr == nil {
					t.Fatal("Do on a closed client returned no error")
				}
				if !errors.Is(err, closedErr) {
					t.Errorf("Do ended by Client.Close = %v, want the error of Do on a closed client (%v)", err, closedErr)
				}
			},
		},
		"error: a refused dial keeps the dial error": {
			dialErr: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
			check: func(t *testing.T, err error, _ *upstream.Client) {
				if _, ok := errors.AsType[*net.OpError](err); !ok || !errors.Is(err, syscall.ECONNREFUSED) {
					t.Errorf("Do with a refused dial = %v, want the *net.OpError wrapping ECONNREFUSED", err)
				}
			},
		},
		"error: a TLS handshake failure keeps the TLS error": {
			baseURL: "https://upstream.test",
			reply:   "HTTP/1.1 400 " + wireMarker + "\r\nContent-Length: 0\r\n\r\n",
			check: func(t *testing.T, err error, _ *upstream.Client) {
				if _, ok := errors.AsType[tls.RecordHeaderError](err); !ok {
					t.Errorf("Do against a plaintext reply to a TLS handshake = %v, want tls.RecordHeaderError", err)
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				up := &pipeUpstream{mode: faultNone}
				up.serve = func(peer net.Conn, _ *faultConn) { replyAfterFirstByte(peer, tt.reply) }
				dial := up.DialContext
				if tt.dialErr != nil {
					dial = func(context.Context, string, string) (net.Conn, error) { return nil, tt.dialErr }
				}
				client := upstream.NewClient(upstream.Options{DialContext: dial})
				defer func() {
					_ = client.Close()
					up.peers.Wait()
				}()

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.timeout > 0 {
					var cancelTimeout context.CancelFunc
					ctx, cancelTimeout = context.WithTimeout(ctx, tt.timeout)
					defer cancelTimeout()
				}

				req := messagesRequest()
				if tt.baseURL != "" {
					req.BaseURL = tt.baseURL
				}
				done := make(chan error, 1)
				var reqWG sync.WaitGroup
				defer reqWG.Wait()
				reqWG.Go(func() {
					resp, err := client.Do(ctx, req)
					if resp != nil {
						_ = resp.Body.Close()
						err = errors.New("unexpected response")
					}
					done <- err
				})

				synctest.Wait()
				if tt.abort != nil {
					select {
					case err := <-done:
						t.Fatalf("Do returned before the request could be aborted: %v", err)
					default:
					}
					tt.abort(cancel, client)
					synctest.Wait()
				}
				var err error
				select {
				case err = <-done:
				default:
					t.Fatal("Do still pending")
				}
				if err == nil {
					t.Fatal("Do returned no error")
				}
				tt.check(t, err, client)
				requireNoWireText(t, err)
			})
		})
	}
}

func TestGrokHeadersReachWireInSortedNameOrder(t *testing.T) {
	// Map iteration order is random, so enough repetitions make an order that depends on it visible.
	const repetitions = 64

	t.Run("success: header names that differ only in case reach the wire in byte order of the names", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			grokHeaders := http.Header{
				"X-Grok-Variant": {"canonical-1", "canonical-2"},
				"x-grok-variant": {"lower"},
				"X-GROK-VARIANT": {"upper"},
				"Content-Type":   {"application/json"},
			}
			var want []string
			for _, name := range slices.Sorted(maps.Keys(grokHeaders)) {
				if http.CanonicalHeaderKey(name) == "X-Grok-Variant" {
					want = append(want, grokHeaders[name]...)
				}
			}

			var mu sync.Mutex
			var seen [][]string
			up := &pipeUpstream{mode: faultNone}
			up.serve = func(peer net.Conn, _ *faultConn) {
				serveHTTP(peer, func(r *http.Request) string {
					mu.Lock()
					seen = append(seen, slices.Clone(r.Header["X-Grok-Variant"]))
					mu.Unlock()
					return okReply
				})
			}
			client := upstream.NewClient(upstream.Options{DialContext: up.DialContext})
			defer func() {
				_ = client.Close()
				up.peers.Wait()
			}()

			for i := range repetitions {
				body, err := doAndDrain(t.Context(), client, upstream.Request{
					Target:      upstream.TargetGrok,
					BaseURL:     fakeUpstreamBase + "/v1/chat/completions",
					GrokHeaders: grokHeaders,
					Body:        []byte(`{"model":"grok-4.3"}`),
				})
				if err != nil {
					t.Fatalf("request %d failed: %v", i, err)
				}
				if diff := gocmp.Diff(okReplyBody, body); diff != "" {
					t.Fatalf("request %d response body (-want +got):\n%s", i, diff)
				}
			}

			mu.Lock()
			got := slices.Clone(seen)
			mu.Unlock()
			if len(got) != repetitions {
				t.Fatalf("upstream received %d requests, want %d", len(got), repetitions)
			}
			var mismatches int
			for i, values := range got {
				if diff := gocmp.Diff(want, values); diff != "" {
					mismatches++
					if mismatches == 1 {
						t.Errorf("request %d X-Grok-Variant values on the wire (-want +got):\n%s", i, diff)
					}
				}
			}
			if mismatches > 0 {
				t.Errorf("%d of %d requests sent the X-Grok-Variant values in another order", mismatches, repetitions)
			}
		})
	})
}
