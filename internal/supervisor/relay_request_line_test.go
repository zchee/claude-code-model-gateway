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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// lineAnswer is what one request produced: the status, the Content-Length and Connection fields of the response head
// exactly as written (nil when absent), the body length, whether the connection was closed after the response, how
// often the handler was invoked, how often the relay asked for the worker port and how many requests the worker
// received in full.
type lineAnswer struct {
	Status        int
	ContentLength []string
	Connection    []string
	BodyLen       int
	Closed        bool
	HandlerCalls  int32
	PortLookups   int32
	WorkerLegs    int
}

// serveRequestLine serves the supervisor handler through supervisor.Serve on a loopback listener of its own, wrapped
// so that its invocations are counted, with a worker port source that counts its calls. The relay asks that source
// before every dial, so a zero count means the relay dialled no worker, even one that would not have completed the
// leg. It returns the listener's authority, which is also the Host the handler allows, the counted handler, its
// invocation count and the count of port lookups.
func serveRequestLine(t *testing.T, workerPort int) (string, http.Handler, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lookups := new(atomic.Int32)
	portSource := func() int {
		lookups.Add(1)
		return workerPort
	}
	h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, portSource, strings.Repeat("3c", 32), "1.2.3", 41)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h.ServeHTTP(w, r)
	})
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, ln, counted) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	})
	return ln.Addr().String(), counted, calls, lookups
}

// readWireHead reads one response head from br without net/http's response parser, which drops a Connection: close
// field, and then the body its Content-Length frames. The head must carry exactly the Content-Length values
// wantLength holds, a single decimal number, and that is checked before any body byte is allocated or read.
func readWireHead(t *testing.T, br *bufio.Reader, wantLength []string) (int, http.Header, int) {
	t.Helper()
	tp := textproto.NewReader(br)
	statusLine, err := tp.ReadLine()
	if err != nil {
		t.Fatalf("reading the status line: %v", err)
	}
	proto, rest, _ := strings.Cut(statusLine, " ")
	code, _, _ := strings.Cut(rest, " ")
	status, err := strconv.Atoi(code)
	if proto != "HTTP/1.1" || err != nil {
		t.Fatalf("status line %q", statusLine)
	}
	mime, err := tp.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("reading the response head: %v", err)
	}
	header := http.Header(mime)
	if diff := gocmp.Diff(wantLength, header.Values("Content-Length")); diff != "" {
		t.Fatalf("Content-Length of the response head %v (-want +got):\n%s", header, diff)
	}
	if len(wantLength) != 1 {
		t.Fatalf("a case reading from the wire must expect one Content-Length value, not %q", wantLength)
	}
	length, err := strconv.ParseUint(wantLength[0], 10, 16)
	if err != nil {
		t.Fatalf("expected Content-Length %q is not a small decimal number: %v", wantLength[0], err)
	}
	if _, err := io.ReadFull(br, make([]byte, length)); err != nil {
		t.Fatalf("reading the response body: %v", err)
	}
	return status, header, int(length)
}

// endedAfterResponse reports whether the server ended the connection right after the response just read: one more
// read must return no byte and io.EOF. Any byte, such as the start of a second response cut short, or any other
// error, such as a reset or the hang guard's timeout, is logged and reported as not ended.
func endedAfterResponse(t *testing.T, br *bufio.Reader) bool {
	t.Helper()
	n, err := br.Read(make([]byte, 1))
	if n == 0 && errors.Is(err, io.EOF) {
		return true
	}
	t.Logf("after the response the connection gave %d byte(s) and %v; want 0 bytes and io.EOF", n, err)
	return false
}

