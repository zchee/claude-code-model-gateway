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

package localprov_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxyconfig"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

const (
	studioTestSecret     = "lmstudio-test-token-never-printed"
	studioRejectedSecret = "lmstudio-rejected-marker-73c1"
	studioRedirectSecret = "lmstudio-redirect-marker-49a7"
)

func redactStudioMarkers(s string) string {
	return strings.NewReplacer(
		studioTestSecret, "<studio-test-secret>",
		studioRejectedSecret, "<studio-rejected-secret>",
		studioRedirectSecret, "<studio-redirect-secret>",
	).Replace(s)
}

func mapLookup(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

type seenRequest struct {
	URL           string
	HasAuthHeader bool
	AuthValues    []string
}

type fakeStudioServer struct {
	baseURL string
	mu      sync.Mutex
	seen    []seenRequest
}

func (f *fakeStudioServer) requests() []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]seenRequest, len(f.seen))
	copy(out, f.seen)
	return out
}

type studioInstanceSpec struct {
	id            string
	contextLength int64
}

func studioCatalogModel(key string, instances []studioInstanceSpec, kind string) map[string]any {
	loaded := make([]map[string]any, 0, len(instances))
	for _, inst := range instances {
		loaded = append(loaded, map[string]any{
			"id": inst.id,
			"config": map[string]any{
				"context_length":  inst.contextLength,
				"eval_batch_size": 2048,
				"parallel":        4,
				"flash_attention": true,
			},
			"remaining_ttl_seconds": 900,
		})
	}
	return map[string]any{
		"type":               kind,
		"publisher":          "example",
		"key":                key,
		"display_name":       key,
		"max_context_length": 262144,
		"format":             "gguf",
		"capabilities": map[string]any{
			"vision":               false,
			"trained_for_tool_use": true,
		},
		"loaded_instances": loaded,
	}
}

