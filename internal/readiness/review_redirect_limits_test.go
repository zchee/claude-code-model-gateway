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

package readiness_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

// redirectChainTransport answers each request in process with handler, so redirects reach the
// client as ordinary responses without any listener.
type redirectChainTransport struct {
	handler http.Handler
}

func (rt redirectChainTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.handler.ServeHTTP(rec, req)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// TestProxyCheckRedirectHopLimitAndUnfollowedStatus runs the proxy readiness check against a
// /v1/models answer reached through a chain of redirects. The check follows 301, 302, 303, 307 and
// 308 for at most ten requests including the first: nine redirects still reach the 200 answer, while
// a tenth redirect fails the check without an eleventh request. A 3xx status outside those five is
// the answer even when it names a Location, which is never requested, and is not a ready proxy.
func TestProxyCheckRedirectHopLimitAndUnfollowedStatus(t *testing.T) {
	followed := []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}
	tests := map[string]struct {
		// redirects is how many followed redirects precede the answer.
		redirects int
		// finalStatus is the status of the answer; a 3xx answer names a Location.
		finalStatus  int
		wantRequests int
		wantModels   bool
	}{
		"success: ten requests including the first reach the answer": {
			redirects:    9,
			finalStatus:  http.StatusOK,
			wantRequests: 10,
			wantModels:   true,
		},
		"error: an eleventh request is never sent": {
			redirects:    10,
			finalStatus:  http.StatusOK,
			wantRequests: 10,
		},
		"error: 300 with a Location is the answer": {
			finalStatus:  http.StatusMultipleChoices,
			wantRequests: 1,
		},
		"error: 304 with a Location is the answer": {
			finalStatus:  http.StatusNotModified,
			wantRequests: 1,
		},
		"error: 305 with a Location is the answer": {
			finalStatus:  http.StatusUseProxy,
			wantRequests: 1,
		},
		"error: 309 with a Location is the answer after a followed redirect": {
			redirects:    1,
			finalStatus:  309,
			wantRequests: 2,
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
					_, _ = io.WriteString(w, `{"data":[]}`)
				}
			})
			got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
				ProxyBinary:    fakeProxyBinary(t),
				ProxyModelsURL: "http://127.0.0.1:19993/v1/models",
				Client:         &http.Client{Transport: redirectChainTransport{handler: handler}},
			})
			want := readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: tt.wantModels}
			if got != want {
				t.Errorf("ProbeProxyAndAuth = %+v, want %+v", got, want)
			}
			mu.Lock()
			paths := slices.Clone(seen)
			mu.Unlock()
			if len(paths) != tt.wantRequests {
				t.Errorf("requests received = %d (%q), want %d", len(paths), paths, tt.wantRequests)
			}
			if slices.Contains(paths, "/unfollowed") {
				t.Errorf("the check requested the Location of an unfollowed status: %q", paths)
			}
		})
	}
}
