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
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// TestRequestTargetOriginRefusedBeforeDial checks that an Anthropic or proxy send refuses a request target naming a
// scheme, an authority, user info, an opaque reference or a leading "//" before any connection is dialed, and that a
// backslash in a relative target stays part of the path on the configured upstream instead of selecting another host.
func TestRequestTargetOriginRefusedBeforeDial(t *testing.T) {
	srv, captured := startCaptureServer(t, http.StatusOK, `{"ok":true}`)
	upstreamAuthority := srv.Listener.Addr().String()
	var mu sync.Mutex
	var dialed []string
	var dialer net.Dialer
	client := upstream.NewClient(upstream.Options{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		// Every dial reaches the capture server, so a changed host shows in the dialed address and the Host field.
		return dialer.DialContext(ctx, network, upstreamAuthority)
	}})
	t.Cleanup(func() { _ = client.Close() })
	dials := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(dialed)
	}

	tests := map[string]struct {
		target string
		// wantPath is the decoded path the upstream receives; empty means the send is refused.
		wantPath string
	}{
		"error: absolute http target":                {target: "http://evil.example/v1/messages"},
		"error: absolute https target":               {target: "https://evil.example/v1/messages"},
		"error: upper-case scheme":                   {target: "HTTP://evil.example/v1/messages"},
		"error: scheme without an authority":         {target: "http:/v1/messages"},
		"error: network-path reference":              {target: "//evil.example/v1/messages"},
		"error: user info and authority":             {target: "//user:secret@evil.example/v1/messages"},
		"error: user info with an absolute target":   {target: "http://user@evil.example/v1/messages"},
		"error: user info without a host":            {target: "//user@/v1/messages"},
		"error: three leading slashes":               {target: "///evil.example/v1/messages"},
		"error: opaque reference":                    {target: "mailto:evil@evil.example"},
		"error: host and port in the first segment":  {target: "evil.example:443/v1/messages"},
		"success: backslash after the leading slash": {target: `/\evil.example/v1/messages`, wantPath: `/\evil.example/v1/messages`},
		"success: two leading backslashes":           {target: `\\evil.example/v1/messages`, wantPath: `/\\evil.example/v1/messages`},
		"success: backslash then slash":              {target: `\/evil.example/v1/messages`, wantPath: `/\/evil.example/v1/messages`},
		"success: slash, backslash and user info":    {target: `/\user@evil.example/v1/messages`, wantPath: `/\user@evil.example/v1/messages`},
		"success: backslashes inside the path":       {target: `/v1\messages`, wantPath: `/v1\messages`},
	}
	for _, target := range []upstream.Target{upstream.TargetAnthropic, upstream.TargetProxy} {
		for name, tt := range tests {
			t.Run(string(target)+"/"+name, func(t *testing.T) {
				dialsBefore, capturedBefore := len(dials()), len(captured())
				resp, err := client.Do(t.Context(), upstream.Request{
					Target:        target,
					BaseURL:       srv.URL,
					Method:        http.MethodPost,
					RequestURI:    tt.target,
					ClientHeaders: http.Header{"Content-Type": {"application/json"}},
					Body:          []byte(`{"model":"claude-opus-5"}`),
				})
				if tt.wantPath == "" {
					if err == nil {
						_ = resp.Body.Close()
						t.Fatalf("Do(%q) succeeded, want a refusal", tt.target)
					}
					if strings.Contains(err.Error(), "evil.example") {
						t.Errorf("refusal repeats the request target: %v", err)
					}
					if diff := gocmp.Diff([]int{dialsBefore, capturedBefore}, []int{len(dials()), len(captured())}); diff != "" {
						t.Errorf("dials and upstream requests after a refusal (-want +got):\n%s", diff)
					}
					return
				}
				if err != nil {
					t.Fatalf("Do(%q): %v", tt.target, err)
				}
				_, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					t.Fatalf("reading the response: %v", readErr)
				}
				for _, addr := range dials()[dialsBefore:] {
					if addr != upstreamAuthority {
						t.Errorf("dialed %q, want the configured upstream %q", addr, upstreamAuthority)
					}
				}
				got := captured()[capturedBefore:]
				if len(got) != 1 {
					t.Fatalf("upstream received %d requests, want 1", len(got))
				}
				path, err := url.PathUnescape(got[0].RequestURI)
				if err != nil {
					t.Fatalf("upstream request target %q: %v", got[0].RequestURI, err)
				}
				if diff := gocmp.Diff([]string{upstreamAuthority, tt.wantPath}, []string{got[0].Host, path}); diff != "" {
					t.Errorf("upstream Host and decoded path (-want +got):\n%s", diff)
				}
			})
		}
	}
}

