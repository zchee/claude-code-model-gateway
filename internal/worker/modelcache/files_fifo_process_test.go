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
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// fifoReadWatchdog is how long one cache operation may take before the test treats it as blocked on
// a FIFO.
const fifoReadWatchdog = 5 * time.Second

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
			done := make(chan string, 1)
			go func() { done <- tt.run(t, cache) }()
			select {
			case got := <-done:
				if got != tt.want {
					t.Errorf("outcome with a FIFO at %s = %q, want %q", filepath.Base(fifo), got, tt.want)
				}
			case <-time.After(fifoReadWatchdog):
				unblockFIFOReader(fifo)
				<-done
				t.Errorf("reading %s blocked on a FIFO for %v", filepath.Base(fifo), fifoReadWatchdog)
			}
		})
	}
}

// TestBoundedReadSurvivesFIFOSwap swaps the Claude global config between a regular file and a FIFO
// while the cache keeps reading it, so a FIFO can appear between a type check and the open. No read
// may block. The swap is a race the test provokes many times, so a pass is evidence, not proof.
func TestBoundedReadSurvivesFIFOSwap(t *testing.T) {
	const (
		readFor    = 2 * time.Second
		stuckAfter = 2 * time.Second
	)
	tests := map[string]struct {
		swapTargets []string
	}{
		"success: symlink swapped between a regular file and a FIFO": {
			swapTargets: []string{"regular.json", "pipe"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cache, resolver := newFIFOCache(t)
			configPath, err := resolver.GlobalConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			regular := filepath.Join(dir, tt.swapTargets[0])
			fifo := filepath.Join(dir, tt.swapTargets[1])
			if err := os.WriteFile(regular, []byte(`{"additionalModelOptionsCache":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			mkfifo(t, fifo)
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(regular, configPath); err != nil {
				t.Fatal(err)
			}

			stop := make(chan struct{})
			var stopOnce sync.Once
			halt := func() { stopOnce.Do(func() { close(stop) }) }
			defer halt()
			var swapper sync.WaitGroup
			swapper.Go(func() {
				staged := configPath + ".swap"
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					_ = os.Remove(staged)
					if err := os.Symlink(filepath.Join(dir, tt.swapTargets[i%2]), staged); err != nil {
						continue
					}
					_ = os.Rename(staged, configPath)
				}
			})

			var reads atomic.Int64
			var readers sync.WaitGroup
			readers.Go(func() {
				deadline := time.Now().Add(readFor)
				for time.Now().Before(deadline) {
					select {
					case <-stop:
						return
					default:
					}
					_, _ = cache.ModelsResponse(t.Context(), http.Header{})
					reads.Add(1)
				}
			})
			readersDone := make(chan struct{})
			go func() {
				readers.Wait()
				close(readersDone)
			}()

			last, lastChange := reads.Load(), time.Now()
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			stuck := false
		watch:
			for {
				select {
				case <-readersDone:
					break watch
				case <-ticker.C:
					if n := reads.Load(); n != last {
						last, lastChange = n, time.Now()
					} else if time.Since(lastChange) >= stuckAfter {
						stuck = true
						break watch
					}
				}
			}
			halt()
			swapper.Wait()
			if stuck {
				for {
					unblockFIFOReader(fifo)
					select {
					case <-readersDone:
					case <-time.After(10 * time.Millisecond):
						continue
					}
					break
				}
				t.Errorf("a read of the Claude global config blocked on a FIFO swapped in after %d reads", last)
			}
			<-readersDone
			t.Logf("%d reads while the path was being swapped", reads.Load())
		})
	}
}
