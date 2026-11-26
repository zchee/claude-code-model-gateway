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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

type pipeAddr string

func (a pipeAddr) Network() string { return "tcp" }
func (a pipeAddr) String() string  { return string(a) }

type pipeListener struct {
	addr     net.Addr
	conns    chan net.Conn
	closed   chan struct{}
	once     sync.Once
	accepted atomic.Int32
}

func newPipeListener(addr string) *pipeListener {
	return &pipeListener{
		addr:   pipeAddr(addr),
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		l.accepted.Add(1)
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return l.addr }

func (l *pipeListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	c1, c2 := net.Pipe()
	select {
	case l.conns <- c2:
		return c1, nil
	case <-l.closed:
		_ = c1.Close()
		_ = c2.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_ = c1.Close()
		_ = c2.Close()
		return nil, ctx.Err()
	}
}

func TestTransportNoRedirect(t *testing.T) {
	tests := map[string]struct {
		status int
	}{
		"success: 301 moved permanently is not followed":  {status: http.StatusMovedPermanently},
		"success: 302 found is not followed":              {status: http.StatusFound},
		"success: 307 temporary redirect is not followed": {status: http.StatusTemporaryRedirect},
		"success: 308 permanent redirect is not followed": {status: http.StatusPermanentRedirect},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var targetHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect-target" {
					targetHits.Add(1)
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(srv.Close)

			client := upstream.NewClient(upstream.Options{})
			t.Cleanup(func() { _ = client.Close() })

			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    srv.URL,
				Method:     http.MethodGet,
				RequestURI: "/start",
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading body: %v", err)
			}
			if diff := gocmp.Diff(tt.status, resp.StatusCode); diff != "" {
				t.Fatalf("status (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("/redirect-target", resp.Header.Get("Location")); diff != "" {
				t.Fatalf("Location (-want +got):\n%s", diff)
			}
			if hits := targetHits.Load(); hits != 0 {
				t.Fatalf("redirect target received %d hits, want 0", hits)
			}
		})
	}
}

