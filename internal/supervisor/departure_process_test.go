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

//go:build process

package supervisor_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// heldEcho marks the answer of a holding worker, so the client can tell it from an answer the listener made up.
const heldEcho = "held-by-worker"

// hangGuard bounds a wait for a server that neither answers nor closes; it is never a pacing delay.
const hangGuard = 30 * time.Second

// holdingWorker is a loopback worker that keeps each relayed request open. It signals when a request has arrived and
// answers only when the test releases it; it signals instead when the supervisor abandons the request by closing the
// relay connection, which ends the request's context on the worker.
type holdingWorker struct {
	port      int
	arrived   chan struct{}
	abandoned chan struct{}
	release   func()
}

func startHoldingWorker(t *testing.T) *holdingWorker {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	var cfg net.ListenConfig
	ln, err := env.Listen(t.Context(), "tcp4", "127.0.0.1:0", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	w := &holdingWorker{
		port:      ln.Addr().(*net.TCPAddr).Port,
		arrived:   make(chan struct{}, 1),
		abandoned: make(chan struct{}, 1),
		release:   sync.OnceFunc(func() { close(released) }),
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case w.arrived <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			select {
			case w.abandoned <- struct{}{}:
			default:
			}
			return
		case <-released:
		}
		rw.Header().Set("X-Probe-Echo", heldEcho)
		rw.WriteHeader(http.StatusTeapot)
	})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		w.release()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(errors.Join(err, srv.Close()))
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return w
}

// await waits for a signal of the holding worker, failing the test when the hang guard passes first.
func await(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	timer := time.NewTimer(hangGuard)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("%s did not happen within %v", what, hangGuard)
	}
}

// writeTapListener hands the public listener connections that count every byte the server tries to write to the
// client and signal when the server closes them.
type writeTapListener struct {
	net.Listener
	conns chan *writeTapConn
}

func (l writeTapListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	c := &writeTapConn{Conn: conn, closed: make(chan struct{})}
	c.closeOnce = sync.OnceFunc(func() { close(c.closed) })
	select {
	case l.conns <- c:
	default:
	}
	return c, nil
}

type writeTapConn struct {
	net.Conn
	mu        sync.Mutex
	attempted int
	closed    chan struct{}
	closeOnce func()
}

// Write counts the bytes offered, whether or not the kernel still accepts them from a connection the peer reset.
func (c *writeTapConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.attempted += len(b)
	c.mu.Unlock()
	return c.Conn.Write(b)
}

func (c *writeTapConn) Close() error {
	c.closeOnce()
	return c.Conn.Close()
}

// CloseWrite keeps the half-close the server uses before it closes a connection.
func (c *writeTapConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *writeTapConn) written() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempted
}

// servePublic runs supervisor.Serve on a loopback listener in front of the worker port and returns its address and
// the connections it accepts, counted by writeTapListener.
func servePublic(t *testing.T, workerPort int) (string, <-chan *writeTapConn) {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, func() int { return workerPort }, strings.Repeat("7c", 32), "1.2.3", 31)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	conns := make(chan *writeTapConn, 4)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, writeTapListener{Listener: ln, conns: conns}, h) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	})
	return ln.Addr().String(), conns
}

// TestHalfCloseDuringRelay sends one complete request and half-closes the connection while the worker holds the
// relayed request. The client is entitled to the worker's own answer, or to a connection that ends without a single
// response byte; a response the listener makes up, such as a success for a request the worker never answered, is
// a fault.
func TestHalfCloseDuringRelay(t *testing.T) {
	// halfOpenWait bounds how long the test waits for the supervisor to abandon the relay after the half-close. It
	// matters only to a listener that keeps relaying for a half-closed client: the worker is then released and answers.
	// A listener that treats the half-close as a departure abandons the relay within milliseconds.
	const halfOpenWait = 2 * time.Second
	tests := map[string]struct {
		request string
	}{
		"error: half-close after a body framed by Content-Length": {
			request: "POST /v1/messages HTTP/1.1\r\nHost: {own}\r\nContent-Length: 2\r\n\r\n{}",
		},
		"error: half-close after a chunked body and its trailer": {
			request: "POST /v1/messages HTTP/1.1\r\nHost: {own}\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\nX-Trailer: end\r\n\r\n",
		},
		"error: half-close after a request without a body": {
			request: "GET /v1/models HTTP/1.1\r\nHost: {own}\r\n\r\n",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			worker := startHoldingWorker(t)
			public, _ := servePublic(t, worker.port)
			var dialer net.Dialer
			conn, err := dialer.DialContext(t.Context(), "tcp4", public)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(hangGuard)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(conn, strings.ReplaceAll(tc.request, "{own}", public)); err != nil {
				t.Fatal(err)
			}
			await(t, worker.arrived, "the relay of the complete request to the worker")
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			wait := time.NewTimer(halfOpenWait)
			select {
			case <-worker.abandoned:
			case <-wait.C:
				worker.release()
			}
			wait.Stop()
			data, err := io.ReadAll(conn)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Fatalf("the connection neither answered nor ended within %v (%d bytes so far)", hangGuard, len(data))
			}
			if len(data) == 0 {
				return
			}
			resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(data)), nil)
			if err != nil {
				t.Fatalf("the connection carried %d bytes that are not a response: %v", len(data), err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusTeapot || resp.Header.Get("X-Probe-Echo") != heldEcho {
				t.Errorf("answered %q, which the worker never gave; want the worker's answer or a connection that ends without response bytes", resp.Status)
			}
		})
	}
}
