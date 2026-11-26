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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
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
			if diff := gocmp.Diff("gzip", resp.RelayHeader.Get("Content-Encoding")); diff != "" {
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
	var decoyHits atomic.Int32
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		decoyHits.Add(1)
		http.Error(w, "decoy proxy must never be dialed", http.StatusBadGateway)
	}))
	t.Cleanup(decoy.Close)

	target, captured := startCaptureServer(t, http.StatusOK, `{"direct":true}`)

	t.Setenv("HTTP_PROXY", decoy.URL)
	t.Setenv("HTTPS_PROXY", decoy.URL)
	t.Setenv("http_proxy", decoy.URL)
	t.Setenv("https_proxy", decoy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })

	resp, err := client.Do(t.Context(), upstream.Request{
		Target:     upstream.TargetAnthropic,
		BaseURL:    target.URL,
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
	if decoyHits.Load() != 0 {
		t.Fatalf("env proxy received %d hits, want 0", decoyHits.Load())
	}
	if len(captured()) != 1 {
		t.Fatalf("target received %d requests, want 1", len(captured()))
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
			_ = resp.Body.Close()
			if err != nil {
				t.Fatalf("reading response body: %v", err)
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

	t.Run("success: default pool caps concurrent connections at 64 per host", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			const totalRequests = upstream.DefaultMaxConnsPerHost + 1

			ln := newPipeListener("127.0.0.1:12345")
			release := make(chan struct{})
			var active, maxActive, total atomic.Int32

			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					cur := active.Add(1)
					for {
						prev := maxActive.Load()
						if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
							break
						}
					}
					total.Add(1)
					defer active.Add(-1)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "ok")
				}),
			}
			serveDone := make(chan struct{})
			go func() {
				defer close(serveDone)
				_ = srv.Serve(ln)
			}()

			client := upstream.NewClient(upstream.Options{DialContext: ln.DialContext})
			released := false
			releaseAll := func() {
				if !released {
					released = true
					close(release)
				}
			}
			defer func() {
				releaseAll()
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				<-serveDone
				synctest.Wait()
			}()

			errs := make([]error, totalRequests)
			var wg sync.WaitGroup
			for i := range totalRequests {
				wg.Go(func() {
					resp, err := client.Do(t.Context(), upstream.Request{
						Target:     upstream.TargetAnthropic,
						BaseURL:    "http://127.0.0.1:12345",
						Method:     http.MethodPost,
						RequestURI: "/v1/messages",
						Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
					})
					if err != nil {
						errs[i] = err
						return
					}
					_, readErr := io.ReadAll(resp.Body)
					closeErr := resp.Body.Close()
					errs[i] = errors.Join(readErr, closeErr)
				})
			}

			synctest.Wait()
			gotActive := active.Load()
			releaseAll()
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("request %d failed: %v", i, err)
				}
			}
			if diff := gocmp.Diff(int32(upstream.DefaultMaxConnsPerHost), gotActive); diff != "" {
				t.Fatalf("active concurrent connections while blocked (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(int32(upstream.DefaultMaxConnsPerHost), maxActive.Load()); diff != "" {
				t.Fatalf("maximum concurrent connections (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(int32(totalRequests), total.Load()); diff != "" {
				t.Fatalf("total completed requests (-want +got):\n%s", diff)
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
			var probeAttempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				if r.URL.Path == "/warmup" {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "warm")
					return
				}
				probeAttempts.Add(1)
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
			_, _ = io.ReadAll(warm.Body)
			_ = warm.Body.Close()

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
			if diff := gocmp.Diff(int32(1), probeAttempts.Load()); diff != "" {
				t.Fatalf("upstream received replayed request (-want +got):\n%s", diff)
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
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				<-serveDone
				synctest.Wait()
			}()

			type outcome struct {
				resp *upstream.Response
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				resp, err := client.Do(t.Context(), upstream.Request{
					Target:     upstream.TargetAnthropic,
					BaseURL:    "http://127.0.0.1:12346",
					Method:     http.MethodPost,
					RequestURI: "/v1/messages",
					Body:       []byte(`{"model":"claude-sonnet-4-6"}`),
				})
				done <- outcome{resp: resp, err: err}
			}()

			select {
			case <-entered:
			case res := <-done:
				if res.resp != nil && res.resp.Body != nil {
					_ = res.resp.Body.Close()
				}
				t.Fatalf("Do returned before server received request: err=%v", res.err)
			}

			time.Sleep(upstream.DefaultIdleTimeout - time.Second)
			synctest.Wait()
			select {
			case res := <-done:
				if res.resp != nil && res.resp.Body != nil {
					_ = res.resp.Body.Close()
				}
				t.Fatalf("Do timed out before 1 hour elapsed: err=%v", res.err)
			default:
			}

			time.Sleep(2 * time.Second)
			synctest.Wait()
			res := <-done
			if res.resp != nil && res.resp.Body != nil {
				_ = res.resp.Body.Close()
			}
			if !errors.Is(res.err, upstream.ErrTimeout) {
				t.Fatalf("expected ErrTimeout after 1 hour silence, got %v", res.err)
			}
		})
	})

	t.Run("error: 1 hour silence mid-stream times out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ln := newPipeListener("127.0.0.1:12347")
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
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
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
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

			first := make([]byte, 64)
			n, err := resp.Body.Read(first)
			if err != nil || n == 0 {
				t.Fatalf("initial stream read = (%d, %v), want data", n, err)
			}

			readErr := make(chan error, 1)
			go func() {
				buf := make([]byte, 64)
				_, err := resp.Body.Read(buf)
				readErr <- err
			}()

			time.Sleep(upstream.DefaultIdleTimeout - time.Second)
			synctest.Wait()
			select {
			case err := <-readErr:
				t.Fatalf("stream read timed out before 1 hour elapsed: err=%v", err)
			default:
			}

			time.Sleep(2 * time.Second)
			synctest.Wait()
			if err := <-readErr; !errors.Is(err, upstream.ErrTimeout) {
				t.Fatalf("expected ErrTimeout after 1 hour mid-stream silence, got %v", err)
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
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				<-serveDone
				synctest.Wait()
			}()

			done := make(chan error, 1)
			go func() {
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
			}()

			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("Do returned before server received request: %v", err)
			}

			if err := client.Close(); err != nil {
				t.Fatalf("Client.Close returned error: %v", err)
			}
			synctest.Wait()
			if err := <-done; err == nil {
				t.Fatal("expected error when Client.Close aborted in-flight request")
			}
		})
	})

	t.Run("error: Client.Close aborts active response stream", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ln := newPipeListener("127.0.0.1:12350")
			release := make(chan struct{})

			srv := &http.Server{
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "data: first\n\n")
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
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
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

			buf := make([]byte, 64)
			if n, err := resp.Body.Read(buf); err != nil || n == 0 {
				t.Fatalf("first read = (%d, %v), want data", n, err)
			}

			readErr := make(chan error, 1)
			go func() {
				_, err := resp.Body.Read(buf)
				readErr <- err
			}()
			synctest.Wait()

			if err := client.Close(); err != nil {
				t.Fatalf("Client.Close returned error: %v", err)
			}
			synctest.Wait()
			if err := <-readErr; err == nil {
				t.Fatal("expected stream read error after Client.Close")
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
			defer func() {
				close(release)
				_ = client.Close()
				_ = srv.Close()
				_ = ln.Close()
				<-serveDone
				synctest.Wait()
			}()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)
			go func() {
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
			}()

			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("Do returned before server received request: %v", err)
			}

			cancel()
			synctest.Wait()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context.Canceled, got %v", err)
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

