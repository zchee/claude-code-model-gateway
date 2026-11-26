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
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// interimWriteLimit is how long the public listener lets an interim 100 Continue take to reach a client that does not
// read, before it gives up on the connection.
const interimWriteLimit = 5 * time.Second

// interimSlack is the extra time a loaded venue may need around interimWriteLimit.
const interimSlack = 3 * time.Second

// stallListener hands the public listener connections on which the interim 100 Continue can never be written: like a
// client whose receive window is full, the write waits until the write deadline the server set has passed and then
// fails with the deadline error. Without a deadline it waits until the connection is closed. Every other write goes
// to the network unchanged.
type stallListener struct {
	net.Listener
	conns chan *stallConn
}

func (l stallListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	c := &stallConn{Conn: conn, changed: make(chan struct{}), closed: make(chan struct{}), stalled: make(chan time.Time, 1)}
	select {
	case l.conns <- c:
	default:
	}
	return c, nil
}

type stallConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
	// changed is closed and replaced whenever the write deadline changes.
	changed   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	// stalled receives the time the interim response began to wait.
	stalled chan time.Time
}

func (c *stallConn) setWriteDeadline(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallConn) SetDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetDeadline(t)
}

func (c *stallConn) Write(b []byte) (int, error) {
	if !bytes.HasPrefix(b, []byte("HTTP/1.1 100 ")) {
		return c.Conn.Write(b)
	}
	select {
	case c.stalled <- time.Now():
	default:
	}
	for {
		c.mu.Lock()
		deadline, changed := c.deadline, c.changed
		c.mu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			wait := time.Until(deadline)
			if wait <= 0 {
				return 0, &net.OpError{Op: "write", Net: "tcp", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: os.ErrDeadlineExceeded}
			}
			timer = time.NewTimer(wait)
			expired = timer.C
		}
		select {
		case <-expired:
		case <-changed:
		case <-c.closed:
			return 0, net.ErrClosed
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (c *stallConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// CloseWrite keeps the half-close the server uses before it closes a connection.
func (c *stallConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// TestInterimResponseWriteDeadline checks the bound on writing an interim 100 Continue. A client that never accepts
// the interim response cannot hold the connection open: the write fails at the bound and the connection closes, and
// the request, whose body never arrived, never reaches the worker. The bound covers that one write only: a final
// response written long after it, for a client that does read, still reaches the client.
func TestInterimResponseWriteDeadline(t *testing.T) {
	tests := map[string]struct {
		run func(t *testing.T)
	}{
		"error: interim response a client never accepts ends the connection at the write bound": {func(t *testing.T) {
			worker := startScriptedWorker(t, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			var cfg net.ListenConfig
			ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, func() int { return worker.port }, strings.Repeat("5e", 32), "1.2.3", 37)
			if err != nil {
				_ = ln.Close()
				t.Fatal(err)
			}
			conns := make(chan *stallConn, 1)
			ctx, cancel := context.WithCancel(t.Context())
			served := make(chan error, 1)
			go func() { served <- supervisor.Serve(ctx, stallListener{Listener: ln, conns: conns}, h) }()
			defer func() {
				cancel()
				if err := <-served; err != nil {
					t.Errorf("public listener: %v", err)
				}
				_ = ln.Close()
			}()

			var dialer net.Dialer
			client, err := dialer.DialContext(t.Context(), "tcp4", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			// The client asks for the interim response and withholds the body until it arrives, which it never does.
			head := "POST /continue HTTP/1.1\r\nHost: " + ln.Addr().String() + "\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\n"
			if _, err := io.WriteString(client, head); err != nil {
				t.Fatal(err)
			}
			var server *stallConn
			select {
			case server = <-conns:
			case <-time.After(hangGuard):
				t.Fatalf("the public listener accepted no connection within %v", hangGuard)
			}
			var stalledAt time.Time
			select {
			case stalledAt = <-server.stalled:
			case <-server.closed:
				t.Fatal("the server closed the connection without trying to write the interim response")
			case <-time.After(hangGuard):
				t.Fatalf("the server wrote no interim response within %v of the request head", hangGuard)
			}
			bound := interimWriteLimit + interimSlack
			select {
			case <-server.closed:
				if waited := time.Since(stalledAt); waited < interimWriteLimit-interimSlack {
					t.Errorf("the server gave up on the interim response after %v, well before its bound of %v", waited, interimWriteLimit)
				}
			case <-time.After(time.Until(stalledAt.Add(bound))):
				t.Fatalf("the connection is still open %v after the interim response began to wait; want it closed at the bound of %v", bound, interimWriteLimit)
			}
			if got := worker.accepted(); got != 0 {
				t.Errorf("the worker accepted %d connection(s) for a request whose body never arrived; want 0", got)
			}
		}},
		"success: final response long after the interim response still reaches the client": {func(t *testing.T) {
			worker := startHoldingWorker(t)
			public, _ := servePublic(t, worker.port)
			var dialer net.Dialer
			client, err := dialer.DialContext(t.Context(), "tcp4", public)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			// A hang guard for a server that neither answers nor closes, not a pacing delay.
			if err := client.SetDeadline(time.Now().Add(hangGuard)); err != nil {
				t.Fatal(err)
			}
			head := "POST /continue HTTP/1.1\r\nHost: " + public + "\r\nExpect: 100-continue\r\nContent-Length: 2\r\nConnection: close\r\n\r\n"
			if _, err := io.WriteString(client, head); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(client)
			interim, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("read the interim response: %v", err)
			}
			_ = interim.Body.Close()
			if diff := gocmp.Diff(http.StatusContinue, interim.StatusCode); diff != "" {
				t.Fatalf("interim status mismatch (-want +got):\n%s", diff)
			}
			interimAt := time.Now()
			if _, err := io.WriteString(client, "{}"); err != nil {
				t.Fatal(err)
			}
			await(t, worker.arrived, "the relayed request's arrival at the worker")
			// The worker answers only once a deadline left over from the interim response would have passed.
			hold := time.NewTimer(time.Until(interimAt.Add(interimWriteLimit + time.Second)))
			<-hold.C
			worker.release()
			type answer struct {
				Status int
				Echo   string
			}
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("read the final response written %v after the interim response: %v", time.Since(interimAt), err)
			}
			_ = resp.Body.Close()
			if diff := gocmp.Diff(answer{Status: http.StatusTeapot, Echo: heldEcho}, answer{Status: resp.StatusCode, Echo: resp.Header.Get("X-Probe-Echo")}); diff != "" {
				t.Errorf("final response mismatch (-want +got):\n%s", diff)
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Each case waits out the interim write bound; the cases need not wait in turn.
			t.Parallel()
			tc.run(t)
		})
	}
}
