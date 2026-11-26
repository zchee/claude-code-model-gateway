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

//go:build process && unix

package modelcache_test

import (
	"bytes"
	json "encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	// fifoReadWatchdog is how long one cache operation may take before the test treats it as blocked
	// on a FIFO.
	fifoReadWatchdog = 5 * time.Second
	// fifoUnblockLimit bounds the attempts to complete a read that blocked on a FIFO.
	fifoUnblockLimit = 10 * time.Second
)

func newFIFOCache(t *testing.T) (*modelcache.Cache, paths.Resolver) {
	t.Helper()
	_, lookup, resolver := newTestPaths(t, nil)
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            resolver,
		ProxyBaseURL:     "http://127.0.0.1:19991",
		DefaultBaseURL:   discoveryGatewayURL,
		Client:           newFailingClient(),
		Clock:            time.Now,
		Routes:           &route.Store{},
		Sentry:           &sentry.State{},
		EffectiveBaseURL: baseURLFunc(discoveryGatewayURL),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cache, resolver
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
}

// unblockFIFOReader opens the write side of fifo without blocking, which completes a reader blocked
// in its open, then closes it so the reader sees end of file.
func unblockFIFOReader(fifo string) {
	if fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0); err == nil {
		_ = unix.Close(fd)
	}
}

// awaitFIFOReader waits for done. When it does not close within fifoReadWatchdog it reports blocked
// and completes the reader blocked on fifo; unblocked is false when even that does not end it.
func awaitFIFOReader(done <-chan struct{}, fifo string) (blocked, unblocked bool) {
	select {
	case <-done:
		return false, true
	case <-time.After(fifoReadWatchdog):
	}
	return true, unblockFIFOReaderUntil(done, fifo)
}

