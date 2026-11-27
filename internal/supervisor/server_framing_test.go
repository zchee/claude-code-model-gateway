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
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// framedAnswer is what the public listener sent for one request, read raw until the connection ended.
type framedAnswer struct {
	Status           int
	ContentLength    []string
	TransferEncoding []string
	// Rest is every byte after the head's blank line, as it arrived.
	Rest string
}

// serveFramingHandler serves handler through supervisor.Serve on a loopback listener of its own and returns the
// listener's authority.
func serveFramingHandler(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, ln, handler) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// exchangeFramed writes one request with the given request line protocol that asks the listener to close the
// connection after its answer, reads everything until the connection ends, and reads the head's field lines as
// written, so a Transfer-Encoding field on the wire is seen rather than consumed for framing.
func exchangeFramed(t *testing.T, authority, method, proto string) framedAnswer {
	t.Helper()
	conn, br := dialPublic(t, authority)
	if _, err := io.WriteString(conn, method+" /framing "+proto+"\r\nHost: "+authority+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	wire, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read the answer until the connection ended: %v (read %q)", err, wire)
	}
	head, rest, ok := bytes.Cut(wire, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("the connection ended without a complete response head: %q", wire)
	}
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(head, "\r\n\r\n"...))))
	statusLine, err := tp.ReadLine()
	if err != nil {
		t.Fatalf("read the status line of %q: %v", head, err)
	}
	_, afterProto, _ := strings.Cut(statusLine, " ")
	code, _, _ := strings.Cut(afterProto, " ")
	status, err := strconv.Atoi(code)
	if err != nil {
		t.Fatalf("status line %q has no numeric status", statusLine)
	}
	fields, err := tp.ReadMIMEHeader()
	if err != nil {
		t.Fatalf("read the field lines of %q: %v", head, err)
	}
	return framedAnswer{Status: status, ContentLength: fields.Values("Content-Length"), TransferEncoding: fields.Values("Transfer-Encoding"), Rest: string(rest)}
}

// TestServeDropsHandlerTransferEncoding checks that the public listener frames every response itself and never sends
// a Transfer-Encoding field a handler set: the handler's field is dropped beside a Content-Length the handler
// declared, on a response the handler finished without a body, on a 204, on an answer to HEAD and on an HTTP/1.0
// answer, whose body ends with the connection. The control is an HTTP/1.1 answer of unknown length, which carries the
// listener's own chunked framing once, in place of the handler's value.
func TestServeDropsHandlerTransferEncoding(t *testing.T) {
	tests := map[string]struct {
		method, proto string
		handler       http.HandlerFunc
		want          framedAnswer
	}{
		"success: a handler's Transfer-Encoding beside its Content-Length is dropped": {
			method: http.MethodGet, proto: "HTTP/1.1",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Transfer-Encoding", "gzip, chunked")
				w.Header().Set("Content-Length", "5")
				_, _ = io.WriteString(w, "hello")
			},
			want: framedAnswer{Status: http.StatusOK, ContentLength: []string{"5"}, Rest: "hello"},
		},
		"success: a handler's Transfer-Encoding on a response finished without a body is dropped": {
			method: http.MethodGet, proto: "HTTP/1.1",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Transfer-Encoding", "chunked")
			},
			want: framedAnswer{Status: http.StatusOK, ContentLength: []string{"0"}},
		},
		"success: a handler's Transfer-Encoding on a 204 is dropped": {
			method: http.MethodGet, proto: "HTTP/1.1",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Transfer-Encoding", "chunked")
				w.WriteHeader(http.StatusNoContent)
			},
			want: framedAnswer{Status: http.StatusNoContent},
		},
		"success: a handler's Transfer-Encoding on an answer to HEAD is dropped": {
			method: http.MethodHead, proto: "HTTP/1.1",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Transfer-Encoding", "chunked")
			},
			want: framedAnswer{Status: http.StatusOK},
		},
		"success: a handler's Transfer-Encoding on an HTTP/1.0 answer is dropped and the connection ends the body": {
			method: http.MethodGet, proto: "HTTP/1.0",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Transfer-Encoding", "chunked")
				_, _ = io.WriteString(w, "hello")
			},
			want: framedAnswer{Status: http.StatusOK, Rest: "hello"},
		},
		"success: an HTTP/1.1 answer of unknown length carries the listener's chunked framing once": {
			method: http.MethodGet, proto: "HTTP/1.1",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Transfer-Encoding", "gzip")
				_, _ = io.WriteString(w, "hello")
			},
			want: framedAnswer{Status: http.StatusOK, TransferEncoding: []string{"chunked"}, Rest: "5\r\nhello\r\n0\r\n\r\n"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			authority := serveFramingHandler(t, tt.handler)
			if diff := gocmp.Diff(tt.want, exchangeFramed(t, authority, tt.method, tt.proto)); diff != "" {
				t.Errorf("answer on the wire (-want +got):\n%s", diff)
			}
		})
	}
}
