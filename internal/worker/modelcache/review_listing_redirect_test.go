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
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// TestListingRedirectHopLimitAndUnfollowedStatus refreshes from a proxy whose listing is reached
// through a chain of redirects. The listing request follows 301, 302, 303, 307 and 308 for at most
// ten requests including the first: nine redirects still read the listing, while a tenth redirect
// leaves the listing unread without an eleventh request. A 3xx status outside those five is the
// answer even when it names a Location, which is never requested; an unread listing keeps the
// built-in catalog.
func TestListingRedirectHopLimitAndUnfollowedStatus(t *testing.T) {
	followed := []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}
	tests := map[string]struct {
		// redirects is how many followed redirects precede the answer.
		redirects int
		// finalStatus is the status of the answer; a 3xx answer names a Location.
		finalStatus  int
		wantRequests int
		wantSource   string
	}{
		"success: ten requests including the first read the listing": {
			redirects:    9,
			finalStatus:  http.StatusOK,
			wantRequests: 10,
			wantSource:   "proxy",
		},
		"error: an eleventh request is never sent": {
			redirects:    10,
			finalStatus:  http.StatusOK,
			wantRequests: 10,
			wantSource:   "fallback",
		},
		"error: 300 with a Location is the answer": {
			finalStatus:  http.StatusMultipleChoices,
			wantRequests: 1,
			wantSource:   "fallback",
		},
		"error: 304 with a Location is the answer": {
			finalStatus:  http.StatusNotModified,
			wantRequests: 1,
			wantSource:   "fallback",
		},
		"error: 305 with a Location is the answer": {
			finalStatus:  http.StatusUseProxy,
			wantRequests: 1,
			wantSource:   "fallback",
		},
		"error: 309 with a Location is the answer after a followed redirect": {
			redirects:    1,
			finalStatus:  309,
			wantRequests: 2,
			wantSource:   "fallback",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.URL.Path)
				mu.Unlock()
				hop := 0
				if rest, ok := strings.CutPrefix(r.URL.Path, "/hop/"); ok {
					n, err := strconv.Atoi(rest)
					if err != nil {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					hop = n
				} else if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				switch {
				case hop < tt.redirects:
					w.Header().Set("Location", "/hop/"+strconv.Itoa(hop+1))
					w.WriteHeader(followed[hop%len(followed)])
				case tt.finalStatus != http.StatusOK:
					w.Header().Set("Location", "/unfollowed")
					w.WriteHeader(tt.finalStatus)
				default:
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
				}
			})
			_, lookup, resolver := newTestPaths(t, nil)
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   "http://127.0.0.1:19991",
				DefaultBaseURL: discoveryGatewayURL,
				Client:         &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:          time.Now,
				Routes:         &route.Store{},
				Sentry:         &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := cache.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if source, _ := cache.HealthCatalog(); source != tt.wantSource {
				t.Errorf("catalog source after the refresh = %q, want %q", source, tt.wantSource)
			}
			mu.Lock()
			paths := slices.Clone(seen)
			mu.Unlock()
			if len(paths) != tt.wantRequests {
				t.Errorf("listing requests received = %d (%q), want %d", len(paths), paths, tt.wantRequests)
			}
			if slices.Contains(paths, "/unfollowed") {
				t.Errorf("the listing requested the Location of an unfollowed status: %q", paths)
			}
		})
	}
}
