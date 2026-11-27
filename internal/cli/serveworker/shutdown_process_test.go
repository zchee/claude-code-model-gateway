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

package serveworker_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// switchedVolume is how much a switched fixture writes after its switch: far more than the socket
// buffers of both ends of a loopback connection hold, so the write ends only while the peer reads.
const switchedVolume = 64 << 20

// switchReply is a protocol switch as net/http recognises one: status 101 with the upgrade headers.
const switchReply = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

// heldConns records connections a fixture took over and closes them all when the test ends. close is
// deferred in the test body, so it runs before the worker's cleanup waits for Run.
type heldConns struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (h *heldConns) add(c net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conns = append(h.conns, c)
}

func (h *heldConns) snapshot() []net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]net.Conn(nil), h.conns...)
}

func (h *heldConns) close() {
	for _, c := range h.snapshot() {
		_ = c.Close()
	}
}

// closedByPeer reads c until the worker closes its end. The deadline only aborts a wait for a close
// that should already have happened; reaching it fails the test.
func closedByPeer(t *testing.T, c net.Conn, what string) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(failureBound)); err != nil {
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
			t.Errorf("the stopped worker left %s open", what)
			return
		default:
			t.Errorf("read of %s: %v; want the worker's close", what, err)
			return
		}
	}
}

