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

package readiness_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

// TestSharedCheckDiscardsResultAfterShutdown runs a second shared check that is still in flight when
// the worker shuts down. Whether the check returns because it saw the cancellation or returns a
// healthy result after it, Latest after Wait still serves the first result with its original check
// time, and no further check starts once the scheduler has run every goroutine that could start one.
func TestSharedCheckDiscardsResultAfterShutdown(t *testing.T) {
	const firstCheckMs = 1000
	tests := map[string]struct {
		// ignoresCancellation makes the second check finish only when released after shutdown, with a
		// result that would look healthy if it were published.
		ignoresCancellation bool
	}{
		"success: check that returns on cancellation is discarded": {},
		"success: check that returns a healthy result after shutdown is discarded": {
			ignoresCancellation: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mu sync.Mutex
				nowMs := int64(firstCheckMs)
				clock := func() time.Time {
					mu.Lock()
					defer mu.Unlock()
					return time.UnixMilli(nowMs)
				}
				workerCtx, workerCancel := context.WithCancel(t.Context())
				release := make(chan struct{})
				var releaseOnce sync.Once
				releaseSecond := func() { releaseOnce.Do(func() { close(release) }) }

				first := readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: false, CodexAuth: true}
				var calls atomic.Int32
				check := readiness.NewSharedCheck(workerCtx, func(runCtx context.Context) readiness.ProxyAuthResult {
					switch calls.Add(1) {
					case 1:
						return first
					case 2:
						if tt.ignoresCancellation {
							<-release
						} else {
							<-runCtx.Done()
						}
						return readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: true}
					default:
						return readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: true}
					}
				}, clock)
				defer func() {
					workerCancel()
					releaseSecond()
					check.Wait()
				}()

				_ = check.Latest()
				synctest.Wait()
				check.Wait()
				want := first
				want.CheckedAt = time.UnixMilli(firstCheckMs)
				if diff := gocmp.Diff(&want, check.Latest()); diff != "" {
					t.Fatalf("test precondition: first published result mismatch (-want +got):\n%s", diff)
				}

				mu.Lock()
				nowMs += readiness.FreshnessTTL.Milliseconds()
				mu.Unlock()
				_ = check.Latest()
				synctest.Wait()
				if got := calls.Load(); got != 2 {
					t.Fatalf("test precondition: checks started after the result went stale = %d, want 2", got)
				}

				workerCancel()
				if tt.ignoresCancellation {
					synctest.Wait()
					releaseSecond()
				}
				check.Wait()

				for i := range 3 {
					if diff := gocmp.Diff(&want, check.Latest()); diff != "" {
						t.Errorf("Latest call %d after shutdown and Wait mismatch (-want +got):\n%s", i+1, diff)
					}
					synctest.Wait()
				}
				if got := calls.Load(); got != 2 {
					t.Errorf("checks started = %d after shutdown, want 2 (none after shutdown)", got)
				}
			})
		})
	}
}
