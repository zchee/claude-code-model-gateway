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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveworker"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
)

const (
	lifecycleToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// failureBound only ends a wait for an event that should already have happened; no assertion
	// passes by waiting it out.
	failureBound = 5 * time.Second
	codexListing = `{"data":[{"id":"gpt-6.1-sol","display_name":"gpt-6.1-sol (codex)"}]}`
)

// lockedSink records one output stream and offers each write to an optional observer.
type lockedSink struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	observe func(p []byte)
}

func (s *lockedSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.buf.Write(p)
	observe := s.observe
	s.mu.Unlock()
	if observe != nil {
		observe(p)
	}
	return n, err
}

func (s *lockedSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// sandbox creates a private home with a short path (Unix socket names are limited) and the gateway
// environment that isolates it, with the fixture control token in place.
func sandbox(t *testing.T) (string, gatewaytest.Environment, string) {
	t.Helper()
	home, err := os.MkdirTemp("", "mg-life-")
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
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(lifecycleToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, env, state
}

type workerRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	public string
}

// startWorker runs serveworker.Run in this process and returns once it printed its TCP readiness line.
func startWorker(t *testing.T, home string, env gatewaytest.Environment, stdout, stderr io.Writer) *workerRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	w := &workerRun{cancel: cancel, done: make(chan struct{})}
	ready := make(chan string, 1)
	go func() {
		defer close(w.done)
		w.err = serveworker.Run(ctx, serveworker.Options{
			Lookup:  func(k string) (string, bool) { v, ok := env[k]; return v, ok },
			Home:    func() (string, error) { return home, nil },
			Cwd:     func() (string, error) { return home, nil },
			Version: "fixture-build",
			Stdout: logSink(func(p []byte) (int, error) {
				if line, ok := strings.CutPrefix(string(p), "model-gateway shim listening on 127.0.0.1:"); ok {
					port, _, _ := strings.Cut(strings.TrimSpace(line), " ")
					select {
					case ready <- "127.0.0.1:" + port:
					default:
					}
				}
				return stdout.Write(p)
			}),
			Stderr: stderr,
		})
	}()
	t.Cleanup(func() { cancel(); <-w.done })
	select {
	case w.public = <-ready:
	case <-w.done:
		t.Fatalf("worker returned before readiness: %v", w.err)
	case <-time.After(failureBound):
		t.Fatal("no worker readiness line")
	}
	return w
}

// returned reports whether Run returned within the failure bound.
func (w *workerRun) returned() bool {
	select {
	case <-w.done:
		return true
	case <-time.After(failureBound):
		return false
	}
}

func getJSON(t *testing.T, rawURL string) map[string]any {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), failureBound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("GET %s: %v", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc map[string]any
	if err := json.UnmarshalRead(resp.Body, &doc); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, decode error %v", req.URL.Path, resp.StatusCode, err)
	}
	return doc
}

