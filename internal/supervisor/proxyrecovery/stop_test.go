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
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// TestStop decides what Stop does when no tick is running (lib/process-supervision.js:1076-1081, 1201-1206): it stops
// only a proxy this monitor started and is still running, by that proxy's whole record, checks the port's release once
// and ignores a failed check; a second Stop does nothing; a tick after Stop reports stopped and asks nothing.
func TestStop(t *testing.T) {
	tests := map[string]struct {
		setup func(t *testing.T, m *proxyrecovery.Monitor, w *world)
		want  []string
		// stops are the records Stop passes to StopOwned.
		stops []procinfo.Record
	}{
		"success: a monitor that started nothing stops nothing": {
			setup: func(*testing.T, *proxyrecovery.Monitor, *world) {},
			want:  []string{},
		},
		"success: the proxy this monitor started is stopped and its release checked": {
			setup: func(t *testing.T, m *proxyrecovery.Monitor, _ *world) { tick(t, m) },
			want:  []string{"stop-owned:900", "wait-release"},
			stops: []procinfo.Record{proxyRecord(900)},
		},
		"success: a release check that fails is ignored": {
			setup: func(t *testing.T, m *proxyrecovery.Monitor, w *world) {
				tick(t, m)
				w.set(func(b *behaviour) { b.waitRelease = func(context.Context) bool { return false } })
			},
			want:  []string{"stop-owned:900", "wait-release"},
			stops: []procinfo.Record{proxyRecord(900)},
		},
		"success: a proxy this monitor started that already exited is not stopped": {
			setup: func(t *testing.T, m *proxyrecovery.Monitor, w *world) {
				tick(t, m)
				w.exit(900)
				synctest.Wait()
			},
			want: []string{},
		},
		"success: only the latest proxy is stopped, not the one it replaced": {
			setup: func(t *testing.T, m *proxyrecovery.Monitor, w *world) {
				tick(t, m)
				time.Sleep(30 * time.Second)
				tick(t, m)
			},
			want:  []string{"stop-owned:901", "wait-release"},
			stops: []procinfo.Record{proxyRecord(901)},
		},
		"success: a holder this monitor stopped and replaced is not stopped again": {
			setup: func(t *testing.T, m *proxyrecovery.Monitor, w *world) {
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
				tick(t, m)
			},
			want:  []string{"stop-owned:900", "wait-release"},
			stops: []procinfo.Record{proxyRecord(900)},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(false)
				m := newMonitor(t, w.options(true))
				tt.setup(t, m, w)
				w.sequence()
				w.journal()
				before := len(w.stops())
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if diff := gocmp.Diff(tt.want, w.sequence(), cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("calls of Stop (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.stops, w.stops()[before:], cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("records Stop stopped (-want +got):\n%s", diff)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("a second Stop: %v", err)
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("a second Stop called %q", got)
				}
				w.answers(false)
				res, err := goRecover(m, t.Context()).result(t, "Recover after Stop")
				if err != nil || res.State != proxyrecovery.Stopped {
					t.Errorf("Recover after Stop = %+v, %v; want stopped", res, err)
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("Recover after Stop called %q", got)
				}
				for _, e := range w.journal() {
					if e.Name == "proxy-recovery-started" || e.Name == "proxy-started" {
						t.Errorf("Stop or a tick after it journaled %v", e)
					}
				}
			})
		})
	}
}

// heldCall is a collaborator call that a test holds open. It records its entry, returns early only when the test
// lets it go, and, when cancellable, records that its context ended and then parks until the test lets it go, so
// that a caller can be seen waiting for it to return.
type heldCall struct {
	in        chan struct{}
	cancelled chan struct{}
	letGo     chan struct{}
	inOnce    sync.Once
	endOnce   sync.Once
	goOnce    sync.Once
}

func newHeldCall(t *testing.T) *heldCall {
	h := &heldCall{in: make(chan struct{}), cancelled: make(chan struct{}), letGo: make(chan struct{})}
	t.Cleanup(h.release)
	return h
}

func (h *heldCall) release() { h.goOnce.Do(func() { close(h.letGo) }) }

// wait holds the call. A cancellable call also returns when ctx ends, after it has parked until the test lets it go.
func (h *heldCall) wait(ctx context.Context, cancellable bool) {
	h.inOnce.Do(func() { close(h.in) })
	if !cancellable {
		<-h.letGo
		return
	}
	select {
	case <-ctx.Done():
		h.endOnce.Do(func() { close(h.cancelled) })
		<-h.letGo
	case <-h.letGo:
	}
}

