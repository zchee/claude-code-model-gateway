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

package worker_test

import (
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// discoveryFile is the part of gateway-models.json the test compares.
type discoveryFile struct {
	BaseURL   string
	FetchedAt int64
	IDs       []string
}

func readDiscoveryFile(t *testing.T, path string) (discoveryFile, []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the discovery cache: %v", err)
	}
	var doc struct {
		BaseURL   string `json:"baseUrl"`
		FetchedAt int64  `json:"fetchedAt"`
		Models    []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the discovery cache: %v", err)
	}
	out := discoveryFile{BaseURL: doc.BaseURL, FetchedAt: doc.FetchedAt, IDs: []string{}}
	for _, m := range doc.Models {
		out.IDs = append(out.IDs, m.ID)
	}
	return out, raw
}

// TestCatalogBurstNoWriteWait checks that a burst of 50 concurrent GET /v1/models requests completes while a discovery
// synchronization is paused before it replaces the discovery-cache file. The source's handler only reads the published
// snapshot and leaves the file work to the refresh (lib/request-worker.js:1536-1538; the burst size is that of
// test/codex-upstream-cases.test.js:205). The test does not show progress while a writer is blocked inside the file
// replacement itself: the source does not have that property either, because its discovery write runs synchronously
// on the event loop.
//
// The listing is published by a completed refresh first. Then an explicit SyncDiscoveryCache is paused in
// EffectiveBaseURL, the first step it takes after locking the mutex that serializes discovery writes, and before it
// compares or replaces the file (internal/worker/modelcache/discovery.go). Only SyncDiscoveryCache calls
// EffectiveBaseURL, so the paused caller is that synchronization. The burst must complete while the pause is held,
// which a deadline cannot prove and does not: the pause returns only when the test releases it. After the release the
// synchronization's own result is the join.
func TestCatalogBurstNoWriteWait(t *testing.T) {
	const gatewayURL = "http://127.0.0.1:18764"
	const listing = `{"data":[{"id":"gpt-5.6-terra","display_name":"gpt-5.6-terra (codex)"},{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`
	const burst = 50
	tests := map[string]struct {
		force bool
		// wantState is the paused synchronization's result once released.
		wantState string
	}{
		"success: forced synchronization paused before it rewrites the file":   {force: true, wantState: "wrote"},
		"success: unforced synchronization paused before it compares the file": {force: false, wantState: "unchanged"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Created first so that its removal runs after every other cleanup below.
			home := t.TempDir()
			vars := map[string]string{
				"HOME":                    home,
				"CLAUDE_CONFIG_DIR":       filepath.Join(home, ".claude"),
				"MODEL_GATEWAY_STATE_DIR": filepath.Join(home, ".claude", "model-gateway"),
				"CCP_CONFIG_DIR":          filepath.Join(home, "proxy-config"),
			}
			lookup := func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
			resolver := paths.New(lookup, func() (string, error) { return home, nil }, func() (string, error) { return home, nil })
			discoveryPath, err := resolver.DiscoveryCachePath()
			if err != nil {
				t.Fatalf("DiscoveryCachePath: %v", err)
			}

			var listings atomic.Int64
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				listings.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, listing)
			}))
			t.Cleanup(proxy.Close)

			// A clock that advances on every reading makes a rewritten file distinguishable by its fetchedAt.
			base := time.UnixMilli(1_700_000_000_000).UTC()
			var ticks atomic.Int64
			clock := func() time.Time { return base.Add(time.Duration(ticks.Add(1)) * time.Millisecond) }

			var (
				armed    atomic.Bool
				parked   = make(chan struct{})
				released = make(chan struct{})
				release  = sync.OnceFunc(func() { close(released) })
			)
			store := &route.Store{}
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   proxy.URL,
				DefaultBaseURL: gatewayURL,
				Client:         proxy.Client(),
				Clock:          clock,
				Routes:         store,
				Sentry:         &sentry.State{},
				EffectiveBaseURL: func() (string, error) {
					if armed.CompareAndSwap(true, false) {
						close(parked)
						<-released
					}
					return gatewayURL, nil
				},
			})
			if err != nil {
				t.Fatalf("modelcache.New: %v", err)
			}
			t.Cleanup(cache.Wait)
			// The worker serves the catalog through the same cache methods compose.New wires.
			gateway := httptest.NewServer(handler(t, worker.Options{Routes: store, Models: cache.ModelsResponse, HealthCatalog: cache.HealthCatalog}))
			t.Cleanup(gateway.Close)
			t.Cleanup(gateway.Client().CloseIdleConnections)
			// Registered last, so it runs first: on any failure the pause is released and every goroutine the test
			// started has returned before the listener closes and the temporary directory is removed.
			var started sync.WaitGroup
			t.Cleanup(func() {
				release()
				started.Wait()
			})

			if err := cache.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if source, _ := cache.HealthCatalog(); source != "proxy" {
				t.Fatalf("catalog source after the refresh = %q, want the proxy listing", source)
			}
			before, beforeRaw := readDiscoveryFile(t, discoveryPath)

			type syncResult struct {
				result modelcache.DiscoverySyncResult
				err    error
			}
			synced := make(chan syncResult, 1)
			armed.Store(true)
			started.Go(func() {
				result, err := cache.SyncDiscoveryCache(tt.force)
				synced <- syncResult{result, err}
			})
			// This deadline only reports a setup that never reaches the pause.
			select {
			case <-parked:
			case r := <-synced:
				t.Fatalf("the synchronization returned (%+v, %v) without calling EffectiveBaseURL", r.result, r.err)
			case <-time.After(30 * time.Second):
				t.Fatal("the synchronization did not reach EffectiveBaseURL within 30s")
			}

			type response struct {
				status int
				ids    []string
				err    error
			}
			responses := make(chan response, burst)
			for range burst {
				started.Go(func() {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+"/v1/models", nil)
					if err != nil {
						responses <- response{err: err}
						return
					}
					resp, err := gateway.Client().Do(req)
					if err != nil {
						responses <- response{err: err}
						return
					}
					defer func() { _ = resp.Body.Close() }()
					var doc struct {
						Data []struct {
							ID string `json:"id"`
						} `json:"data"`
					}
					err = json.UnmarshalRead(resp.Body, &doc)
					ids := []string{}
					for _, row := range doc.Data {
						ids = append(ids, row.ID)
					}
					responses <- response{status: resp.StatusCode, ids: ids, err: err}
				})
			}
			// The deadline only turns a burst stuck behind the paused synchronization into a failure.
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			for i := range burst {
				select {
				case r := <-responses:
					if r.err != nil || r.status != http.StatusOK {
						t.Fatalf("model-list request %d: status %d, error %v", i, r.status, r.err)
					}
					// Only the refreshed proxy listing has the Kimi row; the fallback catalog lists Codex models only.
					if !slices.Contains(r.ids, "claude-kimi-k3") {
						t.Fatalf("model-list request %d served %q, want the refreshed proxy listing", i, r.ids)
					}
				case <-deadline.C:
					t.Fatalf("only %d of %d model-list requests answered while the discovery synchronization was paused", i, burst)
				}
			}
			select {
			case r := <-synced:
				t.Fatalf("the synchronization returned (%+v, %v) while it was paused", r.result, r.err)
			default:
			}
			if got := listings.Load(); got != 1 {
				t.Errorf("proxy listing fetched %d times, want 1: the burst must not start refreshes", got)
			}
			if _, raw := readDiscoveryFile(t, discoveryPath); string(raw) != string(beforeRaw) {
				t.Error("the discovery cache changed while its synchronization was paused")
			}

			release()
			r := <-synced
			if r.err != nil {
				t.Fatalf("SyncDiscoveryCache: %v", r.err)
			}
			if diff := gocmp.Diff(tt.wantState, r.result.State); diff != "" {
				t.Errorf("synchronization state (-want +got):\n%s", diff)
			}
			after, afterRaw := readDiscoveryFile(t, discoveryPath)
			if !slices.Contains(after.IDs, "claude-kimi-k3") || after.BaseURL != gatewayURL {
				t.Errorf("discovery cache after the release = %+v, want the refreshed listing under %s", after, gatewayURL)
			}
			if tt.force {
				// The forced write reads the clock after the refresh's write did, so a rewrite shows as a later fetchedAt.
				if after.FetchedAt <= before.FetchedAt {
					t.Errorf("fetchedAt = %d after the forced synchronization, want later than %d", after.FetchedAt, before.FetchedAt)
				}
			} else if string(afterRaw) != string(beforeRaw) {
				t.Error("an unforced synchronization of an unchanged listing rewrote the discovery cache")
			}
		})
	}
}