// pollHealth reads /healthz until cond holds, failing at the bound with the last document.
func pollHealth(t *testing.T, public, what string, cond func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(failureBound)
	for {
		doc := getJSON(t, "http://"+public+"/healthz")
		if cond(doc) {
			return doc
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never held; last /healthz: %v", what, doc)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// signal is a channel that is closed once.
type signal struct {
	once sync.Once
	ch   chan struct{}
}

func newSignal() *signal { return &signal{ch: make(chan struct{})} }

func (s *signal) fire() { s.once.Do(func() { close(s.ch) }) }

func (s *signal) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-s.ch:
	case <-time.After(failureBound):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func (s *signal) fired() bool {
	select {
	case <-s.ch:
		return true
	default:
		return false
	}
}

func portOf(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

func TestGracefulDrainAfterNativeDeparture(t *testing.T) {
	tests := map[string]struct{ stream bool }{
		"success: drain ends the worker while a departed native stream waits on its provider": {stream: true},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			entered, providerEnded, release := newSignal(), newSignal(), newSignal()
			// Deferred, so it runs before the worker's cleanup waits for Run.
			defer release.fire()
			env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_native\",\"type\":\"message\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10}}}\n\n")
				w.(http.Flusher).Flush()
				entered.fire()
				select {
				case <-r.Context().Done():
					providerEnded.fire()
				case <-release.ch:
				}
			}))
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			clientCtx, leave := context.WithCancel(t.Context())
			defer leave()
			tr := &http.Transport{DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			req, err := http.NewRequestWithContext(clientCtx, http.MethodPost, "http://"+w.public+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatalf("native stream request: %v", err)
			}
			entered.wait(t, "the native provider")
			first := make([]byte, 16)
			if _, err := io.ReadFull(resp.Body, first); err != nil {
				t.Fatalf("native stream head: %v", err)
			}
			// The client leaves while its provider still holds the stream open.
			leave()
			_ = resp.Body.Close()
			pollHealth(t, w.public, "no live request after the departure", func(doc map[string]any) bool { return doc["activeRequests"] == float64(0) })

			status, active, err := postDrain(t.Context(), w.public, lifecycleToken)
			if err != nil || status != http.StatusAccepted || active != 0 {
				t.Fatalf("drain status=%d, active=%d, err=%v", status, active, err)
			}
			if !w.returned() {
				t.Fatalf("the drained worker kept running for a client that had left (provider still held: %t)", !providerEnded.fired())
			}
			if w.err != nil {
				t.Errorf("Run: %v", w.err)
			}
			providerEnded.wait(t, "the worker to end the departed client's provider request")
		})
	}
}

// heldSink holds the one write that contains marker until released. When that write has ended it
// records whether Run had already returned, which a worker that joins its request handlers never
// lets happen.
type heldSink struct {
	lockedSink
	marker  string
	entered *signal
	release *signal
	// runDone is closed when Run returns; it is set before the held write can start.
	runDone atomic.Pointer[chan struct{}]
	// ended is set once the held write has ended, and afterRun records what it saw at that moment.
	ended, afterRun atomic.Bool
}

func (s *heldSink) Write(p []byte) (int, error) {
	if !strings.Contains(string(p), s.marker) {
		return s.lockedSink.Write(p)
	}
	s.entered.fire()
	<-s.release.ch
	n, err := s.lockedSink.Write(p)
	if done := s.runDone.Load(); done != nil {
		select {
		case <-*done:
			s.afterRun.Store(true)
		default:
		}
	}
	s.ended.Store(true)
	return n, err
}