func startFakeStudio(t *testing.T, models func() []map[string]any, status int, requireToken, rawBody string) *fakeStudioServer {
	t.Helper()
	fake := &fakeStudioServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authValues, hasAuth := r.Header["Authorization"]
		fake.mu.Lock()
		fake.seen = append(fake.seen, seenRequest{
			URL:           r.URL.RequestURI(),
			HasAuthHeader: hasAuth,
			AuthValues:    slices.Clone(authValues),
		})
		fake.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if requireToken != "" && r.Header.Get("Authorization") != "Bearer "+requireToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"An LM Studio API token is required to make requests to this server.","type":"invalid_api_key"}}`))
			return
		}
		if rawBody != "" {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(rawBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		data, err := json.Marshal(map[string]any{"models": models()})
		if err != nil {
			t.Errorf("marshal studio models: %v", err)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	fake.baseURL = srv.URL
	return fake
}

// reserveLoopbackPort binds an ephemeral port while other servers remain open so the OS guarantees
// a distinct port; the returned release function closes the listener right before an outage probe.
func reserveLoopbackPort(t *testing.T) (uint16, func()) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral port: %v", err)
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	var once sync.Once
	release := func() {
		once.Do(func() {
			if err := ln.Close(); err != nil {
				t.Errorf("close ephemeral port %d: %v", port, err)
			}
		})
	}
	t.Cleanup(release)
	return port, release
}

func closedLoopbackPort(t *testing.T) uint16 {
	t.Helper()
	port, release := reserveLoopbackPort(t)
	release()
	return port
}

func startFakeLlama(t *testing.T, nCtx func() int64, status int) (uint16, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/props" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			data, err := json.Marshal(map[string]any{
				"default_generation_settings": map[string]any{"n_ctx": nCtx()},
				"total_slots":                 4,
				"model_alias":                 "gguf",
			})
			if err != nil {
				t.Errorf("marshal llama props: %v", err)
				return
			}
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	port := uint16(srv.Listener.Addr().(*net.TCPAddr).Port)
	return port, srv.Close
}

func TestProbeStudio(t *testing.T) {
	emptyDir := t.TempDir()
	catalog := []map[string]any{
		studioCatalogModel("qwen3-coder-30b", []studioInstanceSpec{{"qwen3-coder-30b", 65536}}, "llm"),
		studioCatalogModel("ibm/granite-4-micro", nil, "llm"),
		studioCatalogModel("qwen/qwen3-4b@q4_k_m", []studioInstanceSpec{{"small-copy", 8192}, {"qwen/qwen3-4b@q4_k_m", 131072}}, "llm"),
		studioCatalogModel("renamed-only", []studioInstanceSpec{{"first", 40960}, {"second", 16384}}, "llm"),
		studioCatalogModel("loaded-without-a-context", []studioInstanceSpec{{"loaded-without-a-context", 0}}, "llm"),
		studioCatalogModel("text-embedding-nomic-embed-text-v1.5", nil, "embedding"),
		{"type": "llm", "key": ""},
		{"type": "llm"},
	}
	expectedModels := []localprov.Model{
		{Key: "qwen3-coder-30b", Loaded: true, ContextWindow: 65536},
		{Key: "ibm/granite-4-micro", Loaded: false, ContextWindow: 0},
		{Key: "qwen/qwen3-4b@q4_k_m", Loaded: true, ContextWindow: 131072},
		{Key: "renamed-only", Loaded: true, ContextWindow: 16384},
		{Key: "loaded-without-a-context", Loaded: true, ContextWindow: 0},
	}

	open := startFakeStudio(t, func() []map[string]any { return catalog }, http.StatusOK, "", "")
	locked := startFakeStudio(t, func() []map[string]any { return catalog }, http.StatusOK, studioTestSecret, "")
	old := startFakeStudio(t, func() []map[string]any { return catalog }, http.StatusNotFound, "", "")
	garbled := startFakeStudio(t, func() []map[string]any { return catalog }, http.StatusOK, "", "<html>not LM Studio</html>")
	refused := "http://127.0.0.1:" + strconv.Itoa(int(closedLoopbackPort(t)))

	tests := map[string]struct {
		env  map[string]string
		want localprov.StudioState
	}{
		"success: a running server": {
			env:  map[string]string{"CCP_LMSTUDIO_BASE_URL": open.baseURL},
			want: localprov.StudioState{Enabled: true, BaseURL: open.baseURL, Reachable: true, Models: expectedModels},
		},
		"success: a server that requires the token it is given": {
			env:  map[string]string{"CCP_LMSTUDIO_BASE_URL": locked.baseURL, "CCP_LMSTUDIO_API_KEY": studioTestSecret},
			want: localprov.StudioState{Enabled: true, BaseURL: locked.baseURL, HasAPIKey: true, Reachable: true, Models: expectedModels},
		},
		"error: a server that requires a token and gets none": {
			env:  map[string]string{"CCP_LMSTUDIO_BASE_URL": locked.baseURL},
			want: localprov.StudioState{Enabled: true, BaseURL: locked.baseURL, Reachable: true, AuthRequired: true},
		},
		"error: a server that refuses the token it is given": {
			env:  map[string]string{"CCP_LMSTUDIO_BASE_URL": locked.baseURL, "LM_API_TOKEN": studioRejectedSecret},
			want: localprov.StudioState{Enabled: true, BaseURL: locked.baseURL, HasAPIKey: true, Reachable: true, AuthRequired: true},
		},
		"error: a server without the listing, as before LM Studio 0.4.1": {
			env:  map[string]string{"CCP_LMSTUDIO_BASE_URL": old.baseURL},
			want: localprov.StudioState{Enabled: true, BaseURL: old.baseURL, Error: "GET /api/v1/models answered HTTP 404"},
		},
		"error: something that is not LM Studio": {
			env:  map[string]string{"CCP_LMSTUDIO_BASE_URL": garbled.baseURL},
			want: localprov.StudioState{Enabled: true, BaseURL: garbled.baseURL, Error: "GET /api/v1/models returned no model list"},
		},
		"error: nothing listening": {
			env:  map[string]string{"CCP_LMSTUDIO_BASE_URL": refused},
			want: localprov.StudioState{Enabled: true, BaseURL: refused, Error: "ECONNREFUSED"},
		},
		"error: turned off": {
			env:  map[string]string{"CCP_LMSTUDIO_ENABLED": "off", "CCP_LMSTUDIO_BASE_URL": open.baseURL},
			want: localprov.StudioState{Enabled: false},
		},
		"error: an invalid server setting is never contacted": {
			env: map[string]string{"CCP_LMSTUDIO_BASE_URL": open.baseURL + "/v1", "CCP_LMSTUDIO_API_KEY": studioTestSecret},
			want: localprov.StudioState{
				Enabled: true,
				Invalid: &proxyconfig.InvalidSetting{
					Setting: "CCP_LMSTUDIO_BASE_URL",
					Reason:  "must be a scheme, a host and a port only, without a path such as /v1",
				},
			},
		},
	}

	// Run in deterministic order so the Authorization sequence observed by locked matches the table.
	order := []string{
		"success: a running server",
		"success: a server that requires the token it is given",
		"error: a server that requires a token and gets none",
		"error: a server that refuses the token it is given",
		"error: a server without the listing, as before LM Studio 0.4.1",
		"error: something that is not LM Studio",
		"error: nothing listening",
		"error: turned off",
		"error: an invalid server setting is never contacted",
	}

	results := make(map[string]localprov.StudioState, len(tests))
	for _, name := range order {
		results[name] = localprov.ProbeStudio(t.Context(), mapLookup(tests[name].env), emptyDir, localprov.ProbeOptions{})
	}

	t.Run("states", func(t *testing.T) {
		for _, name := range order {
			got := results[name]
			if diff := gocmp.Diff(tests[name].want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("%s: StudioState mismatch (-want +got):\n%s", name, redactStudioMarkers(diff))
			}
		}
	})

	t.Run("no_token_in_state", func(t *testing.T) {
		tokenPattern := regexp.MustCompile(regexp.QuoteMeta(studioTestSecret) + `|` + regexp.QuoteMeta(studioRejectedSecret))
		for _, name := range order {
			raw, err := json.Marshal(results[name])
			if err != nil {
				t.Fatalf("%s: marshal state: %v", name, err)
			}
			if tokenPattern.Match(raw) {
				t.Errorf("%s: token marker leaked into serialized StudioState", name)
			}
		}
	})

	t.Run("open_requests", func(t *testing.T) {
		want := []seenRequest{{URL: "/api/v1/models", HasAuthHeader: false, AuthValues: nil}}
		if diff := gocmp.Diff(want, open.requests()); diff != "" {
			t.Errorf("open server requests mismatch (-want +got):\n%s", redactStudioMarkers(diff))
		}
	})

	t.Run("locked_requests", func(t *testing.T) {
		want := []seenRequest{
			{URL: "/api/v1/models", HasAuthHeader: true, AuthValues: []string{"Bearer " + studioTestSecret}},
			{URL: "/api/v1/models", HasAuthHeader: false, AuthValues: nil},
			{URL: "/api/v1/models", HasAuthHeader: true, AuthValues: []string{"Bearer " + studioRejectedSecret}},
		}
		if diff := gocmp.Diff(want, locked.requests()); diff != "" {
			t.Errorf("locked server requests mismatch (-want +got):\n%s", redactStudioMarkers(diff))
		}
	})
}

func TestRefreshStudioState(t *testing.T) {
	emptyDir := t.TempDir()
	var mu sync.Mutex
	instances := []studioInstanceSpec{{"qwen3-coder-30b", 65536}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		cur := append([]studioInstanceSpec(nil), instances...)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		data, err := json.Marshal(map[string]any{"models": []map[string]any{studioCatalogModel("qwen3-coder-30b", cur, "llm")}})
		if err != nil {
			t.Errorf("marshal models: %v", err)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)

	// Reserve a second distinct port while srv is still bound so the OS cannot reassign srv's port.
	movedPort, releaseMovedPort := reserveLoopbackPort(t)

	studioRoutes := []proxymodels.Route{
		{ID: "lmstudio/qwen3-coder-30b", Provider: "lmstudio"},
		{ID: "lmstudio/ibm/granite-4-micro", Provider: "lmstudio"},
		{ID: "lmstudio/qwen/qwen3-4b@q4_k_m", Provider: "lmstudio"},
	}
	env := mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": srv.URL})
	var tracker localprov.StudioTracker

	windowFor := func(state *localprov.StudioState, key string) int64 {
		if state == nil {
			t.Fatalf("expected non-nil StudioState")
		}
		for _, m := range state.Models {
			if m.Key == key {
				return m.ContextWindow
			}
		}
		t.Fatalf("model %q missing from state %+v", key, state)
		return 0
	}

	first := tracker.Refresh(t.Context(), studioRoutes, env, emptyDir, localprov.ProbeOptions{})
	if got := windowFor(first, "qwen3-coder-30b"); got != 65536 {
		t.Fatalf("initial contextWindow = %d, want 65536", got)
	}

	mu.Lock()
	instances = nil
	mu.Unlock()
	unloaded := tracker.Refresh(t.Context(), studioRoutes, env, emptyDir, localprov.ProbeOptions{})
	if got := windowFor(unloaded, "qwen3-coder-30b"); got != 0 {
		t.Fatalf("unloaded contextWindow = %d, want 0", got)
	}

	mu.Lock()
	instances = []studioInstanceSpec{{"qwen3-coder-30b", 40960}}
	mu.Unlock()
	reloaded := tracker.Refresh(t.Context(), studioRoutes, env, emptyDir, localprov.ProbeOptions{})
	if got := windowFor(reloaded, "qwen3-coder-30b"); got != 40960 {
		t.Fatalf("reloaded contextWindow = %d, want 40960", got)
	}

	srv.Close()
	down := tracker.Refresh(t.Context(), studioRoutes, env, emptyDir, localprov.ProbeOptions{})
	if down == nil {
		t.Fatal("stopped server refresh returned nil")
	}
	if down.Reachable {
		t.Error("stopped server reported Reachable = true, want false")
	}
	if got := windowFor(down, "qwen3-coder-30b"); got != 40960 {
		t.Errorf("stopped server contextWindow = %d, want preserved 40960", got)
	}
	snap := tracker.State()
	if snap == nil {
		t.Fatal("tracker.State() returned nil during outage")
	}
	if diff := gocmp.Diff(down, snap); diff != "" {
		t.Errorf("tracker.State() mismatch (-want +got):\n%s", diff)
	}
	// Policy resolution uses the tracked snapshot, and mutating returned copies must not alter tracked state or the resolver.
	resolver := policy.NewResolver(policy.ResolverInputs{
		Routes:          proxymodels.NewTable(studioRoutes),
		LMStudioURL:     snap.BaseURL,
		LMStudioWindows: snap.Windows(),
	})
	if got := resolver.AdvertisedWindow("claude-lmstudio/qwen3-coder-30b"); got != 40960 {
		t.Errorf("resolver advertised window during outage = %d, want 40960", got)
	}
	if len(down.Models) > 0 {
		down.Models[0].ContextWindow = 999999
	}
	if len(snap.Models) > 0 {
		snap.Models[0].ContextWindow = 111111
		snap.Models = append(snap.Models, localprov.Model{Key: "injected", Loaded: true, ContextWindow: 50000})
		windowsCopy := snap.Windows()
		windowsCopy["qwen3-coder-30b"] = 12345
	}
	afterMutation := tracker.State()
	if afterMutation == nil {
		t.Fatal("tracker.State() returned nil after copy mutation")
	}
	if len(afterMutation.Models) != 1 || windowFor(afterMutation, "qwen3-coder-30b") != 40960 {
		t.Errorf("tracked state mutated through returned copy: %+v", afterMutation)
	}
	if got := resolver.AdvertisedWindow("claude-lmstudio/qwen3-coder-30b"); got != 40960 {
		t.Errorf("resolver advertised window mutated to %d, want 40960", got)
	}
	freshResolver := policy.NewResolver(policy.ResolverInputs{
		Routes:          proxymodels.NewTable(studioRoutes),
		LMStudioURL:     afterMutation.BaseURL,
		LMStudioWindows: afterMutation.Windows(),
	})
	if got := freshResolver.AdvertisedWindow("claude-lmstudio/qwen3-coder-30b"); got != 40960 {
		t.Errorf("fresh resolver advertised window after copy mutation = %d, want 40960", got)
	}

	releaseMovedPort()
	movedEnv := mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": "http://127.0.0.1:" + strconv.Itoa(int(movedPort))})
	moved := tracker.Refresh(t.Context(), studioRoutes, movedEnv, emptyDir, localprov.ProbeOptions{})
	if moved == nil {
		t.Fatal("moved server refresh returned nil")
	}
	if len(moved.Models) != 0 {
		t.Errorf("moved server retained %d models, want 0", len(moved.Models))
	}

	cleared := tracker.Refresh(t.Context(), []proxymodels.Route{{ID: "kimi-k3", Provider: "kimi"}}, env, emptyDir, localprov.ProbeOptions{})
	if cleared != nil {
		t.Errorf("unlisted provider refresh = %+v, want nil", cleared)
	}
	if got := tracker.State(); got != nil {
		t.Errorf("tracker.State() after unlisted provider = %+v, want nil", got)
	}

	// Mutating Invalid on copies returned by Refresh or State must not alter tracked state.
	var invalidTracker localprov.StudioTracker
	wantInvalid := &proxyconfig.InvalidSetting{
		Setting: "CCP_LMSTUDIO_BASE_URL",
		Reason:  "must be a scheme, a host and a port only, without a path such as /v1",
	}
	invalidEnv := mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": "http://127.0.0.1:1234/v1"})
	invalidRefreshed := invalidTracker.Refresh(t.Context(), studioRoutes, invalidEnv, emptyDir, localprov.ProbeOptions{})
	if invalidRefreshed == nil || invalidRefreshed.Invalid == nil {
		t.Fatalf("invalid-setting Refresh = %+v, want non-nil Invalid", invalidRefreshed)
	}
	if diff := gocmp.Diff(wantInvalid, invalidRefreshed.Invalid); diff != "" {
		t.Errorf("invalid-setting Refresh Invalid mismatch (-want +got):\n%s", diff)
	}
	invalidRefreshed.Invalid.Setting = "MUTATED_FROM_REFRESH"
	invalidRefreshed.Invalid.Reason = "mutated from refresh"
	invalidSnap := invalidTracker.State()
	if invalidSnap == nil || invalidSnap.Invalid == nil {
		t.Fatalf("invalid-setting State() = %+v, want non-nil Invalid", invalidSnap)
	}
	if diff := gocmp.Diff(wantInvalid, invalidSnap.Invalid); diff != "" {
		t.Errorf("invalid-setting State() mutated through Refresh copy (-want +got):\n%s", diff)
	}
	invalidSnap.Invalid.Setting = "MUTATED_FROM_STATE"
	invalidSnap.Invalid.Reason = "mutated from state"
	invalidAfter := invalidTracker.State()
	if invalidAfter == nil || invalidAfter.Invalid == nil {
		t.Fatalf("invalid-setting State() after copy mutation = %+v, want non-nil Invalid", invalidAfter)
	}
	if diff := gocmp.Diff(wantInvalid, invalidAfter.Invalid); diff != "" {
		t.Errorf("invalid-setting State() mutated through State copy (-want +got):\n%s", diff)
	}
}

func TestRefreshStudioUnlistedNoContact(t *testing.T) {
	models := []map[string]any{studioCatalogModel("qwen3-coder-30b", []studioInstanceSpec{{"qwen3-coder-30b", 65536}}, "llm")}
	fake := startFakeStudio(t, func() []map[string]any { return models }, http.StatusOK, "", "")
	env := mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": fake.baseURL})
	var tracker localprov.StudioTracker

	tests := map[string]struct {
		routes []proxymodels.Route
	}{
		"success: empty listing":  {routes: []proxymodels.Route{}},
		"success: other backends": {routes: []proxymodels.Route{{ID: "kimi-k3", Provider: "kimi"}, {ID: "local/gguf", Provider: "local"}}},
		"success: nil listing":    {routes: nil},
	}
	for name, tt := range tests {
		if got := tracker.Refresh(t.Context(), tt.routes, env, t.TempDir(), localprov.ProbeOptions{}); got != nil {
			t.Errorf("%s: Refresh = %+v, want nil", name, got)
		}
	}
	if got := len(fake.requests()); got != 0 {
		t.Errorf("unlisted routes sent %d requests to LM Studio, want 0", got)
	}

	// Confirm the same fixture reaches the server when an LM Studio route is listed so stubs cannot pass vacuously.
	listed := tracker.Refresh(t.Context(), []proxymodels.Route{{ID: "lmstudio/qwen3-coder-30b", Provider: "lmstudio"}}, env, t.TempDir(), localprov.ProbeOptions{})
	if listed == nil || !listed.Reachable {
		t.Fatalf("listed LM Studio refresh = %+v, want reachable state", listed)
	}
	if got := len(fake.requests()); got != 1 {
		t.Errorf("listed route sent %d requests to LM Studio, want 1", got)
	}
}

func TestStudioReadiness(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "my gateway", "bin", "model-gateway")
	restartSuffix := "restart the gateway: `\"" + commandPath + "\" stop`, then `\"" + commandPath + "\" start`."
	proxyBin := filepath.Join(dir, "claude-code-proxy")
	healthy := readiness.SharedChecks{ProxyBinary: true, ProxyModels: true, ShimRunning: true, ServingVersionMatches: true}
	const base = "http://127.0.0.1:1234"

	probed := func(models []localprov.Model, mutate func(*localprov.StudioState)) *localprov.StudioState {
		s := &localprov.StudioState{Enabled: true, BaseURL: base, Reachable: true, Models: models}
		if mutate != nil {
			mutate(s)
		}
		return s
	}

	type tc struct {
		env       map[string]string
		state     *localprov.StudioState
		wantState string
		described *regexp.Regexp
	}

	readyCases := map[string]tc{
		"success: one model loaded with a guarded window, one not loaded": {
			state: probed([]localprov.Model{
				{Key: "qwen3-coder-30b", Loaded: true, ContextWindow: 65536},
				{Key: "ibm/granite-4-micro", Loaded: false, ContextWindow: 0},
			}, nil),
			wantState: "ready",
			described: regexp.MustCompile(`^LM Studio answering at http://127\.0\.0\.1:1234, 2 models, 1 loaded \(qwen3-coder-30b context 65536 tokens\); a model that is not loaded is an unmeasured 200k row with no compaction sentry until LM Studio loads it, which it does on the first request with that model's default context: load it first with ` + "`lms load <model key> --context-length <tokens>`$"),
		},
		"success: every model loaded, nothing to add": {
			state:     probed([]localprov.Model{{Key: "qwen3-coder-30b", Loaded: true, ContextWindow: 65536}}, nil),
			wantState: "ready",
			described: regexp.MustCompile(`^LM Studio answering at http://127\.0\.0\.1:1234, 1 model, 1 loaded \(qwen3-coder-30b context 65536 tokens\)$`),
		},
		"success: a model loaded below the sentry floor, with a warning that names it": {
			state: probed([]localprov.Model{
				{Key: "qwen3-coder-30b", Loaded: true, ContextWindow: 65536},
				{Key: "ibm/granite-4-micro", Loaded: true, ContextWindow: 4096},
			}, nil),
			wantState: "ready",
			described: regexp.MustCompile(`^LM Studio answering at http://127\.0\.0\.1:1234, 2 models, 2 loaded \(qwen3-coder-30b context 65536 tokens, ibm/granite-4-micro context 4096 tokens\); WARNING: ibm/granite-4-micro loaded below 32768, so there is no compaction sentry.*` + "`lms unload <model key>`" + `, then ` + "`lms load <model key> --context-length 32768`" + ` or more$`),
		},
		"success: nothing loaded": {
			state:     probed([]localprov.Model{{Key: "qwen3-coder-30b", Loaded: false, ContextWindow: 0}}, nil),
			wantState: "ready",
			described: regexp.MustCompile(`^LM Studio answering at http://127\.0\.0\.1:1234, 1 model, 0 loaded; a model that is not loaded is an unmeasured 200k row`),
		},
	}

	nonReadyCases := map[string]tc{
		"error: turned off where the check runs": {
			env:       map[string]string{"CCP_LMSTUDIO_ENABLED": "off"},
			wantState: "config-invalid",
			described: regexp.MustCompile(`^NOT CONFIGURED \(claude-code-proxy lists LM Studio models, but the provider is turned off where this check runs.*Use the same CCP_LMSTUDIO_ENABLED where the gateway starts.*, then ` + regexp.QuoteMeta(restartSuffix) + `\)$`),
		},
		"error: an invalid server setting, and the message names it and the reason": {
			env:       map[string]string{"CCP_LMSTUDIO_BASE_URL": "http://lmstudio.example:1234"},
			wantState: "config-invalid",
			described: regexp.MustCompile(`^NOT CONFIGURED \(claude-code-proxy lists LM Studio models, but the LM Studio setting CCP_LMSTUDIO_BASE_URL is invalid where this check runs \(must use https \(plain http only for a loopback host\)\).*, then ` + regexp.QuoteMeta(restartSuffix) + `\)$`),
		},
		"error: not probed in this process": {
			wantState: "auth-unknown",
			described: regexp.MustCompile(`^status unavailable$`),
		},
		"error: probed a different server": {
			env:       map[string]string{"CCP_LMSTUDIO_BASE_URL": "http://127.0.0.1:4321"},
			state:     probed(nil, nil),
			wantState: "auth-unknown",
			described: regexp.MustCompile(`^status unavailable$`),
		},
		"error: server not answering": {
			state:     probed(nil, func(s *localprov.StudioState) { s.Reachable = false; s.Error = "ECONNREFUSED" }),
			wantState: "backend-down",
			described: regexp.MustCompile(`^NOT RUNNING \(LM Studio's server is not answering at http://127\.0\.0\.1:1234 \(ECONNREFUSED\)\. Start it with ` + "`lms server start`" + ` or from the LM Studio app's Developer page; LM Studio 0\.4\.1 or later is required\.\)$`),
		},
		"error: server requires a token and none is set": {
			state:     probed(nil, func(s *localprov.StudioState) { s.AuthRequired = true }),
			wantState: "auth-missing",
			described: regexp.MustCompile(`^MISSING \(LM Studio at http://127\.0\.0\.1:1234 requires an API token: set CCP_LMSTUDIO_API_KEY \(or LM_API_TOKEN\) where the gateway starts .*, or lmstudio\.apiKey in .*config\.json; the token comes from LM Studio's server settings\. Then ` + regexp.QuoteMeta(restartSuffix) + `\)$`),
		},
		"error: server refuses the token that is set": {
			env:       map[string]string{"CCP_LMSTUDIO_API_KEY": studioRejectedSecret},
			state:     probed(nil, func(s *localprov.StudioState) { s.AuthRequired = true; s.HasAPIKey = true }),
			wantState: "auth-missing",
			described: regexp.MustCompile(`^MISSING \(LM Studio at http://127\.0\.0\.1:1234 refused the API token it was sent: check CCP_LMSTUDIO_API_KEY.*\. Then ` + regexp.QuoteMeta(restartSuffix) + `\)$`),
		},
	}

	readyMsgPattern := regexp.MustCompile(`^LM Studio readiness confirms local binary, /v1/models, LM Studio server reachability, shim`)
	forbiddenPattern := regexp.MustCompile(`auth login|sign-in status`)

	runCase := func(t *testing.T, name string, c tc, expectReady bool) {
		t.Helper()
		envMap := map[string]string{"CCP_CONFIG_DIR": dir}
		maps.Copy(envMap, c.env)
		lookup := mapLookup(envMap)
		var spawned atomic.Int32
		noSpawn := func(context.Context, string, []string) (int, error) {
			spawned.Add(1)
			return 0, errors.New("no status process may start for LM Studio")
		}
		authState := readiness.ProviderAuthState(t.Context(), "lmstudio", readiness.ProviderAuthOptions{
			Lookup:      lookup,
			ConfigDir:   dir,
			ProxyBinary: proxyBin,
			RunStatus:   noSpawn,
			StudioState: c.state,
		})
		if spawned.Load() != 0 {
			t.Errorf("%s: LM Studio auth check spawned %d status processes, want 0", name, spawned.Load())
		}
		if authState != c.wantState {
			t.Errorf("%s: ProviderAuthState = %q, want %q", name, authState, c.wantState)
		}
		msgOpts := readiness.ProviderMessageOptions{
			Lookup:      lookup,
			ConfigDir:   dir,
			ProxyBinary: proxyBin,
			CommandPath: commandPath,
			StudioState: c.state,
		}
		line := readiness.DescribeProviderAuth("lmstudio", authState, msgOpts)
		if strings.Contains(line, studioTestSecret) || strings.Contains(line, studioRejectedSecret) {
			t.Errorf("%s: DescribeProviderAuth leaked studio token marker", name)
		}
		if !c.described.MatchString(line) {
			t.Errorf("%s: DescribeProviderAuth = %q, does not match %s", name, redactStudioMarkers(line), c.described)
		}
		got := readiness.ProxyProviderReadiness("lmstudio", healthy, func(string) string { return authState }, msgOpts)
		if strings.Contains(got.Message, studioTestSecret) || strings.Contains(got.Message, studioRejectedSecret) {
			t.Errorf("%s: ProxyProviderReadiness.Message leaked studio token marker", name)
		}
		if got.Ready != expectReady {
			t.Errorf("%s: Ready = %v, want %v", name, got.Ready, expectReady)
		}
		if expectReady {
			if !readyMsgPattern.MatchString(got.Message) {
				t.Errorf("%s: ready Message = %q, does not match %s", name, redactStudioMarkers(got.Message), readyMsgPattern)
			}
		} else {
			if got.Message == "" {
				t.Errorf("%s: non-ready Message is empty", name)
			}
			if forbiddenPattern.MatchString(got.Message) {
				t.Errorf("%s: non-ready Message mentions proxy login: %q", name, redactStudioMarkers(got.Message))
			}
		}
	}

	t.Run("ready", func(t *testing.T) {
		for name, c := range readyCases {
			runCase(t, name, c, true)
		}
	})

	t.Run("non_ready", func(t *testing.T) {
		for name, c := range nonReadyCases {
			runCase(t, name, c, false)
		}
	})
}

