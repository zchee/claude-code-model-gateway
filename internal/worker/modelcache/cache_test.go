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
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

type staticBaseURL string

func (s staticBaseURL) EffectiveBaseURL() string { return string(s) }

type staticPins map[string]string

func (p staticPins) CurrentPins() map[string]string { return p }

type modelRow struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	Type           string `json:"type"`
	MaxInputTokens int64  `json:"max_input_tokens"`
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
			windows := policy.Windows{
				"codex": {Value: config.ContextWindow{Tokens: 272000}, Source: "default"},
				"grok":  {Value: config.ContextWindow{Full: true}, Source: "default"},
			}

			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				Windows:        windows,
				ProxyBaseURL:   "http://127.0.0.1:9",
				DefaultBaseURL: "http://127.0.0.1:19999",
				Client:         newTestClient(t),
				Routes:         routes,
				Sentry:         sentryState,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			// Mutate caller map after construction to prove New copied ResolverInputs.
			windows["codex"] = policy.Setting{Value: config.ContextWindow{Tokens: 185000}, Source: "saved"}

			if gen := cache.Generation(); gen != 1 {
				t.Errorf("startup Generation = %d, want 1", gen)
			}
			source, count := cache.HealthCatalog()
			if source != "fallback" || count != len(tt.wantIDs) {
				t.Errorf("HealthCatalog = (%q, %d), want (%q, %d)", source, count, "fallback", len(tt.wantIDs))
			}

			snap, ok := routes.Load()
			if !ok {
				t.Fatal("route.Store had no snapshot after New")
			}
			snapSource, rawCatalog := snap.Catalog()
			if snapSource != "fallback" {
				t.Errorf("snapshot source = %q, want %q", snapSource, "fallback")
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
		if r.URL.Path != "/api/v1/models" {
			w.WriteHeader(http.StatusNotFound)
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

	// Raw .data carries an escaped lone surrogate that must be dropped by proxymodels.ParseListing,
	// a native Claude alias that must be ignored, both bare and prefixed Vertex Gemini IDs, and Fable.
	rawProxyBody := `{"object":"list","data":[` +
		`{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"},` +
		`{"id":"claude-opus-5-5","display_name":"claude-opus-5-5 (anthropic)"},` +
		`{"id":"bad\uD800model","display_name":"bad\uD800model (kimi)"},` +
		`{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},` +
		`{"id":"gemini-4-argon","display_name":"gemini-4-argon (vertex)"},` +
		`{"id":"vertex/gemini-4-argon","display_name":"vertex/gemini-4-argon (vertex)"},` +
		`{"id":"vertex/anthropic/claude-fable-5-1","display_name":"vertex/anthropic/claude-fable-5-1 (vertex)"},` +
		`{"id":"local/gguf","display_name":"local/gguf (local)"},` +
		`{"id":"lmstudio/qwen3-coder-30b","display_name":"lmstudio/qwen3-coder-30b (lmstudio)"}` +
		`]}`
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rawProxyBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(proxySrv.Close)

	home, lookup, resolver := newTestPaths(t, map[string]string{
		"CCP_LLAMA_PORT":        strconv.Itoa(llamaPort),
		"CCP_LMSTUDIO_BASE_URL": studioSrv.URL,
	})
	grokDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(grokDir, 0o700); err != nil {
		t.Fatalf("mkdir grok dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(grokDir, "models_cache.json"), []byte(`["grok-4.7","grok-unlisted-not-in-policy"]`), 0o600); err != nil {
		t.Fatalf("write grok cache: %v", err)
	}

	routes := &route.Store{}
	sentryState := &sentry.State{}
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
		Sentry:         sentryState,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if got := cache.Generation(); got != 2 {
		t.Fatalf("Generation after first Refresh = %d, want 2", got)
	}

	snap1, ok := routes.Load()
	if !ok {
		t.Fatal("route.Store empty after Refresh")
	}
	src, rawCatalog := snap1.Catalog()
	if src != "proxy" {
		t.Errorf("snapshot source = %q, want %q", src, "proxy")
	}
	var rows []modelRow
	if err := json.Unmarshal(rawCatalog, &rows); err != nil {
		t.Fatalf("unmarshal refreshed catalog: %v", err)
	}
	wantRows := []modelRow{
		{ID: "claude-gpt-6-astra[1m]", DisplayName: "GPT-6-astra (Codex)", Type: "model", MaxInputTokens: 272000},
		{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7", Type: "model", MaxInputTokens: 500000},
		{ID: "claude-kimi-k3", DisplayName: "kimi-k3 (Kimi)", Type: "model", MaxInputTokens: 200000},
		{ID: "claude-gemini-4-argon[1m]", DisplayName: "gemini-4-argon (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
		{ID: "claude-vertex/anthropic/fable-5-1[1m]", DisplayName: "vertex/anthropic/claude-fable-5-1 (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
		{ID: "claude-local/gguf", DisplayName: "local/gguf (Local GGUF)", Type: "model", MaxInputTokens: 65536},
		{ID: "claude-lmstudio/qwen3-coder-30b", DisplayName: "lmstudio/qwen3-coder-30b (LM Studio)", Type: "model", MaxInputTokens: 65536},
	}
	if diff := gocmp.Diff(wantRows, rows); diff != "" {
		t.Errorf("refreshed catalog rows mismatch (-want +got):\n%s", diff)
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
	if got := cache.Generation(); got != 3 {
		t.Fatalf("Generation after second Refresh = %d, want 3", got)
	}
	// The request keeps its routing snapshot (snap1 still resolves 65536)...
	if got := snap1.Resolver().AdvertisedWindow("claude-lmstudio/qwen3-coder-30b"); got != 65536 {
		t.Errorf("snap1 advertised window mutated to %d, want immutable 65536", got)
	}
	// ...but sentry callbacks use the latest published policy (trigger 27307)!
	crossing, refused := sentryState.Check(session, targetFromSnap1, []byte(`{"messages":[{"role":"user","content":"turn"}]}`))
	if !refused || crossing.Usage != 30000 || crossing.Trigger != 27307 {
		t.Errorf("sentry Check after reload = (%+v, %v), want refused (30000 > 27307)", crossing, refused)
	}

	// Unload LM Studio (context 0) and refresh (generation 4): model becomes unguarded.
	studioCtx.Store(0)
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("third Refresh: %v", err)
	}
	if got := cache.Generation(); got != 4 {
		t.Fatalf("Generation after third Refresh = %d, want 4", got)
	}
	if _, refused := sentryState.Check(session, targetFromSnap1, []byte(`{"messages":[{"role":"user","content":"turn"}]}`)); refused {
		t.Error("unloaded LM Studio model must not be refused by sentry")
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
		cache, err := modelcache.New(modelcache.Options{
			Lookup:         lookup,
			Paths:          resolver,
			ProxyBaseURL:   "http://127.0.0.1:9",
			DefaultBaseURL: "http://127.0.0.1:19999",
			Client:         newTestClient(t),
			Routes:         routes,
			Sentry:         &sentry.State{},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := cache.Refresh(t.Context()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		source, _ := cache.HealthCatalog()
		if source != "models.json" {
			t.Errorf("HealthCatalog source = %q, want %q", source, "models.json")
		}
		snap, _ := routes.Load()
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
		cache, err := modelcache.New(modelcache.Options{
			Lookup:         lookup,
			Paths:          resolver,
			ProxyBaseURL:   proxySrv.URL,
			DefaultBaseURL: "http://127.0.0.1:19999",
			Client:         newTestClient(t),
			Routes:         routes,
			Sentry:         &sentry.State{},
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
		source, _ := cache.HealthCatalog()
		if source != "proxy" {
			t.Errorf("source after outage = %q, want preserved %q", source, "proxy")
		}
		snap, _ := routes.Load()
		if d := snap.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"claude-kimi-k3"}`)); d.Branch != "proxy" || d.Provider != "kimi" {
			t.Errorf("retained route decision = %+v, want proxy/kimi", d)
		}
	})
}

func TestRefreshScheduleAndRetryBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var hitTimes []time.Time
		succeedAfter := 8 // First 8 attempts fail (initial + 1s, 2s, 4s, 8s, 16s, 30s, 30s), 9th succeeds.
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
			if n <= succeedAfter {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
		})

		_, lookup, resolver := newTestPaths(t, map[string]string{
			"CODEX_GATEWAY_MODELS_REFRESH_MS": "60000",
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

		// Advance through backoff steps: 1s, 2s, 4s, 8s, 16s, 30s, 30s (periodic tick also fires at 60s).
		// Check the first five exact exponential retry intervals (before the 60s periodic tick arrives).
		expectedDelays := []time.Duration{
			1 * time.Second,
			2 * time.Second,
			4 * time.Second,
			8 * time.Second,
			16 * time.Second,
		}
		for _, d := range expectedDelays {
			time.Sleep(d)
			synctest.Wait()
		}

		mu.Lock()
		gotTimes := append([]time.Time(nil), hitTimes...)
		mu.Unlock()
		if len(gotTimes) < 6 {
			t.Fatalf("recorded %d refresh hits in 31s, want at least 6", len(gotTimes))
		}
		gotIntervals := make([]time.Duration, 5)
		for i := range 5 {
			gotIntervals[i] = gotTimes[i+1].Sub(gotTimes[i])
		}
		if diff := gocmp.Diff(expectedDelays, gotIntervals); diff != "" {
			t.Errorf("exponential retry intervals mismatch (-want +got):\n%s", diff)
		}

		// Advance past cap (30s) and recovery, then confirm periodic 60s refresh succeeds.
		time.Sleep(90 * time.Second)
		synctest.Wait()
		if src, _ := cache.HealthCatalog(); src != "proxy" {
			t.Errorf("source after proxy recovery = %q, want %q", src, "proxy")
		}
	})
}

func TestAnthropicCatalogAndModelsEndpoint(t *testing.T) {
	const secretAPIKey = "sk-ant-secret-never-persisted"
	const bodySecretSentinel = "RAW_UPSTREAM_BODY_SENTINEL_DO_NOT_PERSIST"

	t.Run("success: unauthenticated skips upstream; authenticated paginates, filters headers, relabels previous, and caches without secrets", func(t *testing.T) {
		var mu sync.Mutex
		var seenHeaders []http.Header
		var seenQueries []string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":true,"last_id":"claude-opus-5-5"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5-5","display_name":"Sonnet 5.5"}],"has_more":false,"last_id":"claude-sonnet-5-5"}`))
		}))
		t.Cleanup(upstream.Close)

		home, lookup, resolver := newTestPaths(t, nil)
		// Seed Claude global config with an older Fable row so WithPrevious relabels it.
		claudeDir := filepath.Join(home, ".claude")
		if err := os.MkdirAll(claudeDir, 0o700); err != nil {
			t.Fatalf("mkdir .claude: %v", err)
		}
		globalCfg := `{"additionalModelOptionsCache":[{"value":"claude-fable-5-0","label":"Fable"}]}`
		if err := os.WriteFile(filepath.Join(claudeDir, ".claude.json"), []byte(globalCfg), 0o600); err != nil {
			t.Fatalf("write .claude.json: %v", err)
		}

		now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
		cache, err := modelcache.New(modelcache.Options{
			Lookup:           lookup,
			Paths:            resolver,
			ProxyBaseURL:     "http://127.0.0.1:9",
			AnthropicBaseURL: upstream.URL,
			DefaultBaseURL:   "http://127.0.0.1:19999",
			Client:           newTestClient(t),
			Clock:            func() time.Time { return now },
			Routes:           &route.Store{},
			Sentry:           &sentry.State{},
			BaseURL:          staticBaseURL("http://127.0.0.1:19999"),
			Pins:             staticPins{"fable": "claude-fable-5-1[1m]"},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
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

		// Authenticated request with allowed and disallowed headers.
		reqHeaders := http.Header{
			"X-Api-Key":       {secretAPIKey},
			"Anthropic-Beta":  {"prompt-caching-2024-07-31"},
			"User-Agent":      {"claude-code/2.1"},
			"Cookie":          {"session=must-not-forward"},
			"X-Custom-Secret": {"must-not-forward"},
		}
		rawResp, err := cache.ModelsResponse(t.Context(), reqHeaders)
		if err != nil {
			t.Fatalf("authenticated ModelsResponse: %v", err)
		}

		mu.Lock()
		gotHeaders := append([]http.Header(nil), seenHeaders...)
		gotQueries := append([]string(nil), seenQueries...)
		mu.Unlock()
		wantQueries := []string{"limit=1000", "limit=1000&after_id=claude-opus-5-5"}
		if diff := gocmp.Diff(wantQueries, gotQueries); diff != "" {
			t.Errorf("upstream catalog pagination queries mismatch (-want +got):\n%s", diff)
		}
		if len(gotHeaders) == 0 {
			t.Fatal("authenticated request did not contact upstream")
		}
		h := gotHeaders[0]
		if h.Get("X-Api-Key") != secretAPIKey || h.Get("Anthropic-Version") != "2023-06-01" || h.Get("Anthropic-Beta") != "prompt-caching-2024-07-31" {
			t.Errorf("forwarded allowed headers = %+v", h)
		}
		if h.Get("Cookie") != "" || h.Get("X-Custom-Secret") != "" {
			t.Errorf("disallowed headers leaked to upstream: %+v", h)
		}

		var body struct {
			Data    []map[string]any `json:"data"`
			HasMore bool             `json:"has_more"`
		}
		if err := json.Unmarshal(rawResp, &body); err != nil {
			t.Fatalf("unmarshal /v1/models response: %v", err)
		}
		if body.HasMore {
			t.Error("has_more = true, want false")
		}
		if len(body.Data) < 3 {
			t.Fatalf("expected previous + 2 anthropic + gateway rows, got %d", len(body.Data))
		}
		if body.Data[0]["id"] != "claude-fable-5-0" || body.Data[0]["display_name"] != "Fable 5.0 (previous)" {
			t.Errorf("first row = %+v, want relabelled previous Fable 5.0", body.Data[0])
		}
		if body.Data[1]["id"] != "claude-opus-5-5" || body.Data[2]["id"] != "claude-sonnet-5-5" {
			t.Errorf("anthropic rows = (%+v, %+v), want opus-5-5 then sonnet-5-5", body.Data[1], body.Data[2])
		}

		cacheFile := filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json")
		cachedBytes, err := os.ReadFile(cacheFile)
		if err != nil {
			t.Fatalf("read anthropic-models.json: %v", err)
		}
		if strings.Contains(string(cachedBytes), secretAPIKey) {
			t.Fatalf("API key persisted to anthropic-models.json: %s", cachedBytes)
		}
		info, err := os.Stat(cacheFile)
		if err != nil {
			t.Fatalf("stat anthropic-models.json: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("anthropic-models.json mode = %#o, want 0600", perm)
		}

		// Within 1 hour TTL, another call must not hit upstream again.
		if _, err := cache.ModelsResponse(t.Context(), reqHeaders); err != nil {
			t.Fatalf("cached ModelsResponse: %v", err)
		}
		mu.Lock()
		hitsAfter := len(seenHeaders)
		mu.Unlock()
		if hitsAfter != 2 {
			t.Errorf("fresh cache triggered extra upstream calls: total=%d, want 2", hitsAfter)
		}
	})

	t.Run("error: classified payload-free errors written via WriteFailure", func(t *testing.T) {
		tests := map[string]struct {
			handler   http.HandlerFunc
			wantError string
		}{
			"error: upstream non-200": {
				handler: func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(bodySecretSentinel))
				},
				wantError: "upstream HTTP 503",
			},
			"error: invalid catalog JSON": {
				handler: func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("{not-json " + bodySecretSentinel))
				},
				wantError: "invalid catalog JSON",
			},
			"error: invalid catalog data": {
				handler: func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"data":"` + bodySecretSentinel + `"}`))
				},
				wantError: "invalid catalog data",
			},
			"error: catalog too large": {
				handler: func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"data":[{"id":"claude-opus-5-5","display_name":"`)
					_, _ = io.WriteString(w, strings.Repeat("a", modelcache.MaxAnthropicPageBytes+64))
					_, _ = io.WriteString(w, `"}]}`)
				},
				wantError: "catalog too large",
			},
			"error: pagination limit on repeated last_id": {
				handler: func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"has_more":true,"last_id":"same-id"}`))
				},
				wantError: "catalog pagination limit",
			},
		}

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				upstream := httptest.NewServer(tt.handler)
				t.Cleanup(upstream.Close)

				home, lookup, resolver := newTestPaths(t, nil)
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://127.0.0.1:9",
					AnthropicBaseURL: upstream.URL,
					DefaultBaseURL:   "http://127.0.0.1:19999",
					Client:           newTestClient(t),
					Clock:            func() time.Time { return time.UnixMilli(5000000) },
					Routes:           &route.Store{},
					Sentry:           &sentry.State{},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if _, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {secretAPIKey}}); err != nil {
					t.Fatalf("ModelsResponse: %v", err)
				}
				cacheFile := filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json")
				raw, err := os.ReadFile(cacheFile)
				if err != nil {
					t.Fatalf("read anthropic-models.json: %v", err)
				}
				if strings.Contains(string(raw), bodySecretSentinel) || strings.Contains(string(raw), secretAPIKey) {
					t.Fatalf("payload or secret leaked into failure cache: %s", raw)
				}
				var saved struct {
					Error       string  `json:"error"`
					UpdatedAt   float64 `json:"updatedAt"`
					AttemptedAt float64 `json:"attemptedAt"`
				}
				if err := json.Unmarshal(raw, &saved); err != nil {
					t.Fatalf("unmarshal anthropic-models.json: %v", err)
				}
				if saved.Error != tt.wantError {
					t.Errorf("saved error = %q, want %q", saved.Error, tt.wantError)
				}
				if saved.UpdatedAt != 0 || saved.AttemptedAt != 5000000 {
					t.Errorf("timestamps = (updatedAt=%v, attemptedAt=%v), want (0, 5000000)", saved.UpdatedAt, saved.AttemptedAt)
				}
			})
		}
	})

	t.Run("success: stale cache serves immediately and deduplicates background refresh", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var calls atomic.Int32
			release := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				calls.Add(1)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5-5","display_name":"Sonnet 5.5"}],"has_more":false}`))
			})

			home, lookup, resolver := newTestPaths(t, nil)
			stateDir := filepath.Join(home, ".claude", "model-gateway")
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatalf("mkdir state dir: %v", err)
			}
			staleCache := `{"data":[{"id":"claude-opus-5-5","display_name":"Opus 5.5"}],"updatedAt":1000,"error":null}` + "\n"
			if err := os.WriteFile(filepath.Join(stateDir, "anthropic-models.json"), []byte(staleCache), 0o600); err != nil {
				t.Fatalf("write stale cache: %v", err)
			}

			now := time.UnixMilli(1000 + time.Hour.Milliseconds())
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:9",
				AnthropicBaseURL: "http://127.0.0.1:19992",
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:            func() time.Time { return now },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				BaseURL:          staticBaseURL("http://127.0.0.1:19999"),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer cache.Wait()

			headers := http.Header{"X-Api-Key": {secretAPIKey}}
			raw1, err := cache.ModelsResponse(t.Context(), headers)
			if err != nil {
				t.Fatalf("first stale ModelsResponse: %v", err)
			}
			if !strings.Contains(string(raw1), `"claude-opus-5-5"`) {
				t.Errorf("stale response did not serve cached row immediately: %s", raw1)
			}
			cache.TriggerAnthropicRefresh(t.Context(), headers)
			synctest.Wait()
			if got := calls.Load(); got != 1 {
				t.Fatalf("concurrent stale refresh upstream calls = %d, want 1", got)
			}

			close(release)
			cache.Wait()

			raw2, err := cache.ModelsResponse(t.Context(), headers)
			if err != nil {
				t.Fatalf("post-refresh ModelsResponse: %v", err)
			}
			if !strings.Contains(string(raw2), `"claude-sonnet-5-5"`) {
				t.Errorf("post-refresh response missing new Anthropic row: %s", raw2)
			}
			discoveryPath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
			discoveryRaw, err := os.ReadFile(discoveryPath)
			if err != nil {
				t.Fatalf("expected discovery cache synced on Anthropic refresh: %v", err)
			}
			if !strings.Contains(string(discoveryRaw), `"claude-sonnet-5-5"`) {
				t.Errorf("discovery cache missing refreshed Anthropic row: %s", discoveryRaw)
			}
		})
	})

	t.Run("error: per-page catalog timeout records classified message", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			})
			home, lookup, resolver := newTestPaths(t, nil)
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:9",
				AnthropicBaseURL: "http://127.0.0.1:19993",
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:            time.Now,
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {secretAPIKey}}); err != nil {
				t.Fatalf("ModelsResponse: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(home, ".claude", "model-gateway", "anthropic-models.json"))
			if err != nil {
				t.Fatalf("read anthropic-models.json: %v", err)
			}
			var saved struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatalf("unmarshal anthropic-models.json: %v", err)
			}
			if saved.Error != "catalog timeout" {
				t.Errorf("timeout error = %q, want %q", saved.Error, "catalog timeout")
			}
		})
	})
}