func TestTransportNoAutoDecompression(t *testing.T) {
	var rawCompressed bytes.Buffer
	gz := gzip.NewWriter(&rawCompressed)
	if _, err := gz.Write([]byte(`{"type":"message","content":[{"type":"text","text":"compressed"}]}`)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	wantBytes := rawCompressed.Bytes()

	tests := map[string]struct {
		clientAcceptEncoding string
	}{
		"success: preserves gzip response bytes when client sends no Accept-Encoding": {},
		"success: preserves gzip response bytes when client sends Accept-Encoding":    {clientAcceptEncoding: "gzip"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var seenAcceptEncoding []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seenAcceptEncoding = r.Header.Values("Accept-Encoding")
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", strconv.Itoa(len(wantBytes)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(wantBytes)
			}))
			t.Cleanup(srv.Close)

			headers := http.Header{"Content-Type": {"application/json"}}
			if tt.clientAcceptEncoding != "" {
				headers.Set("Accept-Encoding", tt.clientAcceptEncoding)
			}
			client := upstream.NewClient(upstream.Options{})
			t.Cleanup(func() { _ = client.Close() })

			resp, err := client.Do(t.Context(), upstream.Request{
				Target:        upstream.TargetAnthropic,
				BaseURL:       srv.URL,
				Method:        http.MethodPost,
				RequestURI:    "/v1/messages",
				ClientHeaders: headers,
				Body:          []byte(`{"model":"claude-sonnet-4-6"}`),
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			gotBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			if tt.clientAcceptEncoding == "" && len(seenAcceptEncoding) != 0 {
				t.Errorf("transport synthesized Accept-Encoding: %v", seenAcceptEncoding)
			}
			if diff := gocmp.Diff("gzip", resp.Header.Get("Content-Encoding")); diff != "" {
				t.Errorf("Header Content-Encoding (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff([]string{"gzip"}, headerValuesCI(t, resp.RelayHeader, "Content-Encoding")); diff != "" {
				t.Errorf("RelayHeader Content-Encoding (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(wantBytes, gotBytes); diff != "" {
				t.Fatalf("response body bytes were altered (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTransportHTTP11Only(t *testing.T) {
	var protoMajor, protoMinor int
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protoMajor, protoMinor = r.ProtoMajor, r.ProtoMinor
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	client := upstream.NewClient(upstream.Options{TLSConfig: tlsCfg})
	t.Cleanup(func() { _ = client.Close() })

	resp, err := client.Do(t.Context(), upstream.Request{
		Target:     upstream.TargetAnthropic,
		BaseURL:    srv.URL,
		Method:     http.MethodPost,
		RequestURI: "/v1/messages",
		Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
	})
	if err != nil {
		t.Fatalf("Do over TLS returned error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if protoMajor != 1 || protoMinor != 1 {
		t.Fatalf("upstream received HTTP/%d.%d, want HTTP/1.1", protoMajor, protoMinor)
	}
}

func TestTransportIgnoresEnvProxy(t *testing.T) {
	var decoyProxyHits atomic.Int32
	decoyProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		decoyProxyHits.Add(1)
		http.Error(w, "environment proxy must never be dialed", http.StatusBadGateway)
	}))
	t.Cleanup(decoyProxy.Close)

	var httpHits, httpsHits atomic.Int32
	var httpHost, httpsHost string
	var mu sync.Mutex

	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpHits.Add(1)
		mu.Lock()
		httpHost = r.Host
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"direct":"http"}`)
	}))
	t.Cleanup(httpTarget.Close)

	httpsTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpsHits.Add(1)
		mu.Lock()
		httpsHost = r.Host
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"direct":"https"}`)
	}))
	t.Cleanup(httpsTarget.Close)

	tlsCfg := httpsTarget.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsCfg.ServerName = "127.0.0.1"

	tests := map[string]struct {
		baseURL   string
		wantHost  string
		tlsConfig *tls.Config
		hits      *atomic.Int32
		gotHost   func() string
	}{
		"success: HTTP request to non-loopback host ignores environment proxy": {
			baseURL:  "http://upstream.invalid:8080",
			wantHost: "upstream.invalid:8080",
			hits:     &httpHits,
			gotHost: func() string {
				mu.Lock()
				defer mu.Unlock()
				return httpHost
			},
		},
		"success: HTTPS request to non-loopback host ignores environment proxy": {
			baseURL:   "https://upstream.invalid:8443",
			wantHost:  "upstream.invalid:8443",
			tlsConfig: tlsCfg,
			hits:      &httpsHits,
			gotHost: func() string {
				mu.Lock()
				defer mu.Unlock()
				return httpsHost
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Verify the test precondition: Go's process-cached ProxyFromEnvironment would route
			// this non-loopback host through the decoy proxy seeded in TestMain.
			probeReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tt.baseURL+"/v1/messages", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			envProxy, err := http.ProxyFromEnvironment(probeReq)
			if err != nil || envProxy == nil || envProxy.Host != testEnvProxyHost {
				t.Fatalf("test precondition failed: ProxyFromEnvironment(%q) = (%v, %v), want host %q", tt.baseURL, envProxy, err, testEnvProxyHost)
			}

			var decoyDialHits atomic.Int32
			var dialer net.Dialer
			dialCtx := func(ctx context.Context, network, addr string) (net.Conn, error) {
				switch addr {
				case "upstream.invalid:8080":
					return dialer.DialContext(ctx, network, httpTarget.Listener.Addr().String())
				case "upstream.invalid:8443":
					return dialer.DialContext(ctx, network, httpsTarget.Listener.Addr().String())
				case testEnvProxyHost:
					decoyDialHits.Add(1)
					return dialer.DialContext(ctx, network, decoyProxy.Listener.Addr().String())
				default:
					t.Errorf("attempted dial to unapproved address %q", addr)
					return nil, fmt.Errorf("blocked unapproved dial to %q", addr)
				}
			}

			control := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialCtx}
			proxied, proxyErr := control.RoundTrip(probeReq)
			if proxied != nil {
				_ = proxied.Body.Close()
			}
			control.CloseIdleConnections()
			if decoyDialHits.Swap(0) != 1 || (proxyErr == nil && proxied.StatusCode != http.StatusBadGateway) {
				t.Fatalf("environment-proxy control missed decoy: response=%v err=%v", proxied, proxyErr)
			}

			beforeHits := tt.hits.Load()
			beforeDecoy := decoyProxyHits.Load()

			client := upstream.NewClient(upstream.Options{
				DialContext: dialCtx,
				TLSConfig:   tt.tlsConfig,
			})
			t.Cleanup(func() { _ = client.Close() })

			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    tt.baseURL,
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
				t.Fatalf("status (-want +got):\n%s", diff)
			}
			if decoyDialHits.Load() != 0 {
				t.Fatalf("transport dialed environment proxy %d time(s), want 0", decoyDialHits.Load())
			}
			if decoyProxyHits.Load() != beforeDecoy {
				t.Fatalf("environment proxy received %d hit(s), want 0", decoyProxyHits.Load()-beforeDecoy)
			}
			if got := tt.hits.Load() - beforeHits; got != 1 {
				t.Fatalf("target received %d request(s), want 1", got)
			}
			if diff := gocmp.Diff(tt.wantHost, tt.gotHost()); diff != "" {
				t.Fatalf("target Host (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTransportConnectionPoolingAndLimit(t *testing.T) {
	t.Run("success: sequential requests reuse pooled keep-alive connection", func(t *testing.T) {
		var mu sync.Mutex
		var addrs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			addrs = append(addrs, r.RemoteAddr)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		}))
		t.Cleanup(srv.Close)

		client := upstream.NewClient(upstream.Options{})
		t.Cleanup(func() { _ = client.Close() })

		for range 2 {
			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    srv.URL,
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			_, err = io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			if closeErr != nil {
				t.Fatalf("closing response body: %v", closeErr)
			}
		}

		mu.Lock()
		defer mu.Unlock()
		if len(addrs) != 2 {
			t.Fatalf("got %d requests, want 2", len(addrs))
		}
		if diff := gocmp.Diff(addrs[0], addrs[1]); diff != "" {
			t.Fatalf("sequential requests did not reuse TCP connection (-first +second):\n%s", diff)
		}
	})

	t.Run("success: pool caps concurrent connections at 64 per host while allowing a second origin", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			const (
				maxPerHost    = 64
				totalRequests = 65
				originAAddr   = "127.0.0.1:12345"
				originBAddr   = "127.0.0.1:12346"
			)

			lnA := newPipeListener(originAAddr)
			lnB := newPipeListener(originBAddr)
			release := make(chan struct{})
			var activeA, maxActiveA, totalA, totalB atomic.Int32

			srvA := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					cur := activeA.Add(1)
					for {
						prev := maxActiveA.Load()
						if cur <= prev || maxActiveA.CompareAndSwap(prev, cur) {
							break
						}
					}
					totalA.Add(1)
					defer activeA.Add(-1)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "ok-a")
				}),
			}
			srvB := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					totalB.Add(1)
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "ok-b")
				}),
			}
			serveDoneA := make(chan struct{})
			go func() {
				defer close(serveDoneA)
				_ = srvA.Serve(lnA)
			}()
			serveDoneB := make(chan struct{})
			go func() {
				defer close(serveDoneB)
				_ = srvB.Serve(lnB)
			}()

			dialCtx := func(ctx context.Context, network, addr string) (net.Conn, error) {
				switch addr {
				case originAAddr:
					return lnA.DialContext(ctx, network, addr)
				case originBAddr:
					return lnB.DialContext(ctx, network, addr)
				default:
					return nil, fmt.Errorf("unexpected dial addr %q", addr)
				}
			}

			client := upstream.NewClient(upstream.Options{DialContext: dialCtx})
			released := false
			releaseAll := func() {
				if !released {
					released = true
					close(release)
				}
			}
			var wgA, wgB sync.WaitGroup
			defer func() {
				releaseAll()
				_ = client.Close()
				_ = srvA.Close()
				_ = srvB.Close()
				_ = lnA.Close()
				_ = lnB.Close()
				wgA.Wait()
				wgB.Wait()
				<-serveDoneA
				<-serveDoneB
				synctest.Wait()
			}()

			errsA := make([]error, totalRequests)
			for i := range totalRequests {
				wgA.Go(func() {
					resp, err := client.Do(t.Context(), upstream.Request{
						Target:     upstream.TargetAnthropic,
						BaseURL:    "http://" + originAAddr,
						Method:     http.MethodPost,
						RequestURI: "/v1/messages",
						Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
					})
					if err != nil {
						errsA[i] = err
						return
					}
					_, readErr := io.ReadAll(resp.Body)
					closeErr := resp.Body.Close()
					errsA[i] = errors.Join(readErr, closeErr)
				})
			}

			synctest.Wait()
			gotActiveA := activeA.Load()
			gotAcceptedA := lnA.accepted.Load()
			if gotActiveA != maxPerHost || gotAcceptedA != maxPerHost {
				releaseAll()
				wgA.Wait()
				for i, err := range errsA {
					if err != nil {
						t.Fatalf("request %d failed: %v", i, err)
					}
				}
				t.Fatalf("while origin A is saturated: active=%d accepted=%d, want %d and %d", gotActiveA, gotAcceptedA, maxPerHost, maxPerHost)
			}

			// While all 64 connections to origin A are held and the 65th is queued,
			// a second origin must still make progress without waiting on origin A.
			beforeB := time.Now()
			originBDone := make(chan error, 1)
			wgB.Go(func() {
				resp, err := client.Do(t.Context(), upstream.Request{
					Target:     upstream.TargetAnthropic,
					BaseURL:    "http://" + originBAddr,
					Method:     http.MethodPost,
					RequestURI: "/v1/messages",
					Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
				})
				if err != nil {
					originBDone <- err
					return
				}
				_, readErr := io.ReadAll(resp.Body)
				closeErr := resp.Body.Close()
				originBDone <- errors.Join(readErr, closeErr)
			})
			synctest.Wait()
			if !time.Now().Equal(beforeB) {
				t.Fatalf("second origin request advanced virtual clock by %v", time.Since(beforeB))
			}
			select {
			case err := <-originBDone:
				if err != nil {
					t.Fatalf("second origin request failed while origin A was saturated: %v", err)
				}
			default:
				t.Fatal("second origin request blocked while origin A held 64 connections (cap is not per-host)")
			}
			if diff := gocmp.Diff(int32(1), totalB.Load()); diff != "" {
				t.Fatalf("origin B completed requests (-want +got):\n%s", diff)
			}

			releaseAll()
			wgA.Wait()

			for i, err := range errsA {
				if err != nil {
					t.Fatalf("request %d failed: %v", i, err)
				}
			}
			if diff := gocmp.Diff(int32(maxPerHost), maxActiveA.Load()); diff != "" {
				t.Fatalf("maximum concurrent connections on origin A (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(int32(totalRequests), totalA.Load()); diff != "" {
				t.Fatalf("total completed requests on origin A (-want +got):\n%s", diff)
			}
		})
	})
}

