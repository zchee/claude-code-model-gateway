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
	"bytes"
	"context"
	"encoding/base64"
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

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func TestMain(m *testing.M) {
	venue.Require()
	goleak.VerifyTestMain(m)
}

const (
	fixtureControlToken  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fixtureGrokAccess    = "grok-fixture-access-secret"
	fixtureGrokRefresh   = "grok-fixture-refresh-secret"
	fixtureAnthropicKey  = "sk-ant-fixture-secret"
	fixtureRotatedAccess = "rotated-grok-fixture-access"
	fixtureCompaction    = "You are a helpful AI assistant tasked with summarizing conversations."
)

func fixedClock() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() }

func mapLookup(env map[string]string) config.LookupEnv {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func assertNoSecrets(t *testing.T, label string, data []byte) {
	t.Helper()
	for _, secret := range []string{fixtureGrokAccess, fixtureRotatedAccess, fixtureGrokRefresh, fixtureAnthropicKey, fixtureControlToken} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("%s leaked fixture credential", label)
		}
	}
}

func mustDiff(t *testing.T, label string, want, got any) {
	t.Helper()
	if diff := gocmp.Diff(want, got); diff != "" {
		assertNoSecrets(t, label, []byte(diff))
		t.Fatalf("%s (-want +got):\n%s", label, diff)
	}
}

func providerModel(t *testing.T, r *http.Request) string {
	t.Helper()
	var doc struct {
		Model string `json:"model"`
	}
	if err := json.UnmarshalRead(r.Body, &doc); err != nil || doc.Model == "" {
		t.Error("provider received invalid request model")
		return ""
	}
	return doc.Model
}

func decodeJSONObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode JSON object: %v", err)
	}
	return out
}

// otlpCollector records posted OTLP JSON payloads on a loopback httptest server.
type otlpCollector struct {
	srv      *httptest.Server
	mu       sync.Mutex
	payloads [][]byte
}

func newOTLPCollector(t *testing.T, newServer func(http.Handler) *httptest.Server) *otlpCollector {
	t.Helper()
	c := &otlpCollector{}
	c.srv = newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		c.mu.Lock()
		c.payloads = append(c.payloads, body)
		c.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *otlpCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

func (c *otlpCollector) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.payloads))
	copy(out, c.payloads)
	return out
}

type otlpSpan struct {
	TraceID    string
	Name       string
	Attributes map[string]any
}

func decodeOTLPSpans(t *testing.T, payloads [][]byte) []otlpSpan {
	t.Helper()
	var spans []otlpSpan
	for _, raw := range payloads {
		assertNoSecrets(t, "OTLP trace payload", raw)
		var doc struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						TraceID    string          `json:"traceId"`
						Name       string          `json:"name"`
						Attributes []otlpAttribute `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode OTLP trace payload: %v", err)
		}
		for _, rs := range doc.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					spans = append(spans, otlpSpan{TraceID: s.TraceID, Name: s.Name, Attributes: decodeOTLPAttrs(t, s.Attributes)})
				}
			}
		}
	}
	return spans
}

const (
	traceOne   = "11111111111111111111111111111111"
	traceTwo   = "22222222222222222222222222222222"
	traceThree = "33333333333333333333333333333333"
)

func traced(id string) http.Header {
	return http.Header{"Traceparent": {"00-" + id + "-00f067aa0ba902b7-01"}}
}

func assertSpan(t *testing.T, spans []otlpSpan, id, status string, code int64) {
	t.Helper()
	n := 0
	for _, s := range spans {
		if s.TraceID == id {
			n++
			mustDiff(t, "span status", status, s.Attributes["status"])
			mustDiff(t, "span code", code, s.Attributes["status_code"])
		}
	}
	mustDiff(t, "spans for "+id, 1, n)
}

type otlpLogRecord struct {
	EventName  string
	Attributes map[string]any
}

func decodeOTLPLogs(t *testing.T, payloads [][]byte) []otlpLogRecord {
	t.Helper()
	var logs []otlpLogRecord
	for _, raw := range payloads {
		assertNoSecrets(t, "OTLP usage payload", raw)
		var doc struct {
			ResourceLogs []struct {
				ScopeLogs []struct {
					LogRecords []struct {
						EventName  string          `json:"eventName"`
						Attributes []otlpAttribute `json:"attributes"`
					} `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode OTLP usage payload: %v", err)
		}
		for _, rl := range doc.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					logs = append(logs, otlpLogRecord{EventName: lr.EventName, Attributes: decodeOTLPAttrs(t, lr.Attributes)})
				}
			}
		}
	}
	return logs
}