func TestStopWithSwitchedUpstreamPending(t *testing.T) {
	tests := map[string]struct {
		body string
		grok bool
	}{
		// The native stream's relay reads the switched connection on its own goroutine.
		"success: a forced stop returns while a native stream relays from a switched upstream connection": {
			body: `{"model":"claude-sonnet-4-6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`,
		},
		// The Grok branch reads a reply that is not a success inside the request handler.
		"success: a forced stop returns while a Grok request reads a switched upstream connection": {
			body: `{"model":"claude-grok-4.7[1m]","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
			grok: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			var held heldConns
			defer held.close()
			settled := newSignal()
			fixture := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					return
				}
				held.add(conn)
				// The switch is followed by far more bytes of the switched protocol than the socket
				// buffers of both ends hold, so the write ends only once the worker has taken the
				// response: it either completes while the worker reads the switched connection, or fails
				// because the worker closed it. Then the connection stays open and silent.
				defer settled.fire()
				if _, err := io.WriteString(conn, switchReply); err != nil {
					return
				}
				chunk := bytes.Repeat([]byte("switched-protocol-bytes\n"), 1<<12)
				for written := 0; written < switchedVolume; written += len(chunk) {
					if _, err := conn.Write(chunk); err != nil {
						return
					}
				}
			}))
			if tt.grok {
				env["CODEX_GATEWAY_GROK_ENDPOINT"] = fixture + "/v1/responses"
				writeFreshGrokAuth(t, env)
			} else {
				env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = fixture
			}
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			// A raw client sends the request and keeps waiting: it never leaves on its own.
			client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", w.public)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Close() }()
			if _, err := io.WriteString(client, "POST /v1/messages HTTP/1.1\r\nHost: "+w.public+"\r\nContent-Type: application/json\r\nContent-Length: "+strconv.Itoa(len(tt.body))+"\r\n\r\n"+tt.body); err != nil {
				t.Fatal(err)
			}
			settled.wait(t, "the worker to take the switched response")

			w.cancel()
			// Run is the join: a worker that waits for the switched connection's inactivity limit does
			// not return within the failure bound.
			if !w.returned() {
				t.Fatal("the stopped worker did not return while an upstream held a switched connection open")
			}
			if w.err != nil {
				t.Errorf("Run: %v", w.err)
			}
			conns := held.snapshot()
			if len(conns) != 1 {
				t.Fatalf("switched connections = %d, want 1", len(conns))
			}
			closedByPeer(t, conns[0], "the switched upstream connection")
		})
	}
}

// writeFreshGrokAuth stores a Grok credential that stays valid for an hour, so no refresh is made.
func writeFreshGrokAuth(t *testing.T, env gatewaytest.Environment) {
	t.Helper()
	dir := env["CODEX_GATEWAY_GROK_HOME"]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"https://auth.x.ai::fixture-client": map[string]any{
		"key":            grokFixtureAccess,
		"refresh_token":  grokFixtureRefresh,
		"expires_at":     time.Now().Add(time.Hour).UnixMilli(),
		"oidc_client_id": "fixture-client",
		"oidc_issuer":    "http://127.0.0.1:9",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// silentTLS accepts connections and never answers, so a client's TLS handshake on them never ends.
type silentTLS struct {
	ln       net.Listener
	accepted chan struct{} // closed at the first accepted connection
	once     sync.Once
	held     heldConns
	loop     sync.WaitGroup
}

func startSilentTLS(t *testing.T, env gatewaytest.Environment) *silentTLS {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := env.Listen(t.Context(), "tcp4", "127.0.0.1:0", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	s := &silentTLS{ln: ln, accepted: make(chan struct{})}
	s.loop.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.held.add(c)
			s.once.Do(func() { close(s.accepted) })
		}
	})
	return s
}

func (s *silentTLS) addr() string { return s.ln.Addr().String() }

// close stops accepting, joins the accept loop and closes every accepted connection.
func (s *silentTLS) close() {
	_ = s.ln.Close()
	s.loop.Wait()
	s.held.close()
}

// TestStopEndsPendingTLSHandshakes decides what a stop does to a catalog refresh and to a readiness
// check whose connection to an HTTPS endpoint never finishes its TLS handshake: Run returns, the
// endpoint sees its accepted connection closed by the worker, and no goroutine of the worker remains.
// The shared package transports would keep such a handshake, and the goroutine dialling it, past
// both joins.
func TestStopEndsPendingTLSHandshakes(t *testing.T) {
	tests := map[string]struct {
		pending string
	}{
		"success: a stop ends the TLS handshake of a pending Anthropic catalog refresh":  {pending: "catalog"},
		"success: a stop ends the TLS handshake of a pending readiness check's redirect": {pending: "readiness"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, state := sandbox(t)
			endpoint := startSilentTLS(t, env)
			defer endpoint.close()

			var armed atomic.Bool
			listed := newSignal()
			switch tt.pending {
			case "catalog":
				env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = "https://" + endpoint.addr()
			case "readiness":
				// The readiness check runs only when the proxy binary exists; this one is not executable,
				// so its auth status command fails at once.
				if err := os.MkdirAll(filepath.Join(state, "bin"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, "bin", "claude-code-proxy"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				// The catalog's listing fetch reads the same proxy URL as the readiness check. It is
				// answered until the redirect is armed, and the periodic refresh is held off, so the only
				// request sent to the TLS endpoint is the readiness check's.
				env["CODEX_GATEWAY_MODELS_REFRESH_MS"] = "Infinity"
				env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/models" {
						http.NotFound(w, r)
						return
					}
					if armed.Load() {
						http.Redirect(w, r, "https://"+endpoint.addr()+"/v1/models", http.StatusFound)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, codexListing)
					listed.fire()
				})))
			}

			// Goroutines that exist before the worker starts, the fixtures' included, are not the worker's.
			ignore := goleak.IgnoreCurrent()
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			clientCtx, cancelClient := context.WithCancel(t.Context())
			var client sync.WaitGroup
			switch tt.pending {
			case "catalog":
				// A cold catalog is refreshed for a listing request that carries a credential.
				client.Go(func() {
					tr := &http.Transport{DisableKeepAlives: true}
					defer tr.CloseIdleConnections()
					req, err := http.NewRequestWithContext(clientCtx, http.MethodGet, "http://"+w.public+"/v1/models", http.NoBody)
					if err != nil {
						return
					}
					req.Header.Set("X-Api-Key", "fixture-catalog-key")
					if resp, err := tr.RoundTrip(req); err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
					}
				})
			case "readiness":
				listed.wait(t, "the startup refresh to read the proxy listing")
				armed.Store(true)
				// The first /healthz starts the shared readiness check.
				_ = getJSON(t, "http://"+w.public+"/healthz")
			}
			select {
			case <-endpoint.accepted:
			case <-time.After(failureBound):
				cancelClient()
				client.Wait()
				t.Fatalf("the %s never connected to the TLS endpoint", tt.pending)
			}

			w.cancel()
			returned := w.returned()
			cancelClient()
			client.Wait()
			if !returned {
				t.Fatal("the stopped worker did not return")
			}
			if w.err != nil {
				t.Errorf("Run: %v", w.err)
			}
			for i, c := range endpoint.held.snapshot() {
				closedByPeer(t, c, "connection "+strconv.Itoa(i)+" to the TLS endpoint")
			}
			goleak.VerifyNone(t, ignore)
		})
	}
}
