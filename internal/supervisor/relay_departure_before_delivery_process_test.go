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
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// fenceRequest is the test's own request to the late worker, sent once the handler has returned. It is told apart
// from a relayed request by its method and target.
const fenceRequest = "GET /fence HTTP/1.1\r\nHost: fence\r\nConnection: close\r\n\r\n"

// workerConn is what one connection accepted by the late worker carried: the method, target and body of the request
// read from it, or, when no complete request was read, whether any byte arrived at all. A connection that was closed
// without a byte is the zero value. The relay writes its own Host field and Connection: close on the worker leg, so
// the head as a whole is not the client's; the body is compared byte for byte.
type workerConn struct {
	Method, Target, Body string
	IncompleteBytes      bool
}

// acceptedConn is one connection of the late worker; got is final once done is closed.
type acceptedConn struct {
	done chan struct{}
	got  workerConn
}

// lateWorker is a loopback worker that the relay learns about only if the test publishes its port. Its single accept
// loop registers every connection before it accepts the next one, and it answers every request it reads to the end,
// so a relay that reaches it always gets a final answer and its handler can return.
type lateWorker struct {
	port  int
	mu    sync.Mutex
	conns []*acceptedConn
}

// countingReader counts the bytes read from one worker connection.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func startLateWorker(t *testing.T, answer string) *lateWorker {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := &lateWorker{port: ln.Addr().(*net.TCPAddr).Port}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			rec := &acceptedConn{done: make(chan struct{})}
			w.mu.Lock()
			w.conns = append(w.conns, rec)
			w.mu.Unlock()
			wg.Go(func() {
				defer close(rec.done)
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(hangGuard))
				counted := &countingReader{r: conn}
				req, err := http.ReadRequest(bufio.NewReader(counted))
				if err != nil {
					rec.got.IncompleteBytes = counted.n > 0
					return
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					rec.got.IncompleteBytes = true
					return
				}
				rec.got = workerConn{Method: req.Method, Target: req.RequestURI, Body: string(body)}
				_, _ = io.WriteString(conn, answer)
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return w
}

// fence returns what every connection the worker accepted carried, the test's own fence connection last, once each
// of them has been read to its end. It is called after the handler has returned, so every connection the relay
// completed was dialled before the fence connection. The fence connection is answered only after the worker accepted
// and read it, and loopback connections reach the single accept loop in the order they were dialled, so by then the
// worker has registered every earlier connection, including one the relay closed without sending a byte.
func (w *lateWorker) fence(t *testing.T) []workerConn {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(w.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(hangGuard)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, fenceRequest); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("the worker did not answer the fence request: %v", err)
	}
	_ = resp.Body.Close()
	w.mu.Lock()
	conns := slices.Clone(w.conns)
	w.mu.Unlock()
	got := make([]workerConn, 0, len(conns))
	for i, c := range conns {
		await(t, c.done, "the end of the worker's read of connection "+strconv.Itoa(i))
		got = append(got, c.got)
	}
	return got
}

// observedHandler is the supervisor's handler with what the test needs to see of it: the request context, whether
// that context had ended when the handler returned, and whether the handler returned normally.
type observedHandler struct {
	inner       http.Handler
	ctx         chan context.Context
	returned    chan struct{}
	errAtReturn error
	completed   bool
}

func (h *observedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.ctx <- r.Context()
	// The deferred function also runs when the handler is left by a panic, which leaves completed false. The context's
	// error is read before the server cancels the context itself after the handler, so a non-nil error here means the
	// context ended while the relay was still running.
	defer func() {
		h.errAtReturn = r.Context().Err()
		close(h.returned)
	}()
	h.inner.ServeHTTP(w, r)
	h.completed = true
}

// departureOutcome is what the test observed of one request.
type departureOutcome struct {
	// HandlerCompleted reports that the handler returned normally rather than by a panic.
	HandlerCompleted bool
	// ContextEndedBeforeReturn reports that the request context ended while the handler was still running.
	ContextEndedBeforeReturn bool
	// BeforeLastAttempt reports that the handler returned before the relay's loop reached its last port lookup.
	BeforeLastAttempt bool
	// WorkerConnections lists every connection the late worker accepted, the test's fence connection last.
	WorkerConnections []workerConn
	WroteToClient     bool
	ClientStatus      int
	ClientBody        string
}