func TestTransportNoAutoReplayWithBody(t *testing.T) {
	tests := map[string]struct {
		method  string
		headers http.Header
	}{
		"error: POST with body and Idempotency-Key is never replayed after server closes pooled connection": {
			method: http.MethodPost,
			headers: http.Header{
				"Content-Type":      {"application/json"},
				"Idempotency-Key":   {"idem-key-1"},
				"X-Idempotency-Key": {"idem-key-1"},
			},
		},
		"error: GET with non-empty body is never replayed after server closes pooled connection": {
			method:  http.MethodGet,
			headers: http.Header{"Content-Type": {"application/json"}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var warmAddr string
			var probeAddrs []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				if r.URL.Path == "/warmup" {
					mu.Lock()
					warmAddr = r.RemoteAddr
					mu.Unlock()
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "warm")
					return
				}
				mu.Lock()
				probeAddrs = append(probeAddrs, r.RemoteAddr)
				mu.Unlock()
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
			}))
			t.Cleanup(srv.Close)

			client := upstream.NewClient(upstream.Options{})
			t.Cleanup(func() { _ = client.Close() })

			// Warm one pooled keep-alive connection.
			warm, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    srv.URL,
				Method:     http.MethodGet,
				RequestURI: "/warmup",
			})
			if err != nil {
				t.Fatalf("warmup Do failed: %v", err)
			}
			_, readErr := io.ReadAll(warm.Body)
			closeErr := warm.Body.Close()
			if readErr != nil {
				t.Fatalf("reading warmup response body: %v", readErr)
			}
			if closeErr != nil {
				t.Fatalf("closing warmup response body: %v", closeErr)
			}

			resp, err := client.Do(t.Context(), upstream.Request{
				Target:        upstream.TargetAnthropic,
				BaseURL:       srv.URL,
				Method:        tt.method,
				RequestURI:    "/v1/messages",
				ClientHeaders: tt.headers,
				Body:          []byte(`{"model":"claude-sonnet-4-6","attempt":1}`),
			})
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("expected transport error when server closed connection, got nil")
			}

			mu.Lock()
			gotWarm := warmAddr
			gotProbes := slices.Clone(probeAddrs)
			mu.Unlock()

			if gotWarm == "" {
				t.Fatal("warmup request did not record a remote address")
			}
			if len(gotProbes) == 0 {
				t.Fatal("probe request never reached upstream server")
			}
			if diff := gocmp.Diff(gotWarm, gotProbes[0]); diff != "" {
				t.Fatalf("probe did not reuse warmed connection (-warm +probe):\n%s", diff)
			}
			if diff := gocmp.Diff(1, len(gotProbes)); diff != "" {
				t.Fatalf("upstream received replayed request (attempts=%v) (-want +got):\n%s", gotProbes, diff)
			}
		})
	}
}

