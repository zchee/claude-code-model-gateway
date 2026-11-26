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
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// lostWorkerBody is the answer to a request whose worker took the body and then gave no usable response.
const lostWorkerBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway lost the shim worker connection after this request reached the model; that answer cannot be recovered, and retrying automatically would pay for a second inference, so send it again yourself if you still want one"}}`

// scriptedWorker answers every connection with the same response bytes after reading the request head, and counts
// the connections it accepted.
type scriptedWorker struct {
	port  int
	mu    sync.Mutex
	conns int
}

func startScriptedWorker(t *testing.T, response string) *scriptedWorker {
	t.Helper()
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	var cfg net.ListenConfig
	ln, err := env.Listen(t.Context(), "tcp4", "127.0.0.1:0", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	w := &scriptedWorker{port: ln.Addr().(*net.TCPAddr).Port}
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			w.mu.Lock()
			w.conns++
			w.mu.Unlock()
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(hangGuard))
				var head []byte
				buf := make([]byte, 4096)
				for !bytes.Contains(head, []byte("\r\n\r\n")) {
					n, err := conn.Read(buf)
					head = append(head, buf[:n]...)
					if err != nil {
						return
					}
				}
				_, _ = io.WriteString(conn, response)
			})
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return w
}

func (w *scriptedWorker) accepted() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conns
}

// relayAnswer is what the client of the public listener sees, plus how many connections the worker accepted.
type relayAnswer struct {
	Status      int
	NoRetry     bool
	ContentType string
	WorkerFinal string
	Body        string
	WorkerConns int
}

// TestRelayInterimResponses checks the relay's handling of interim responses from the worker. A worker that sends
// more interim responses than a client accepts before its final answer (net/http's transport accepts five), or that
// answers with a status below 100, gives no usable response: the client receives the same answer as for a worker
// that switches protocols, the non-retryable 503 for a request whose body the worker took.
func TestRelayInterimResponses(t *testing.T) {
	const final = "HTTP/1.1 200 OK\r\nX-Worker-Final: yes\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	const hint = "HTTP/1.1 103 Early Hints\r\nLink: </hint>; rel=preload\r\n\r\n"
	lost := relayAnswer{Status: http.StatusServiceUnavailable, NoRetry: true, ContentType: "application/json", Body: lostWorkerBody, WorkerConns: 1}
	tests := map[string]struct {
		response string
		want     relayAnswer
	}{
		"success: worker switching protocols gets the lost-worker answer": {
			response: "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n",
			want:     lost,
		},
		"error: six interim responses before the final answer": {
			response: strings.Repeat(hint, 6) + final,
			want:     lost,
		},
		"error: status below 100 before the final answer": {
			// The interim answer carries Content-Length: 0 so that a client that skips it does not take the final answer for
			// its body.
			response: "HTTP/1.1 099 Below Range\r\nContent-Length: 0\r\n\r\n" + final,
			want:     lost,
		},
		"success: five interim responses before the final answer are relayed": {
			response: strings.Repeat(hint, 5) + final,
			want:     relayAnswer{Status: http.StatusOK, WorkerFinal: "yes", Body: "ok", WorkerConns: 1},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// A worker without a usable answer is retried for about four seconds; the cases need not wait in turn.
			t.Parallel()
			worker := startScriptedWorker(t, tc.response)
			public, _ := servePublic(t, worker.port)
			ctx, cancel := context.WithTimeout(t.Context(), hangGuard)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+public+"/v1/models", nil)
			if err != nil {
				t.Fatal(err)
			}
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
			got := relayAnswer{
				Status: resp.StatusCode, NoRetry: resp.Header.Get("X-Should-Retry") == "false", ContentType: resp.Header.Get("Content-Type"),
				WorkerFinal: resp.Header.Get("X-Worker-Final"), Body: string(body), WorkerConns: worker.accepted(),
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("client-visible answer (-want +got):\n%s", diff)
			}
		})
	}
}
