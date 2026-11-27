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

// TestWorkerTransportEstablishmentCutoff decides that a connection of a worker-owned transport that
// is still being established five seconds after its dial began is given up, while the group that
// built the transport stays live, and that one established just before that moment is used.
//
// The dial is the real network dial of the transport the group built. httptrace's ConnectStart hook,
// which that dial calls just before it connects, holds it on the bubble's clock; the server listens
// outside the bubble, so the connect and the TLS handshake that follow it run on real time and do
// not move the bubble's clock. The upstream policy below the group bounds a connect only by its
// one-hour socket inactivity, so without the group's cutoff the late dial would connect too.
func TestWorkerTransportEstablishmentCutoff(t *testing.T) {
	tests := map[string]struct {
		tls bool
		// hold is how long the dial is held before it connects.
		hold time.Duration
		late bool
	}{
		"success: a connection established before five seconds is used": {
			hold: 4900 * time.Millisecond,
		},
		"success: a TLS connection established before five seconds is used": {
			tls: true, hold: 4900 * time.Millisecond,
		},
		"error: a connection still connecting at five seconds is given up": {
			hold: 5001 * time.Millisecond, late: true,
		},
		"error: a TLS connection still connecting at five seconds is given up": {
			tls: true, hold: 5001 * time.Millisecond, late: true,
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
				rt, ok := group.transport().(refusingSwitch)
				if !ok {
					t.Fatalf("test precondition: the group's transport is %T, want refusingSwitch", group.transport())
				}
				dial := rt.DialContext
				if tt.tls {
					// The transport's TLS dial clones this configuration for every dial.
					pool := x509.NewCertPool()
					pool.AddCert(srv.Certificate())
					rt.TLSClientConfig.RootCAs = pool
					dial = rt.DialTLSContext
				}

				var connects atomic.Int32
				ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{ConnectStart: func(string, string) {
					connects.Add(1)
					time.Sleep(tt.hold)
				}})
				conn, err := dial(ctx, "tcp", addr)
				if conn != nil {
					_ = conn.Close()
				}
				if got := connects.Load(); got != 1 {
					t.Fatalf("test precondition: the dial reached its connect %d times, want 1", got)
				}
				if group.ctx.Err() != nil {
					t.Fatal("test precondition: the group's context ended, so the group, not the cutoff, could have ended the dial")
				}
				if !tt.late {
					if err != nil {
						t.Fatalf("a dial held %v returned %v, want a connection", tt.hold, err)
					}
					return
				}
				if conn != nil {
					t.Fatalf("a dial held %v returned a connection, want the establishment cutoff", tt.hold)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("a dial held %v returned %v, want the establishment deadline", tt.hold, err)
				}
				if errors.Is(err, upstream.ErrTimeout) {
					t.Errorf("a dial held %v returned the upstream inactivity timeout %v, want the establishment deadline", tt.hold, err)
				}
			})
		})
	}
}

// TestWorkerTransportNoPoolNoHostCap decides that a worker-owned transport puts no cap on the
// connections it opens to one host and keeps none of them for a later request. Eighty requests to
// one host are held by the server until all eighty have arrived, which a cap of 64 connections per
// host, the upstream policy's own, would prevent; two requests made one after another afterwards
// each open a new connection.
func TestWorkerTransportNoPoolNoHostCap(t *testing.T) {
	const concurrent = 80
	var (
		arrivedMu sync.Mutex
		arrived   int
		newConns  atomic.Int64
	)
	all := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	holding := true
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivedMu.Lock()
		hold := holding
		if hold {
			arrived++
			if arrived == concurrent {
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
	srv.Start()
	defer srv.Close()
	defer releaseOnce()

	group := newDials(t.Context())
	defer group.close()
	client := &http.Client{Transport: group.transport()}
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

	errs := make(chan error, concurrent)
	var wg sync.WaitGroup
	for range concurrent {
		wg.Go(func() {
			_, err := get(t.Context())
			errs <- err
		})
	}
	select {
	case <-all:
	case <-time.After(waitBound):
		arrivedMu.Lock()
		got := arrived
		arrivedMu.Unlock()
		releaseOnce()
		wg.Wait()
		t.Fatalf("only %d of %d concurrent requests to one host reached it", got, concurrent)
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
	if got := newConns.Load(); got != concurrent {
		t.Errorf("connections opened for %d concurrent requests = %d, want %d", concurrent, got, concurrent)
	}

	for i := range 2 {
		reused, err := get(t.Context())
		if err != nil {
			t.Fatalf("sequential request %d: %v", i+1, err)
		}
		if reused {
			t.Errorf("sequential request %d reused a pooled connection", i+1)
		}
	}
	if got := newConns.Load(); got != concurrent+2 {
		t.Errorf("connections opened after two sequential requests = %d, want %d", got, concurrent+2)
	}
}
