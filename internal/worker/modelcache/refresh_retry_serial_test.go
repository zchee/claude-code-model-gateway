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

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// TestDueRetryWaitsForHeldPeriodicRefresh comes due with a retry while a periodic refresh is held in
// its listing request, in virtual time. With a two-second interval, the first listing and the one-second
// retry after it come back empty, so the next retry is due at three seconds; the periodic refresh at
// two seconds is held until three and a half seconds. No listing request starts while it is held, so
// the retry never overlaps it, and the retry that came due runs once the held refresh returns, before
// the next periodic refresh at four seconds.
func TestDueRetryWaitsForHeldPeriodicRefresh(t *testing.T) {
	const (
		interval = 2 * time.Second
		// firstRetry restates modelcache.InitialRetryDelay; the retry after it waits twice as long.
		firstRetry  = time.Second
		secondRetry = 2 * firstRetry
		heldFrom    = interval
		releaseAt   = heldFrom + 1500*time.Millisecond
	)
	tests := map[string]struct {
		setting string
		// wantStarts are the listing request start times before the next periodic refresh.
		wantStarts []time.Duration
	}{
		"success: a retry due during a held periodic refresh runs after it": {
			setting:    "2000",
			wantStarts: []time.Duration{0, firstRetry, heldFrom, releaseAt},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			retryDue := firstRetry + secondRetry
			if retryDue <= heldFrom || retryDue >= releaseAt || releaseAt >= 2*interval {
				t.Fatalf("test precondition: the retry due at %v must fall inside the hold [%v, %v) and the release before the next tick at %v", retryDue, heldFrom, releaseAt, 2*interval)
			}
			if releaseAt-heldFrom >= modelcache.ProxyListingTimeout {
				t.Fatalf("test precondition: the hold %v must stay below the listing deadline %v", releaseAt-heldFrom, modelcache.ProxyListingTimeout)
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
					n := len(starts)
					active++
					maxActive = max(maxActive, active)
					mu.Unlock()
					defer func() {
						mu.Lock()
						active--
						mu.Unlock()
					}()
					w.Header().Set("Content-Type", "application/json")
					switch n {
					case 1, 2:
						_, _ = w.Write([]byte(`{"data":[]}`))
						return
					case 3:
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
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

				time.Sleep(heldFrom)
				synctest.Wait()
				if got, _ := snapshot(); !slices.Equal(got, tt.wantStarts[:3]) {
					t.Fatalf("test precondition: listing request starts by the periodic refresh = %v, want %v", got, tt.wantStarts[:3])
				}

				time.Sleep(releaseAt - heldFrom)
				synctest.Wait()
				if got, _ := snapshot(); len(got) != 3 {
					t.Errorf("listing request starts while the periodic refresh was held = %v, want none after %v", got, heldFrom)
				}

				releaseOnce.Do(func() { close(release) })
				synctest.Wait()
				time.Sleep(2*interval - releaseAt - time.Millisecond)
				synctest.Wait()
				got, most := snapshot()
				if diff := gocmp.Diff(tt.wantStarts, got); diff != "" {
					t.Errorf("listing request starts before the next periodic refresh mismatch (-want +got):\n%s", diff)
				}
				if most != 1 {
					t.Errorf("concurrent listing requests peaked at %d, want 1", most)
				}
			})
		})
	}
}
