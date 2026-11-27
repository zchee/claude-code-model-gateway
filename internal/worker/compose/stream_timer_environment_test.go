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

package compose_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// timerEnvWrite is one body write toward the client and the bubble's clock when it happened.
type timerEnvWrite struct {
	at   time.Time
	data string
}

// timerEnvWriter records when the response head and each body write reached the client.
type timerEnvWriter struct {
	mu     sync.Mutex
	header http.Header
	headAt time.Time
	code   int
	writes []timerEnvWrite
}

func (w *timerEnvWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.header
}

func (w *timerEnvWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code, w.headAt = code, time.Now()
	}
}

func (w *timerEnvWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code, w.headAt = http.StatusOK, time.Now()
	}
	w.writes = append(w.writes, timerEnvWrite{at: time.Now(), data: string(p)})
	return len(p), nil
}

func (w *timerEnvWriter) Flush() {}

func (w *timerEnvWriter) snapshot() (int, time.Time, []timerEnvWrite) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.code, w.headAt, slices.Clone(w.writes)
}

// TestStreamTimerEnvironmentConversion decides how the composed worker converts the
// CODEX_GATEWAY_SSE_HEARTBEAT_S and CODEX_GATEWAY_LMSTUDIO_FIRST_EVENT_WAIT_MS values it reads from
// its environment, as the worker command passes them: millisecond delays are normalized as Node
// timers are (a fraction is dropped, below one millisecond or above 2^31-1 milliseconds fires after
// one millisecond), a zero heartbeat disables the heartbeat, and a negative or unparsable value
// selects the configured default (20 s heartbeat, 10 s first-event wait). Every expectation differs
// from what the same number given directly as a relay option would do.
//
// An LM Studio stream whose provider sends its head and then nothing is relayed on the bubble's
// clock: the client's head is written when the first-event wait ends, and the first heartbeat
// follows one heartbeat interval later.
func TestStreamTimerEnvironmentConversion(t *testing.T) {
	tests := map[string]struct {
		heartbeat, firstWait string
		wantFirstWait        time.Duration
		// wantHeartbeat is zero when no heartbeat may be written at all.
		wantHeartbeat time.Duration
	}{
		"success: zero disables the heartbeat and makes the first-event wait one millisecond": {
			heartbeat: "0", firstWait: "0",
			wantFirstWait: time.Millisecond,
		},
		"success: negative values select the defaults": {
			heartbeat: "-1", firstWait: "-1",
			wantFirstWait: 10 * time.Second, wantHeartbeat: 20 * time.Second,
		},
		"success: fractions of a millisecond are dropped": {
			heartbeat: "1.0005", firstWait: "2.5",
			wantFirstWait: 2 * time.Millisecond, wantHeartbeat: time.Second,
		},
		"success: delays below one millisecond fire after one millisecond": {
			heartbeat: "0.0004", firstWait: "0.5",
			wantFirstWait: time.Millisecond, wantHeartbeat: time.Millisecond,
		},
		"success: delays beyond the JavaScript timer limit fire after one millisecond": {
			heartbeat: "3000000", firstWait: "3000000000",
			wantFirstWait: time.Millisecond, wantHeartbeat: time.Millisecond,
		},
		"success: unparsable values select the defaults": {
			heartbeat: "soon", firstWait: "soon",
			wantFirstWait: 10 * time.Second, wantHeartbeat: 20 * time.Second,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				releaseOnce := sync.OnceFunc(func() { close(release) })
				var providerHead atomic.Pointer[time.Time]
				h := newHarness(t, harnessConfig{
					inMemory: true, disableTrace: true, disableUsage: true,
					env: map[string]string{
						"CODEX_GATEWAY_SSE_HEARTBEAT_S":              tt.heartbeat,
						"CODEX_GATEWAY_LMSTUDIO_FIRST_EVENT_WAIT_MS": tt.firstWait,
					},
					proxyHandler: func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
						providerHead.Store(new(time.Now()))
						select {
						case <-release:
						case <-r.Context().Done():
						}
					},
				})
				t.Cleanup(releaseOnce)

				ctx, cancel := context.WithCancel(t.Context())
				w := &timerEnvWriter{header: http.Header{}}
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer func() {
						if p := recover(); p != nil {
							if err, ok := p.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
								panic(p)
							}
						}
					}()
					req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(messageRequestJSON("claude-lmstudio/ibm/granite-4-micro", true)))
					h.workerHandler.ServeHTTP(w, req)
				}()
				t.Cleanup(func() { cancel(); h.cancelService(); releaseOnce(); <-done })

				synctest.Wait()
				mustDiff(t, "provider sends", int64(1), h.proxyHits.Load())
				providerHeadAt := providerHead.Load()
				if providerHeadAt == nil {
					t.Fatal("the provider did not send its head")
				}
				// Past the expected moments; a disabled heartbeat is watched past the 20 s default.
				horizon := tt.wantFirstWait + tt.wantHeartbeat + time.Millisecond
				if tt.wantHeartbeat == 0 {
					horizon = tt.wantFirstWait + 20*time.Second + time.Second
				}
				time.Sleep(horizon)
				synctest.Wait()
				assertRunning(t, "the relayed stream", done)

				code, headAt, writes := w.snapshot()
				mustDiff(t, "client status", http.StatusOK, code)
				mustDiff(t, "first-event wait before the client's head", tt.wantFirstWait, headAt.Sub(*providerHeadAt))
				if tt.wantHeartbeat == 0 {
					mustDiff(t, "writes with the heartbeat disabled", 0, len(writes))
					return
				}
				if len(writes) == 0 {
					t.Fatalf("no heartbeat within %v of the provider's head", horizon)
				}
				mustDiff(t, "first write", ": ping\n\n", writes[0].data)
				mustDiff(t, "heartbeat interval after the client's head", tt.wantHeartbeat, writes[0].at.Sub(headAt))
			})
		})
	}
}