func TestProbeLlama(t *testing.T) {
	measured, _ := startFakeLlama(t, func() int64 { return 40960 }, http.StatusOK)
	router, _ := startFakeLlama(t, func() int64 { return 0 }, http.StatusOK)
	failing, _ := startFakeLlama(t, func() int64 { return 40960 }, http.StatusServiceUnavailable)
	refused := closedLoopbackPort(t)

	tests := map[string]struct {
		env  map[string]string
		want localprov.LlamaState
	}{
		"success: a running server": {
			env:  map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(measured))},
			want: localprov.LlamaState{Port: measured, Configured: true, Reachable: true, ContextWindow: 40960},
		},
		"success: router mode answers but reports no window": {
			env:  map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(router))},
			want: localprov.LlamaState{Port: router, Configured: true, Reachable: true, ContextWindow: 0},
		},
		"error: a server that answers /props with an error": {
			env:  map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(failing))},
			want: localprov.LlamaState{Port: failing, Configured: true, Reachable: false, Error: "GET /props answered HTTP 503"},
		},
		"error: nothing listening": {
			env:  map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(refused))},
			want: localprov.LlamaState{Port: refused, Configured: true, Reachable: false, Error: "ECONNREFUSED"},
		},
		"error: not configured": {
			env:  map[string]string{},
			want: localprov.LlamaState{Port: 0, Configured: false, Reachable: false},
		},
		"error: not a port": {
			env:  map[string]string{"CCP_LLAMA_PORT": "x"},
			want: localprov.LlamaState{Port: 0, Configured: true, Reachable: false, Error: "CCP_LLAMA_PORT=x is not a port"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := localprov.ProbeLlama(t.Context(), mapLookup(tt.env), localprov.ProbeOptions{})
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ProbeLlama mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefreshLlamaState(t *testing.T) {
	port, closeSrv := startFakeLlama(t, func() int64 { return 65536 }, http.StatusOK)
	// Reserve a second distinct port while llama-server is still bound so the OS cannot reassign its port.
	movedPort, releaseMovedPort := reserveLoopbackPort(t)
	routes := []proxymodels.Route{{ID: "local/gguf", Provider: "local"}}
	env := mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(port))})
	var tracker localprov.LlamaTracker

	first := tracker.Refresh(t.Context(), routes, env, localprov.ProbeOptions{})
	if first == nil {
		t.Fatal("initial Refresh returned nil")
	}
	if first.ContextWindow != 65536 {
		t.Fatalf("initial ContextWindow = %d, want 65536", first.ContextWindow)
	}

	closeSrv()
	down := tracker.Refresh(t.Context(), routes, env, localprov.ProbeOptions{})
	if down == nil {
		t.Fatal("stopped server Refresh returned nil")
	}
	if down.Reachable {
		t.Error("stopped server reported Reachable = true, want false")
	}
	if down.ContextWindow != 65536 {
		t.Errorf("stopped server ContextWindow = %d, want preserved 65536", down.ContextWindow)
	}

	releaseMovedPort()
	movedEnv := mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(movedPort))})
	moved := tracker.Refresh(t.Context(), routes, movedEnv, localprov.ProbeOptions{})
	if moved == nil {
		t.Fatal("moved port Refresh returned nil")
	}
	if moved.ContextWindow != 0 {
		t.Errorf("moved port ContextWindow = %d, want 0", moved.ContextWindow)
	}

	cleared := tracker.Refresh(t.Context(), []proxymodels.Route{{ID: "kimi-k3", Provider: "kimi"}}, env, localprov.ProbeOptions{})
	if cleared != nil {
		t.Errorf("unlisted local row Refresh = %+v, want nil", cleared)
	}
	if got := tracker.State(); got != nil {
		t.Errorf("tracker.State() after unlisted local row = %+v, want nil", got)
	}
}

