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
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf16"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const discoveryGatewayURL = "http://127.0.0.1:19999"

// proxyCommitSignal forwards every snapshot and closes committed when the first proxy snapshot is
// published.
type proxyCommitSignal struct {
	store     *route.Store
	committed chan struct{}
	once      sync.Once
}

func (p *proxyCommitSignal) Publish(snapshot route.Snapshot) {
	p.store.Publish(snapshot)
	if source, _ := snapshot.Catalog(); source == "proxy" {
		p.once.Do(func() { close(p.committed) })
	}
}

// discoveryFileIDs returns the model IDs of the discovery cache on disk.
func discoveryFileIDs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read discovery cache: %v", err)
	}
	var doc struct {
		Models []idDisplay `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal discovery cache: %v", err)
	}
	ids := make([]string, 0, len(doc.Models))
	for _, m := range doc.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestForcedDiscoverySyncNeverOverwritesNewerGeneration parks a forced discovery sync in its pin
// lookup, commits a newer generation meanwhile and lets its sync finish or wait behind the parked one,
// then lets the older sync finish: the discovery cache on disk holds the newer generation's rows.
func TestForcedDiscoverySyncNeverOverwritesNewerGeneration(t *testing.T) {
	tests := map[string]struct {
		listing string
		wantID  string
	}{
		"success: newer proxy table reaches the discovery cache last": {
			listing: `{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"},{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`,
			wantID:  "claude-kimi-k3",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, lookup, resolver := newTestPaths(t, nil)
				discoveryPath, err := resolver.DiscoveryCachePath()
				if err != nil {
					t.Fatalf("DiscoveryCachePath: %v", err)
				}
				pinsParked := make(chan struct{})
				releasePins := make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releasePins) }) }
				defer release()
				var pinCalls atomic.Int32
				pins := func() map[string]string {
					if pinCalls.Add(1) == 1 {
						close(pinsParked)
						select {
						case <-releasePins:
						case <-time.After(time.Minute):
							t.Error("the parked pin lookup was never released")
						}
					}
					return nil
				}
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/models" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tt.listing))
				})
				routes := &proxyCommitSignal{store: &route.Store{}, committed: make(chan struct{})}
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://127.0.0.1:19991",
					DefaultBaseURL:   discoveryGatewayURL,
					Client:           &http.Client{Transport: handlerTransport{handler: handler}},
					Clock:            time.Now,
					Routes:           routes,
					Sentry:           &sentry.State{},
					EffectiveBaseURL: baseURLFunc(discoveryGatewayURL),
					CurrentPins:      pins,
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}

				type syncOutcome struct {
					result modelcache.DiscoverySyncResult
					err    error
				}
				olderDone := make(chan syncOutcome, 1)
				go func() {
					result, err := cache.SyncDiscoveryCache(true)
					olderDone <- syncOutcome{result, err}
				}()
				awaitSignal(t, pinsParked, "the older forced sync to park in its pin lookup")

				var refreshErr error
				refreshDone := make(chan struct{})
				go func() {
					defer close(refreshDone)
					refreshErr = refreshBehindParkedSync(t.Context(), cache)
				}()
				awaitSignal(t, routes.committed, "the newer refresh to commit its proxy table")
				// The newer refresh either finishes its own sync, or waits on the lock the parked sync
				// holds; only then is the older sync released.
				refreshReturned, ok := doneOrLockWait(refreshDone, "modelcache_test.refreshBehindParkedSync")
				if !ok {
					t.Fatal("the newer refresh neither returned nor waited behind the parked sync")
				}

				release()
				var older syncOutcome
				select {
				case older = <-olderDone:
				case <-time.After(time.Minute):
					t.Fatal("timed out waiting for the older forced sync")
				}
				if older.err != nil {
					t.Fatalf("older SyncDiscoveryCache: %v", older.err)
				}
				if !refreshReturned {
					awaitSignal(t, refreshDone, "the newer refresh to return")
				}
				if refreshErr != nil {
					t.Fatalf("newer Refresh: %v", refreshErr)
				}

				ids := discoveryFileIDs(t, discoveryPath)
				if !slices.Contains(ids, tt.wantID) {
					t.Errorf("discovery cache on disk holds %q, want the newer generation's rows including %q: the older forced sync wrote last", ids, tt.wantID)
				}
				if result, err := cache.SyncDiscoveryCache(false); err != nil || result.State != "unchanged" {
					t.Errorf("SyncDiscoveryCache(false) after both writers = (%+v, %v), want unchanged: the file did not match the committed generation", result, err)
				}
			})
		})
	}
}

// refreshBehindParkedSync is the newer refresh's entry point; its frame names that goroutine in a
// goroutine dump.
//
//go:noinline
func refreshBehindParkedSync(ctx context.Context, cache *modelcache.Cache) error {
	return cache.Refresh(ctx)
}