func TestDiscoveryCacheSync(t *testing.T) {
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(proxySrv.Close)

	const defaultURL = "http://127.0.0.1:19999"

	tests := map[string]struct {
		shadow       bool
		effectiveURL string
		wantState    string
		wantReason   string
		wantExists   bool
	}{
		"success: wired proxy writes discovery cache": {
			effectiveURL: defaultURL,
			wantState:    "wrote",
			wantExists:   true,
		},
		"success: shadow mode never writes discovery cache": {
			shadow:       true,
			effectiveURL: defaultURL,
			wantState:    "skipped",
			wantReason:   "shadow",
			wantExists:   false,
		},
		"success: legacy compat URL skips discovery cache": {
			effectiveURL: "http://api.anthropic.com",
			wantState:    "skipped",
			wantReason:   "rc-compatibility",
			wantExists:   false,
		},
		"success: unwired base URL skips discovery cache": {
			effectiveURL: "https://api.anthropic.com",
			wantState:    "skipped",
			wantReason:   "gateway-not-wired",
			wantExists:   false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, lookup, resolver := newTestPaths(t, nil)
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   proxySrv.URL,
				DefaultBaseURL: defaultURL,
				Shadow:         tt.shadow,
				Client:         newTestClient(t),
				Clock:          func() time.Time { return time.UnixMilli(1234567890) },
				Routes:         &route.Store{},
				Sentry:         &sentry.State{},
				BaseURL:        staticBaseURL(tt.effectiveURL),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := cache.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			cachePath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
			_, statErr := os.Stat(cachePath)
			if tt.wantExists && statErr != nil {
				t.Fatalf("expected discovery cache at %s: %v", cachePath, statErr)
			}
			if !tt.wantExists && !os.IsNotExist(statErr) {
				t.Fatalf("expected no discovery cache at %s, stat err = %v", cachePath, statErr)
			}

			if tt.wantExists {
				// Second sync without force must report unchanged.
				res, err := cache.SyncDiscoveryCache(false)
				if err != nil {
					t.Fatalf("SyncDiscoveryCache(false): %v", err)
				}
				if res.State != "unchanged" {
					t.Errorf("SyncDiscoveryCache(false) state = %q, want %q", res.State, "unchanged")
				}
				// Forced sync must report wrote.
				forced, err := cache.SyncDiscoveryCache(true)
				if err != nil {
					t.Fatalf("SyncDiscoveryCache(true): %v", err)
				}
				if forced.State != "wrote" {
					t.Errorf("SyncDiscoveryCache(true) state = %q, want %q", forced.State, "wrote")
				}
				raw, err := os.ReadFile(cachePath)
				if err != nil {
					t.Fatalf("read discovery cache: %v", err)
				}
				var doc struct {
					BaseURL   string `json:"baseUrl"`
					FetchedAt int64  `json:"fetchedAt"`
					Models    []struct {
						ID          string `json:"id"`
						DisplayName string `json:"display_name"`
					} `json:"models"`
				}
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatalf("unmarshal discovery cache: %v", err)
				}
				if doc.BaseURL != defaultURL || doc.FetchedAt != 1234567890 {
					t.Errorf("discovery cache header = (%q, %d), want (%q, 1234567890)", doc.BaseURL, doc.FetchedAt, defaultURL)
				}
				wantModels := []struct {
					ID          string `json:"id"`
					DisplayName string `json:"display_name"`
				}{
					{ID: "claude-gpt-6-astra[1m]", DisplayName: "GPT-6-astra (Codex)"},
					{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7"},
					{ID: "claude-grok-4.6[1m]", DisplayName: "Grok 4.6"},
					{ID: "claude-grok-4.5[1m]", DisplayName: "Grok 4.5"},
				}
				if diff := gocmp.Diff(wantModels, doc.Models); diff != "" {
					t.Errorf("discovery cache models mismatch (-want +got):\n%s", diff)
				}
			} else {
				res, err := cache.SyncDiscoveryCache(true)
				if err != nil {
					t.Fatalf("SyncDiscoveryCache(true): %v", err)
				}
				if res.State != tt.wantState || res.Reason != tt.wantReason {
					t.Errorf("SyncDiscoveryCache(true) = (%q, %q), want (%q, %q)", res.State, res.Reason, tt.wantState, tt.wantReason)
				}
			}
		})
	}
}
