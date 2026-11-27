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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// catalogRecorder answers every catalog request in memory with status and body and records each
// request's path.
type catalogRecorder struct {
	status int
	body   string

	mu    sync.Mutex
	paths []string
}

func (r *catalogRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	return &http.Response{
		StatusCode:    r.status,
		Header:        http.Header{"Content-Type": {"application/json"}},
		ContentLength: int64(len(r.body)),
		Body:          io.NopCloser(strings.NewReader(r.body)),
		Request:       req,
	}, nil
}

func (r *catalogRecorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A non-nil copy, so no request compares equal to an empty want.
	out := make([]string, len(r.paths))
	copy(out, r.paths)
	return out
}

// TestInvalidCatalogBaseURLDisablesRefreshSilently configures an Anthropic base URL that does not
// parse, is relative, names no scheme, or is empty, with a stale saved catalog and request credentials
// that would otherwise start a refresh. The cache sends no catalog request, keeps serving the saved
// row, leaves the saved catalog byte for byte as it was, with no failure recorded, and logs nothing.
// A working base URL with the same inputs refreshes the catalog, without a log line either.
func TestInvalidCatalogBaseURLDisablesRefreshSilently(t *testing.T) {
	const freshPage = `{"data":[{"id":"claude-fresh-1","display_name":"Fresh One"}],"has_more":false}`
	tests := map[string]struct {
		base string
		// wantRequests is the catalog paths requested, wantSaved the saved catalog afterwards; a nil
		// wantSaved requires the file unchanged.
		wantRequests []string
		wantSaved    *savedCatalog
	}{
		"error: a base URL that does not parse disables the refresh": {
			base:         "http://[::1",
			wantRequests: []string{},
		},
		"error: a relative base URL disables the refresh": {
			base:         "/anthropic",
			wantRequests: []string{},
		},
		"error: a base URL without a scheme disables the refresh": {
			base:         "//anthropic.test",
			wantRequests: []string{},
		},
		"error: an empty base URL disables the refresh": {
			base:         "",
			wantRequests: []string{},
		},
		"success: a working base URL refreshes the catalog": {
			base:         catalogTestBase,
			wantRequests: []string{"/v1/models"},
			wantSaved:    &savedCatalog{IDs: []string{"claude-fresh-1"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			upstream := &catalogRecorder{status: http.StatusOK, body: freshPage}
			_, lookup, resolver := newTestPaths(t, nil)
			catalogPath := filepath.Join(stateDirOf(t, resolver), "anthropic-models.json")
			writeTestFile(t, catalogPath, []byte(seededCatalog))
			var logs logCapture
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: tt.base,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: upstream},
				Clock:            func() time.Time { return time.UnixMilli(staleCatalogNow) },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				Logf:             logs.logf,
			})
			if err != nil {
				t.Fatalf("New with base URL %q: %v, want the cache constructed", tt.base, err)
			}
			incoming := http.Header{"X-Api-Key": {apiKeyMarker}}
			body, err := cache.ModelsResponse(t.Context(), incoming)
			if err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			cache.TriggerAnthropicRefresh(incoming)
			cache.Wait()
			if diff := gocmp.Diff(tt.wantRequests, upstream.requests()); diff != "" {
				t.Errorf("catalog requests with base URL %q mismatch (-want +got):\n%s", tt.base, diff)
			}
			if ids := responseRowIDs(t, body); !slices.Contains(ids, "claude-cached-1") {
				t.Errorf("listing = %q, want the saved row claude-cached-1", ids)
			}
			if tt.wantSaved == nil {
				if saved, err := os.ReadFile(catalogPath); err != nil || string(saved) != seededCatalog {
					t.Errorf("saved catalog with base URL %q = %q (err %v), want it unchanged %q", tt.base, saved, err, seededCatalog)
				}
			} else if diff := gocmp.Diff(*tt.wantSaved, readSavedCatalog(t, catalogPath)); diff != "" {
				t.Errorf("saved catalog after the refresh mismatch (-want +got):\n%s", diff)
			}
			if logged := logs.bytes(); len(logged) != 0 {
				t.Errorf("log with base URL %q = %q, want no log line", tt.base, redactTestMarkers(string(logged)))
			}
		})
	}
}

// TestCatalogFailureRecordWriteFailureIsLogged answers a catalog refresh with HTTP 503 while a
// directory stands where the saved catalog belongs, so the failure record cannot be written. The cache
// logs one line that names the failure by its class, without the path or the request credentials,
// and the directory stays. With a writable saved catalog, the same refusal is recorded in the file and
// nothing is logged.
func TestCatalogFailureRecordWriteFailureIsLogged(t *testing.T) {
	// recordFailureLog restates the line the cache logs when the failure record cannot be written,
	// with the class of a rename onto a directory (EISDIR).
	const recordFailureLog = "model-gateway: could not record the Anthropic catalog refresh failure (is a directory)\n"
	tests := map[string]struct {
		directoryLeaf bool
		wantLog       string
		// wantSaved is the saved catalog afterwards when the leaf is a file.
		wantSaved savedCatalog
	}{
		"error: a directory at the catalog leaf logs the failed record by class only": {
			directoryLeaf: true,
			wantLog:       recordFailureLog,
		},
		"success: a writable saved catalog records the failure without a log line": {
			wantSaved: savedCatalog{IDs: []string{"claude-cached-1"}, Error: "upstream HTTP 503"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			upstream := &catalogRecorder{status: http.StatusServiceUnavailable, body: `{"type":"error"}`}
			_, lookup, resolver := newTestPaths(t, nil)
			stateDir := stateDirOf(t, resolver)
			catalogPath := filepath.Join(stateDir, "anthropic-models.json")
			if tt.directoryLeaf {
				if err := os.MkdirAll(catalogPath, 0o700); err != nil {
					t.Fatalf("create a directory at the catalog leaf: %v", err)
				}
			} else {
				writeTestFile(t, catalogPath, []byte(seededCatalog))
			}
			var logs logCapture
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: catalogTestBase,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: upstream},
				Clock:            func() time.Time { return time.UnixMilli(staleCatalogNow) },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				Logf:             logs.logf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := cache.ModelsResponse(t.Context(), http.Header{"Authorization": {authBearerHeader}}); err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			cache.Wait()
			if got := upstream.requests(); !slices.Equal(got, []string{"/v1/models"}) {
				t.Fatalf("test precondition: catalog requests = %q, want one request for /v1/models", got)
			}
			logged := string(logs.bytes())
			if logged != tt.wantLog {
				t.Errorf("log = %q, want %q", redactTestMarkers(logged), tt.wantLog)
			}
			for _, leaked := range []string{stateDir, "anthropic-models.json", authBearerPayload, "anthropic.test"} {
				if strings.Contains(logged, leaked) {
					t.Errorf("log names %q, want only the failure class", redactTestMarkers(leaked))
				}
			}
			if tt.directoryLeaf {
				if info, err := os.Lstat(catalogPath); err != nil || !info.IsDir() {
					t.Errorf("catalog leaf after the refresh: %v (err %v), want the directory left in place", info, err)
				}
				return
			}
			if diff := gocmp.Diff(tt.wantSaved, readSavedCatalog(t, catalogPath)); diff != "" {
				t.Errorf("saved catalog after the refused refresh mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