// until holds the call, recording its entry, until ctx ends or the test lets it go; it does not park.
func (h *heldCall) until(ctx context.Context) {
	h.inOnce.Do(func() { close(h.in) })
	select {
	case <-ctx.Done():
	case <-h.letGo:
	}
}

// await waits, bounded by the watchdog, for ch to close.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(watchdog):
		t.Fatalf("%s never happened", what)
	}
}

// TestStopCancelsProbes decides that Stop cancels the probe work of a tick in flight and joins it: the held probe's
// context ends, and Stop does not return while that probe has not returned (test/gateway-drain.test.js:927;
// test/gateway-process-isolation.test.js:1887). A tick stopped before it acted stops and starts nothing and reports
// stopped; one stopped in its owner lookup may report owner-unknown instead, as the source does when its lookup child
// is killed. A tick stopped in the probe after its start has started a proxy: Stop also waits for the stop of that
// proxy, which it makes itself, before it returns.
func TestStopCancelsProbes(t *testing.T) {
	tests := map[string]struct {
		step   string
		states []proxyrecovery.State
		// started is the number of proxies the tick started before Stop; stopped are the processes stopped in all.
		started int
		stopped []int
	}{
		"success: during the first probe":            {step: "probe", states: []proxyrecovery.State{proxyrecovery.Stopped}},
		"success: during the listener sample":        {step: "listening", states: []proxyrecovery.State{proxyrecovery.Stopped}},
		"success: during the confirmation probe":     {step: "second probe", states: []proxyrecovery.State{proxyrecovery.Stopped}},
		"success: during the second listener sample": {step: "second listening", states: []proxyrecovery.State{proxyrecovery.Stopped}},
		"success: during the owner lookup": {
			step:   "resolve",
			states: []proxyrecovery.State{proxyrecovery.Stopped, proxyrecovery.OwnerUnknown},
		},
		"success: during the probe after the start": {
			step:    "third probe",
			states:  []proxyrecovery.State{proxyrecovery.Stopped, proxyrecovery.Starting},
			started: 1,
			stopped: []int{4242, 900},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
				h, cleanup := newHeldCall(t), newHeldCall(t)
				var probes, samples int
				w.set(func(b *behaviour) {
					b.probe = func(ctx context.Context) bool {
						w.mu.Lock()
						probes++
						n := probes
						w.mu.Unlock()
						if tt.step == "probe" && n == 1 || tt.step == "second probe" && n == 2 || tt.step == "third probe" && n == 3 {
							h.wait(ctx, true)
						}
						return false
					}
					b.listening = func(ctx context.Context) (bool, error) {
						w.mu.Lock()
						samples++
						n := samples
						w.mu.Unlock()
						if tt.step == "listening" && n == 1 || tt.step == "second listening" && n == 2 {
							h.wait(ctx, true)
							return false, ctx.Err()
						}
						return true, nil
					}
					if tt.step == "resolve" {
						b.resolve = func(ctx context.Context) (ownership.Owner, error) {
							h.wait(ctx, true)
							return ownership.Owner{Class: ownership.Unknown}, ctx.Err()
						}
					}
					// The stop of the proxy this monitor started is held as well, so that Stop can be seen waiting for it.
					b.stopOwned = func(ctx context.Context, r procinfo.Record) error {
						if r.PID == 900 {
							cleanup.wait(ctx, false)
						}
						w.exit(r.PID)
						return nil
					}
				})
				m := newMonitor(t, w.options(true))
				p := goRecover(m, t.Context())
				await(t, h.in, "the call to "+tt.step)
				synctest.Wait()
				s := goStop(m, t.Context())
				await(t, h.cancelled, "the cancellation of "+tt.step+" by Stop")
				synctest.Wait()
				if s.finished() {
					t.Errorf("Stop returned while %s had not returned", tt.step)
				}
				// Stop joins the tick before it stops the proxy the tick started (lib/process-supervision.js:1203-1205).
				if closed(cleanup.in) {
					t.Errorf("Stop began to stop the monitor's own proxy while the tick was still in %s", tt.step)
				}
				h.release()
				if tt.started > 0 {
					await(t, cleanup.in, "the stop of the proxy the tick started")
					synctest.Wait()
					if s.finished() {
						t.Errorf("Stop returned while the stop of the proxy the tick started had not returned")
					}
					cleanup.release()
				}
				if _, err := s.result(t, "Stop"); err != nil {
					t.Errorf("Stop: %v", err)
				}
				// Once Stop has returned, nothing more is journaled or stopped.
				records, stops := w.effects()
				res, err := p.result(t, "Recover")
				synctest.Wait()
				if laterRecords, laterStops := w.effects(); laterRecords != records || laterStops != stops {
					t.Errorf("after Stop returned, %d journal calls and %d stops were entered", laterRecords-records, laterStops-stops)
				}
				if err != nil || !slices.Contains(tt.states, res.State) {
					t.Errorf("the stopped tick = %+v, %v; want one of %q", res, err, tt.states)
				}
				if n := w.count("start"); n != tt.started {
					t.Errorf("a tick stopped in %s started %d proxies, want %d", tt.step, n, tt.started)
				}
				var stopped []int
				for _, r := range w.stops() {
					stopped = append(stopped, r.PID)
				}
				if diff := gocmp.Diff(tt.stopped, stopped, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("processes stopped (-want +got):\n%s", diff)
				}
				for _, pid := range w.startedPIDs() {
					if !w.child(pid).exited() {
						t.Errorf("the monitor's own proxy %d was left running by Stop", pid)
					}
				}
			})
		})
	}
}