// unblockFIFOReaderUntil completes the reader blocked on fifo, retrying the write-side open (which
// fails while no reader has the FIFO open) until done closes. It reports false when done is still
// open after fifoUnblockLimit.
func unblockFIFOReaderUntil(done <-chan struct{}, fifo string) bool {
	limit := time.After(fifoUnblockLimit)
	for {
		unblockFIFOReader(fifo)
		select {
		case <-done:
			return true
		case <-limit:
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestBoundedReadsTreatFIFOAsAbsent puts a FIFO with no writer at each path the cache reads through
// its size bound. Each operation returns promptly and treats the file as absent.
func TestBoundedReadsTreatFIFOAsAbsent(t *testing.T) {
	tests := map[string]struct {
		path func(t *testing.T, resolver paths.Resolver) string
		run  func(t *testing.T, cache *modelcache.Cache) string
		want string
	}{
		"success: models.json stand-in": {
			path: func(t *testing.T, resolver paths.Resolver) string {
				dir, err := resolver.StateDir()
				if err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "models.json")
			},
			run: func(t *testing.T, cache *modelcache.Cache) string {
				if err := cache.Refresh(t.Context()); err != nil {
					t.Errorf("Refresh: %v", err)
				}
				source, _ := cache.HealthCatalog()
				return source
			},
			want: "fallback",
		},
		"success: Claude global config": {
			path: func(t *testing.T, resolver paths.Resolver) string {
				path, err := resolver.GlobalConfigPath()
				if err != nil {
					t.Fatal(err)
				}
				return path
			},
			run: func(t *testing.T, cache *modelcache.Cache) string {
				if _, err := cache.ModelsResponse(t.Context(), http.Header{}); err != nil {
					return "error"
				}
				return "answered"
			},
			want: "answered",
		},
		"success: discovery cache": {
			path: func(t *testing.T, resolver paths.Resolver) string {
				path, err := resolver.DiscoveryCachePath()
				if err != nil {
					t.Fatal(err)
				}
				return path
			},
			run: func(t *testing.T, cache *modelcache.Cache) string {
				result, err := cache.SyncDiscoveryCache(false)
				if err != nil {
					return "error"
				}
				return result.State
			},
			want: "wrote",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cache, resolver := newFIFOCache(t)
			fifo := tt.path(t, resolver)
			mkfifo(t, fifo)
			var got string
			done := make(chan struct{})
			go func() {
				defer close(done)
				got = tt.run(t, cache)
			}()
			blocked, unblocked := awaitFIFOReader(done, fifo)
			switch {
			case !unblocked:
				t.Fatalf("reading %s blocked on a FIFO and could not be completed within %v", filepath.Base(fifo), fifoUnblockLimit)
			case blocked:
				t.Errorf("reading %s blocked on a FIFO for %v", filepath.Base(fifo), fifoReadWatchdog)
			case got != tt.want:
				t.Errorf("outcome with a FIFO at %s = %q, want %q", filepath.Base(fifo), got, tt.want)
			}
		})
	}
}

// TestBoundedReadSurvivesFIFOSwap swaps the Claude global config between a regular file and a FIFO
// while the cache keeps reading it, so a FIFO can appear between a type check and the open. No read
// may block, both targets must really have been installed, a FIFO must have been installed before the
// reads begin and again after the reader's first listing completed and before its last one started,
// and reads must have seen the regular file. The swap is a race the test provokes many times, so a
// pass is evidence, not proof.
func TestBoundedReadSurvivesFIFOSwap(t *testing.T) {
	const (
		// readFor is how long the reads run once a FIFO has been installed.
		readFor = 2 * time.Second
		// stallLimit is how long the reads may go without completing one listing before the test treats
		// a read as blocked. It runs on its own timer, reset by every completed listing.
		stallLimit = 5 * time.Second
		// previousID is the regular file's server row; a listing that carries it read the regular file.
		previousID = `"claude-opus-4"`
		// installLimit bounds the wait for the first FIFO install before the reads begin.
		installLimit              = 5 * time.Second
		regularTarget, fifoTarget = 0, 1
	)
	tests := map[string]struct {
		swapTargets [2]string
	}{
		"success: symlink swapped between a regular file and a FIFO": {
			swapTargets: [2]string{regularTarget: "regular.json", fifoTarget: "pipe"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, nil)
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				DefaultBaseURL:   discoveryGatewayURL,
				Client:           newFailingClient(),
				Clock:            time.Now,
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
				EffectiveBaseURL: baseURLFunc(discoveryGatewayURL),
				CurrentPins:      pinsFunc(map[string]string{"opus": "claude-opus-5-5[1m]"}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			configPath, err := resolver.GlobalConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			regular := filepath.Join(dir, tt.swapTargets[regularTarget])
			fifo := filepath.Join(dir, tt.swapTargets[fifoTarget])
			if err := os.WriteFile(regular, []byte(`{"additionalModelOptionsCache":[{"value":"claude-opus-4"}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			mkfifo(t, fifo)
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(regular, configPath); err != nil {
				t.Fatal(err)
			}
			if body, err := cache.ModelsResponse(t.Context(), http.Header{}); err != nil || !bytes.Contains(body, []byte(previousID)) {
				t.Fatalf("test precondition: a listing through the symlink to the regular file = (%s, %v), want the row %s", body, err, previousID)
			}

			stop := make(chan struct{})
			var stopOnce sync.Once
			halt := func() { stopOnce.Do(func() { close(stop) }) }
			defer halt()
			// installs counts, per target, the renames that put a symbolic link to it at the path.
			var installs [2]atomic.Int64
			fifoInstalled := make(chan struct{})
			var fifoOnce sync.Once
			var swapper sync.WaitGroup
			swapper.Go(func() {
				staged := configPath + ".swap"
				// The path starts as a link to the regular file, so the first install is the FIFO.
				for i := fifoTarget; ; i ^= 1 {
					select {
					case <-stop:
						return
					default:
					}
					_ = os.Remove(staged)
					if err := os.Symlink(filepath.Join(dir, tt.swapTargets[i]), staged); err != nil {
						continue
					}
					if err := os.Rename(staged, configPath); err == nil {
						installs[i].Add(1)
						if i == fifoTarget {
							fifoOnce.Do(func() { close(fifoInstalled) })
						}
					}
				}
			})
			select {
			case <-fifoInstalled:
			case <-time.After(installLimit):
				halt()
				swapper.Wait()
				t.Fatalf("test precondition: no FIFO was installed at the path within %v (%d regular-file installs)", installLimit, installs[regularTarget].Load())
			}

			var calls, failed, sawRegular atomic.Int64
			// progress receives a token after each completed listing; the watchdog below resets on it.
			progress := make(chan struct{}, 1)
			// fifoDuringReads counts the FIFO installs after the reader's first listing completed and
			// before its last listing started: only those overlap the reads. An install before the first
			// listing completed may have happened before any read ran.
			var fifoDuringReads int64
			var readers sync.WaitGroup
			readers.Go(func() {
				var afterFirst, beforeLast int64
				deadline := time.Now().Add(readFor)
				for time.Now().Before(deadline) {
					select {
					case <-stop:
						return
					default:
					}
					if calls.Load() > 0 {
						beforeLast = installs[fifoTarget].Load()
					}
					body, err := cache.ModelsResponse(t.Context(), http.Header{})
					switch {
					case err != nil:
						failed.Add(1)
					case bytes.Contains(body, []byte(previousID)):
						sawRegular.Add(1)
					}
					if calls.Add(1) == 1 {
						afterFirst = installs[fifoTarget].Load()
						beforeLast = afterFirst
					}
					select {
					case progress <- struct{}{}:
					default:
					}
				}
				fifoDuringReads = beforeLast - afterFirst
			})
			readersDone := make(chan struct{})
			go func() {
				readers.Wait()
				close(readersDone)
			}()

			watchdog := time.NewTimer(stallLimit)
			defer watchdog.Stop()
			stuck := false
		watch:
			for {
				select {
				case <-readersDone:
					break watch
				case <-progress:
					watchdog.Reset(stallLimit)
				case <-watchdog.C:
					// The reads may have ended, or a listing completed, while the timer fired: select picks
					// among ready cases at random. Only an interval with neither counts as a stall.
					select {
					case <-readersDone:
						break watch
					default:
					}
					select {
					case <-progress:
						watchdog.Reset(stallLimit)
						continue
					default:
					}
					stuck = true
					break watch
				}
			}
			halt()
			swapper.Wait()
			if stuck {
				if !unblockFIFOReaderUntil(readersDone, fifo) {
					t.Fatalf("no listing of the Claude global config completed for %v after %d listings, and the reads did not end within %v even with the FIFO's write side opened", stallLimit, calls.Load(), fifoUnblockLimit)
				}
				t.Errorf("no listing of the Claude global config completed for %v after %d listings", stallLimit, calls.Load())
			}
			regularInstalls, fifoInstalls := installs[regularTarget].Load(), installs[fifoTarget].Load()
			t.Logf("%d listings (%d saw the regular file, %d failed) while the path was swapped to the regular file %d times and to the FIFO %d times (%d after the first listing completed and before the last one started)",
				calls.Load(), sawRegular.Load(), failed.Load(), regularInstalls, fifoInstalls, fifoDuringReads)
			if regularInstalls == 0 || fifoInstalls == 0 {
				t.Errorf("test precondition: installs per target: regular file %d, FIFO %d; want both", regularInstalls, fifoInstalls)
			}
			switch {
			case stuck:
			case calls.Load() < 2:
				t.Errorf("test precondition: the reader completed %d listing(s), want at least two so that a FIFO install can fall between them", calls.Load())
			case fifoDuringReads == 0:
				t.Error("test precondition: no FIFO was installed after the first listing completed and before the last one started")
			}
			if sawRegular.Load() == 0 {
				t.Error("no listing saw the regular file while the path was swapped")
			}
			if n := failed.Load(); n != 0 {
				t.Errorf("%d listing(s) failed while the path was swapped, want none", n)
			}
		})
	}
}

// TestCatalogCacheFIFOIsAbsent puts a FIFO with no writer at the Anthropic catalog cache path, or a
// symbolic link to one there: a model listing and Wait return promptly, and the catalog counts as
// absent. A symbolic link to a regular catalog file is still read.
func TestCatalogCacheFIFOIsAbsent(t *testing.T) {
	const (
		cachedRow   = `{"id":"claude-sonnet-4-0","display_name":"Claude Sonnet 4"}`
		upstreamRow = `{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}`
	)
	tests := map[string]struct {
		// link is what the catalog path is a symbolic link to, "fifo" or "regular"; when empty the
		// FIFO is at the path itself.
		link        string
		credentials bool
		wantIDs     []string
		wantPages   int32
	}{
		"success: FIFO at the catalog path lists no catalog rows": {
			wantIDs: []string{},
		},
		"success: FIFO at the catalog path with credentials waits for a refresh of the absent catalog": {
			credentials: true,
			wantIDs:     []string{"claude-opus-5-5"},
			wantPages:   1,
		},
		"success: symbolic link to a FIFO lists no catalog rows": {
			link:    "fifo",
			wantIDs: []string{},
		},
		"success: symbolic link to a regular catalog file lists its rows": {
			link:        "regular",
			credentials: true,
			wantIDs:     []string{"claude-sonnet-4-0"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, nil)
			stateDir, err := resolver.StateDir()
			if err != nil {
				t.Fatalf("StateDir: %v", err)
			}
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(stateDir, "anthropic-models.json")
			dir := t.TempDir()
			fifo := path
			switch tt.link {
			case "":
				mkfifo(t, path)
			case "fifo":
				fifo = filepath.Join(dir, "pipe")
				mkfifo(t, fifo)
				if err := os.Symlink(fifo, path); err != nil {
					t.Fatal(err)
				}
			case "regular":
				target := filepath.Join(dir, "catalog.json")
				if err := anthropiccatalog.WriteSuccess(target, []byte("["+cachedRow+"]"), time.Now()); err != nil {
					t.Fatalf("write the regular catalog: %v", err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			var pages atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				pages.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[` + upstreamRow + `],"has_more":false}`))
			})
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: catalogTestBase,
				DefaultBaseURL:   discoveryGatewayURL,
				Client:           &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:            time.Now,
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			headers := http.Header{}
			if tt.credentials {
				headers.Set("X-Api-Key", apiKeyMarker)
			}

			var body []byte
			var listErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				body, listErr = cache.ModelsResponse(t.Context(), headers)
				cache.Wait()
			}()
			blocked, unblocked := awaitFIFOReader(done, fifo)
			switch {
			case !unblocked:
				t.Fatalf("the model listing blocked on the FIFO and could not be completed within %v", fifoUnblockLimit)
			case blocked:
				t.Fatalf("the model listing or Wait blocked on the FIFO for %v", fifoReadWatchdog)
			case listErr != nil:
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(listErr.Error()))
			}
			if diff := gocmp.Diff(tt.wantIDs, listedCatalogIDs(t, body)); diff != "" {
				t.Errorf("catalog rows of the model listing mismatch (-want +got):\n%s", diff)
			}
			if n := pages.Load(); n != tt.wantPages {
				t.Errorf("catalog pages fetched = %d, want %d", n, tt.wantPages)
			}
		})
	}
}

// listedCatalogIDs returns the IDs of a model listing other than the built-in Codex rows a cache
// lists before any proxy listing was read.
func listedCatalogIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var listing struct {
		Data []idDisplay `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatalf("unmarshal the model listing: %v", err)
	}
	ids := []string{}
	for _, row := range listing.Data {
		if !strings.HasPrefix(row.ID, "claude-gpt-") {
			ids = append(ids, row.ID)
		}
	}
	return ids
}
