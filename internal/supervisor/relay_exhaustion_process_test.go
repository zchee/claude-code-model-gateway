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
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// undeliveredBody is the answer to a request that no worker took: a resend is free, so it carries no retry veto.
const undeliveredBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway could not deliver this request to the shim worker; retry it shortly"}}`

// relayedPayload is the body of the client's request. Every worker that receives the request must receive exactly
// these bytes; an empty or altered body counts as a different body.
const relayedPayload = `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"relay this body unchanged"}]}`

// takingWorker reads each relayed request to the end of its body and records the body it received, and then either
// writes answer or, when answer is empty, closes the connection without a response. Its listener stays open until the
// test ends, so a later dial to its port reaches it and is recorded.
type takingWorker struct {
	port     int
	mu       sync.Mutex
	received []string
}

func startTakingWorker(t *testing.T, answer string) *takingWorker {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := &takingWorker{port: ln.Addr().(*net.TCPAddr).Port}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(hangGuard))
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return
				}
				w.mu.Lock()
				w.received = append(w.received, string(body))
				w.mu.Unlock()
				if answer != "" {
					_, _ = io.WriteString(conn, answer)
				}
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return w
}

// bodies returns the request bodies the worker received, in arrival order, or nil when it received none.
func (w *takingWorker) bodies() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.received)
}