// TestStopDuringActions decides Stop during a step of a tick that acts: Stop waits for the step to return, then no
// proxy is started; a proxy whose start was under way is the monitor's own, and Stop stops it and checks its release
// before it returns. The held steps here ignore their context, as a start or a stop may.
func TestStopDuringActions(t *testing.T) {
	tests := map[string]struct {
		step string
		// bound makes the port held by a holder this root's record names, so that the tick stops it first.
		bound bool
	}{
		"success: during the start":            {step: "start"},
		"success: during the stop of a holder": {step: "stop", bound: true},
		"success: during the release check":    {step: "release", bound: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(tt.bound)
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
				h := newHeldCall(t)
				w.set(func(b *behaviour) {
					switch tt.step {
					case "start":
						b.start = func(ctx context.Context) (proxyrecovery.Child, error) {
							h.wait(ctx, false)
							return w.spawn(), nil
						}
					case "stop":
						b.stopOwned = func(ctx context.Context, r procinfo.Record) error {
							h.wait(ctx, false)
							w.exit(r.PID)
							return nil
						}
					case "release":
						b.waitRelease = func(ctx context.Context) bool {
							w.mu.Lock()
							first := len(w.stopped) == 1
							w.mu.Unlock()
							if first {
								h.wait(ctx, false)
							}
							return true
						}
					}
				})
				m := newMonitor(t, w.options(true))
				p := goRecover(m, t.Context())
				await(t, h.in, "the "+tt.step)
				synctest.Wait()
				startsBefore := w.count("start")
				s := goStop(m, t.Context())
				synctest.Wait()
				if s.finished() {
					t.Errorf("Stop returned while the %s had not returned", tt.step)
				}
				w.sequence()
				h.release()
				if _, err := s.result(t, "Stop"); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if _, err := p.result(t, "Recover"); err != nil {
					t.Errorf("Recover: %v", err)
				}
				after := w.sequence()
				if n := w.count("start") - startsBefore; n != 0 {
					t.Errorf("%d proxies were started after Stop, calls %q", n, after)
				}
				if tt.step != "start" {
					if pids := w.startedPIDs(); len(pids) != 0 {
						t.Errorf("proxies %v were started", pids)
					}
					return
				}
				stopAt := slices.Index(after, "stop-owned:900")
				if stopAt < 0 || !slices.Contains(after[stopAt:], "wait-release") {
					t.Errorf("Stop did not stop the proxy whose start was under way and check its release: calls %q", after)
				}
				if c := w.child(900); c == nil || !c.exited() {
					t.Error("the proxy whose start was under way was left running")
				}
			})
		})
	}
}

