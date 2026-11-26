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

package compose_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
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

func TestConstructionValidation(t *testing.T) {
	baseOptions := func(t *testing.T) compose.Options {
		t.Helper()
		dir := t.TempDir()
		lookup := mapLookup(map[string]string{
			"CODEX_GATEWAY_USAGE_ENDPOINT":     "0",
			"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
		})
		upClient := upstream.NewClient(upstream.Options{})
		t.Cleanup(func() { _ = upClient.Close() })
		pr, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: filepath.Join(dir, "dispatch.json"), Lookup: lookup, Now: fixedClock})
		if err != nil {
			t.Fatalf("proxyreq.New: %v", err)
		}
		gb, err := grok.NewBranch(grok.BranchOptions{Client: upClient, Now: fixedClock})
		if err != nil {
			t.Fatalf("grok.NewBranch: %v", err)
		}
		return compose.Options{
			ServiceContext:   t.Context(),
			Version:          "fixture-build",
			ControlToken:     fixtureControlToken,
			Lookup:           lookup,
			AnthropicBaseURL: "http://127.0.0.1:18766",
			ProxyBaseURL:     "http://127.0.0.1:18765",
			Routes:           &route.Store{},
			ModelCache:       &modelcache.Cache{},
			Sentry:           &sentry.State{},
			Upstream:         upClient,
			UpstreamState:    &upstreamstate.Store{Dir: filepath.Join(dir, "state"), Now: fixedClock},
			ProxyReq:         pr,
			CompactGuard:     &compactguard.Runner{ServiceContext: t.Context()},
			Grok:             gb,
			Usage:            usage.NewEmitter(lookup, usage.NewBuilder(usage.Options{Now: fixedClock})),
			Telemetry:        telemetry.NewRecorder(telemetry.Options{Lookup: lookup, Now: fixedClock}),
			Now:              fixedClock,
		}
	}

	tests := map[string]struct {
		mutate      func(*compose.Options)
		wantErrName string
	}{
		"success: all required collaborators supplied":     {},
		"error: nil ServiceContext":                        {mutate: func(o *compose.Options) { o.ServiceContext = nil }, wantErrName: "ServiceContext"},
		"error: nil Lookup":                                {mutate: func(o *compose.Options) { o.Lookup = nil }, wantErrName: "Lookup"},
		"error: empty AnthropicBaseURL":                    {mutate: func(o *compose.Options) { o.AnthropicBaseURL = "" }, wantErrName: "AnthropicBaseURL"},
		"error: empty ProxyBaseURL":                        {mutate: func(o *compose.Options) { o.ProxyBaseURL = "" }, wantErrName: "ProxyBaseURL"},
		"error: nil Routes":                                {mutate: func(o *compose.Options) { o.Routes = nil }, wantErrName: "Routes"},
		"error: nil ModelCache":                            {mutate: func(o *compose.Options) { o.ModelCache = nil }, wantErrName: "ModelCache"},
		"error: nil Sentry":                                {mutate: func(o *compose.Options) { o.Sentry = nil }, wantErrName: "Sentry"},
		"error: nil Upstream":                              {mutate: func(o *compose.Options) { o.Upstream = nil }, wantErrName: "Upstream"},
		"error: nil UpstreamState":                         {mutate: func(o *compose.Options) { o.UpstreamState = nil }, wantErrName: "UpstreamState"},
		"error: nil ProxyReq":                              {mutate: func(o *compose.Options) { o.ProxyReq = nil }, wantErrName: "ProxyReq"},
		"error: nil CompactGuard":                          {mutate: func(o *compose.Options) { o.CompactGuard = nil }, wantErrName: "CompactGuard"},
		"error: nil CompactGuard.ServiceContext fails now": {mutate: func(o *compose.Options) { o.CompactGuard.ServiceContext = nil }, wantErrName: "ServiceContext"},
		"error: nil Grok":                                  {mutate: func(o *compose.Options) { o.Grok = nil }, wantErrName: "Grok"},
		"error: nil Usage":                                 {mutate: func(o *compose.Options) { o.Usage = nil }, wantErrName: "Usage"},
		"error: nil Telemetry":                             {mutate: func(o *compose.Options) { o.Telemetry = nil }, wantErrName: "Telemetry"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			opts := baseOptions(t)
			var drains atomic.Int32
			var diagnostics strings.Builder
			opts.BeginDrain = func() { drains.Add(1) }
			opts.Diagnostics = &diagnostics
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			got, err := compose.New(opts)
			if tt.wantErrName == "" {
				if err != nil {
					assertNoSecrets(t, "compose.New error", []byte(err.Error()))
					t.Fatalf("compose.New unexpected error: %v", err)
				}
				if got.Anthropic == nil || got.Proxy == nil || got.Grok == nil || got.BeforeBody == nil || got.LogRoute == nil || got.Models == nil || got.HealthCatalog == nil || got.RefreshAnthropic == nil {
					t.Fatal("compose.New returned worker.Options with unset required callbacks")
				}
				if got.BeginDrain == nil || got.Diagnostics == nil {
					t.Fatal("drain or diagnostic hook missing")
				}
				got.BeginDrain()
				mustDiff(t, "bound drain callback", int32(1), drains.Load())
				if _, err := io.WriteString(got.Diagnostics, "diagnostic"); err != nil {
					t.Fatal("bound diagnostic writer failed")
				}
				mustDiff(t, "bound diagnostic output", "diagnostic", diagnostics.String())
				return
			}
			if err == nil {
				t.Fatalf("compose.New succeeded with missing %s, want error naming it", tt.wantErrName)
			}
			assertNoSecrets(t, "compose.New error", []byte(err.Error()))
			if !strings.Contains(err.Error(), tt.wantErrName) {
				t.Errorf("compose.New error = %q, want message naming %q", err.Error(), tt.wantErrName)
			}
		})
	}
}