func TestForcedStopJoinsRequestHandlers(t *testing.T) {
	tests := map[string]struct{ marker string }{
		"success: a forced stop returns only after a request handler's blocked diagnostic write has ended": {
			marker: "unambiguous OpenAI rejection",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			heldEntered, serviceEnded := newSignal(), newSignal()
			stderr := &heldSink{marker: tt.marker, entered: newSignal(), release: newSignal()}
			// Deferred, so it runs before the worker's cleanup waits for Run, which joins the handler
			// that this sink holds.
			defer stderr.release.fire()
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, codexListing)
			})
			mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if r.Header.Get("X-Claude-Code-Request-Id") == "held" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_held\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":10}}}\n\n")
					w.(http.Flusher).Flush()
					heldEntered.fire()
					<-r.Context().Done()
					serviceEnded.fire()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Openai-Request-Id", "req-fixture-rejection")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"OpenAI rejected the fixture credential"}}`)
			})
			env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, mux))
			var stdout lockedSink
			w := startWorker(t, home, env, &stdout, stderr)
			stderr.runDone.Store(&w.done)
			pollHealth(t, w.public, "the proxy catalog", func(doc map[string]any) bool { return doc["catalog"] == "proxy" })

			var clients sync.WaitGroup
			defer clients.Wait()
			clientCtx, cancelClients := context.WithCancel(t.Context())
			defer cancelClients()
			send := func(id string, stream bool) {
				clients.Go(func() {
					tr := &http.Transport{DisableKeepAlives: true}
					defer tr.CloseIdleConnections()
					body := `{"model":"claude-gpt-6.1-sol[1m]","max_tokens":16,"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `,"messages":[{"role":"user","content":"hello"}]}`
					req, err := http.NewRequestWithContext(clientCtx, http.MethodPost, "http://"+w.public+"/v1/messages", strings.NewReader(body))
					if err != nil {
						return
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Claude-Code-Request-Id", id)
					if resp, err := tr.RoundTrip(req); err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
					}
				})
			}
			send("held", true)
			heldEntered.wait(t, "the held provider stream")
			send("rejected", false)
			stderr.entered.wait(t, "the handler to reach its diagnostic line")

			w.cancel()
			// The service context has ended once the held provider request is cancelled; only then is the
			// blocked handler let go.
			serviceEnded.wait(t, "the forced stop to cancel the service")
			stderr.release.fire()
			if !w.returned() {
				t.Fatal("the stopped worker did not return")
			}
			if w.err != nil {
				t.Errorf("Run: %v", w.err)
			}
			// Both observations order the held write against Run's return: one made when the write
			// ended, one made now that Run has returned.
			if stderr.afterRun.Load() {
				t.Error("Run returned before a request handler's blocked diagnostic write had ended")
			}
			if !stderr.ended.Load() {
				t.Errorf("Run returned while a request handler was still blocked writing its diagnostic; stderr=%q", stderr.String())
			}
		})
	}
}

const (
	grokFixtureAccess  = "grok-fixture-expired-access"
	grokFixtureRefresh = "grok-fixture-refresh"
	grokRotatedAccess  = "grok-fixture-rotated-access"
)

func writeExpiredGrokAuth(t *testing.T, env gatewaytest.Environment, issuer string) string {
	t.Helper()
	dir := env["CODEX_GATEWAY_GROK_HOME"]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"https://auth.x.ai::fixture-client": map[string]any{
		"key":            grokFixtureAccess,
		"refresh_token":  grokFixtureRefresh,
		"expires_at":     time.Now().Add(-time.Hour).UnixMilli(),
		"oidc_client_id": "fixture-client",
		"oidc_issuer":    issuer,
	}})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// listenerClosed waits until the worker's TCP listener refuses connections. Only a refused
// connection counts. An accepted connection, a dial timeout, or a reset of a connection that was
// queued while the listener closed means the listener's state is not decided yet, and the dial is
// repeated; any other dial error says nothing about the listener and fails the test.
func listenerClosed(t *testing.T, public string) {
	t.Helper()
	deadline := time.Now().Add(failureBound)
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(t.Context(), "tcp", public)
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			return
		case err == nil:
			_ = conn.Close()
		case errors.Is(err, syscall.ECONNRESET):
		default:
			if ne, ok := errors.AsType[net.Error](err); !ok || !ne.Timeout() {
				t.Fatalf("dial of the stopped worker failed other than by a refused connection: %v", err)
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the stopped worker kept accepting connections")
}

func TestShutdownDuringGrokAuth(t *testing.T) {
	tests := map[string]struct{ hold string }{
		"success: a forced stop cancels a Grok credential discovery":                              {hold: "discovery"},
		"success: a forced stop leaves a committed Grok token exchange to persist its credential": {hold: "exchange"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			entered, ended, release := newSignal(), newSignal(), newSignal()
			// Deferred, so it runs before the worker's cleanup waits for Run, which joins the exchange.
			defer release.fire()
			mux := http.NewServeMux()
			mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
				if tt.hold == "discovery" {
					entered.fire()
					select {
					case <-r.Context().Done():
						ended.fire()
					case <-release.ch:
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"token_endpoint":"http://`+r.Host+`/oauth/token"}`)
			})
			mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				entered.fire()
				select {
				case <-r.Context().Done():
					ended.fire()
				case <-release.ch:
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"access_token":"`+grokRotatedAccess+`","refresh_token":"`+grokFixtureRefresh+`","expires_in":3600}`)
				}
			})
			authFile := writeExpiredGrokAuth(t, env, env.StartUpstream(t, mux))
			env["CODEX_GATEWAY_GROK_ENDPOINT"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_grok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			})) + "/v1/responses"
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)

			clientCtx, cancelClient := context.WithCancel(t.Context())
			var client sync.WaitGroup
			defer client.Wait()
			defer cancelClient()
			client.Go(func() {
				tr := &http.Transport{DisableKeepAlives: true}
				defer tr.CloseIdleConnections()
				req, err := http.NewRequestWithContext(clientCtx, http.MethodPost, "http://"+w.public+"/v1/messages", strings.NewReader(`{"model":"claude-grok-4.7[1m]","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
				if err != nil {
					return
				}
				req.Header.Set("Content-Type", "application/json")
				if resp, err := tr.RoundTrip(req); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
			})
			entered.wait(t, "the Grok credential refresh to reach the issuer")

			w.cancel()
			if tt.hold == "exchange" {
				// The exchange is answered only once the worker has stopped accepting connections.
				listenerClosed(t, w.public)
				release.fire()
			}
			if !w.returned() {
				t.Fatal("the stopped worker did not return")
			}
			if w.err != nil {
				t.Errorf("Run: %v", w.err)
			}
			switch tt.hold {
			case "discovery":
				ended.wait(t, "the worker to end its Grok credential discovery")
			case "exchange":
				raw, err := os.ReadFile(authFile)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(raw, []byte(grokRotatedAccess)) {
					t.Errorf("the committed token exchange was not persisted once Run had returned (rotated credential stored: false, expired credential still stored: %t, exchange cancelled: %t)", bytes.Contains(raw, []byte(grokFixtureAccess)), ended.fired())
				}
			}
		})
	}
}

