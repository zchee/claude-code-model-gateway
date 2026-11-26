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

package modelcache_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxyconfig"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

var (
	_ modelcache.RoutePublisher                                                     = (*route.Store)(nil)
	_ modelcache.SentryPublisher                                                    = (*sentry.State)(nil)
	_ func(*modelcache.Cache, context.Context, http.Header) (jsontext.Value, error) = (*modelcache.Cache).ModelsResponse
	_ func(*modelcache.Cache) (string, int)                                         = (*modelcache.Cache).HealthCatalog
	_ func(*modelcache.Cache, http.Header)                                          = (*modelcache.Cache).TriggerAnthropicRefresh
	_ func(*modelcache.Cache) (route.Snapshot, uint64)                              = (*modelcache.Cache).Snapshot
)

const (
	studioSecretMarker      = "lmstudio-synth-marker-84f1"
	authBearerPayload       = "anthropic-auth-synth-marker-91c2"
	authBearerHeader        = "Bearer " + authBearerPayload
	apiKeyMarker            = "sk-ant-synth-marker-37b4"
	disallowedCookiePayload = "cookie-synth-marker-52d8"
	disallowedCookieHeader  = "session=" + disallowedCookiePayload
	disallowedHeaderMarker  = "custom-synth-marker-63e9"
	bodySecretSentinel      = "RAW_UPSTREAM_BODY_SENTINEL_DO_NOT_PERSIST"
)

var allCredentialMarkers = map[string]string{
	"studio token":      studioSecretMarker,
	"authorization":     authBearerPayload,
	"x-api-key":         apiKeyMarker,
	"disallowed cookie": disallowedCookiePayload,
	"disallowed header": disallowedHeaderMarker,
}

func redactTestMarkers(s string) string {
	return strings.NewReplacer(
		studioSecretMarker, "<studio-token-marker>",
		authBearerPayload, "<auth-bearer-marker>",
		apiKeyMarker, "<api-key-marker>",
		disallowedCookiePayload, "<cookie-marker>",
		disallowedHeaderMarker, "<custom-header-marker>",
		bodySecretSentinel, "<upstream-body-sentinel>",
	).Replace(s)
}

func baseURLFunc(u string) func() (string, error) {
	return func() (string, error) { return u, nil }
}

func pinsFunc(p map[string]string) func() map[string]string {
	return func() map[string]string { return maps.Clone(p) }
}

type modelRow struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	Type           string `json:"type"`
	MaxInputTokens int64  `json:"max_input_tokens"`
}

type idDisplay struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.buf, format+"\n", args...)
}

func (l *logCapture) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.buf.Bytes())
}

type recordingSentry struct {
	state *sentry.State
	mu    sync.Mutex
	gens  []uint64
}

func (r *recordingSentry) Publish(generation uint64, resolver sentry.ModelResolver, windows policy.Windows, env float64) {
	r.mu.Lock()
	r.gens = append(r.gens, generation)
	r.mu.Unlock()
	r.state.Publish(generation, resolver, windows, env)
}

func (r *recordingSentry) generations() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.gens)
}

type failingTransport struct {
	err error
}

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

func envLookup(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{Proxy: nil}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newFailingClient() *http.Client {
	return &http.Client{Transport: failingTransport{err: errors.New("proxy unavailable")}}
}

func assertNoMarkers(t *testing.T, artifact string, data []byte, markers map[string]string) {
	t.Helper()
	for label, marker := range markers {
		if bytes.Contains(data, []byte(marker)) {
			t.Errorf("%s leaked %s marker", artifact, label)
		}
	}
}

func assertNoHeaderMarkers(t *testing.T, recipient string, header http.Header, markers map[string]string) {
	t.Helper()
	for name, vals := range header {
		for label, marker := range markers {
			if strings.Contains(name, marker) {
				t.Errorf("%s received leaked %s marker in header name", recipient, label)
			}
			for _, v := range vals {
				if strings.Contains(v, marker) {
					t.Errorf("%s received leaked %s marker in header %q", recipient, label, redactTestMarkers(name))
				}
			}
		}
	}
}

func assertNoRequestMarkers(t *testing.T, recipient string, r *http.Request, markers map[string]string) {
	t.Helper()
	assertNoHeaderMarkers(t, recipient, r.Header, markers)
	for label, marker := range markers {
		if strings.Contains(r.URL.String(), marker) || strings.Contains(r.RequestURI, marker) {
			t.Errorf("%s received leaked %s marker in request URL", recipient, label)
		}
	}
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("%s read request body: %v", recipient, err)
			return
		}
		assertNoMarkers(t, recipient+" request body", body, markers)
	}
}

type gatedTransport struct {
	base        http.RoundTripper
	allowedHost string
	blocked     atomic.Bool
	attempts    atomic.Int32
}

func (g *gatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if g.blocked.Load() {
		g.attempts.Add(1)
		return nil, errors.New("outbound HTTP attempt blocked during test")
	}
	if g.allowedHost != "" && req.URL.Host != g.allowedHost {
		g.attempts.Add(1)
		return nil, fmt.Errorf("unexpected outbound host %q during setup", req.URL.Host)
	}
	return g.base.RoundTrip(req)
}

func newTestPaths(t *testing.T, extra map[string]string) (string, config.LookupEnv, paths.Resolver) {
	t.Helper()
	home := t.TempDir()
	values := map[string]string{
		"HOME":                    home,
		"CLAUDE_CONFIG_DIR":       filepath.Join(home, ".claude"),
		"MODEL_GATEWAY_STATE_DIR": filepath.Join(home, ".claude", "model-gateway"),
		"CODEX_GATEWAY_GROK_HOME": filepath.Join(home, ".grok"),
		"CCP_CONFIG_DIR":          filepath.Join(home, "proxy-config"),
	}
	maps.Copy(values, extra)
	lookup := envLookup(values)
	resolver := paths.New(lookup, func() (string, error) { return home, nil }, func() (string, error) { return home, nil })
	return home, lookup, resolver
}

type handlerTransport struct {
	handler http.Handler
}