// refusingPort returns a loopback port that refuses connections. The port is the local end of an open client
// connection to a listener of the test's own, held until the test ends: both Linux and macOS refuse a connection
// attempt to that port (ECONNREFUSED). A socket that is only bound to a port is not used: Linux refuses a connection
// to it, but macOS leaves the attempt unanswered, so the dial would hang instead of failing.
//
// While the connection is held, a bind to port 0 is never given this port (Linux 6.12: inet_bind_conflict in
// net/ipv4/inet_connection_sock.c; XNU: in_pcbbind in bsd/netinet/in_pcb.c, which takes only an unused port for a
// port-0 bind). Every listener in this package's tests is bound to port 0 and the relay under test opens none. An
// explicit bind of the number is not excluded on macOS: Go sets SO_REUSEADDR on every listener, and macOS lets such a
// bind take a port held by a connection, where Linux refuses it. The procinfo tests, which can run at the same time,
// bind numbers drawn at random from 20000 to 60000, so on macOS they could take this port over by chance. The dial
// check before the port is returned proves that the port refuses at that moment only.
//
// What a run proves therefore depends on the system. On Linux the refusal holds for the whole case, so a Linux run
// decides the rows that use this port. On macOS a procinfo listener that holds the port when the relay dials it
// completes the relay's connection and closes it without an HTTP answer, because neither procinfo test serves its
// connections; the relay counts that port as one whose worker took the body and failed. In the row "error: the worker
// took the body and another port that refuses is published afterwards" that leads to the expected non-retryable 503
// whether or not any dial to the port was refused, so a green macOS run of that row is not independent proof that
// the relay keeps the first worker's port after a refused dial. In the row "error: no worker took the body because
// the port refuses from the start" the same takeover can only make the row fail.
func refusingPort(t *testing.T) int {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var dialer net.Dialer
	held, err := dialer.DialContext(t.Context(), "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	peer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	port := held.LocalAddr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	probe, err := dialer.DialContext(ctx, "tcp4", held.LocalAddr().String())
	if err == nil {
		_ = probe.Close()
		t.Fatalf("port %d accepted a connection; want it refused", port)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dialling port %d: %v; want ECONNREFUSED", port, err)
	}
	return port
}

// exhaustionAnswer is what the client sees, with the X-Should-Retry values exactly as sent (nil when the field is
// absent), the number of times the relay asked for the worker port, and the request bodies each worker received.
type exhaustionAnswer struct {
	Status            int
	ShouldRetry       []string
	ContentType       string
	Body              string
	PortLookups       int32
	TakerBodies       []string
	ReplacementBodies []string
}

// TestRelayExhaustedAfterBodyTaken pins the relay's answer once a worker accepted the request body and then closed
// the connection without a response. The relay remembers one port, the most recent one whose worker accepted the
// connection and then failed, overwriting it when a worker on another port accepts and fails, and it does not dial
// that port while it is the remembered one. Here the worker that took the body keeps listening on its port, so a
// second leg to it would reach it and be recorded. When no worker on another port answers before the attempts run
// out, the client receives the non-retryable 503, whether the remembered port is still published and accepting, no
// port is published any more, or another port is published that refuses. A request that no worker ever took gets the
// retryable 503 without the X-Should-Retry field, and a worker on another port that answers before the attempts run
// out receives the same body a second time and answers the request.
//
// The relay's attempts are fixed: one attempt plus 80 waits of 50 milliseconds (relayAttempts and relayWait in
// supervisor.go). Its loop asks for the worker port once per attempt, also in the attempts that do not dial because
// no port is published or the port is the remembered one, so a request that exhausts the attempts makes 81 lookups.
// That number is a property of this implementation's loop, not a contract of the relay; it shows here that the relay
// gave up only after its last attempt.
func TestRelayExhaustedAfterBodyTaken(t *testing.T) {
	const relayLookups = 1 + 80
	const replacementAnswer = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	// The port the source names on its first lookup and on every later one.
	const (
		taker       = "the worker that takes the body"
		replacement = "a worker on another port that answers"
		refused     = "another port that refuses connections"
		none        = "no port"
	)
	tookOnce := []string{relayedPayload}
	tests := map[string]struct {
		first, later string
		want         exhaustionAnswer
	}{
		"error: the worker took the body and its port stays published and keeps accepting": {
			first: taker, later: taker,
			want: exhaustionAnswer{
				Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json",
				Body: lostWorkerBody, PortLookups: relayLookups, TakerBodies: tookOnce,
			},
		},
		"error: the worker took the body and no worker port is published afterwards": {
			first: taker, later: none,
			want: exhaustionAnswer{
				Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json",
				Body: lostWorkerBody, PortLookups: relayLookups, TakerBodies: tookOnce,
			},
		},
		// A Linux run decides this row. On macOS another test's listener can hold the refusing port when the relay
		// dials it, and then the row passes whether or not a dial was refused (see refusingPort), so a green macOS run
		// does not prove the refusal case.
		"error: the worker took the body and another port that refuses is published afterwards": {
			first: taker, later: refused,
			want: exhaustionAnswer{
				Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json",
				Body: lostWorkerBody, PortLookups: relayLookups, TakerBodies: tookOnce,
			},
		},
		// A Linux run decides this row too. On macOS a listener that holds the refusing port when the relay dials it
		// makes the row fail, never pass.
		"error: no worker took the body because the port refuses from the start": {
			first: refused, later: refused,
			want: exhaustionAnswer{
				Status: http.StatusServiceUnavailable, ContentType: "application/json",
				Body: undeliveredBody, PortLookups: relayLookups,
			},
		},
		"success: a worker on another port takes the body again and answers in time": {
			first: taker, later: replacement,
			want: exhaustionAnswer{
				Status: http.StatusOK, ContentType: "text/plain", Body: "ok", PortLookups: 2,
				TakerBodies: tookOnce, ReplacementBodies: tookOnce,
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The relay's attempts last about four seconds; the cases need not wait in turn.
			t.Parallel()
			takingOne := startTakingWorker(t, "")
			answering := startTakingWorker(t, replacementAnswer)
			ports := map[string]int{taker: takingOne.port, replacement: answering.port, refused: refusingPort(t), none: 0}
			var lookups atomic.Int32
			public := servePublicFrom(t, func() int {
				if lookups.Add(1) == 1 {
					return ports[tc.first]
				}
				return ports[tc.later]
			})
			ctx, cancel := context.WithTimeout(t.Context(), hangGuard)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/v1/messages", strings.NewReader(relayedPayload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			transport := &http.Transport{DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			got := exhaustionAnswer{
				Status: resp.StatusCode, ShouldRetry: resp.Header.Values("X-Should-Retry"), ContentType: resp.Header.Get("Content-Type"),
				Body: string(body), PortLookups: lookups.Load(), TakerBodies: takingOne.bodies(), ReplacementBodies: answering.bodies(),
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("client-visible answer, relay attempts and bodies received (-want +got):\n%s", diff)
			}
		})
	}
}