func codexReadiness(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	cr, ok := doc["codexReadiness"].(map[string]any)
	if !ok {
		t.Fatalf("/healthz has no codexReadiness object: %v", doc)
	}
	return cr
}

func TestCommandWiresCodexReadiness(t *testing.T) {
	tests := map[string]struct{ settled bool }{
		"success: a persisted upstream rejection is reported before the first check completes": {},
		"success: the shared proxy and auth check completes and replaces the checking summary": {settled: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, state := sandbox(t)
			store := upstreamstate.Store{Dir: state, Now: time.Now, CommandPath: "/opt/model-gateway"}
			if _, err := store.SetBlocked(http.StatusUnauthorized, "header:x-openai-request-id", nil); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)
			// The first /healthz starts the shared check, so it always answers before the check completes.
			cr := codexReadiness(t, getJSON(t, "http://"+w.public+"/healthz"))
			if !tt.settled {
				want := map[string]any{
					"state":     "checking",
					"message":   "Codex readiness is still being checked (proxy /v1/models and codex auth status); ask /healthz again in a few seconds.",
					"checks":    nil,
					"checkedAt": nil,
				}
				got := map[string]any{"state": cr["state"], "message": cr["message"], "checks": cr["checks"], "checkedAt": cr["checkedAt"]}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("summary before the first check completes (-want +got):\n%s", diff)
				}
			}
			if tt.settled {
				doc := pollHealth(t, w.public, "a completed readiness check", func(doc map[string]any) bool {
					state, _ := codexReadiness(t, doc)["state"].(string)
					return state != "" && state != "checking"
				})
				cr = codexReadiness(t, doc)
				if _, ok := cr["checks"].(map[string]any); !ok {
					t.Errorf("a completed readiness check reports no checks: %v", cr)
				}
				if cr["checkedAt"] == nil {
					t.Errorf("a completed readiness check reports no checkedAt: %v", cr)
				}
			}
			blocked, ok := cr["upstreamBlocked"].(map[string]any)
			if !ok {
				t.Fatalf("codexReadiness does not expose the persisted rejection: upstreamBlocked=%v (state %v)", cr["upstreamBlocked"], cr["state"])
			}
			if diff := gocmp.Diff(map[string]any{"state": "upstream-blocked", "statusCode": float64(http.StatusUnauthorized)}, map[string]any{"state": blocked["state"], "statusCode": blocked["statusCode"]}); diff != "" {
				t.Errorf("persisted rejection (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCommandLabelsPreviousServerRows(t *testing.T) {
	tests := map[string]struct {
		overrides    string
		wantPrevious []string
	}{
		"success: saved pin overrides decide which server rows are previous": {
			overrides:    `{"opus":"claude-opus-4-8[1m]"}`,
			wantPrevious: []string{"Opus 4.6 (previous)"},
		},
		"success: shipped default pins decide which server rows are previous": {
			wantPrevious: []string{"Opus 4.6 (previous)", "Opus 5.1 (previous)"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, state := sandbox(t)
			if tt.overrides != "" {
				if err := os.WriteFile(filepath.Join(state, "pins.json"), []byte(tt.overrides), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			global := filepath.Join(env["CLAUDE_CONFIG_DIR"], ".claude.json")
			if err := os.WriteFile(global, []byte(`{"additionalModelOptionsCache":[{"value":"claude-opus-4-6","label":"Opus 4.6"},{"value":"claude-opus-5-1","label":"Opus 5.1"}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)
			doc := getJSON(t, "http://"+w.public+"/v1/models")
			rows, _ := doc["data"].([]any)
			var previous []string
			for _, row := range rows {
				m, _ := row.(map[string]any)
				if label, _ := m["display_name"].(string); strings.HasSuffix(label, " (previous)") {
					previous = append(previous, label)
				}
			}
			if diff := gocmp.Diff(tt.wantPrevious, previous); diff != "" {
				t.Errorf("previous server rows in /v1/models (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCommandReportsSettingsLevelBaseURL(t *testing.T) {
	tests := map[string]struct {
		projectLocal string
		processEnv   string
		want         bool
	}{
		"success: project-local settings wiring is reported":                               {projectLocal: "http://127.0.0.1:0", want: true},
		"success: a process-level gateway URL is reported as the source's wired mode does": {processEnv: "http://127.0.0.1:0", want: true},
		"success: an unwired worker reports no settings-level base URL":                    {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			if tt.projectLocal != "" {
				dir := filepath.Join(home, ".claude")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "settings.local.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"`+tt.projectLocal+`"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.processEnv != "" {
				env["ANTHROPIC_BASE_URL"] = tt.processEnv
			}
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)
			doc := getJSON(t, "http://"+w.public+"/healthz")
			usage, _ := doc["usage"].(map[string]any)
			if got := usage["settingsLevelBaseUrl"]; got != tt.want {
				t.Errorf("usage.settingsLevelBaseUrl = %v, want %t", got, tt.want)
			}
			w.cancel()
			if !w.returned() {
				t.Fatal("the stopped worker did not return")
			}
			if strings.Contains(stderr.String(), "shell-only") {
				t.Errorf("a wired worker printed the shell-only warning: %q", stderr.String())
			}
		})
	}
}

func TestDiagnosticStreamSelection(t *testing.T) {
	tests := map[string]struct {
		line       string
		wantStderr bool
	}{
		"success: a discovery cache failure is reported on the diagnostic stream": {line: "could not update discovery cache", wantStderr: true},
		"success: the advertised sentry policies are printed on standard output":  {line: "model-gateway: sentry policy id="},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, codexListing)
			})
			env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, mux))
			// The project is wired to the gateway, so a published proxy listing is written to the
			// discovery cache, whose directory is taken by a regular file.
			claude := filepath.Join(home, ".claude")
			if err := os.WriteFile(filepath.Join(claude, "settings.local.json"), []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:0"}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(env["CLAUDE_CONFIG_DIR"], "cache"), []byte("not a directory"), 0o600); err != nil {
				t.Fatal(err)
			}
			policies := newSignal()
			stdout := &lockedSink{observe: func(p []byte) {
				// The policies are printed after the first refresh, and so after its discovery write.
				if strings.Contains(string(p), "model-gateway: sentry policy id=") {
					policies.fire()
				}
			}}
			var stderr lockedSink
			w := startWorker(t, home, env, stdout, &stderr)
			policies.wait(t, "the first catalog refresh")
			w.cancel()
			if !w.returned() {
				t.Fatal("the stopped worker did not return")
			}
			onStdout, onStderr := strings.Contains(stdout.String(), tt.line), strings.Contains(stderr.String(), tt.line)
			if onStdout == tt.wantStderr || onStderr != tt.wantStderr {
				t.Errorf("%q: on stdout %t, on stderr %t; want only on %s", tt.line, onStdout, onStderr, map[bool]string{true: "stderr", false: "stdout"}[tt.wantStderr])
			}
		})
	}
}