func (ht handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	ht.handler.ServeHTTP(rec, req)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

func TestStartupFallbackAndPublication(t *testing.T) {
	tests := map[string]struct {
		listDispatch string
		wantIDs      []string
	}{
		"success: default fallback models without dispatch": {
			listDispatch: "0",
			wantIDs: []string{
				"claude-gpt-6.1-sol[1m]",
				"claude-gpt-5.6-sol[1m]",
				"claude-gpt-5.6-terra[1m]",
				"claude-gpt-5.6-luna[1m]",
				"claude-gpt-6-astra[1m]",
			},
		},
		"success: default fallback models with dispatch enabled": {
			listDispatch: "1",
			wantIDs: []string{
				"claude-gpt-6.1-sol[1m]",
				"claude-gpt-5.6-sol[1m]",
				"claude-gpt-5.6-terra[1m]",
				"claude-gpt-5.6-luna[1m]",
				"claude-gpt-6-astra[1m]",
				"claude-codex-auto",
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, map[string]string{
				"CODEX_GATEWAY_LIST_DISPATCH_MODEL": tt.listDispatch,
			})
			routes := &route.Store{}
			sentryState := &sentry.State{}
			recSentry := &recordingSentry{state: sentryState}
			var logs logCapture
			windows := policy.Windows{
				"codex": {Value: config.ContextWindow{Tokens: 272000}, Source: "default"},
				"grok":  {Value: config.ContextWindow{Full: true}, Source: "default"},
			}

			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				Windows:        windows,
				ProxyBaseURL:   "http://proxy.invalid",
				DefaultBaseURL: "http://127.0.0.1:19999",
				Client:         newFailingClient(),
				Routes:         routes,
				Sentry:         recSentry,
				Logf:           logs.logf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Mutate caller map after construction to prove New copied ResolverInputs.
			windows["codex"] = policy.Setting{Value: config.ContextWindow{Tokens: 185000}, Source: "saved"}

			snapFromCache, cacheGen := cache.Snapshot()
			if cacheGen != 1 || cache.Generation() != 1 {
				t.Errorf("startup Snapshot/Generation = (%d, %d), want (1, 1)", cacheGen, cache.Generation())
			}
			if diff := gocmp.Diff([]uint64{1}, recSentry.generations()); diff != "" {
				t.Errorf("startup sentry generations mismatch (-want +got):\n%s", diff)
			}
			source, count := cache.HealthCatalog()
			if source != "fallback" || count != len(tt.wantIDs) {
				t.Errorf("HealthCatalog = (%q, %d), want (%q, %d)", source, count, "fallback", len(tt.wantIDs))
			}

			snap, ok := routes.Load()
			if !ok {
				t.Fatal("route.Store had no snapshot after New")
			}
			cacheSrc, cacheRaw := snapFromCache.Catalog()
			snapSource, rawCatalog := snap.Catalog()
			if snapSource != "fallback" || cacheSrc != "fallback" {
				t.Errorf("snapshot sources = (store=%q, cache=%q), want fallback", snapSource, cacheSrc)
			}
			if !bytes.Equal(cacheRaw, rawCatalog) {
				t.Errorf("route.Store catalog differs from Cache.Snapshot catalog")
			}
			var rows []modelRow
			if err := json.Unmarshal(rawCatalog, &rows); err != nil {
				t.Fatalf("unmarshal startup catalog: %v", err)
			}
			gotIDs := make([]string, 0, len(rows))
			for _, r := range rows {
				gotIDs = append(gotIDs, r.ID)
			}
			if diff := gocmp.Diff(tt.wantIDs, gotIDs); diff != "" {
				t.Errorf("startup model IDs mismatch (-want +got):\n%s", diff)
			}
			if len(rows) == 0 {
				t.Fatal("startup catalog has 0 rows")
			}
			// Confirm the cap remained 272000 despite caller map mutation.
			if rows[0].MaxInputTokens != 272000 {
				t.Errorf("startup codex max_input_tokens = %d, want immutable 272000", rows[0].MaxInputTokens)
			}

			// Confirm sentry.State received generation 1 at startup (272000 cap -> trigger 187000).
			session := sentry.SessionKey(true, "startup-session", nil)
			target := sentry.Target{Key: "gpt-6.1-sol"}
			sentryState.Observe(session, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":187001}}`), true)
			crossing, refused := sentryState.Check(session, target, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
			if !refused || crossing.Trigger != 187000 {
				t.Errorf("startup sentry Check = (%+v, %v), want refused at trigger 187000", crossing, refused)
			}
		})
	}
}

func TestRefreshOrderingGenerationAndSentryFollowsLatest(t *testing.T) {
	var llamaCtx atomic.Int64
	llamaCtx.Store(65536)
	llamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertNoRequestMarkers(t, "llama-server", r, allCredentialMarkers)
		if r.URL.Path != "/props" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":` + strconv.FormatInt(llamaCtx.Load(), 10) + `}}`))
	}))
	t.Cleanup(llamaSrv.Close)
	llamaPort := llamaSrv.Listener.Addr().(*net.TCPAddr).Port

	var studioCtx atomic.Int64
	studioCtx.Store(65536)
	studioSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertNoRequestMarkers(t, "LM Studio", r, map[string]string{
			"authorization":     authBearerPayload,
			"x-api-key":         apiKeyMarker,
			"disallowed cookie": disallowedCookiePayload,
			"disallowed header": disallowedHeaderMarker,
		})
		if r.URL.Path != "/api/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+studioSecretMarker {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		ctxLen := studioCtx.Load()
		instances := `[]`
		if ctxLen > 0 {
			instances = `[{"id":"qwen3-coder-30b","config":{"context_length":` + strconv.FormatInt(ctxLen, 10) + `}}]`
		}
		_, _ = w.Write([]byte(`{"models":[{"type":"llm","key":"qwen3-coder-30b","loaded_instances":` + instances + `}]}`))
	}))
	t.Cleanup(studioSrv.Close)

	// Raw .data carries an escaped lone surrogate and a raw invalid UTF-8 byte beside a literal U+FFFD
	// counterpart so only callers passing raw .data to proxymodels.ParseListing satisfy the contract.
	rawProxyBody := append([]byte(
		`{"object":"list","data":[`+
			`{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"},`+
			`{"id":"claude-opus-5-5","display_name":"claude-opus-5-5 (anthropic)"},`+
			`{"id":"bad\uD800model","display_name":"bad\uD800model (kimi)"},`+
			`{"id":"kimi-raw`,
	), 0xff)
	rawProxyBody = append(rawProxyBody, []byte(`","display_name":"kimi-raw`)...)
	rawProxyBody = append(rawProxyBody, 0xff)
	rawProxyBody = append(rawProxyBody, []byte(
		` (kimi)"},`+
			`{"id":"kimi-lit`+"�"+`","display_name":"kimi-lit`+"�"+` (kimi)"},`+
			`{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},`+
			`{"id":"gemini-4-argon","display_name":"gemini-4-argon (vertex)"},`+
			`{"id":"vertex/gemini-4-argon","display_name":"vertex/gemini-4-argon (vertex)"},`+
			`{"id":"vertex/anthropic/claude-fable-5-1","display_name":"vertex/anthropic/claude-fable-5-1 (vertex)"},`+
			`{"id":"local/gguf","display_name":"local/gguf (local)"},`+
			`{"id":"lmstudio/qwen3-coder-30b","display_name":"lmstudio/qwen3-coder-30b (lmstudio)"}`+
			`]}`,
	)...)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertNoRequestMarkers(t, "proxy /v1/models", r, allCredentialMarkers)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(rawProxyBody)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(proxySrv.Close)

	_, lookup, resolver := newTestPaths(t, map[string]string{
		"CCP_LLAMA_PORT":        strconv.Itoa(llamaPort),
		"CCP_LMSTUDIO_BASE_URL": studioSrv.URL,
		"CCP_LMSTUDIO_API_KEY":  studioSecretMarker,
	})

	routes := &route.Store{}
	sentryState := &sentry.State{}
	recSentry := &recordingSentry{state: sentryState}
	var logs logCapture
	windows := policy.Windows{
		"codex": {Value: config.ContextWindow{Tokens: 272000}, Source: "default"},
		"grok":  {Value: config.ContextWindow{Full: true}, Source: "default"},
	}
	cache, err := modelcache.New(modelcache.Options{
		Lookup:         lookup,
		Paths:          resolver,
		Windows:        windows,
		ProxyBaseURL:   proxySrv.URL,
		DefaultBaseURL: "http://127.0.0.1:19999",
		Client:         newTestClient(t),
		Routes:         routes,
		Sentry:         recSentry,
		GrokIDs:        func() []string { return []string{"grok-4.7", "grok-unlisted-not-in-policy"} },
		Logf:           logs.logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, gen := cache.Snapshot(); gen != 1 {
		t.Fatalf("Snapshot generation after New = %d, want 1", gen)
	}
	if diff := gocmp.Diff([]uint64{1}, recSentry.generations()); diff != "" {
		t.Fatalf("sentry generations after New mismatch (-want +got):\n%s", diff)
	}

	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	snap1FromCache, gen1 := cache.Snapshot()
	if gen1 != 2 || cache.Generation() != 2 {
		t.Fatalf("Snapshot/Generation after first Refresh = (%d, %d), want (2, 2)", gen1, cache.Generation())
	}
	if diff := gocmp.Diff([]uint64{1, 2}, recSentry.generations()); diff != "" {
		t.Fatalf("sentry generations after first Refresh mismatch (-want +got):\n%s", diff)
	}

	snap1, ok := routes.Load()
	if !ok {
		t.Fatal("route.Store empty after Refresh")
	}
	src, rawCatalog := snap1.Catalog()
	cacheSrc1, cacheRaw1 := snap1FromCache.Catalog()
	if src != "proxy" || cacheSrc1 != "proxy" || !bytes.Equal(rawCatalog, cacheRaw1) {
		t.Errorf("snapshot mismatch: store source=%q cache source=%q", src, cacheSrc1)
	}
	var rows []modelRow
	if err := json.Unmarshal(rawCatalog, &rows); err != nil {
		t.Fatalf("unmarshal refreshed catalog: %v", err)
	}
	wantRows := []modelRow{
		{ID: "claude-gpt-6-astra[1m]", DisplayName: "GPT-6-astra (Codex)", Type: "model", MaxInputTokens: 272000},
		{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7", Type: "model", MaxInputTokens: 500000},
		{ID: "claude-kimi-lit�", DisplayName: "kimi-lit� (Kimi)", Type: "model", MaxInputTokens: 200000},
		{ID: "claude-kimi-k3", DisplayName: "kimi-k3 (Kimi)", Type: "model", MaxInputTokens: 200000},
		{ID: "claude-gemini-4-argon[1m]", DisplayName: "gemini-4-argon (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
		{ID: "claude-vertex/anthropic/fable-5-1[1m]", DisplayName: "vertex/anthropic/claude-fable-5-1 (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
		{ID: "claude-local/gguf", DisplayName: "local/gguf (Local GGUF)", Type: "model", MaxInputTokens: 65536},
		{ID: "claude-lmstudio/qwen3-coder-30b", DisplayName: "lmstudio/qwen3-coder-30b (LM Studio)", Type: "model", MaxInputTokens: 65536},
	}
	if diff := gocmp.Diff(wantRows, rows); diff != "" {
		t.Errorf("refreshed catalog rows mismatch (-want +got):\n%s", redactTestMarkers(diff))
	}

	// Local-engine state handed out by Cache must be an immutable deep copy.
	llamaCopy := cache.LlamaState()
	if llamaCopy == nil || !llamaCopy.Reachable || llamaCopy.ContextWindow != 65536 {
		t.Fatalf("cache.LlamaState() = %s, want reachable 65536", redactTestMarkers(fmt.Sprintf("%+v", llamaCopy)))
	}
	llamaCopy.Reachable = false
	llamaCopy.ContextWindow = 999999
	if got := cache.LlamaState(); got == nil || !got.Reachable || got.ContextWindow != 65536 {
		t.Errorf("cache.LlamaState() mutated through returned copy: %s", redactTestMarkers(fmt.Sprintf("%+v", got)))
	}
	studioCopy := cache.StudioState()
	if studioCopy == nil || !studioCopy.Reachable || len(studioCopy.Models) != 1 || studioCopy.Models[0].ContextWindow != 65536 {
		t.Fatalf("cache.StudioState() = %s, want reachable 1 model with 65536", redactTestMarkers(fmt.Sprintf("%+v", studioCopy)))
	}
	studioCopy.Reachable = false
	studioCopy.Models[0].ContextWindow = 999999
	studioCopy.Models = append(studioCopy.Models, localprov.Model{Key: "injected", Loaded: true, ContextWindow: 50000})
	if got := cache.StudioState(); got == nil || !got.Reachable || len(got.Models) != 1 || got.Models[0].ContextWindow != 65536 {
		t.Errorf("cache.StudioState() mutated through returned copy: %s", redactTestMarkers(fmt.Sprintf("%+v", got)))
	}

	// Simulate an in-flight request that captured snap1 while LM Studio had 65536 (trigger 43691).
	session := sentry.SessionKey(true, "window-shift-session", nil)
	targetFromSnap1, err := sentry.ResolveTarget(snap1.Resolver(), "claude-lmstudio/qwen3-coder-30b", 0, 0, 0)
	if err != nil {
		t.Fatalf("ResolveTarget on snap1: %v", err)
	}
	sentryState.Observe(session, targetFromSnap1, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":30000}}`), true)
	if _, refused := sentryState.Check(session, targetFromSnap1, []byte(`{"messages":[{"role":"user","content":"turn"}]}`)); refused {
		t.Fatal("usage 30000 must not trip 65536 trigger (43691)")
	}

	// Reload LM Studio to 40960 (trigger 27307) and refresh (generation 3).
	studioCtx.Store(40960)
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	_, gen2 := cache.Snapshot()
	if gen2 != 3 || cache.Generation() != 3 {
		t.Fatalf("Snapshot/Generation after second Refresh = (%d, %d), want (3, 3)", gen2, cache.Generation())
	}
	if diff := gocmp.Diff([]uint64{1, 2, 3}, recSentry.generations()); diff != "" {
		t.Fatalf("sentry generations after second Refresh mismatch (-want +got):\n%s", diff)
	}
	snap2, ok := routes.Load()
	if !ok {
		t.Fatal("route.Store empty after second Refresh")
	}
	if got := snap2.Resolver().AdvertisedWindow("claude-lmstudio/qwen3-coder-30b"); got != 40960 {
		t.Errorf("snap2 advertised window = %d, want 40960", got)
	}
	// The request keeps its routing snapshot (snap1 still resolves 65536)...
	if got := snap1.Resolver().AdvertisedWindow("claude-lmstudio/qwen3-coder-30b"); got != 65536 {
		t.Errorf("snap1 advertised window mutated to %d, want immutable 65536", got)
	}
	// ...but sentry callbacks use the latest published policy (trigger 27307).
	crossing, refused := sentryState.Check(session, targetFromSnap1, []byte(`{"messages":[{"role":"user","content":"turn"}]}`))
	if !refused || crossing.Usage != 30000 || crossing.Trigger != 27307 {
		t.Errorf("sentry Check after reload = (%+v, %v), want refused (30000 > 27307)", crossing, refused)
	}

	// Seed a fresh unlatched high-usage session under generation 3 so omitting generation 4 would trip 30000 > 27307.
	freshSession := sentry.SessionKey(true, "unlatched-unload-session", nil)
	sentryState.Observe(freshSession, targetFromSnap1, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":30000}}`), true)

	// Unload LM Studio (context 0) and refresh (generation 4): model becomes unguarded.
	studioCtx.Store(0)
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("third Refresh: %v", err)
	}
	_, gen3 := cache.Snapshot()
	if gen3 != 4 || cache.Generation() != 4 {
		t.Fatalf("Snapshot/Generation after third Refresh = (%d, %d), want (4, 4)", gen3, cache.Generation())
	}
	if diff := gocmp.Diff([]uint64{1, 2, 3, 4}, recSentry.generations()); diff != "" {
		t.Fatalf("sentry generations after third Refresh mismatch (-want +got):\n%s", diff)
	}
	snap3, ok := routes.Load()
	if !ok {
		t.Fatal("route.Store empty after third Refresh")
	}
	if got := snap3.Resolver().AdvertisedWindow("claude-lmstudio/qwen3-coder-30b"); got != 200000 {
		t.Errorf("snap3 advertised window after unload = %d, want 200000", got)
	}
	if _, refused := sentryState.Check(freshSession, targetFromSnap1, []byte(`{"messages":[{"role":"user","content":"turn"}]}`)); refused {
		t.Error("unloaded LM Studio model refused an unlatched high-usage session")
	}
	assertNoMarkers(t, "refresh logs", logs.bytes(), allCredentialMarkers)
	assertNoMarkers(t, "refreshed catalog", rawCatalog, allCredentialMarkers)
}

func TestCacheStudioStateInvalidDeepCopy(t *testing.T) {
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"lmstudio/qwen3-coder-30b","display_name":"lmstudio/qwen3-coder-30b (lmstudio)"}]}`))
	}))
	t.Cleanup(proxySrv.Close)

	_, lookup, resolver := newTestPaths(t, map[string]string{
		"CCP_LMSTUDIO_BASE_URL": "http://127.0.0.1:1234/v1",
	})
	cache, err := modelcache.New(modelcache.Options{
		Lookup:         lookup,
		Paths:          resolver,
		ProxyBaseURL:   proxySrv.URL,
		DefaultBaseURL: "http://127.0.0.1:19999",
		Client:         newTestClient(t),
		Routes:         &route.Store{},
		Sentry:         &sentry.State{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	wantInvalid := &proxyconfig.InvalidSetting{
		Setting: "CCP_LMSTUDIO_BASE_URL",
		Reason:  "must be a scheme, a host and a port only, without a path such as /v1",
	}
	first := cache.StudioState()
	if first == nil || first.Invalid == nil {
		t.Fatalf("cache.StudioState() = %+v, want non-nil Invalid", first)
	}
	if diff := gocmp.Diff(wantInvalid, first.Invalid); diff != "" {
		t.Fatalf("cache.StudioState().Invalid mismatch (-want +got):\n%s", diff)
	}
	first.Invalid.Setting = "MUTATED_SETTING"
	first.Invalid.Reason = "mutated reason"

	second := cache.StudioState()
	if second == nil || second.Invalid == nil {
		t.Fatalf("cache.StudioState() after mutation = %+v, want non-nil Invalid", second)
	}
	if diff := gocmp.Diff(wantInvalid, second.Invalid); diff != "" {
		t.Errorf("cache.StudioState().Invalid mutated through returned copy (-want +got):\n%s", diff)
	}
}

type pubEvent struct {
	Kind     string
	Gen      uint64
	Source   string
	HasFresh bool
	HasStale bool
}

type observingSentry struct {
	state      *sentry.State
	routeStore *route.Store
	mu         *sync.Mutex
	events     *[]pubEvent
	onPublish  func(gen uint64)
}

func (o *observingSentry) Publish(generation uint64, resolver sentry.ModelResolver, windows policy.Windows, env float64) {
	if o.onPublish != nil {
		o.onPublish(generation)
	}
	hasFresh := resolver != nil && resolver.ResolveBackend("kimi-fresh") != nil
	hasStale := resolver != nil && resolver.ResolveBackend("kimi-stale") != nil
	o.mu.Lock()
	*o.events = append(*o.events, pubEvent{Kind: "sentry", Gen: generation, HasFresh: hasFresh, HasStale: hasStale})
	o.mu.Unlock()
	o.state.Publish(generation, resolver, windows, env)
}

type observingRoutes struct {
	store     *route.Store
	mu        *sync.Mutex
	events    *[]pubEvent
	onPublish func(snapshot route.Snapshot)
}

func (o *observingRoutes) Publish(snapshot route.Snapshot) {
	if o.onPublish != nil {
		o.onPublish(snapshot)
	}
	src, _ := snapshot.Catalog()
	r := snapshot.Resolver()
	hasFresh := r.ResolveBackend("kimi-fresh") != nil
	hasStale := r.ResolveBackend("kimi-stale") != nil
	o.mu.Lock()
	*o.events = append(*o.events, pubEvent{Kind: "route", Source: src, HasFresh: hasFresh, HasStale: hasStale})
	o.mu.Unlock()
	o.store.Publish(snapshot)
}

func TestRefreshPublicationOrderAndOverlappingRefreshes(t *testing.T) {
	var reqCount atomic.Int32
	reqAStarted := make(chan struct{})
	releaseA := make(chan struct{})
	var closeAOnce sync.Once
	closeA := func() { closeAOnce.Do(func() { close(releaseA) }) }

	routeBPaused := make(chan struct{})
	releaseRouteB := make(chan struct{})
	var pauseRouteBOnce sync.Once
	var closeRouteBOnce sync.Once
	closeRouteB := func() { closeRouteBOnce.Do(func() { close(releaseRouteB) }) }

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := reqCount.Add(1)
		if n == 1 {
			close(reqAStarted)
			select {
			case <-releaseA:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"kimi-stale","display_name":"kimi-stale (kimi)"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"kimi-fresh","display_name":"kimi-fresh (kimi)"}]}`))
	})

	_, lookup, resolver := newTestPaths(t, nil)
	routeStore := &route.Store{}
	sentryState := &sentry.State{}
	var evMu sync.Mutex
	var events []pubEvent
	obsSentry := &observingSentry{
		state:      sentryState,
		routeStore: routeStore,
		mu:         &evMu,
		events:     &events,
		onPublish: func(gen uint64) {
			snap, ok := routeStore.Load()
			switch gen {
			case 1:
				if ok {
					t.Errorf("route.Store already published before sentry generation 1")
				}
			case 2:
				if !ok {
					t.Errorf("route.Store missing generation 1 snapshot before sentry generation 2")
				} else if snap.Resolver().ResolveBackend("kimi-fresh") != nil {
					t.Errorf("route.Store already received kimi-fresh before sentry generation 2")
				}
			}
		},
	}
	obsRoutes := &observingRoutes{
		store:  routeStore,
		mu:     &evMu,
		events: &events,
		onPublish: func(snapshot route.Snapshot) {
			if src, _ := snapshot.Catalog(); src == "proxy" {
				pauseRouteBOnce.Do(func() {
					close(routeBPaused)
					<-releaseRouteB
				})
			}
		},
	}

	cache, err := modelcache.New(modelcache.Options{
		Lookup:         lookup,
		Paths:          resolver,
		ProxyBaseURL:   "http://127.0.0.1:19991",
		DefaultBaseURL: "http://127.0.0.1:19999",
		Client:         &http.Client{Transport: handlerTransport{handler: handler}},
		Clock:          time.Now,
		Routes:         obsRoutes,
		Sentry:         obsSentry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Startup must publish sentry generation 1 before route.Store and expose (snapshot, 1).
	snap0, gen0 := cache.Snapshot()
	if gen0 != 1 {
		t.Fatalf("startup Snapshot generation = %d, want 1", gen0)
	}
	storeSnap0, ok := routeStore.Load()
	if !ok {
		t.Fatal("route.Store empty after New")
	}
	src0, raw0 := snap0.Catalog()
	if src0 != "fallback" {
		t.Errorf("startup Cache.Snapshot source = %q, want fallback", src0)
	} else if _, rawB := storeSnap0.Catalog(); !bytes.Equal(raw0, rawB) {
		t.Error("startup Cache.Snapshot differs from route.Store.Load")
	}
	evMu.Lock()
	gotStartup := slices.Clone(events)
	evMu.Unlock()
	wantStartup := []pubEvent{
		{Kind: "sentry", Gen: 1},
		{Kind: "route", Source: "fallback"},
	}
	if diff := gocmp.Diff(wantStartup, gotStartup); diff != "" {
		t.Fatalf("startup publication order mismatch (-want +got):\n%s", diff)
	}

	// Launch slow older refresh A and wait until it is inside the HTTP call.
	errA := make(chan error, 1)
	var waitAOnce sync.Once
	var errAResult error
	waitA := func() error {
		waitAOnce.Do(func() {
			errAResult = <-errA
		})
		return errAResult
	}
	defer func() {
		closeA()
		_ = waitA()
	}()
	go func() {
		errA <- cache.Refresh(t.Context())
	}()
	select {
	case <-reqAStarted:
	case err := <-errA:
		waitAOnce.Do(func() { errAResult = err })
		t.Fatalf("slow older refresh A returned before proxy HTTP call: %v", err)
	}

	// Readers must not block while refresh A is waiting on the network.
	if _, g := cache.Snapshot(); g != 1 {
		t.Errorf("Snapshot during network call returned generation %d, want 1", g)
	}
	if g := cache.Generation(); g != 1 {
		t.Errorf("Generation during network call = %d, want 1", g)
	}
	if src, _ := cache.HealthCatalog(); src != "fallback" {
		t.Errorf("HealthCatalog during network call = %q, want fallback", src)
	}
	_ = cache.LlamaState()
	_ = cache.StudioState()
	if _, err := cache.ModelsResponse(t.Context(), http.Header{}); err != nil {
		t.Errorf("unauthenticated ModelsResponse during network call: %v", err)
	}

	// Launch newer overlapping refresh B while A is still blocked; pause B's generation-2 route publish
	// before forwarding to route.Store so a separate Snapshot reader verifies Cache does not become
	// visible before Routes.Publish. Do not call Snapshot synchronously from the callback, because a
	// valid commit mutex may block readers until commit finishes.
	errB := make(chan error, 1)
	var waitBOnce sync.Once
	var errBResult error
	waitB := func() error {
		waitBOnce.Do(func() {
			errBResult = <-errB
		})
		return errBResult
	}
	defer func() {
		closeRouteB()
		_ = waitB()
	}()
	go func() {
		errB <- cache.Refresh(t.Context())
	}()
	select {
	case <-routeBPaused:
	case err := <-errB:
		waitBOnce.Do(func() { errBResult = err })
		t.Fatalf("overlapping refresh B finished before generation-2 route publish: %v", err)
	}

	type snapRead struct {
		snap    route.Snapshot
		gen     uint64
		genOnly uint64
	}
	readerStarted := make(chan struct{})
	snapCh := make(chan snapRead, 1)
	var snapJoined atomic.Bool
	defer func() {
		closeRouteB()
		if !snapJoined.Load() {
			<-snapCh
		}
	}()
	go func() {
		close(readerStarted)
		s, g := cache.Snapshot()
		g2 := cache.Generation()
		snapCh <- snapRead{snap: s, gen: g, genOnly: g2}
	}()
	<-readerStarted

	pauseTimer := time.NewTimer(50 * time.Millisecond)
	var pausedRead *snapRead
	select {
	case r := <-snapCh:
		snapJoined.Store(true)
		pausedRead = &r
	case <-pauseTimer.C:
	}
	pauseTimer.Stop()

	if pausedRead != nil {
		srcPaused, rawPaused := pausedRead.snap.Catalog()
		hasFreshPaused := pausedRead.snap.Resolver().ResolveBackend("kimi-fresh") != nil
		if pausedRead.gen != 1 || pausedRead.genOnly != 1 || srcPaused != "fallback" || !bytes.Equal(rawPaused, raw0) || hasFreshPaused {
			t.Errorf("cache exposed snapshot/generation before Routes.Publish: got (gen=%d, Generation=%d, source=%q, hasFresh=%v), want old pair (1, 1, fallback, false)",
				pausedRead.gen, pausedRead.genOnly, srcPaused, hasFreshPaused)
		}
	}

	// Release the route publisher before joining a blocked Snapshot reader.
	closeRouteB()
	if pausedRead == nil {
		r := <-snapCh
		snapJoined.Store(true)
		srcJoined, rawJoined := r.snap.Catalog()
		hasFreshJoined := r.snap.Resolver().ResolveBackend("kimi-fresh") != nil
		validOld := r.gen == 1 && srcJoined == "fallback" && bytes.Equal(rawJoined, raw0) && !hasFreshJoined
		validNew := r.gen == 2 && srcJoined == "proxy" && hasFreshJoined
		if !validOld && !validNew {
			t.Errorf("Cache.Snapshot returned mixed pair across commit: (gen=%d, source=%q, hasFresh=%v)", r.gen, srcJoined, hasFreshJoined)
		}
		if r.genOnly != 1 && r.genOnly != 2 || r.genOnly < r.gen {
			t.Errorf("Cache.Generation across commit = %d after Snapshot gen %d", r.genOnly, r.gen)
		}
	}

	if err := waitB(); err != nil {
		t.Fatalf("overlapping refresh B: %v", err)
	}
	snapB, genB := cache.Snapshot()
	if genB != 2 {
		t.Fatalf("Snapshot generation after refresh B = %d, want 2", genB)
	}
	if d := snapB.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"claude-kimi-fresh"}`)); d.Branch != "proxy" || d.Provider != "kimi" {
		t.Errorf("refresh B route decision for claude-kimi-fresh = %+v, want proxy/kimi", d)
	}

	// Unblock slow older refresh A; it must not overwrite refresh B's newer snapshot or sentry policy.
	closeA()
	if err := waitA(); err != nil {
		t.Fatalf("slow older refresh A: %v", err)
	}

	snapAfter, genAfter := cache.Snapshot()
	if genAfter != 2 {
		t.Errorf("Snapshot generation after slow older refresh A = %d, want 2 (no stale overwrite)", genAfter)
	}
	storeAfter, ok := routeStore.Load()
	if !ok {
		t.Fatal("route.Store empty after overlapping refreshes")
	}
	if d := storeAfter.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"claude-kimi-fresh"}`)); d.Branch != "proxy" || d.Provider != "kimi" {
		t.Errorf("route.Store lost kimi-fresh after slow older refresh A: %+v", d)
	}
	if d := storeAfter.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"claude-kimi-stale"}`)); d.Branch == "proxy" && d.Provider == "kimi" {
		t.Errorf("slow older refresh A overwrote route.Store with kimi-stale: %+v", d)
	}
	if _, rawSnap := snapAfter.Catalog(); true {
		if _, rawStore := storeAfter.Catalog(); !bytes.Equal(rawSnap, rawStore) {
			t.Error("Cache.Snapshot diverges from route.Store after overlapping refreshes")
		}
	}

	evMu.Lock()
	gotEvents := slices.Clone(events)
	evMu.Unlock()
	wantEvents := []pubEvent{
		{Kind: "sentry", Gen: 1},
		{Kind: "route", Source: "fallback"},
		{Kind: "sentry", Gen: 2, HasFresh: true},
		{Kind: "route", Source: "proxy", HasFresh: true},
	}
	if diff := gocmp.Diff(wantEvents, gotEvents); diff != "" {
		t.Errorf("publication event order mismatch (-want +got):\n%s", diff)
	}
}

func TestRefreshFallbackAndModelsJSON(t *testing.T) {
	t.Run("success: models.json used when proxy is down before first success", func(t *testing.T) {
		home, lookup, resolver := newTestPaths(t, nil)
		stateDir := filepath.Join(home, ".claude", "model-gateway")
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			t.Fatalf("mkdir state dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "models.json"), []byte(`["gpt-6.1-sol","not-codex"]`), 0o600); err != nil {
			t.Fatalf("write models.json: %v", err)
		}
		routes := &route.Store{}
		recSentry := &recordingSentry{state: &sentry.State{}}
		cache, err := modelcache.New(modelcache.Options{
			Lookup:         lookup,
			Paths:          resolver,
			ProxyBaseURL:   "http://proxy.invalid",
			DefaultBaseURL: "http://127.0.0.1:19999",
			Client:         newFailingClient(),
			Routes:         routes,
			Sentry:         recSentry,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := cache.Refresh(t.Context()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if _, gen := cache.Snapshot(); gen != 2 {
			t.Errorf("Snapshot generation = %d, want 2", gen)
		}
		if diff := gocmp.Diff([]uint64{1, 2}, recSentry.generations()); diff != "" {
			t.Errorf("sentry generations mismatch (-want +got):\n%s", diff)
		}
		source, _ := cache.HealthCatalog()
		if source != "models.json" {
			t.Errorf("HealthCatalog source = %q, want %q", source, "models.json")
		}
		snap, ok := routes.Load()
		if !ok {
			t.Fatal("route.Store empty after models.json Refresh")
		}
		_, raw := snap.Catalog()
		var rows []modelRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatalf("unmarshal catalog: %v", err)
		}
		wantIDs := []string{"claude-gpt-6.1-sol[1m]", "claude-grok-4.7[1m]", "claude-grok-4.6[1m]", "claude-grok-4.5[1m]"}
		gotIDs := make([]string, 0, len(rows))
		for _, r := range rows {
			gotIDs = append(gotIDs, r.ID)
		}
		if diff := gocmp.Diff(wantIDs, gotIDs); diff != "" {
			t.Errorf("models.json fallback IDs mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("success: failed refresh after proxy success retains proxy routes and source", func(t *testing.T) {
		var fail atomic.Bool
		proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if fail.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`))
		}))
		t.Cleanup(proxySrv.Close)

		_, lookup, resolver := newTestPaths(t, nil)
		routes := &route.Store{}
		recSentry := &recordingSentry{state: &sentry.State{}}
		cache, err := modelcache.New(modelcache.Options{
			Lookup:         lookup,
			Paths:          resolver,
			ProxyBaseURL:   proxySrv.URL,
			DefaultBaseURL: "http://127.0.0.1:19999",
			Client:         newTestClient(t),
			Routes:         routes,
			Sentry:         recSentry,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := cache.Refresh(t.Context()); err != nil {
			t.Fatalf("initial Refresh: %v", err)
		}
		fail.Store(true)
		if err := cache.Refresh(t.Context()); err != nil {
			t.Fatalf("outage Refresh: %v", err)
		}
		if _, gen := cache.Snapshot(); gen != 3 {
			t.Errorf("Snapshot generation after outage refresh = %d, want 3", gen)
		}
		if diff := gocmp.Diff([]uint64{1, 2, 3}, recSentry.generations()); diff != "" {
			t.Errorf("sentry generations after outage refresh mismatch (-want +got):\n%s", diff)
		}
		source, _ := cache.HealthCatalog()
		if source != "proxy" {
			t.Errorf("source after outage = %q, want preserved %q", source, "proxy")
		}
		snap, ok := routes.Load()
		if !ok {
			t.Fatal("route.Store empty after outage Refresh")
		}
		if snapSrc, _ := snap.Catalog(); snapSrc != "proxy" {
			t.Errorf("route.Store snapshot source = %q, want %q", snapSrc, "proxy")
		}
		if d := snap.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"claude-kimi-k3"}`)); d.Branch != "proxy" || d.Provider != "kimi" {
			t.Errorf("retained route decision = %+v, want proxy/kimi", d)
		}
	})
}

func TestRefreshScheduleAndRetryBackoff(t *testing.T) {
	t.Run("success: default 60s periodic refresh on healthy startup", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var mu sync.Mutex
			var hitTimes []time.Time
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				mu.Lock()
				hitTimes = append(hitTimes, time.Now())
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
			})

			// Do not set CODEX_GATEWAY_MODELS_REFRESH_MS so the 60s default itself is exercised.
			_, lookup, resolver := newTestPaths(t, nil)
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   "http://127.0.0.1:19991",
				DefaultBaseURL: "http://127.0.0.1:19999",
				Client:         &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:          time.Now,
				Routes:         &route.Store{},
				Sentry:         &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer func() {
				cancel()
				cache.Wait()
			}()
			start := time.Now()
			cache.Start(ctx)
			synctest.Wait()

			hits := func() []time.Time {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(hitTimes)
			}
			if got := len(hits()); got != 1 {
				t.Fatalf("initial refresh hits = %d, want 1", got)
			}

			time.Sleep(59*time.Second + 999*time.Millisecond)
			synctest.Wait()
			if got := len(hits()); got != 1 {
				t.Fatalf("refresh fired early at 59.999s: hits = %d, want 1", got)
			}

			time.Sleep(time.Millisecond)
			synctest.Wait()
			if got := len(hits()); got != 2 {
				t.Fatalf("hits at 60s = %d, want 2", got)
			}

			time.Sleep(59*time.Second + 999*time.Millisecond)
			synctest.Wait()
			if got := len(hits()); got != 2 {
				t.Fatalf("second periodic refresh fired early at 119.999s: hits = %d, want 2", got)
			}

			time.Sleep(time.Millisecond)
			synctest.Wait()
			gotTimes := hits()
			if len(gotTimes) != 3 {
				t.Fatalf("hits at 120s = %d, want 3", len(gotTimes))
			}
			gotOffsets := []time.Duration{
				gotTimes[0].Sub(start),
				gotTimes[1].Sub(start),
				gotTimes[2].Sub(start),
			}
			wantOffsets := []time.Duration{0, 60 * time.Second, 120 * time.Second}
			if diff := gocmp.Diff(wantOffsets, gotOffsets); diff != "" {
				t.Errorf("periodic refresh offsets mismatch (-want +got):\n%s", diff)
			}
		})
	})

	t.Run("success: exponential retry backoff caps at 30s and resets to 1s after recovery", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var mu sync.Mutex
			var hitTimes []time.Time
			var forceFail atomic.Bool
			var attempts atomic.Int32

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				n := int(attempts.Add(1))
				mu.Lock()
				hitTimes = append(hitTimes, time.Now())
				mu.Unlock()
				if n <= 8 || forceFail.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
			})

			// Use a 600s periodic interval so the retry cap and reset are observed without periodic interference.
			_, lookup, resolver := newTestPaths(t, map[string]string{
				"CODEX_GATEWAY_MODELS_REFRESH_MS": "600000",
			})
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   "http://127.0.0.1:19991",
				DefaultBaseURL: "http://127.0.0.1:19999",
				Client:         &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:          time.Now,
				Routes:         &route.Store{},
				Sentry:         &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer func() {
				cancel()
				cache.Wait()
			}()
			cache.Start(ctx)
			synctest.Wait()

			// Observe 8 consecutive failures (initial + 7 retries: 1s, 2s, 4s, 8s, 16s, 30s, 30s), then 9th succeeds at +30s.
			expectedDelays := []time.Duration{
				1 * time.Second,
				2 * time.Second,
				4 * time.Second,
				8 * time.Second,
				16 * time.Second,
				30 * time.Second,
				30 * time.Second,
				30 * time.Second,
			}
			for _, d := range expectedDelays {
				time.Sleep(d)
				synctest.Wait()
			}

			mu.Lock()
			gotPhase1 := slices.Clone(hitTimes)
			mu.Unlock()
			if len(gotPhase1) != 9 {
				t.Fatalf("recorded %d hits through recovery at 121s, want 9", len(gotPhase1))
			}
			gotIntervals := make([]time.Duration, len(expectedDelays))
			for i := range expectedDelays {
				gotIntervals[i] = gotPhase1[i+1].Sub(gotPhase1[i])
			}
			if diff := gocmp.Diff(expectedDelays, gotIntervals); diff != "" {
				t.Errorf("retry backoff intervals mismatch (-want +got):\n%s", diff)
			}
			if src, _ := cache.HealthCatalog(); src != "proxy" {
				t.Fatalf("source after recovery = %q, want %q", src, "proxy")
			}

			// Confirm no retry timer remains scheduled after success.
			time.Sleep(30 * time.Second)
			synctest.Wait()
			mu.Lock()
			hitsAfterQuiet := len(hitTimes)
			mu.Unlock()
			if hitsAfterQuiet != 9 {
				t.Fatalf("unexpected retry fired after recovery: hits = %d, want 9", hitsAfterQuiet)
			}

			// Flip back to failing and advance to the 600s periodic refresh (449s after t=151s).
			forceFail.Store(true)
			time.Sleep(449 * time.Second)
			synctest.Wait()

			// After the periodic failure at t=600s, the retry backoff must have reset to 1s then 2s.
			time.Sleep(1 * time.Second)
			synctest.Wait()
			time.Sleep(2 * time.Second)
			synctest.Wait()

			mu.Lock()
			gotPhase2 := slices.Clone(hitTimes)
			mu.Unlock()
			if len(gotPhase2) != 12 {
				t.Fatalf("hits after second outage (600s, 601s, 603s) = %d, want 12", len(gotPhase2))
			}
			resetIntervals := []time.Duration{
				gotPhase2[10].Sub(gotPhase2[9]),
				gotPhase2[11].Sub(gotPhase2[10]),
			}
			wantReset := []time.Duration{1 * time.Second, 2 * time.Second}
			if diff := gocmp.Diff(wantReset, resetIntervals); diff != "" {
				t.Errorf("post-recovery retry reset intervals mismatch (-want +got):\n%s", diff)
			}
		})
	})

	t.Run("error: proxy listing timeout bounds Refresh at 2.5s and schedules retry", func(t *testing.T) {
		if modelcache.ProxyListingTimeout != 2500*time.Millisecond {
			t.Fatalf("ProxyListingTimeout = %v, want 2.5s", modelcache.ProxyListingTimeout)
		}
		synctest.Test(t, func(t *testing.T) {
			var attempts atomic.Int32
			var req1Done atomic.Bool
			var req1CtxErr atomic.Pointer[error]
			var req1EndMu sync.Mutex
			var req1End time.Time
			releaseReq1 := make(chan struct{})
			var closeReq1Once sync.Once
			closeReq1 := func() { closeReq1Once.Do(func() { close(releaseReq1) }) }

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				n := attempts.Add(1)
				if n == 1 {
					select {
					case <-r.Context().Done():
						err := r.Context().Err()
						req1CtxErr.Store(&err)
						req1EndMu.Lock()
						req1End = time.Now()
						req1EndMu.Unlock()
						req1Done.Store(true)
					case <-releaseReq1:
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
			})

			_, lookup, resolver := newTestPaths(t, map[string]string{
				"CODEX_GATEWAY_MODELS_REFRESH_MS": "600000",
			})
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   "http://127.0.0.1:19991",
				DefaultBaseURL: "http://127.0.0.1:19999",
				Client:         &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:          time.Now,
				Routes:         &route.Store{},
				Sentry:         &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer func() {
				closeReq1()
				cancel()
				cache.Wait()
			}()
			start := time.Now()
			cache.Start(ctx)
			synctest.Wait()

			if got := attempts.Load(); got != 1 {
				t.Fatalf("initial proxy attempts = %d, want 1", got)
			}

			time.Sleep(2499 * time.Millisecond)
			synctest.Wait()
			if req1Done.Load() {
				t.Fatal("proxy listing request timed out early at 2499ms")
			}

			time.Sleep(time.Millisecond)
			synctest.Wait()
			if !req1Done.Load() {
				t.Fatal("proxy listing request did not time out at 2500ms")
			}
			if errPtr := req1CtxErr.Load(); errPtr == nil || !errors.Is(*errPtr, context.DeadlineExceeded) {
				t.Errorf("proxy listing timeout ctx.Err() = %v, want context.DeadlineExceeded", errPtr)
			}
			req1EndMu.Lock()
			elapsed := req1End.Sub(start)
			req1EndMu.Unlock()
			if elapsed != 2500*time.Millisecond {
				t.Errorf("proxy listing timeout elapsed = %v, want 2.5s", elapsed)
			}
			if src, _ := cache.HealthCatalog(); src != "fallback" {
				t.Errorf("HealthCatalog after proxy timeout = %q, want fallback", src)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("retry fired immediately after proxy timeout: attempts = %d, want 1", got)
			}

			time.Sleep(999 * time.Millisecond)
			synctest.Wait()
			if got := attempts.Load(); got != 1 {
				t.Fatalf("retry fired early at +999ms: attempts = %d, want 1", got)
			}

			time.Sleep(time.Millisecond)
			synctest.Wait()
			if got := attempts.Load(); got != 2 {
				t.Fatalf("retry did not fire at +1s after proxy timeout: attempts = %d, want 2", got)
			}
			if src, _ := cache.HealthCatalog(); src != "proxy" {
				t.Errorf("HealthCatalog after post-timeout recovery = %q, want proxy", src)
			}
		})
	})

	t.Run("success: HTTP 200 empty listing retains last usable proxy route and schedules retry", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var mu sync.Mutex
			var hitTimes []time.Time
			var attempts atomic.Int32

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				n := attempts.Add(1)
				mu.Lock()
				hitTimes = append(hitTimes, time.Now())
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch n {
				case 1:
					_, _ = w.Write([]byte(`{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`))
				case 2, 3:
					_, _ = w.Write([]byte(`{"data":[]}`))
				default:
					_, _ = w.Write([]byte(`{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
				}
			})

			_, lookup, resolver := newTestPaths(t, map[string]string{
				"CODEX_GATEWAY_MODELS_REFRESH_MS": "600000",
			})
			routes := &route.Store{}
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   "http://127.0.0.1:19991",
				DefaultBaseURL: "http://127.0.0.1:19999",
				Client:         &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:          time.Now,
				Routes:         routes,
				Sentry:         &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer func() {
				cancel()
				cache.Wait()
			}()
			cache.Start(ctx)
			synctest.Wait()

			if got := attempts.Load(); got != 1 {
				t.Fatalf("initial attempts = %d, want 1", got)
			}
			if src, _ := cache.HealthCatalog(); src != "proxy" {
				t.Fatalf("initial HealthCatalog source = %q, want proxy", src)
			}

			// Advance to the 600s periodic refresh, which receives HTTP 200 with an empty data array.
			time.Sleep(600 * time.Second)
			synctest.Wait()
			if got := attempts.Load(); got != 2 {
				t.Fatalf("attempts at 600s periodic refresh = %d, want 2", got)
			}
			if src, _ := cache.HealthCatalog(); src != "proxy" {
				t.Errorf("HealthCatalog source after 200-empty = %q, want retained proxy", src)
			}
			snapEmpty1, ok := routes.Load()
			if !ok {
				t.Fatal("route.Store empty after 200-empty refresh")
			}
			if d := snapEmpty1.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"claude-kimi-k3"}`)); d.Branch != "proxy" || d.Provider != "kimi" {
				t.Errorf("route after first 200-empty = %+v, want retained proxy/kimi", d)
			}

			// First retry must fire at +1s (also 200-empty) while continuing to retain the last usable route.
			time.Sleep(1 * time.Second)
			synctest.Wait()
			if got := attempts.Load(); got != 3 {
				t.Fatalf("attempts at +1s retry after 200-empty = %d, want 3", got)
			}
			if src, _ := cache.HealthCatalog(); src != "proxy" {
				t.Errorf("HealthCatalog source after second 200-empty = %q, want retained proxy", src)
			}
			snapEmpty2, ok := routes.Load()
			if !ok {
				t.Fatal("route.Store empty after second 200-empty refresh")
			}
			if d := snapEmpty2.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"claude-kimi-k3"}`)); d.Branch != "proxy" || d.Provider != "kimi" {
				t.Errorf("route after second 200-empty = %+v, want retained proxy/kimi", d)
			}

			// Second retry must fire at +2s and recover the non-empty listing.
			time.Sleep(2 * time.Second)
			synctest.Wait()
			if got := attempts.Load(); got != 4 {
				t.Fatalf("attempts at +2s retry after 200-empty = %d, want 4", got)
			}
			snapRecovered, ok := routes.Load()
			if !ok {
				t.Fatal("route.Store empty after recovery")
			}
			if d := snapRecovered.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"claude-gpt-6-astra[1m]"}`)); d.Branch != "proxy" || d.Provider != "codex" {
				t.Errorf("recovered route for claude-gpt-6-astra[1m] = %+v, want proxy/codex", d)
			}

			// No retry timer should remain scheduled after recovery.
			time.Sleep(30 * time.Second)
			synctest.Wait()
			if got := attempts.Load(); got != 4 {
				t.Errorf("unexpected retry after 200-empty recovery: attempts = %d, want 4", got)
			}
			mu.Lock()
			gotTimes := slices.Clone(hitTimes)
			mu.Unlock()
			if len(gotTimes) != 4 {
				t.Fatalf("recorded hit times = %d, want 4", len(gotTimes))
			}
			gotRetryGaps := []time.Duration{
				gotTimes[2].Sub(gotTimes[1]),
				gotTimes[3].Sub(gotTimes[2]),
			}
			wantRetryGaps := []time.Duration{1 * time.Second, 2 * time.Second}
			if diff := gocmp.Diff(wantRetryGaps, gotRetryGaps); diff != "" {
				t.Errorf("200-empty retry gaps mismatch (-want +got):\n%s", diff)
			}
		})
	})
}

func TestAnthropicCatalogAndModelsEndpoint(t *testing.T) {
	t.Run("success: unauthenticated skips upstream; authenticated paginates, filters headers, handles collisions and raw ids, and confines secrets", func(t *testing.T) {
		var mu sync.Mutex
		var seenHeaders []http.Header
		var seenQueries []string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assertNoRequestMarkers(t, "Anthropic upstream", r, map[string]string{
				"studio token":      studioSecretMarker,
				"disallowed cookie": disallowedCookiePayload,
				"disallowed header": disallowedHeaderMarker,
			})
			if r.URL.Path != "/v1/models" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			mu.Lock()
			seenHeaders = append(seenHeaders, r.Header.Clone())
			seenQueries = append(seenQueries, r.URL.RawQuery)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("after_id") == "" {
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-fable-5-0","display_name":"Fable 5.0"},{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":true,"last_id":"claude-opus-5-5"}`))
				return
			}
			// Page 2 carries claude-kimi-k3, which collides with the proxy kimi-k3 row.
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5-5","display_name":"Sonnet 5.5"},{"id":"claude-kimi-k3","display_name":"Anthropic Kimi"}],"has_more":false,"last_id":"claude-kimi-k3"}`))
		}))
		t.Cleanup(upstream.Close)

		var llamaHits atomic.Int32
		llamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			llamaHits.Add(1)
			assertNoRequestMarkers(t, "llama-server", r, allCredentialMarkers)
			if r.URL.Path != "/props" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":65536}}`))
		}))
		t.Cleanup(llamaSrv.Close)
		llamaPort := llamaSrv.Listener.Addr().(*net.TCPAddr).Port

		var studioHits atomic.Int32
		studioSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			studioHits.Add(1)
			assertNoRequestMarkers(t, "LM Studio", r, map[string]string{
				"authorization":     authBearerPayload,
				"x-api-key":         apiKeyMarker,
				"disallowed cookie": disallowedCookiePayload,
				"disallowed header": disallowedHeaderMarker,
			})
			if r.URL.Path != "/api/v1/models" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+studioSecretMarker {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"type":"llm","key":"qwen3-coder-30b","loaded_instances":[{"id":"qwen3-coder-30b","config":{"context_length":65536}}]}]}`))
		}))
		t.Cleanup(studioSrv.Close)

		var proxyHits atomic.Int32
		proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxyHits.Add(1)
			assertNoRequestMarkers(t, "proxy /v1/models", r, allCredentialMarkers)
			if r.URL.Path != "/v1/models" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"data":[` +
					`{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"},` +
					`{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},` +
					`{"id":"�-1","display_name":"�-1 (kimi)"},` +
					`{"id":"local/gguf","display_name":"local/gguf (local)"},` +
					`{"id":"lmstudio/qwen3-coder-30b","display_name":"lmstudio/qwen3-coder-30b (lmstudio)"}` +
					`]}`,
			))
		}))
		t.Cleanup(proxySrv.Close)

		home, lookup, resolver := newTestPaths(t, map[string]string{
			"CCP_LLAMA_PORT":        strconv.Itoa(llamaPort),
			"CCP_LMSTUDIO_BASE_URL": studioSrv.URL,
			"CCP_LMSTUDIO_API_KEY":  studioSecretMarker,
		})
		// Seed Claude global config with a raw malformed byte entry beside the valid older Fable entry
		// so only callers passing raw additionalModelOptionsCache bytes avoid relabelling claude-�-1.
		claudeDir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(claudeDir, 0o700); err != nil {
			t.Fatalf("mkdir .claude: %v", err)
		}
		globalCfg := append([]byte(`{"additionalModelOptionsCache":[{"value":"claude-`), 0xff)
		globalCfg = append(globalCfg, []byte(`-1","label":"Bad"},{"value":"claude-fable-5-0","label":"Fable"}]}`)...)
		if err := os.WriteFile(filepath.Join(claudeDir, ".claude.json"), globalCfg, 0o600); err != nil {
			t.Fatalf("write .claude.json: %v", err)
		}

		now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
		var logs logCapture
		cache, err := modelcache.New(modelcache.Options{
			Lookup:           lookup,
			Paths:            resolver,
			ProxyBaseURL:     proxySrv.URL,
			AnthropicBaseURL: upstream.URL,
			DefaultBaseURL:   "http://127.0.0.1:19999",
			Client:           newTestClient(t),
			Clock:            func() time.Time { return now },
			Routes:           &route.Store{},
			Sentry:           &sentry.State{},
			EffectiveBaseURL: baseURLFunc("http://127.0.0.1:19999"),
			CurrentPins:      pinsFunc(map[string]string{"�": "claude-�-2", "fable": "claude-fable-5-1[1m]"}),
			Logf:             logs.logf,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := cache.Refresh(t.Context()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}

		// Unauthenticated request must never contact upstream.
		if _, err := cache.ModelsResponse(t.Context(), http.Header{}); err != nil {
			t.Fatalf("unauthenticated ModelsResponse: %v", err)
		}
		mu.Lock()
		hitsBefore := len(seenHeaders)
		mu.Unlock()
		if hitsBefore != 0 {
			t.Fatalf("unauthenticated /v1/models sent %d upstream requests, want 0", hitsBefore)
		}

		// Authenticated request with allowed and disallowed headers; omitting Anthropic-Version checks the default.
		reqHeaders := http.Header{
			"X-Api-Key":              {apiKeyMarker},
			"Authorization":          {authBearerHeader},
			"Anthropic-Beta":         {"prompt-caching-2024-07-31"},
			"Anthropic-Workspace-Id": {"ws-test-123"},
			"User-Agent":             {"claude-code/2.1"},
			"Cookie":                 {disallowedCookieHeader},
			"X-Custom-Secret":        {disallowedHeaderMarker},
		}
		rawResp, err := cache.ModelsResponse(t.Context(), reqHeaders)
		if err != nil {
			t.Fatalf("authenticated ModelsResponse: %s", redactTestMarkers(err.Error()))
		}

		mu.Lock()
		gotHeaders := slices.Clone(seenHeaders)
		gotQueries := slices.Clone(seenQueries)
		mu.Unlock()
		wantQueries := []string{"limit=1000", "limit=1000&after_id=claude-opus-5-5"}
		if diff := gocmp.Diff(wantQueries, gotQueries); diff != "" {
			t.Errorf("upstream catalog pagination queries mismatch (-want +got):\n%s", diff)
		}
		if len(gotHeaders) != 2 {
			t.Fatalf("authenticated request made %d upstream page requests, want 2", len(gotHeaders))
		}
		for i, h := range gotHeaders {
			if h.Get("X-Api-Key") != apiKeyMarker ||
				h.Get("Authorization") != authBearerHeader ||
				h.Get("Anthropic-Version") != "2023-06-01" ||
				h.Get("Anthropic-Beta") != "prompt-caching-2024-07-31" ||
				h.Get("Anthropic-Workspace-Id") != "ws-test-123" ||
				h.Get("User-Agent") != "claude-code/2.1" {
				t.Errorf("page %d missing or wrong allowed headers", i+1)
			}
			if _, ok := h["Cookie"]; ok {
				t.Errorf("page %d forwarded disallowed Cookie header", i+1)
			}
			if _, ok := h["X-Custom-Secret"]; ok {
				t.Errorf("page %d forwarded disallowed X-Custom-Secret header", i+1)
			}
			assertNoHeaderMarkers(t, fmt.Sprintf("Anthropic page %d", i+1), h, map[string]string{
				"studio token":      studioSecretMarker,
				"disallowed cookie": disallowedCookiePayload,
				"disallowed header": disallowedHeaderMarker,
			})
		}

		// Require an explicit JSON boolean false "has_more" field (not null) and full ordered rows including both colliding rows.
		var rawEnvelope map[string]jsontext.Value
		if err := json.Unmarshal(rawResp, &rawEnvelope); err != nil {
			t.Fatalf("unmarshal /v1/models envelope: %s", redactTestMarkers(err.Error()))
		}
		rawHasMore, ok := rawEnvelope["has_more"]
		if !ok {
			t.Fatal("/v1/models response omitted explicit has_more field")
		}
		if rawHasMore.Kind() != 'f' {
			t.Errorf("/v1/models has_more JSON token kind = %q, want 'f' (explicit boolean false)", rawHasMore.Kind())
		}
		var gotRespRows []idDisplay
		if err := json.Unmarshal(rawEnvelope["data"], &gotRespRows); err != nil {
			t.Fatalf("unmarshal /v1/models data: %s", redactTestMarkers(err.Error()))
		}
		wantHTTP := []idDisplay{
			{ID: "claude-fable-5-0", DisplayName: "Fable 5.0 (previous)"},
			{ID: "claude-opus-5-5", DisplayName: "Opus 5.5"},
			{ID: "claude-sonnet-5-5", DisplayName: "Sonnet 5.5"},
			{ID: "claude-kimi-k3", DisplayName: "Anthropic Kimi"},
			{ID: "claude-gpt-6-astra[1m]", DisplayName: "GPT-6-astra (Codex)"},
			{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7"},
			{ID: "claude-grok-4.6[1m]", DisplayName: "Grok 4.6"},
			{ID: "claude-grok-4.5[1m]", DisplayName: "Grok 4.5"},
			{ID: "claude-kimi-k3", DisplayName: "kimi-k3 (Kimi)"},
			{ID: "claude-�-1", DisplayName: "�-1 (Kimi)"},
			{ID: "claude-local/gguf", DisplayName: "local/gguf (Local GGUF)"},
			{ID: "claude-lmstudio/qwen3-coder-30b", DisplayName: "lmstudio/qwen3-coder-30b (LM Studio)"},
		}
		if diff := gocmp.Diff(wantHTTP, gotRespRows); diff != "" {
			t.Errorf("GET /v1/models rows mismatch (-want +got):\n%s", redactTestMarkers(diff))
		}

		// Discovery cache prefers the cached Anthropic row on collision and carries no extra top-level fields.
		discoveryPath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
		discoveryRaw, err := os.ReadFile(discoveryPath)
		if err != nil {
			t.Fatalf("read gateway-models.json: %v", err)
		}
		var discoveryKeys map[string]jsontext.Value
		if err := json.Unmarshal(discoveryRaw, &discoveryKeys); err != nil {
			t.Fatalf("unmarshal gateway-models.json keys: %s", redactTestMarkers(err.Error()))
		}
		gotKeys := slices.Sorted(maps.Keys(discoveryKeys))
		if diff := gocmp.Diff([]string{"baseUrl", "fetchedAt", "models"}, gotKeys); diff != "" {
			t.Errorf("gateway-models.json keys mismatch (-want +got):\n%s", diff)
		}
		var discoveryModels []idDisplay
		if err := json.Unmarshal(discoveryKeys["models"], &discoveryModels); err != nil {
			t.Fatalf("unmarshal gateway-models.json models: %s", redactTestMarkers(err.Error()))
		}
		wantDiscovery := []idDisplay{
			{ID: "claude-fable-5-0", DisplayName: "Fable 5.0 (previous)"},
			{ID: "claude-opus-5-5", DisplayName: "Opus 5.5"},
			{ID: "claude-sonnet-5-5", DisplayName: "Sonnet 5.5"},
			{ID: "claude-kimi-k3", DisplayName: "Anthropic Kimi"},
			{ID: "claude-gpt-6-astra[1m]", DisplayName: "GPT-6-astra (Codex)"},
			{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7"},
			{ID: "claude-grok-4.6[1m]", DisplayName: "Grok 4.6"},
			{ID: "claude-grok-4.5[1m]", DisplayName: "Grok 4.5"},
			{ID: "claude-�-1", DisplayName: "�-1 (Kimi)"},
			{ID: "claude-local/gguf", DisplayName: "local/gguf (Local GGUF)"},
			{ID: "claude-lmstudio/qwen3-coder-30b", DisplayName: "lmstudio/qwen3-coder-30b (LM Studio)"},
		}
		if diff := gocmp.Diff(wantDiscovery, discoveryModels); diff != "" {
			t.Errorf("gateway-models.json models mismatch (-want +got):\n%s", redactTestMarkers(diff))
		}

		cacheFile := filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json")
		cachedBytes, err := os.ReadFile(cacheFile)
		if err != nil {
			t.Fatalf("read anthropic-models.json: %v", err)
		}
		info, err := os.Stat(cacheFile)
		if err != nil {
			t.Fatalf("stat anthropic-models.json: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("anthropic-models.json mode = %#o, want 0600", perm)
		}
		assertNoMarkers(t, "GET /v1/models response", rawResp, allCredentialMarkers)
		assertNoMarkers(t, "anthropic-models.json", cachedBytes, allCredentialMarkers)
		assertNoMarkers(t, "gateway-models.json", discoveryRaw, allCredentialMarkers)
		assertNoMarkers(t, "cache logs", logs.bytes(), allCredentialMarkers)

		// Within 1 hour TTL, another call must not hit upstream again and must not leak markers.
		freshResp, err := cache.ModelsResponse(t.Context(), reqHeaders)
		if err != nil {
			t.Fatalf("cached ModelsResponse: %s", redactTestMarkers(err.Error()))
		}
		assertNoMarkers(t, "fresh-cache GET /v1/models response", freshResp, allCredentialMarkers)
		mu.Lock()
		hitsAfter := len(seenHeaders)
		mu.Unlock()
		if hitsAfter != 2 {
			t.Errorf("fresh cache triggered extra upstream calls: total=%d, want 2", hitsAfter)
		}

		// Run a subsequent local refresh after the authenticated request with all three local recipients enabled
		// to prove incoming credentials are never retained and sent to proxy, LM Studio, or llama-server.
		proxyBefore, studioBefore, llamaBefore := proxyHits.Load(), studioHits.Load(), llamaHits.Load()
		if err := cache.Refresh(t.Context()); err != nil {
			t.Fatalf("post-auth Refresh: %s", redactTestMarkers(err.Error()))
		}
		if proxyHits.Load() <= proxyBefore || studioHits.Load() <= studioBefore || llamaHits.Load() <= llamaBefore {
			t.Errorf("post-auth Refresh did not contact all local recipients: proxy=%d->%d studio=%d->%d llama=%d->%d",
				proxyBefore, proxyHits.Load(), studioBefore, studioHits.Load(), llamaBefore, llamaHits.Load())
		}
		if postDiscovery, err := os.ReadFile(discoveryPath); err != nil {
			t.Fatalf("read gateway-models.json after post-auth Refresh: %v", err)
		} else {
			assertNoMarkers(t, "post-auth gateway-models.json", postDiscovery, allCredentialMarkers)
		}
		if postCached, err := os.ReadFile(cacheFile); err != nil {
			t.Fatalf("read anthropic-models.json after post-auth Refresh: %v", err)
		} else {
			assertNoMarkers(t, "post-auth anthropic-models.json", postCached, allCredentialMarkers)
		}
		assertNoMarkers(t, "post-auth cache logs", logs.bytes(), allCredentialMarkers)
	})

	t.Run("success: authorization-only refresh forwards explicit version, workspace, and user-agent", func(t *testing.T) {
		var mu sync.Mutex
		var seen []http.Header
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assertNoRequestMarkers(t, "Anthropic auth-only upstream", r, map[string]string{
				"studio token":      studioSecretMarker,
				"x-api-key":         apiKeyMarker,
				"disallowed cookie": disallowedCookiePayload,
				"disallowed header": disallowedHeaderMarker,
			})
			if r.URL.Path != "/v1/models" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			mu.Lock()
			seen = append(seen, r.Header.Clone())
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":false}`))
		}))
		t.Cleanup(upstream.Close)

		home, lookup, resolver := newTestPaths(t, nil)
		var logs logCapture
		cache, err := modelcache.New(modelcache.Options{
			Lookup:           lookup,
			Paths:            resolver,
			ProxyBaseURL:     "http://proxy.invalid",
			AnthropicBaseURL: upstream.URL,
			DefaultBaseURL:   "http://127.0.0.1:19999",
			Client:           newTestClient(t),
			Clock:            func() time.Time { return time.UnixMilli(6000000) },
			Routes:           &route.Store{},
			Sentry:           &sentry.State{},
			Logf:             logs.logf,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		headers := http.Header{
			"Authorization":          {authBearerHeader},
			"Anthropic-Version":      {"2025-01-01"},
			"Anthropic-Workspace-Id": {"ws-auth-only"},
			"User-Agent":             {"custom-agent/9.9"},
			"Cookie":                 {disallowedCookieHeader},
			"X-Custom-Secret":        {disallowedHeaderMarker},
		}
		rawResp, err := cache.ModelsResponse(t.Context(), headers)
		if err != nil {
			t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
		}
		mu.Lock()
		got := slices.Clone(seen)
		mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("authorization-only refresh sent %d upstream requests, want 1", len(got))
		}
		h := got[0]
		if h.Get("Authorization") != authBearerHeader ||
			h.Get("Anthropic-Version") != "2025-01-01" ||
			h.Get("Anthropic-Workspace-Id") != "ws-auth-only" ||
			h.Get("User-Agent") != "custom-agent/9.9" {
			t.Error("authorization-only refresh did not forward expected allowed headers")
		}
		if _, ok := h["X-Api-Key"]; ok {
			t.Error("authorization-only refresh sent unexpected X-Api-Key header")
		}
		if _, ok := h["Cookie"]; ok {
			t.Error("authorization-only refresh forwarded disallowed Cookie header")
		}
		if _, ok := h["X-Custom-Secret"]; ok {
			t.Error("authorization-only refresh forwarded disallowed X-Custom-Secret header")
		}
		cachedBytes, err := os.ReadFile(filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json"))
		if err != nil {
			t.Fatalf("read anthropic-models.json: %v", err)
		}
		assertNoMarkers(t, "authorization-only response", rawResp, allCredentialMarkers)
		assertNoMarkers(t, "authorization-only cache file", cachedBytes, allCredentialMarkers)
		assertNoMarkers(t, "authorization-only logs", logs.bytes(), allCredentialMarkers)
	})

	t.Run("success: UTF-16 code-unit page size boundary", func(t *testing.T) {
		const maxUTF16Units = 2 * 1024 * 1024
		const prefix = `{"data":[{"id":"claude-opus-5-5","display_name":"`
		const suffix = `"}],"has_more":false}`
		const overhead = len(prefix) + len(suffix)

		tests := map[string]struct {
			body      string
			wantError string
		}{
			"success: exact 2097152 ASCII UTF-16 code units": {
				body: prefix + strings.Repeat("a", maxUTF16Units-overhead) + suffix,
			},
			"success: multibyte CJK page over 2 MiB bytes but under 2097152 UTF-16 code units": {
				// 900,000 CJK characters = 2,700,000 UTF-8 bytes (> 2 MiB) and 900,000 UTF-16 code units.
				body: prefix + strings.Repeat("漢", 900000) + suffix,
			},
			"error: one ASCII UTF-16 code unit over 2097152": {
				body:      prefix + strings.Repeat("a", maxUTF16Units-overhead+1) + suffix,
				wantError: "catalog too large",
			},
			"error: supplementary-plane surrogate pairs under 2097152 runes but over 2097152 UTF-16 code units": {
				// Each U+1F600 is 1 rune and 2 UTF-16 code units; 1,048,577 runes = 2,097,154 UTF-16 units (> 2,097,152).
				body:      prefix + strings.Repeat("😀", 1048577) + suffix,
				wantError: "catalog too large",
			},
		}

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, tt.body)
				}))
				t.Cleanup(upstream.Close)

				home, lookup, resolver := newTestPaths(t, nil)
				var logs logCapture
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://proxy.invalid",
					AnthropicBaseURL: upstream.URL,
					DefaultBaseURL:   "http://127.0.0.1:19999",
					Client:           newTestClient(t),
					Clock:            func() time.Time { return time.UnixMilli(7000000) },
					Routes:           &route.Store{},
					Sentry:           &sentry.State{},
					Logf:             logs.logf,
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				rawResp, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}})
				if err != nil {
					t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
				}
				raw, err := os.ReadFile(filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json"))
				if err != nil {
					t.Fatalf("read anthropic-models.json: %v", err)
				}
				assertNoMarkers(t, "page-limit response", rawResp, allCredentialMarkers)
				assertNoMarkers(t, "page-limit cache file", raw, allCredentialMarkers)
				assertNoMarkers(t, "page-limit logs", logs.bytes(), allCredentialMarkers)
				discoveryPath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
				if discoveryRaw, readErr := os.ReadFile(discoveryPath); readErr == nil {
					assertNoMarkers(t, "page-limit discovery cache", discoveryRaw, allCredentialMarkers)
					if tt.wantError != "" {
						t.Error("page-limit failure must not write gateway-models.json")
					}
				} else if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("read page-limit discovery cache: %v", readErr)
				}

				var saved struct {
					Data      []idDisplay `json:"data"`
					Error     *string     `json:"error"`
					UpdatedAt float64     `json:"updatedAt"`
				}
				if err := json.Unmarshal(raw, &saved); err != nil {
					t.Fatalf("unmarshal anthropic-models.json: %s", redactTestMarkers(err.Error()))
				}
				if tt.wantError == "" {
					if saved.Error != nil {
						t.Errorf("saved error = %q, want nil", redactTestMarkers(*saved.Error))
					}
					if saved.UpdatedAt != 7000000 || len(saved.Data) != 1 || saved.Data[0].ID != "claude-opus-5-5" {
						t.Errorf("saved catalog = (updatedAt=%v, len=%d), want (7000000, 1 row claude-opus-5-5)", saved.UpdatedAt, len(saved.Data))
					}
				} else {
					if saved.Error == nil || *saved.Error != tt.wantError {
						gotErr := "<nil>"
						if saved.Error != nil {
							gotErr = redactTestMarkers(*saved.Error)
						}
						t.Errorf("saved error = %q, want %q", gotErr, tt.wantError)
					}
					if saved.UpdatedAt != 0 {
						t.Errorf("updatedAt on failure = %v, want 0", saved.UpdatedAt)
					}
				}
			})
		}
	})

	t.Run("error: classified payload-free errors written via WriteFailure", func(t *testing.T) {
		tests := map[string]struct {
			makeHandler func(hits *atomic.Int32) http.HandlerFunc
			wantError   string
			wantHits    int32
		}{
			"error: upstream non-200": {
				makeHandler: func(hits *atomic.Int32) http.HandlerFunc {
					return func(w http.ResponseWriter, _ *http.Request) {
						hits.Add(1)
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(bodySecretSentinel))
					}
				},
				wantError: "upstream HTTP 503",
				wantHits:  1,
			},
			"error: invalid catalog JSON": {
				makeHandler: func(hits *atomic.Int32) http.HandlerFunc {
					return func(w http.ResponseWriter, _ *http.Request) {
						hits.Add(1)
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte("{not-json " + bodySecretSentinel))
					}
				},
				wantError: "invalid catalog JSON",
				wantHits:  1,
			},
			"error: invalid catalog data": {
				makeHandler: func(hits *atomic.Int32) http.HandlerFunc {
					return func(w http.ResponseWriter, _ *http.Request) {
						hits.Add(1)
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(`{"data":"` + bodySecretSentinel + `"}`))
					}
				},
				wantError: "invalid catalog data",
				wantHits:  1,
			},
			"error: pagination limit on repeated last_id": {
				makeHandler: func(hits *atomic.Int32) http.HandlerFunc {
					return func(w http.ResponseWriter, _ *http.Request) {
						n := hits.Add(1)
						w.WriteHeader(http.StatusOK)
						if n > 10 {
							_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":false,"last_id":"same-id"}`))
							return
						}
						_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":true,"last_id":"same-id"}`))
					}
				},
				wantError: "catalog pagination limit",
				wantHits:  2,
			},
			"error: pagination limit after 10 distinct pages": {
				makeHandler: func(hits *atomic.Int32) http.HandlerFunc {
					return func(w http.ResponseWriter, _ *http.Request) {
						n := hits.Add(1)
						w.WriteHeader(http.StatusOK)
						if n > 10 {
							_, _ = fmt.Fprintf(w, `{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":false,"last_id":"cursor-%d"}`, n)
							return
						}
						_, _ = fmt.Fprintf(w, `{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":true,"last_id":"cursor-%d"}`, n)
					}
				},
				wantError: "catalog pagination limit",
				wantHits:  10,
			},
		}

		failMarkers := maps.Clone(allCredentialMarkers)
		failMarkers["upstream body sentinel"] = bodySecretSentinel

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				var hits atomic.Int32
				inner := tt.makeHandler(&hits)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assertNoRequestMarkers(t, "Anthropic failure upstream", r, map[string]string{
						"studio token":      studioSecretMarker,
						"disallowed cookie": disallowedCookiePayload,
						"disallowed header": disallowedHeaderMarker,
					})
					inner(w, r)
				}))
				t.Cleanup(upstream.Close)

				home, lookup, resolver := newTestPaths(t, nil)
				var logs logCapture
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://proxy.invalid",
					AnthropicBaseURL: upstream.URL,
					DefaultBaseURL:   "http://127.0.0.1:19999",
					Client:           newTestClient(t),
					Clock:            func() time.Time { return time.UnixMilli(5000000) },
					Routes:           &route.Store{},
					Sentry:           &sentry.State{},
					EffectiveBaseURL: baseURLFunc("http://127.0.0.1:19999"),
					Logf:             logs.logf,
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				rawResp, err := cache.ModelsResponse(t.Context(), http.Header{
					"X-Api-Key":       {apiKeyMarker},
					"Authorization":   {authBearerHeader},
					"Cookie":          {disallowedCookieHeader},
					"X-Custom-Secret": {disallowedHeaderMarker},
				})
				if err != nil {
					t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
				}
				if gotHits := hits.Load(); gotHits != tt.wantHits {
					t.Errorf("upstream hits = %d, want %d", gotHits, tt.wantHits)
				}
				cacheFile := filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json")
				raw, err := os.ReadFile(cacheFile)
				if err != nil {
					t.Fatalf("read anthropic-models.json: %v", err)
				}
				assertNoMarkers(t, "failure response", rawResp, failMarkers)
				assertNoMarkers(t, "failure cache file", raw, failMarkers)
				assertNoMarkers(t, "failure logs", logs.bytes(), failMarkers)
				discoveryPath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
				if discoveryRaw, readErr := os.ReadFile(discoveryPath); readErr == nil {
					assertNoMarkers(t, "failure discovery cache", discoveryRaw, failMarkers)
					t.Error("upstream catalog failure must not write gateway-models.json")
				} else if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("read failure discovery cache: %v", readErr)
				}

				var saved struct {
					Error       string  `json:"error"`
					UpdatedAt   float64 `json:"updatedAt"`
					AttemptedAt float64 `json:"attemptedAt"`
				}
				if err := json.Unmarshal(raw, &saved); err != nil {
					t.Fatalf("unmarshal anthropic-models.json: %s", redactTestMarkers(err.Error()))
				}
				if saved.Error != tt.wantError {
					t.Errorf("saved error = %q, want %q", redactTestMarkers(saved.Error), tt.wantError)
				}
				if saved.UpdatedAt != 0 || saved.AttemptedAt != 5000000 {
					t.Errorf("timestamps = (updatedAt=%v, attemptedAt=%v), want (0, 5000000)", saved.UpdatedAt, saved.AttemptedAt)
				}
			})
		}
	})

	t.Run("success: stale cache serves immediately, survives request cancellation, clones headers, deduplicates refresh, and cancels immediately on worker shutdown", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var anthropicCalls atomic.Int32
			var firstCtxCanceled atomic.Bool
			var secondPageHeadersOK atomic.Bool
			var triggerHeadersOK atomic.Bool
			var secondCtxErr atomic.Pointer[error]

			releaseFirst := make(chan struct{})
			var closeFirstOnce sync.Once
			closeFirst := func() { closeFirstOnce.Do(func() { close(releaseFirst) }) }

			releaseSecond := make(chan struct{})
			var closeSecondOnce sync.Once
			closeSecond := func() { closeSecondOnce.Do(func() { close(releaseSecond) }) }

			const proxyHost = "127.0.0.1:19991"
			const anthropicHost = "127.0.0.1:19992"

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host == proxyHost && r.URL.Path == "/v1/models" {
					assertNoRequestMarkers(t, "proxy /v1/models", r, allCredentialMarkers)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
					return
				}
				if r.Host != anthropicHost || r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				assertNoRequestMarkers(t, "Anthropic background refresh", r, map[string]string{
					"studio token":      studioSecretMarker,
					"disallowed cookie": disallowedCookiePayload,
					"disallowed header": disallowedHeaderMarker,
				})
				call := anthropicCalls.Add(1)
				switch call {
				case 1:
					select {
					case <-releaseFirst:
						firstCtxCanceled.Store(r.Context().Err() != nil)
					case <-r.Context().Done():
						firstCtxCanceled.Store(true)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5-5","display_name":"Sonnet 5.5"}],"has_more":true,"last_id":"claude-sonnet-5-5"}`))
					return
				case 2:
					_, hasCookie := r.Header["Cookie"]
					if r.URL.Query().Get("after_id") == "claude-sonnet-5-5" &&
						r.Header.Get("X-Api-Key") == apiKeyMarker &&
						r.Header.Get("Anthropic-Beta") == "prompt-caching-2024-07-31" &&
						!hasCookie {
						secondPageHeadersOK.Store(true)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"claude-haiku-5-0","display_name":"Haiku 5.0"}],"has_more":false,"last_id":"claude-haiku-5-0"}`))
					return
				default:
					_, hasCookie := r.Header["Cookie"]
					if r.Header.Get("X-Api-Key") == apiKeyMarker && !hasCookie {
						triggerHeadersOK.Store(true)
					}
					select {
					case <-releaseSecond:
					case <-r.Context().Done():
						err := r.Context().Err()
						secondCtxErr.Store(&err)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":false}`))
				}
			})

			home, lookup, resolver := newTestPaths(t, map[string]string{
				"CODEX_GATEWAY_MODELS_REFRESH_MS": "600000",
			})
			stateDir := filepath.Join(home, ".claude", "model-gateway")
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatalf("mkdir state dir: %v", err)
			}
			staleCache := `{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"updatedAt":1000,"error":null}` + "\n"
			if err := os.WriteFile(filepath.Join(stateDir, "anthropic-models.json"), []byte(staleCache), 0o600); err != nil {
				t.Fatalf("write stale cache: %v", err)
			}

			var nowMu sync.Mutex
			now := time.UnixMilli(1000 + time.Hour.Milliseconds())
			clock := func() time.Time {
				nowMu.Lock()
				defer nowMu.Unlock()
				return now
			}
			var logs logCapture
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://" + proxyHost,
				AnthropicBaseURL: "http://" + anthropicHost,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:            clock,
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				EffectiveBaseURL: baseURLFunc("http://127.0.0.1:19999"),
				Logf:             logs.logf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			workerCtx, workerCancel := context.WithCancel(t.Context())
			defer func() {
				closeFirst()
				closeSecond()
				workerCancel()
				cache.Wait()
			}()
			cache.Start(workerCtx)
			synctest.Wait()

			apiKeySlice := []string{apiKeyMarker}
			betaSlice := []string{"prompt-caching-2024-07-31"}
			headers := http.Header{
				"X-Api-Key":      apiKeySlice,
				"Anthropic-Beta": betaSlice,
			}
			reqCtx, reqCancel := context.WithCancel(t.Context())
			raw1, err := cache.ModelsResponse(reqCtx, headers)
			// Immediately cancel the initiating request and mutate the caller's header map and backing slices;
			// the background refresh must survive and fetch subsequent pages using its cloned headers.
			reqCancel()
			apiKeySlice[0] = "mutated-api-key-must-not-be-sent"
			betaSlice[0] = "mutated-beta-must-not-be-sent"
			headers.Set("Cookie", disallowedCookieHeader)
			if err != nil {
				t.Fatalf("first stale ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			var resp1 struct {
				Data []idDisplay `json:"data"`
			}
			if err := json.Unmarshal(raw1, &resp1); err != nil {
				t.Fatalf("unmarshal stale ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			wantStale := []idDisplay{
				{ID: "claude-opus-5-5", DisplayName: "Opus 5.5"},
				{ID: "claude-gpt-6-astra[1m]", DisplayName: "GPT-6-astra (Codex)"},
				{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7"},
				{ID: "claude-grok-4.6[1m]", DisplayName: "Grok 4.6"},
				{ID: "claude-grok-4.5[1m]", DisplayName: "Grok 4.5"},
			}
			if diff := gocmp.Diff(wantStale, resp1.Data); diff != "" {
				t.Errorf("stale response rows mismatch (-want +got):\n%s", redactTestMarkers(diff))
			}

			// Observe that stale ModelsResponse itself started call 1 BEFORE invoking the deduplication trigger.
			synctest.Wait()
			if got := anthropicCalls.Load(); got != 1 {
				t.Fatalf("stale ModelsResponse did not start background refresh: calls = %d, want 1", got)
			}

			cache.TriggerAnthropicRefresh(http.Header{"X-Api-Key": {apiKeyMarker}})
			synctest.Wait()
			if got := anthropicCalls.Load(); got != 1 {
				t.Fatalf("concurrent stale refresh upstream calls = %d, want 1", got)
			}

			closeFirst()
			synctest.Wait()
			if firstCtxCanceled.Load() {
				t.Error("background Anthropic refresh was canceled when the initiating request ended")
			}
			if got := anthropicCalls.Load(); got != 2 {
				t.Fatalf("paginated background refresh upstream calls = %d, want 2", got)
			}
			if !secondPageHeadersOK.Load() {
				t.Error("page 2 of background refresh did not retain cloned caller headers after caller mutation")
			}

			raw2, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}})
			if err != nil {
				t.Fatalf("post-refresh ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			var resp2 struct {
				Data []idDisplay `json:"data"`
			}
			if err := json.Unmarshal(raw2, &resp2); err != nil {
				t.Fatalf("unmarshal post-refresh ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			wantRefreshed := []idDisplay{
				{ID: "claude-sonnet-5-5", DisplayName: "Sonnet 5.5"},
				{ID: "claude-haiku-5-0", DisplayName: "Haiku 5.0"},
				{ID: "claude-gpt-6-astra[1m]", DisplayName: "GPT-6-astra (Codex)"},
				{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7"},
				{ID: "claude-grok-4.6[1m]", DisplayName: "Grok 4.6"},
				{ID: "claude-grok-4.5[1m]", DisplayName: "Grok 4.5"},
			}
			if diff := gocmp.Diff(wantRefreshed, resp2.Data); diff != "" {
				t.Errorf("post-refresh response rows mismatch (-want +got):\n%s", redactTestMarkers(diff))
			}
			discoveryPath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
			discoveryRaw, err := os.ReadFile(discoveryPath)
			if err != nil {
				t.Fatalf("expected discovery cache synced on Anthropic refresh: %v", err)
			}
			var discoveryDoc struct {
				BaseURL   string      `json:"baseUrl"`
				FetchedAt int64       `json:"fetchedAt"`
				Models    []idDisplay `json:"models"`
			}
			if err := json.Unmarshal(discoveryRaw, &discoveryDoc); err != nil {
				t.Fatalf("unmarshal discovery cache: %s", redactTestMarkers(err.Error()))
			}
			if diff := gocmp.Diff(wantRefreshed, discoveryDoc.Models); diff != "" {
				t.Errorf("discovery cache models after Anthropic refresh mismatch (-want +got):\n%s", redactTestMarkers(diff))
			}
			assertNoMarkers(t, "stale refresh discovery cache", discoveryRaw, allCredentialMarkers)
			assertNoMarkers(t, "stale refresh logs", logs.bytes(), allCredentialMarkers)

			// Advance past TTL, trigger a second background refresh, mutate caller headers immediately,
			// and verify worker shutdown cancels it immediately (context.Canceled with 0 virtual-time advance).
			nowMu.Lock()
			now = now.Add(time.Hour)
			nowMu.Unlock()
			trigSlice := []string{apiKeyMarker}
			trigHeaders := http.Header{"X-Api-Key": trigSlice}
			cache.TriggerAnthropicRefresh(trigHeaders)
			trigSlice[0] = "mutated-trigger-api-key"
			trigHeaders.Set("Cookie", disallowedCookieHeader)
			synctest.Wait()
			if got := anthropicCalls.Load(); got != 3 {
				t.Fatalf("second background refresh upstream calls = %d, want 3", got)
			}
			if !triggerHeadersOK.Load() {
				t.Error("TriggerAnthropicRefresh did not clone caller headers before starting background refresh")
			}
			cancelStart := time.Now()
			workerCancel()
			cache.Wait()
			if elapsed := time.Since(cancelStart); elapsed != 0 {
				t.Errorf("worker shutdown waited %v virtual time, want 0 (must cancel via worker context, not page deadline)", elapsed)
			}
			if errPtr := secondCtxErr.Load(); errPtr == nil || !errors.Is(*errPtr, context.Canceled) {
				t.Errorf("in-flight background Anthropic refresh ctx.Err() = %v, want context.Canceled", errPtr)
			}
		})
	})

	t.Run("success: cold first-fill waiter stops on request cancellation while worker-owned fetch completes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var anthropicCalls atomic.Int32
			var upstreamCanceled atomic.Bool
			releaseUpstream := make(chan struct{})
			var closeReleaseOnce sync.Once
			closeRelease := func() { closeReleaseOnce.Do(func() { close(releaseUpstream) }) }

			const proxyHost = "127.0.0.1:19991"
			const anthropicHost = "127.0.0.1:19992"

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host == proxyHost && r.URL.Path == "/v1/models" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
					return
				}
				if r.Host != anthropicHost || r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				anthropicCalls.Add(1)
				select {
				case <-releaseUpstream:
					upstreamCanceled.Store(r.Context().Err() != nil)
				case <-r.Context().Done():
					upstreamCanceled.Store(true)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":false}`))
			})

			home, lookup, resolver := newTestPaths(t, map[string]string{
				"CODEX_GATEWAY_MODELS_REFRESH_MS": "600000",
			})
			var logs logCapture
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://" + proxyHost,
				AnthropicBaseURL: "http://" + anthropicHost,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:            time.Now,
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				EffectiveBaseURL: baseURLFunc("http://127.0.0.1:19999"),
				Logf:             logs.logf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			workerCtx, workerCancel := context.WithCancel(t.Context())
			defer func() {
				closeRelease()
				workerCancel()
				cache.Wait()
			}()
			cache.Start(workerCtx)
			synctest.Wait()

			reqCtx, reqCancel := context.WithCancel(t.Context())
			waitDone := make(chan error, 1)
			go func() {
				_, err := cache.ModelsResponse(reqCtx, http.Header{"X-Api-Key": {apiKeyMarker}})
				waitDone <- err
			}()
			synctest.Wait()

			if got := anthropicCalls.Load(); got != 1 {
				t.Fatalf("cold first-fill upstream calls = %d, want 1", got)
			}
			select {
			case err := <-waitDone:
				t.Fatalf("cold waiter returned before upstream response or request cancellation: %v", err)
			default:
			}

			// Cancel the cold waiter before releasing the upstream response; it must unblock immediately
			// with context.Canceled without aborting the shared worker-owned fetch.
			cancelStart := time.Now()
			reqCancel()
			waitErr := <-waitDone
			if elapsed := time.Since(cancelStart); elapsed != 0 {
				t.Errorf("canceled cold waiter elapsed = %v, want 0", elapsed)
			}
			if !errors.Is(waitErr, context.Canceled) {
				t.Errorf("canceled cold waiter err = %v, want context.Canceled", waitErr)
			}

			closeRelease()
			synctest.Wait()
			if upstreamCanceled.Load() {
				t.Error("canceling the cold waiter aborted the shared worker-owned Anthropic fetch")
			}

			rawAfter, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}})
			if err != nil {
				t.Fatalf("follow-up ModelsResponse after worker fetch completed: %s", redactTestMarkers(err.Error()))
			}
			if got := anthropicCalls.Load(); got != 1 {
				t.Errorf("follow-up ModelsResponse sent extra upstream calls: total=%d, want 1", got)
			}
			var doc struct {
				Data []idDisplay `json:"data"`
			}
			if err := json.Unmarshal(rawAfter, &doc); err != nil {
				t.Fatalf("unmarshal follow-up ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			if len(doc.Data) == 0 || doc.Data[0].ID != "claude-opus-5-5" {
				t.Errorf("follow-up ModelsResponse first row = %s, want claude-opus-5-5", redactTestMarkers(fmt.Sprintf("%v", doc.Data)))
			}
			assertNoMarkers(t, "cold-cancel response", rawAfter, allCredentialMarkers)
			if cachedBytes, err := os.ReadFile(filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json")); err != nil {
				t.Fatalf("read anthropic-models.json: %v", err)
			} else {
				assertNoMarkers(t, "cold-cancel cache file", cachedBytes, allCredentialMarkers)
			}
			assertNoMarkers(t, "cold-cancel logs", logs.bytes(), allCredentialMarkers)
		})
	})

	t.Run("error: per-page catalog timeout records classified message after 5s", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			})
			home, lookup, resolver := newTestPaths(t, nil)
			var logs logCapture
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://proxy.invalid",
				AnthropicBaseURL: "http://127.0.0.1:19993",
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:            time.Now,
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				EffectiveBaseURL: baseURLFunc("http://127.0.0.1:19999"),
				Logf:             logs.logf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			start := time.Now()
			rawResp, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}})
			elapsed := time.Since(start)
			if err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			if elapsed != 5*time.Second {
				t.Errorf("catalog timeout elapsed = %v, want 5s", elapsed)
			}
			raw, err := os.ReadFile(filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json"))
			if err != nil {
				t.Fatalf("read anthropic-models.json: %v", err)
			}
			assertNoMarkers(t, "timeout response", rawResp, allCredentialMarkers)
			assertNoMarkers(t, "timeout cache file", raw, allCredentialMarkers)
			assertNoMarkers(t, "timeout logs", logs.bytes(), allCredentialMarkers)
			discoveryPath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
			if discoveryRaw, readErr := os.ReadFile(discoveryPath); readErr == nil {
				assertNoMarkers(t, "timeout discovery cache", discoveryRaw, allCredentialMarkers)
				t.Error("catalog timeout must not write gateway-models.json")
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("read timeout discovery cache: %v", readErr)
			}
			var saved struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatalf("unmarshal anthropic-models.json: %s", redactTestMarkers(err.Error()))
			}
			if saved.Error != "catalog timeout" {
				t.Errorf("timeout error = %q, want %q", redactTestMarkers(saved.Error), "catalog timeout")
			}
		})
	})
}

