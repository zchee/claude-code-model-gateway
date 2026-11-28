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
	"errors"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// TestRecoverySequence decides the whole tick that replaces a proxy (lib/process-supervision.js:1100-1196): the
// attempt is journaled before the binary is checked; the port is sampled again before its owner is looked up; a holder
// is stopped only on this root's record, after the stop is journaled; its release is confirmed before the start; the
// start is journaled and the new proxy probed. Each case compares the exact calls and the journal of one tick.
func TestRecoverySequence(t *testing.T) {
	tests := map[string]struct {
		setup func(w *world)
		// before is the number of ticks run before the observed one; each must be healthy, and their calls are forgotten.
		before  int
		want    []string
		state   proxyrecovery.State
		retryIn time.Duration
		journal []event
		stopped []procinfo.Record
		started []int
	}{
		"success: an unhealthy holder this root's record names is stopped, its release confirmed, then replaced": {
			setup: func(w *world) {
				w.answers(false, false, true)
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
			},
			want: []string{
				"probe", "listening", "probe", "record:proxy-recovery-started", "binary", "listening", "resolve",
				"recorded:4242", "record:proxy-stop-requested", "stop-owned:4242", "wait-release", "start",
				"record:proxy-started", "probe", "record:proxy-recovery-finished",
			},
			state:   proxyrecovery.Recovered,
			journal: []event{attemptEvent(1), stopRequestedEvent(4242), startedEvent(900), finishedEvent(proxyrecovery.Recovered)},
			stopped: []procinfo.Record{proxyRecord(4242)},
			started: []int{900},
		},
		"success: a proxy that died after it was healthy is started again without a stop": {
			setup:  func(w *world) { w.answers(true, false, true); w.listens(false) },
			before: 1,
			want: []string{
				"probe", "listening", "record:proxy-recovery-started", "binary", "listening", "start",
				"record:proxy-started", "probe", "record:proxy-recovery-finished",
			},
			state:   proxyrecovery.Recovered,
			journal: []event{attemptEvent(1), startedEvent(900), finishedEvent(proxyrecovery.Recovered)},
			started: []int{900},
		},
		"success: a started proxy that does not answer yet is starting until its retry time": {
			setup: func(w *world) { w.answers(false); w.listens(false) },
			want: []string{
				"probe", "listening", "record:proxy-recovery-started", "binary", "listening", "start",
				"record:proxy-started", "probe", "record:proxy-recovery-finished",
			},
			state:   proxyrecovery.Starting,
			retryIn: time.Second,
			journal: []event{attemptEvent(1), startedEvent(900), finishedEvent(proxyrecovery.Starting)},
			started: []int{900},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				tt.setup(w)
				m := newMonitor(t, w.options(true))
				for range tt.before {
					if res := tick(t, m); res.State != proxyrecovery.Healthy {
						t.Errorf("tick before the observed one = %+v, want healthy", res)
					}
				}
				w.sequence()
				w.journal()
				at := w.now()
				res := tick(t, m)
				if diff := gocmp.Diff(tt.want, w.sequence()); diff != "" {
					t.Errorf("calls of the tick (-want +got):\n%s", diff)
				}
				var retry time.Time
				if tt.retryIn != 0 {
					retry = at.Add(tt.retryIn)
				}
				if res.State != tt.state || !res.RetryAt.Equal(retry) {
					t.Errorf("result = %+v, want state %q retrying at %v", res, tt.state, retry)
				}
				if diff := gocmp.Diff(tt.journal, w.journal(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("journal (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.stopped, w.stops(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("records stopped (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.started, w.startedPIDs(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("proxies started (-want +got):\n%s", diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestStopBeforeStart decides "stop, confirm the release, and only then start" by holding each step open: while the
// stop of the holder runs nothing else is asked, and while the release check runs no proxy is started.
func TestStopBeforeStart(t *testing.T) {
	tests := map[string]struct {
		hold string
		// after lists the calls that must not have been made while the held call runs.
		after []string
	}{
		"success: nothing follows the stop of the holder before it returns": {hold: "stop", after: []string{"wait-release", "start"}},
		"success: no proxy is started before the release is confirmed":      {hold: "release", after: []string{"start"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
				g := newGate()
				t.Cleanup(g.open)
				w.set(func(b *behaviour) {
					switch tt.hold {
					case "stop":
						b.stopOwned = func(context.Context, procinfo.Record) error { return g.hold(t.Context()) }
					case "release":
						b.waitRelease = func(context.Context) bool { return g.hold(t.Context()) == nil }
					}
				})
				m := newMonitor(t, w.options(true))
				p := goRecover(m, t.Context())
				reached(t, g, tt.hold)
				synctest.Wait()
				for _, call := range tt.after {
					if n := w.count(call); n != 0 {
						t.Errorf("%s was called %d times while the %s was held", call, n, tt.hold)
					}
				}
				g.open()
				if _, err := p.result(t, "Recover"); err != nil {
					t.Fatalf("Recover: %v", err)
				}
				if diff := gocmp.Diff([]int{900}, w.startedPIDs()); diff != "" {
					t.Errorf("proxies started once the step returned (-want +got):\n%s", diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestOutcomes decides each refusal outcome of a tick with a live, unhealthy listener: its state and retry time, its
// journal (lib/process-supervision.js:1111-1158), and that nothing is stopped or started after it.
func TestOutcomes(t *testing.T) {
	lookupBudget := errors.New("the ownership lookup ran out of budget")
	tests := map[string]struct {
		setup   func(w *world)
		state   proxyrecovery.State
		noRetry bool
		journal []event
		// stopped is the number of StopOwned calls the outcome makes; every other case makes none.
		stopped int
		// samples, when set, is the number of listener samples the tick takes.
		samples int
		// absent lists calls the tick must not make.
		absent []string
	}{
		"success: a missing binary refuses before the port is sampled again or its owner looked up": {
			setup:   func(w *world) { w.set(func(b *behaviour) { b.binary = func() bool { return false } }) },
			state:   proxyrecovery.BinaryMissing,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.BinaryMissing)},
			absent:  []string{"resolve", "recorded"},
			samples: 1,
		},
		"success: a holder the lookup could not establish is owner-unknown": {
			setup:   func(w *world) { w.owner(ownership.Owner{Class: ownership.Unknown, PID: 903}, nil) },
			state:   proxyrecovery.OwnerUnknown,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.OwnerUnknown)},
			absent:  []string{"recorded"},
		},
		"success: a lookup that timed out is owner-unknown": {
			setup:   func(w *world) { w.owner(ownership.Owner{Class: ownership.Unknown}, context.DeadlineExceeded) },
			state:   proxyrecovery.OwnerUnknown,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.OwnerUnknown)},
			absent:  []string{"recorded"},
		},
		"success: a lookup that failed is owner-unknown whatever class it reports beside the error": {
			setup: func(w *world) {
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, lookupBudget)
				w.place(proxyRecord(4242))
			},
			state:   proxyrecovery.OwnerUnknown,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.OwnerUnknown)},
		},
		"success: a holder whose record cannot be read is owner-unknown": {
			setup:   func(w *world) { w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil) },
			state:   proxyrecovery.OwnerUnknown,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.OwnerUnknown)},
		},
		"success: a record read that names another process is owner-unknown": {
			setup: func(w *world) {
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.set(func(b *behaviour) {
					b.recorded = func(context.Context, int) (procinfo.Record, error) { return proxyRecord(4243), nil }
				})
			},
			state:   proxyrecovery.OwnerUnknown,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.OwnerUnknown)},
		},
		"success: a bound port whose holder cannot be resolved is port-owned": {
			setup:   func(w *world) { w.owner(ownership.Owner{Class: ownership.PortOwned}, nil) },
			state:   proxyrecovery.PortOwned,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.PortOwned)},
			absent:  []string{"recorded"},
		},
		"success: a stopped holder that keeps the port is port-stuck": {
			setup: func(w *world) {
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
				w.set(func(b *behaviour) { b.waitRelease = func(context.Context) bool { return false } })
			},
			state:   proxyrecovery.PortStuck,
			journal: []event{attemptEvent(1), stopRequestedEvent(4242), finishedEvent(proxyrecovery.PortStuck)},
			stopped: 1,
		},
		"success: a proven foreign holder is foreign-port-owner without a retry time": {
			setup: func(w *world) {
				w.owner(ownership.Owner{Class: ownership.Foreign, PID: 4242, Reason: "another install"}, nil)
			},
			state:   proxyrecovery.ForeignPortOwner,
			noRetry: true,
			journal: []event{attemptEvent(1), finishedEvent(proxyrecovery.ForeignPortOwner)},
			absent:  []string{"recorded"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(true)
				tt.setup(w)
				m := newMonitor(t, w.options(true))
				at := w.now()
				res := tick(t, m)
				retry := at.Add(time.Second)
				if tt.noRetry {
					retry = time.Time{}
				}
				if res.State != tt.state || !res.RetryAt.Equal(retry) {
					t.Errorf("result = %+v, want state %q retrying at %v", res, tt.state, retry)
				}
				if diff := gocmp.Diff(tt.journal, w.journal()); diff != "" {
					t.Errorf("journal (-want +got):\n%s", diff)
				}
				if n := w.count("stop-owned"); n != tt.stopped {
					t.Errorf("StopOwned was called %d times, want %d", n, tt.stopped)
				}
				if n := w.count("listening"); tt.samples != 0 && n != tt.samples {
					t.Errorf("the listener was sampled %d times, want %d", n, tt.samples)
				}
				for _, call := range append([]string{"start"}, tt.absent...) {
					if n := w.count(call); n != 0 {
						t.Errorf("%s was called %d times", call, n)
					}
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestForeignOwnerHaltsForGood decides that a proven foreign holder halts recovery for the monitor's lifetime
// (lib/process-supervision.js:1085, 1139-1145): every later tick returns foreign-port-owner and asks, journals,
// starts and stops nothing, also after any time and when the proxy would answer.
func TestForeignOwnerHaltsForGood(t *testing.T) {
	tests := map[string]struct {
		later func(w *world)
	}{
		"success: the next tick":                          {},
		"success: a tick an hour later":                   {later: func(*world) { time.Sleep(time.Hour) }},
		"success: a tick when the proxy would answer now": {later: func(w *world) { w.answers(true) }},
		"success: a tick when the port is free now": {later: func(w *world) {
			w.listens(false)
			w.owner(ownership.Owner{Class: ownership.Unowned}, nil)
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Foreign, PID: 4242, Reason: "another install"}, nil)
				m := newMonitor(t, w.options(true))
				if res := tick(t, m); res.State != proxyrecovery.ForeignPortOwner {
					t.Fatalf("first tick = %+v, want foreign-port-owner", res)
				}
				w.sequence()
				w.journal()
				if tt.later != nil {
					tt.later(w)
				}
				for i := range 3 {
					if res := tick(t, m); res.State != proxyrecovery.ForeignPortOwner || !res.RetryAt.IsZero() {
						t.Errorf("tick %d after the halt = %+v, want foreign-port-owner without a retry time", i+1, res)
					}
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("ticks after the halt called %q", got)
				}
				if got := w.journal(); len(got) != 0 {
					t.Errorf("ticks after the halt journaled %v", got)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if n := w.count("stop-owned") + w.count("start"); n != 0 {
					t.Errorf("a halted monitor started or stopped %d times", n)
				}
			})
		})
	}
}

// TestForeignHaltWithOwnChild decides a foreign halt while a proxy this monitor started is still running: later ticks
// stay foreign and call nothing; the halt is not a stop, so Stop still stops the monitor's own proxy, by its whole
// record and once, and a tick after Stop reports stopped and calls nothing. The foreign holder is never stopped.
func TestForeignHaltWithOwnChild(t *testing.T) {
	tests := map[string]struct {
		// ticks is the number of ticks after the halt and before Stop.
		ticks int
	}{
		"success: Stop right after the halt":     {},
		"success: Stop after three silent ticks": {ticks: 3},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(false)
				m := newMonitor(t, w.options(true))
				tick(t, m)
				time.Sleep(30 * time.Second)
				// Another holder now has the port, and the lookup proves it foreign. The monitor's own proxy still runs.
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Foreign, PID: 4242, Reason: "another install"}, nil)
				if res := tick(t, m); res.State != proxyrecovery.ForeignPortOwner || !res.RetryAt.IsZero() {
					t.Fatalf("the halting tick = %+v, want foreign-port-owner without a retry time", res)
				}
				w.sequence()
				w.journal()
				for i := range tt.ticks {
					if res := tick(t, m); res.State != proxyrecovery.ForeignPortOwner || !res.RetryAt.IsZero() {
						t.Errorf("tick %d after the halt = %+v, want foreign-port-owner", i+1, res)
					}
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("ticks after the halt called %q", got)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if diff := gocmp.Diff([]string{"stop-owned:900", "wait-release"}, w.sequence()); diff != "" {
					t.Errorf("calls of Stop after the halt (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]procinfo.Record{proxyRecord(900)}, w.stops()); diff != "" {
					t.Errorf("records stopped (-want +got):\n%s", diff)
				}
				res, err := goRecover(m, t.Context()).result(t, "Recover after Stop")
				if err != nil || res.State != proxyrecovery.Stopped {
					t.Errorf("Recover after Stop = %+v, %v; want stopped", res, err)
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("Recover after Stop called %q", got)
				}
				if got := w.journal(); len(got) != 0 {
					t.Errorf("the halted monitor journaled %v", got)
				}
			})
		})
	}
}