type otlpAttribute struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string  `json:"stringValue"`
		IntValue    *string  `json:"intValue"`
		DoubleValue *float64 `json:"doubleValue"`
		BoolValue   *bool    `json:"boolValue"`
	} `json:"value"`
}

func decodeOTLPAttrs(t *testing.T, attrs []otlpAttribute) map[string]any {
	t.Helper()
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		switch {
		case a.Value.StringValue != nil:
			out[a.Key] = *a.Value.StringValue
		case a.Value.IntValue != nil:
			var n int64
			if err := json.Unmarshal([]byte(*a.Value.IntValue), &n); err != nil {
				t.Fatalf("decode OTLP intValue for %q: %v", a.Key, err)
			}
			out[a.Key] = n
		case a.Value.DoubleValue != nil:
			out[a.Key] = *a.Value.DoubleValue
		case a.Value.BoolValue != nil:
			out[a.Key] = *a.Value.BoolValue
		default:
			// Key presence matters even when the value has an unknown OTLP shape.
			out[a.Key] = nil
		}
	}
	return out
}

func writeGrokAuthFile(t *testing.T, dir, issuer string, expiresAt time.Time) string {
	t.Helper()
	authPath := filepath.Join(dir, "auth.json")
	doc := map[string]any{
		"https://auth.x.ai::fixture-client": map[string]any{
			"key":            fixtureGrokAccess,
			"refresh_token":  fixtureGrokRefresh,
			"expires_at":     expiresAt.UnixMilli(),
			"oidc_client_id": "fixture-client",
			"oidc_issuer":    issuer,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture grok auth: %v", err)
	}
	if err := os.WriteFile(authPath, raw, 0o600); err != nil {
		t.Fatalf("write fixture grok auth: %v", err)
	}
	return authPath
}

type harnessConfig struct {
	env               map[string]string
	disableTrace      bool
	disableUsage      bool
	sentryDisabled    bool
	sentryEnvTrigger  float64
	windows           policy.Windows
	proxyModelsJSON   string
	anthropicHandler  http.HandlerFunc
	proxyHandler      http.HandlerFunc
	grokHandler       http.HandlerFunc
	grokAuthHandler   http.HandlerFunc
	grokAuthExpiry    time.Time
	grokCacheIDs      []string
	withReadiness     bool
	readinessProbe    func(context.Context) readiness.ProxyAuthResult
	compactRetries    *float64
	compactRetryDelay *float64
	replyRetries      *int
	replyRetryDelay   *time.Duration
	heartbeatInterval time.Duration
	inMemory          bool
	observeOptions    func(*worker.Options)
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type harness struct {
	t                *testing.T
	serviceCtx       context.Context
	cancelService    context.CancelFunc
	dir              string
	logPath          string
	grokAuthPath     string
	grokCachePath    string
	anthropicSrv     *httptest.Server
	proxySrv         *httptest.Server
	grokSrv          *httptest.Server
	grokAuthSrv      *httptest.Server
	traceCollector   *otlpCollector
	usageCollector   *otlpCollector
	gatewaySrv       *httptest.Server
	anthropicHits    atomic.Int64
	anthropicModel   atomic.Int64
	proxyCatalogHits atomic.Int64
	proxyHits        atomic.Int64
	grokHits         atomic.Int64
	grokAuthHits     atomic.Int64
	authRequests     atomic.Int64
	grokDials        atomic.Int64
	routeStore       *route.Store
	sentryState      *sentry.State
	modelCache       *modelcache.Cache
	upstreamClient   *upstream.Client
	upstreamState    *upstreamstate.Store
	usageEmitter     *usage.Emitter
	telemetryRec     *telemetry.Recorder
	readinessCheck   *readiness.SharedCheck
	workerHandler    http.Handler
}

func defaultProxyListing() string {
	return `{"data":[` +
		`{"id":"claude-opus-5-5","display_name":"claude-opus-5-5 (codex)"},` +
		`{"id":"gpt-6.1-sol","display_name":"gpt-6.1-sol (codex)"},` +
		`{"id":"gpt-5.6-sol","display_name":"gpt-5.6-sol (codex)"},` +
		`{"id":"gpt-5.6-terra","display_name":"gpt-5.6-terra (codex)"},` +
		`{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},` +
		`{"id":"lmstudio/ibm/granite-4-micro","display_name":"lmstudio/ibm/granite-4-micro (lmstudio)"},` +
		`{"id":"local/gguf","display_name":"local/gguf (local)"}` +
		`]}`
}

func newHarness(t *testing.T, cfg harnessConfig) *harness {
	t.Helper()
	serviceCtx, cancelService := context.WithCancel(t.Context())
	dir := t.TempDir()
	h := &harness{
		t:             t,
		serviceCtx:    serviceCtx,
		cancelService: cancelService,
		dir:           dir,
		logPath:       filepath.Join(dir, "logs", "request-routes.jsonl"),
		routeStore:    &route.Store{},
		sentryState:   &sentry.State{},
	}

	newServer := httptest.NewServer
	dial := (&net.Dialer{}).DialContext
	if cfg.inMemory {
		var lnMu sync.RWMutex
		listeners := map[string]*pipeListener{}
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			lnMu.RLock()
			ln := listeners[addr]
			lnMu.RUnlock()
			if ln == nil {
				return nil, errors.New("unknown in-memory endpoint")
			}
			return ln.DialContext(ctx, network, addr)
		}
		newServer = func(handler http.Handler) *httptest.Server {
			lnMu.Lock()
			addr := net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 20000 + len(listeners)}
			ln := &pipeListener{addr: &addr, conns: make(chan net.Conn), done: make(chan struct{})}
			listeners[addr.String()] = ln
			lnMu.Unlock()
			s := &httptest.Server{Listener: ln, Config: &http.Server{Handler: handler}}
			s.Start()
			s.Client().Transport.(*http.Transport).DialContext = dial
			return s
		}
	}
	h.traceCollector = newOTLPCollector(t, newServer)
	h.usageCollector = newOTLPCollector(t, newServer)

	proxyListing := cfg.proxyModelsJSON
	if proxyListing == "" {
		proxyListing = defaultProxyListing()
	}

	h.anthropicSrv = newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models") {
			h.anthropicModel.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6","type":"model"}],"has_more":false}`)
			return
		}
		h.anthropicHits.Add(1)
		if cfg.anthropicHandler != nil {
			cfg.anthropicHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_anthropic","type":"message","model":"claude-sonnet-4-6","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	t.Cleanup(h.anthropicSrv.Close)

	h.proxySrv = newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			h.proxyCatalogHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, proxyListing)
			return
		}
		h.proxyHits.Add(1)
		if cfg.proxyHandler != nil {
			cfg.proxyHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_proxy","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":12,"output_tokens":4}}`)
	}))
	t.Cleanup(h.proxySrv.Close)

	h.grokSrv = newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.grokHits.Add(1)
		if cfg.grokHandler != nil {
			cfg.grokHandler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_grok","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":15,"output_tokens":6}}`)
	}))
	t.Cleanup(h.grokSrv.Close)

	h.grokAuthSrv = newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.grokAuthHits.Add(1)
		if cfg.grokAuthHandler != nil {
			cfg.grokAuthHandler(w, r)
			return
		}
		if r.URL.Path == "/.well-known/openid-configuration" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"token_endpoint":"`+h.grokAuthSrv.URL+`/oauth/token"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"`+fixtureGrokAccess+`","refresh_token":"`+fixtureGrokRefresh+`","expires_in":3600}`)
	}))
	t.Cleanup(h.grokAuthSrv.Close)

	expiry := cfg.grokAuthExpiry
	if expiry.IsZero() {
		expiry = fixedClock().Add(time.Hour)
	}
	h.grokAuthPath = writeGrokAuthFile(t, dir, h.grokAuthSrv.URL, expiry)
	if len(cfg.grokCacheIDs) > 0 {
		h.grokCachePath = filepath.Join(dir, "models_cache.json")
		var rows []map[string]any
		for _, id := range cfg.grokCacheIDs {
			rows = append(rows, map[string]any{"id": id, "context_window": 256000})
		}
		raw, err := json.Marshal(map[string]any{"models": rows})
		if err != nil {
			t.Fatalf("marshal grok models cache: %v", err)
		}
		if err := os.WriteFile(h.grokCachePath, raw, 0o600); err != nil {
			t.Fatalf("write grok models cache: %v", err)
		}
	}

	localEngine := newServer(http.NotFoundHandler())
	t.Cleanup(localEngine.Close)
	env := map[string]string{
		"CCP_LMSTUDIO_BASE_URL":             localEngine.URL,
		"CODEX_GATEWAY_TELEMETRY_ENDPOINT":  h.traceCollector.srv.URL + "/v1/traces",
		"CODEX_GATEWAY_USAGE_ENDPOINT":      h.usageCollector.srv.URL + "/v1/logs",
		"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1",
	}
	if cfg.disableTrace {
		env["CODEX_GATEWAY_TELEMETRY_ENDPOINT"] = "0"
	}
	if cfg.disableUsage {
		env["CODEX_GATEWAY_USAGE_ENDPOINT"] = "0"
	}
	maps.Copy(env, cfg.env)
	lookup := mapLookup(env)

	upOpts := upstream.Options{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch addr {
		case h.grokSrv.Listener.Addr().String():
			h.grokDials.Add(1)
		case h.anthropicSrv.Listener.Addr().String(), h.proxySrv.Listener.Addr().String():
		default:
			return nil, errors.New("inference dial outside provider fixtures")
		}
		return dial(ctx, network, addr)
	}}
	h.upstreamClient = upstream.NewClient(upOpts)
	t.Cleanup(func() {
		cancelService()
		_ = h.upstreamClient.Close()
		if h.modelCache != nil {
			h.modelCache.Wait()
		}
		if h.readinessCheck != nil {
			h.readinessCheck.Wait()
		}
		if h.usageEmitter != nil {
			h.usageEmitter.Wait()
		}
		if h.telemetryRec != nil {
			h.telemetryRec.Wait()
		}
	})

	authTr := upstream.NewTransport(upstream.Options{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != h.grokAuthSrv.Listener.Addr().String() {
			return nil, errors.New("auth dial outside issuer fixture")
		}
		return dial(ctx, network, addr)
	}})
	if authTr == nil {
		t.Fatal("upstream.NewTransport returned nil (not implemented)")
	}
	t.Cleanup(authTr.CloseIdleConnections)

	grokBranch, err := compose.NewGrokBranch(compose.GrokOptions{
		Endpoint:        h.grokSrv.URL + "/v1/responses",
		AuthFile:        h.grokAuthPath,
		InferenceClient: h.upstreamClient,
		AuthTransport:   transportFunc(func(r *http.Request) (*http.Response, error) { h.authRequests.Add(1); return authTr.RoundTrip(r) }),
		Sentry:          h.sentryState,
		SentryEnabled:   !cfg.sentryDisabled,
		Now:             fixedClock,
	})
	if err != nil {
		t.Fatalf("compose.NewGrokBranch: %v", err)
	}

	var grokIDs func() []string
	if h.grokCachePath != "" {
		grokIDs = compose.NewGrokModelIDs(h.grokCachePath)
	}

	p := paths.New(lookup, func() (string, error) { return dir, nil }, func() (string, error) { return dir, nil })
	mc, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            p,
		Windows:          cfg.windows,
		ProxyBaseURL:     h.proxySrv.URL,
		AnthropicBaseURL: h.anthropicSrv.URL,
		DefaultBaseURL:   "http://127.0.0.1:18764",
		Shadow:           true,
		Client:           h.proxySrv.Client(),
		Clock:            fixedClock,
		Routes:           h.routeStore,
		Sentry:           h.sentryState,
		GrokIDs:          grokIDs,
	})
	if err != nil {
		t.Fatalf("modelcache.New: %v", err)
	}
	h.modelCache = mc
	if err := mc.Refresh(serviceCtx); err != nil {
		t.Fatalf("modelcache.Refresh: %v", err)
	}

	h.upstreamState = &upstreamstate.Store{
		Dir:         filepath.Join(dir, "state"),
		Now:         fixedClock,
		CommandPath: "/opt/model-gateway",
	}

	pr, err := proxyreq.New(proxyreq.Options{
		StartupDir:   dir,
		DispatchPath: filepath.Join(dir, "state", "dispatch-routes.json"),
		Lookup:       lookup,
		Now:          fixedClock,
		Windows:      cfg.windows,
	})
	if err != nil {
		t.Fatalf("proxyreq.New: %v", err)
	}

	guardSettings := compactguard.Settings{
		StreamGuard:      true,
		Retries:          2,
		MaxBufferedBytes: 16 * 1024 * 1024,
		RetryDelayMillis: 0,
	}
	if cfg.compactRetries != nil {
		guardSettings.Retries = *cfg.compactRetries
	}
	if cfg.compactRetryDelay != nil {
		guardSettings.RetryDelayMillis = *cfg.compactRetryDelay
	}
	guardRunner := &compactguard.Runner{
		ServiceContext: serviceCtx,
		Settings:       guardSettings,
		Stderr:         io.Discard,
	}

	usageBuilder := usage.NewBuilder(usage.Options{
		Now: fixedClock,
	})
	h.usageEmitter = usage.NewEmitter(lookup, usageBuilder)
	highWater := usage.NewHighWater(filepath.Join(dir, "highwater"), fixedClock)
	h.telemetryRec = telemetry.NewRecorder(telemetry.Options{
		Lookup:      lookup,
		LogPath:     h.logPath,
		Diagnostics: io.Discard,
		Now:         fixedClock,
	})

	if cfg.withReadiness {
		probeFn := cfg.readinessProbe
		if probeFn == nil {
			probeFn = func(context.Context) readiness.ProxyAuthResult {
				return readiness.ProxyAuthResult{
					ProxyBinary: true,
					ProxyModels: true,
					CodexAuth:   true,
					CheckedAt:   fixedClock(),
				}
			}
		}
		h.readinessCheck = readiness.NewSharedCheck(serviceCtx, probeFn, fixedClock)
	}

	workerOpts, err := compose.New(compose.Options{
		ServiceContext:    serviceCtx,
		Version:           "fixture-build",
		ControlToken:      fixtureControlToken,
		Lookup:            lookup,
		AnthropicBaseURL:  h.anthropicSrv.URL,
		ProxyBaseURL:      h.proxySrv.URL,
		CommandPath:       "/opt/model-gateway",
		InstalledVersion:  "fixture-build",
		LogsDir:           filepath.Join(dir, "logs"),
		Windows:           cfg.windows,
		SentryEnabled:     !cfg.sentryDisabled,
		SentryEnvTrigger:  cfg.sentryEnvTrigger,
		Routes:            h.routeStore,
		ModelCache:        mc,
		Sentry:            h.sentryState,
		Upstream:          h.upstreamClient,
		UpstreamState:     h.upstreamState,
		ProxyReq:          pr,
		CompactGuard:      guardRunner,
		Grok:              grokBranch,
		Usage:             h.usageEmitter,
		HighWater:         highWater,
		Telemetry:         h.telemetryRec,
		ReadinessCheck:    h.readinessCheck,
		Now:               fixedClock,
		Diagnostics:       io.Discard,
		ReplyRetries:      cfg.replyRetries,
		ReplyRetryDelay:   cfg.replyRetryDelay,
		HeartbeatInterval: cfg.heartbeatInterval,
	})
	if err != nil {
		t.Fatalf("compose.New: %v", err)
	}

	if cfg.observeOptions != nil {
		cfg.observeOptions(&workerOpts)
	}
	wh, err := worker.NewHandler(workerOpts)
	if err != nil {
		t.Fatalf("worker.NewHandler: %v", err)
	}
	h.workerHandler = wh
	h.gatewaySrv = newServer(wh)
	t.Cleanup(func() { cancelService(); h.gatewaySrv.Close() })
	return h
}

type httpResult struct {
	status int
	header http.Header
	body   []byte
	err    error
}

func (h *harness) request(method, path, body string, headers http.Header) httpResult {
	ctx, cancel := context.WithTimeout(h.t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, h.gatewaySrv.URL+path, strings.NewReader(body))
	if err != nil {
		return httpResult{err: err}
	}
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	resp, err := h.gatewaySrv.Client().Do(req)
	if err != nil {
		return httpResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	return httpResult{resp.StatusCode, resp.Header.Clone(), raw, err}
}

func (h *harness) do(method, path, body string, headers http.Header) (int, http.Header, []byte) {
	h.t.Helper()
	r := h.request(method, path, body, headers)
	if r.err != nil {
		h.t.Fatalf("%s %s failed: %T", method, path, r.err)
	}
	assertNoSecrets(h.t, "response body", r.body)
	return r.status, r.header, r.body
}

func (h *harness) startPost(body string, headers http.Header, release func()) func() int {
	var result httpResult
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result = h.request(http.MethodPost, "/v1/messages", body, headers)
	}()
	h.t.Cleanup(func() { release(); waitSignal(h.t, "request cleanup", finished, 6*time.Second) })
	return func() int {
		h.t.Helper()
		waitSignal(h.t, "response", finished, 6*time.Second)
		if result.err != nil {
			h.t.Fatalf("POST failed: %T", result.err)
		}
		assertNoSecrets(h.t, "response body", result.body)
		return result.status
	}
}

func (h *harness) highWaterPath(session string) string {
	return filepath.Join(h.dir, "highwater", base64.RawURLEncoding.EncodeToString([]byte(session))+".json")
}

func (h *harness) healthz() map[string]any {
	h.t.Helper()
	status, _, raw := h.do(http.MethodGet, "/healthz", "", nil)
	if status != http.StatusOK {
		h.t.Fatalf("GET /healthz status=%d", status)
	}
	return decodeJSONObject(h.t, raw)
}

func (h *harness) readAuditLines() []map[string]any {
	h.t.Helper()
	raw, err := os.ReadFile(h.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		h.t.Fatalf("read audit log: %v", err)
	}
	assertNoSecrets(h.t, "audit log", raw)
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	var rows []map[string]any
	for line := range strings.SplitSeq(trimmed, "\n") {
		rows = append(rows, decodeJSONObject(h.t, []byte(line)))
	}
	return rows
}

// pipeListener runs an in-memory HTTP server inside a synctest bubble without OS sockets.
type pipeListener struct {
	addr   net.Addr
	mu     sync.Mutex
	closed bool
	conns  chan net.Conn
	done   chan struct{}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.done)
	}
	return nil
}

func (l *pipeListener) Addr() net.Addr { return l.addr }

func (l *pipeListener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	c1, c2 := net.Pipe()
	select {
	case l.conns <- c2:
		return c1, nil
	case <-l.done:
		_ = c1.Close()
		_ = c2.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		_ = c1.Close()
		_ = c2.Close()
		return nil, ctx.Err()
	}
}

func waitSignal(t *testing.T, label string, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", label)
	}
}

func compactRequestJSON(model string) string {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"stream":     true,
		"system":     []map[string]any{{"type": "text", "text": fixtureCompaction}},
		"messages":   []map[string]any{{"role": "user", "content": "summarize"}},
	})
	return string(b)
}

func messageRequestJSON(model string, stream bool) string {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 16,
		"stream":     stream,
		"messages":   []map[string]any{{"role": "user", "content": "hello"}},
	})
	return string(b)
}