func TestLiveUpstreamHeadersToUpstreamState(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		headers http.Header
		wantExp string
	}{
		"success: repeated Retry-After uses first occurrence": {
			headers: http.Header{
				"X-Openai-Request-Id": {"req_1"},
				"Retry-After":         {"30", "120"},
			},
			wantExp: "2026-09-28T10:00:30.000Z",
		},
		"success: IMF-fixdate Retry-After parses exact instant": {
			headers: http.Header{
				"X-Openai-Request-Id": {"req_2"},
				"Retry-After":         {"Mon, 28 Sep 2026 10:05:00 GMT"},
			},
			wantExp: "2026-09-28T10:05:00.000Z",
		},
		"success: single reset header sets epoch expiry": {
			headers: http.Header{
				"X-Openai-Request-Id":               {"req_3"},
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700"},
			},
			wantExp: "2026-09-28T12:15:00.000Z",
		},
		"success: duplicate reset headers stay intact and join to default 60s": {
			headers: http.Header{
				"X-Openai-Request-Id":               {"req_4"},
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700", "1790597715"},
			},
			wantExp: "2026-09-28T10:01:00.000Z",
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
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			if diff := gocmp.Diff(http.StatusTooManyRequests, resp.StatusCode); diff != "" {
				t.Fatalf("status (-want +got):\n%s", diff)
			}

			// RelayHeader strips Keep-Alive but keeps non-stripped headers like Upgrade.
			if vals := resp.RelayHeader.Values("Keep-Alive"); len(vals) != 0 {
				t.Errorf("RelayHeader kept Keep-Alive: %v", vals)
			}
			if diff := gocmp.Diff("websocket", resp.RelayHeader.Get("Upgrade")); diff != "" {
				t.Errorf("RelayHeader Upgrade (-want +got):\n%s", diff)
			}

			// Raw Header passed directly to upstreamstate must preserve exact wire occurrences.
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
