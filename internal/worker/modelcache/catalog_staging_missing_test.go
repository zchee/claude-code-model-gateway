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
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// stagingEntries returns the sorted names in dir.
func stagingEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// TestCatalogCacheWriteWithoutStagingDir decides what becomes of a catalog cache write whose
// private staging subdirectory is gone, for example removed after the cache created it. There is
// no second staging path: the write fails and is reported as any failed cache write is. A failed
// Anthropic catalog write leads to the failure record, whose write fails the same way and is
// logged by class only; a failed discovery write returns its error. Neither leaves an entry in the
// cache directory. With the subdirectory the cache created, both writes succeed and log nothing.
func TestCatalogCacheWriteWithoutStagingDir(t *testing.T) {
	tests := map[string]struct {
		// dir is the cache directory below the test home; final is the cache file's name.
		dir   []string
		final string
		// removeStaging removes the staging subdirectory after the cache created it.
		removeStaging bool
		// write makes the cache write its file once, synchronously, and returns the error the
		// cache reported to its caller.
		write   func(t *testing.T, cache *modelcache.Cache) error
		wantErr error
		wantLog string
		// want is a string the written file must hold when the write succeeds.
		want string
	}{
		"success: the Anthropic catalog is written through the staging subdirectory the cache created": {
			dir: []string{".claude", "model-gateway"}, final: "anthropic-models.json",
			write: modelsWithCredentials, want: "claude-staged-1",
		},
		"error: the Anthropic catalog without its staging subdirectory fails and its failure record is logged": {
			dir: []string{".claude", "model-gateway"}, final: "anthropic-models.json", removeStaging: true,
			write:   modelsWithCredentials,
			wantLog: "model-gateway: could not record the Anthropic catalog refresh failure (no such file or directory)\n",
		},
		"success: the discovery cache is written through the staging subdirectory the cache created": {
			dir: []string{".claude", "cache"}, final: "gateway-models.json",
			write: syncDiscoveryWrote, want: `"baseUrl"`,
		},
		"error: the discovery cache without its staging subdirectory fails with its error": {
			dir: []string{".claude", "cache"}, final: "gateway-models.json", removeStaging: true,
			write: syncDiscoveryWrote, wantErr: fs.ErrNotExist,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, lookup, resolver := newTestPaths(t, nil)
			var logs logCapture
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: catalogTestBase,
				DefaultBaseURL:   discoveryGatewayURL,
				Client:           &http.Client{Transport: handlerTransport{handler: stagingWatchRequests{}}},
				Clock:            time.Now,
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				EffectiveBaseURL: baseURLFunc(discoveryGatewayURL),
				Logf:             logs.logf,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer cache.Wait()
			dir := filepath.Join(append([]string{home}, tt.dir...)...)
			staging := filepath.Join(dir, atomicfile.StagingDirName)
			if info, err := os.Lstat(staging); err != nil || !info.IsDir() {
				t.Fatalf("test precondition: the cache did not create its staging subdirectory %s: %v", staging, err)
			}
			if tt.removeStaging {
				if err := os.Remove(staging); err != nil {
					t.Fatal(err)
				}
			}
			before := stagingEntries(t, dir)

			err = tt.write(t, cache)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("the cache reported %v, want an error matching %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("the cache reported %v, want no error", err)
			}
			raw, readErr := os.ReadFile(filepath.Join(dir, tt.final))
			switch {
			case tt.removeStaging:
				if !errors.Is(readErr, fs.ErrNotExist) {
					t.Errorf("%s after the failed write: read error %v (%q), want it absent", tt.final, readErr, raw)
				}
				if d := gocmp.Diff(before, stagingEntries(t, dir)); d != "" {
					t.Errorf("entries of %s changed by the failed write (-before +after):\n%s", dir, d)
				}
			case readErr != nil:
				t.Errorf("read %s: %v", tt.final, readErr)
			case !bytes.Contains(raw, []byte(tt.want)):
				t.Errorf("%s holds %s, want it to name %s", tt.final, raw, tt.want)
			}
			if d := gocmp.Diff(tt.wantLog, string(logs.bytes())); d != "" {
				t.Errorf("cache log (-want +got):\n%s", d)
			}
		})
	}
}

// modelsWithCredentials lists the models with an API key, which on a cold catalog waits for the
// catalog refresh and its write.
func modelsWithCredentials(t *testing.T, cache *modelcache.Cache) error {
	t.Helper()
	_, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}})
	return err
}

// syncDiscoveryWrote forces a discovery write and returns its error, requiring the wrote state
// when there is none.
func syncDiscoveryWrote(t *testing.T, cache *modelcache.Cache) error {
	t.Helper()
	result, err := cache.SyncDiscoveryCache(true)
	if err == nil && result.State != "wrote" {
		t.Errorf("test precondition: SyncDiscoveryCache state %q (%s), want wrote", result.State, result.Reason)
	}
	return err
}