// wireHead is one request head exactly as a client wrote it: the request line, then each field line's lowercase name
// mapped to its values in order.
type wireHead struct {
	line   string
	fields map[string][]string
	body   string
	conn   int
}

// rawHeadServer is a loopback HTTP/1.1 server that reads request heads from the connection's bytes, so that framing
// and trailer fields a parsing server moves out of its header map are seen as they were written. It answers every
// request with a two-byte body and keeps the connection open. A head whose declared length differs from the length of
// the body the test sends is recorded without a body and its connection is closed at once, instead of waiting for
// bytes that never come.
type rawHeadServer struct {
	addr       string
	wantLength int
	wg         sync.WaitGroup
	mu         sync.Mutex
	conns      int
	heads      []wireHead
}

func startRawHeadServer(t *testing.T, wantLength int) *rawHeadServer {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &rawHeadServer{addr: ln.Addr().String(), wantLength: wantLength}
	var open sync.Map
	s.wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			id := s.conns
			s.mu.Unlock()
			open.Store(conn, struct{}{})
			s.wg.Go(func() {
				defer open.Delete(conn)
				s.serve(conn, id)
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		open.Range(func(c, _ any) bool {
			_ = c.(net.Conn).Close()
			return true
		})
		s.wg.Wait()
	})
	return s
}

func (s *rawHeadServer) serve(conn net.Conn, id int) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	tp := textproto.NewReader(br)
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		head := wireHead{line: line, fields: make(map[string][]string), conn: id}
		length := 0
		for {
			field, err := tp.ReadLine()
			if err != nil {
				return
			}
			if field == "" {
				break
			}
			name, value, _ := strings.Cut(field, ":")
			name, value = strings.ToLower(name), strings.TrimSpace(value)
			head.fields[name] = append(head.fields[name], value)
			if name == "content-length" {
				if length, err = strconv.Atoi(value); err != nil {
					return
				}
			}
		}
		if length != s.wantLength {
			s.mu.Lock()
			s.heads = append(s.heads, head)
			s.mu.Unlock()
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		head.body = string(body)
		s.mu.Lock()
		s.heads = append(s.heads, head)
		s.mu.Unlock()
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\n\r\nok"); err != nil {
			return
		}
	}
}

func (s *rawHeadServer) recorded() []wireHead {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.heads)
}

// TestRequestWireFraming checks the outgoing HTTP/1.1 wire of Anthropic and proxy sends whose client headers carry an
// explicitly empty User-Agent and the client's own framing fields. The transport writes its own Content-Length for the
// buffered body, copies neither the incoming Transfer-Encoding nor the Trailer declaration, omits the empty user agent,
// and sends a second request on the same connection without any Connection or Keep-Alive field.
func TestRequestWireFraming(t *testing.T) {
	framing := http.Header{
		"Transfer-Encoding": {"chunked"},
		"Trailer":           {"X-Checksum"},
		"Content-Length":    {"999"},
		"Connection":        {"keep-alive"},
		"Keep-Alive":        {"timeout=5"},
		"Content-Type":      {"application/json"},
	}
	with := func(extra http.Header) http.Header {
		h := framing.Clone()
		for name, values := range extra {
			h[name] = slices.Clone(values)
		}
		return h
	}
	const body = `{"model":"claude-opus-5"}`
	tests := map[string]struct {
		target  upstream.Target
		method  string
		headers http.Header
		body    string
		want    map[string][]string
	}{
		"success: Anthropic POST with an explicitly empty user agent": {
			target: upstream.TargetAnthropic, method: http.MethodPost, headers: with(http.Header{"User-Agent": {""}}), body: body,
			want: map[string][]string{"content-length": {strconv.Itoa(len(body))}, "content-type": {"application/json"}},
		},
		"success: proxy POST with an explicitly empty user agent": {
			target: upstream.TargetProxy, method: http.MethodPost, headers: with(http.Header{"User-Agent": {""}}), body: body,
			want: map[string][]string{"content-length": {strconv.Itoa(len(body))}, "content-type": {"application/json"}},
		},
		"success: empty user agent under a lower-case name": {
			target: upstream.TargetAnthropic, method: http.MethodPost, headers: with(http.Header{"user-agent": {""}}), body: body,
			want: map[string][]string{"content-length": {strconv.Itoa(len(body))}, "content-type": {"application/json"}},
		},
		"success: Anthropic GET without a body": {
			target: upstream.TargetAnthropic, method: http.MethodGet, headers: with(http.Header{"User-Agent": {""}}),
			want: map[string][]string{"content-type": {"application/json"}},
		},
		"success: non-empty user agent is sent as received": {
			target: upstream.TargetAnthropic, method: http.MethodPost, headers: with(http.Header{"User-Agent": {"claude-cli/2.1"}}), body: body,
			want: map[string][]string{"content-length": {strconv.Itoa(len(body))}, "content-type": {"application/json"}, "user-agent": {"claude-cli/2.1"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := startRawHeadServer(t, len(tt.body))
			client := upstream.NewClient(upstream.Options{})
			t.Cleanup(func() { _ = client.Close() })
			for range 2 {
				resp, err := client.Do(t.Context(), upstream.Request{
					Target:        tt.target,
					BaseURL:       "http://" + srv.addr,
					Method:        tt.method,
					RequestURI:    "/v1/messages?beta=true",
					ClientHeaders: tt.headers,
					Body:          []byte(tt.body),
				})
				if err != nil {
					t.Fatalf("Do: %v; request heads on the wire so far: %+v", err, srv.recorded())
				}
				// Reading to the end returns the connection to the pool before the next send.
				_, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					t.Fatalf("reading the response: %v", readErr)
				}
			}
			want := maps.Clone(tt.want)
			want["host"] = []string{srv.addr}
			wantHead := wireHead{line: tt.method + " /v1/messages?beta=true HTTP/1.1", fields: want, body: tt.body, conn: 1}
			if diff := gocmp.Diff([]wireHead{wantHead, wantHead}, srv.recorded(), gocmp.AllowUnexported(wireHead{})); diff != "" {
				t.Errorf("request heads on the wire, both on the first connection (-want +got):\n%s", diff)
			}
		})
	}
}