func TestTransportIdleTimeout(t *testing.T) {
	t.Run("error: 1 hour silence before response headers times out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ln := newPipeListener("127.0.0.1:12346")
			entered := make(chan struct{})
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			type outcome struct {
				resp *upstream.Response
				err  error
			}
			done := make(chan outcome, 1)
			var reqWG sync.WaitGroup
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				reqWG.Wait()
				<-serveDone
				synctest.Wait()
			}()

			reqWG.Go(func() {
				resp, err := client.Do(t.Context(), upstream.Request{
					Target:     upstream.TargetAnthropic,
					BaseURL:    "http://127.0.0.1:12346",
					Method:     http.MethodPost,
					RequestURI: "/v1/messages",
					Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
				})
				done <- outcome{resp: resp, err: err}
			})

			select {
			case <-entered:
			case res := <-done:
				if res.resp != nil && res.resp.Body != nil {
					_ = res.resp.Body.Close()
				}
				t.Fatalf("Do returned before server received request: err=%v", res.err)
			}

			time.Sleep(time.Hour - time.Nanosecond)
			synctest.Wait()
			select {
			case res := <-done:
				if res.resp != nil && res.resp.Body != nil {
					_ = res.resp.Body.Close()
				}
				t.Fatalf("Do timed out before 1 hour elapsed: err=%v", res.err)
			default:
			}

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case res := <-done:
				if res.resp != nil && res.resp.Body != nil {
					_ = res.resp.Body.Close()
				}
				if !errors.Is(res.err, upstream.ErrTimeout) {
					t.Fatalf("expected ErrTimeout after 1 hour silence, got %v", res.err)
				}
			default:
				t.Fatal("Do did not time out once 1 hour of silence elapsed")
			}
		})
	})

	t.Run("error: 1 hour silence mid-stream times out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			const firstFrame = "event: message_start\ndata: {}\n\n"
			ln := newPipeListener("127.0.0.1:12347")
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, firstFrame)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			var readWG sync.WaitGroup
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				readWG.Wait()
				<-serveDone
				synctest.Wait()
			}()

			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetProxy,
				BaseURL:    "http://127.0.0.1:12347",
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"gpt-5.6-sol"}`),
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			gotFirst := make([]byte, len(firstFrame))
			if _, err := io.ReadFull(resp.Body, gotFirst); err != nil {
				t.Fatalf("reading first stream frame: %v", err)
			}
			if diff := gocmp.Diff(firstFrame, string(gotFirst)); diff != "" {
				t.Fatalf("first stream frame mismatch (-want +got):\n%s", diff)
			}

			readErr := make(chan error, 1)
			readWG.Go(func() {
				buf := make([]byte, 64)
				_, err := resp.Body.Read(buf)
				readErr <- err
			})

			time.Sleep(time.Hour - time.Nanosecond)
			synctest.Wait()
			select {
			case err := <-readErr:
				t.Fatalf("stream read timed out before 1 hour elapsed: err=%v", err)
			default:
			}

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case err := <-readErr:
				if !errors.Is(err, upstream.ErrTimeout) {
					t.Fatalf("expected ErrTimeout after 1 hour mid-stream silence, got %v", err)
				}
			default:
				t.Fatal("stream read did not time out once 1 hour of mid-stream silence elapsed")
			}
		})
	})

	t.Run("success: periodic stream activity resets the 1 hour idle deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ln := newPipeListener("127.0.0.1:12348")

			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					f, _ := w.(http.Flusher)
					for i := range 3 {
						if i > 0 {
							select {
							case <-time.After(40 * time.Minute):
							case <-r.Context().Done():
								return
							}
						}
						_, _ = io.WriteString(w, "data: chunk\n\n")
						if f != nil {
							f.Flush()
						}
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			defer func() {
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				<-serveDone
				synctest.Wait()
			}()

			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    "http://127.0.0.1:12348",
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("stream lasting 80m with 40m gaps failed: %v", err)
			}
			want := "data: chunk\n\ndata: chunk\n\ndata: chunk\n\n"
			if diff := gocmp.Diff(want, string(body)); diff != "" {
				t.Fatalf("stream body mismatch (-want +got):\n%s", diff)
			}
		})
	})
}

func requireImmediateClose(t *testing.T, client *upstream.Client) {
	t.Helper()
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	synctest.Wait()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Client.Close returned error: %v", err)
		}
	default:
		t.Fatal("Client.Close did not return without advancing virtual time")
	}
}

func TestTransportShutdownBoundsRequest(t *testing.T) {
	t.Run("error: Client.Close aborts in-flight request waiting for headers", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ln := newPipeListener("127.0.0.1:12349")
			entered := make(chan struct{})
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			var reqWG sync.WaitGroup
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				reqWG.Wait()
				<-serveDone
				synctest.Wait()
			}()

			done := make(chan error, 1)
			reqWG.Go(func() {
				resp, err := client.Do(t.Context(), upstream.Request{
					Target:     upstream.TargetAnthropic,
					BaseURL:    "http://127.0.0.1:12349",
					Method:     http.MethodPost,
					RequestURI: "/v1/messages",
					Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
				})
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				done <- err
			})

			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("Do returned before server received request: %v", err)
			}

			requireImmediateClose(t, client)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected error when Client.Close aborted in-flight request")
				}
			default:
				t.Fatal("Client.Close did not immediately abort in-flight request")
			}
		})
	})

	t.Run("error: Client.Close aborts active response stream", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			const firstFrame = "data: first\n\n"
			ln := newPipeListener("127.0.0.1:12350")
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, firstFrame)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			var readWG sync.WaitGroup
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				readWG.Wait()
				<-serveDone
				synctest.Wait()
			}()

			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    "http://127.0.0.1:12350",
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			gotFirst := make([]byte, len(firstFrame))
			if _, err := io.ReadFull(resp.Body, gotFirst); err != nil {
				t.Fatalf("reading first stream frame: %v", err)
			}
			if diff := gocmp.Diff(firstFrame, string(gotFirst)); diff != "" {
				t.Fatalf("first stream frame mismatch (-want +got):\n%s", diff)
			}

			readErr := make(chan error, 1)
			readWG.Go(func() {
				buf := make([]byte, 64)
				_, err := resp.Body.Read(buf)
				readErr <- err
			})
			synctest.Wait()

			requireImmediateClose(t, client)
			select {
			case err := <-readErr:
				if err == nil {
					t.Fatal("expected stream read error after Client.Close")
				}
			default:
				t.Fatal("Client.Close did not immediately abort active response stream")
			}
		})
	})

	t.Run("error: canceled request context aborts in-flight request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ln := newPipeListener("127.0.0.1:12351")
			entered := make(chan struct{})
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			var reqWG sync.WaitGroup
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				reqWG.Wait()
				<-serveDone
				synctest.Wait()
			}()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)
			reqWG.Go(func() {
				resp, err := client.Do(ctx, upstream.Request{
					Target:     upstream.TargetAnthropic,
					BaseURL:    "http://127.0.0.1:12351",
					Method:     http.MethodPost,
					RequestURI: "/v1/messages",
					Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
				})
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				done <- err
			})

			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("Do returned before server received request: %v", err)
			}

			beforeCancel := time.Now()
			cancel()
			synctest.Wait()
			if !time.Now().Equal(beforeCancel) {
				t.Fatalf("context cancel advanced virtual clock by %v", time.Since(beforeCancel))
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected context.Canceled, got %v", err)
				}
			default:
				t.Fatal("canceling request context did not immediately abort in-flight request")
			}
		})
	})

	t.Run("error: Do after Client.Close fails immediately", func(t *testing.T) {
		srv, captured := startCaptureServer(t, http.StatusOK, `{"ok":true}`)
		client := upstream.NewClient(upstream.Options{})
		if err := client.Close(); err != nil {
			t.Fatalf("Client.Close returned error: %v", err)
		}
		resp, err := client.Do(t.Context(), upstream.Request{
			Target:     upstream.TargetAnthropic,
			BaseURL:    srv.URL,
			Method:     http.MethodPost,
			RequestURI: "/v1/messages",
			Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
		})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("expected error calling Do after Close")
		}
		if len(captured()) != 0 {
			t.Fatalf("closed client sent %d requests, want 0", len(captured()))
		}
	})
}

func TestTransportInvalidRequests(t *testing.T) {
	srv, _ := startCaptureServer(t, http.StatusOK, `{"ok":true}`)
	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })

	tests := map[string]upstream.Request{
		"error: empty BaseURL": {
			Target:     upstream.TargetAnthropic,
			BaseURL:    "",
			Method:     http.MethodPost,
			RequestURI: "/v1/messages",
		},
		"error: malformed BaseURL": {
			Target:     upstream.TargetAnthropic,
			BaseURL:    "://bad-url",
			Method:     http.MethodPost,
			RequestURI: "/v1/messages",
		},
		"error: unsupported scheme": {
			Target:     upstream.TargetAnthropic,
			BaseURL:    "ftp://127.0.0.1:21",
			Method:     http.MethodPost,
			RequestURI: "/v1/messages",
		},
		"error: unknown target": {
			Target:     upstream.Target("unknown"),
			BaseURL:    srv.URL,
			Method:     http.MethodPost,
			RequestURI: "/v1/messages",
		},
	}

	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			resp, err := client.Do(t.Context(), req)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatal("expected error for invalid request, got nil")
			}
			// Guard against passing on the compile-only stub without reaching validation.
			if err.Error() == "not implemented" {
				t.Fatalf("got stub error: %v", err)
			}
		})
	}
}

func TestTransportChunkedTrailerPreserved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Trailer", "X-Checksum")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "chunked-payload")
		w.Header().Set("X-Checksum", "sha256=deadbeef")
	}))
	t.Cleanup(srv.Close)

	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })

	resp, err := client.Do(t.Context(), upstream.Request{
		Target:     upstream.TargetAnthropic,
		BaseURL:    srv.URL,
		Method:     http.MethodPost,
		RequestURI: "/v1/messages",
		Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
	})
	if err != nil {
		t.Fatalf("Do returned error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
		t.Fatalf("status (-want +got):\n%s", diff)
	}
	if vals := headerValuesCI(t, resp.RelayHeader, "Transfer-Encoding"); len(vals) != 0 {
		t.Errorf("RelayHeader kept Transfer-Encoding: %v", vals)
	}
	if vals := headerValuesCI(t, resp.RelayHeader, "Keep-Alive"); len(vals) != 0 {
		t.Errorf("RelayHeader kept Keep-Alive: %v", vals)
	}
	if diff := gocmp.Diff([]string{"X-Checksum"}, headerValuesCI(t, resp.RelayHeader, "Trailer")); diff != "" {
		t.Errorf("RelayHeader Trailer declaration (-want +got):\n%s", diff)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading chunked body: %v", err)
	}
	if diff := gocmp.Diff("chunked-payload", string(body)); diff != "" {
		t.Fatalf("chunked body (-want +got):\n%s", diff)
	}
}

func TestLiveUpstreamHeadersToUpstreamState(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		headers          http.Header
		wantExp          string
		wantCaptureRetry []string
		wantRelayRetry   []string
		wantCaptureReset []string
		wantRelayReset   []string
	}{
		"success: repeated Retry-After stays unjoined in Header, first-only in RelayHeader": {
			headers: http.Header{
				"X-Openai-Request-Id": {"req_1"},
				"Retry-After":         {"30", "120"},
			},
			wantExp:          "2026-09-28T10:00:30.000Z",
			wantCaptureRetry: []string{"30", "120"},
			wantRelayRetry:   []string{"30"},
		},
		"success: IMF-fixdate Retry-After parses exact instant": {
			headers: http.Header{
				"X-Openai-Request-Id": {"req_2"},
				"Retry-After":         {"Mon, 28 Sep 2026 10:05:00 GMT"},
			},
			wantExp:          "2026-09-28T10:05:00.000Z",
			wantCaptureRetry: []string{"Mon, 28 Sep 2026 10:05:00 GMT"},
			wantRelayRetry:   []string{"Mon, 28 Sep 2026 10:05:00 GMT"},
		},
		"success: single reset header sets epoch expiry": {
			headers: http.Header{
				"X-Openai-Request-Id":               {"req_3"},
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700"},
			},
			wantExp:          "2026-09-28T12:15:00.000Z",
			wantCaptureReset: []string{"1790597700"},
			wantRelayReset:   []string{"1790597700"},
		},
		"success: duplicate reset headers stay unjoined in Header and join with comma-space in RelayHeader": {
			headers: http.Header{
				"X-Openai-Request-Id":               {"req_4"},
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700", "1790597715"},
			},
			wantExp:          "2026-09-28T10:01:00.000Z",
			wantCaptureReset: []string{"1790597700", "1790597715"},
			wantRelayReset:   []string{"1790597700, 1790597715"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, vals := range tt.headers {
					for _, v := range vals {
						w.Header().Add(k, v)
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Keep-Alive", "timeout=5")
				w.Header().Set("Upgrade", "websocket")
				w.Header().Add("Set-Cookie", "a=1")
				w.Header().Add("Set-Cookie", "b=2")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limit"}}`)
			}))
			t.Cleanup(srv.Close)

			client := upstream.NewClient(upstream.Options{})
			t.Cleanup(func() { _ = client.Close() })

			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetProxy,
				BaseURL:    srv.URL,
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"gpt-5.6-sol"}`),
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			if diff := gocmp.Diff(http.StatusTooManyRequests, resp.StatusCode); diff != "" {
				t.Fatalf("status (-want +got):\n%s", diff)
			}

			relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				maps.Copy(w.Header(), resp.RelayHeader)
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(body)
			}))
			t.Cleanup(relay.Close)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, relay.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			received, err := relay.Client().Do(req)
			if err != nil {
				t.Fatalf("receiving relay response: %v", err)
			}
			defer func() { _ = received.Body.Close() }()
			if vals := received.Header.Values("Keep-Alive"); len(vals) != 0 {
				t.Errorf("RelayHeader kept Keep-Alive: %v", vals)
			}
			if diff := gocmp.Diff([]string{"websocket"}, headerValuesCI(t, resp.RelayHeader, "Upgrade")); diff != "" {
				t.Errorf("RelayHeader Upgrade (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff([]string{"a=1", "b=2"}, received.Header.Values("Set-Cookie")); diff != "" {
				t.Errorf("RelayHeader Set-Cookie (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff([]string{"a=1", "b=2"}, resp.Header.Values("Set-Cookie")); diff != "" {
				t.Errorf("Header Set-Cookie (-want +got):\n%s", diff)
			}
			if tt.wantCaptureRetry != nil {
				if diff := gocmp.Diff(tt.wantCaptureRetry, resp.Header.Values("Retry-After")); diff != "" {
					t.Errorf("Header Retry-After (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.wantRelayRetry, received.Header.Values("Retry-After")); diff != "" {
					t.Errorf("RelayHeader Retry-After (-want +got):\n%s", diff)
				}
			}
			if tt.wantCaptureReset != nil {
				if diff := gocmp.Diff(tt.wantCaptureReset, resp.Header.Values("Anthropic-Ratelimit-Unified-Reset")); diff != "" {
					t.Errorf("Header Anthropic-Ratelimit-Unified-Reset (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.wantRelayReset, received.Header.Values("Anthropic-Ratelimit-Unified-Reset")); diff != "" {
					t.Errorf("RelayHeader Anthropic-Ratelimit-Unified-Reset (-want +got):\n%s", diff)
				}
			}

			// Mutating RelayHeader must not affect the parsed capture Header copy.
			headerSnapshot := resp.Header.Clone()
			resp.RelayHeader.Set("Upgrade", "mutated")
			resp.RelayHeader.Set("X-Relay-Only", "mutated")
			if vals := resp.RelayHeader.Values("Set-Cookie"); len(vals) > 0 {
				vals[0] = "mutated=1"
			}
			if diff := gocmp.Diff(headerSnapshot, resp.Header); diff != "" {
				t.Fatalf("mutating RelayHeader altered Header (-want +got):\n%s", diff)
			}

			// Parsed Header passed directly to upstreamstate must preserve exact occurrences.
			store := upstreamstate.Store{
				Dir: t.TempDir(),
				Now: func() time.Time { return now },
			}
			block, err := store.SetBlocked(resp.StatusCode, "headers:x-openai-request-id", resp.Header)
			if err != nil {
				t.Fatalf("SetBlocked returned error: %v", err)
			}
			if diff := gocmp.Diff(tt.wantExp, block.ExpiresAt); diff != "" {
				t.Fatalf("upstreamstate expiry (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNewTransport(t *testing.T) {
	t.Run("success: returned transport configures safe policy and distinct instances", func(t *testing.T) {
		tr1 := upstream.NewTransport(upstream.Options{})
		if tr1 == nil {
			t.Fatal("NewTransport returned nil")
		}
		t.Cleanup(tr1.CloseIdleConnections)

		tr2 := upstream.NewTransport(upstream.Options{})
		if tr2 == nil {
			t.Fatal("second NewTransport returned nil")
		}
		t.Cleanup(tr2.CloseIdleConnections)

		if tr1 == tr2 {
			t.Fatal("NewTransport returned the same *http.Transport instance twice")
		}
		if tr1.Proxy != nil {
			t.Error("NewTransport returned non-nil Proxy, want nil (no environment proxy)")
		}
		if !tr1.DisableCompression {
			t.Error("NewTransport returned DisableCompression=false, want true")
		}
		if tr1.DisableKeepAlives {
			t.Error("NewTransport returned DisableKeepAlives=true, want false (keep-alive pooling enabled)")
		}
		if diff := gocmp.Diff(64, tr1.MaxConnsPerHost); diff != "" {
			t.Errorf("NewTransport MaxConnsPerHost (-want +got):\n%s", diff)
		}
		h2Disabled := (tr1.Protocols != nil && tr1.Protocols.HTTP1() && !tr1.Protocols.HTTP2() && !tr1.Protocols.UnencryptedHTTP2()) ||
			(!tr1.ForceAttemptHTTP2 && tr1.TLSNextProto != nil && len(tr1.TLSNextProto) == 0)
		if !h2Disabled {
			t.Errorf("NewTransport did not disable HTTP/2 (Protocols=%v, ForceAttemptHTTP2=%v, TLSNextProto=%v)", tr1.Protocols, tr1.ForceAttemptHTTP2, tr1.TLSNextProto)
		}
	})

	t.Run("success: HTTP and HTTPS non-loopback requests ignore environment proxy without inference headers", func(t *testing.T) {
		var decoyProxyHits atomic.Int32
		decoyProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			decoyProxyHits.Add(1)
			http.Error(w, "environment proxy must never be dialed", http.StatusBadGateway)
		}))
		t.Cleanup(decoyProxy.Close)

		var mu sync.Mutex
		var httpHits, httpsHits atomic.Int32
		var seenAuth, seenContentType, seenBody string

		handleOIDC := func(hits *atomic.Int32) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				hits.Add(1)
				mu.Lock()
				seenAuth = r.Header.Get("Authorization")
				seenContentType = r.Header.Get("Content-Type")
				seenBody = string(body)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"access_token":"refreshed"}`)
			}
		}

		httpTarget := httptest.NewServer(handleOIDC(&httpHits))
		t.Cleanup(httpTarget.Close)

		httpsTarget := httptest.NewTLSServer(handleOIDC(&httpsHits))
		t.Cleanup(httpsTarget.Close)

		tlsCfg := httpsTarget.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		tlsCfg.ServerName = "127.0.0.1"

		cases := map[string]struct {
			url       string
			tlsConfig *tls.Config
			hits      *atomic.Int32
		}{
			"success: HTTP OIDC form POST ignores environment proxy": {
				url:  "http://upstream.invalid:8080/oauth/token",
				hits: &httpHits,
			},
			"success: HTTPS OIDC form POST ignores environment proxy": {
				url:       "https://upstream.invalid:8443/oauth/token",
				tlsConfig: tlsCfg,
				hits:      &httpsHits,
			},
		}

		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				var decoyDialHits atomic.Int32
				var dialer net.Dialer
				dialCtx := func(ctx context.Context, network, addr string) (net.Conn, error) {
					switch addr {
					case "upstream.invalid:8080":
						return dialer.DialContext(ctx, network, httpTarget.Listener.Addr().String())
					case "upstream.invalid:8443":
						return dialer.DialContext(ctx, network, httpsTarget.Listener.Addr().String())
					case testEnvProxyHost:
						decoyDialHits.Add(1)
						return dialer.DialContext(ctx, network, decoyProxy.Listener.Addr().String())
					default:
						t.Errorf("attempted dial to unapproved address %q", addr)
						return nil, fmt.Errorf("blocked unapproved dial to %q", addr)
					}
				}

				probeReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tc.url, nil)
				if err != nil {
					t.Fatalf("NewRequestWithContext: %v", err)
				}
				control := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialCtx}
				proxied, proxyErr := control.RoundTrip(probeReq)
				if proxied != nil {
					_ = proxied.Body.Close()
				}
				control.CloseIdleConnections()
				if decoyDialHits.Swap(0) != 1 || (proxyErr == nil && proxied.StatusCode != http.StatusBadGateway) {
					t.Fatalf("environment-proxy control missed decoy: response=%v err=%v", proxied, proxyErr)
				}

				tr := upstream.NewTransport(upstream.Options{DialContext: dialCtx, TLSConfig: tc.tlsConfig})
				if tr == nil {
					t.Fatal("NewTransport returned nil")
				}
				t.Cleanup(tr.CloseIdleConnections)

				beforeHits := tc.hits.Load()
				beforeDecoy := decoyProxyHits.Load()

				const formPayload = "grant_type=refresh_token&refresh_token=tok"
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, tc.url, strings.NewReader(formPayload))
				if err != nil {
					t.Fatalf("NewRequestWithContext: %v", err)
				}
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				req.Header.Set("Authorization", "Bearer oidc-refresh-auth")

				resp, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatalf("RoundTrip returned error: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()
				if _, err := io.ReadAll(resp.Body); err != nil {
					t.Fatalf("reading response body: %v", err)
				}
				if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
					t.Fatalf("status (-want +got):\n%s", diff)
				}
				if decoyDialHits.Load() != 0 {
					t.Fatalf("transport dialed environment proxy %d time(s), want 0", decoyDialHits.Load())
				}
				if decoyProxyHits.Load() != beforeDecoy {
					t.Fatalf("environment proxy received %d hit(s), want 0", decoyProxyHits.Load()-beforeDecoy)
				}
				if got := tc.hits.Load() - beforeHits; got != 1 {
					t.Fatalf("target received %d request(s), want 1", got)
				}
				mu.Lock()
				gotAuth, gotCT, gotBody := seenAuth, seenContentType, seenBody
				mu.Unlock()
				if diff := gocmp.Diff("Bearer oidc-refresh-auth", gotAuth); diff != "" {
					t.Errorf("Authorization (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("application/x-www-form-urlencoded", gotCT); diff != "" {
					t.Errorf("Content-Type (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(formPayload, gotBody); diff != "" {
					t.Errorf("form body (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("success: RoundTrip refuses redirects, skips auto-decompression, and forces HTTP/1.1 over TLS", func(t *testing.T) {
		var rawCompressed bytes.Buffer
		gz := gzip.NewWriter(&rawCompressed)
		if _, err := gz.Write([]byte(`{"issuer":"https://oidc.invalid"}`)); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		wantGzip := rawCompressed.Bytes()

		var redirectTargetHits atomic.Int32
		var protoMajor, protoMinor atomic.Int32
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protoMajor.Store(int32(r.ProtoMajor))
			protoMinor.Store(int32(r.ProtoMinor))
			switch r.URL.Path {
			case "/redirect":
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(http.StatusFound)
			case "/redirect-target":
				redirectTargetHits.Add(1)
				w.WriteHeader(http.StatusOK)
			case "/gzip":
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", strconv.Itoa(len(wantGzip)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(wantGzip)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		srv.EnableHTTP2 = true
		srv.StartTLS()
		t.Cleanup(srv.Close)

		tlsCfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		tr := upstream.NewTransport(upstream.Options{TLSConfig: tlsCfg})
		if tr == nil {
			t.Fatal("NewTransport returned nil")
		}
		t.Cleanup(tr.CloseIdleConnections)

		reqRedir, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/redirect", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		respRedir, err := tr.RoundTrip(reqRedir)
		if err != nil {
			t.Fatalf("RoundTrip /redirect: %v", err)
		}
		_, _ = io.ReadAll(respRedir.Body)
		_ = respRedir.Body.Close()
		if diff := gocmp.Diff(http.StatusFound, respRedir.StatusCode); diff != "" {
			t.Fatalf("redirect status (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff("/redirect-target", respRedir.Header.Get("Location")); diff != "" {
			t.Fatalf("redirect Location (-want +got):\n%s", diff)
		}
		if hits := redirectTargetHits.Load(); hits != 0 {
			t.Fatalf("redirect target received %d hit(s), want 0", hits)
		}

		reqGzip, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/gzip", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		respGzip, err := tr.RoundTrip(reqGzip)
		if err != nil {
			t.Fatalf("RoundTrip /gzip: %v", err)
		}
		gotGzip, readErr := io.ReadAll(respGzip.Body)
		_ = respGzip.Body.Close()
		if readErr != nil {
			t.Fatalf("reading gzip body: %v", readErr)
		}
		if diff := gocmp.Diff("gzip", respGzip.Header.Get("Content-Encoding")); diff != "" {
			t.Errorf("Content-Encoding (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(wantGzip, gotGzip); diff != "" {
			t.Fatalf("gzip bytes altered (-want +got):\n%s", diff)
		}
		if protoMajor.Load() != 1 || protoMinor.Load() != 1 {
			t.Fatalf("TLS server received HTTP/%d.%d, want HTTP/1.1", protoMajor.Load(), protoMinor.Load())
		}
	})

	t.Run("error: 1 hour socket-inactivity deadline fires at boundary", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ln := newPipeListener("127.0.0.1:12352")
			entered := make(chan struct{})
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			tr := upstream.NewTransport(upstream.Options{DialContext: ln.DialContext})
			var reqWG sync.WaitGroup
			defer func() {
				close(release)
				if tr != nil {
					tr.CloseIdleConnections()
				}
				_ = srv.Close()
				_ = ln.Close()
				reqWG.Wait()
				<-serveDone
				synctest.Wait()
			}()
			if tr == nil {
				t.Fatal("NewTransport returned nil")
			}

			done := make(chan error, 1)
			reqWG.Go(func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:12352/.well-known/openid-configuration", nil)
				if err != nil {
					done <- err
					return
				}
				resp, err := tr.RoundTrip(req)
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				done <- err
			})

			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("RoundTrip returned before server received request: err=%v", err)
			}

			time.Sleep(time.Hour - time.Nanosecond)
			synctest.Wait()
			select {
			case err := <-done:
				t.Fatalf("RoundTrip timed out before 1 hour elapsed: err=%v", err)
			default:
			}

			time.Sleep(time.Nanosecond)
			synctest.Wait()
			select {
			case err := <-done:
				if !errors.Is(err, upstream.ErrTimeout) {
					t.Fatalf("expected ErrTimeout after 1 hour silence, got %v", err)
				}
			default:
				t.Fatal("RoundTrip did not time out once 1 hour of silence elapsed")
			}
		})
	})

	t.Run("success: transport instance is distinct and isolated from Client lifetime", func(t *testing.T) {
		const (
			part1 = "data: part1\n\n"
			part2 = "data: part2\n\n"
		)
		releasePart2 := make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(releasePart2) })
		t.Cleanup(releaseOnce)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/stream" {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, part1)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				select {
				case <-releasePart2:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, part2)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				return
			}
			w.Header().Set("X-Peer", r.RemoteAddr)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok")
		}))
		t.Cleanup(srv.Close)

		client := upstream.NewClient(upstream.Options{})
		t.Cleanup(func() { _ = client.Close() })

		authTr := upstream.NewTransport(upstream.Options{})
		if authTr == nil {
			t.Fatal("NewTransport returned nil")
		}
		t.Cleanup(authTr.CloseIdleConnections)

		pingClient := func() string {
			t.Helper()
			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    srv.URL,
				Method:     http.MethodGet,
				RequestURI: "/ping",
			})
			if err != nil {
				t.Fatalf("client.Do /ping returned error: %v", err)
			}
			_, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("draining client /ping body: read=%v close=%v", readErr, closeErr)
			}
			return resp.Header.Get("X-Peer")
		}

		pingAuth := func() string {
			t.Helper()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/ping", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := authTr.RoundTrip(req)
			if err != nil {
				t.Fatalf("authTr.RoundTrip /ping returned error: %v", err)
			}
			_, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("draining auth /ping body: read=%v close=%v", readErr, closeErr)
			}
			return resp.Header.Get("X-Peer")
		}

		clientAddr1 := pingClient()
		authAddr1 := pingAuth()
		if clientAddr1 == "" || authAddr1 == "" {
			t.Fatalf("missing peer addresses: client=%q auth=%q", clientAddr1, authAddr1)
		}
		if clientAddr1 == authAddr1 {
			t.Fatalf("NewClient and NewTransport shared TCP connection %q", clientAddr1)
		}

		// Closing idle connections on authTr must not disturb client's pooled connection.
		authTr.CloseIdleConnections()
		clientAddr2 := pingClient()
		if diff := gocmp.Diff(clientAddr1, clientAddr2); diff != "" {
			t.Fatalf("authTr.CloseIdleConnections disturbed client pool (-before +after):\n%s", diff)
		}

		// Closing client must not cancel an open stream on authTr or prevent later authTr calls.
		streamReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/stream", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		streamResp, err := authTr.RoundTrip(streamReq)
		if err != nil {
			t.Fatalf("authTr.RoundTrip /stream returned error: %v", err)
		}
		defer func() { _ = streamResp.Body.Close() }()

		gotPart1 := make([]byte, len(part1))
		if _, err := io.ReadFull(streamResp.Body, gotPart1); err != nil {
			t.Fatalf("reading stream part1: %v", err)
		}
		if diff := gocmp.Diff(part1, string(gotPart1)); diff != "" {
			t.Fatalf("stream part1 (-want +got):\n%s", diff)
		}

		if err := client.Close(); err != nil {
			t.Fatalf("client.Close returned error: %v", err)
		}

		releaseOnce()
		gotPart2 := make([]byte, len(part2))
		if _, err := io.ReadFull(streamResp.Body, gotPart2); err != nil {
			t.Fatalf("authTr stream failed after client.Close: %v", err)
		}
		if diff := gocmp.Diff(part2, string(gotPart2)); diff != "" {
			t.Fatalf("stream part2 (-want +got):\n%s", diff)
		}

		if peerAfterClose := pingAuth(); peerAfterClose == "" {
			t.Fatal("authTr /ping after client.Close returned empty peer")
		}
	})
}