func TestLlamaReadiness(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "my gateway", "bin", "model-gateway")
	restartSuffix := "restart the gateway: `\"" + commandPath + "\" stop`, then `\"" + commandPath + "\" start`."
	proxyBin := filepath.Join(dir, "claude-code-proxy")
	healthy := readiness.SharedChecks{ProxyBinary: true, ProxyModels: true, ShimRunning: true, ServingVersionMatches: true}

	type tc struct {
		env       map[string]string
		state     *localprov.LlamaState
		wantState string
		described *regexp.Regexp
	}

	readyCases := map[string]tc{
		"success: answering with a guarded window": {
			env:       map[string]string{"CCP_LLAMA_PORT": "8080"},
			state:     &localprov.LlamaState{Port: 8080, Configured: true, Reachable: true, ContextWindow: 65536},
			wantState: "ready",
			described: regexp.MustCompile(`^llama-server answering on 127\.0\.0\.1:8080, context 65536 tokens$`),
		},
		"success: answering below the sentry floor, with a warning": {
			env:       map[string]string{"CCP_LLAMA_PORT": "8080"},
			state:     &localprov.LlamaState{Port: 8080, Configured: true, Reachable: true, ContextWindow: 8192},
			wantState: "ready",
			described: regexp.MustCompile(`^llama-server answering on 127\.0\.0\.1:8080, context 8192 tokens; WARNING: below 32768, so there is no compaction sentry.*-c 32768 or more$`),
		},
		"success: answering in router mode": {
			env:       map[string]string{"CCP_LLAMA_PORT": "8080"},
			state:     &localprov.LlamaState{Port: 8080, Configured: true, Reachable: true, ContextWindow: 0},
			wantState: "ready",
			described: regexp.MustCompile(`context unknown .*unmeasured 200k row$`),
		},
	}

	nonReadyCases := map[string]tc{
		"error: CCP_LLAMA_PORT unset where the gateway runs": {
			env:       map[string]string{},
			wantState: "config-invalid",
			described: regexp.MustCompile(`^NOT CONFIGURED \(claude-code-proxy lists local/gguf, but CCP_LLAMA_PORT is not set where the gateway runs.*, then ` + regexp.QuoteMeta(restartSuffix) + `\)$`),
		},
		"error: CCP_LLAMA_PORT not a port, and the message names the value": {
			env:       map[string]string{"CCP_LLAMA_PORT": "abc"},
			wantState: "config-invalid",
			described: regexp.MustCompile(`^NOT CONFIGURED \(claude-code-proxy lists local/gguf, but CCP_LLAMA_PORT="abc" is not a port where the gateway runs.*, then ` + regexp.QuoteMeta(restartSuffix) + `\)$`),
		},
		"error: not probed in this process": {
			env:       map[string]string{"CCP_LLAMA_PORT": "8080"},
			wantState: "auth-unknown",
			described: regexp.MustCompile(`^status unavailable$`),
		},
		"error: probed a different port": {
			env:       map[string]string{"CCP_LLAMA_PORT": "8080"},
			state:     &localprov.LlamaState{Port: 9090, Configured: true, Reachable: true, ContextWindow: 65536},
			wantState: "auth-unknown",
			described: regexp.MustCompile(`^status unavailable$`),
		},
		"error: server not answering": {
			env:       map[string]string{"CCP_LLAMA_PORT": "8080"},
			state:     &localprov.LlamaState{Port: 8080, Configured: true, Reachable: false, Error: "ECONNREFUSED"},
			wantState: "backend-down",
			described: regexp.MustCompile(`^NOT RUNNING \(llama-server is not answering on 127\.0\.0\.1:8080 \(ECONNREFUSED\)\. Start it with ` + "`llama-server -m <model>\\.gguf --jinja --host 127\\.0\\.0\\.1 --port 8080`" + `\.\)$`),
		},
	}

	readyMsgPattern := regexp.MustCompile(`^Local GGUF readiness confirms local binary, /v1/models, llama-server reachability, shim`)
	forbiddenPattern := regexp.MustCompile(`auth login|sign-in status`)

	runCase := func(t *testing.T, name string, c tc, expectReady bool) {
		t.Helper()
		lookup := mapLookup(c.env)
		var spawned atomic.Int32
		noSpawn := func(context.Context, string, []string) (int, error) {
			spawned.Add(1)
			return 0, errors.New("no status process may start for local")
		}
		authState := readiness.ProviderAuthState(t.Context(), "local", readiness.ProviderAuthOptions{
			Lookup:      lookup,
			ConfigDir:   dir,
			ProxyBinary: proxyBin,
			RunStatus:   noSpawn,
			LlamaState:  c.state,
		})
		if spawned.Load() != 0 {
			t.Errorf("%s: local auth check spawned %d status processes, want 0", name, spawned.Load())
		}
		if authState != c.wantState {
			t.Errorf("%s: ProviderAuthState = %q, want %q", name, authState, c.wantState)
		}
		msgOpts := readiness.ProviderMessageOptions{
			Lookup:      lookup,
			ConfigDir:   dir,
			ProxyBinary: proxyBin,
			CommandPath: commandPath,
			LlamaState:  c.state,
		}
		line := readiness.DescribeProviderAuth("local", authState, msgOpts)
		if !c.described.MatchString(line) {
			t.Errorf("%s: DescribeProviderAuth = %q, does not match %s", name, line, c.described)
		}
		got := readiness.ProxyProviderReadiness("local", healthy, func(string) string { return authState }, msgOpts)
		if got.Ready != expectReady {
			t.Errorf("%s: Ready = %v, want %v", name, got.Ready, expectReady)
		}
		if expectReady {
			if !readyMsgPattern.MatchString(got.Message) {
				t.Errorf("%s: ready Message = %q, does not match %s", name, got.Message, readyMsgPattern)
			}
		} else {
			if got.Message == "" {
				t.Errorf("%s: non-ready Message is empty", name)
			}
			if forbiddenPattern.MatchString(got.Message) {
				t.Errorf("%s: non-ready Message mentions proxy login: %q", name, got.Message)
			}
		}
	}

	t.Run("ready", func(t *testing.T) {
		for name, c := range readyCases {
			runCase(t, name, c, true)
		}
	})

	t.Run("non_ready", func(t *testing.T) {
		for name, c := range nonReadyCases {
			runCase(t, name, c, false)
		}
	})
}

