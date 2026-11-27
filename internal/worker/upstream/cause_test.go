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
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// closingConn runs onClose before it closes its connection.
type closingConn struct {
	net.Conn
	onClose func()
}

func (c *closingConn) Close() error {
	c.onClose()
	return c.Conn.Close()
}

func TestDoKeepsCallerCause(t *testing.T) {
	// errCallerCause wraps neither context.Canceled nor context.DeadlineExceeded.
	errCallerCause := errors.New("caller gave up on the upstream")

	tests := map[string]struct {
		// withCause derives the request context from parent; end makes it end with cause once the
		// request is outstanding.
		withCause func(parent context.Context, cause error) (ctx context.Context, end, release func())
		// closeWhileEnding makes the transport's closing of the cancelled connection call
		// Client.Close, so the client closes after the caller ended the request and before Do
		// returns.
		closeWhileEnding bool
	}{
		"error: a caller cancelling with its own cause gets that cause": {
			withCause: func(parent context.Context, cause error) (context.Context, func(), func()) {
				ctx, cancel := context.WithCancelCause(parent)
				return ctx, func() { cancel(cause) }, func() { cancel(nil) }
			},
		},
		"error: a caller deadline with its own cause gets that cause": {
			withCause: func(parent context.Context, cause error) (context.Context, func(), func()) {
				ctx, cancel := context.WithDeadlineCause(parent, time.Now().Add(time.Minute), cause)
				return ctx, func() { time.Sleep(time.Minute) }, cancel
			},
		},
		"error: a caller cancelling with its own cause while Client.Close runs gets that cause": {
			withCause: func(parent context.Context, cause error) (context.Context, func(), func()) {
				ctx, cancel := context.WithCancelCause(parent)
				return ctx, func() { cancel(cause) }, func() { cancel(nil) }
			},
			closeWhileEnding: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				up := &pipeUpstream{mode: faultNone}
				up.serve = func(peer net.Conn, _ *faultConn) { replyAfterFirstByte(peer, "") }
				var client *upstream.Client
				dial := up.DialContext
				// hookStarted and hookFinished record the Client.Close call made from the connection's
				// Close, before it starts and after it returns.
				var hookStarted, hookFinished atomic.Bool
				if tt.closeWhileEnding {
					var closeOnce sync.Once
					dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
						c, err := up.DialContext(ctx, network, addr)
						if err != nil {
							return nil, err
						}
						return &closingConn{Conn: c, onClose: func() {
							closeOnce.Do(func() {
								hookStarted.Store(true)
								_ = client.Close()
								hookFinished.Store(true)
							})
						}}, nil
					}
				}
				client = upstream.NewClient(upstream.Options{DialContext: dial})

				ctx, end, release := tt.withCause(t.Context(), errCallerCause)
				defer release()

				// doResult is what the request goroutine saw at the moment Do returned.
				type doResult struct {
					err                       error
					hookStarted, hookFinished bool
				}
				done := make(chan doResult, 1)
				var reqWG sync.WaitGroup
				defer func() {
					_ = client.Close()
					reqWG.Wait()
					up.peers.Wait()
				}()
				reqWG.Go(func() {
					resp, err := client.Do(ctx, messagesRequest())
					// Read before anything else runs on this goroutine, so a hook still running
					// elsewhere when Do returned shows as unfinished.
					res := doResult{err: err, hookStarted: hookStarted.Load(), hookFinished: hookFinished.Load()}
					if resp != nil {
						_ = resp.Body.Close()
						res.err = errors.New("unexpected response")
					}
					done <- res
				})

				synctest.Wait()
				select {
				case res := <-done:
					t.Fatalf("Do returned before the caller ended the request: %v", res.err)
				default:
				}
				end()
				synctest.Wait()

				var res doResult
				select {
				case res = <-done:
				default:
					t.Fatal("Do still pending after the caller ended the request")
				}
				if tt.closeWhileEnding {
					switch {
					case !res.hookStarted:
						t.Error("Client.Close was not called from the connection's Close before Do returned; the case did not race Close with the caller's cancellation")
					case !res.hookFinished:
						t.Error("Client.Close called from the connection's Close had not returned when Do returned")
					}
				}
				err := res.err
				if err == nil {
					t.Fatal("Do returned no error")
				}
				t.Logf("Do error: %v", err)
				if !errors.Is(err, errCallerCause) {
					t.Errorf("Do error = %v, want the caller's cause %v", err, errCallerCause)
				}
				if got, want := err.Error(), errCallerCause.Error(); got != want {
					t.Errorf("Do error text = %q, want the caller's cause %q", got, want)
				}
			})
		})
	}
}