// TestStopBeforeActing decides that Stop also ends a tick that has not acted yet: once Stop is called, a tick that was
// still reading this root's record, journaling the stop it is about to make, or taking its last sample of a free port
// stops no holder and starts nothing, and Stop waits for that tick. This is a deliberate difference from the source,
// and a stricter one: the source checks its halt flag only after its probes and before its start
// (lib/process-supervision.js:1091, 1101, 1161), so a stop that arrives during its owner check can still be followed by
// its signal to the holder (:1146-1153). For the same reason a tick after Stop reports the stopped state where the
// source reports the foreign-owner state.
func TestStopBeforeActing(t *testing.T) {
	tests := map[string]struct {
		step string
	}{
		"success: during the record read":                         {step: "recorded"},
		"success: during an owner lookup that answers":            {step: "resolve"},
		"success: while the stop of the holder is journaled":      {step: "journal"},
		"success: during the last listener sample of a free port": {step: "free port"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(true)
				w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
				w.place(proxyRecord(4242))
				h := newHeldCall(t)
				w.set(func(b *behaviour) {
					switch tt.step {
					case "recorded":
						b.recorded = func(ctx context.Context, pid int) (procinfo.Record, error) {
							h.wait(ctx, false)
							return proxyRecord(pid), nil
						}
					case "resolve":
						b.resolve = func(ctx context.Context) (ownership.Owner, error) {
							h.wait(ctx, false)
							return ownership.Owner{Class: ownership.Ours, PID: 4242}, nil
						}
					case "journal":
						b.record = func(name string) {
							if name == "proxy-stop-requested" {
								h.wait(t.Context(), false)
							}
						}
					case "free port":
						var n int
						b.listening = func(ctx context.Context) (bool, error) {
							w.mu.Lock()
							n++
							last := n == 2
							w.mu.Unlock()
							if last {
								h.wait(ctx, false)
							}
							return false, nil
						}
					}
				})
				m := newMonitor(t, w.options(true))
				p := goRecover(m, t.Context())
				await(t, h.in, "the "+tt.step)
				synctest.Wait()
				s := goStop(m, t.Context())
				synctest.Wait()
				if s.finished() {
					t.Errorf("Stop returned while the tick was held at the %s", tt.step)
				}
				h.release()
				if _, err := s.result(t, "Stop"); err != nil {
					t.Errorf("Stop: %v", err)
				}
				// A journal call that had entered before Stop may complete; after Stop returned, none is entered.
				records, stops := w.effects()
				if _, err := p.result(t, "Recover"); err != nil {
					t.Errorf("Recover: %v", err)
				}
				synctest.Wait()
				if laterRecords, laterStops := w.effects(); laterRecords != records || laterStops != stops {
					t.Errorf("after Stop returned, %d journal calls and %d stops were entered", laterRecords-records, laterStops-stops)
				}
				if n := w.count("stop-owned"); n != 0 {
					t.Errorf("the holder was stopped %d times after Stop", n)
				}
				if n := w.count("start"); n != 0 {
					t.Errorf("%d proxies were started after Stop", n)
				}
			})
		})
	}
}

