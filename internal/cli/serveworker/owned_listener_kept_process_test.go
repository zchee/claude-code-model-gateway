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

//go:build process && (linux || darwin)

package serveworker_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// TestWorkerLeavesParentListenerOpen checks that a worker whose drain exchange failed, or that stopped at once on its
// parent's end, closes only its own copy of the supervisor-owned listening socket. The parent keeps its copy open
// throughout: once the worker has returned, a connection to the port is accepted by the parent's copy and can be
// handed to a later owner, rather than refused, because the socket itself still listens.
//
// Every row rejects a worker that ends the shared socket instead of its own descriptor, for instance by shutting the
// listening socket down to wake its accept loop: the parent's copy then stops listening as well and the connection is
// refused. The control at the end of every row closes the parent's copy too and requires the port to refuse, so the
// connection accepted before it can only have been the parent's copy's.
func TestWorkerLeavesParentListenerOpen(t *testing.T) {
	tests := map[string]struct {
		// answer is the line the parent writes after the notice; "" writes none, so the exchange's budget ends it.
		answer string
		// parentLoss ends the serving worker by its parent's end of file instead of a drain.
		parentLoss bool
	}{
		"error: after an answer for another generation the parent's copy of the listener still accepts": {
			answer: `{"type":"retired","generation":"another-generation"}` + "\n",
		},
		"error: after an unanswered drain notice the parent's copy of the listener still accepts": {},
		"error: after an immediate stop on the parent's end the parent's copy of the listener still accepts": {
			parentLoss: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHandoff(t)
			parentLn, parent := manualHandoff(t, h, false)
			l := h.launch(t, ownedMode)
			port := l.awaitReady(t)
			h.closeTransfer()
			want := strconv.Itoa(parentLn.Addr().(*net.TCPAddr).Port)
			if port != want {
				l.finish(t)
				t.Fatalf("readiness on port %q; want the inherited listener's port %s; status %v: %s\n%s", port, want, l.runErr, strings.Join(l.output, "\n"), l.stderr)
			}
			if data := h.reportBytes(t); len(data) == 0 {
				t.Fatal("the worker did not report its listener")
			}
			ctx, cancel := context.WithDeadline(t.Context(), l.startedAt.Add(fixtureLifetime))
			defer cancel()
			public := "127.0.0.1:" + port
			if status, err := get(ctx, mustPort(t, port), "/healthz"); err != nil || status != http.StatusOK {
				t.Fatalf("health before the stop: %d, %v; want 200", status, err)
			}
			if tc.parentLoss {
				if err := h.liveW.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				// The exchange's budget of five seconds and a margin for a loaded venue, then the checks after it.
				l.requireHeadroom(t, 8*time.Second, "the setup before the drain request")
				if code, _, err := postDrain(ctx, public, ownedToken); err != nil || code != http.StatusAccepted {
					t.Fatalf("direct drain: status %d, %v; want 202", code, err)
				}
				if err := parent.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				msg, err := workerlink.DecodeDrain(bufio.NewReader(parent))
				if err != nil {
					t.Fatalf("read the worker's drain notice: %v", err)
				}
				if diff := gocmp.Diff(workerlink.DrainMessage{Type: workerlink.Draining, Generation: ownedGeneration}, msg); diff != "" {
					t.Errorf("drain notice (-want +got):\n%s", diff)
				}
				if tc.answer != "" {
					if _, err := io.WriteString(parent, tc.answer); err != nil {
						t.Fatalf("write the wrong answer: %v", err)
					}
				}
			}
			if !l.finish(t) || l.runErr != nil {
				t.Fatalf("the worker did not return cleanly: %v: %s", l.runErr, l.stderr)
			}
			if !tc.parentLoss {
				if lines := drainDiagnostics(l.stderr.String()); len(lines) != 1 {
					t.Errorf("diagnostics about the failed drain exchange: %q; want exactly one", lines)
				}
			}

			// The worker has returned; the parent's copy is the only open descriptor of the socket.
			var dialer net.Dialer
			client, err := dialer.DialContext(ctx, "tcp4", public)
			if err != nil {
				t.Fatalf("dial the port after the worker returned: %v; want the parent's copy of the listener to accept it", err)
			}
			defer func() { _ = client.Close() }()
			tcp := parentLn.(*net.TCPListener)
			if err := tcp.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			handed, err := tcp.Accept()
			if err != nil {
				t.Fatalf("the parent's copy of the listener did not hand over the connection: %v", err)
			}
			defer func() { _ = handed.Close() }()
			if diff := gocmp.Diff(client.LocalAddr().String(), handed.RemoteAddr().String()); diff != "" {
				t.Errorf("the connection the parent's copy handed over is not the one dialled (-want +got):\n%s", diff)
			}
			// The later owner talks on it.
			if _, err := io.WriteString(handed, "later owner\n"); err != nil {
				t.Fatal(err)
			}
			if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(client).ReadString('\n')
			if err != nil || line != "later owner\n" {
				t.Errorf("read from the later owner: %q, %v", line, err)
			}
			// Control: with the parent's copy closed too, nothing listens on the port any more.
			if err := parentLn.Close(); err != nil {
				t.Fatal(err)
			}
			if conn, err := dialer.DialContext(ctx, "tcp4", public); err == nil {
				_ = conn.Close()
				t.Error("the port still accepted a connection after the parent closed its copy; another copy of the listener is open")
			} else if !errors.Is(err, syscall.ECONNREFUSED) {
				t.Errorf("dial after the parent closed its copy: %v; want the connection refused", err)
			}
		})
	}
}
