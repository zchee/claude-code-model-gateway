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
	"context"
	json "encoding/json/v2"
	"net/http"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// liveWorkerRequests answers the proxy listing with one row and counts those requests, and parks the
// Anthropic catalog page request until the test releases it, when it answers one catalog row, or until
// its context ends, which it records.
type liveWorkerRequests struct {
	listings   atomic.Int32
	arrived    chan struct{}
	arriveOnce sync.Once
	release    chan struct{}
	canceled   atomic.Bool
}

func (l *liveWorkerRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Host != "anthropic.test" {
		l.listings.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`))
		return
	}
	l.arriveOnce.Do(func() { close(l.arrived) })
	select {
	case <-l.release:
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-live-1","display_name":"Live One"}],"has_more":false}`))
	case <-r.Context().Done():
		l.canceled.Store(true)
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// TestWaitDoesNotCancelLiveWorker starts the cache with a one-second refresh interval, admits a catalog
// refresh and parks its page request, and then calls Wait from its own goroutine while the service
// context stays live. Wait cancels nothing: the parked page request's context stays live, periodic
// listing refreshes go on, the released refresh saves its rows, and Wait returns only once the service
// context ends.
func TestWaitDoesNotCancelLiveWorker(t *testing.T) {
	const (
		interval = time.Second
		observe  = 1500 * time.Millisecond
	)
	tests := map[string]struct {
		credentials http.Header
		wantSaved   savedCatalog
	}{
		"success: a catalog refresh admitted before Wait runs to its write while the worker lives": {
			credentials: http.Header{"X-Api-Key": {apiKeyMarker}},
			wantSaved:   savedCatalog{IDs: []string{"claude-live-1"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if observe <= interval || observe >= modelcache.AnthropicPageTimeout {
				t.Fatalf("test precondition: the observation window %v must span a refresh interval %v and stay below the page deadline %v", observe, interval, modelcache.AnthropicPageTimeout)
			}
			synctest.Test(t, func(t *testing.T) {
				requests := &liveWorkerRequests{arrived: make(chan struct{}), release: make(chan struct{})}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(requests.release) }) }
				_, lookup, resolver := newTestPaths(t, map[string]string{"CODEX_GATEWAY_MODELS_REFRESH_MS": "1000"})
				routes := &proxyCommitSignal{store: &route.Store{}, committed: make(chan struct{})}
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://127.0.0.1:19991",
					AnthropicBaseURL: catalogTestBase,
					DefaultBaseURL:   "http://127.0.0.1:19999",
					Client:           &http.Client{Transport: handlerTransport{handler: requests}},
					Clock:            time.Now,
					Routes:           routes,
					Sentry:           &sentry.State{},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				waitDone := make(chan struct{})
				var waitStarted atomic.Bool
				defer func() {
					release()
					cancel()
					if waitStarted.Load() {
						<-waitDone
					} else {
						cache.Wait()
					}
				}()
				cache.Start(ctx)
				awaitSignal(t, routes.committed, "the first refresh to commit")

				cache.TriggerAnthropicRefresh(tt.credentials)
				awaitSignal(t, requests.arrived, "the catalog refresh to request its first page")
				admitted := time.Now()
				listingsBefore := requests.listings.Load()

				waitStarted.Store(true)
				go func() {
					defer close(waitDone)
					cache.Wait()
				}()
				time.Sleep(observe)
				synctest.Wait()
				if elapsed := time.Since(admitted); elapsed >= modelcache.AnthropicPageTimeout {
					t.Fatalf("test precondition: %v passed since the page request arrived, want less than the page deadline %v", elapsed, modelcache.AnthropicPageTimeout)
				}
				if requests.canceled.Load() {
					t.Error("the parked catalog page request saw its context end after Wait, want it live while the service context lives")
				}
				if n := requests.listings.Load() - listingsBefore; n < 1 {
					t.Errorf("listing requests during %v after Wait was called = %d, want the periodic refresh to go on", observe, n)
				}
				select {
				case <-waitDone:
					t.Error("Wait returned while the service context was live, want it to join the running worker")
				default:
				}

				release()
				synctest.Wait()
				path := filepath.Join(stateDirOf(t, resolver), "anthropic-models.json")
				if diff := gocmp.Diff(tt.wantSaved, readSavedCatalog(t, path)); diff != "" {
					t.Errorf("saved catalog after the released refresh mismatch (-want +got):\n%s", diff)
				}
				select {
				case <-waitDone:
					t.Error("Wait returned after the catalog refresh ended but before the service context did")
				default:
				}

				cancel()
				awaitSignal(t, waitDone, "Wait to return after the service context ended")
			})
		})
	}
}

// TestCatalogRefreshReturnsSavedCatalogReread has a cold caller wait for a catalog refresh whose
// fetched rows are saved, while another writer replaces the saved catalog right after that write; the
// replacement happens in the discovery sync the refresh runs after it saves. The caller is served the
// catalog as read back from disk after the write, the replacement, and not the fetched rows.
func TestCatalogRefreshReturnsSavedCatalogReread(t *testing.T) {
	tests := map[string]struct {
		fetched     string
		replacement string
		wantIDs     []string
		absentID    string
	}{
		"success: a cold caller gets the catalog read back after the write": {
			fetched:     `{"data":[{"id":"claude-fetched-1","display_name":"Fetched One"}],"has_more":false}`,
			replacement: `[{"id":"claude-replaced-1","display_name":"Replaced One"}]`,
			wantIDs:     []string{"claude-replaced-1"},
			absentID:    "claude-fetched-1",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, nil)
			path := filepath.Join(stateDirOf(t, resolver), "anthropic-models.json")
			now := time.UnixMilli(staleCatalogNow)
			var syncs atomic.Int32
			var savedAtSync []string
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: catalogTestBase,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           &http.Client{Transport: pageTransport{body: []byte(tt.fetched)}},
				Clock:            func() time.Time { return now },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				EffectiveBaseURL: func() (string, error) {
					if syncs.Add(1) == 1 {
						// This runs on the refresh goroutine, so it reports a failure without stopping the test.
						var doc struct {
							Data []idDisplay `json:"data"`
						}
						if err := json.Unmarshal(anthropiccatalog.Read(path).Object, &doc); err != nil {
							t.Errorf("decode the saved catalog during the discovery sync: %v", err)
						}
						for _, row := range doc.Data {
							savedAtSync = append(savedAtSync, row.ID)
						}
						if err := anthropiccatalog.WriteSuccess(path, []byte(tt.replacement), now); err != nil {
							t.Errorf("replace the saved catalog: %v", err)
						}
					}
					return "", nil
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			body, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}})
			cache.Wait()
			if err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			if n := syncs.Load(); n != 1 {
				t.Fatalf("test precondition: discovery syncs during the refresh = %d, want 1", n)
			}
			if !slices.Contains(savedAtSync, tt.absentID) {
				t.Fatalf("test precondition: saved catalog when the discovery sync ran = %q, want the fetched row %s", savedAtSync, tt.absentID)
			}
			ids := responseRowIDs(t, body)
			for _, id := range tt.wantIDs {
				if !slices.Contains(ids, id) {
					t.Errorf("cold caller's listing = %q, want %s from the catalog read back after the write", ids, id)
				}
			}
			if slices.Contains(ids, tt.absentID) {
				t.Errorf("cold caller's listing = %q, want no %s: the fetched rows were replaced on disk before the read back", ids, tt.absentID)
			}
		})
	}
}
