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

//go:build unix

package modelcache_test

import (
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// catalogSaveLimit is a file size limit far below the fetched catalog and far above the failure record.
const catalogSaveLimit = 64 << 10

// TestCatalogSuccessWriteFailureIsRecorded fetches a valid catalog far larger than catalogSaveLimit
// while the process file size limit is lowered to it, so saving the fetched rows fails and only the
// small failure record fits. The saved catalog keeps the cached rows with the failure message for a
// failed save. Without the limit the same catalog is saved, so the size limit is the only difference.
// The limit applies to the whole test process; no test in this package runs in parallel, and it is
// lowered only around the refresh.
func TestCatalogSuccessWriteFailureIsRecorded(t *testing.T) {
	const rowCount = 2000
	rows := make([]idDisplay, 0, rowCount)
	fetchedIDs := make([]string, 0, rowCount)
	for i := range rowCount {
		id := fmt.Sprintf("claude-save-%04d", i)
		rows = append(rows, idDisplay{ID: id, DisplayName: "Saved row " + strings.Repeat("x", 200)})
		fetchedIDs = append(fetchedIDs, id)
	}
	page, err := json.Marshal(struct {
		Data    []idDisplay `json:"data"`
		HasMore bool        `json:"has_more"`
	}{Data: rows, HasMore: false})
	if err != nil {
		t.Fatalf("marshal the catalog page: %v", err)
	}
	if len(page) < 4*catalogSaveLimit {
		t.Fatalf("test precondition: the %d-byte page is not far over the %d-byte limit", len(page), catalogSaveLimit)
	}

	tests := map[string]struct {
		limit uint64
		want  savedCatalog
	}{
		"success: a fetched catalog that can be saved replaces the cached rows": {
			want: savedCatalog{IDs: fetchedIDs},
		},
		"error: a fetched catalog that cannot be saved records catalog write failed": {
			limit: catalogSaveLimit,
			want:  savedCatalog{IDs: []string{"claude-cached-1"}, Error: "catalog write failed"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, nil)
			catalogPath := filepath.Join(stateDirOf(t, resolver), "anthropic-models.json")
			writeTestFile(t, catalogPath, []byte(seededCatalog))
			upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(page)
			})
			var logs logCapture
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: "http://anthropic-catalog.test",
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: handlerTransport{handler: upstream}},
				Clock:            func() time.Time { return time.UnixMilli(staleCatalogNow) },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				Logf:             logs.logf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			restore := func() {}
			if tt.limit != 0 {
				restore = lowerFileSizeLimit(t, tt.limit)
			}
			_, err = cache.ModelsResponse(t.Context(), http.Header{"Authorization": {authBearerHeader}})
			cache.Wait()
			restore()
			if err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}

			if diff := gocmp.Diff(tt.want, readSavedCatalog(t, catalogPath)); diff != "" {
				t.Errorf("saved catalog after the refresh mismatch (-want +got):\n%s", diff)
			}
			if logged := logs.bytes(); len(logged) != 0 {
				t.Errorf("the refresh logged %q, want no log line", redactTestMarkers(string(logged)))
			}
		})
	}
}

// lowerFileSizeLimit lowers the process soft file size limit to limit and returns the function that
// restores it, which also runs at cleanup. A write that would grow a file past the limit then fails
// with EFBIG; the Go runtime does not stop the process for the accompanying SIGXFSZ.
func lowerFileSizeLimit(t *testing.T, limit uint64) func() {
	t.Helper()
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("read the file size limit: %v", err)
	}
	lowered := original
	lowered.Cur = min(limit, original.Max)
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &lowered); err != nil {
		t.Fatalf("lower the file size limit: %v", err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &original); err != nil {
				t.Errorf("restore the file size limit: %v", err)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}
