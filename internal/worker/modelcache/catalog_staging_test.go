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
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// cacheDirEvent is one change of a watched directory's entries: Op is "create", "modify",
// "metadata change", "rename into the directory", "rename out of the directory" or "delete", and
// Dir marks an entry that is a directory.
type cacheDirEvent struct {
	Op   string
	Name string
	Dir  bool
}

// arrivedWhole is the only change a reader that watches a cache directory may see for one cache
// write: the finished file renamed into place, which wakes the reader once with the new content.
func arrivedWhole(final string) []cacheDirEvent {
	return []cacheDirEvent{{Op: "rename into the directory", Name: final}}
}

// stagingWatchRequests answers the proxy listing with one row, and the Anthropic catalog page with
// one row, or with HTTP 401 when unauthorized is set.
type stagingWatchRequests struct {
	unauthorized bool
}

func (s stagingWatchRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Host == "anthropic.test" && r.URL.Path == "/v1/models" && s.unauthorized:
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error"}}`))
	case r.URL.Host == "anthropic.test" && r.URL.Path == "/v1/models":
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-staged-1","display_name":"Staged One"}],"has_more":false}`))
	case r.URL.Path == "/v1/models":
		_, _ = w.Write([]byte(`{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

const (
	// staleEmptyCatalog is a saved Anthropic catalog with no rows, an hour and more old, so a
	// listing with credentials waits for the refresh that replaces it.
	staleEmptyCatalog = `{"data":[],"updatedAt":0}`
	// oldDiscoveryCache is a discovery cache an earlier run left.
	oldDiscoveryCache = `{"baseUrl":"http://127.0.0.1:19999","fetchedAt":0,"models":[]}`
)

// TestCatalogCacheArrivesWhole decides, for both catalog caches, what a non-recursive watcher on the
// cache's directory sees while the running cache writes it, both when the file is new and when it
// replaces one an earlier run left: exactly one change, the finished file renamed into place, and
// nothing else before or after it. Such a reader wakes on the first change and reads the file at
// once, so any other change (a staging file beside the target, a staging directory created by the
// write, the old file deleted, moved away or changed in place, or the new file filled after its
// rename) lets it read the old or an incomplete content. The replacement of a saved catalog by a
// failure record after HTTP 401 is the write a hosted run caught. Both directories exist before
// the cache is created, as they do on a host where the gateway and Claude Code ran before. The
// watch starts once the started cache's first listing was stored and its own discovery sync found
// the gateway unwired, as a reader starts watching a running gateway, and the gateway is wired
// only for the watched write, so no background discovery write overlaps it.
func TestCatalogCacheArrivesWhole(t *testing.T) {
	tests := map[string]struct {
		// final is the cache file's name; dir is its directory below the test home.
		final string
		dir   []string
		// seed, when set, is the file an earlier run left at final.
		seed         string
		unauthorized bool
		// write makes the running cache write its file once, synchronously.
		write func(t *testing.T, ctx context.Context, cache *modelcache.Cache, wired *atomic.Bool)
		// want is a string the written file must hold.
		want string
	}{
		"success: a new Anthropic catalog cache arrives whole": {
			final: "anthropic-models.json", dir: []string{".claude", "model-gateway"},
			write: listWithCredentials, want: "claude-staged-1",
		},
		"success: a refreshed Anthropic catalog replaces a stale one whole": {
			final: "anthropic-models.json", dir: []string{".claude", "model-gateway"}, seed: staleEmptyCatalog,
			write: listWithCredentials, want: "claude-staged-1",
		},
		"success: the failure record after HTTP 401 replaces a stale catalog whole": {
			final: "anthropic-models.json", dir: []string{".claude", "model-gateway"}, seed: staleEmptyCatalog, unauthorized: true,
			write: listWithCredentials, want: `"error": "upstream HTTP 401"`,
		},
		"success: a new discovery cache arrives whole": {
			final: "gateway-models.json", dir: []string{".claude", "cache"},
			write: wireAndSyncDiscovery, want: "kimi-k3",
		},
		"success: a discovery cache replaces an old one whole": {
			final: "gateway-models.json", dir: []string{".claude", "cache"}, seed: oldDiscoveryCache,
			write: wireAndSyncDiscovery, want: "kimi-k3",
		},
	}
	// Observing the directory needs Linux inotify; elsewhere the write is still checked on disk, and
	// this is said once.
	noteUnobservable := sync.OnceFunc(func() {
		t.Log("the order of a directory's entries is not observed on this system: observing it needs Linux inotify")
	})
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, lookup, resolver := newTestPaths(t, nil)
			for _, dir := range []string{filepath.Join(home, ".claude", "model-gateway"), filepath.Join(home, ".claude", "cache")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
			}
			dir := filepath.Join(append([]string{home}, tt.dir...)...)
			if tt.seed != "" {
				if err := os.WriteFile(filepath.Join(dir, tt.final), []byte(tt.seed), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The started cache commits its first listing and then syncs the discovery cache, which
			// looks up the effective base URL; the first lookup that finds the gateway unwired
			// comes after the listing was stored, and that sync then writes nothing.
			var wired atomic.Bool
			unwiredLookup := make(chan struct{})
			signalUnwired := sync.OnceFunc(func() { close(unwiredLookup) })
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: catalogTestBase,
				DefaultBaseURL:   discoveryGatewayURL,
				Client:           &http.Client{Transport: handlerTransport{handler: stagingWatchRequests{unauthorized: tt.unauthorized}}},
				Clock:            time.Now,
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				EffectiveBaseURL: func() (string, error) {
					if wired.Load() {
						return discoveryGatewayURL, nil
					}
					signalUnwired()
					return "", nil
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer func() {
				cancel()
				cache.Wait()
			}()
			cache.Start(ctx)
			awaitSignal(t, unwiredLookup, "the startup discovery sync to find the gateway unwired")

			watch, observable := watchCacheDir(t, dir)
			tt.write(t, ctx, cache, &wired)
			raw, err := os.ReadFile(filepath.Join(dir, tt.final))
			if err != nil {
				t.Fatalf("read the written cache: %v", err)
			}
			if !bytes.Contains(raw, []byte(tt.want)) {
				t.Errorf("test precondition: %s holds %s, want it to hold %s", tt.final, raw, tt.want)
			}
			if !observable {
				noteUnobservable()
				return
			}
			if d := gocmp.Diff(arrivedWhole(tt.final), watch.events(t)); d != "" {
				t.Errorf("a watcher on %s saw more than %s arriving whole (-want +got):\n%s", dir, tt.final, d)
			}
		})
	}
}

// listWithCredentials lists the models with an API key. A catalog with no rows makes the listing
// wait for the shared refresh, which returns only after it wrote the catalog or its failure record.
func listWithCredentials(t *testing.T, ctx context.Context, cache *modelcache.Cache, _ *atomic.Bool) {
	t.Helper()
	if _, err := cache.ModelsResponse(ctx, http.Header{"X-Api-Key": {apiKeyMarker}}); err != nil {
		t.Errorf("ModelsResponse: %v", err)
	}
}

// wireAndSyncDiscovery wires the gateway and forces one discovery write.
func wireAndSyncDiscovery(t *testing.T, _ context.Context, cache *modelcache.Cache, wired *atomic.Bool) {
	t.Helper()
	wired.Store(true)
	result, err := cache.SyncDiscoveryCache(true)
	if err != nil {
		t.Errorf("SyncDiscoveryCache: %v", err)
	}
	if result.State != "wrote" {
		t.Errorf("test precondition: SyncDiscoveryCache state %q (%s), want wrote", result.State, result.Reason)
	}
}

// TestCacheDirWatchControl shows that the watch of TestCatalogCacheArrivesWhole reports every way a
// write can wake a reader before or after the finished file arrives, and nothing for a file staged
// in a subdirectory that existed before the watch and renamed into place, because a non-recursive
// watch is not told about a subdirectory's entries.
func TestCacheDirWatchControl(t *testing.T) {
	const final = "catalog.json"
	const sibling = "catalog.json.4242.0123456789ab.tmp"
	staged := filepath.Join(".staging", "catalog.json.tmp")
	tests := map[string]struct {
		// write changes dir, which holds final with old content and an empty .staging
		// subdirectory, both made before the watch.
		write func(t *testing.T, dir string)
		want  []cacheDirEvent
	}{
		"success: a file staged in an existing subdirectory and renamed in arrives whole": {
			write: func(t *testing.T, dir string) {
				writeAndRename(t, filepath.Join(dir, staged), filepath.Join(dir, final), `{"new":true}`)
			},
			want: arrivedWhole(final),
		},
		"error: a staging file beside the target is reported": {
			write: func(t *testing.T, dir string) {
				writeAndRename(t, filepath.Join(dir, sibling), filepath.Join(dir, final), `{"new":true}`)
			},
			want: []cacheDirEvent{{Op: "create", Name: sibling}, {Op: "modify", Name: sibling}, {Op: "rename out of the directory", Name: sibling}, {Op: "rename into the directory", Name: final}},
		},
		"error: deleting the old file before the new one arrives is reported": {
			write: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, final)); err != nil {
					t.Fatal(err)
				}
				writeAndRename(t, filepath.Join(dir, staged), filepath.Join(dir, final), `{"new":true}`)
			},
			want: []cacheDirEvent{{Op: "delete", Name: final}, {Op: "rename into the directory", Name: final}},
		},
		"error: moving the old file away before the new one arrives is reported": {
			write: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, final), filepath.Join(dir, ".staging", "old")); err != nil {
					t.Fatal(err)
				}
				writeAndRename(t, filepath.Join(dir, staged), filepath.Join(dir, final), `{"new":true}`)
			},
			want: []cacheDirEvent{{Op: "rename out of the directory", Name: final}, {Op: "rename into the directory", Name: final}},
		},
		"error: an empty file renamed in and filled afterwards is reported": {
			write: func(t *testing.T, dir string) {
				writeAndRename(t, filepath.Join(dir, staged), filepath.Join(dir, final), "")
				f, err := os.OpenFile(filepath.Join(dir, final), os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = f.Close() }()
				if _, err := f.WriteString(`{"new":true}`); err != nil {
					t.Fatal(err)
				}
			},
			want: []cacheDirEvent{{Op: "rename into the directory", Name: final}, {Op: "modify", Name: final}},
		},
		"error: changing the new file's mode after it arrived is reported": {
			write: func(t *testing.T, dir string) {
				writeAndRename(t, filepath.Join(dir, staged), filepath.Join(dir, final), `{"new":true}`)
				if err := os.Chmod(filepath.Join(dir, final), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: []cacheDirEvent{{Op: "rename into the directory", Name: final}, {Op: "metadata change", Name: final}},
		},
	}
	noteUnobservable := sync.OnceFunc(func() {
		t.Log("the order of a directory's entries is not observed on this system: observing it needs Linux inotify")
	})
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, final), []byte(`{"old":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, ".staging"), 0o700); err != nil {
				t.Fatal(err)
			}
			watch, observable := watchCacheDir(t, dir)
			tt.write(t, dir)
			if !observable {
				noteUnobservable()
				return
			}
			if d := gocmp.Diff(tt.want, watch.events(t)); d != "" {
				t.Errorf("events (-want +got):\n%s", d)
			}
		})
	}
}

// writeAndRename writes data to stage and renames it to final.
func writeAndRename(t *testing.T, stage, final, data string) {
	t.Helper()
	if err := os.WriteFile(stage, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stage, final); err != nil {
		t.Fatal(err)
	}
}
