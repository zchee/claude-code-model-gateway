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
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// servePublicFrom runs supervisor.Serve on a loopback listener whose handler asks workerPort for the worker port at
// each relay attempt, and returns the listener's address.
func servePublicFrom(t *testing.T, workerPort func() int) string {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, err := supervisor.NewHandler(ln.Addr().(*net.TCPAddr).Port, workerPort, strings.Repeat("5d", 32), "1.2.3", 37)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(ctx, ln, h) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("public listener: %v", err)
		}
		_ = ln.Close()
	})
	return ln.Addr().String()
}

// exhaustedAnswer is what the client sees after a worker sent too many interim responses, with the number of times
// the relay asked for the worker port and the connections each worker accepted.
type exhaustedAnswer struct {
	Answer       relayAnswer
	PortLookups  int32
	FloodConns   int
	ReplaceConns int
}

// TestInterimLimitAnsweredAfterRelayAttempts checks when the 503 for a worker that sent more interim responses than
// the relay accepts is the client's answer. That worker took the request body, so the relay never sends it there
// again, but it keeps looking for another worker port for as long as its attempts last: the 503 is the answer only
// once every attempt has passed without one, and a worker that appears on another port in the meantime answers the
// request instead.
func TestInterimLimitAnsweredAfterRelayAttempts(t *testing.T) {
	// relayLookups is one attempt plus the 80 waits of the relay (relayAttempts in supervisor.go); the relay asks for
	// the worker port once per attempt.
	const relayLookups = 1 + 80
	const final = "HTTP/1.1 200 OK\r\nX-Worker-Final: yes\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	flooding := strings.Repeat("HTTP/1.1 103 Early Hints\r\nLink: </hint>; rel=preload\r\n\r\n", 6) + final
	tests := map[string]struct {
		// replace makes the port source name the replacement worker from its second lookup on.
		replace bool
		want    exhaustedAnswer
	}{
		"error: only the flooding worker is answered with 503 after every relay attempt": {
			want: exhaustedAnswer{
				Answer:      relayAnswer{Status: http.StatusServiceUnavailable, NoRetry: true, ContentType: "application/json", Body: lostWorkerBody},
				PortLookups: relayLookups, FloodConns: 1,
			},
		},
		"success: a worker on another port answers after the flooding one": {
			replace: true,
			want: exhaustedAnswer{
				Answer:      relayAnswer{Status: http.StatusOK, WorkerFinal: "yes", Body: "ok"},
				PortLookups: 2, FloodConns: 1, ReplaceConns: 1,
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// The relay's attempts last about four seconds; the cases need not wait in turn.
			t.Parallel()
			flood := startScriptedWorker(t, flooding)
			replacement := startScriptedWorker(t, final)
			var lookups atomic.Int32
			public := servePublicFrom(t, func() int {
				if lookups.Add(1) > 1 && tc.replace {
					return replacement.port
				}
				return flood.port
			})
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
			got := exhaustedAnswer{
				Answer: relayAnswer{
					Status: resp.StatusCode, NoRetry: resp.Header.Get("X-Should-Retry") == "false", ContentType: resp.Header.Get("Content-Type"),
					WorkerFinal: resp.Header.Get("X-Worker-Final"), Body: string(body),
				},
				PortLookups: lookups.Load(), FloodConns: flood.accepted(), ReplaceConns: replacement.accepted(),
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("client-visible answer and relay attempts (-want +got):\n%s", diff)
			}
		})
	}
}