// uncomparableCause is an error whose field holds a value of any type. Two of them compare without
// a panic only while the field holds comparable values; holding a slice, == panics.
type uncomparableCause struct {
	detail any
}

func (e uncomparableCause) Error() string {
	return "caller gave up with details"
}

func TestDoIgnoresUncomparableCallerCause(t *testing.T) {
	// The cause holds a slice, so no two such values can be compared.
	cause := uncomparableCause{detail: []string{"first reason", "second reason"}}

	tests := map[string]struct {
		// withCause derives the request context from parent; end makes it end with cause once the
		// request holds a connection.
		withCause func(parent context.Context, cause error) (ctx context.Context, end, release func())
	}{
		"error: a caller cancelling with a cause that cannot be compared gets the fixed response error": {
			withCause: func(parent context.Context, cause error) (context.Context, func(), func()) {
				ctx, cancel := context.WithCancelCause(parent)
				return ctx, func() { cancel(cause) }, func() { cancel(nil) }
			},
		},
		"error: a caller deadline with a cause that cannot be compared gets the fixed response error": {
			withCause: func(parent context.Context, cause error) (context.Context, func(), func()) {
				ctx, cancel := context.WithDeadlineCause(parent, time.Now().Add(time.Minute), cause)
				return ctx, func() { time.Sleep(time.Minute) }, cancel
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				up := &pipeUpstream{mode: faultNone}
				up.serve = func(peer net.Conn, _ *faultConn) { replyAfterFirstByte(peer, "") }
				client := upstream.NewClient(upstream.Options{DialContext: up.DialContext})

				ctx, end, release := tt.withCause(t.Context(), cause)
				defer release()

				// doResult is what Do returned, or the panic it raised.
				type doResult struct {
					err      error
					panicked any
				}
				done := make(chan doResult, 1)
				var reqWG sync.WaitGroup
				defer func() {
					_ = client.Close()
					reqWG.Wait()
					up.peers.Wait()
				}()
				reqWG.Go(func() {
					var res doResult
					defer func() {
						if r := recover(); r != nil {
							res.panicked = r
						}
						done <- res
					}()
					resp, err := client.Do(ctx, messagesRequest())
					res.err = err
					if resp != nil {
						_ = resp.Body.Close()
						res.err = errors.New("unexpected response")
					}
				})

				// The request has written its bytes on a connection and waits for a reply.
				synctest.Wait()
				select {
				case res := <-done:
					t.Fatalf("Do returned before the caller ended the request: err=%v panic=%v", res.err, res.panicked)
				default:
				}
				end()
				synctest.Wait()

				var res doResult
				select {
				case res = <-done:
				default:
					t.Fatal("Do still pending after the caller ended the request")
				}
				if res.panicked != nil {
					t.Fatalf("Do panicked on a cause that cannot be compared: %v", res.panicked)
				}
				if res.err == nil {
					t.Fatal("Do returned no error")
				}
				// The cause is never matched, so the failure is classified like any other after
				// the connection was obtained.
				if diff := gocmp.Diff([]string{responseFailureText}, errorTexts(res.err)); diff != "" {
					t.Errorf("texts of the error of a request ended with a cause that cannot be compared (-want +got):\n%s", diff)
				}
			})
		})
	}
}
