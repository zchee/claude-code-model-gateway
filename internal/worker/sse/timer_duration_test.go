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

package sse_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// nodeTimerMax is the largest delay in milliseconds a JavaScript timer accepts; the source's
// timers fire after one millisecond for anything above it, and drop fractions of a millisecond.
const nodeTimerMax = 1<<31 - 1

// TestRelayTimerDurations checks that the heartbeat interval and the first-event wait are Go
// durations taken as given: a fraction of a millisecond is kept, a duration beyond the JavaScript
// timer maximum is kept, and a negative first-event wait hands the stream back at once. The
// source's timers would fire one millisecond early, after one millisecond, and after one
// millisecond for these values. The timer is checked one nanosecond before its duration and at
// it, on the bubble's virtual clock.
func TestRelayTimerDurations(t *testing.T) {
	// literalHeartbeat restates sse.HeartbeatComment.
	const literalHeartbeat = ": ping\n\n"
	beyondNodeMax := time.Duration(nodeTimerMax+1)*time.Millisecond + 500*time.Microsecond

	heartbeatRelays := map[string]func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, interval time.Duration) (outcome.Result, error){
		"proxy relay": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, interval time.Duration) (outcome.Result, error) {
			return sse.RelayProxy(ctx, w, resp, sse.ProxyOptions{Provider: "codex", AdvertisedModel: "claude-gpt-5.6-sol", BackendModel: "gpt-5.6-sol", HeartbeatInterval: interval})
		},
		"proxy passthrough": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, interval time.Duration) (outcome.Result, error) {
			return sse.RelayPassthrough(ctx, w, resp, sse.PassthroughOptions{ProxyProvider: "codex", HeartbeatInterval: interval})
		},
	}

	tests := map[string]struct {
		relay string
		// heartbeat, when set, is the heartbeat interval under test; otherwise firstEventWait is
		// the LM Studio first-event wait under test, with heartbeats disabled.
		heartbeat      time.Duration
		firstEventWait time.Duration
		// fireAt is when the timer must fire: the first heartbeat, or the first-event wait's hand-back
		// that commits the 200 head.
		fireAt time.Duration
	}{
		"success: proxy relay keeps the sub-millisecond part of the heartbeat interval": {
			relay: "proxy relay", heartbeat: 1500 * time.Microsecond, fireAt: 1500 * time.Microsecond,
		},
		"success: proxy relay keeps a heartbeat interval beyond the JavaScript timer maximum": {
			relay: "proxy relay", heartbeat: beyondNodeMax, fireAt: beyondNodeMax,
		},
		"success: proxy passthrough keeps the sub-millisecond part of the heartbeat interval": {
			relay: "proxy passthrough", heartbeat: 1500 * time.Microsecond, fireAt: 1500 * time.Microsecond,
		},
		"success: proxy passthrough keeps a heartbeat interval beyond the JavaScript timer maximum": {
			relay: "proxy passthrough", heartbeat: beyondNodeMax, fireAt: beyondNodeMax,
		},
		"success: LM Studio relay keeps the sub-millisecond part of the first-event wait": {
			relay: "LM Studio proxy relay", firstEventWait: 2500 * time.Microsecond, fireAt: 2500 * time.Microsecond,
		},
		"success: LM Studio relay keeps a first-event wait beyond the JavaScript timer maximum": {
			relay: "LM Studio proxy relay", firstEventWait: beyondNodeMax, fireAt: beyondNodeMax,
		},
		"success: LM Studio relay hands the stream back at once for a negative first-event wait": {
			relay: "LM Studio proxy relay", firstEventWait: -time.Millisecond, fireAt: 0,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				src := newHeldBody()
				rec := newStreamingRecorder()
				run := func(ctx context.Context) (outcome.Result, error) {
					if tt.relay == "LM Studio proxy relay" {
						return sse.RelayProxy(ctx, rec, sseResponse(src, nil), sse.ProxyOptions{
							Provider:          sse.LMStudioProvider,
							AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
							BackendModel:      "lmstudio/qwen3-coder-30b",
							HeartbeatInterval: -1,
							FirstEventWait:    tt.firstEventWait,
							OnOverflow:        testEngineOverflowResponder,
						})
					}
					return heartbeatRelays[tt.relay](ctx, rec, sseResponse(src, nil), tt.heartbeat)
				}
				type relayOut struct {
					res outcome.Result
					err error
				}
				done := make(chan relayOut, 1)
				go func() {
					res, err := run(t.Context())
					done <- relayOut{res: res, err: err}
				}()
				joined := false
				defer func() {
					if !joined {
						_ = src.Close()
						<-done
					}
				}()

				// fired reports whether the timer under test has fired by now.
				fired := func() bool {
					_, wroteHeader, body, _, _ := rec.snapshot()
					if tt.heartbeat != 0 {
						return strings.Contains(body, literalHeartbeat)
					}
					return wroteHeader
				}
				start := time.Now()
				synctest.Wait()
				if tt.fireAt > 0 {
					time.Sleep(tt.fireAt - time.Nanosecond)
					synctest.Wait()
					if fired() {
						t.Fatalf("timer fired by %v, before its duration %v", time.Since(start), tt.fireAt)
					}
					time.Sleep(time.Nanosecond)
					synctest.Wait()
				}
				if !fired() {
					t.Fatalf("timer had not fired at %v, its duration %v", time.Since(start), tt.fireAt)
				}
				if tt.heartbeat != 0 {
					_, _, body, _, _ := rec.snapshot()
					if got := strings.Count(body, literalHeartbeat); got != 1 {
						t.Errorf("heartbeats at %v = %d, want 1; body=%q", tt.fireAt, got, body)
					}
				} else if status, _, _, _, _ := rec.snapshot(); status != http.StatusOK {
					t.Errorf("head committed at the hand-back = %d, want 200", status)
				}

				src.release("", io.EOF)
				synctest.Wait()
				select {
				case out := <-done:
					joined = true
					if out.err != nil {
						t.Errorf("relay error = %v, want nil", out.err)
					}
					if out.res.Status != http.StatusOK {
						t.Errorf("relay result status = %d, want 200", out.res.Status)
					}
				default:
					t.Fatal("relay did not return after the upstream's clean end")
				}
				if got := src.closes.Load(); got != 1 {
					t.Errorf("owned resp.Body close count = %d, want 1", got)
				}
			})
		})
	}
	if diff := gocmp.Diff(literalHeartbeat, sse.HeartbeatComment); diff != "" {
		t.Errorf("sse.HeartbeatComment (-restated +got):\n%s", diff)
	}
}
