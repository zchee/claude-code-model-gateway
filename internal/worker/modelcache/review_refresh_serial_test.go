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
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// TestRefreshesNeverOverlapAcrossTicks holds the worker's first listing request across ten refresh
// intervals in virtual time. No second refresh starts while the first is held, so listing requests
// never overlap; once the held request is released, a later refresh does run, and it starts no
// earlier than the release. How soon after the release it runs is not part of the check.
func TestRefreshesNeverOverlapAcrossTicks(t *testing.T) {
	const (
		interval = 100 * time.Millisecond
		// hold stays below the listing request deadline, so the held request is not cut short.
		hold = 10 * interval
	)
	tests := map[string]struct {
		setting string
	}{
		"success: periodic ticks wait for a held refresh": {
			setting: "100",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if hold >= modelcache.ProxyListingTimeout {
				t.Fatalf("test precondition: hold %v must stay below the listing deadline %v", hold, modelcache.ProxyListingTimeout)
			}
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				var mu sync.Mutex
				var starts []time.Duration
				var active, maxActive int
				var start time.Time
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					starts = append(starts, time.Since(start))
					first := len(starts) == 1
					active++
					maxActive = max(maxActive, active)
					mu.Unlock()
					defer func() {
						mu.Lock()
						active--
						mu.Unlock()
					}()
					if first {
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
				})
				snapshot := func() ([]time.Duration, int) {
					mu.Lock()
					defer mu.Unlock()
					return slices.Clone(starts), maxActive
				}
				_, lookup, resolver := newTestPaths(t, map[string]string{"CODEX_GATEWAY_MODELS_REFRESH_MS": tt.setting})
				cache, err := modelcache.New(modelcache.Options{
					Lookup:         lookup,
					Paths:          resolver,
					ProxyBaseURL:   "http://127.0.0.1:19991",
					DefaultBaseURL: discoveryGatewayURL,
					Client:         &http.Client{Transport: handlerTransport{handler: handler}},
					Clock:          time.Now,
					Routes:         &route.Store{},
					Sentry:         &sentry.State{},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				var releaseOnce sync.Once
				defer func() {
					releaseOnce.Do(func() { close(release) })
					cancel()
					cache.Wait()
				}()
				start = time.Now()
				cache.Start(ctx)
				synctest.Wait()

				time.Sleep(hold)
				synctest.Wait()
				if got, _ := snapshot(); len(got) != 1 {
					t.Fatalf("listing requests started while the first refresh was held for %v = %v, want only the first", hold, got)
				}

				releaseOnce.Do(func() { close(release) })
				synctest.Wait()
				time.Sleep(interval)
				synctest.Wait()
				got, most := snapshot()
				if len(got) < 2 {
					t.Errorf("listing requests one interval after the release = %v, want a later refresh after the held one", got)
				}
				for i, at := range got[1:] {
					if at < hold {
						t.Errorf("listing request %d started at %v, before the release at %v", i+2, at, hold)
					}
				}
				if most != 1 {
					t.Errorf("concurrent listing requests peaked at %d, want 1", most)
				}
			})
		})
	}
}