type observingSentry struct {
	state *sentry.State
	onPub func(gen uint64)
}

func (o *observingSentry) Publish(generation uint64, resolver sentry.ModelResolver, windows policy.Windows, env float64) {
	o.state.Publish(generation, resolver, windows, env)
	o.onPub(generation)
}

type observingRoutes struct {
	store *route.Store
	onPub func(snap route.Snapshot)
}

func (o *observingRoutes) Publish(snap route.Snapshot) {
	o.onPub(snap)
	o.store.Publish(snap)
}

func TestOrderOfUse(t *testing.T) {
	t.Run("success: usage capture starts before branch runs and finalizes after observation", func(t *testing.T) {
		firstEntered := make(chan struct{})
		markFirst := sync.OnceFunc(func() { close(firstEntered) })
		releaseFirst := make(chan struct{})
		unblockFirst := sync.OnceFunc(func() { close(releaseFirst) })
		defer unblockFirst()

		h := newHarness(t, harnessConfig{
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				reqID := r.Header.Get("X-Claude-Code-Request-Id")
				if reqID == "req-order-1" {
					markFirst()
					select {
					case <-releaseFirst:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Request-Id", "resp-order-1")
					_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"first"}],"usage":{"input_tokens":31,"output_tokens":3}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Request-Id", "resp-order-2")
				_, _ = io.WriteString(w, `{"id":"msg_2","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"second"}],"usage":{"input_tokens":52,"output_tokens":5}}`)
			},
		})

		finish := h.startPost(messageRequestJSON("claude-gpt-6.1-sol[1m]", false), http.Header{"X-Claude-Code-Request-Id": {"req-order-1"}}, unblockFirst)

		waitSignal(t, "first request reaching proxy", firstEntered, 3*time.Second)

		// Request 2 starts and completes while Request 1 is still blocked at the proxy.
		s2, _, _ := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-gpt-6.1-sol[1m]", false), http.Header{
			"X-Claude-Code-Request-Id": {"req-order-2"},
		})
		mustDiff(t, "second request status", http.StatusOK, s2)

		unblockFirst()
		mustDiff(t, "first request status", http.StatusOK, finish())

		h.usageEmitter.Wait()
		logs := decodeOTLPLogs(t, h.usageCollector.snapshot())
		mustDiff(t, "emitted usage records", 2, len(logs))
		byReq := make(map[string]map[string]any, 2)
		for _, rec := range logs {
			if id, ok := rec.Attributes["request_id"].(string); ok {
				byReq[id] = rec.Attributes
			}
		}
		// Arrival-order sequence proves Usage.Start ran before upstream execution; token counts prove Finish ran after observation.
		mustDiff(t, "first request sequence", int64(1), byReq["resp-order-1"]["request_sequence"])
		mustDiff(t, "second request sequence", int64(2), byReq["resp-order-2"]["request_sequence"])
		mustDiff(t, "first request input_tokens", int64(31), byReq["resp-order-1"]["input_tokens"])
		mustDiff(t, "second request input_tokens", int64(52), byReq["resp-order-2"]["input_tokens"])
	})

	t.Run("success: model cache publishes sentry snapshot before routes on startup and refresh", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		native := httptest.NewServer(http.NotFoundHandler())
		defer native.Close()
		var sends atomic.Int32
		proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
				_, _ = io.WriteString(w, `{"data":[{"id":"kimi-fresh","display_name":"kimi-fresh (kimi)"}]}`)
				return
			}
			if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || providerModel(t, r) != "kimi-fresh" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sends.Add(1)
			_, _ = io.WriteString(w, `{"type":"message","model":"kimi-fresh","content":[]}`)
		}))
		defer proxySrv.Close()

		var mu sync.Mutex
		var events []string
		var latestSentryGen uint64
		routeStore := &route.Store{}
		sentryState := &sentry.State{}

		obsSentry := &observingSentry{
			state: sentryState,
			onPub: func(gen uint64) {
				mu.Lock()
				latestSentryGen = gen
				events = append(events, "sentry")
				mu.Unlock()
			},
		}
		obsRoutes := &observingRoutes{
			store: routeStore,
			onPub: func(snap route.Snapshot) {
				mu.Lock()
				defer mu.Unlock()
				if latestSentryGen == 0 {
					t.Error("routes published before any sentry publication")
				}
				events = append(events, "routes")
			},
		}

		dir := t.TempDir()
		lookup := mapLookup(map[string]string{
			"CODEX_GATEWAY_USAGE_ENDPOINT":     "0",
			"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
		})
		mc, err := modelcache.New(modelcache.Options{
			Lookup:           lookup,
			Paths:            paths.New(lookup, func() (string, error) { return dir, nil }, func() (string, error) { return dir, nil }),
			ProxyBaseURL:     proxySrv.URL,
			AnthropicBaseURL: native.URL,
			DefaultBaseURL:   "http://127.0.0.1:18764",
			Shadow:           true,
			Client:           proxySrv.Client(),
			Clock:            fixedClock,
			Routes:           obsRoutes,
			Sentry:           obsSentry,
		})
		if err != nil {
			t.Fatalf("modelcache.New: %v", err)
		}
		defer func() { cancel(); mc.Wait() }()

		// Build the handler against the initial snapshot, before the provider is listed.
		upClient := upstream.NewClient(upstream.Options{})
		defer func() { _ = upClient.Close() }()
		authTr := upstream.NewTransport(upstream.Options{})
		if authTr == nil {
			t.Fatal("upstream.NewTransport returned nil (not implemented)")
		}
		defer authTr.CloseIdleConnections()
		gb, err := compose.NewGrokBranch(compose.GrokOptions{
			Endpoint:        "http://127.0.0.1:1/v1/responses",
			AuthFile:        writeGrokAuthFile(t, dir, "http://127.0.0.1:1", fixedClock().Add(time.Hour)),
			InferenceClient: upClient,
			AuthTransport:   authTr,
			Sentry:          sentryState,
			SentryEnabled:   true,
			Now:             fixedClock,
		})
		if err != nil {
			t.Fatalf("compose.NewGrokBranch: %v", err)
		}
		pr, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: filepath.Join(dir, "dispatch.json"), Lookup: lookup, Now: fixedClock})
		if err != nil {
			t.Fatalf("proxyreq.New: %v", err)
		}
		workerOpts, err := compose.New(compose.Options{
			ServiceContext:   ctx,
			Version:          "fixture-build",
			ControlToken:     fixtureControlToken,
			Lookup:           lookup,
			AnthropicBaseURL: native.URL,
			ProxyBaseURL:     proxySrv.URL,
			Routes:           routeStore,
			ModelCache:       mc,
			Sentry:           sentryState,
			Upstream:         upClient,
			UpstreamState:    &upstreamstate.Store{Dir: filepath.Join(dir, "state"), Now: fixedClock},
			ProxyReq:         pr,
			CompactGuard:     &compactguard.Runner{ServiceContext: ctx},
			Grok:             gb,
			Usage:            usage.NewEmitter(lookup, usage.NewBuilder(usage.Options{Now: fixedClock})),
			Telemetry:        telemetry.NewRecorder(telemetry.Options{Lookup: lookup, Now: fixedClock}),
			Now:              fixedClock,
		})
		if err != nil {
			t.Fatalf("compose.New: %v", err)
		}
		wh, err := worker.NewHandler(workerOpts)
		if err != nil {
			t.Fatalf("worker.NewHandler: %v", err)
		}
		post := func() int {
			rec := httptest.NewRecorder()
			wh.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(messageRequestJSON("claude-kimi-fresh", false))))
			return rec.Code
		}
		mustDiff(t, "unlisted model goes to native fixture", http.StatusNotFound, post())
		if err := mc.Refresh(ctx); err != nil {
			t.Fatalf("modelcache.Refresh: %v", err)
		}
		mu.Lock()
		gotEvents := slices.Clone(events)
		mu.Unlock()
		mustDiff(t, "publication order across startup and refresh", []string{"sentry", "routes", "sentry", "routes"}, gotEvents)
		mustDiff(t, "new route on same handler", http.StatusOK, post())
		mustDiff(t, "fresh model reached proxy", int32(1), sends.Load())
		rec := httptest.NewRecorder()
		wh.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/healthz", nil))
		mustDiff(t, "composed /healthz status", http.StatusOK, rec.Code)
		health := decodeJSONObject(t, rec.Body.Bytes())
		mustDiff(t, "composed /healthz catalog source", any("proxy"), health["catalog"])
	})
}

