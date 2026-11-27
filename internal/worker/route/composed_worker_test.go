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

package route_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// The source's stateless-backend refusal, byte for byte (lib/request-worker.js:241-253, as
// test/thread-negotiation.test.js:8-16 serializes it).
const sourceThreadRefusal = `{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: beta_header:message-threads-2026-08-12; the model-gateway proxy and Grok backends hold no conversation state. Resend this turn with the full message history.","details":{"error_code":"thread_unsupported_request"}}}`

const composedControlToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// composedListing is the proxy's /v1/models answer: one Codex row and two listed non-Codex providers.
const composedListing = `{"data":[` +
	`{"id":"gpt-5.6-terra","display_name":"gpt-5.6-terra (codex)"},` +
	`{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},` +
	`{"id":"glm-5.3","display_name":"glm-5.3 (opencode)"}` +
	`]}`

func composedClock() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() }

// providerFake is a loopback provider that records every inference request it receives. A model
// listing fetch is answered without being recorded, so it never stands in for a forwarded turn.
type providerFake struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
}

func newProviderFake(t *testing.T, listing string, answer func(http.ResponseWriter, []byte)) *providerFake {
	t.Helper()
	f := &providerFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if listing != "" && r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, listing)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		answer(w, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// received returns copies of the inference request bodies in arrival order.
func (f *providerFake) received() (bodies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range f.bodies {
		bodies = append(bodies, string(b))
	}
	return bodies
}

func jsonAnswer(body string) func(http.ResponseWriter, []byte) {
	return func(w http.ResponseWriter, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// composedWorker is a worker built by compose.New over loopback fakes of Anthropic, the proxy, the
// Grok inference endpoint and the Grok token issuer, served on a loopback listener.
type composedWorker struct {
	t         *testing.T
	gateway   *httptest.Server
	anthropic *providerFake
	proxy     *providerFake
	grok      *providerFake
	issuer    *providerFake
	// grokDials counts connections the upstream client opened toward the Grok endpoint.
	grokDials atomic.Int64
}

// newComposedWorker wires the production collaborators the way the serving command does, with
// telemetry and usage export turned off and the request-side sentry off as in the source fixture
// (test/thread-negotiation.test.js:80-85). env adds worker environment variables.
func newComposedWorker(t *testing.T, env map[string]string, proxyAnswer func(http.ResponseWriter, []byte)) *composedWorker {
	t.Helper()
	serviceCtx, cancelService := context.WithCancel(t.Context())
	dir := t.TempDir()
	cw := &composedWorker{t: t}
	cw.anthropic = newProviderFake(t, "", jsonAnswer(`{"id":"msg_anthropic","type":"message","model":"claude-opus-5","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	cw.proxy = newProviderFake(t, composedListing, proxyAnswer)
	cw.grok = newProviderFake(t, "", jsonAnswer(`{"id":"resp_grok","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	cw.issuer = newProviderFake(t, "", jsonAnswer(`{"access_token":"grok-fixture-access","refresh_token":"grok-fixture-refresh","expires_in":3600}`))
	localEngine := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(localEngine.Close)

	vars := map[string]string{
		"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
		"CODEX_GATEWAY_USAGE_ENDPOINT":     "0",
		"CODEX_GATEWAY_SENTRY":             "0",
		// Keeps the LM Studio probe on a loopback fake instead of the default local port.
		"CCP_LMSTUDIO_BASE_URL": localEngine.URL,
	}
	maps.Copy(vars, env)
	lookup := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }

	grokAddr := cw.grok.srv.Listener.Addr().String()
	allowed := map[string]bool{grokAddr: true, cw.anthropic.srv.Listener.Addr().String(): true, cw.proxy.srv.Listener.Addr().String(): true}
	var dialer net.Dialer
	client := upstream.NewClient(upstream.Options{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !allowed[addr] {
			return nil, errors.New("inference dial outside the provider fakes")
		}
		if addr == grokAddr {
			cw.grokDials.Add(1)
		}
		return dialer.DialContext(ctx, network, addr)
	}})
	issuerAddr := cw.issuer.srv.Listener.Addr().String()
	authTransport := upstream.NewTransport(upstream.Options{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != issuerAddr {
			return nil, errors.New("auth dial outside the issuer fake")
		}
		return dialer.DialContext(ctx, network, addr)
	}})
	t.Cleanup(authTransport.CloseIdleConnections)

	authFile := filepath.Join(dir, "grok-auth.json")
	authDoc, err := json.Marshal(map[string]any{"https://auth.x.ai::fixture-client": map[string]any{
		"key":            "grok-fixture-access",
		"refresh_token":  "grok-fixture-refresh",
		"expires_at":     composedClock().Add(time.Hour).UnixMilli(),
		"oidc_client_id": "fixture-client",
		"oidc_issuer":    cw.issuer.srv.URL,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, authDoc, 0o600); err != nil {
		t.Fatal(err)
	}
	grokBranch, err := compose.NewGrokBranch(compose.GrokOptions{
		Endpoint:        cw.grok.srv.URL + "/v1/responses",
		AuthFile:        authFile,
		InferenceClient: client,
		AuthTransport:   authTransport,
		Sentry:          &sentry.State{},
		Now:             composedClock,
	})
	if err != nil {
		t.Fatalf("compose.NewGrokBranch: %v", err)
	}

	store, sentryState := &route.Store{}, &sentry.State{}
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            paths.New(lookup, func() (string, error) { return dir, nil }, func() (string, error) { return dir, nil }),
		ProxyBaseURL:     cw.proxy.srv.URL,
		AnthropicBaseURL: cw.anthropic.srv.URL,
		DefaultBaseURL:   "http://127.0.0.1:18764",
		Shadow:           true,
		Client:           cw.proxy.srv.Client(),
		Clock:            composedClock,
		Routes:           store,
		Sentry:           sentryState,
	})
	if err != nil {
		t.Fatalf("modelcache.New: %v", err)
	}
	usageEmitter := usage.NewEmitter(lookup, usage.NewBuilder(usage.Options{Now: composedClock}))
	recorder := telemetry.NewRecorder(telemetry.Options{Lookup: lookup, LogPath: filepath.Join(dir, "logs", "request-routes.jsonl"), Diagnostics: io.Discard, Now: composedClock})
	t.Cleanup(func() {
		cancelService()
		_ = client.Close()
		cache.Wait()
		usageEmitter.Wait()
		recorder.Wait()
	})
	if err := cache.Refresh(serviceCtx); err != nil {
		t.Fatalf("modelcache.Refresh: %v", err)
	}
	if source, _ := cache.HealthCatalog(); source != "proxy" {
		t.Fatalf("catalog source after the first refresh = %q, want the proxy listing", source)
	}

	prepare, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: filepath.Join(dir, "state", "dispatch-routes.json"), Lookup: lookup, Now: composedClock})
	if err != nil {
		t.Fatalf("proxyreq.New: %v", err)
	}
	options, err := compose.New(compose.Options{
		ServiceContext:   serviceCtx,
		Version:          "fixture-build",
		ControlToken:     composedControlToken,
		Lookup:           lookup,
		AnthropicBaseURL: cw.anthropic.srv.URL,
		ProxyBaseURL:     cw.proxy.srv.URL,
		CommandPath:      "/opt/model-gateway",
		InstalledVersion: "fixture-build",
		LogsDir:          filepath.Join(dir, "logs"),
		Routes:           store,
		ModelCache:       cache,
		Sentry:           sentryState,
		Upstream:         client,
		UpstreamState:    &upstreamstate.Store{Dir: filepath.Join(dir, "state"), Now: composedClock, CommandPath: "/opt/model-gateway"},
		ProxyReq:         prepare,
		CompactGuard:     &compactguard.Runner{ServiceContext: serviceCtx, Settings: compactguard.Settings{StreamGuard: true, Retries: 2, MaxBufferedBytes: 16 << 20}, Stderr: io.Discard},
		Grok:             grokBranch,
		Usage:            usageEmitter,
		Telemetry:        recorder,
		Now:              composedClock,
		Diagnostics:      io.Discard,
	})
	if err != nil {
		t.Fatalf("compose.New: %v", err)
	}
	handler, err := worker.NewHandler(options)
	if err != nil {
		t.Fatalf("worker.NewHandler: %v", err)
	}
	cw.gateway = httptest.NewServer(handler)
	t.Cleanup(func() {
		cancelService()
		cw.gateway.CloseClientConnections()
		cw.gateway.Close()
	})
	return cw
}

// post sends one request through the worker's listener and returns its status, content type and body.
func (cw *composedWorker) post(path, body string) (status int, contentType, response string) {
	cw.t.Helper()
	req, err := http.NewRequestWithContext(cw.t.Context(), http.MethodPost, cw.gateway.URL+path, strings.NewReader(body))
	if err != nil {
		cw.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cw.gateway.Client().Do(req)
	if err != nil {
		cw.t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		cw.t.Fatalf("read the response of POST %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(data)
}

// threadPayload is the source's three-turn conversation (test/thread-negotiation.test.js:88-99) for
// model, with a thread member spelled by thread when it is not empty.
func threadPayload(model, thread string) string {
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"first turn"},{"role":"assistant","content":"first response"},{"role":"user","content":"current turn"}]`
	if thread != "" {
		body += `,"thread":` + thread
	}
	return body + `}`
}

// threadShapes are the refused thread spellings of the source cases: a continuation and each
// malformed value (test/thread-negotiation.test.js:111-136).
var threadShapes = map[string]string{
	"continuation":            `{"type":"continue","previous_message_id":"message_123"}`,
	"null thread":             `null`,
	"string thread":           `"continue"`,
	"array thread":            `[]`,
	"empty object thread":     `{}`,
	"thread of unknown type":  `{"type":"resume"}`,
	"create spelled as array": `["create"]`,
}
