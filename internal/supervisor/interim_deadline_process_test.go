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
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// closingWriteBound is how long the public listener lets an interim 100 Continue, a refusal or a closing flush take to
// reach a client that does not read, before it gives up on the connection.
const closingWriteBound = 5 * time.Second

// deadlineSlack is how far before the bound a recorded deadline may end: the server reads the clock before the
// recording connection does, and a loaded venue can deschedule it in between. A deadline never ends after the bound.
const deadlineSlack = time.Second

// recordListener hands the public listener connections that record what the server does to their write side.
type recordListener struct {
	net.Listener
	stallInterim bool
	conns        chan *recordConn
}

func (l recordListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	c := &recordConn{Conn: conn, stallInterim: l.stallInterim, changed: make(chan struct{}), closed: make(chan struct{})}
	select {
	case l.conns <- c:
	default:
	}
	return c, nil
}

// recordConn records, in order, each write deadline the server installs or clears, and each response head it writes,
// with the deadline in force and the result of the write. With stallInterim, the interim 100 Continue can never be
// written: like a client whose receive window is full, the write waits until the write deadline in force has passed
// and then fails with the deadline error; without a deadline it waits until the connection is closed. Every other
// write goes to the network unchanged.
type recordConn struct {
	net.Conn
	stallInterim bool
	mu           sync.Mutex
	deadline     time.Time
	// changed is closed and replaced whenever the write deadline changes.
	changed   chan struct{}
	events    []string
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *recordConn) record(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *recordConn) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

func (c *recordConn) setWriteDeadline(t time.Time) {
	now := time.Now()
	event := "clear the write deadline"
	if !t.IsZero() {
		ahead := t.Sub(now)
		event = fmt.Sprintf("install a write deadline %v ahead", ahead)
		if ahead <= closingWriteBound && ahead > closingWriteBound-deadlineSlack {
			event = "install a write deadline at the closing write bound"
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	close(c.changed)
	c.changed = make(chan struct{})
	c.events = append(c.events, event)
}

func (c *recordConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *recordConn) SetDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetDeadline(t)
}

func (c *recordConn) Write(b []byte) (int, error) {
	if !bytes.HasPrefix(b, []byte("HTTP/1.1 ")) {
		return c.Conn.Write(b)
	}
	line, _, _ := bytes.Cut(b, []byte("\r\n"))
	c.mu.Lock()
	inForce := "no write deadline"
	if !c.deadline.IsZero() {
		inForce = "a write deadline"
	}
	c.mu.Unlock()
	var n int
	var err error
	if c.stallInterim && bytes.HasPrefix(b, []byte("HTTP/1.1 100 ")) {
		err = c.stall()
	} else {
		n, err = c.Conn.Write(b)
	}
	result := "written"
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		result = "failed when the deadline passed"
	case err != nil:
		result = "failed: " + err.Error()
	}
	c.record(fmt.Sprintf("write %q under %s: %s", line, inForce, result))
	return n, err
}

