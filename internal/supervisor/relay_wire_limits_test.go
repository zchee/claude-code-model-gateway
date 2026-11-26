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

package supervisor_test

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// legRecord is one request exactly as the relay wrote it to the worker: the request line, every field line with its
// name lowercased, and the body.
type legRecord struct {
	requestLine string
	fields      map[string][]string
	body        string
}

// rawLegWorker is a loopback worker that reads each relayed request from the connection's bytes, so that fields a
// parsing server would move out of its header map (framing, trailer declarations) are seen as they were written.
// It answers every request with its request line as the body and Connection: close, as the relay asks it to.
type rawLegWorker struct {
	port int
	wg   sync.WaitGroup
	mu   sync.Mutex
	legs []legRecord
}

func startRawLegWorker(t *testing.T) *rawLegWorker {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := &rawLegWorker{port: ln.Addr().(*net.TCPAddr).Port}
	w.wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			w.wg.Go(func() { w.serve(conn) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		w.wg.Wait()
	})
	return w
}

func (w *rawLegWorker) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	tp := textproto.NewReader(br)
	requestLine, err := tp.ReadLine()
	if err != nil {
		return
	}
	rec := legRecord{requestLine: requestLine, fields: make(map[string][]string)}
	length := 0
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		if line == "" {
			break
		}
		name, value, _ := strings.Cut(line, ":")
		name, value = strings.ToLower(name), strings.TrimSpace(value)
		rec.fields[name] = append(rec.fields[name], value)
		if name == "content-length" {
			if length, err = strconv.Atoi(value); err != nil {
				return
			}
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return
	}
	rec.body = string(body)
	w.mu.Lock()
	w.legs = append(w.legs, rec)
	w.mu.Unlock()
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s", len(requestLine), requestLine)
}

func (w *rawLegWorker) recorded() []legRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]legRecord(nil), w.legs...)
}

// startPublicListener serves the supervisor handler on a loopback listener of its own, relaying to workerPort, and
// returns the listener's authority, which is also the Host the handler allows.
func startPublicListener(t *testing.T, workerPort int) string {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, func() int { return workerPort }, strings.Repeat("7e", 32), "1.2.3", 29)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, ln, h) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// dialPublic opens one client connection to the public listener.
func dialPublic(t *testing.T, authority string) (net.Conn, *bufio.Reader) {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp4", authority)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// A hang guard for a server that neither answers nor closes, not a pacing delay.
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn, bufio.NewReader(conn)
}

// finalAnswer is the final response to one request as the client read it.
type finalAnswer struct {
	status int
	// closing reports that the response said Connection: close.
	closing bool
	body    string
}

