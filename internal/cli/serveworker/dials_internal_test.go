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

package serveworker

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// gatedDial is a connection establishment the test holds. It reports that it started, waits for its
// context to end, reports that together with the virtual time it waited, and returns only once the
// test releases it.
type gatedDial struct {
	started, cancelled, release chan struct{}
	waited                      time.Duration
}

func newGatedDial() *gatedDial {
	return &gatedDial{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedDial) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	start := time.Now()
	close(g.started)
	<-ctx.Done()
	g.waited = time.Since(start)
	close(g.cancelled)
	<-g.release
	return nil, ctx.Err()
}

// closed reports whether ch is closed, without waiting.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestDialGroupCloseJoinsPendingEstablishment decides that ending a group of worker-owned transports
// waits for a connection establishment that is still running: a dial that has seen its context end
// but has not returned keeps the group's close, and the worker's stop, from returning until the dial
// returns. Virtual time shows that the close, not the establishment bound, ended the dial.
//
// The dial is composed here the way (*dials).transport composes it: the upstream lifetime transport
// bound to the group's context, each dial function wrapped by the group's track. transport takes no
// upstream.Options, so the network dial of the transport it builds cannot be replaced by the gate.
func TestDialGroupCloseJoinsPendingEstablishment(t *testing.T) {
	tests := map[string]struct {
		tls bool
		// closer is "group" for the group's own close, "stop-service" for the worker's stop ending the
		// catalog and readiness group, "stop-exports" for the stop ending the exporters' group.
		closer string
		// request makes net/http start the dial for a request that gives up before the close.
		request bool
	}{
		"success: closing the group waits for a cancelled dial that has not returned": {
			closer: "group",
		},
		"success: closing the group waits for a cancelled dial of a TLS connection that has not returned": {
			tls: true, closer: "group",
		},
		"success: closing the group waits for a dial net/http carries on after its request gave up": {
			closer: "group", request: true,
		},
		"success: the worker's stop waits for a cancelled dial of the catalog and readiness transports": {
			closer: "stop-service",
		},
		"success: the worker's stop waits for a cancelled dial of a TLS connection of the exporters' transports": {
			tls: true, closer: "stop-exports",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// The exporters' group hangs off the test's context, not the service's, as in the worker,
				// so ending the service cannot end it.
				serviceCtx, cancel := context.WithCancel(t.Context())
				defer cancel()
				life := &lifetime{
					ctx:     serviceCtx,
					cancel:  cancel,
					auth:    upstream.NewTransport(upstream.Options{}),
					dials:   newDials(serviceCtx),
					exports: newDials(t.Context()),
					changed: make(chan struct{}, 1),
				}
				group, closeGroup := life.dials, life.dials.close
				switch tt.closer {
				case "stop-service":
					closeGroup = func() { life.stop(nil) }
				case "stop-exports":
					group, closeGroup = life.exports, func() { life.stop(nil) }
				}

				gate := newGatedDial()
				// A failure before the release must not leave the dial, and with it the bubble, blocked.
				defer group.close()
				releaseGate := sync.OnceFunc(func() { close(gate.release) })
				defer releaseGate()
				lifetimeTransport := upstream.NewLifetimeTransport(upstream.Options{DialContext: gate.dial}, group.ctx)
				dial := group.track(lifetimeTransport.DialContext)
				if tt.tls {
					dial = group.track(lifetimeTransport.DialTLSContext)
				}

				dialed := make(chan error, 1)
				if tt.request {
					tr := &http.Transport{DialContext: dial, DisableKeepAlives: true}
					defer tr.CloseIdleConnections()
					reqCtx, giveUp := context.WithCancel(t.Context())
					req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://collector.invalid/", http.NoBody)
					if err != nil {
						t.Fatal(err)
					}
					roundTrip := make(chan error, 1)
					go func() {
						resp, err := tr.RoundTrip(req)
						if err == nil {
							_ = resp.Body.Close()
						}
						roundTrip <- err
					}()
					<-gate.started
					giveUp()
					if err := <-roundTrip; !errors.Is(err, context.Canceled) {
						t.Fatalf("test precondition: the request that gave up ended with %v, want context.Canceled", err)
					}
					synctest.Wait()
					if closed(gate.cancelled) {
						t.Fatal("test precondition: net/http ended the dial with its request, so no dial outlives the request")
					}
				} else {
					go func() {
						_, err := dial(t.Context(), "tcp", "collector.invalid:443")
						dialed <- err
					}()
					<-gate.started
				}

				returned := make(chan struct{})
				go func() {
					closeGroup()
					close(returned)
				}()
				<-gate.cancelled
				if gate.waited != 0 {
					t.Errorf("the dial saw its context end %v after it started, want at once: something other than the close ended it", gate.waited)
				}
				// Every goroutine of the bubble is now blocked: the dial on its release, the close on
				// whatever it waits for.
				synctest.Wait()
				if closed(returned) {
					t.Fatal("the close returned while a dial it had cancelled was still running")
				}

				releaseGate()
				<-returned
				if !tt.request {
					if err := <-dialed; !errors.Is(err, context.Canceled) {
						t.Errorf("the released dial returned %v, want context.Canceled", err)
					}
				}
				if _, err := dial(t.Context(), "tcp", "collector.invalid:443"); !errors.Is(err, net.ErrClosed) {
					t.Errorf("a dial after the close returned %v, want net.ErrClosed", err)
				}
			})
		})
	}
}