func TestProbeRedirects(t *testing.T) {
	t.Run("success: LM Studio follows same-origin redirect and retains Authorization", func(t *testing.T) {
		var mu sync.Mutex
		var seen []seenRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authValues, hasAuth := r.Header["Authorization"]
			mu.Lock()
			seen = append(seen, seenRequest{
				URL:           r.URL.RequestURI(),
				HasAuthHeader: hasAuth,
				AuthValues:    slices.Clone(authValues),
			})
			mu.Unlock()
			if r.URL.Path == "/api/v1/models" {
				w.Header().Set("Location", "/redirected/models")
				w.WriteHeader(http.StatusFound)
				return
			}
			if r.URL.Path != "/redirected/models" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+studioRedirectSecret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			data, err := json.Marshal(map[string]any{
				"models": []map[string]any{studioCatalogModel("qwen3-coder-30b", []studioInstanceSpec{{"qwen3-coder-30b", 65536}}, "llm")},
			})
			if err != nil {
				t.Errorf("marshal redirected models: %v", err)
				return
			}
			_, _ = w.Write(data)
		}))
		t.Cleanup(srv.Close)

		env := mapLookup(map[string]string{
			"CCP_LMSTUDIO_BASE_URL": srv.URL,
			"CCP_LMSTUDIO_API_KEY":  studioRedirectSecret,
		})
		got := localprov.ProbeStudio(t.Context(), env, t.TempDir(), localprov.ProbeOptions{})
		want := localprov.StudioState{
			Enabled:   true,
			BaseURL:   srv.URL,
			HasAPIKey: true,
			Reachable: true,
			Models:    []localprov.Model{{Key: "qwen3-coder-30b", Loaded: true, ContextWindow: 65536}},
		}
		if diff := gocmp.Diff(want, got); diff != "" {
			t.Errorf("same-origin redirect StudioState mismatch (-want +got):\n%s", redactStudioMarkers(diff))
		}
		mu.Lock()
		gotSeen := slices.Clone(seen)
		mu.Unlock()
		wantSeen := []seenRequest{
			{URL: "/api/v1/models", HasAuthHeader: true, AuthValues: []string{"Bearer " + studioRedirectSecret}},
			{URL: "/redirected/models", HasAuthHeader: true, AuthValues: []string{"Bearer " + studioRedirectSecret}},
		}
		if diff := gocmp.Diff(wantSeen, gotSeen); diff != "" {
			t.Errorf("same-origin redirect requests mismatch (-want +got):\n%s", redactStudioMarkers(diff))
		}
	})

	t.Run("success: LM Studio follows cross-origin redirect and strips Authorization", func(t *testing.T) {
		var targetMu sync.Mutex
		var targetSeen []seenRequest
		var targetSawSecret atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authValues, hasAuth := r.Header["Authorization"]
			for _, vals := range r.Header {
				for _, v := range vals {
					if strings.Contains(v, studioRedirectSecret) {
						targetSawSecret.Store(true)
					}
				}
			}
			targetMu.Lock()
			targetSeen = append(targetSeen, seenRequest{
				URL:           r.URL.RequestURI(),
				HasAuthHeader: hasAuth,
				AuthValues:    slices.Clone(authValues),
			})
			targetMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			data, err := json.Marshal(map[string]any{
				"models": []map[string]any{studioCatalogModel("qwen3-coder-30b", []studioInstanceSpec{{"qwen3-coder-30b", 40960}}, "llm")},
			})
			if err != nil {
				t.Errorf("marshal cross-origin models: %v", err)
				return
			}
			_, _ = w.Write(data)
		}))
		t.Cleanup(target.Close)

		var originMu sync.Mutex
		var originSeen []seenRequest
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authValues, hasAuth := r.Header["Authorization"]
			originMu.Lock()
			originSeen = append(originSeen, seenRequest{
				URL:           r.URL.RequestURI(),
				HasAuthHeader: hasAuth,
				AuthValues:    slices.Clone(authValues),
			})
			originMu.Unlock()
			w.Header().Set("Location", target.URL+"/cross/models")
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(origin.Close)

		env := mapLookup(map[string]string{
			"CCP_LMSTUDIO_BASE_URL": origin.URL,
			"CCP_LMSTUDIO_API_KEY":  studioRedirectSecret,
		})
		got := localprov.ProbeStudio(t.Context(), env, t.TempDir(), localprov.ProbeOptions{})
		want := localprov.StudioState{
			Enabled:   true,
			BaseURL:   origin.URL,
			HasAPIKey: true,
			Reachable: true,
			Models:    []localprov.Model{{Key: "qwen3-coder-30b", Loaded: true, ContextWindow: 40960}},
		}
		if diff := gocmp.Diff(want, got); diff != "" {
			t.Errorf("cross-origin redirect StudioState mismatch (-want +got):\n%s", redactStudioMarkers(diff))
		}
		if targetSawSecret.Load() {
			t.Error("cross-origin redirect leaked studio token marker to different-origin target")
		}
		originMu.Lock()
		gotOrigin := slices.Clone(originSeen)
		originMu.Unlock()
		if len(gotOrigin) != 1 || !gotOrigin[0].HasAuthHeader {
			t.Errorf("origin server expected 1 authenticated request, got %d (hasAuth=%v)", len(gotOrigin), len(gotOrigin) > 0 && gotOrigin[0].HasAuthHeader)
		}
		targetMu.Lock()
		gotTarget := slices.Clone(targetSeen)
		targetMu.Unlock()
		wantTarget := []seenRequest{{URL: "/cross/models", HasAuthHeader: false, AuthValues: nil}}
		if diff := gocmp.Diff(wantTarget, gotTarget); diff != "" {
			t.Errorf("cross-origin target requests mismatch (-want +got):\n%s", redactStudioMarkers(diff))
		}
	})

	t.Run("success: llama-server probe follows redirect on /props", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/props" {
				w.Header().Set("Location", "/props-v2")
				w.WriteHeader(http.StatusFound)
				return
			}
			if r.URL.Path == "/props-v2" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":65536}}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		port := uint16(srv.Listener.Addr().(*net.TCPAddr).Port)

		got := localprov.ProbeLlama(t.Context(), mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(port))}), localprov.ProbeOptions{})
		want := localprov.LlamaState{Port: port, Configured: true, Reachable: true, ContextWindow: 65536}
		if diff := gocmp.Diff(want, got); diff != "" {
			t.Errorf("redirected ProbeLlama mismatch (-want +got):\n%s", diff)
		}
	})
}
