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

package proxyreq_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
)

// TestDispatchNowPanicReleasesRouteLock checks that a panic in Options.Now during a dispatched request does not
// leave the handler's dispatch lock held and wedge later dispatched requests on the same handler.
func TestDispatchNowPanicReleasesRouteLock(t *testing.T) {
	const (
		path              = "/v1/messages"
		laterTurnWatchdog = 30 * time.Second
	)
	type injectedClockPanic string
	const clockPanicSentinel injectedClockPanic = "injected clock panic"
	headers := http.Header{"X-Claude-Code-Session-Id": {"s"}, "X-Claude-Code-Agent-Id": {"a"}}
	tests := map[string]struct {
		seed                 bool
		panicBody, laterBody string
		want                 turnOutcome
	}{
		"success: panic during route store": {
			panicBody: autoBody("[sidequest-route model=gpt-5.6-sol]"),
			laterBody: autoBody("[sidequest-route model=gpt-5.6-terra]"),
			want:      turnOutcome{Via: "dispatch", Model: "gpt-5.6-terra", Ticket: "<none>"},
		},
		"success: panic during route lookup": {
			seed:      true,
			panicBody: autoBody("follow up"),
			laterBody: autoBody("follow up"),
			want:      turnOutcome{Via: "dispatch-cached", Model: "gpt-5.6-sol", Ticket: "<none>"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var armed atomic.Bool
			h, err := proxyreq.New(proxyreq.Options{
				Now: func() time.Time {
					if armed.CompareAndSwap(true, false) {
						panic(clockPanicSentinel)
					}
					return time.UnixMilli(1234)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.seed {
				seedTurn := newPendingTurn(t, path, autoBody("[sidequest-route model=gpt-5.6-sol]"), headers)
				if got, want := outcomeOf(seedTurn.prepare(h)), (turnOutcome{Via: "dispatch", Model: "gpt-5.6-sol", Ticket: "<none>"}); got != want {
					t.Fatalf("seed briefing = %+v, want %+v", got, want)
				}
			}

			armed.Store(true)
			panicTurn := newPendingTurn(t, path, tc.panicBody, headers)
			recovered := func() (r any) {
				defer func() { r = recover() }()
				_, _ = panicTurn.prepare(h)
				return nil
			}()
			if recovered != clockPanicSentinel {
				t.Fatalf("Options.Now panic = %v (%T), want %v (%T)", recovered, recovered, clockPanicSentinel, clockPanicSentinel)
			}

			laterTurn := newPendingTurn(t, path, tc.laterBody, headers)
			type prepareResult struct {
				outcome  turnOutcome
				panicked any
			}
			done := make(chan prepareResult, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						done <- prepareResult{panicked: r}
					}
				}()
				done <- prepareResult{outcome: outcomeOf(laterTurn.prepare(h))}
			}()

			timer := time.NewTimer(laterTurnWatchdog)
			defer timer.Stop()
			select {
			case got := <-done:
				if got.panicked != nil {
					t.Fatalf("later dispatched request panicked: %v", got.panicked)
				}
				if diff := gocmp.Diff(tc.want, got.outcome); diff != "" {
					t.Errorf("later dispatched request (-want +got): %s", diff)
				}
			case <-timer.C:
				t.Fatalf("later dispatched request did not complete within %s", laterTurnWatchdog)
			}
		})
	}
}