// stall waits until the write deadline in force has passed, following every change to it, or until the connection
// closes.
func (c *recordConn) stall() error {
	for {
		c.mu.Lock()
		deadline, changed := c.deadline, c.changed
		c.mu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			wait := time.Until(deadline)
			if wait <= 0 {
				return &net.OpError{Op: "write", Net: "tcp", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: os.ErrDeadlineExceeded}
			}
			timer = time.NewTimer(wait)
			expired = timer.C
		}
		select {
		case <-expired:
		case <-changed:
		case <-c.closed:
			return net.ErrClosed
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (c *recordConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// CloseWrite keeps the half-close the server uses before it closes a connection.
func (c *recordConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// serveRecorded runs supervisor.Serve in front of the worker port on connections that record their write side, and
// returns the public address and the first connection it accepts.
func serveRecorded(t *testing.T, workerPort int, stallInterim bool) (string, <-chan *recordConn) {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, func() int { return workerPort }, strings.Repeat("5e", 32), "1.2.3", 37)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	conns := make(chan *recordConn, 1)
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() {
		served <- supervisor.Serve(ctx, recordListener{Listener: ln, stallInterim: stallInterim, conns: conns}, h)
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	})
	return ln.Addr().String(), conns
}

// accepted returns the connection the public listener accepted, failing the test when there is none within the hang
// guard.
func accepted(t *testing.T, conns <-chan *recordConn) *recordConn {
	t.Helper()
	select {
	case c := <-conns:
		return c
	case <-time.After(hangGuard):
		t.Fatalf("the public listener accepted no connection within %v", hangGuard)
	}
	return nil
}

// awaitClosed waits until the server closed the connection, failing the test when the hang guard passes first.
func awaitClosed(t *testing.T, c *recordConn) {
	t.Helper()
	select {
	case <-c.closed:
	case <-time.After(hangGuard):
		t.Fatalf("the server did not close the connection within %v; recorded: %q", hangGuard, c.recorded())
	}
}

// TestInterimResponseWriteDeadline checks the bound on writing an interim 100 Continue, as the server sets it on the
// connection. Before the interim response it installs a write deadline at the closing write bound; a client that never
// accepts the interim response makes that write fail with the deadline error, the server clears the deadline and
// closes the connection, and the request, whose body never arrived, never reaches the worker. The bound covers that
// one write only: the deadline is cleared once the interim response is out, so the final response is written without
// one. Elapsed time is not measured; the waits are hang guards.
func TestInterimResponseWriteDeadline(t *testing.T) {
	tests := map[string]struct {
		run func(t *testing.T)
	}{
		"error: interim response a client never accepts ends the connection at the write bound": {func(t *testing.T) {
			worker := startScriptedWorker(t, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			public, conns := serveRecorded(t, worker.port, true)
			var dialer net.Dialer
			client, err := dialer.DialContext(t.Context(), "tcp4", public)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			// The client asks for the interim response and withholds the body until it arrives, which it never does.
			head := "POST /continue HTTP/1.1\r\nHost: " + public + "\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\n"
			if _, err := io.WriteString(client, head); err != nil {
				t.Fatal(err)
			}
			server := accepted(t, conns)
			awaitClosed(t, server)
			want := []string{
				"install a write deadline at the closing write bound",
				`write "HTTP/1.1 100 Continue" under a write deadline: failed when the deadline passed`,
				"clear the write deadline",
			}
			if diff := gocmp.Diff(want, server.recorded()); diff != "" {
				t.Errorf("write side of the connection (-want +got):\n%s", diff)
			}
			if got := worker.accepted(); got != 0 {
				t.Errorf("the worker accepted %d connection(s) for a request whose body never arrived; want 0", got)
			}
		}},
		"success: final response after the interim response is written without a write deadline": {func(t *testing.T) {
			worker := startHoldingWorker(t)
			public, conns := serveRecorded(t, worker.port, false)
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
			server := accepted(t, conns)
			br := bufio.NewReader(client)
			interim, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("read the interim response: %v", err)
			}
			_ = interim.Body.Close()
			if diff := gocmp.Diff(http.StatusContinue, interim.StatusCode); diff != "" {
				t.Fatalf("interim status mismatch (-want +got):\n%s", diff)
			}
			if _, err := io.WriteString(client, "{}"); err != nil {
				t.Fatal(err)
			}
			await(t, worker.arrived, "the relayed request's arrival at the worker")
			worker.release()
			type answer struct {
				Status int
				Echo   string
			}
			resp, err := http.ReadResponse(br, nil)
			if err != nil {
				t.Fatalf("read the final response: %v", err)
			}
			_ = resp.Body.Close()
			if diff := gocmp.Diff(answer{Status: http.StatusTeapot, Echo: heldEcho}, answer{Status: resp.StatusCode, Echo: resp.Header.Get("X-Probe-Echo")}); diff != "" {
				t.Errorf("final response mismatch (-want +got):\n%s", diff)
			}
			_ = client.Close()
			awaitClosed(t, server)
			// What the server does to close the connection after the final response is not part of this check.
			want := []string{
				"install a write deadline at the closing write bound",
				`write "HTTP/1.1 100 Continue" under a write deadline: written`,
				"clear the write deadline",
				`write "HTTP/1.1 418 I'm a teapot" under no write deadline: written`,
			}
			got := server.recorded()
			if diff := gocmp.Diff(want, got[:min(len(got), len(want))]); diff != "" {
				t.Errorf("write side of the connection up to the final response (-want +got):\n%s\nall recorded: %q", diff, got)
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The first case waits out the interim write bound; the cases need not wait in turn.
			t.Parallel()
			tc.run(t)
		})
	}
}
