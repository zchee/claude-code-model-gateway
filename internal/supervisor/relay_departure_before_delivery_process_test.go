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

// deliveredRequest is what a worker read of one relayed request: the request line's method and target and the body
// bytes. The relay writes its own Host field and Connection: close on the worker leg, so the head as a whole is not
// the client's; the body is compared byte for byte.
type deliveredRequest struct {
	Method, Target, Body string
}

// lateWorker is a loopback worker that the relay learns about only once the test publishes its port. It counts every
// connection it accepts and every byte it reads, records each request it reads to the end of its body, and answers
// it, so a relay that reaches it always gets a final answer and its handler can return.
type lateWorker struct {
	port     int
	conns    atomic.Int32
	read     atomic.Int64
	mu       sync.Mutex
	requests []deliveredRequest
}

// countingReader adds every byte read from a worker connection to the worker's total.
type countingReader struct {
	r     io.Reader
	total *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.total.Add(int64(n))
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
			w.conns.Add(1)
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(hangGuard))
				req, err := http.ReadRequest(bufio.NewReader(countingReader{r: conn, total: &w.read}))
				if err != nil {
					return
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return
				}
				w.mu.Lock()
				w.requests = append(w.requests, deliveredRequest{Method: req.Method, Target: req.RequestURI, Body: string(body)})
				w.mu.Unlock()
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

func (w *lateWorker) delivered() []deliveredRequest {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.requests)
}

// observedHandler is the supervisor's handler with what the test needs to see of it: when its request context ended,
// whether that happened before the handler returned, and when it returned.
type observedHandler struct {
	inner       http.Handler
	ctxEnded    chan struct{}
	returned    chan struct{}
	errAtReturn error
}

func (h *observedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	context.AfterFunc(r.Context(), func() { close(h.ctxEnded) })
	// The deferred function also runs when the relay abandons the handler by panicking, so the test sees the return
	// instead of waiting out its hang guard. The context's error is read before the server cancels the context
	// itself after the handler, so a non-nil error here means the context ended while the relay was still running.
	defer func() {
		h.errAtReturn = r.Context().Err()
		close(h.returned)
	}()
	h.inner.ServeHTTP(w, r)
}

// departureOutcome is what the test observed of one request.
type departureOutcome struct {
	// ContextEndedBeforeReturn reports that the request context ended while the handler was still running.
	ContextEndedBeforeReturn bool
	// BeforeLastAttempt reports that the handler returned before the relay's loop reached its last port lookup.
	BeforeLastAttempt bool
	WorkerConns       int32
	WorkerReadBytes   bool
	Delivered         []deliveredRequest
	WroteToClient     bool
	ClientStatus      int
	ClientBody        string
}

// TestRelayDepartureBeforeDelivery decides what the relay does with a request whose client leaves while the relay is
// still waiting for a worker port. The client sends a complete request while no worker port is published, so the
// relay keeps looking the port up; the client then closes its connection. The relay ends its wait on that departure
// and delivers nothing to the worker published after it: no worker connection, no request byte, no inference bought
// for a client that can no longer receive the answer, and nothing written back. The source, which watches the client
// only until the upload ends, would deliver the buffered request to that worker. The success row runs the same
// sequence with a client that stays: the worker published later receives the request once, with the client's body
// byte for byte, and the client gets the worker's answer, which shows that the worker can observe a delivery.
//
// The error row cannot pass by timing. Nothing in it sleeps: each step waits for an event of the relay, and the hang
// guard only fails the test. The worker is published only after the test has seen the request context end, and the
// test then waits until the handler itself has returned, which is the end of the relay's loop however many lookups
// it had left. A relay that went on looking up the port after the departure would find the published worker at its
// next lookup, connect and send the body; it could return only after the worker answered (or after the worker had
// accepted and read the request, if it then failed to pass the answer on), so the worker's counts already hold that
// connection and that body when the outcome is read, and the row fails. A relay that gave up only because its
// attempts ran out would also fail the row: the handler would return with its lookups at the full count.
//
// The relay's loop asks for the worker port once per attempt, and it makes one attempt plus 80 waits of 50
// milliseconds (relayAttempts and relayWait in supervisor.go), so 81 lookups mean the loop ran out. That count is a
// property of this implementation's loop; here it tells a relay that stopped on the departure from one that ran out.
func TestRelayDepartureBeforeDelivery(t *testing.T) {
	const relayLookups = 1 + 80
	const workerAnswer = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	tests := map[string]struct {
		disconnect bool
		want       departureOutcome
	}{
		"error: the client leaves before any worker port is published and the relay delivers to none": {
			disconnect: true,
			want:       departureOutcome{ContextEndedBeforeReturn: true, BeforeLastAttempt: true},
		},
		"success: the client stays and the worker published later receives the request once": {
			want: departureOutcome{
				BeforeLastAttempt: true,
				WorkerConns:       1,
				WorkerReadBytes:   true,
				Delivered:         []deliveredRequest{{Method: http.MethodPost, Target: "/v1/messages", Body: relayedPayload}},
				WroteToClient:     true,
				ClientStatus:      http.StatusOK,
				ClientBody:        "ok",
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			worker := startLateWorker(t, workerAnswer)
			var lookups, published atomic.Int32
			waiting := make(chan struct{})
			markWaiting := sync.OnceFunc(func() { close(waiting) })
			lookup := func() int {
				// The second lookup comes after one wait of the loop, so the relay is in its wait for a worker.
				if lookups.Add(1) >= 2 {
					markWaiting()
				}
				return int(published.Load())
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
			handler := &observedHandler{inner: inner, ctxEnded: make(chan struct{}), returned: make(chan struct{})}
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
			await(t, waiting, "the relay's wait for a worker port")

			var got departureOutcome
			if tc.disconnect {
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
				await(t, handler.ctxEnded, "the end of the request context after the client left")
				published.Store(int32(worker.port))
				await(t, handler.returned, "the handler's return")
				await(t, server.closed, "the server's close of the departed client's connection")
			} else {
				published.Store(int32(worker.port))
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
				await(t, handler.returned, "the handler's return")
			}
			n := lookups.Load()
			got.ContextEndedBeforeReturn = handler.errAtReturn != nil
			got.BeforeLastAttempt = n < relayLookups
			got.WorkerConns = worker.conns.Load()
			got.WorkerReadBytes = worker.read.Load() > 0
			got.Delivered = worker.delivered()
			got.WroteToClient = server.written() > 0
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("what the relay did with the request (-want +got):\n%s\nport lookups: %d of %d; worker bytes read: %d; bytes offered to the client: %d",
					diff, n, relayLookups, worker.read.Load(), server.written())
			}
		})
	}
}