// TestOwnerUnknownStaysEligible decides that an unresolved holder is not a halt (lib/process-supervision.js:1117-1139):
// at its retry time the next attempt looks the owner up again, and it is never stopped.
func TestOwnerUnknownStaysEligible(t *testing.T) {
	tests := map[string]struct {
		owner ownership.Owner
		err   error
	}{
		"success: an unidentified holder": {owner: ownership.Owner{Class: ownership.Unknown, PID: 903}},
		"success: a timed-out lookup":     {owner: ownership.Owner{Class: ownership.Unknown}, err: context.DeadlineExceeded},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(true)
				w.owner(tt.owner, tt.err)
				m := newMonitor(t, w.options(true))
				for attempt := 1; attempt <= 3; attempt++ {
					res := tick(t, m)
					if res.State != proxyrecovery.OwnerUnknown {
						t.Errorf("attempt %d = %+v, want owner-unknown", attempt, res)
					}
					if diff := gocmp.Diff([]event{attemptEvent(attempt), finishedEvent(proxyrecovery.OwnerUnknown)}, w.journal()); diff != "" {
						t.Errorf("journal of attempt %d (-want +got):\n%s", attempt, diff)
					}
					w.until(res.RetryAt)
				}
				if n := w.count("resolve"); n != 3 {
					t.Errorf("the owner was looked up %d times in three attempts", n)
				}
				if n := w.count("stop-owned") + w.count("start") + w.count("recorded"); n != 0 {
					t.Errorf("an unresolved holder led to %d record reads, stops or starts", n)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestUndecidedHolders covers evidence the contract and the source do not map to an outcome. Only the safety
// property is decided: nothing is stopped, and no proxy is started onto a port that is not known to be free. The
// state each case reports is logged, not asserted.
func TestUndecidedHolders(t *testing.T) {
	sample := errors.New("the listener table could not be read")
	tests := map[string]struct {
		setup func(w *world)
		// startAllowed leaves open whether a proxy is started.
		startAllowed bool
	}{
		"success: a Node gateway of this install on the proxy port is neither stopped nor raced": {
			setup: func(w *world) {
				w.owner(ownership.Owner{Class: ownership.LegacyNode, PID: 4242, Reason: "the Node gateway of this install"}, nil)
			},
		},
		"success: a port released between the sample and the lookup stops nothing": {
			setup:        func(w *world) { w.owner(ownership.Owner{Class: ownership.Unowned}, nil) },
			startAllowed: true,
		},
		"success: a listener table that cannot be read stops and starts nothing": {
			setup: func(w *world) {
				w.set(func(b *behaviour) { b.listening = func(context.Context) (bool, error) { return false, sample } })
			},
		},
		"success: a listener table that cannot be read the second time stops and starts nothing": {
			setup: func(w *world) {
				var n int
				w.set(func(b *behaviour) {
					b.listening = func(context.Context) (bool, error) {
						w.mu.Lock()
						defer w.mu.Unlock()
						n++
						if n == 1 {
							return true, nil
						}
						return false, sample
					}
				})
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(true)
				tt.setup(w)
				m := newMonitor(t, w.options(true))
				res := tick(t, m)
				t.Logf("reported %+v after calls %q", res, w.sequence())
				if n := w.count("stop-owned"); n != 0 {
					t.Errorf("StopOwned was called %d times", n)
				}
				if n := w.count("start"); !tt.startAllowed && n != 0 {
					t.Errorf("Start was called %d times", n)
				}
				if res.State == proxyrecovery.Healthy || res.State == proxyrecovery.Recovered {
					t.Errorf("an unanswering proxy was reported %q", res.State)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestFailedStarts covers a stop or a start that fails, which the source does not report (lib/process-supervision.js:
// 1153, 1163). Decided: a failed stop never leads to a start while the port is held; a failed start journals no
// started proxy, keeps the attempt's retry time, and is retried only at that time.
func TestFailedStarts(t *testing.T) {
	spawn := errors.New("the proxy could not be started")
	tests := map[string]struct {
		setup func(w *world)
		check func(t *testing.T, m *proxyrecovery.Monitor, w *world, res proxyrecovery.Result, at time.Time)
	}{
		"success: a refused stop with the port still held starts nothing": {
			setup: func(w *world) {
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
				w.set(func(b *behaviour) {
					b.stopOwned = func(context.Context, procinfo.Record) error { return ownership.ErrNotOwned }
					b.waitRelease = func(context.Context) bool { return false }
				})
			},
			check: func(t *testing.T, _ *proxyrecovery.Monitor, w *world, res proxyrecovery.Result, _ time.Time) {
				t.Logf("reported %+v", res)
				if n := w.count("stop-owned:4242"); n != 1 {
					t.Errorf("the stop of the holder was attempted %d times, want once", n)
				}
				if n := w.count("start"); n != 0 {
					t.Errorf("Start was called %d times after a refused stop", n)
				}
			},
		},
		"success: a failed start is retried only at the attempt's retry time": {
			setup: func(w *world) {
				w.listens(false)
				w.set(func(b *behaviour) {
					b.start = func(context.Context) (proxyrecovery.Child, error) { return proxyrecovery.Child{}, spawn }
				})
			},
			check: func(t *testing.T, m *proxyrecovery.Monitor, w *world, res proxyrecovery.Result, at time.Time) {
				if !res.RetryAt.Equal(at.Add(time.Second)) {
					t.Errorf("RetryAt after a failed start = %v, want %v", res.RetryAt, at.Add(time.Second))
				}
				for _, e := range w.journal() {
					if e.Name == "proxy-started" {
						t.Errorf("a failed start was journaled as %v", e)
					}
				}
				time.Sleep(time.Second - time.Nanosecond)
				tick(t, m)
				if n := w.count("start"); n != 1 {
					t.Errorf("Start was called %d times before the retry time", n)
				}
				time.Sleep(time.Nanosecond)
				tick(t, m)
				if n := w.count("start"); n != 2 {
					t.Errorf("Start was called %d times by the retry time, want 2", n)
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(true)
				tt.setup(w)
				m := newMonitor(t, w.options(true))
				at := w.now()
				res := tick(t, m)
				tt.check(t, m, w, res, at)
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}
