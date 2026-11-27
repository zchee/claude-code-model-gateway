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
	"crypto/tls"
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

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// TestWorkerTransportEstablishmentCutoff decides that a connection of a worker-owned transport is
// given up once its establishment, the connect and for HTTPS also the TLS handshake, has run for five
// seconds, while the group that built the transport stays live, and that one established in time is
// returned.
//
// The constant itself is decided at the group's dial tracking: a dial it wraps finds, at its entry on
// the bubble's clock, exactly five seconds left before its context's deadline. The other cases use the
// real dial of the transport the group built, held on the bubble's clock in httptrace's ConnectStart
// hook, which the network dial calls just before it connects, and in the TLS configuration's
// VerifyConnection hook, which the handshake calls after the TCP connection was made and the server's
// certificate was verified. The server listens outside the bubble, so the connect and the handshake's
// own reads and writes run on real time and do not move the bubble's clock; no connect is held near
// the deadline, so none has to finish within a few real milliseconds. The upstream policy below the
// group bounds establishment only by its one-hour socket inactivity, so without the group's cutoff
// every late dial here would connect too.
func TestWorkerTransportEstablishmentCutoff(t *testing.T) {
	tests := map[string]struct {
		tls bool
		// connectHold and handshakeHold are how long the dial is held before it connects and inside
		// its TLS handshake.
		connectHold, handshakeHold time.Duration
		late                       bool
		// tracked observes the deadline at the entry of a dial the group's tracking wraps instead.
		tracked bool
	}{
		"success: a dial the group tracks has exactly five seconds to establish its connection": {
			tracked: true,
		},
		"success: a connection established at once is returned": {},
		"success: a TLS connection whose connect and handshake end just before five seconds is returned": {
			tls: true, connectHold: 2 * time.Second, handshakeHold: 2999 * time.Millisecond,
		},
		"error: a connection still connecting at five seconds is given up": {
			connectHold: 5001 * time.Millisecond, late: true,
		},
		"error: a TLS connection still connecting at five seconds is given up": {
			tls: true, connectHold: 5001 * time.Millisecond, late: true,
		},
		"error: a TLS connection still in its handshake at five seconds is given up": {
			tls: true, connectHold: 2 * time.Second, handshakeHold: 3001 * time.Millisecond, late: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			var srv *httptest.Server
			if tt.tls {
				srv = httptest.NewTLSServer(handler)
			} else {
				srv = httptest.NewServer(handler)
			}
			defer srv.Close()
			addr := srv.Listener.Addr().String()

			synctest.Test(t, func(t *testing.T) {
				group := newDials(t.Context())
				defer group.close()
				if tt.tracked {
					errObserved := errors.New("deadline observed")
					var left []time.Duration
					dial := group.track(func(ctx context.Context, _, _ string) (net.Conn, error) {
						deadline, ok := ctx.Deadline()
						if !ok {
							return nil, errors.New("the tracked dial has no deadline")
						}
						left = append(left, time.Until(deadline))
						return nil, errObserved
					})
					if _, err := dial(t.Context(), "tcp", addr); !errors.Is(err, errObserved) {
						t.Fatalf("the tracked dial returned %v, want the observation's own error", err)
					}
					if diff := gocmp.Diff([]time.Duration{5 * time.Second}, left); diff != "" {
						t.Errorf("time left to establish at the tracked dial's entry (-want +got):\n%s", diff)
					}
					return
				}
				rt, ok := group.transport().(refusingSwitch)
				if !ok {
					t.Fatalf("test precondition: the group's transport is %T, want refusingSwitch", group.transport())
				}
				var connects, handshakes atomic.Int32
				dial := rt.DialContext
				if tt.tls {
					// The transport's TLS dial clones this configuration for every dial.
					pool := x509.NewCertPool()
					pool.AddCert(srv.Certificate())
					rt.TLSClientConfig.RootCAs = pool
					rt.TLSClientConfig.VerifyConnection = func(tls.ConnectionState) error {
						handshakes.Add(1)
						time.Sleep(tt.handshakeHold)
						return nil
					}
					dial = rt.DialTLSContext
				}
				ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{ConnectStart: func(string, string) {
					connects.Add(1)
					time.Sleep(tt.connectHold)
				}})

				conn, err := dial(ctx, "tcp", addr)
				if conn != nil {
					_ = conn.Close()
				}
				wantHandshakes := int32(0)
				if tt.tls && tt.connectHold < 5*time.Second {
					wantHandshakes = 1
				}
				if diff := gocmp.Diff([]int32{1, wantHandshakes}, []int32{connects.Load(), handshakes.Load()}); diff != "" {
					t.Fatalf("test precondition: connect and handshake hook calls (-want +got):\n%s", diff)
				}
				if group.ctx.Err() != nil {
					t.Fatal("test precondition: the group's context ended, so the group, not the cutoff, could have ended the dial")
				}
				if diff := gocmp.Diff(!tt.late, conn != nil); diff != "" {
					t.Errorf("connection returned (-want +got):\n%s\nerror: %v", diff, err)
				}
				if !tt.late {
					if err != nil {
						t.Errorf("a dial held %v and %v returned %v, want no error", tt.connectHold, tt.handshakeHold, err)
					}
					return
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("a dial held %v and %v returned %v, want the establishment deadline", tt.connectHold, tt.handshakeHold, err)
				}
				if errors.Is(err, upstream.ErrTimeout) {
					t.Errorf("a dial held %v and %v returned the upstream inactivity timeout %v, want the establishment deadline", tt.connectHold, tt.handshakeHold, err)
				}
			})
		})
	}
}

