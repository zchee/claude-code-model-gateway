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
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

// TestSharedCheckLatestReturnsCopy changes every field of the result Latest returned. The next Latest
// still returns the completed check's result and its time, so a caller cannot alter what other
// callers of the shared check see.
func TestSharedCheckLatestReturnsCopy(t *testing.T) {
	checkedAt := time.UnixMilli(1_700_000_000_000)
	tests := map[string]struct {
		result readiness.ProxyAuthResult
		mutate func(result *readiness.ProxyAuthResult)
	}{
		"success: a ready result keeps its fields after a caller changes its copy": {
			result: readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: true},
			mutate: func(result *readiness.ProxyAuthResult) {
				result.ProxyBinary = false
				result.ProxyModels = false
				result.CodexAuth = false
				result.CheckedAt = time.Time{}
			},
		},
		"success: a failed result keeps its fields after a caller changes its copy": {
			result: readiness.ProxyAuthResult{ProxyBinary: true},
			mutate: func(result *readiness.ProxyAuthResult) {
				result.ProxyModels = true
				result.CodexAuth = true
				result.CheckedAt = checkedAt.Add(time.Hour)
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var runs atomic.Int32
			check := readiness.NewSharedCheck(t.Context(), func(context.Context) readiness.ProxyAuthResult {
				runs.Add(1)
				return tt.result
			}, func() time.Time { return checkedAt })
			defer check.Wait()
			if got := check.Latest(); got != nil {
				t.Fatalf("Latest before any check completed = %+v, want nil", *got)
			}
			check.Wait()

			want := tt.result
			want.CheckedAt = checkedAt
			first := check.Latest()
			if first == nil {
				t.Fatal("Latest after the check completed returned nil")
			}
			if diff := gocmp.Diff(want, *first); diff != "" {
				t.Fatalf("test precondition: Latest after the check completed mismatch (-want +got):\n%s", diff)
			}
			tt.mutate(first)
			second := check.Latest()
			if second == nil {
				t.Fatal("Latest after a caller changed its copy returned nil")
			}
			if diff := gocmp.Diff(want, *second); diff != "" {
				t.Errorf("Latest after a caller changed its copy mismatch (-want +got):\n%s", diff)
			}
			if first == second {
				t.Error("two Latest calls returned the same pointer, want a copy for each caller")
			}
			check.Wait()
			if n := runs.Load(); n != 1 {
				t.Errorf("checks run = %d, want 1: a fresh result starts no other check", n)
			}
		})
	}
}

// TestProxyCheckRefusesRedirectWithUserinfo sends the /v1/models check through redirects whose target
// carries userinfo, on the first redirect or on a later one, to the same host or another. The check
// never requests such a target, which would turn the userinfo into an Authorization header, and the
// proxy is not answering, although the target would answer 200. The same target without userinfo is
// followed and answers.
func TestProxyCheckRefusesRedirectWithUserinfo(t *testing.T) {
	tests := map[string]struct {
		// first is where /v1/models redirects; /hop redirects to final.
		first, final string
		wantPaths    []string
		wantModels   bool
	}{
		"success: the same target without userinfo is followed": {
			first:      "http://127.0.0.1:19993/final",
			wantPaths:  []string{"/v1/models", "/final"},
			wantModels: true,
		},
		"error: a first redirect to the same host with userinfo is refused": {
			first:     "http://check-user:check-pass@127.0.0.1:19993/final",
			wantPaths: []string{"/v1/models"},
		},
		"error: a first redirect to another host with userinfo is refused": {
			first:     "http://check-user:check-pass@proxy-elsewhere.test/final",
			wantPaths: []string{"/v1/models"},
		},
		"error: a later redirect with userinfo is refused": {
			first:     "/hop",
			final:     "http://check-user:check-pass@127.0.0.1:19993/final",
			wantPaths: []string{"/v1/models", "/hop"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			var authorized []string
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				if _, ok := r.Header["Authorization"]; ok {
					authorized = append(authorized, r.URL.Path)
				}
				mu.Unlock()
				switch r.URL.Path {
				case "/v1/models":
					w.Header().Set("Location", tt.first)
					w.WriteHeader(http.StatusFound)
				case "/hop":
					w.Header().Set("Location", tt.final)
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "/final":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"data":[]}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
				ProxyBinary:    fakeProxyBinary(t),
				ProxyModelsURL: "http://127.0.0.1:19993/v1/models",
				Client:         &http.Client{Transport: redirectChainTransport{handler: handler}},
			})
			want := readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: tt.wantModels}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("ProbeProxyAndAuth mismatch (-want +got):\n%s", diff)
			}
			mu.Lock()
			gotPaths, gotAuthorized := slices.Clone(paths), slices.Clone(authorized)
			mu.Unlock()
			if diff := gocmp.Diff(tt.wantPaths, gotPaths); diff != "" {
				t.Errorf("requested paths mismatch (-want +got):\n%s", diff)
			}
			if len(gotAuthorized) != 0 {
				t.Errorf("requests carrying Authorization = %q, want none", gotAuthorized)
			}
		})
	}
}
