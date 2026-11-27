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

// parkOnce returns a gate whose first call after arm blocks until release. Every other call passes.
type parkOnce struct {
	armed    atomic.Bool
	parked   chan struct{}
	released chan struct{}
	once     sync.Once
}

func newParkOnce() *parkOnce {
	return &parkOnce{parked: make(chan struct{}), released: make(chan struct{})}
}

func (p *parkOnce) pass() {
	if p.armed.CompareAndSwap(true, false) {
		close(p.parked)
		<-p.released
	}
}

func (p *parkOnce) release() { p.once.Do(func() { close(p.released) }) }

// TestCatalogBurstNoWriteWait checks that GET /v1/models answers a burst of 50 concurrent requests from the published
// catalog while the refresh that published it is still inside its discovery-cache sync. The source's handler only
// reads the published snapshot and a refresh does the file work (lib/request-worker.js:1536-1538, and the burst of
// test/codex-upstream-cases.test.js:205). Each case parks the sync at a step it runs while holding the lock that
// serializes discovery writes (internal/worker/modelcache/discovery.go): the wiring read, the pin lookup, and the
// timestamp taken just before the file is encoded and written. The burst completes before the park is released, so
// no model-list response waited on the write; after the release the file holds the refreshed listing.
func TestCatalogBurstNoWriteWait(t *testing.T) {
	const gatewayURL = "http://127.0.0.1:18764"
	const listing = `{"data":[{"id":"gpt-5.6-terra","display_name":"gpt-5.6-terra (codex)"},{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`
	const burst = 50
	tests := map[string]struct {
		// park selects the seam the discovery sync is held in.
		park string
	}{
		"success: sync parked reading the wiring":         {park: "wiring"},
		"success: sync parked reading the pins":           {park: "pins"},
		"success: sync parked before the file is written": {park: "write"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
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

			gate := newParkOnce()
			store := &route.Store{}
			fixed := time.UnixMilli(1_700_000_000_000).UTC()
			opts := modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     proxy.URL,
				DefaultBaseURL:   gatewayURL,
				Client:           proxy.Client(),
				Clock:            func() time.Time { return fixed },
				Routes:           store,
				Sentry:           &sentry.State{},
				EffectiveBaseURL: func() (string, error) { return gatewayURL, nil },
			}
			switch tt.park {
			case "wiring":
				opts.EffectiveBaseURL = func() (string, error) { gate.pass(); return gatewayURL, nil }
			case "pins":
				opts.CurrentPins = func() map[string]string { gate.pass(); return nil }
			case "write":
				opts.Clock = func() time.Time { gate.pass(); return fixed }
			}
			cache, err := modelcache.New(opts)
			if err != nil {
				t.Fatalf("modelcache.New: %v", err)
			}
			t.Cleanup(cache.Wait)
			// The worker serves the catalog through the same cache methods compose.New wires.
			gateway := httptest.NewServer(handler(t, worker.Options{Routes: store, Models: cache.ModelsResponse, HealthCatalog: cache.HealthCatalog}))
			t.Cleanup(gateway.Close)
			t.Cleanup(gateway.Client().CloseIdleConnections)
			// Registered last so it runs first: a failed case releases the park before the server waits for
			// the requests that may still be held behind it.
			t.Cleanup(gate.release)

			// The refresh is the only caller between arming and the park, so the parked call is the sync's.
			gate.armed.Store(true)
			refreshed := make(chan error, 1)
			go func() { refreshed <- cache.Refresh(t.Context()) }()
			select {
			case <-gate.parked:
			case err := <-refreshed:
				t.Fatalf("the refresh returned (%v) without reaching the %s step of its discovery sync", err, tt.park)
			}
			if _, statErr := os.Stat(discoveryPath); !os.IsNotExist(statErr) {
				t.Fatalf("discovery cache exists before the parked sync wrote it: %v", statErr)
			}

			type response struct {
				status int
				ids    []string
				err    error
			}
			responses := make(chan response, burst)
			for range burst {
				go func() {
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
				}()
			}
			// The deadline only turns a burst stuck behind the parked write into a failure; success is the burst
			// completing while the park is still held.
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
					t.Fatalf("only %d of %d model-list requests answered while the discovery sync was parked at the %s step", i, burst, tt.park)
				}
			}
			select {
			case err := <-refreshed:
				t.Fatalf("the refresh returned (%v) while its discovery sync was parked", err)
			default:
			}
			if got := listings.Load(); got != 1 {
				t.Errorf("proxy listing fetched %d times, want 1: the burst must not start refreshes", got)
			}

			gate.release()
			if err := <-refreshed; err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			raw, err := os.ReadFile(discoveryPath)
			if err != nil {
				t.Fatalf("the released sync did not write the discovery cache: %v", err)
			}
			var written struct {
				BaseURL   string `json:"baseUrl"`
				FetchedAt int64  `json:"fetchedAt"`
				Models    []struct {
					ID string `json:"id"`
				} `json:"models"`
			}
			if err := json.Unmarshal(raw, &written); err != nil {
				t.Fatalf("decode the discovery cache: %v", err)
			}
			ids := []string{}
			for _, m := range written.Models {
				ids = append(ids, m.ID)
			}
			type file struct {
				BaseURL   string
				FetchedAt int64
				HasKimi   bool
			}
			got := file{written.BaseURL, written.FetchedAt, slices.Contains(ids, "claude-kimi-k3")}
			want := file{BaseURL: gatewayURL, FetchedAt: fixed.UnixMilli(), HasKimi: true}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("discovery cache after the release (-want +got):\n%s\nmodels: %q", diff, ids)
			}
		})
	}
}
