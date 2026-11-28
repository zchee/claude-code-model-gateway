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
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// bindsWhileAlive makes the port held, and its holder this monitor's own proxy, while a proxy the world started is
// running, as a real proxy binds its port at once.
func (w *world) bindsWhileAlive() {
	live := func() int {
		for _, pid := range w.startedPIDs() {
			if c := w.child(pid); c != nil && !c.exited() {
				return pid
			}
		}
		return 0
	}
	w.set(func(b *behaviour) {
		b.listening = func(context.Context) (bool, error) { return live() != 0, nil }
		b.resolve = func(context.Context) (ownership.Owner, error) {
			if pid := live(); pid != 0 {
				return ownership.Owner{Class: ownership.Ours, PID: pid}, nil
			}
			return ownership.Owner{Class: ownership.Unowned}, nil
		}
	})
}

// slowStarts makes every start take startTook and the first probe after each start take probeTook, so that the
// moment Start returns differs from the tick's start, from Start's entry and from the end of the tick.
func (w *world) slowStarts(startTook, probeTook time.Duration) {
	var afterStart bool
	w.set(func(b *behaviour) {
		b.start = func(context.Context) (proxyrecovery.Child, error) {
			time.Sleep(startTook)
			w.mu.Lock()
			afterStart = true
			w.mu.Unlock()
			return w.spawn(), nil
		}
		b.probe = func(context.Context) bool {
			w.mu.Lock()
			slow := afterStart
			afterStart = false
			w.mu.Unlock()
			if slow {
				time.Sleep(probeTook)
			}
			return false
		}
	})
}

// replacement returns the calls of a tick that replaces the monitor's own unanswering proxy pid at the end of its
// grace, as the permitted sequences: with the port held by it the holder is looked up, and the record of the
// monitor's own proxy may be read once before its stop is journaled.
func replacement(bound bool, pid int) [][]string {
	stopped := "stop-owned:" + strconv.Itoa(pid)
	if !bound {
		return [][]string{{
			"probe", "listening", "record:proxy-recovery-started", "binary", "listening", stopped,
			"wait-release", "start", "record:proxy-started", "probe", "record:proxy-recovery-finished",
		}}
	}
	head := []string{"probe", "listening", "probe", "record:proxy-recovery-started", "binary", "listening", "resolve"}
	tail := []string{
		"record:proxy-stop-requested", stopped, "wait-release", "start", "record:proxy-started", "probe",
		"record:proxy-recovery-finished",
	}
	return [][]string{
		append(append(append([]string{}, head...), "recorded:"+strconv.Itoa(pid)), tail...),
		append(append([]string{}, head...), tail...),
	}
}