// connectionEnded reports whether err is how a closed connection shows up to a client that writes or reads on it.
func connectionEnded(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

// probeClosed sends /restart without the control token on conn and reports whether the connection had been closed:
// the probe cannot be written or its answer cannot be read. A connection that is still open answers the probe with
// the handler's 403, which needs no worker.
func probeClosed(t *testing.T, conn net.Conn, br *bufio.Reader, authority string) bool {
	t.Helper()
	if _, err := io.WriteString(conn, "GET /restart HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
		if connectionEnded(err) {
			return true
		}
		t.Fatalf("writing the probe: %v", err)
	}
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		if connectionEnded(err) {
			return true
		}
		t.Fatalf("reading the probe's answer: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("probe on an open connection answered %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	return false
}

// TestRelayRequestLineBytes decides, for the raw request lines sent here, none of them a CONNECT, whether a line whose
// method or target holds a byte the relay refuses to write to the worker (a control byte, a space or DEL) reaches the
// handler through supervisor.Serve, the supervisor's public listener. For each of these lines the listener refuses
// the line with its own 400, a Content-Length: 0 and Connection: close head, before the handler runs: the handler is
// not invoked, the relay never asks for the worker port and the worker receives nothing. It then ends the connection,
// which shows as io.EOF on the next read. A CONNECT line takes another path in the listener, which closes it without
// an answer, and is not covered. For these inputs the handler's own 400 is not reachable through the listener.
//
// Two rows are controls. The ordinary request line reaches the worker and leaves the connection open, which a second
// request on it decides: /restart without the control token, which the handler refuses with 403 without contacting
// the worker. The row that calls the handler directly does not go through the listener and says nothing about it; it
// pins the handler's own 400 for a target holding DEL.
func TestRelayRequestLineBytes(t *testing.T) {
	parserRefusal := lineAnswer{
		Status: http.StatusBadRequest, ContentLength: []string{"0"}, Connection: []string{"close"}, Closed: true,
	}
	tests := map[string]struct {
		method, target string
		// direct calls the counted handler with a request built here instead of sending the request line to the
		// listener, for an entry point the listener does not produce for these bytes.
		direct bool
		want   lineAnswer
	}{
		"error: target holding DEL": {
			method: http.MethodGet, target: "/v1/mod\x7fels", want: parserRefusal,
		},
		"error: target holding a horizontal tab": {
			method: http.MethodGet, target: "/v1/mod\tels", want: parserRefusal,
		},
		"error: target holding a raw 0x01": {
			method: http.MethodGet, target: "/v1/mod\x01els", want: parserRefusal,
		},
		"error: target holding a space": {
			method: http.MethodGet, target: "/v1/mod els", want: parserRefusal,
		},
		"error: method holding DEL": {
			method: "GE\x7fT", target: "/v1/models", want: parserRefusal,
		},
		"success: ordinary request line reaches the worker": {
			method: http.MethodGet, target: "/v1/models",
			// The worker answers with its request line, GET /v1/models HTTP/1.1, and its Connection: close does not
			// reach the client. The handler runs twice, for the request and for the probe; the probe is refused
			// before the relay, so the port is looked up once.
			want: lineAnswer{Status: http.StatusOK, ContentLength: []string{"23"}, BodyLen: 23, HandlerCalls: 2, PortLookups: 1, WorkerLegs: 1},
		},
		"error: handler called directly with a target holding DEL": {
			method: http.MethodGet, target: "/v1/mod\x7fels", direct: true,
			want: lineAnswer{Status: http.StatusBadRequest, HandlerCalls: 1},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			worker := startRawLegWorker(t)
			authority, handler, calls, lookups := serveRequestLine(t, worker.port)
			if tc.direct {
				// The control outside the listener: the handler's own refusal, before any port lookup.
				rec := httptest.NewRecorder()
				req := (&http.Request{
					Method: tc.method, URL: &url.URL{Path: "/v1/models"}, RequestURI: tc.target, Proto: "HTTP/1.1",
					ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header), Host: authority, Body: http.NoBody,
					RemoteAddr: "127.0.0.1:1",
				}).WithContext(t.Context())
				handler.ServeHTTP(rec, req)
				got := lineAnswer{
					Status: rec.Code, ContentLength: rec.Header().Values("Content-Length"), Connection: rec.Header().Values("Connection"),
					BodyLen: rec.Body.Len(), HandlerCalls: calls.Load(), PortLookups: lookups.Load(), WorkerLegs: len(worker.recorded()),
				}
				if diff := gocmp.Diff(tc.want, got); diff != "" {
					t.Errorf("request %q %q called directly (-want +got):\n%s", tc.method, tc.target, diff)
				}
				return
			}
			conn, br := dialPublic(t, authority)
			if _, err := io.WriteString(conn, tc.method+" "+tc.target+" HTTP/1.1\r\nHost: "+authority+"\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			status, header, length := readWireHead(t, br, tc.want.ContentLength)
			got := lineAnswer{
				Status: status, ContentLength: header.Values("Content-Length"), Connection: header.Values("Connection"),
				BodyLen: length,
			}
			if tc.want.Closed {
				got.Closed = endedAfterResponse(t, br)
			} else {
				got.Closed = probeClosed(t, conn, br, authority)
			}
			// Read after the connection's end or the probe, so that any contact the request caused is counted.
			got.HandlerCalls, got.PortLookups, got.WorkerLegs = calls.Load(), lookups.Load(), len(worker.recorded())
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("request line %q %q (-want +got):\n%s", tc.method, tc.target, diff)
			}
		})
	}
}
