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
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveworker"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// TestDrainInterruptedByStopOwnedListener carries the cases of the drain-interrupted-by-stop test with the supervised
// worker's listener and drain channel received from its parent. A draining worker with a stalled upload is ended at
// once by cancellation (standalone, over TCP and over its Unix socket) and by its parent's end of file (supervised):
// the drain was accepted with the upload counted, the worker announced it once, and the end of file comes while the
// parent withholds its answer, or after it answered. The worker exits cleanly within moments, with the upload still
// stalled: it waits neither for an answer nor for the upload.
func TestDrainInterruptedByStopOwnedListener(t *testing.T) {
	tests := map[string]struct {
		useUnix   bool
		parentEOF bool
		// answered makes the parent answer the notice before its end of file.
		answered bool
	}{
		"error: cancellation ends draining worker with stalled TCP upload":                       {},
		"error: cancellation ends draining worker with stalled Unix upload":                      {useUnix: true},
		"error: parent EOF ends draining worker with stalled upload":                             {parentEOF: true},
		"error: parent EOF after the drain was answered ends the worker with its upload stalled": {parentEOF: true, answered: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			var public string
			var stop func()
			var exited <-chan struct{}
			var exitErr func() error
			var h *handoff
			// uploadSocket, when set, is the Unix socket the stalled upload goes to; the drain always uses TCP, as a
			// drain over the Unix socket is refused.
			var uploadSocket string
			if tt.parentEOF {
				h = newHandoff(t)
				l := h.launch(t, ownedMode)
				port := l.awaitReady(t)
				h.closeTransfer()
				if port == "" {
					l.finish(t)
					t.Fatalf("supervised worker exited before readiness: %v; output=%s; stderr=%s", l.runErr, strings.Join(l.output, "\n"), l.stderr)
				}
				_ = h.reportBytes(t)
				public = "127.0.0.1:" + port
				stop = func() { _ = h.liveW.Close() }
				exited = l.done
				exitErr = func() error { return l.runErr }
			} else {
				home, err := os.MkdirTemp("", "mg-stop-") // sockaddr_un paths have a small platform limit.
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.RemoveAll(home); err != nil {
						t.Error(err)
					}
				})
				env := gatewaytest.NewEnv(home, nil, nil, nil)
				state := filepath.Join(env["CLAUDE_CONFIG_DIR"], "model-gateway")
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(ownedToken+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				serviceCtx, cancelService := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancelService()
				portCh := make(chan string, 1)
				done := make(chan struct{})
				var runErr error
				go func() {
					defer close(done)
					runErr = serveworker.Run(serviceCtx, serveworker.Options{
						Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
						Home:   func() (string, error) { return home, nil },
						Cwd:    func() (string, error) { return home, nil },
						Stdout: logSink(func(p []byte) (int, error) {
							if line, ok := strings.CutPrefix(string(p), "model-gateway shim listening on 127.0.0.1:"); ok {
								port, _, _ := strings.Cut(strings.TrimSpace(line), " ")
								select {
								case portCh <- "127.0.0.1:" + port:
								default:
								}
							}
							return len(p), nil
						}),
					})
				}()
				t.Cleanup(func() { cancelService(); unblock(); <-done })
				select {
				case public = <-portCh:
				case <-done:
					t.Fatalf("worker exited before readiness: %v", runErr)
				case <-serviceCtx.Done():
					t.Fatal(serviceCtx.Err())
				}
				if tt.useUnix {
					uploadSocket = env["CODEX_GATEWAY_SOCKET_PATH"]
				}
				stop = cancelService
				exited = done
				exitErr = func() error { return runErr }
			}
			target := public
			if uploadSocket != "" {
				target = unixProxy(t, uploadSocket)
			}
			inflight := stalledUpload(t, ctx, target, release)
			status, active, err := postDrain(ctx, public, ownedToken)
			if err != nil || status != http.StatusAccepted || active != 1 {
				t.Fatalf("drain status=%d, active=%d, err=%v", status, active, err)
			}
			if h != nil {
				msg, err := h.readDrain(t, 5*time.Second)
				if err != nil {
					t.Fatalf("read the worker's drain notice: %v", err)
				}
				if diff := gocmp.Diff(workerlink.DrainMessage{Type: workerlink.Draining, Generation: ownedGeneration}, msg); diff != "" {
					t.Errorf("drain notice (-want +got):\n%s", diff)
				}
				if tt.answered {
					h.res.Retire()
					if err := h.res.AcknowledgeDrain(ctx); err != nil {
						t.Fatalf("answer the drain notice: %v", err)
					}
				}
			}
			// Signal stop while the upload remains stalled; the draining worker must exit promptly.
			stop()
			select {
			case <-exited:
				if err := exitErr(); err != nil {
					t.Errorf("draining worker exited with error after stop: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Errorf("draining worker stayed alive after stop while upload remained stalled")
			}
			// The upload was still stalled, so it never gets a response; whether its connection has ended by now is not
			// pinned.
			select {
			case r := <-inflight:
				if r.err == nil && r.status != 0 {
					t.Errorf("the stalled upload got a response %d after the stop; want none", r.status)
				}
			default:
			}
		})
	}
}

// unixProxy forwards loopback TCP connections to the Unix socket at path, so that the HTTP helpers that dial TCP
// reach the worker's Unix listener. It returns the TCP address.
func unixProxy(t *testing.T, path string) string {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = client.Close() }()
				var dialer net.Dialer
				server, err := dialer.DialContext(context.WithoutCancel(t.Context()), "unix", path)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				var pipes sync.WaitGroup
				pipes.Go(func() { copyAndClose(server, client) })
				copyAndClose(client, server)
				pipes.Wait()
			})
		}
	})
	return ln.Addr().String()
}

func copyAndClose(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = dst.Close()
}