func TestGrokCompositionCallSites(t *testing.T) {
	t.Run("error: NewGrokBranch rejects nil auth transport and nil inference client", func(t *testing.T) {
		dir := t.TempDir()
		authFile := writeGrokAuthFile(t, dir, "http://127.0.0.1:1", fixedClock().Add(time.Hour))
		upClient := upstream.NewClient(upstream.Options{})
		defer func() { _ = upClient.Close() }()

		tests := map[string]struct {
			nilTransport bool
		}{
			"error: nil AuthTransport":   {nilTransport: true},
			"error: nil InferenceClient": {nilTransport: false},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				opts := compose.GrokOptions{
					AuthFile: authFile,
					Now:      fixedClock,
				}
				if tt.nilTransport {
					opts.InferenceClient = upClient
				} else {
					authTr := upstream.NewTransport(upstream.Options{})
					if authTr == nil {
						t.Fatal("upstream.NewTransport returned nil (not implemented)")
					}
					defer authTr.CloseIdleConnections()
					opts.AuthTransport = authTr
				}
				branch, err := compose.NewGrokBranch(opts)
				if err == nil {
					t.Fatal("compose.NewGrokBranch succeeded, want error")
				}
				if branch != nil {
					t.Error("compose.NewGrokBranch returned non-nil branch on error")
				}
				assertNoSecrets(t, "NewGrokBranch error", []byte(err.Error()))
				if strings.Contains(err.Error(), "not implemented") {
					t.Fatalf("compose.NewGrokBranch: %v", err)
				}
			})
		}
	})

	t.Run("success: NewGrokBranch isolates auth transport from inference client and refreshes without leaking secrets", func(t *testing.T) {
		var authHits atomic.Int32
		var seenClientHeader atomic.Bool

		// Token is already expired so Handle must refresh via the isolated auth transport.
		h := newHarness(t, harnessConfig{
			grokAuthExpiry: fixedClock().Add(-time.Minute),
			grokAuthHandler: func(w http.ResponseWriter, r *http.Request) {
				n := authHits.Add(1)
				for _, key := range []string{"Authorization", "X-Api-Key", "X-Fixture-Client", "X-Grok-Client-Version", "X-Grok-Model-Override", "X-Grok-Client-Identifier"} {
					if r.Header.Get(key) != "" {
						t.Errorf("auth received forbidden header %s", key)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}
				if n == 1 && r.Method == http.MethodGet && r.URL.RequestURI() == "/.well-known/openid-configuration" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"token_endpoint":"`+"http://"+r.Host+`/oauth/token"}`)
					return
				}
				if n != 2 || r.Method != http.MethodPost || r.URL.RequestURI() != "/oauth/token" || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") || r.ParseForm() != nil || len(r.PostForm) != 3 || r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("client_id") != "fixture-client" || r.PostForm.Get("refresh_token") != fixtureGrokRefresh {
					t.Error("incorrect token exchange method, path, headers or form")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"access_token":"`+fixtureRotatedAccess+`","refresh_token":"`+fixtureGrokRefresh+`","expires_in":3600}`)
			},
			grokHandler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+fixtureRotatedAccess || r.Method != http.MethodPost || r.URL.RequestURI() != "/v1/responses" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Fixture-Client") != "" {
					seenClientHeader.Store(true)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_iso","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`)
			},
		})

		status, _, body := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-grok-4.7[1m]", false), http.Header{
			"X-Api-Key": {fixtureAnthropicKey}, "X-Fixture-Client": {"only-for-client"},
		})
		mustDiff(t, "grok refreshed inference status", http.StatusOK, status)
		assertNoSecrets(t, "grok response body", body)
		mustDiff(t, "discovery then exchange", int32(2), authHits.Load())
		if h.authRequests.Load() != 2 || h.grokDials.Load() == 0 {
			t.Fatal("auth or inference bypassed the dedicated transport")
		}
		mustDiff(t, "inference requests", int64(1), h.grokHits.Load())
		if seenClientHeader.Load() {
			t.Error("client X-Api-Key leaked to Grok upstream")
		}
	})

	t.Run("success: NewGrokModelIDs wires modelcache custom grok ids into worker routing", func(t *testing.T) {
		var mu sync.Mutex
		var seenModel string
		h := newHarness(t, harnessConfig{
			grokCacheIDs: []string{"grok-private.2"},
			grokHandler: func(w http.ResponseWriter, r *http.Request) {
				model := providerModel(t, r)
				mu.Lock()
				seenModel = model
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_priv","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`)
			},
		})

		status, _, _ := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-grok-private-2", false), nil)
		mustDiff(t, "custom cached grok model status", http.StatusOK, status)
		mu.Lock()
		defer mu.Unlock()
		mustDiff(t, "resolved cached grok backend model", "grok-private.2", seenModel)
	})

	t.Run("success: NewGrokReadiness wires catalog without refreshing auth", func(t *testing.T) {
		var authHits atomic.Int32
		authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer authSrv.Close()

		dir := t.TempDir()
		validExpired := writeGrokAuthFile(t, dir, authSrv.URL, fixedClock().Add(-time.Hour))
		invalidPath := filepath.Join(dir, "invalid-auth.json")
		if err := os.WriteFile(invalidPath, []byte(`{"not-grok":{}}`), 0o600); err != nil {
			t.Fatalf("write invalid auth: %v", err)
		}
		missingPath := filepath.Join(dir, "missing-auth.json")

		tests := map[string]struct {
			file      string
			wantReady bool
			wantState string
		}{
			"success: valid auth file reports ready without network refresh": {file: validExpired, wantReady: true, wantState: "ready"},
			"error: invalid auth file reports auth-invalid without refresh":  {file: invalidPath, wantReady: false, wantState: "auth-invalid"},
			"error: missing auth file reports auth-missing without refresh":  {file: missingPath, wantReady: false, wantState: "auth-missing"},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				readGrok := compose.NewGrokReadiness(tt.file)
				if readGrok == nil {
					t.Fatal("compose.NewGrokReadiness returned nil (not implemented)")
				}
				src := catalog.NewLiveReadinessSource(catalog.LiveReadinessOptions{
					Lookup:   mapLookup(nil),
					ReadGrok: readGrok,
				})
				got := src.Probe(t.Context(), "grok")
				mustDiff(t, "grok readiness ready", tt.wantReady, got.Ready)
				mustDiff(t, "grok readiness state", tt.wantState, got.State)
				mustDiff(t, "grok readiness must never refresh over network", int32(0), authHits.Load())
			})
		}
	})
}