// readFinal reads the final response to a request with the given method, skipping interim 100 responses.
func readFinal(t *testing.T, br *bufio.Reader, method string) finalAnswer {
	t.Helper()
	for {
		resp, err := http.ReadResponse(br, &http.Request{Method: method})
		if err != nil {
			t.Fatalf("reading the response: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("reading the response body: %v", err)
		}
		if resp.StatusCode == http.StatusContinue {
			continue
		}
		return finalAnswer{status: resp.StatusCode, closing: resp.Close, body: string(body)}
	}
}

// TestRelayWorkerLegFramingAndClientReuse checks the fields of the buffered worker leg on the wire and the client
// connection around it. The relay writes the worker's Host and its own Content-Length for the decoded body, drops the
// client's framing, trailer declaration, Expect and keep-alive fields, and asks the worker to close with
// Connection: close. The worker's Connection: close ends only that leg: an HTTP/1.1 client connection carries the next
// request. An HTTP/1.0 client connection is closed after its response even when the client asked for keep-alive.
func TestRelayWorkerLegFramingAndClientReuse(t *testing.T) {
	tests := map[string]struct {
		// head is the client's request head; {own} is the public listener's authority.
		head, body string
		// wantLine, wantFields and wantBody describe the worker leg; {worker} is the worker's authority.
		wantLine   string
		wantFields map[string][]string
		wantBody   string
		// http10 marks a client request whose connection must close after its response.
		http10 bool
	}{
		"success: chunked body with a trailer declaration and keep-alive fields": {
			head:     "POST /v1/messages?beta=1 HTTP/1.1\r\nHost: {own}\r\nConnection: keep-alive\r\nKeep-Alive: timeout=5\r\nTrailer: X-Sum\r\nTransfer-Encoding: chunked\r\nX-Custom: kept\r\n\r\n",
			body:     "2\r\n{}\r\n3\r\nabc\r\n0\r\nX-Sum: 1\r\n\r\n",
			wantLine: "POST /v1/messages?beta=1 HTTP/1.1",
			wantFields: map[string][]string{
				"host": {"{worker}"}, "x-custom": {"kept"}, "content-length": {"5"}, "connection": {"close"},
			},
			wantBody: "{}abc",
		},
		"success: expect-continue body with a length": {
			head:     "POST /v1/messages HTTP/1.1\r\nHost: {own}\r\nExpect: 100-continue\r\nContent-Length: 2\r\nX-Custom: kept\r\n\r\n",
			body:     "{}",
			wantLine: "POST /v1/messages HTTP/1.1",
			wantFields: map[string][]string{
				"host": {"{worker}"}, "x-custom": {"kept"}, "content-length": {"2"}, "connection": {"close"},
			},
			wantBody: "{}",
		},
		"success: PUT with a keep-alive field": {
			head:     "PUT /v1/messages HTTP/1.1\r\nHost: {own}\r\nKeep-Alive: timeout=5, max=9\r\nContent-Length: 3\r\nX-Custom: kept\r\n\r\n",
			body:     "abc",
			wantLine: "PUT /v1/messages HTTP/1.1",
			wantFields: map[string][]string{
				"host": {"{worker}"}, "x-custom": {"kept"}, "content-length": {"3"}, "connection": {"close"},
			},
			wantBody: "abc",
		},
		"success: bodyless GET": {
			head:     "GET /v1/models HTTP/1.1\r\nHost: {own}\r\nConnection: keep-alive\r\nX-Custom: kept\r\n\r\n",
			wantLine: "GET /v1/models HTTP/1.1",
			wantFields: map[string][]string{
				"host": {"{worker}"}, "x-custom": {"kept"}, "connection": {"close"},
			},
		},
		"success: HTTP/1.0 request asking for keep-alive": {
			head:     "POST /v1/messages HTTP/1.0\r\nHost: {own}\r\nConnection: keep-alive\r\nKeep-Alive: timeout=5\r\nContent-Length: 2\r\nX-Custom: kept\r\n\r\n",
			body:     "{}",
			wantLine: "POST /v1/messages HTTP/1.1",
			wantFields: map[string][]string{
				"host": {"{worker}"}, "x-custom": {"kept"}, "content-length": {"2"}, "connection": {"close"},
			},
			wantBody: "{}",
			http10:   true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			worker := startRawLegWorker(t)
			own := startPublicListener(t, worker.port)
			workerAuthority := net.JoinHostPort("127.0.0.1", strconv.Itoa(worker.port))
			conn, br := dialPublic(t, own)

			if _, err := io.WriteString(conn, strings.ReplaceAll(tt.head, "{own}", own)+tt.body); err != nil {
				t.Fatal(err)
			}
			method, _, _ := strings.Cut(tt.head, " ")
			first := readFinal(t, br, method)
			if diff := gocmp.Diff([]any{http.StatusOK, tt.wantLine, tt.http10}, []any{first.status, first.body, first.closing}); diff != "" {
				t.Errorf("first response: status, relayed request line, Connection: close (-want +got):\n%s", diff)
			}

			wantLegs := []legRecord{{requestLine: tt.wantLine, fields: make(map[string][]string), body: tt.wantBody}}
			for field, values := range tt.wantFields {
				for _, v := range values {
					wantLegs[0].fields[field] = append(wantLegs[0].fields[field], strings.ReplaceAll(v, "{worker}", workerAuthority))
				}
			}
			if tt.http10 {
				if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
					t.Errorf("after the HTTP/1.0 response the connection is not closed: peek error %v, want EOF", err)
				}
			} else {
				// The same client connection carries the next request although the worker closed its leg.
				if _, err := fmt.Fprintf(conn, "GET /again HTTP/1.1\r\nHost: %s\r\n\r\n", own); err != nil {
					t.Fatalf("writing the second request on the same connection: %v", err)
				}
				second := readFinal(t, br, http.MethodGet)
				if diff := gocmp.Diff([]any{http.StatusOK, "GET /again HTTP/1.1", false}, []any{second.status, second.body, second.closing}); diff != "" {
					t.Errorf("second response on the same connection (-want +got):\n%s", diff)
				}
				wantLegs = append(wantLegs, legRecord{
					requestLine: "GET /again HTTP/1.1",
					fields:      map[string][]string{"host": {workerAuthority}, "connection": {"close"}},
				})
			}
			if diff := gocmp.Diff(wantLegs, worker.recorded(), gocmp.AllowUnexported(legRecord{})); diff != "" {
				t.Errorf("worker legs on the wire (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHeadFieldCountBoundary checks that a request head of exactly 2,000 field lines, Host included, is relayed with
// every field, and that one more field line is refused with 431 before the worker is contacted, rather than relayed
// with the excess fields silently dropped. Repeated lines of one name count one by one. The compact heads stay under
// the source's 16 KiB head limit at both counts, so their 431 is a refusal of the count, not of the size.
func TestHeadFieldCountBoundary(t *testing.T) {
	const (
		limit = 2000
		// sourceHeadLimit is the source server's bound on the bytes of a request head.
		sourceHeadLimit = 16 << 10
	)
	tests := map[string]struct {
		lines int
		// repeated writes every filler line under one name; otherwise each line has a name of its own.
		repeated bool
		// compact writes every filler line as the shortest repeated field, "X:y".
		compact bool
		want    int
	}{
		"success: 2,000 distinct field lines":                    {lines: limit, want: http.StatusOK},
		"success: 2,000 lines of a repeated name":                {lines: limit, repeated: true, want: http.StatusOK},
		"error: 2,001 distinct field lines":                      {lines: limit + 1, want: http.StatusRequestHeaderFieldsTooLarge},
		"error: 2,001 lines of a repeated name":                  {lines: limit + 1, repeated: true, want: http.StatusRequestHeaderFieldsTooLarge},
		"success: 2,000 compact lines under the head size limit": {lines: limit, compact: true, want: http.StatusOK},
		"error: 2,001 compact lines under the head size limit":   {lines: limit + 1, compact: true, want: http.StatusRequestHeaderFieldsTooLarge},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			worker := startRawLegWorker(t)
			own := startPublicListener(t, worker.port)
			conn, br := dialPublic(t, own)

			// Host and Content-Length are two of the field lines.
			fillers := tt.lines - 2
			var head strings.Builder
			fmt.Fprintf(&head, "POST /fields HTTP/1.1\r\nHost: %s\r\nContent-Length: 2\r\n", own)
			for i := range fillers {
				switch {
				case tt.compact:
					head.WriteString("X:y\r\n")
				case tt.repeated:
					fmt.Fprintf(&head, "X-Repeat: %04d\r\n", i)
				default:
					fmt.Fprintf(&head, "X-Field-%04d: v\r\n", i)
				}
			}
			head.WriteString("\r\n")
			if tt.compact && head.Len() >= sourceHeadLimit {
				t.Fatalf("test precondition: compact head is %d bytes, want under %d", head.Len(), sourceHeadLimit)
			}
			head.WriteString("{}")
			if _, err := io.WriteString(conn, head.String()); err != nil {
				t.Fatal(err)
			}
			answer := readFinal(t, br, http.MethodPost)
			legs := worker.recorded()
			if tt.want != http.StatusOK {
				if diff := gocmp.Diff([]any{tt.want, true, 0}, []any{answer.status, answer.closing, len(legs)}); diff != "" {
					t.Errorf("status, Connection: close, worker legs (-want +got):\n%s", diff)
				}
				if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
					t.Errorf("after the refusal the connection is not closed: peek error %v, want EOF", err)
				}
				return
			}
			if diff := gocmp.Diff([]any{http.StatusOK, 1}, []any{answer.status, len(legs)}); diff != "" {
				t.Fatalf("status, worker legs (-want +got):\n%s", diff)
			}
			relayed := 0
			for field, values := range legs[0].fields {
				if strings.HasPrefix(field, "x-field-") || field == "x-repeat" || field == "x" {
					relayed += len(values)
				}
			}
			if diff := gocmp.Diff(fillers, relayed); diff != "" {
				t.Errorf("filler field lines the worker received (-want +got):\n%s", diff)
			}
			last := fmt.Sprintf("x-field-%04d", fillers-1)
			want := []string{"v"}
			switch {
			case tt.compact:
				last, want = "x", []string{"y"}
			case tt.repeated:
				last, want = "x-repeat", []string{fmt.Sprintf("%04d", fillers-1)}
			}
			if tt.compact || tt.repeated {
				if values := legs[0].fields[last]; len(values) > 0 {
					legs[0].fields[last] = values[len(values)-1:]
				}
			}
			if diff := gocmp.Diff(want, legs[0].fields[last]); diff != "" {
				t.Errorf("last field line %q at the worker (-want +got):\n%s", last, diff)
			}
		})
	}
}

// healthDocument returns a JSON object of exactly size bytes with one string member.
func healthDocument(size int) string {
	const prefix, suffix = `{"workerField":"`, `"}`
	return prefix + strings.Repeat("w", size-len(prefix)-len(suffix)) + suffix
}

// TestHealthBodyLimit checks the bound on the worker's health document: a document of exactly 1 MiB is relayed with
// the supervisor's fields added, and one byte more is answered with 502 and nothing of the worker's answer, whether
// the worker declares its length or streams it chunked.
func TestHealthBodyLimit(t *testing.T) {
	const limit = 1 << 20
	tests := map[string]struct {
		size     int
		declared bool
		want     int
	}{
		"success: document of exactly the limit with a declared length":  {size: limit, declared: true, want: http.StatusOK},
		"success: chunked document of exactly the limit":                 {size: limit, want: http.StatusOK},
		"error: document one byte over the limit with a declared length": {size: limit + 1, declared: true, want: http.StatusBadGateway},
		"error: chunked document one byte over the limit":                {size: limit + 1, want: http.StatusBadGateway},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			document := healthDocument(tt.size)
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("worker request path %q, want /healthz", r.URL.Path)
				}
				w.Header().Set("X-Worker-Response", "yes")
				w.Header().Set("Content-Type", "application/json")
				if tt.declared {
					w.Header().Set("Content-Length", strconv.Itoa(len(document)))
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, document)
			}))
			t.Cleanup(worker.Close)
			u, err := url.Parse(worker.URL)
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatal(err)
			}
			h, err := supervisor.NewHandler(40002, func() int { return port }, strings.Repeat("ab", 32), "test-version", 7)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
			r.Host = "127.0.0.1:40002"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if tt.want != http.StatusOK {
				got := []any{w.Code, w.Body.Len(), w.Header().Get("X-Worker-Response"), w.Header().Get("Content-Type")}
				if diff := gocmp.Diff([]any{http.StatusBadGateway, 0, "", ""}, got); diff != "" {
					t.Errorf("status, body bytes, worker headers (-want +got):\n%s", diff)
				}
				return
			}
			got := []any{w.Code, w.Header().Get("X-Worker-Response"), w.Header().Get("Content-Length")}
			if diff := gocmp.Diff([]any{http.StatusOK, "yes", strconv.Itoa(w.Body.Len())}, got); diff != "" {
				t.Errorf("status, worker header, Content-Length (-want +got):\n%s", diff)
			}
			var health map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
				t.Fatalf("decode the relayed health document: %v", err)
			}
			field, _ := health["workerField"].(string)
			health["workerField"] = len(field)
			want := map[string]any{"workerField": tt.size - len(`{"workerField":""}`), "supervisorVersion": "test-version", "supervisorPid": float64(7), "proxyRecovery": true}
			if diff := gocmp.Diff(want, health); diff != "" {
				t.Errorf("relayed health document, worker field as its length (-want +got):\n%s", diff)
			}
		})
	}
}
