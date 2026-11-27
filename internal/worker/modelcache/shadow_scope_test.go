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
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// TestShadowCacheStaysInItsRoot decides where a model cache in shadow mode may create anything: in
// its own root, the gateway state directory it is given, and nowhere under the Claude
// configuration directory, which belongs to the Claude Code installation the shadow instance must
// leave untouched. Shadow mode never writes the discovery cache there, so it needs no staging
// directory there either. The cache is created, started, and refreshed once more after its first
// listing; a watch installed before the cache was created observes any creation under the Claude
// configuration directory, and the directory is checked afterwards as well. Without shadow mode
// the same cache creates the discovery cache's staging directory there, which shows the watch and
// the checks see such a creation; that control leaves the gateway unwired, so its discovery cache
// is not written and the staging directory is the only creation.
func TestShadowCacheStaysInItsRoot(t *testing.T) {
	tests := map[string]struct {
		shadow bool
		// wired makes the gateway wired, so that only shadow mode keeps the discovery cache from
		// being written.
		wired bool
		// claudeExists creates the Claude configuration directory, empty, before the cache.
		claudeExists bool
		// wantClaude lists the Claude configuration directory's entries afterwards, recursively;
		// nil wants the directory absent.
		wantClaude []string
		// wantCreated lists the creations the watch must see under the Claude configuration
		// directory, or of it.
		wantCreated []cacheDirEvent
	}{
		"success: in shadow mode nothing is created in an existing Claude configuration directory": {
			shadow: true, wired: true, claudeExists: true, wantClaude: []string{},
		},
		"success: in shadow mode no Claude configuration directory is created": {
			shadow: true, wired: true,
		},
		"success: without shadow mode the discovery staging directory is created in the Claude configuration directory": {
			claudeExists: true,
			wantClaude:   []string{"cache", filepath.Join("cache", atomicfile.StagingDirName)},
			wantCreated:  []cacheDirEvent{{Op: "create", Name: "cache", Dir: true}},
		},
	}
	noteUnobservable := sync.OnceFunc(func() {
		t.Log("creations are not observed on this system: observing them needs Linux inotify; the directory is still checked afterwards")
	})
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			shadowRoot := filepath.Join(t.TempDir(), "shadow-root")
			if err := os.Mkdir(shadowRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			home, lookup, resolver := newTestPaths(t, map[string]string{"MODEL_GATEWAY_STATE_DIR": shadowRoot})
			claude := filepath.Join(home, ".claude")
			// The watch is on the Claude configuration directory when it exists, and otherwise on
			// its parent, where creating it would show.
			watched := home
			if tt.claudeExists {
				if err := os.Mkdir(claude, 0o700); err != nil {
					t.Fatal(err)
				}
				watched = claude
			}
			watch, observable := watchCacheDir(t, watched)

			var effective func() (string, error)
			if tt.wired {
				effective = baseURLFunc(discoveryGatewayURL)
			}
			routes := &proxyCommitSignal{store: &route.Store{}, committed: make(chan struct{})}
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: catalogTestBase,
				DefaultBaseURL:   discoveryGatewayURL,
				Shadow:           tt.shadow,
				Client:           &http.Client{Transport: handlerTransport{handler: stagingWatchRequests{}}},
				Clock:            time.Now,
				Routes:           routes,
				Sentry:           &sentry.State{},
				EffectiveBaseURL: effective,
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
			if err := cache.Refresh(ctx); err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			if observable {
				var created []cacheDirEvent
				for _, event := range watch.events(t) {
					// Below an existing Claude configuration directory every change counts; on its
					// parent only the directory itself does.
					if tt.claudeExists || event.Name == ".claude" {
						created = append(created, event)
					}
				}
				if d := gocmp.Diff(tt.wantCreated, created); d != "" {
					t.Errorf("changes observed under the Claude configuration directory %s (-want +got):\n%s", claude, d)
				}
			} else {
				noteUnobservable()
			}
			if d := gocmp.Diff(tt.wantClaude, treeEntries(t, claude)); d != "" {
				t.Errorf("entries of the Claude configuration directory %s afterwards (-want +got):\n%s", claude, d)
			}
			if info, err := os.Lstat(filepath.Join(shadowRoot, atomicfile.StagingDirName)); err != nil || !info.IsDir() {
				t.Errorf("the cache's own root %s has no catalog staging directory: %v", shadowRoot, err)
			}
		})
	}
}

// treeEntries returns every path below root, relative to it and sorted, or nil when root is absent.
func treeEntries(t *testing.T, root string) []string {
	t.Helper()
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	entries := []string{}
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != root {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			entries = append(entries, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(entries)
	return entries
}
