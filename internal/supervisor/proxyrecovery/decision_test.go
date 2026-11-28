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

package proxyrecovery_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// answers makes the probe return the values in order; the last one repeats.
func (w *world) answers(values ...bool) {
	var n int
	w.set(func(b *behaviour) {
		b.probe = func(context.Context) bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			v := values[min(n, len(values)-1)]
			n++
			return v
		}
	})
}

// listens makes every listener sample answer v.
func (w *world) listens(v bool) {
	w.set(func(b *behaviour) { b.listening = func(context.Context) (bool, error) { return v, nil } })
}

// TestDecisionOrder decides the order of one tick (lib/process-supervision.js:1083-1104): a probe that answers ends
// it; a proxy this monitor started is left alone inside its grace; an unexpired retry time defers; a live listener is
// probed a second time before the failure is believed; only then is the attempt journaled. Each case prepares the
// monitor, forgets what the preparation called, and compares the calls of the one tick it observes.
func TestDecisionOrder(t *testing.T) {
	tests := map[string]struct {
		setup   func(w *world)
		prepare func(t *testing.T, m *proxyrecovery.Monitor, w *world)
		want    []string
		state   proxyrecovery.State
		// retryIn is the expected RetryAt relative to the observed tick's start time; zero expects no retry time.
		retryIn time.Duration
		// retryAt, when set, is the expected RetryAt relative to the test's start instead.
		retryAt time.Duration
		// anyRetry leaves RetryAt open: the source returns none for a proxy inside its grace and the contract does not
		// say.
		anyRetry bool
		pid      int
		journal  []event
	}{
		"success: a probe that answers ends the tick as healthy": {
			setup: func(w *world) { w.answers(true); w.listens(true) },
			want:  []string{"probe"},
			state: proxyrecovery.Healthy,
		},
		"success: a live listener that answers the second probe is healthy and nothing else is asked": {
			setup: func(w *world) {
				w.answers(false, true)
				w.listens(true)
				w.set(func(b *behaviour) {
					b.resolve = func(context.Context) (ownership.Owner, error) {
						w.t.Error("a healthy confirmation looked up the owner")
						return ownership.Owner{Class: ownership.Unknown}, nil
					}
				})
			},
			want:  []string{"probe", "listening", "probe"},
			state: proxyrecovery.Healthy,
		},
		"success: without a listener the failure is believed after one probe": {
			setup: func(w *world) {
				w.answers(false)
				w.listens(false)
				w.set(func(b *behaviour) { b.binary = func() bool { return false } })
			},
			want:    []string{"probe", "listening", "record:proxy-recovery-started", "binary", "record:proxy-recovery-finished"},
			state:   proxyrecovery.BinaryMissing,
			retryIn: time.Second,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.BinaryMissing)},
		},
		"success: a live listener that fails the second probe is checked again after the binary": {
			setup: func(w *world) { w.answers(false); w.listens(true) },
			want: []string{
				"probe", "listening", "probe", "record:proxy-recovery-started", "binary", "listening", "resolve",
				"record:proxy-recovery-finished",
			},
			state:   proxyrecovery.OwnerUnknown,
			retryIn: time.Second,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.OwnerUnknown)},
		},
		"success: an unexpired retry time defers after the probe": {
			setup: func(w *world) {
				w.answers(false)
				w.listens(true)
				w.set(func(b *behaviour) { b.binary = func() bool { return false } })
			},
			prepare: func(t *testing.T, m *proxyrecovery.Monitor, w *world) {
				tick(t, m)
				time.Sleep(time.Second - time.Nanosecond)
			},
			want:    []string{"probe"},
			state:   proxyrecovery.BackingOff,
			retryAt: time.Second,
		},
		"success: a probe that answers during the backoff is healthy": {
			setup: func(w *world) {
				w.answers(false, false, true)
				w.listens(true)
				w.set(func(b *behaviour) { b.binary = func() bool { return false } })
			},
			prepare: func(t *testing.T, m *proxyrecovery.Monitor, w *world) { tick(t, m) },
			want:    []string{"probe"},
			state:   proxyrecovery.Healthy,
		},
		"success: a proxy this monitor started is left alone inside its grace, also past its retry time": {
			setup: func(w *world) { w.answers(false); w.listens(false) },
			prepare: func(t *testing.T, m *proxyrecovery.Monitor, w *world) {
				tick(t, m)
				time.Sleep(5 * time.Second)
			},
			want:     []string{"probe"},
			state:    proxyrecovery.Starting,
			pid:      900,
			anyRetry: true,
		},
		"success: a proxy this monitor started is left alone inside its grace, before its retry time": {
			setup: func(w *world) { w.answers(false); w.listens(false) },
			prepare: func(t *testing.T, m *proxyrecovery.Monitor, w *world) {
				tick(t, m)
				// Strictly before the attempt's retry time, one second away: the grace decides, not the backoff.
				time.Sleep(500 * time.Millisecond)
			},
			want:     []string{"probe"},
			state:    proxyrecovery.Starting,
			pid:      900,
			anyRetry: true,
		},
		"success: a proxy inside its grace that answers is healthy": {
			setup: func(w *world) { w.answers(false, false, true); w.listens(false) },
			prepare: func(t *testing.T, m *proxyrecovery.Monitor, w *world) {
				tick(t, m)
				time.Sleep(5 * time.Second)
			},
			want:  []string{"probe"},
			state: proxyrecovery.Healthy,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				t0 := w.now()
				tt.setup(w)
				m := newMonitor(t, w.options(true))
				if tt.prepare != nil {
					tt.prepare(t, m, w)
				}
				w.sequence()
				w.journal()
				at := w.now()
				res := tick(t, m)
				if diff := gocmp.Diff(tt.want, w.sequence()); diff != "" {
					t.Errorf("calls of the tick (-want +got):\n%s", diff)
				}
				if res.State != tt.state {
					t.Errorf("state = %q, want %q", res.State, tt.state)
				}
				var wantRetry time.Time
				switch {
				case tt.retryAt != 0:
					wantRetry = t0.Add(tt.retryAt)
				case tt.retryIn != 0:
					wantRetry = at.Add(tt.retryIn)
				}
				if !tt.anyRetry && !res.RetryAt.Equal(wantRetry) {
					t.Errorf("RetryAt = %v, want %v", res.RetryAt, wantRetry)
				}
				if tt.pid != 0 && res.PID != tt.pid {
					t.Errorf("PID = %d, want %d", res.PID, tt.pid)
				}
				if diff := gocmp.Diff(tt.journal, w.journal(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("journal of the tick (-want +got):\n%s", diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestBackoffSchedule decides the retry schedule min(30 s, 1 s * 2^(n-1)) (lib/process-supervision.js:1050-1051,
// 1102-1104): each attempt is journaled with its number; its retry time is counted from the moment the attempt is
// made, after the probes of its tick, which here take time; until that retry time a tick only probes; at the retry
// time the next attempt begins.
func TestBackoffSchedule(t *testing.T) {
	const probeTook = 250 * time.Millisecond
	tests := map[string]struct {
		setup   func(w *world)
		outcome proxyrecovery.State
		// probes is the number of probes a tick makes before its attempt.
		probes int
	}{
		"success: a missing binary": {
			setup: func(w *world) {
				w.listens(false)
				w.set(func(b *behaviour) { b.binary = func() bool { return false } })
			},
			outcome: proxyrecovery.BinaryMissing,
			probes:  1,
		},
		"success: a holder that stays unknown": {
			setup:   func(w *world) { w.listens(true) },
			outcome: proxyrecovery.OwnerUnknown,
			probes:  2,
		},
	}
	schedule := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				tt.setup(w)
				m := newMonitor(t, w.options(true))
				slow := func(context.Context) bool { time.Sleep(probeTook); return false }
				fast := func(context.Context) bool { return false }
				for i, step := range schedule {
					attempt := i + 1
					w.set(func(b *behaviour) { b.probe = slow })
					at := w.now()
					res := tick(t, m)
					want := proxyrecovery.Result{State: tt.outcome, RetryAt: at.Add(time.Duration(tt.probes)*probeTook + step*time.Second)}
					if diff := gocmp.Diff(want, res); diff != "" {
						t.Fatalf("attempt %d at %v (-want +got):\n%s", attempt, at, diff)
					}
					if diff := gocmp.Diff([]event{attemptEvent(attempt), finishedEvent(tt.outcome)}, w.journal()); diff != "" {
						t.Errorf("journal of attempt %d (-want +got):\n%s", attempt, diff)
					}
					w.set(func(b *behaviour) { b.probe = fast })
					w.until(want.RetryAt.Add(-time.Nanosecond))
					w.sequence()
					deferred := tick(t, m)
					if diff := gocmp.Diff(proxyrecovery.Result{State: proxyrecovery.BackingOff, RetryAt: want.RetryAt}, deferred); diff != "" {
						t.Errorf("one nanosecond before attempt %d (-want +got):\n%s", attempt+1, diff)
					}
					if diff := gocmp.Diff([]string{"probe"}, w.sequence()); diff != "" {
						t.Errorf("calls one nanosecond before attempt %d (-want +got):\n%s", attempt+1, diff)
					}
					if got := w.journal(); len(got) != 0 {
						t.Errorf("a deferred tick journaled %v", got)
					}
					time.Sleep(time.Nanosecond)
				}
				if got := w.count("start") + w.count("stop-owned"); got != 0 {
					t.Errorf("the schedule started or stopped %d times", got)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestBackoffReset decides that each answer that ends a tick as healthy or recovered starts the schedule again: the
// retry time is cleared and the count restarts at one (lib/process-supervision.js:1087-1089, 1095-1099, 1188-1192).
// Three attempts accumulate first, the last one with a retry time four seconds away. A failure after the reset is
// then attempt one with a retry time one second away, not a deferred tick and not attempt four. A confirmation probe
// is asked only at or after the retry time, so after that reset only the count can be observed.
func TestBackoffReset(t *testing.T) {
	tests := map[string]struct {
		// answer runs the tick whose answer resets the schedule; last is the time of the third attempt.
		answer func(t *testing.T, m *proxyrecovery.Monitor, w *world, last time.Time)
		// started is the number of proxies started before the failure after the reset.
		started int
	}{
		"success: a first probe that answers inside the backoff": {
			answer: func(t *testing.T, m *proxyrecovery.Monitor, w *world, last time.Time) {
				w.until(last.Add(time.Second))
				w.answers(true)
				if diff := gocmp.Diff(proxyrecovery.Result{State: proxyrecovery.Healthy}, tick(t, m)); diff != "" {
					t.Errorf("the answering tick (-want +got):\n%s", diff)
				}
			},
		},
		"success: a confirmation probe that answers": {
			answer: func(t *testing.T, m *proxyrecovery.Monitor, w *world, last time.Time) {
				w.until(last.Add(4 * time.Second))
				w.answers(false, true, false)
				w.listens(true)
				w.sequence()
				if diff := gocmp.Diff(proxyrecovery.Result{State: proxyrecovery.Healthy}, tick(t, m)); diff != "" {
					t.Errorf("the answering tick (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]string{"probe", "listening", "probe"}, w.sequence()); diff != "" {
					t.Errorf("calls of the answering tick (-want +got):\n%s", diff)
				}
				w.listens(false)
			},
		},
		"success: a started proxy that answers": {
			answer: func(t *testing.T, m *proxyrecovery.Monitor, w *world, last time.Time) {
				w.until(last.Add(4 * time.Second))
				w.set(func(b *behaviour) { b.binary = func() bool { return true } })
				w.answers(false, true, false)
				if res := tick(t, m); res.State != proxyrecovery.Recovered {
					t.Errorf("the answering tick = %+v, want recovered", res)
				}
				// The started proxy exits, so that no grace hides the schedule from the next tick.
				w.exit(900)
				synctest.Wait()
			},
			started: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(false)
				w.set(func(b *behaviour) { b.binary = func() bool { return false } })
				m := newMonitor(t, w.options(true))
				var last time.Time
				for i, step := range []time.Duration{1, 2, 4} {
					last = w.now()
					res := tick(t, m)
					if !res.RetryAt.Equal(last.Add(step * time.Second)) {
						t.Fatalf("attempt %d = %+v, want a retry at %v", i+1, res, last.Add(step*time.Second))
					}
					if i < 2 {
						w.until(res.RetryAt)
					}
				}
				tt.answer(t, m, w, last)
				w.sequence()
				w.journal()
				w.answers(false)
				at := w.now()
				res := tick(t, m)
				if res.State == proxyrecovery.BackingOff {
					t.Errorf("the failure after the reset was deferred to %v: the retry time was kept", res.RetryAt)
				}
				if !res.RetryAt.Equal(at.Add(time.Second)) {
					t.Errorf("the failure after the reset = %+v, want a retry one second later, at %v", res, at.Add(time.Second))
				}
				var attempts []event
				for _, e := range w.journal() {
					if e.Name == "proxy-recovery-started" {
						attempts = append(attempts, e)
					}
				}
				if diff := gocmp.Diff([]event{attemptEvent(1)}, attempts); diff != "" {
					t.Errorf("attempt journaled after the reset (-want +got):\n%s", diff)
				}
				if n := w.count("start"); n > tt.started+1 {
					t.Errorf("%d proxies were started", n)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestSecondListenerSample decides that the sample taken after the attempt decides what the tick does with the port
// (lib/process-supervision.js:1095, 1116): a port that a listener took between the two samples is looked up and never
// started onto, and a port released between them is started onto without looking up or stopping the listener the
// first sample saw.
func TestSecondListenerSample(t *testing.T) {
	tests := map[string]struct {
		samples []bool
		owner   ownership.Owner
		want    []string
		state   proxyrecovery.State
		noRetry bool
		started []int
	}{
		"error: a port taken between the samples by a holder that cannot be established is not started onto": {
			samples: []bool{false, true},
			owner:   ownership.Owner{Class: ownership.Unknown, PID: 903},
			want:    []string{"probe", "listening", "record:proxy-recovery-started", "binary", "listening", "resolve", "record:proxy-recovery-finished"},
			state:   proxyrecovery.OwnerUnknown,
		},
		"error: a port taken between the samples by a proven foreign holder halts": {
			samples: []bool{false, true},
			owner:   ownership.Owner{Class: ownership.Foreign, PID: 903, Reason: "another install"},
			want:    []string{"probe", "listening", "record:proxy-recovery-started", "binary", "listening", "resolve", "record:proxy-recovery-finished"},
			state:   proxyrecovery.ForeignPortOwner,
			noRetry: true,
		},
		"success: a port released between the samples stops no stale owner": {
			samples: []bool{true, false},
			owner:   ownership.Owner{Class: ownership.Ours, PID: 4242},
			want: []string{
				"probe", "listening", "probe", "record:proxy-recovery-started", "binary", "listening", "start",
				"record:proxy-started", "probe", "record:proxy-recovery-finished",
			},
			state:   proxyrecovery.Starting,
			started: []int{900},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.owner(tt.owner, nil)
				w.place(proxyRecord(4242))
				var n int
				w.set(func(b *behaviour) {
					b.listening = func(context.Context) (bool, error) {
						w.mu.Lock()
						defer w.mu.Unlock()
						v := tt.samples[min(n, len(tt.samples)-1)]
						n++
						return v, nil
					}
				})
				m := newMonitor(t, w.options(true))
				at := w.now()
				res := tick(t, m)
				if diff := gocmp.Diff(tt.want, w.sequence()); diff != "" {
					t.Errorf("calls (-want +got):\n%s", diff)
				}
				retry := at.Add(time.Second)
				if tt.noRetry {
					retry = time.Time{}
				}
				if res.State != tt.state || !res.RetryAt.Equal(retry) {
					t.Errorf("result = %+v, want %q retrying at %v", res, tt.state, retry)
				}
				if diff := gocmp.Diff(tt.started, w.startedPIDs(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("proxies started (-want +got):\n%s", diff)
				}
				if n := w.count("stop-owned"); n != 0 {
					t.Errorf("StopOwned was called %d times", n)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestInjectedClock decides that grace and backoff are read from Options.Now and from nothing else: the injected
// clock moves forward while no time passes, and the monitor acts on it.
func TestInjectedClock(t *testing.T) {
	tests := map[string]struct {
		setup func(w *world)
		// jump is how far the injected clock moves after the first tick.
		jump time.Duration
		want []string
	}{
		"success: the grace ends on the injected clock": {
			setup: func(w *world) { w.listens(false) },
			jump:  30 * time.Second,
			want: []string{
				"probe", "listening", "record:proxy-recovery-started", "binary", "listening", "stop-owned:900",
				"wait-release", "start", "record:proxy-started", "probe", "record:proxy-recovery-finished",
			},
		},
		"success: the retry time comes on the injected clock": {
			setup: func(w *world) {
				w.listens(false)
				w.set(func(b *behaviour) { b.binary = func() bool { return false } })
			},
			jump: time.Second,
			want: []string{"probe", "listening", "record:proxy-recovery-started", "binary", "record:proxy-recovery-finished"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				tt.setup(w)
				m := newMonitor(t, w.options(true))
				tick(t, m)
				w.sequence()
				w.jump(tt.jump)
				tick(t, m)
				if diff := gocmp.Diff(tt.want, w.sequence()); diff != "" {
					t.Errorf("calls once the injected clock moved %v (-want +got):\n%s", tt.jump, diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}