// TestStartupGrace decides the 30 s grace of a proxy this monitor started (lib/process-supervision.js:1052-1054,
// 1072-1074, 1092, 1163-1167; test/shim-lifecycle.test.js:16, 67). The grace is counted from the moment Start
// returns: time passes separately before the tick, inside Start and inside the probe after the start, so the last
// tick inside the grace and the first one after it fall on that moment and on no other. Inside the grace a tick only
// probes and reports the child; at its end an unanswering child is stopped and its release checked before the
// replacement starts; a child that exited has no grace, and it is neither stopped nor waited for.
func TestStartupGrace(t *testing.T) {
	const startTook, probeTook = 3 * time.Second, 2 * time.Second
	tests := map[string]struct {
		// bound makes the port held while a child of this monitor lives, as a real proxy binds at once.
		bound bool
	}{
		"success: a child that never binds":   {},
		"success: a child that binds at once": {bound: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.listens(false)
				w.slowStarts(startTook, probeTook)
				if tt.bound {
					w.bindsWhileAlive()
				}
				m := newMonitor(t, w.options(true))
				// The monitor's construction is not the grace's start.
				time.Sleep(700 * time.Millisecond)
				at := w.now()
				if res := tick(t, m); res.State != proxyrecovery.Starting || !res.RetryAt.Equal(at.Add(time.Second)) {
					t.Fatalf("first tick = %+v, want starting until %v", res, at.Add(time.Second))
				}
				startReturned := at.Add(startTook)
				w.sequence()
				w.journal()
				for _, in := range []time.Duration{5 * time.Second, 30*time.Second - time.Nanosecond} {
					w.until(startReturned.Add(in))
					res := tick(t, m)
					if res.State != proxyrecovery.Starting || res.PID != 900 {
						t.Errorf("tick %v after Start returned = %+v, want starting with PID 900", in, res)
					}
					if diff := gocmp.Diff([]string{"probe"}, w.sequence()); diff != "" {
						t.Errorf("calls %v after Start returned, inside the grace (-want +got):\n%s", in, diff)
					}
				}
				if got := w.journal(); len(got) != 0 {
					t.Errorf("ticks inside the grace journaled %v", got)
				}

				// Thirty seconds after Start returned the grace has ended, and the child does not answer: it is stopped
				// and replaced.
				w.until(startReturned.Add(30 * time.Second))
				graceEnd := w.now()
				res := tick(t, m)
				if ok, diff := oneOf(w.sequence(), replacement(tt.bound, 900)...); !ok {
					t.Errorf("calls at the end of the grace: %s", diff)
				}
				wantJournal := []event{attemptEvent(2), startedEvent(901), finishedEvent(proxyrecovery.Starting)}
				if tt.bound {
					wantJournal = []event{attemptEvent(2), stopRequestedEvent(900), startedEvent(901), finishedEvent(proxyrecovery.Starting)}
				}
				if diff := gocmp.Diff(wantJournal, w.journal()); diff != "" {
					t.Errorf("journal at the end of the grace (-want +got):\n%s", diff)
				}
				if res.State != proxyrecovery.Starting || !res.RetryAt.Equal(graceEnd.Add(2*time.Second)) {
					t.Errorf("tick at the end of the grace = %+v, want starting until %v", res, graceEnd.Add(2*time.Second))
				}

				// The replacement exits on its own after its retry time: the next tick neither stops it nor waits for the
				// release of its port, and starts the next proxy.
				w.exit(901)
				synctest.Wait()
				w.until(graceEnd.Add(2 * time.Second))
				tick(t, m)
				want := []string{
					"probe", "listening", "record:proxy-recovery-started", "binary", "listening", "start",
					"record:proxy-started", "probe", "record:proxy-recovery-finished",
				}
				if diff := gocmp.Diff(want, w.sequence()); diff != "" {
					t.Errorf("calls of the tick after the replacement exited (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]int{900, 901, 902}, w.startedPIDs()); diff != "" {
					t.Errorf("proxies started (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]procinfo.Record{proxyRecord(900)}, w.stops()); diff != "" {
					t.Errorf("records stopped (-want +got):\n%s", diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestExitMatchedByIdentity decides that the exit of a proxy this monitor replaced says nothing about its
// replacement (lib/process-supervision.js:1175-1178): the old child's exit is seen only after the new child started,
// and the new child keeps its grace and its process number, and Stop stops the new child, once.
func TestExitMatchedByIdentity(t *testing.T) {
	tests := map[string]struct {
		// answers makes the replacement answer the probe after its start.
		answers bool
		state   proxyrecovery.State
	}{
		"success: the replaced child exits after its replacement started":  {state: proxyrecovery.Starting},
		"success: the replaced child exits after its replacement answered": {answers: true, state: proxyrecovery.Recovered},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(false)
				// A stop does not end the child here; its exit is seen later.
				w.set(func(b *behaviour) { b.stopOwned = func(context.Context, procinfo.Record) error { return nil } })
				m := newMonitor(t, w.options(true))
				tick(t, m)
				time.Sleep(30 * time.Second)
				if tt.answers {
					w.answers(false, true, false)
				}
				if res := tick(t, m); res.State != tt.state {
					t.Fatalf("the replacing tick = %+v, want %q", res, tt.state)
				}
				replaced := w.now()
				if diff := gocmp.Diff([]int{900, 901}, w.startedPIDs()); diff != "" {
					t.Fatalf("proxies started (-want +got):\n%s", diff)
				}
				w.exit(900)
				synctest.Wait()
				w.sequence()
				w.journal()
				w.until(replaced.Add(5 * time.Second))
				if res := tick(t, m); res.State != proxyrecovery.Starting || res.PID != 901 {
					t.Errorf("a tick inside the replacement's grace = %+v, want starting with PID 901", res)
				}
				if diff := gocmp.Diff([]string{"probe"}, w.sequence()); diff != "" {
					t.Errorf("calls inside the replacement's grace (-want +got):\n%s", diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if diff := gocmp.Diff([]string{"stop-owned:901", "wait-release"}, w.sequence()); diff != "" {
					t.Errorf("calls of Stop (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]procinfo.Record{proxyRecord(900), proxyRecord(901)}, w.stops()); diff != "" {
					t.Errorf("records stopped (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestExitMatchedByInstance decides that an exit is matched by the child it belongs to, not by its process number: a
// replacement that the system gave the same process number, with its own generation, start token and exit, outlives
// the late exit of the child it replaced. No exit is journaled, the replacement keeps its grace, and Stop stops the
// replacement by its own whole record, once.
func TestExitMatchedByInstance(t *testing.T) {
	tests := map[string]struct {
		// answers makes the replacement answer the probe after its start.
		answers bool
		state   proxyrecovery.State
	}{
		"success: the replaced child exits after its replacement started":  {state: proxyrecovery.Starting},
		"success: the replaced child exits after its replacement answered": {answers: true, state: proxyrecovery.Recovered},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				first, second := proxyRecord(900), proxyRecord(900)
				second.Generation = "proxy-generation-900-reused"
				second.StartToken = procinfo.StartToken("2b1c0f9e-6d1a-4c0b-9a51-1f2e3d4c5b6a/990900")
				firstDone, secondDone := make(chan struct{}), make(chan struct{})
				var closeFirst, closeSecond sync.Once
				t.Cleanup(func() {
					closeFirst.Do(func() { close(firstDone) })
					closeSecond.Do(func() { close(secondDone) })
				})
				w := newWorld(t)
				w.answers(false)
				w.listens(false)
				var starts int
				w.set(func(b *behaviour) {
					b.start = func(context.Context) (proxyrecovery.Child, error) {
						w.mu.Lock()
						starts++
						n := starts
						w.mu.Unlock()
						if n == 1 {
							return proxyrecovery.Child{Record: first, Done: firstDone}, nil
						}
						return proxyrecovery.Child{Record: second, Done: secondDone}, nil
					}
					// A stop does not end the child here; its exit is delivered later.
					b.stopOwned = func(context.Context, procinfo.Record) error { return nil }
				})
				m := newMonitor(t, w.options(true))
				tick(t, m)
				time.Sleep(30 * time.Second)
				if tt.answers {
					w.answers(false, true, false)
				}
				if res := tick(t, m); res.State != tt.state {
					t.Fatalf("the replacing tick = %+v, want %q", res, tt.state)
				}
				replaced := w.now()
				if diff := gocmp.Diff([]procinfo.Record{first}, w.stops()); diff != "" {
					t.Fatalf("records stopped by the replacing tick (-want +got):\n%s", diff)
				}
				w.sequence()
				w.journal()
				closeFirst.Do(func() { close(firstDone) })
				synctest.Wait()
				if got := w.journal(); len(got) != 0 {
					t.Errorf("the late exit of the replaced child was journaled as %v", got)
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("the late exit of the replaced child led to calls %q", got)
				}
				w.until(replaced.Add(5 * time.Second))
				if res := tick(t, m); res.State != proxyrecovery.Starting || res.PID != 900 {
					t.Errorf("a tick inside the replacement's grace = %+v, want starting with PID 900", res)
				}
				if diff := gocmp.Diff([]string{"probe"}, w.sequence()); diff != "" {
					t.Errorf("calls inside the replacement's grace (-want +got):\n%s", diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if diff := gocmp.Diff([]string{"stop-owned:900", "wait-release"}, w.sequence()); diff != "" {
					t.Errorf("calls of Stop (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]procinfo.Record{first, second}, w.stops()); diff != "" {
					t.Errorf("records stopped (-want +got):\n%s", diff)
				}
				// The exit of the stopped replacement arrives after Stop returned: it is journaled nowhere and leads to
				// no stop.
				records, stops := w.effects()
				closeSecond.Do(func() { close(secondDone) })
				synctest.Wait()
				if laterRecords, laterStops := w.effects(); laterRecords != records || laterStops != stops {
					t.Errorf("the exit after Stop led to %d journal calls and %d stops", laterRecords-records, laterStops-stops)
				}
			})
		})
	}
}

// TestOwnChildReleaseRefused decides what follows when the monitor has stopped its own proxy, which did not hold the
// port, and the release check then reports that the port is still held: nothing is started onto it, and the tick
// reports port-stuck with the attempt's retry time, as a stopped holder of the port does. This is a deliberate
// difference from the source, which ignores the answer of that check and starts a proxy anyway
// (lib/process-supervision.js:1076-1081, 1159-1163).
func TestOwnChildReleaseRefused(t *testing.T) {
	tests := map[string]struct {
		// exits makes the stop end the child; otherwise it is still running when the release check answers.
		exits bool
	}{
		"success: a child that exits when stopped": {exits: true},
		"success: a child that outlives its stop":  {},
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
				w.set(func(b *behaviour) {
					b.waitRelease = func(context.Context) bool { return false }
					if !tt.exits {
						b.stopOwned = func(context.Context, procinfo.Record) error { return nil }
					}
				})
				w.sequence()
				w.journal()
				at := w.now()
				res := tick(t, m)
				if !res.RetryAt.Equal(at.Add(2*time.Second)) || res.State != proxyrecovery.PortStuck {
					t.Errorf("result = %+v, want port-stuck until %v", res, at.Add(2*time.Second))
				}
				want := []string{
					"probe", "listening", "record:proxy-recovery-started", "binary", "listening", "stop-owned:900",
					"wait-release", "record:proxy-recovery-finished",
				}
				if diff := gocmp.Diff(want, w.sequence()); diff != "" {
					t.Errorf("calls (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]event{attemptEvent(2), finishedEvent(proxyrecovery.PortStuck)}, w.journal()); diff != "" {
					t.Errorf("journal (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]int{900}, w.startedPIDs()); diff != "" {
					t.Errorf("proxies started (-want +got):\n%s", diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestWarmingProxyIsNotRaced decides the source's slow-start case (test/shim-lifecycle.test.js:16): a child that
// answers before its grace ends is never stopped and never given a second proxy beside it.
func TestWarmingProxyIsNotRaced(t *testing.T) {
	tests := map[string]struct {
		answersAt time.Duration
	}{
		"success: answering after one and a half seconds": {answersAt: 1500 * time.Millisecond},
		"success: answering just before the grace ends":   {answersAt: 30*time.Second - time.Millisecond},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				t0 := w.now()
				w.listens(true)
				w.set(func(b *behaviour) {
					b.probe = func(context.Context) bool { return !w.now().Before(t0.Add(tt.answersAt)) }
					b.listening = func(context.Context) (bool, error) { return len(w.startedPIDs()) > 0, nil }
					b.resolve = func(context.Context) (ownership.Owner, error) {
						return ownership.Owner{Class: ownership.Ours, PID: 900}, nil
					}
				})
				m := newMonitor(t, w.options(true))
				var res proxyrecovery.Result
				for range 400 {
					if res = tick(t, m); res.State == proxyrecovery.Healthy {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				if res.State != proxyrecovery.Healthy {
					t.Errorf("the warming proxy never became healthy: %+v", res)
				}
				if diff := gocmp.Diff([]int{900}, w.startedPIDs()); diff != "" {
					t.Errorf("proxies started (-want +got):\n%s", diff)
				}
				if n := w.count("stop-owned"); n != 0 {
					t.Errorf("the warming proxy was stopped %d times", n)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// TestProxyExitJournal decides that the monitor journals nothing when a proxy it started exits. Only the collaborator
// behind Start waits on the process and knows its exit status, so the exit record, with its exit code and signal,
// belongs to the integration. The tick after each exit, at the attempt's retry time, starts the next proxy without
// stopping the one that exited.
func TestProxyExitJournal(t *testing.T) {
	tests := map[string]struct {
		exits []int
	}{
		"success: one child exits":                  {exits: []int{900}},
		"success: a child and its replacement exit": {exits: []int{900, 901}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(false)
				m := newMonitor(t, w.options(true))
				res := tick(t, m)
				for _, pid := range tt.exits {
					w.journal()
					w.sequence()
					w.exit(pid)
					synctest.Wait()
					if got := w.journal(); len(got) != 0 {
						t.Errorf("the exit of proxy %d was journaled as %v", pid, got)
					}
					if got := w.sequence(); len(got) != 0 {
						t.Errorf("the exit of proxy %d led to calls %q", pid, got)
					}
					w.until(res.RetryAt)
					res = tick(t, m)
					if res.State != proxyrecovery.Starting {
						t.Errorf("the tick after proxy %d exited = %+v, want starting", pid, res)
					}
				}
				if diff := gocmp.Diff(append(append([]int{}, tt.exits...), 900+len(tt.exits)), w.startedPIDs()); diff != "" {
					t.Errorf("proxies started (-want +got):\n%s", diff)
				}
				if n := w.count("stop-owned"); n != 0 {
					t.Errorf("a proxy that exited by itself was stopped %d times", n)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}