// TestWorkerTransportNoPoolNoHostCap decides that a worker-owned transport sets no cap on the
// connections it opens to one host and keeps none of them for a later request. The transport the group
// builds is configured with no per-host cap and no keep-alive. On the wire, eighty plain requests to one
// host are held by the server until all eighty have arrived, which the upstream policy's own cap of 64
// would prevent, two TLS requests overlap the same way, and two requests made one after another
// afterwards each open a new connection.
func TestWorkerTransportNoPoolNoHostCap(t *testing.T) {
	tests := map[string]struct {
		tls bool
		// concurrent is the number of requests the server holds until all have arrived.
		concurrent int
	}{
		"success: plain connections are neither capped per host nor pooled": {concurrent: 80},
		"success: TLS connections overlap on one host and are not pooled":   {tls: true, concurrent: 2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var (
				arrivedMu sync.Mutex
				arrived   int
				holding   = true
				newConns  atomic.Int64
			)
			all := make(chan struct{})
			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrivedMu.Lock()
				hold := holding
				if hold {
					arrived++
					if arrived == tt.concurrent {
						close(all)
					}
				}
				arrivedMu.Unlock()
				if hold {
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					newConns.Add(1)
				}
			}
			if tt.tls {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			defer releaseOnce()

			group := newDials(t.Context())
			defer group.close()
			rt, ok := group.transport().(refusingSwitch)
			if !ok {
				t.Fatalf("test precondition: the group's transport is %T, want refusingSwitch", group.transport())
			}
			if diff := gocmp.Diff([]any{0, true}, []any{rt.MaxConnsPerHost, rt.DisableKeepAlives}); diff != "" {
				t.Errorf("transport MaxConnsPerHost and DisableKeepAlives (-want +got):\n%s", diff)
			}
			if tt.tls {
				pool := x509.NewCertPool()
				pool.AddCert(srv.Certificate())
				rt.TLSClientConfig.RootCAs = pool
			}
			client := &http.Client{Transport: rt}
			get := func(ctx context.Context) (bool, error) {
				reused := false
				ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", http.NoBody)
				if err != nil {
					return false, err
				}
				resp, err := client.Do(req)
				if err != nil {
					return false, err
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusNoContent {
					return reused, errors.New("unexpected status " + resp.Status)
				}
				return reused, nil
			}

			// The batch has a context of its own, so a failure below can end requests still queued.
			batch, cancelBatch := context.WithCancel(t.Context())
			defer cancelBatch()
			errs := make(chan error, tt.concurrent)
			var wg sync.WaitGroup
			for range tt.concurrent {
				wg.Go(func() {
					_, err := get(batch)
					errs <- err
				})
			}
			select {
			case <-all:
			case <-time.After(waitBound):
				arrivedMu.Lock()
				got := arrived
				arrivedMu.Unlock()
				cancelBatch()
				releaseOnce()
				wg.Wait()
				t.Fatalf("only %d of %d concurrent requests to one host reached it", got, tt.concurrent)
			}
			arrivedMu.Lock()
			holding = false
			arrivedMu.Unlock()
			releaseOnce()
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Errorf("concurrent request: %v", err)
				}
			}
			if diff := gocmp.Diff(int64(tt.concurrent), newConns.Load()); diff != "" {
				t.Errorf("connections opened for the concurrent requests (-want +got):\n%s", diff)
			}

			var reusedAll []bool
			for i := range 2 {
				reused, err := get(t.Context())
				if err != nil {
					t.Fatalf("sequential request %d: %v", i+1, err)
				}
				reusedAll = append(reusedAll, reused)
			}
			if diff := gocmp.Diff([]bool{false, false}, reusedAll); diff != "" {
				t.Errorf("sequential requests that reused a pooled connection (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(int64(tt.concurrent+2), newConns.Load()); diff != "" {
				t.Errorf("connections opened after two sequential requests (-want +got):\n%s", diff)
			}
		})
	}
}