func TestDiscoveryCacheSync(t *testing.T) {
	var proxyHits atomic.Int32
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(proxySrv.Close)

	const defaultURL = "http://127.0.0.1:19999"

	t.Run("success: wired proxy writes discovery cache and only updates fetchedAt on force", func(t *testing.T) {
		home, lookup, resolver := newTestPaths(t, nil)
		nowMs := int64(1234567890)
		cache, err := modelcache.New(modelcache.Options{
			Lookup:           lookup,
			Paths:            resolver,
			ProxyBaseURL:     proxySrv.URL,
			DefaultBaseURL:   defaultURL,
			Client:           newTestClient(t),
			Clock:            func() time.Time { return time.UnixMilli(nowMs) },
			Routes:           &route.Store{},
			Sentry:           &sentry.State{},
			EffectiveBaseURL: baseURLFunc(defaultURL),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := cache.Refresh(t.Context()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}

		cachePath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
		readDoc := func(t *testing.T) (string, int64, []idDisplay) {
			t.Helper()
			raw, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatalf("read discovery cache: %v", err)
			}
			var keys map[string]jsontext.Value
			if err := json.Unmarshal(raw, &keys); err != nil {
				t.Fatalf("unmarshal discovery cache keys: %v", err)
			}
			if diff := gocmp.Diff([]string{"baseUrl", "fetchedAt", "models"}, slices.Sorted(maps.Keys(keys))); diff != "" {
				t.Errorf("discovery cache keys mismatch (-want +got):\n%s", diff)
			}
			var doc struct {
				BaseURL   string      `json:"baseUrl"`
				FetchedAt int64       `json:"fetchedAt"`
				Models    []idDisplay `json:"models"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("unmarshal discovery cache: %v", err)
			}
			return doc.BaseURL, doc.FetchedAt, doc.Models
		}

		baseURL, fetchedAt, models := readDoc(t)
		if baseURL != defaultURL || fetchedAt != 1234567890 {
			t.Errorf("initial discovery cache header = (%q, %d), want (%q, 1234567890)", baseURL, fetchedAt, defaultURL)
		}
		wantModels := []idDisplay{
			{ID: "claude-gpt-6-astra[1m]", DisplayName: "GPT-6-astra (Codex)"},
			{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7"},
			{ID: "claude-grok-4.6[1m]", DisplayName: "Grok 4.6"},
			{ID: "claude-grok-4.5[1m]", DisplayName: "Grok 4.5"},
		}
		if diff := gocmp.Diff(wantModels, models); diff != "" {
			t.Errorf("discovery cache models mismatch (-want +got):\n%s", diff)
		}

		// Advance clock and sync without force: must report unchanged and keep original fetchedAt.
		nowMs = 1234569999
		res, err := cache.SyncDiscoveryCache(false)
		if err != nil {
			t.Fatalf("SyncDiscoveryCache(false): %v", err)
		}
		if res.State != "unchanged" {
			t.Errorf("SyncDiscoveryCache(false) state = %q, want %q", res.State, "unchanged")
		}
		_, fetchedAfterUnchanged, _ := readDoc(t)
		if fetchedAfterUnchanged != 1234567890 {
			t.Errorf("fetchedAt changed on unchanged sync: got %d, want 1234567890", fetchedAfterUnchanged)
		}

		// Advance clock again and force sync: must report wrote and update fetchedAt on disk.
		nowMs = 1234577777
		forced, err := cache.SyncDiscoveryCache(true)
		if err != nil {
			t.Fatalf("SyncDiscoveryCache(true): %v", err)
		}
		if forced.State != "wrote" {
			t.Errorf("SyncDiscoveryCache(true) state = %q, want %q", forced.State, "wrote")
		}
		_, fetchedAfterForce, forcedModels := readDoc(t)
		if fetchedAfterForce != 1234577777 {
			t.Errorf("fetchedAt after forced sync = %d, want 1234577777", fetchedAfterForce)
		}
		if diff := gocmp.Diff(wantModels, forcedModels); diff != "" {
			t.Errorf("forced discovery cache models mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("success: skipped modes never create, overwrite, or dial even when forced", func(t *testing.T) {
		const preexistingSentinel = `{"sentinel":"preexisting-discovery-cache-must-not-change"}` + "\n"
		tests := map[string]struct {
			shadow       bool
			effectiveURL func() (string, error)
			wantReason   string
			wantErr      bool
		}{
			"success: shadow mode": {
				shadow:       true,
				effectiveURL: baseURLFunc(defaultURL),
				wantReason:   "shadow",
			},
			"success: legacy compat URL": {
				effectiveURL: baseURLFunc("http://api.anthropic.com"),
				wantReason:   "gateway-not-wired",
			},
			"success: unwired base URL": {
				effectiveURL: baseURLFunc("https://api.anthropic.com"),
				wantReason:   "gateway-not-wired",
			},
			"success: empty effective base URL": {
				effectiveURL: baseURLFunc(""),
				wantReason:   "gateway-not-wired",
			},
			"error: EffectiveBaseURL error with matching URL never writes": {
				effectiveURL: func() (string, error) { return defaultURL, errors.New("settings unreadable") },
				wantErr:      true,
			},
			"error: EffectiveBaseURL error with empty URL never writes": {
				effectiveURL: func() (string, error) { return "", errors.New("settings unreadable") },
				wantErr:      true,
			},
		}

		for name, tt := range tests {
			for _, seeded := range []bool{false, true} {
				sub := name + "/absent"
				if seeded {
					sub = name + "/preexisting"
				}
				t.Run(sub, func(t *testing.T) {
					home, lookup, resolver := newTestPaths(t, nil)
					cachePath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
					if seeded {
						if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
							t.Fatalf("mkdir discovery cache dir: %v", err)
						}
						if err := os.WriteFile(cachePath, []byte(preexistingSentinel), 0o600); err != nil {
							t.Fatalf("seed preexisting discovery cache: %v", err)
						}
					}

					baseTr := &http.Transport{Proxy: nil}
					t.Cleanup(baseTr.CloseIdleConnections)
					gated := &gatedTransport{
						base:        baseTr,
						allowedHost: proxySrv.Listener.Addr().String(),
					}
					client := &http.Client{
						Transport: gated,
						CheckRedirect: func(*http.Request, []*http.Request) error {
							return http.ErrUseLastResponse
						},
					}

					cache, err := modelcache.New(modelcache.Options{
						Lookup:           lookup,
						Paths:            resolver,
						ProxyBaseURL:     proxySrv.URL,
						DefaultBaseURL:   defaultURL,
						Shadow:           tt.shadow,
						Client:           client,
						Clock:            func() time.Time { return time.UnixMilli(1234567890) },
						Routes:           &route.Store{},
						Sentry:           &sentry.State{},
						EffectiveBaseURL: tt.effectiveURL,
					})
					if err != nil {
						t.Fatalf("New: %v", err)
					}
					if err := cache.Refresh(t.Context()); err != nil {
						t.Fatalf("Refresh: %v", err)
					}
					if unexpected := gated.attempts.Load(); unexpected != 0 {
						t.Errorf("Refresh attempted %d non-proxy outbound requests, want 0", unexpected)
					}

					checkFileUnchanged := func(stage string) {
						t.Helper()
						data, err := os.ReadFile(cachePath)
						if !seeded {
							if !os.IsNotExist(err) {
								t.Fatalf("%s: expected no discovery cache at %s, err = %v", stage, cachePath, err)
							}
							return
						}
						if err != nil {
							t.Fatalf("%s: preexisting discovery cache disappeared: %v", stage, err)
						}
						if string(data) != preexistingSentinel {
							t.Errorf("%s: preexisting discovery cache modified: got %q, want %q", stage, redactTestMarkers(string(data)), preexistingSentinel)
						}
					}

					checkFileUnchanged("after Refresh")

					// Block and record ALL outbound HTTP transport attempts (any destination) after setup.
					gated.blocked.Store(true)
					gated.attempts.Store(0)
					hitsBefore := proxyHits.Load()

					for _, force := range []bool{false, true} {
						res, err := cache.SyncDiscoveryCache(force)
						if gotAttempts := gated.attempts.Load(); gotAttempts != 0 {
							t.Errorf("SyncDiscoveryCache(%v) attempted %d outbound transport calls, want 0", force, gotAttempts)
						}
						if gotHits := proxyHits.Load(); gotHits != hitsBefore {
							t.Errorf("SyncDiscoveryCache(%v) hit proxy fake %d times, want 0", force, gotHits-hitsBefore)
						}
						if tt.wantErr {
							if err == nil {
								t.Fatalf("SyncDiscoveryCache(%v) error = nil, want non-nil error (result=%+v)", force, res)
							}
						} else {
							if err != nil {
								t.Fatalf("SyncDiscoveryCache(%v): %v", force, err)
							}
							if res.State != "skipped" || res.Reason != tt.wantReason {
								t.Errorf("SyncDiscoveryCache(%v) = (%q, %q), want (%q, %q)", force, res.State, res.Reason, "skipped", tt.wantReason)
							}
						}
						checkFileUnchanged(fmt.Sprintf("after SyncDiscoveryCache(%v)", force))
					}
				})
			}
		}
	})

	t.Run("success: non-proxy refresh does not persist fallback or models.json to discovery cache", func(t *testing.T) {
		tests := map[string]struct {
			modelsJSON string
			wantSource string
		}{
			"success: built-in fallback": {
				wantSource: "fallback",
			},
			"success: models.json stand-in": {
				modelsJSON: `["gpt-6.1-sol"]`,
				wantSource: "models.json",
			},
		}

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				home, lookup, resolver := newTestPaths(t, nil)
				if tt.modelsJSON != "" {
					stateDir := filepath.Join(home, ".claude", "model-gateway")
					if err := os.MkdirAll(stateDir, 0o700); err != nil {
						t.Fatalf("mkdir state dir: %v", err)
					}
					if err := os.WriteFile(filepath.Join(stateDir, "models.json"), []byte(tt.modelsJSON), 0o600); err != nil {
						t.Fatalf("write models.json: %v", err)
					}
				}
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://proxy.invalid",
					DefaultBaseURL:   defaultURL,
					Client:           newFailingClient(),
					Clock:            func() time.Time { return time.UnixMilli(1234567890) },
					Routes:           &route.Store{},
					Sentry:           &sentry.State{},
					EffectiveBaseURL: baseURLFunc(defaultURL),
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if err := cache.Refresh(t.Context()); err != nil {
					t.Fatalf("Refresh: %v", err)
				}
				if src, _ := cache.HealthCatalog(); src != tt.wantSource {
					t.Errorf("HealthCatalog source = %q, want %q", src, tt.wantSource)
				}
				cachePath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
				if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
					t.Errorf("non-proxy refresh (%s) created discovery cache: stat err = %v", tt.wantSource, err)
				}
			})
		}
	})
}