// TestDiscoveryDisplayNameKeepsLoneSurrogate checks that a display name carrying an escaped lone
// surrogate reaches the discovery cache with its exact UTF-16 code units, and that a cached name
// spelled with U+FFFD in its place counts as a different name.
func TestDiscoveryDisplayNameKeepsLoneSurrogate(t *testing.T) {
	const rowID = "claude-opus-9-9"
	loneName := []uint16{'O', 'p', 'u', 's', ' ', 0xD800, ' ', 'n', 'i', 'n', 'e'}
	replacedName := slices.Clone(loneName)
	replacedName[5] = 0xFFFD

	tests := map[string]struct {
		existingName []uint16
		force        bool
		wantState    string
	}{
		"success: forced write keeps the lone surrogate": {
			force:     true,
			wantState: "wrote",
		},
		"success: cached name with U+FFFD instead of the lone surrogate is rewritten": {
			existingName: replacedName,
			wantState:    "wrote",
		},
		"success: cached name with the same lone surrogate is unchanged": {
			existingName: loneName,
			wantState:    "unchanged",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, nil)
			stateDir, err := resolver.StateDir()
			if err != nil {
				t.Fatalf("StateDir: %v", err)
			}
			discoveryPath, err := resolver.DiscoveryCachePath()
			if err != nil {
				t.Fatalf("DiscoveryCachePath: %v", err)
			}
			now := time.UnixMilli(1_900_000_000_000)
			rows := []byte(`[{"id":"` + rowID + `","display_name":"Opus \ud800 nine","type":"model"}]`)
			if err := anthropiccatalog.WriteSuccess(filepath.Join(stateDir, "anthropic-models.json"), rows, now); err != nil {
				t.Fatalf("seed the Anthropic catalog cache: %v", err)
			}
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				DefaultBaseURL:   discoveryGatewayURL,
				Client:           newFailingClient(),
				Clock:            func() time.Time { return now },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				EffectiveBaseURL: baseURLFunc(discoveryGatewayURL),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if tt.existingName != nil {
				if _, err := cache.SyncDiscoveryCache(true); err != nil {
					t.Fatalf("seed the discovery cache: %v", err)
				}
				rewriteDiscoveryName(t, discoveryPath, rowID, tt.existingName)
			}

			result, err := cache.SyncDiscoveryCache(tt.force)
			if err != nil {
				t.Fatalf("SyncDiscoveryCache: %v", err)
			}
			if result.State != tt.wantState {
				t.Errorf("SyncDiscoveryCache state = %q, want %q", result.State, tt.wantState)
			}
			if diff := gocmp.Diff(loneName, discoveryName(t, discoveryPath, rowID)); diff != "" {
				t.Errorf("display name code units in the discovery cache mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// discoveryModel returns the decoded models array of the discovery cache and the index of id.
func discoveryModel(t *testing.T, path, id string) (jsonx.WireValue, []jsonx.WireValue, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read discovery cache: %v", err)
	}
	doc, err := jsonx.DecodeWire(raw)
	if err != nil {
		t.Fatalf("decode discovery cache: %v", err)
	}
	var models []jsonx.WireValue
	for _, m := range doc.Members {
		if string(utf16.Decode(m.Name)) == "models" {
			models = m.Value.Elements
		}
	}
	for i, model := range models {
		for _, m := range model.Members {
			if string(utf16.Decode(m.Name)) == "id" && string(utf16.Decode(m.Value.Text)) == id {
				return doc, models, i
			}
		}
	}
	t.Fatalf("discovery cache has no model %q", id)
	return jsonx.WireValue{}, nil, -1
}

// discoveryName returns the UTF-16 code units of the display name of model id in the discovery cache.
func discoveryName(t *testing.T, path, id string) []uint16 {
	t.Helper()
	_, models, i := discoveryModel(t, path, id)
	for _, m := range models[i].Members {
		if string(utf16.Decode(m.Name)) == "display_name" {
			return m.Value.Text
		}
	}
	t.Fatalf("discovery model %q has no display name", id)
	return nil
}

// rewriteDiscoveryName replaces the display name of model id in the discovery cache with name,
// keeping every other member as written.
func rewriteDiscoveryName(t *testing.T, path, id string, name []uint16) {
	t.Helper()
	doc, models, i := discoveryModel(t, path, id)
	for j, m := range models[i].Members {
		if string(utf16.Decode(m.Name)) == "display_name" {
			models[i].Members[j].Value = jsonx.WireValue{Kind: '"', Text: slices.Clone(name)}
		}
	}
	encoded, err := jsonx.EncodeWire(doc)
	if err != nil {
		t.Fatalf("encode discovery cache: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("rewrite discovery cache: %v", err)
	}
}
