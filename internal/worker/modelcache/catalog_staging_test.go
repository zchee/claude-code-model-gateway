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
	"fmt"
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

// cacheDirEvent is one change of a watched directory's entries: Op is "create", "modify" or "rename
// into the directory", and Dir marks an entry that is a directory.
type cacheDirEvent struct {
	Op   string
	Name string
	Dir  bool
}

// stagingFindings reads events as a reader that waits for final to appear would, and reports every
// event that reached the directory before final arrived, and whether it arrived. A finished file
// must arrive by one rename into the directory, so an arrival by create or modify is reported too.
func stagingFindings(events []cacheDirEvent, final string) ([]string, bool) {
	var findings []string
	for _, event := range events {
		if event.Name == final && !event.Dir {
			if event.Op != "rename into the directory" {
				findings = append(findings, fmt.Sprintf("%s first appeared by %s, not by a rename of the finished file into place", final, event.Op))
			}
			return findings, true
		}
		kind := "file"
		if event.Dir {
			kind = "directory"
		}
		findings = append(findings, fmt.Sprintf("%s of the %s %q before %s arrived", event.Op, kind, event.Name, final))
	}
	return findings, false
}

// stagingWatchRequests answers the proxy listing with one row and the Anthropic catalog page with
// one row.
type stagingWatchRequests struct{}

func (stagingWatchRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Host == "anthropic.test" && r.URL.Path == "/v1/models":
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-staged-1","display_name":"Staged One"}],"has_more":false}`))
	case r.URL.Path == "/v1/models":
		_, _ = w.Write([]byte(`{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestCatalogCacheArrivesWhole decides, for both catalog caches, what a non-recursive watcher on the
// cache's directory sees while the running cache writes it: no entry is created or modified in the
// directory before the finished file arrives, and the finished file arrives by one rename. Such a
// reader wakes on the first event and reads the file at once, so any earlier event (a staging file
// beside the target, or a staging directory created by the write) lets it read the old content.
// Both directories exist before the cache is created, as they do on a host where the gateway and
// Claude Code ran before, and the watch starts only after the cache started and committed its first
// listing, as a reader starts watching a running gateway. The gateway is wired only for the watched
// write, so no background discovery write overlaps it.
func TestCatalogCacheArrivesWhole(t *testing.T) {
	tests := map[string]struct {
		// final is the cache file's name; dir is its directory below the test home.
		final string
		dir   []string
		// write makes the running cache write its file once, synchronously.
		write func(t *testing.T, ctx context.Context, cache *modelcache.Cache, wired *atomic.Bool)
		// want is a string the written file must hold.
		want string
	}{
		"success: the Anthropic catalog cache arrives whole in the gateway state directory": {
			final: "anthropic-models.json",
			dir:   []string{".claude", "model-gateway"},
			write: func(t *testing.T, ctx context.Context, cache *modelcache.Cache, _ *atomic.Bool) {
				// A cold catalog makes the listing wait for the shared refresh, which returns only
				// after the catalog was written.
				if _, err := cache.ModelsResponse(ctx, http.Header{"X-Api-Key": {apiKeyMarker}}); err != nil {
					t.Errorf("ModelsResponse: %v", err)
				}
			},
			want: "claude-staged-1",
		},
		"success: the discovery cache arrives whole in Claude's cache directory": {
			final: "gateway-models.json",
			dir:   []string{".claude", "cache"},
			write: func(t *testing.T, _ context.Context, cache *modelcache.Cache, wired *atomic.Bool) {
				wired.Store(true)
				result, err := cache.SyncDiscoveryCache(true)
				if err != nil {
					t.Errorf("SyncDiscoveryCache: %v", err)
				}
				if result.State != "wrote" {
					t.Errorf("test precondition: SyncDiscoveryCache state %q (%s), want wrote", result.State, result.Reason)
				}
			},
			want: "kimi-k3",
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
			var wired atomic.Bool
			routes := &proxyCommitSignal{store: &route.Store{}, committed: make(chan struct{})}
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: catalogTestBase,
				DefaultBaseURL:   discoveryGatewayURL,
				Client:           &http.Client{Transport: handlerTransport{handler: stagingWatchRequests{}}},
				Clock:            time.Now,
				Routes:           routes,
				Sentry:           &sentry.State{},
				EffectiveBaseURL: func() (string, error) {
					if wired.Load() {
						return discoveryGatewayURL, nil
					}
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
			awaitSignal(t, routes.committed, "the first listing to commit")

			watch, observable := watchCacheDir(t, dir)
			tt.write(t, ctx, cache, &wired)
			raw, err := os.ReadFile(filepath.Join(dir, tt.final))
			if err != nil {
				t.Fatalf("read the written cache: %v", err)
			}
			if !bytes.Contains(raw, []byte(tt.want)) {
				t.Errorf("test precondition: %s holds %s, want it to name %s", tt.final, raw, tt.want)
			}
			if !observable {
				noteUnobservable()
				return
			}
			events := watch.events(t)
			findings, arrived := stagingFindings(events, tt.final)
			if !arrived {
				t.Errorf("the watcher saw no arrival of %s; events: %+v", tt.final, events)
			}
			if d := gocmp.Diff([]string(nil), findings); d != "" {
				t.Errorf("a watcher on %s saw the directory change before %s arrived whole (-want +got):\n%s", dir, tt.final, d)
			}
		})
	}
}

// TestCacheDirWatchControl shows that the watch of TestCatalogCacheArrivesWhole sees what a write
// does to the directory: a staging file written beside the target is reported before the target
// arrives, and a file staged in a subdirectory that existed before the watch and renamed into place
// arrives alone, because a non-recursive watch is not told about entries of a subdirectory.
func TestCacheDirWatchControl(t *testing.T) {
	const final = "catalog.json"
	tests := map[string]struct {
		// stage is the staging path relative to the watched directory; sub, when set, is created
		// before the watch starts.
		stage, sub   string
		wantFindings []string
	}{
		"success: a staging file beside the target is reported": {
			stage: "catalog.json.4242.0123456789ab.tmp",
			wantFindings: []string{
				`create of the file "catalog.json.4242.0123456789ab.tmp" before catalog.json arrived`,
				`modify of the file "catalog.json.4242.0123456789ab.tmp" before catalog.json arrived`,
			},
		},
		"success: a file staged in an existing subdirectory arrives alone": {
			stage: filepath.Join(".staging", "catalog.json.tmp"),
			sub:   ".staging",
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
			if tt.sub != "" {
				if err := os.Mkdir(filepath.Join(dir, tt.sub), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			watch, observable := watchCacheDir(t, dir)
			stage := filepath.Join(dir, tt.stage)
			if err := os.WriteFile(stage, []byte(`{"new":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(stage, filepath.Join(dir, final)); err != nil {
				t.Fatal(err)
			}
			if !observable {
				noteUnobservable()
				return
			}
			events := watch.events(t)
			findings, arrived := stagingFindings(events, final)
			if !arrived {
				t.Errorf("the watcher saw no arrival of %s; events: %+v", final, events)
			}
			if d := gocmp.Diff(tt.wantFindings, findings); d != "" {
				t.Errorf("findings (-want +got):\n%s", d)
			}
		})
	}
}