// TestConcurrentStop decides two Stop calls that overlap: while the first one stops the monitor's own proxy, a second
// caller with a live context waits for that work too, and the proxy is stopped once, by its whole record, with one
// check of its release.
func TestConcurrentStop(t *testing.T) {
	tests := map[string]struct {
		// hold is the step of the first Stop that is held while the second caller arrives.
		hold string
	}{
		"success: while the first Stop stops the proxy":    {hold: "stop"},
		"success: while the first Stop checks the release": {hold: "release"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(false)
				m := newMonitor(t, w.options(true))
				tick(t, m)
				w.sequence()
				h := newHeldCall(t)
				w.set(func(b *behaviour) {
					switch tt.hold {
					case "stop":
						b.stopOwned = func(ctx context.Context, r procinfo.Record) error {
							h.wait(ctx, false)
							w.exit(r.PID)
							return nil
						}
					case "release":
						b.waitRelease = func(ctx context.Context) bool {
							h.wait(ctx, false)
							return true
						}
					}
				})
				first := goStop(m, t.Context())
				await(t, h.in, "the "+tt.hold+" of the first Stop")
				synctest.Wait()
				second := goStop(m, t.Context())
				synctest.Wait()
				if first.finished() || second.finished() {
					t.Errorf("a Stop returned while the first one's %s was held: first %v, second %v", tt.hold, first.finished(), second.finished())
				}
				h.release()
				if _, err := first.result(t, "the first Stop"); err != nil {
					t.Errorf("the first Stop: %v", err)
				}
				if _, err := second.result(t, "the second Stop"); err != nil {
					t.Errorf("the second Stop: %v", err)
				}
				if diff := gocmp.Diff([]string{"stop-owned:900", "wait-release"}, w.sequence()); diff != "" {
					t.Errorf("calls of the two Stops (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]procinfo.Record{proxyRecord(900)}, w.stops()); diff != "" {
					t.Errorf("records stopped (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestStopContext decides Stop's own context: when it ends while Stop joins a tick, Stop returns its error at once and
// the monitor stays stopped; a later Stop finishes the work, stopping the proxy whose start was under way once.
func TestStopContext(t *testing.T) {
	tests := map[string]struct {
		cause error
	}{
		"error: cancelled while joining":         {cause: context.Canceled},
		"error: past its deadline while joining": {cause: context.DeadlineExceeded},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				h := newHeldCall(t)
				w.set(func(b *behaviour) {
					b.start = func(ctx context.Context) (proxyrecovery.Child, error) {
						h.wait(ctx, false)
						return w.spawn(), nil
					}
				})
				m := newMonitor(t, w.options(true))
				p := goRecover(m, t.Context())
				await(t, h.in, "the start")
				synctest.Wait()
				var ctx context.Context
				var cancel context.CancelFunc
				if errors.Is(tt.cause, context.DeadlineExceeded) {
					ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				t.Cleanup(cancel)
				s := goStop(m, ctx)
				synctest.Wait()
				if s.finished() {
					t.Fatal("Stop returned while the start had not returned")
				}
				if errors.Is(tt.cause, context.Canceled) {
					cancel()
				}
				synctest.Wait()
				if _, err := s.result(t, "Stop"); !errors.Is(err, tt.cause) {
					t.Errorf("Stop error = %v, want %v", err, tt.cause)
				}
				w.sequence()
				res, err := goRecover(m, t.Context()).result(t, "Recover after the first Stop")
				if err != nil || res.State != proxyrecovery.Stopped {
					t.Errorf("Recover after the first Stop = %+v, %v; want stopped", res, err)
				}
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("Recover after the first Stop called %q", got)
				}
				h.release()
				if err := stop(t, m); err != nil {
					t.Errorf("the second Stop: %v", err)
				}
				if _, err := p.result(t, "the first Recover"); err != nil {
					t.Errorf("the first Recover: %v", err)
				}
				if diff := gocmp.Diff([]procinfo.Record{proxyRecord(900)}, w.stops()); diff != "" {
					t.Errorf("records stopped (-want +got):\n%s", diff)
				}
				if n := w.count("start"); n != 1 {
					t.Errorf("Start was called %d times", n)
				}
			})
		})
	}
}

// TestStopContextWhileStoppingOwnProxy decides Stop's own context in the steps Stop waits in for the proxy this monitor
// started: when the stop of that proxy or the check of its release honours its context and the context ends, Stop
// returns at once. Whether Stop reports the context's error or ignores the failed step is not decided; an error it
// returns must match the context's.
func TestStopContextWhileStoppingOwnProxy(t *testing.T) {
	tests := map[string]struct {
		step string
	}{
		"error: cancelled while the proxy this monitor started is stopped": {step: "stop"},
		"error: cancelled while the release of its port is checked":        {step: "release"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.answers(false)
				w.listens(false)
				m := newMonitor(t, w.options(true))
				tick(t, m)
				h := newHeldCall(t)
				w.set(func(b *behaviour) {
					switch tt.step {
					case "stop":
						b.stopOwned = func(ctx context.Context, _ procinfo.Record) error {
							h.until(ctx)
							return ctx.Err()
						}
					case "release":
						b.waitRelease = func(ctx context.Context) bool {
							h.until(ctx)
							return false
						}
					}
				})
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				s := goStop(m, ctx)
				await(t, h.in, "the "+tt.step+" of Stop")
				synctest.Wait()
				if s.finished() {
					t.Fatalf("Stop returned while its %s was held", tt.step)
				}
				cancel()
				synctest.Wait()
				if !s.finished() {
					t.Fatalf("Stop still waits in its %s after its context ended", tt.step)
				}
				if _, err := s.result(t, "Stop"); err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("Stop error = %v, want nil or context.Canceled", err)
				}
				if res, err := goRecover(m, t.Context()).result(t, "Recover after Stop"); err != nil || res.State != proxyrecovery.Stopped {
					t.Errorf("Recover after Stop = %+v, %v; want stopped", res, err)
				}
			})
		})
	}
}
