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
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// largestTimerDelay is the largest delay a Node timer keeps, 2^31-1 ms.
const largestTimerDelay = (1<<31 - 1) * time.Millisecond

// TestRefreshIntervalSpellings counts periodic refreshes of a healthy proxy in virtual time. A
// fractional interval is truncated to whole milliseconds, a value below one millisecond runs every
// millisecond, and a value above the largest timer delay, including +Infinity, runs at that delay.
func TestRefreshIntervalSpellings(t *testing.T) {
	tests := map[string]struct {
		setting string
		want    time.Duration
	}{
		"success: fractional interval truncated to whole milliseconds": {
			setting: "1.9",
			want:    time.Millisecond,
		},
		"success: interval below one millisecond runs every millisecond": {
			setting: "0.5",
			want:    time.Millisecond,
		},
		"success: interval one millisecond above the largest timer delay saturates": {
			setting: "2147483648",
			want:    largestTimerDelay,
		},
		"success: positive infinity saturates at the largest timer delay": {
			setting: "Infinity",
			want:    largestTimerDelay,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				var hits []time.Time
				handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					hits = append(hits, time.Now())
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
				})
				_, lookup, resolver := newTestPaths(t, map[string]string{"CODEX_GATEWAY_MODELS_REFRESH_MS": tt.setting})
				cache, err := modelcache.New(modelcache.Options{
					Lookup:         lookup,
					Paths:          resolver,
					ProxyBaseURL:   "http://127.0.0.1:19991",
					DefaultBaseURL: "http://127.0.0.1:19999",
					Client:         &http.Client{Transport: handlerTransport{handler: handler}},
					Clock:          time.Now,
					Routes:         &route.Store{},
					Sentry:         &sentry.State{},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				offsets := func(start time.Time) []time.Duration {
					mu.Lock()
					defer mu.Unlock()
					out := make([]time.Duration, 0, len(hits))
					for _, h := range hits {
						out = append(out, h.Sub(start))
					}
					return out
				}

				ctx, cancel := context.WithCancel(t.Context())
				defer func() {
					cancel()
					cache.Wait()
				}()
				start := time.Now()
				cache.Start(ctx)
				synctest.Wait()

				// A short first window stops a wrong millisecond loop before it runs for the whole interval.
				first := min(tt.want-time.Nanosecond, 10*time.Millisecond)
				for _, window := range []time.Duration{first, tt.want - time.Nanosecond - first} {
					time.Sleep(window)
					synctest.Wait()
					if diff := gocmp.Diff([]time.Duration{0}, offsets(start)); diff != "" {
						t.Fatalf("refreshes before one interval of %v mismatch (-want +got):\n%s", tt.want, diff)
					}
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				time.Sleep(tt.want)
				synctest.Wait()
				want := []time.Duration{0, tt.want, 2 * tt.want}
				if diff := gocmp.Diff(want, offsets(start)); diff != "" {
					t.Errorf("refresh offsets over two intervals of %v mismatch (-want +got):\n%s", tt.want, diff)
				}
			})
		})
	}
}