// TestRelayDepartureBeforeDelivery decides what the relay does with a request whose client leaves while the relay is
// waiting for a worker port. The client sends a complete request while no worker port is published, so the relay
// keeps looking the port up; the client then closes its connection. The relay ends its wait on that departure,
// before its attempts run out, and delivers nothing to a worker published after it: no worker connection, no request
// byte, no inference bought for a client that can no longer receive the answer, and nothing written back. The
// source, which watches the client only until the upload ends, would deliver the buffered request to that worker.
// The success row runs the same sequence with a client that stays: the worker published later receives the request
// once, with the client's body byte for byte, and the client gets the worker's answer, which shows that the fixture
// can observe a delivery.
//
// The relay's loop asks for the worker port once per attempt, and it makes one attempt plus 80 waits of 50
// milliseconds (relayAttempts and relayWait in supervisor.go), so 81 lookups mean the loop ran out. That count is a
// property of this implementation's loop; here it tells a relay that stopped on the departure from one that ran out.
//
// No row passes by timing. Nothing sleeps, and the hang guard only fails the test. The relay is held inside its
// second port lookup, after it has read port 0, until the test has closed the client and seen the request context
// end, or, in the success row, has published the worker; every later step waits for the handler's return. So the
// departure always comes while the relay is in its wait, however the goroutines are scheduled.
//   - With no worker ever published, the departure is the only thing that can end the wait before the last attempt:
//     a relay whose wait ignored it would run every attempt and return at 81 lookups, and the row fails.
//   - With a worker published after the departure, a relay that went on looking up the port and dialled with a live
//     context would reach the worker at its next lookup; the handler can return only after that dial, and the fence
//     connection, which the test sends after the return, is read only after every earlier connection was
//     registered, so the worker's list holds the relay's connection, with or without request bytes, and the row
//     fails. This row alone does not decide what ends the wait: a relay that ignored the departure in its wait but
//     dialled with the ended context would be refused by the dialer and pass it, which is why the first row exists.
func TestRelayDepartureBeforeDelivery(t *testing.T) {
	const relayLookups = 1 + 80
	const workerAnswer = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	fence := workerConn{Method: http.MethodGet, Target: "/fence"}
	tests := map[string]struct {
		disconnect, publish bool
		want                departureOutcome
	}{
		"error: the client leaves during the wait and no worker port is ever published": {
			disconnect: true,
			want: departureOutcome{
				HandlerCompleted: true, ContextEndedBeforeReturn: true, BeforeLastAttempt: true,
				WorkerConnections: []workerConn{fence},
			},
		},
		"error: the client leaves during the wait and the worker published afterwards receives nothing": {
			disconnect: true, publish: true,
			want: departureOutcome{
				HandlerCompleted: true, ContextEndedBeforeReturn: true, BeforeLastAttempt: true,
				WorkerConnections: []workerConn{fence},
			},
		},
		"success: the client stays and the worker published during the wait receives the request once": {
			publish: true,
			want: departureOutcome{
				HandlerCompleted: true, BeforeLastAttempt: true,
				WorkerConnections: []workerConn{
					{Method: http.MethodPost, Target: "/v1/messages", Body: relayedPayload},
					fence,
				},
				WroteToClient: true, ClientStatus: http.StatusOK, ClientBody: "ok",
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			worker := startLateWorker(t, workerAnswer)
			var lookups, published atomic.Int32
			held := make(chan struct{})
			releaseCh := make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseCh) })
			lookup := func() int {
				port := int(published.Load())
				if lookups.Add(1) == 2 {
					close(held)
					<-releaseCh
				}
				return port
			}

			var cfg net.ListenConfig
			ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			inner, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, lookup, strings.Repeat("3e", 32), "1.2.3", 41)
			if err != nil {
				_ = ln.Close()
				t.Fatal(err)
			}
			handler := &observedHandler{inner: inner, ctx: make(chan context.Context, 1), returned: make(chan struct{})}
			accepted := make(chan *writeTapConn, 1)
			serveCtx, stopServe := context.WithCancel(context.WithoutCancel(t.Context()))
			served := make(chan error, 1)
			go func() { served <- supervisor.Serve(serveCtx, writeTapListener{Listener: ln, conns: accepted}, handler) }()
			t.Cleanup(func() {
				stopServe()
				if err := <-served; err != nil {
					t.Errorf("public listener: %v", err)
				}
				_ = ln.Close()
			})
			// Registered after the listener's cleanup, so it runs first: a relay still held in its lookup when the
			// test fails is let go before Serve waits for it.
			t.Cleanup(release)
			public := ln.Addr().String()

			var dialer net.Dialer
			conn, err := dialer.DialContext(t.Context(), "tcp4", public)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(time.Now().Add(hangGuard)); err != nil {
				t.Fatal(err)
			}
			request := "POST /v1/messages HTTP/1.1\r\nHost: " + public + "\r\nContent-Type: application/json\r\nContent-Length: " +
				strconv.Itoa(len(relayedPayload)) + "\r\n\r\n" + relayedPayload
			if _, err := io.WriteString(conn, request); err != nil {
				t.Fatal(err)
			}
			var server *writeTapConn
			select {
			case server = <-accepted:
			case <-time.After(hangGuard):
				t.Fatalf("the public listener accepted no connection within %v", hangGuard)
			}
			await(t, held, "the relay's second port lookup")
			// The handler sent its context before the relay started, and the relay is now held in its lookup.
			reqCtx := <-handler.ctx

			var got departureOutcome
			if tc.disconnect {
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
				await(t, reqCtx.Done(), "the end of the request context after the client left")
			}
			if tc.publish {
				published.Store(int32(worker.port))
			}
			release()
			if !tc.disconnect {
				resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
				if err != nil {
					t.Fatalf("reading the relayed answer: %v", err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil {
					t.Fatalf("reading the relayed answer's body: %v", err)
				}
				got.ClientStatus, got.ClientBody = resp.StatusCode, string(body)
			}
			await(t, handler.returned, "the handler's return")
			if tc.disconnect {
				await(t, server.closed, "the server's close of the departed client's connection")
			}
			n := lookups.Load()
			got.HandlerCompleted = handler.completed
			got.ContextEndedBeforeReturn = handler.errAtReturn != nil
			got.BeforeLastAttempt = n < relayLookups
			got.WorkerConnections = worker.fence(t)
			got.WroteToClient = server.written() > 0
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("what the relay did with the request (-want +got):\n%s\nport lookups: %d of %d; bytes offered to the client: %d",
					diff, n, relayLookups, server.written())
			}
		})
	}
}
