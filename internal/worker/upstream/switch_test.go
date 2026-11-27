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
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// switchReply is a protocol switch as net/http recognises one: status 101 with the upgrade headers.
// net/http hands such a response's connection to the caller as a read-write body and leaves the
// request's cancellation behind.
const switchReply = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

// TestSwitchedConnectionIsATransportFailure decides what the inference client does with an upstream
// that answers a request with a protocol switch and then keeps its socket open and silent. The source
// registers no 'upgrade' listener on its upstream requests, and Node then destroys the socket and
// fails the request, so the attempt must end as a transport failure, with no response handed out,
// and the client must close the switched connection itself.
func TestSwitchedConnectionIsATransportFailure(t *testing.T) {
	tests := map[string]struct {
		req upstream.Request
	}{
		"error: an Anthropic request answered by a protocol switch fails and its connection is closed": {
			req: messagesRequest(),
		},
		"error: a proxy request answered by a protocol switch fails and its connection is closed": {
			req: upstream.Request{
				Target:        upstream.TargetProxy,
				BaseURL:       fakeUpstreamBase,
				Method:        http.MethodPost,
				RequestURI:    "/v1/messages",
				ClientHeaders: http.Header{"Content-Type": {"application/json"}},
				Body:          []byte(`{"model":"gpt-6.1-sol"}`),
			},
		},
		"error: a Grok request answered by a protocol switch fails and its connection is closed": {
			req: upstream.Request{
				Target:      upstream.TargetGrok,
				BaseURL:     fakeUpstreamBase + "/v1/responses",
				GrokHeaders: http.Header{"Content-Type": {"application/json"}},
				Body:        []byte(`{"model":"grok-4.7"}`),
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// The fake answers the request with the switch, then reads on until the client closes
				// its end, and only then closes its own.
				fake := &pipeUpstream{serve: func(peer net.Conn, _ *faultConn) {
					serveHTTP(peer, func(*http.Request) string { return switchReply })
				}}
				client := upstream.NewClient(upstream.Options{DialContext: fake.DialContext})
				defer func() {
					_ = client.Close()
					fake.peers.Wait()
				}()

				resp, err := client.Do(t.Context(), tt.req)
				if resp != nil {
					// The switched connection was handed out; closing the body closes it, so the fake ends.
					t.Errorf("Do handed out the switched response with status %d (error %v); want a transport failure and no response", resp.StatusCode, err)
					_ = resp.Body.Close()
				}
				if err == nil {
					t.Error("Do reported no error for a request answered by a protocol switch")
				}
				if errors.Is(err, upstream.ErrTimeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Do reported %v; want a transport failure of the attempt, not a timeout or an ended context", err)
				}
				// Nothing else runs in the bubble once the fake has seen its connection closed.
				synctest.Wait()
				conns := fake.dialed()
				if len(conns) != 1 {
					t.Fatalf("dials = %d, want 1 (a request carrying a body is never replayed)", len(conns))
				}
				if !conns[0].isClosed() {
					t.Error("the client left the switched connection open")
				}
			})
		})
	}
}
