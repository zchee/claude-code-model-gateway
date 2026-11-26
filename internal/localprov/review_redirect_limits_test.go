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
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
)

// handlerTransport answers each request in process with handler, so redirects reach the client as
// ordinary responses without any listener.
type handlerTransport struct {
	handler http.Handler
}

func (h handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

// TestProbeRedirectHopLimitAndUnfollowedStatus probes a llama-server whose /props answer is a chain
// of redirects. The probe follows 301, 302, 303, 307 and 308 for at most ten requests including the
// first: nine redirects still reach the answer, while a tenth redirect fails the probe without an
// eleventh request. A 3xx status outside those five is the answer even when it names a Location,
// which is never requested. Each case runs in a synctest bubble with the chain served from memory: the
// probe deadline runs on the bubble clock, and the answer arrives with no bubble time passed, so no
// deadline decides an outcome.
func TestProbeRedirectHopLimitAndUnfollowedStatus(t *testing.T) {
	followed := []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}
	tests := map[string]struct {
		// redirects is how many followed redirects precede the answer.
		redirects int
		// finalStatus is the status of the answer; a 3xx answer names a Location.
		finalStatus  int
		wantRequests int
		wantWindow   int64
		wantError    string
	}{
		"success: ten requests including the first reach the answer": {
			redirects:    9,
			finalStatus:  http.StatusOK,
			wantRequests: 10,
			wantWindow:   40960,
		},
		"error: an eleventh request is never sent": {
			redirects:    10,
			finalStatus:  http.StatusOK,
			wantRequests: 10,
			wantError:    "request failed",
		},
		"error: 300 with a Location is the answer": {
			finalStatus:  http.StatusMultipleChoices,
			wantRequests: 1,
			wantError:    "GET /props answered HTTP 300",
		},
		"error: 304 with a Location is the answer": {
			finalStatus:  http.StatusNotModified,
			wantRequests: 1,
			wantError:    "GET /props answered HTTP 304",
		},
		"error: 305 with a Location is the answer": {
			finalStatus:  http.StatusUseProxy,
			wantRequests: 1,
			wantError:    "GET /props answered HTTP 305",
		},
		"error: 309 with a Location is the answer after a followed redirect": {
			redirects:    1,
			finalStatus:  309,
			wantRequests: 2,
			wantError:    "GET /props answered HTTP 309",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
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
					} else if r.URL.Path != "/props" {
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
						data, err := json.Marshal(map[string]any{"default_generation_settings": map[string]any{"n_ctx": 40960}})
						if err != nil {
							t.Errorf("marshal llama props: %v", err)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(data)
					}
				})
				// The transport answers in memory, so the port is never dialled or bound.
				const port uint16 = 9
				start := time.Now()
				got := localprov.ProbeLlama(t.Context(), mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(port))}), localprov.ProbeOptions{Client: &http.Client{Transport: handlerTransport{handler: handler}}})
				if elapsed := time.Since(start); elapsed != 0 {
					t.Errorf("test precondition: the probe took %v of bubble time, want none", elapsed)
				}
				want := localprov.LlamaState{Port: port, Configured: true, Reachable: tt.wantError == "", ContextWindow: tt.wantWindow, Error: tt.wantError}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("ProbeLlama mismatch (-want +got):\n%s", diff)
				}
				mu.Lock()
				paths := slices.Clone(seen)
				mu.Unlock()
				if len(paths) != tt.wantRequests {
					t.Errorf("requests received = %d (%q), want %d", len(paths), paths, tt.wantRequests)
				}
				if slices.Contains(paths, "/unfollowed") {
					t.Errorf("the probe requested the Location of an unfollowed status: %q", paths)
				}
			})
		})
	}
}
