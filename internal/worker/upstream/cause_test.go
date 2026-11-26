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
	"testing"
	"testing/synctest"
	"time"

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
				if tt.closeWhileEnding {
					var closeOnce sync.Once
					dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
						c, err := up.DialContext(ctx, network, addr)
						if err != nil {
							return nil, err
						}
						return &closingConn{Conn: c, onClose: func() { closeOnce.Do(func() { _ = client.Close() }) }}, nil
					}
				}
				client = upstream.NewClient(upstream.Options{DialContext: dial})

				ctx, end, release := tt.withCause(t.Context(), errCallerCause)
				defer release()

				done := make(chan error, 1)
				var reqWG sync.WaitGroup
				defer func() {
					_ = client.Close()
					reqWG.Wait()
					up.peers.Wait()
				}()
				reqWG.Go(func() {
					resp, err := client.Do(ctx, messagesRequest())
					if resp != nil {
						_ = resp.Body.Close()
						err = errors.New("unexpected response")
					}
					done <- err
				})

				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("Do returned before the caller ended the request: %v", err)
				default:
				}
				end()
				synctest.Wait()

				var err error
				select {
				case err = <-done:
				default:
					t.Fatal("Do still pending after the caller ended the request")
				}
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