// TestDialGroupTransportsTrackTheirDials decides that the transports a group builds dial through the
// group's tracking, which the close waits for: once the group is closed, both dial functions of such a
// transport are refused by the group itself, with net.ErrClosed, before the lifetime transport below
// would refuse them with its own closed-client error. Neither dial reaches the network.
func TestDialGroupTransportsTrackTheirDials(t *testing.T) {
	tests := map[string]struct{ tls bool }{
		"success: a closed group refuses the dial of its transport":                     {},
		"success: a closed group refuses the dial of a TLS connection of its transport": {tls: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			group := newDials(t.Context())
			rt, ok := group.transport().(refusingSwitch)
			if !ok {
				t.Fatalf("test precondition: the group's transport is %T, want refusingSwitch", group.transport())
			}
			group.close()
			dial := rt.DialContext
			if tt.tls {
				dial = rt.DialTLSContext
			}
			conn, err := dial(t.Context(), "tcp", "127.0.0.1:9")
			if conn != nil {
				_ = conn.Close()
			}
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("a dial of the closed group's transport returned %v, want net.ErrClosed from the group's tracking", err)
			}
		})
	}
}

// waitBound only ends a wait for an event that should already have happened; no assertion passes by
// waiting it out.
const waitBound = 5 * time.Second

// TestDialGroupTransportEndsWithTheGroupContext decides that the transport a group builds is bound to
// the group's context, on its own, with admission still open: once the context has ended, a dial of
// the transport is refused, and a dial that was already connecting when it ended is ended by it. The
// connect is held in httptrace's ConnectStart hook, inside the real network dial of the transport the
// group built, until the context has ended. The error decides what ended the dial: the transport's
// refusal of a closed lifetime, or the cancellation of the dial's context, and not the establishment
// bound, which would report a deadline. The live row shows that the same fixture and transport reach
// the server when nothing ends the context, so the refusal of the other rows is the context's doing.
func TestDialGroupTransportEndsWithTheGroupContext(t *testing.T) {
	tests := map[string]struct {
		tls bool
		// end is "none" for a live group, "before" to end the group's context before the request,
		// "connecting" to end it while the dial is held in its connect.
		end string
	}{
		"success: a live group's transport reaches the server":                             {end: "none"},
		"success: a live group's transport reaches the server over TLS":                    {tls: true, end: "none"},
		"error: a dial of the transport after the group's context ended is refused":        {end: "before"},
		"error: a TLS dial of the transport after the group's context ended is refused":    {tls: true, end: "before"},
		"error: a dial of the transport connecting when the group's context ends is ended": {end: "connecting"},
		"error: a TLS dial of the transport connecting when the group's context ends is ended": {
			tls: true, end: "connecting",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var served atomic.Int64
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served.Add(1)
				w.WriteHeader(http.StatusNoContent)
			})
			var srv *httptest.Server
			if tt.tls {
				srv = httptest.NewTLSServer(handler)
			} else {
				srv = httptest.NewServer(handler)
			}
			defer srv.Close()

			// The refusal of a transport whose lifetime has ended, taken from a transport of the same
			// constructor; it dials nothing.
			ended, end := context.WithCancel(t.Context())
			end()
			_, refused := upstream.NewLifetimeTransport(upstream.Options{}, ended).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
			if refused == nil {
				t.Fatal("test precondition: a transport whose lifetime ended dialled")
			}

			group := newDials(t.Context())
			defer group.close()
			rt, ok := group.transport().(refusingSwitch)
			if !ok {
				t.Fatalf("test precondition: the group's transport is %T, want refusingSwitch", group.transport())
			}
			if tt.tls {
				// The transport's TLS dial clones this configuration for every dial.
				pool := x509.NewCertPool()
				pool.AddCert(srv.Certificate())
				rt.TLSClientConfig.RootCAs = pool
			}

			connecting, proceed := make(chan struct{}), make(chan struct{})
			// Registered after the group's close, so it runs first: a dial held in its connect must
			// be let go before the close waits for it.
			proceedOnce := sync.OnceFunc(func() { close(proceed) })
			defer proceedOnce()
			ctx := t.Context()
			if tt.end == "connecting" {
				var connectOnce sync.Once
				ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{ConnectStart: func(string, string) {
					connectOnce.Do(func() { close(connecting) })
					<-proceed
				}})
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			if tt.end == "before" {
				group.cancel()
			}
			roundTrip := make(chan error, 1)
			go func() {
				resp, err := rt.RoundTrip(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				roundTrip <- err
			}()
			if tt.end == "connecting" {
				select {
				case <-connecting:
				case <-time.After(waitBound):
					t.Fatal("the transport's dial never reached its connect")
				}
				group.cancel()
				proceedOnce()
			}
			select {
			case err = <-roundTrip:
			case <-time.After(waitBound):
				t.Fatal("the request through the group's transport did not return")
			}

			switch tt.end {
			case "none":
				if err != nil {
					t.Fatalf("a request through a live group's transport: %v", err)
				}
			case "before":
				if !errors.Is(err, refused) {
					t.Errorf("a request through the transport after the group's context ended: %v; want the refusal of an ended lifetime, %v", err, refused)
				}
			case "connecting":
				if !errors.Is(err, refused) && !errors.Is(err, context.Canceled) {
					t.Errorf("a request through the transport whose context ended while it connected: %v; want the refusal of an ended lifetime or the dial's cancellation, not a deadline", err)
				}
			}
			want := int64(0)
			if tt.end == "none" {
				want = 1
			}
			if got := served.Load(); got != want {
				t.Errorf("requests that reached the server = %d, want %d", got, want)
			}
		})
	}
}
