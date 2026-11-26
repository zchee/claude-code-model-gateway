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

//go:build process

package modelcache_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	cacheEnvProxyHelper = "modelcache-env-proxy"
	// cacheEnvProxyDone is the child's last output line; without it the child did not run the cache.
	cacheEnvProxyDone = "modelcache env-proxy child completed"
)

// envProxyVariables names the proxy settings net/http reads from the process environment.
var envProxyVariables = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"}

// TestGatewayFixtureHelper dispatches the child processes this package launches through gatewaytest.
func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{
		cacheEnvProxyHelper: func(_ testing.TB, args []string) { runCacheEnvProxyChild(t, args) },
	})
}

// TestCacheIgnoresEnvironmentProxy runs the cache in a child test process whose environment names a
// decoy proxy for every scheme, and hands it a client with no transport. Neither the LM Studio probe
// nor the Anthropic catalog page, both addressed to non-loopback hosts, may reach the decoy. The
// child process is needed because net/http reads the proxy environment once per process.
func TestCacheIgnoresEnvironmentProxy(t *testing.T) {
	tests := map[string]struct {
		lookupProxy bool
	}{
		"success: proxy named in the process environment": {},
		"success: proxy named in the process environment and in the injected lookup": {
			lookupProxy: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var decoyHits []string
			decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				decoyHits = append(decoyHits, r.Method+" "+r.RequestURI)
				mu.Unlock()
				w.Header().Set("Connection", "close")
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(decoy.Close)

			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			env["NO_PROXY"], env["no_proxy"] = "", ""
			for _, key := range envProxyVariables {
				env[key] = decoy.URL
			}
			child, err := env.HelperCommand(t, cacheEnvProxyHelper, decoy.URL, strconv.FormatBool(tt.lookupProxy))
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			child.Stdout, child.Stderr = &output, &output
			if err := env.RunFixtureWithin(child, 30*time.Second); err != nil {
				t.Fatalf("child test process: %v\n%s", err, redactTestMarkers(output.String()))
			}
			if !strings.Contains(output.String(), cacheEnvProxyDone) {
				t.Fatalf("child test process did not run the cache:\n%s", redactTestMarkers(output.String()))
			}
			mu.Lock()
			hits := slices.Clone(decoyHits)
			mu.Unlock()
			if len(hits) != 0 {
				t.Errorf("the environment proxy received %d request(s), want none: %q", len(hits), hits)
			}
		})
	}
}

// runCacheEnvProxyChild is the child half of TestCacheIgnoresEnvironmentProxy. Its arguments are the
// decoy proxy URL and whether the injected lookup also names the decoy.
func runCacheEnvProxyChild(t *testing.T, args []string) {
	if len(args) != 2 {
		t.Fatalf("child arguments = %q, want the decoy URL and the lookup flag", args)
	}
	decoyURL := args[0]
	lookupProxy, err := strconv.ParseBool(args[1])
	if err != nil {
		t.Fatalf("child lookup flag: %v", err)
	}
	for _, target := range []string{"https://lmstudio.invalid:1234/api/v1/models", "http://anthropic-catalog.invalid/v1/models"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if u, err := http.ProxyFromEnvironment(req); err != nil || u == nil || u.String() != decoyURL {
			t.Fatalf("test precondition: ProxyFromEnvironment(%s) = (%v, %v), want the decoy %s", target, u, err, decoyURL)
		}
	}

	listing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"lmstudio/qwen3-coder-30b","display_name":"lmstudio/qwen3-coder-30b (lmstudio)"}]}`))
	}))
	t.Cleanup(listing.Close)
	t.Cleanup(http.DefaultTransport.(*http.Transport).CloseIdleConnections)

	extra := map[string]string{"CCP_LMSTUDIO_BASE_URL": "https://lmstudio.invalid:1234"}
	if lookupProxy {
		for _, key := range envProxyVariables {
			extra[key] = decoyURL
		}
	}
	_, lookup, resolver := newTestPaths(t, extra)
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            resolver,
		ProxyBaseURL:     listing.URL,
		AnthropicBaseURL: "http://anthropic-catalog.invalid",
		DefaultBaseURL:   "http://127.0.0.1:19999",
		Client:           &http.Client{},
		Clock:            time.Now,
		Routes:           &route.Store{},
		Sentry:           &sentry.State{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if state := cache.StudioState(); state == nil || state.Invalid != nil || state.BaseURL != "https://lmstudio.invalid:1234" {
		t.Fatalf("test precondition: LM Studio was not probed: %+v", state)
	}
	if _, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}}); err != nil {
		t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
	}
	cache.Wait()
	fmt.Println(cacheEnvProxyDone)
}
