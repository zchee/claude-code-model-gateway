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
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const jarCookieMarker = "jar-cookie-synth-marker-6b0e"

// ownershipRequest is what a fake server saw of one request.
type ownershipRequest struct {
	Server    string
	Path      string
	HasCookie bool
	BasicAuth bool
}

// ownershipLog records the requests of every fake server of one test.
type ownershipLog struct {
	mu   sync.Mutex
	seen []ownershipRequest
}

func (l *ownershipLog) record(server string, r *http.Request) {
	_, _, basic := r.BasicAuth()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, ownershipRequest{Server: server, Path: r.URL.Path, HasCookie: len(r.Header.Values("Cookie")) > 0, BasicAuth: basic})
}

func (l *ownershipLog) requests() []ownershipRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seen)
}

// jarFor returns a cookie jar holding a session cookie for every URL in targets.
func jarFor(t *testing.T, targets ...string) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	for _, target := range targets {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatalf("parse %q: %v", target, err)
		}
		jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: jarCookieMarker, Path: "/"}})
	}
	return jar
}

// TestCacheRequestsNeverUseInjectedCookieJar hands the cache a client with a cookie jar holding a
// cookie for every fake server and checks that neither the proxy listing, the llama-server probe nor
// the Anthropic catalog page carries a Cookie header, whether or not the client names a transport.
func TestCacheRequestsNeverUseInjectedCookieJar(t *testing.T) {
	tests := map[string]struct {
		transport func(t *testing.T) http.RoundTripper
	}{
		"success: client without a transport": {
			transport: func(t *testing.T) http.RoundTripper {
				t.Cleanup(http.DefaultTransport.(*http.Transport).CloseIdleConnections)
				return nil
			},
		},
		"success: client with its own transport": {
			transport: func(t *testing.T) http.RoundTripper {
				tr := &http.Transport{Proxy: nil}
				t.Cleanup(tr.CloseIdleConnections)
				return tr
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var log ownershipLog
			llama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record("llama-server", r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":65536}}`))
			}))
			t.Cleanup(llama.Close)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record("proxy", r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},{"id":"local/gguf","display_name":"local/gguf (local)"}]}`))
			}))
			t.Cleanup(proxy.Close)
			anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record("anthropic", r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}],"has_more":false}`))
			}))
			t.Cleanup(anthropic.Close)

			_, lookup, resolver := newTestPaths(t, map[string]string{
				"CCP_LLAMA_PORT": strconv.Itoa(llama.Listener.Addr().(*net.TCPAddr).Port),
			})
			client := &http.Client{Transport: tt.transport(t), Jar: jarFor(t, llama.URL, proxy.URL, anthropic.URL)}
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     proxy.URL,
				AnthropicBaseURL: anthropic.URL,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           client,
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
			if _, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}}); err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			cache.Wait()

			got := log.requests()
			servers := map[string]bool{}
			for _, r := range got {
				servers[r.Server] = true
				if r.HasCookie {
					t.Errorf("%s received a Cookie header on %s from the injected client's cookie jar", r.Server, r.Path)
				}
			}
			if diff := gocmp.Diff(map[string]bool{"anthropic": true, "llama-server": true, "proxy": true}, servers); diff != "" {
				t.Fatalf("test precondition: servers contacted mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCatalogBaseURLWithUserinfoIsRefused configures an Anthropic base URL that carries userinfo,
// with a stale saved catalog and request credentials that would otherwise start a refresh. The cache
// is constructed and logs the refusal once, in a fixed text that names neither the userinfo nor the
// address. It never sends a catalog request, so no Basic credential is synthesized from the URL; it
// keeps serving the saved rows and leaves the saved catalog as it was, with no failure recorded.
func TestCatalogBaseURLWithUserinfoIsRefused(t *testing.T) {
	// refusalLog restates the line the cache logs when it refuses such a base URL.
	const refusalLog = "model-gateway: the Anthropic catalog base URL carries credentials; the catalog is not refreshed\n"
	tests := map[string]struct {
		incoming http.Header
	}{
		"error: request credentials carry only an API key": {
			incoming: http.Header{"X-Api-Key": {apiKeyMarker}},
		},
		"error: request credentials carry a bearer token": {
			incoming: http.Header{"Authorization": {authBearerHeader}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var log ownershipLog
			anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record("anthropic", r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}],"has_more":false}`))
			}))
			t.Cleanup(anthropic.Close)
			base := strings.Replace(anthropic.URL, "http://", "http://catalog-user:catalog-pass@", 1)

			_, lookup, resolver := newTestPaths(t, nil)
			catalogPath := filepath.Join(stateDirOf(t, resolver), "anthropic-models.json")
			writeTestFile(t, catalogPath, []byte(seededCatalog))
			var logs logCapture
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: base,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           newTestClient(t),
				Clock:            func() time.Time { return time.UnixMilli(staleCatalogNow) },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				Logf:             logs.logf,
			})
			if err != nil {
				t.Fatalf("New with a base URL that carries userinfo: %s, want the cache constructed with refresh disabled", redactTestMarkers(err.Error()))
			}
			body, err := cache.ModelsResponse(t.Context(), tt.incoming)
			if err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			cache.TriggerAnthropicRefresh(tt.incoming)
			cache.Wait()
			if got := log.requests(); len(got) != 0 {
				t.Errorf("catalog server received %d request(s) through a base URL with userinfo, want none: %+v", len(got), got)
			}
			if ids := responseRowIDs(t, body); !slices.Contains(ids, "claude-cached-1") {
				t.Errorf("listing with refresh disabled = %q, want the saved row claude-cached-1", ids)
			}
			if saved, err := os.ReadFile(catalogPath); err != nil || string(saved) != seededCatalog {
				t.Errorf("saved catalog after the refused refresh = %q (err %v), want it unchanged %q", saved, err, seededCatalog)
			}
			logged := string(logs.bytes())
			if logged != refusalLog {
				t.Errorf("log = %q, want exactly the refusal %q", redactTestMarkers(logged), refusalLog)
			}
			host := strings.TrimPrefix(anthropic.URL, "http://")
			for _, leaked := range []string{"catalog-user", "catalog-pass", host} {
				if strings.Contains(logged, leaked) {
					t.Errorf("log names %q from the refused base URL, want it redacted", leaked)
				}
			}
		})
	}
}
