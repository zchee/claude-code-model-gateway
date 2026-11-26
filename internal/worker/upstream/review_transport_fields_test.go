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
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// TestNewTransportCapturesHTTPSDialSettings sends one HTTPS request through a transport from
// NewTransport after changing it as each case says. Every handshake offers exactly http/1.1 in ALPN,
// whatever the caller's configuration lists and also for an upgrade request. The HTTPS dial uses the
// dial function and TLS configuration captured from the construction options: replacing the
// returned transport's DialContext, TLSHandshakeTimeout or TLSClientConfig does not change it, while
// mutating the configuration object TLSClientConfig points to before first use does, because that
// object is shared with the dialer and cloned for every dial.
func TestNewTransportCapturesHTTPSDialSettings(t *testing.T) {
	type setup struct {
		trusted   *x509.CertPool
		untrusted *x509.CertPool
	}
	tests := map[string]struct {
		// options builds the construction options from the server's trust anchors.
		options func(s setup) *tls.Config
		// change alters the returned transport before its first request.
		change  func(t *testing.T, tr *http.Transport, s setup, replacedDials *atomic.Int32)
		upgrade bool
		wantErr bool
	}{
		"error: default options offer only http/1.1 and verify the server": {
			options: func(setup) *tls.Config { return nil },
			wantErr: true,
		},
		"success: a caller configuration listing h2 offers only http/1.1": {
			options: func(s setup) *tls.Config {
				return &tls.Config{RootCAs: s.trusted, NextProtos: []string{"h2", "http/1.1"}}
			},
		},
		"success: an upgrade request offers only http/1.1": {
			options: func(s setup) *tls.Config {
				return &tls.Config{RootCAs: s.trusted, NextProtos: []string{"h2", "http/1.1"}}
			},
			upgrade: true,
		},
		"success: a replaced TLSClientConfig does not configure the HTTPS dial": {
			options: func(s setup) *tls.Config { return &tls.Config{RootCAs: s.trusted} },
			change: func(_ *testing.T, tr *http.Transport, s setup, _ *atomic.Int32) {
				tr.TLSClientConfig = &tls.Config{RootCAs: s.untrusted, NextProtos: []string{"h2"}}
			},
		},
		"success: a replaced TLSHandshakeTimeout does not bound the HTTPS dial": {
			options: func(s setup) *tls.Config { return &tls.Config{RootCAs: s.trusted} },
			change: func(_ *testing.T, tr *http.Transport, _ setup, _ *atomic.Int32) {
				tr.TLSHandshakeTimeout = time.Nanosecond
			},
		},
		"success: a replaced DialContext is not used for the HTTPS dial": {
			options: func(s setup) *tls.Config { return &tls.Config{RootCAs: s.trusted} },
			change: func(_ *testing.T, tr *http.Transport, _ setup, replacedDials *atomic.Int32) {
				tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
					replacedDials.Add(1)
					return nil, errors.New("replaced dial function must not be used for HTTPS")
				}
			},
		},
		"success: trusting the server in the shared configuration before first use takes effect": {
			options: func(setup) *tls.Config { return &tls.Config{} },
			change: func(t *testing.T, tr *http.Transport, s setup, _ *atomic.Int32) {
				if tr.TLSClientConfig == nil {
					t.Fatal("test precondition: NewTransport returned no TLSClientConfig")
				}
				tr.TLSClientConfig.RootCAs = s.trusted
			},
		},
		"error: distrusting the server in the shared configuration before first use takes effect": {
			options: func(s setup) *tls.Config { return &tls.Config{RootCAs: s.trusted} },
			change: func(t *testing.T, tr *http.Transport, s setup, _ *atomic.Int32) {
				if tr.TLSClientConfig == nil {
					t.Fatal("test precondition: NewTransport returned no TLSClientConfig")
				}
				tr.TLSClientConfig.RootCAs = s.untrusted
			},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var offeredMu sync.Mutex
			var offered [][]string
			var served atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				served.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"ok":true}`)
			}))
			srv.EnableHTTP2 = true
			srv.TLS = &tls.Config{
				GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
					offeredMu.Lock()
					offered = append(offered, slices.Clone(hello.SupportedProtos))
					offeredMu.Unlock()
					return nil, nil
				},
			}
			srv.StartTLS()
			t.Cleanup(srv.Close)
			s := setup{
				trusted:   srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs,
				untrusted: x509.NewCertPool(),
			}
			if s.trusted == nil {
				t.Fatal("test precondition: the test server's client has no trust anchors")
			}

			var optionDials, replacedDials atomic.Int32
			var dialer net.Dialer
			tr := upstream.NewTransport(upstream.Options{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					optionDials.Add(1)
					return dialer.DialContext(ctx, network, addr)
				},
				TLSConfig: tt.options(s),
			})
			t.Cleanup(tr.CloseIdleConnections)
			if tt.change != nil {
				tt.change(t, tr, s, &replacedDials)
			}

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/models", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			if tt.upgrade {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			resp, err := tr.RoundTrip(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			switch {
			case tt.wantErr && err == nil:
				t.Errorf("RoundTrip succeeded with status %d, want a failed TLS handshake", resp.StatusCode)
			case !tt.wantErr && err != nil:
				t.Errorf("RoundTrip: %v", err)
			case !tt.wantErr && resp.ProtoMajor != 1:
				t.Errorf("response protocol HTTP/%d.%d, want HTTP/1.1", resp.ProtoMajor, resp.ProtoMinor)
			}
			wantServed := int32(1)
			if tt.wantErr {
				wantServed = 0
			}
			if n := served.Load(); n != wantServed {
				t.Errorf("requests served = %d, want %d", n, wantServed)
			}
			if n := optionDials.Load(); n != 1 {
				t.Errorf("dials through the construction option = %d, want 1", n)
			}
			if n := replacedDials.Load(); n != 0 {
				t.Errorf("dials through the replaced DialContext = %d, want 0", n)
			}
			offeredMu.Lock()
			gotOffered := slices.Clone(offered)
			offeredMu.Unlock()
			if diff := gocmp.Diff([][]string{{"http/1.1"}}, gotOffered); diff != "" {
				t.Errorf("ALPN protocols offered per handshake mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
