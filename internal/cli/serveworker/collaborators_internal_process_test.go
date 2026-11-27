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

//go:build process && (darwin || linux)

package serveworker

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
)

const (
	// waitBound only ends a wait for an event that should already have happened; no assertion passes
	// by waiting it out.
	waitBound = 5 * time.Second
	// switchAnswer is a protocol switch as net/http recognises one: status 101 with the upgrade headers.
	switchAnswer = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
)

// composedWorker builds the worker's collaborators as Run builds them, in a private sandbox with
// env's settings, and stops them when the test ends. Nothing serves; the test drives the
// collaborators itself.
func composedWorker(t *testing.T, home string, env gatewaytest.Environment) *lifetime {
	t.Helper()
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	dir := func() (string, error) { return home, nil }
	resolver := paths.New(lookup, dir, dir)
	state, err := resolver.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	_, life, err := buildWorker(workerSetup{
		ctx:      t.Context(),
		lookup:   lookup,
		resolver: resolver,
		state:    state,
		cwd:      dir,
		version:  "fixture-build",
		token:    strings.Repeat("0123456789abcdef", 4),
		stdout:   io.Discard,
		stderr:   io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { life.stop(nil) })
	return life
}

// internalSandbox creates a private home with a short path and the gateway environment that isolates it.
func internalSandbox(t *testing.T) (string, gatewaytest.Environment) {
	t.Helper()
	home, err := os.MkdirTemp("", "mg-int-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	return home, gatewaytest.NewEnv(home, nil, nil, nil)
}

// takenConns records connections a fixture took over and closes them all when the test ends.
type takenConns struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (h *takenConns) add(c net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns = append(h.conns, c)
}

func (h *takenConns) snapshot() []net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]net.Conn(nil), h.conns...)
}

func (h *takenConns) close() {
	for _, c := range h.snapshot() {
		_ = c.Close()
	}
}

// peerClosed reads c until the worker closes its end. The deadline only aborts a wait for a close
// that should already have happened; reaching it fails the test.
func peerClosed(t *testing.T, c net.Conn, what string) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(waitBound)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	for {
		_, err := c.Read(buf)
		switch {
		case err == nil:
			continue
		case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
			return
		case errors.Is(err, os.ErrDeadlineExceeded):
			t.Errorf("the worker left %s open", what)
			return
		default:
			t.Errorf("read of %s: %v; want the worker's close", what, err)
			return
		}
	}
}

func fixturePort(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// TestLocalProviderProbeSwitchedResponse decides what the llama-server probe, on the transport the
// worker gives the model cache, makes of a server that answers /props with a protocol switch and then
// stays silent: the probe fails as a request that could not be completed, as for a server that
// closes without answering, the refresh that ran it returns, the worker closes the switched
// connection, and the worker's stop returns while the server still holds its end open.
func TestLocalProviderProbeSwitchedResponse(t *testing.T) {
	tests := map[string]struct{ switched bool }{
		"error: a llama-server that closes the probe without an answer is a failed request":       {},
		"error: a llama-server that answers the probe with a protocol switch is a failed request": {switched: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env := internalSandbox(t)
			var taken takenConns
			defer taken.close()
			probed := make(chan struct{})
			var probedOnce sync.Once
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":[{"id":"local/gguf","display_name":"local/gguf (local)"}]}`)
			})
			mux.HandleFunc("GET /props", func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				defer probedOnce.Do(func() { close(probed) })
				if !tt.switched {
					_ = conn.Close()
					return
				}
				taken.add(conn)
				_, _ = io.WriteString(conn, switchAnswer)
			})
			fixture := env.StartUpstream(t, mux)
			port := fixturePort(t, fixture)
			env["CODEX_GATEWAY_PROXY_PORT"] = port
			env["CCP_LLAMA_PORT"] = port
			life := composedWorker(t, home, env)

			refreshed := make(chan error, 1)
			go func() { refreshed <- life.cache.Refresh(t.Context()) }()
			select {
			case err := <-refreshed:
				if err != nil {
					t.Fatalf("Refresh: %v", err)
				}
			case <-time.After(waitBound):
				t.Fatal("the refresh did not return while the llama-server held a switched probe connection open")
			}
			select {
			case <-probed:
			default:
				t.Fatal("the refresh returned without probing the llama-server")
			}
			n, err := strconv.ParseUint(port, 10, 16)
			if err != nil {
				t.Fatal(err)
			}
			want := &localprov.LlamaState{Port: uint16(n), Configured: true, Error: "request failed"}
			if diff := gocmp.Diff(want, life.cache.LlamaState()); diff != "" {
				t.Errorf("llama-server state after the probe (-want +got):\n%s", diff)
			}
			if tt.switched {
				conns := taken.snapshot()
				if len(conns) != 1 {
					t.Fatalf("switched probe connections = %d, want 1", len(conns))
				}
				peerClosed(t, conns[0], "the switched probe connection")
			}

			// The server still holds its end of every connection it took.
			stopped := make(chan struct{})
			go func() {
				life.stop(nil)
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(waitBound):
				t.Fatal("the worker's stop did not return while the llama-server held its probe connection")
			}
		})
	}
}

// collectorRequest is what an export fixture saw of one request.
type collectorRequest struct {
	Path        string
	ContentType string
	Empty       bool
}

// TestExportsOutliveTheService decides that the exporters' connections do not end with the service
// context: a record finished after the service ended, as by a request handler that finishes during a
// drain, still reaches its collector, and the exporter's Wait joins its delivery. A transport bound to
// the service would refuse the export's dial at once.
func TestExportsOutliveTheService(t *testing.T) {
	tests := map[string]struct{ trace bool }{
		"success: a usage record finished after the service ended reaches the collector": {},
		"success: a trace span finished after the service ended reaches the collector":   {trace: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env := internalSandbox(t)
			var mu sync.Mutex
			var got []collectorRequest
			collector := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				got = append(got, collectorRequest{Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), Empty: len(body) == 0})
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			path := "/v1/logs"
			if tt.trace {
				path = "/v1/traces"
				env["CODEX_GATEWAY_TELEMETRY_ENDPOINT"] = collector + path
				env["CLAUDE_CODE_PROPAGATE_TRACEPARENT"] = "1"
			} else {
				env["CODEX_GATEWAY_USAGE_ENDPOINT"] = collector + path
			}
			life := composedWorker(t, home, env)

			life.cancel()
			if life.ctx.Err() == nil {
				t.Fatal("test precondition: the service context is still live")
			}
			if tt.trace {
				life.telemetry.Start(nil).Finish(telemetry.Route{Backend: "codex"}, http.StatusOK, "")
				life.telemetry.Wait()
			} else {
				capture := life.usage.Start(usage.Request{})
				capture.SetResponse(http.StatusOK, nil)
				capture.ObserveJSON([]byte(`{"usage":{"input_tokens":15}}`))
				if capture.Finish() == nil {
					t.Fatal("test precondition: the capture built no usage record")
				}
				life.usage.Wait()
			}
			mu.Lock()
			defer mu.Unlock()
			want := []collectorRequest{{Path: path, ContentType: "application/json"}}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("requests the collector received once the exporter's Wait returned (-want +got):\n%s", diff)
			}
		})
	}
}
