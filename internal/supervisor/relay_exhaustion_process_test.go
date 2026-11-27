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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// undeliveredBody is the answer to a request that no worker took: a resend is free, so it carries no retry veto.
const undeliveredBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway could not deliver this request to the shim worker; retry it shortly"}}`

// takingWorker reads each relayed request to the end of its body, counts the bodies it received, and then either
// writes answer or, when answer is empty, closes the connection without a response. With closeAfterTake it closes its
// listener before it closes the first connection whose body it took, so its port refuses connections from then on.
type takingWorker struct {
	port  int
	mu    sync.Mutex
	taken int
}

func startTakingWorker(t *testing.T, answer string, closeAfterTake bool) *takingWorker {
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
				if _, err := io.ReadAll(req.Body); err != nil {
					return
				}
				w.mu.Lock()
				w.taken++
				w.mu.Unlock()
				if closeAfterTake {
					_ = ln.Close()
				}
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

func (w *takingWorker) bodies() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.taken
}

// refusedPort returns a loopback port whose listener has been closed, so that a connection to it is refused.
func refusedPort(t *testing.T) int {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
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
	TakerBodies       int
	ReplacementBodies int
}

// TestRelayExhaustedAfterBodyTaken pins the relay's answer once a worker accepted the request body and then closed
// the connection without a response. The relay remembers the port of that worker for the rest of the request and
// never dials it again; when no worker on another port answers before its attempts run out, the client receives the
// non-retryable 503, whether the remembered port refuses connections by then, no port is published any more, or
// another port is published that refuses. A request that no worker ever took gets the retryable 503 without the
// X-Should-Retry field, and a worker on another port that answers before the attempts run out receives the body a
// second time and answers the request.
//
// The relay's attempts are fixed: one attempt plus 80 waits of 50 milliseconds (relayAttempts and relayWait in
// supervisor.go), and the relay asks for the worker port once per attempt. The number of lookups shows that the relay
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
	tests := map[string]struct {
		first, later string
		want         exhaustionAnswer
	}{
		"error: the worker took the body and its port refuses connections afterwards": {
			first: taker, later: taker,
			want: exhaustionAnswer{
				Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json",
				Body: lostWorkerBody, PortLookups: relayLookups, TakerBodies: 1,
			},
		},
		"error: the worker took the body and no worker port is published afterwards": {
			first: taker, later: none,
			want: exhaustionAnswer{
				Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json",
				Body: lostWorkerBody, PortLookups: relayLookups, TakerBodies: 1,
			},
		},
		"error: the worker took the body and another port that refuses is published afterwards": {
			first: taker, later: refused,
			want: exhaustionAnswer{
				Status: http.StatusServiceUnavailable, ShouldRetry: []string{"false"}, ContentType: "application/json",
				Body: lostWorkerBody, PortLookups: relayLookups, TakerBodies: 1,
			},
		},
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
				Status: http.StatusOK, ContentType: "text/plain", Body: "ok", PortLookups: 2, TakerBodies: 1, ReplacementBodies: 1,
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The relay's attempts last about four seconds; the cases need not wait in turn.
			t.Parallel()
			takingOne := startTakingWorker(t, "", true)
			answering := startTakingWorker(t, replacementAnswer, false)
			ports := map[string]int{taker: takingOne.port, replacement: answering.port, refused: refusedPort(t), none: 0}
			var lookups atomic.Int32
			public := servePublicFrom(t, func() int {
				if lookups.Add(1) == 1 {
					return ports[tc.first]
				}
				return ports[tc.later]
			})
			ctx, cancel := context.WithTimeout(t.Context(), hangGuard)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/v1/messages", strings.NewReader(`{"model":"m"}`))
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