// startRawResponseServer serves response, written byte for byte, to every request on a loopback connection.
func startRawResponseServer(t *testing.T, response string) string {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var open sync.Map
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			open.Store(conn, struct{}{})
			wg.Go(func() {
				defer open.Delete(conn)
				defer func() { _ = conn.Close() }()
				br := bufio.NewReader(conn)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					if _, err := io.Copy(io.Discard, req.Body); err != nil {
						return
					}
					if _, err := io.WriteString(conn, response); err != nil {
						return
					}
				}
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		open.Range(func(c, _ any) bool {
			_ = c.(net.Conn).Close()
			return true
		})
		wg.Wait()
	})
	return "http://" + ln.Addr().String()
}

// TestRelayTrailerAndPragmaNormalization checks the two header maps of a response whose header the Go parser
// rewrites. For a chunked response the capture header omits the Trailer declaration and the relay header rebuilds it
// from the sorted canonical trailer names, however the upstream spelled and repeated them; neither map receives a
// trailer value. A lone Pragma: no-cache also yields Cache-Control: no-cache in both maps, while an explicit
// Cache-Control is kept as sent.
func TestRelayTrailerAndPragmaNormalization(t *testing.T) {
	const chunkedBody = "5\r\nhello\r\n0\r\nx-zeta: 1\r\nX-alpha: 2\r\nx-MID: 3\r\n\r\n"
	tests := map[string]struct {
		response string
		// wantTrailer is the relay header's Trailer declaration; nil means none.
		wantTrailer []string
		// wantCacheControl is Cache-Control in both maps; nil means none.
		wantCacheControl []string
	}{
		"success: mixed-case trailer names in one declaration": {
			response:    "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTrailer: x-zeta, X-alpha,x-MID\r\nTransfer-Encoding: chunked\r\n\r\n" + chunkedBody,
			wantTrailer: []string{"X-Alpha, X-Mid, X-Zeta"},
		},
		"success: mixed-case trailer names over repeated declarations": {
			response:    "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTrailer: x-zeta\r\nTransfer-Encoding: chunked\r\nTRAILER: X-ALPHA, x-mid\r\ntrailer: x-alpha\r\n\r\n" + chunkedBody,
			wantTrailer: []string{"X-Alpha, X-Mid, X-Zeta"},
		},
		"success: lone Pragma no-cache": {
			response:         "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nPragma: no-cache\r\nContent-Length: 5\r\n\r\nhello",
			wantCacheControl: []string{"no-cache"},
		},
		"success: lone Pragma no-cache on a chunked response with trailers": {
			response:         "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nPragma: no-cache\r\nTrailer: X-Alpha, x-mid, X-ZETA\r\nTransfer-Encoding: chunked\r\n\r\n" + chunkedBody,
			wantTrailer:      []string{"X-Alpha, X-Mid, X-Zeta"},
			wantCacheControl: []string{"no-cache"},
		},
		"success: Pragma no-cache beside an explicit Cache-Control": {
			response:         "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nPragma: no-cache\r\nCache-Control: max-age=5\r\nContent-Length: 5\r\n\r\nhello",
			wantCacheControl: []string{"max-age=5"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			base := startRawResponseServer(t, tt.response)
			client := upstream.NewClient(upstream.Options{})
			t.Cleanup(func() { _ = client.Close() })
			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    base,
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"claude-opus-5"}`),
			})
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("reading the response: %v", readErr)
			}
			if diff := gocmp.Diff("hello", string(body)); diff != "" {
				t.Errorf("body (-want +got):\n%s", diff)
			}
			// Read after the body reached its end, when the trailer values have arrived.
			got := map[string][]any{
				"capture Trailer":       {headerValuesCI(t, resp.Header, "Trailer")},
				"relay Trailer":         {headerValuesCI(t, resp.RelayHeader, "Trailer")},
				"capture Cache-Control": {headerValuesCI(t, resp.Header, "Cache-Control")},
				"relay Cache-Control":   {headerValuesCI(t, resp.RelayHeader, "Cache-Control")},
			}
			want := map[string][]any{
				"capture Trailer":       {[]string(nil)},
				"relay Trailer":         {tt.wantTrailer},
				"capture Cache-Control": {tt.wantCacheControl},
				"relay Cache-Control":   {tt.wantCacheControl},
			}
			for _, trailer := range []string{"X-Alpha", "X-Mid", "X-Zeta"} {
				got["capture "+trailer] = []any{headerValuesCI(t, resp.Header, trailer)}
				got["relay "+trailer] = []any{headerValuesCI(t, resp.RelayHeader, trailer)}
				want["capture "+trailer] = []any{[]string(nil)}
				want["relay "+trailer] = []any{[]string(nil)}
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("capture and relay header fields (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCaptureHeaderLosesFramingFields checks the framing fields the Go response parser removes before the capture
// header is built, which the source's capture still sees: Transfer-Encoding, and on an HTTP/1.1 response a Connection
// field naming the close token. Connection without close, Keep-Alive, and Connection: close on an HTTP/1.0 response
// stay in the capture header. The relay header carries none of the three fields in any case. Which fields the parser
// removes is the Go 1.27.4 net/http implementation, observed behaviour rather than a documented guarantee.
func TestCaptureHeaderLosesFramingFields(t *testing.T) {
	const (
		chunkedHello = "5\r\nhello\r\n0\r\n\r\n"
		lengthHello  = "Content-Length: 5\r\n\r\nhello"
	)
	// framingFields are the response fields the relay header never carries.
	framingFields := []string{"Transfer-Encoding", "Connection", "Keep-Alive"}

	tests := map[string]struct {
		response string
		// wantCapture holds the capture header's value of each framing field; a missing name means
		// the field is absent.
		wantCapture map[string][]string
	}{
		"success: a chunked response's Transfer-Encoding is absent from the capture header": {
			response: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n" + chunkedHello,
		},
		"success: an HTTP/1.1 Connection: close is absent from the capture header": {
			response: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n" + lengthHello,
		},
		"success: an HTTP/1.1 Connection field naming close among other tokens is absent from the capture header": {
			response: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: x-hop, close\r\nX-Hop: 1\r\n" + lengthHello,
		},
		"success: an HTTP/1.1 Connection: keep-alive and its Keep-Alive hint stay in the capture header": {
			response: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: keep-alive\r\nKeep-Alive: timeout=5, max=100\r\n" + lengthHello,
			wantCapture: map[string][]string{
				"Connection": {"keep-alive"},
				"Keep-Alive": {"timeout=5, max=100"},
			},
		},
		"success: an HTTP/1.0 Connection: close stays in the capture header": {
			response:    "HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n" + lengthHello,
			wantCapture: map[string][]string{"Connection": {"close"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			base := startRawResponseServer(t, tt.response)
			client := upstream.NewClient(upstream.Options{})
			t.Cleanup(func() { _ = client.Close() })
			resp, err := client.Do(t.Context(), upstream.Request{
				Target:     upstream.TargetAnthropic,
				BaseURL:    base,
				Method:     http.MethodPost,
				RequestURI: "/v1/messages",
				Body:       []byte(`{"model":"claude-opus-5"}`),
			})
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("reading the response: %v", readErr)
			}
			if diff := gocmp.Diff("hello", string(body)); diff != "" {
				t.Errorf("body (-want +got):\n%s", diff)
			}
			got := map[string][]string{}
			want := map[string][]string{}
			for _, field := range framingFields {
				if v := headerValuesCI(t, resp.Header, field); v != nil {
					got["capture "+field] = v
				}
				if v := headerValuesCI(t, resp.RelayHeader, field); v != nil {
					got["relay "+field] = v
				}
				if v, ok := tt.wantCapture[field]; ok {
					want["capture "+field] = v
				}
			}
			// Content-Type, a field the parser keeps, reaches both maps unchanged.
			for _, header := range []struct {
				view string
				h    http.Header
			}{{"capture", resp.Header}, {"relay", resp.RelayHeader}} {
				got[header.view+" Content-Type"] = headerValuesCI(t, header.h, "Content-Type")
				want[header.view+" Content-Type"] = []string{"text/plain"}
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("framing fields of the capture and relay headers (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDefaultALPNOffer checks the exact ALPN list of every TLS handshake: only http/1.1, whether the caller's TLS
// configuration names no protocols or other ones, for a client send and for a direct round trip, including a request
// asking to upgrade the connection.
func TestDefaultALPNOffer(t *testing.T) {
	tests := map[string]struct {
		// nextProtos is the caller's TLS configuration's protocol list.
		nextProtos []string
		// transport sends through NewTransport's RoundTrip instead of a Client.
		transport bool
		upgrade   bool
	}{
		"success: client send with no protocols configured":               {},
		"success: client send with h2 and http/1.1 configured":            {nextProtos: []string{"h2", "http/1.1"}},
		"success: client send asking for a websocket upgrade":             {upgrade: true},
		"success: transport round trip with no protocols configured":      {transport: true},
		"success: transport round trip with only h2 configured":           {transport: true, nextProtos: []string{"h2"}},
		"success: transport upgrade request with no protocols configured": {transport: true, upgrade: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var offers [][]string
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "ok")
			}))
			srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				mu.Lock()
				offers = append(offers, slices.Clone(hello.SupportedProtos))
				mu.Unlock()
				return nil, nil
			}}
			srv.StartTLS()
			t.Cleanup(srv.Close)
			roots := x509.NewCertPool()
			roots.AddCert(srv.Certificate())
			tlsConfig := &tls.Config{RootCAs: roots, NextProtos: slices.Clone(tt.nextProtos)}

			var body io.ReadCloser
			if tt.transport {
				tr := upstream.NewTransport(upstream.Options{TLSConfig: tlsConfig})
				t.Cleanup(tr.CloseIdleConnections)
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/models", nil)
				if err != nil {
					t.Fatal(err)
				}
				if tt.upgrade {
					req.Header.Set("Connection", "Upgrade")
					req.Header.Set("Upgrade", "websocket")
				}
				resp, err := tr.RoundTrip(req)
				if err != nil {
					t.Fatalf("RoundTrip: %v", err)
				}
				// Closed again below after reading; a second close of a response body is a no-op.
				defer func() { _ = resp.Body.Close() }()
				body = resp.Body
			} else {
				client := upstream.NewClient(upstream.Options{TLSConfig: tlsConfig})
				t.Cleanup(func() { _ = client.Close() })
				headers := http.Header{"Content-Type": {"application/json"}}
				if tt.upgrade {
					headers.Set("Connection", "Upgrade")
					headers.Set("Upgrade", "websocket")
				}
				resp, err := client.Do(t.Context(), upstream.Request{
					Target:        upstream.TargetAnthropic,
					BaseURL:       srv.URL,
					Method:        http.MethodPost,
					RequestURI:    "/v1/messages",
					ClientHeaders: headers,
					Body:          []byte(`{"model":"claude-opus-5"}`),
				})
				if err != nil {
					t.Fatalf("Do: %v", err)
				}
				body = resp.Body
			}
			_, readErr := io.ReadAll(body)
			_ = body.Close()
			if readErr != nil {
				t.Fatalf("reading the response: %v", readErr)
			}
			mu.Lock()
			got := slices.Clone(offers)
			mu.Unlock()
			if diff := gocmp.Diff([][]string{{"http/1.1"}}, got); diff != "" {
				t.Errorf("ALPN offers of the handshakes (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.nextProtos, tlsConfig.NextProtos); diff != "" {
				t.Errorf("caller's TLS configuration changed (-want +got):\n%s", diff)
			}
		})
	}
}