func TestSendAndContextOwnership(t *testing.T) {
	t.Run("success: Send bound to an immutable Request performs one attempt per call and two calls are two attempts", func(t *testing.T) {
		srv, captured := startCaptureServer(t, http.StatusOK, `{"ok":true}`)
		client := upstream.NewClient(upstream.Options{})
		t.Cleanup(func() { _ = client.Close() })

		reqHeaders := http.Header{
			"Content-Type": {"application/json"},
			"X-Multi":      {"one", "two"},
		}
		reqBody := []byte(`{"model":"gpt-5.6-sol","attempt":"immutable"}`)
		req := upstream.Request{
			Target:        upstream.TargetProxy,
			BaseURL:       srv.URL,
			Method:        http.MethodPost,
			RequestURI:    "/v1/messages",
			ClientHeaders: reqHeaders,
			Body:          reqBody,
		}
		headersBefore := reqHeaders.Clone()
		bodyBefore := bytes.Clone(reqBody)

		var send upstream.Send = func(ctx context.Context) (*upstream.Response, error) {
			return client.Do(ctx, req)
		}

		for attempt := 1; attempt <= 2; attempt++ {
			resp, err := send(t.Context())
			if err != nil {
				t.Fatalf("Send attempt %d returned error: %v", attempt, err)
			}
			_, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("Send attempt %d body drain: read=%v close=%v", attempt, readErr, closeErr)
			}
			if diff := gocmp.Diff(attempt, len(captured())); diff != "" {
				t.Fatalf("upstream attempt count after call %d (-want +got):\n%s", attempt, diff)
			}
		}

		all := captured()
		for i, rec := range all {
			if diff := gocmp.Diff(bodyBefore, rec.Body); diff != "" {
				t.Errorf("attempt %d body (-want +got):\n%s", i+1, diff)
			}
			if diff := gocmp.Diff([]string{"one, two"}, rec.Header.Values("X-Multi")); diff != "" {
				t.Errorf("attempt %d X-Multi (-want +got):\n%s", i+1, diff)
			}
		}
		if diff := gocmp.Diff(headersBefore, reqHeaders); diff != "" {
			t.Errorf("Send mutated bound Request headers (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(bodyBefore, reqBody); diff != "" {
			t.Errorf("Send mutated bound Request body (-want +got):\n%s", diff)
		}
	})

	t.Run("error: Send performs one attempt on closed pooled connection and a second call is a second attempt", func(t *testing.T) {
		var mu sync.Mutex
		var warmAddr string
		var probeAddrs []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			if r.URL.Path == "/warmup" {
				mu.Lock()
				warmAddr = r.RemoteAddr
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "warm")
				return
			}
			mu.Lock()
			probeAddrs = append(probeAddrs, r.RemoteAddr)
			attempt := len(probeAddrs)
			mu.Unlock()
			if attempt == 1 {
				if hj, ok := w.(http.Hijacker); ok {
					if conn, _, err := hj.Hijack(); err == nil {
						_ = conn.Close()
					}
				}
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
		t.Cleanup(srv.Close)

		client := upstream.NewClient(upstream.Options{})
		t.Cleanup(func() { _ = client.Close() })

		warm, err := client.Do(t.Context(), upstream.Request{
			Target:     upstream.TargetAnthropic,
			BaseURL:    srv.URL,
			Method:     http.MethodGet,
			RequestURI: "/warmup",
		})
		if err != nil {
			t.Fatalf("warmup Do failed: %v", err)
		}
		_, readErr := io.ReadAll(warm.Body)
		closeErr := warm.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("warmup body drain: read=%v close=%v", readErr, closeErr)
		}

		req := upstream.Request{
			Target:     upstream.TargetAnthropic,
			BaseURL:    srv.URL,
			Method:     http.MethodPost,
			RequestURI: "/v1/messages",
			ClientHeaders: http.Header{
				"Content-Type":    {"application/json"},
				"Idempotency-Key": {"send-idem-1"},
			},
			Body: []byte(`{"model":"claude-sonnet-4-6"}`),
		}
		var send upstream.Send = func(ctx context.Context) (*upstream.Response, error) {
			return client.Do(ctx, req)
		}

		resp1, err := send(t.Context())
		if resp1 != nil && resp1.Body != nil {
			_ = resp1.Body.Close()
		}
		if err == nil {
			t.Fatal("first Send call expected error on closed pooled connection, got nil")
		}
		mu.Lock()
		gotWarm := warmAddr
		afterFirst := slices.Clone(probeAddrs)
		mu.Unlock()
		if diff := gocmp.Diff(1, len(afterFirst)); diff != "" {
			t.Fatalf("first Send call replayed automatically (attempts=%v) (-want +got):\n%s", afterFirst, diff)
		}
		if diff := gocmp.Diff(gotWarm, afterFirst[0]); diff != "" {
			t.Fatalf("first Send call did not reuse warmed connection (-warm +probe):\n%s", diff)
		}

		resp2, err := send(t.Context())
		if err != nil {
			t.Fatalf("second explicit Send call failed: %v", err)
		}
		_, readErr = io.ReadAll(resp2.Body)
		closeErr = resp2.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("second Send body drain: read=%v close=%v", readErr, closeErr)
		}
		if diff := gocmp.Diff(http.StatusOK, resp2.StatusCode); diff != "" {
			t.Fatalf("second Send status (-want +got):\n%s", diff)
		}
		mu.Lock()
		afterSecond := slices.Clone(probeAddrs)
		mu.Unlock()
		if diff := gocmp.Diff(2, len(afterSecond)); diff != "" {
			t.Fatalf("two Send calls must produce two upstream attempts (-want +got):\n%s", diff)
		}
	})

	t.Run("success: Send bound to service context survives caller cancellation and is bounded by Client.Close", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			const (
				firstFrame  = "data: first\n\n"
				secondFrame = "data: second\n\n"
			)
			ln := newPipeListener("127.0.0.1:12353")
			sendSecond := make(chan struct{})
			release := make(chan struct{})
			sendSecondOnce := sync.OnceFunc(func() { close(sendSecond) })
			releaseOnce := sync.OnceFunc(func() { close(release) })
			var sawEarlyCancel atomic.Bool

			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, firstFrame)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					select {
					case <-sendSecond:
					case <-r.Context().Done():
						sawEarlyCancel.Store(true)
						return
					}
					_, _ = io.WriteString(w, secondFrame)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			var readWG sync.WaitGroup
			defer func() {
				sendSecondOnce()
				releaseOnce()
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				readWG.Wait()
				<-serveDone
				synctest.Wait()
			}()

			serviceCtx, cancelService := context.WithCancel(t.Context())
			defer cancelService()
			callerCtx, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()

			req := upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    "http://127.0.0.1:12353",
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
			}
			// Caller deliberately binds the provider exchange to serviceCtx rather than callerCtx.
			var send upstream.Send = func(_ context.Context) (*upstream.Response, error) {
				return client.Do(serviceCtx, req)
			}

			resp, err := send(callerCtx)
			if err != nil {
				t.Fatalf("Send returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			gotFirst := make([]byte, len(firstFrame))
			if _, err := io.ReadFull(resp.Body, gotFirst); err != nil {
				t.Fatalf("reading first frame: %v", err)
			}
			if diff := gocmp.Diff(firstFrame, string(gotFirst)); diff != "" {
				t.Fatalf("first frame (-want +got):\n%s", diff)
			}

			// Canceling the caller's request context leaves the service-bound provider exchange alive.
			cancelCaller()
			synctest.Wait()
			if sawEarlyCancel.Load() {
				t.Fatal("provider request was canceled when only callerCtx was canceled")
			}

			sendSecondOnce()
			gotSecond := make([]byte, len(secondFrame))
			if _, err := io.ReadFull(resp.Body, gotSecond); err != nil {
				t.Fatalf("reading second frame after callerCtx cancel: %v", err)
			}
			if diff := gocmp.Diff(secondFrame, string(gotSecond)); diff != "" {
				t.Fatalf("second frame (-want +got):\n%s", diff)
			}

			// Service shutdown via Client.Close still bounds the surviving exchange immediately.
			readErr := make(chan error, 1)
			readWG.Go(func() {
				buf := make([]byte, 64)
				_, err := resp.Body.Read(buf)
				readErr <- err
			})
			synctest.Wait()

			requireImmediateClose(t, client)
			select {
			case err := <-readErr:
				if err == nil {
					t.Fatal("expected stream read error after Client.Close")
				}
			default:
				t.Fatal("Client.Close did not immediately bound surviving service-context stream")
			}
		})
	})

	t.Run("error: Send passing caller context aborts active stream promptly on cancellation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			const firstFrame = "data: first\n\n"
			ln := newPipeListener("127.0.0.1:12354")
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, firstFrame)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
					select {
					case <-release:
					case <-r.Context().Done():
					}
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			var readWG sync.WaitGroup
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				readWG.Wait()
				<-serveDone
				synctest.Wait()
			}()

			callerCtx, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()

			req := upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    "http://127.0.0.1:12354",
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
			}
			var send upstream.Send = func(ctx context.Context) (*upstream.Response, error) {
				return client.Do(ctx, req)
			}

			resp, err := send(callerCtx)
			if err != nil {
				t.Fatalf("Send returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			gotFirst := make([]byte, len(firstFrame))
			if _, err := io.ReadFull(resp.Body, gotFirst); err != nil {
				t.Fatalf("reading first frame: %v", err)
			}
			if diff := gocmp.Diff(firstFrame, string(gotFirst)); diff != "" {
				t.Fatalf("first frame (-want +got):\n%s", diff)
			}

			readErr := make(chan error, 1)
			readWG.Go(func() {
				buf := make([]byte, 64)
				_, err := resp.Body.Read(buf)
				readErr <- err
			})
			synctest.Wait()

			beforeCancel := time.Now()
			cancelCaller()
			synctest.Wait()
			if !time.Now().Equal(beforeCancel) {
				t.Fatalf("caller cancel advanced virtual clock by %v", time.Since(beforeCancel))
			}
			select {
			case err := <-readErr:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("expected context.Canceled on stream read, got %v", err)
				}
			default:
				t.Fatal("canceling caller context did not immediately abort active stream read")
			}
		})
	})
}
