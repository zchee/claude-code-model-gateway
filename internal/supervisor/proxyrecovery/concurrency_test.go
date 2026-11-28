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

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// holdFirstProbe makes the first probe wait at g and answer false; every later probe answers later.
func (w *world) holdFirstProbe(g *gate, later bool) {
	var n int
	w.set(func(b *behaviour) {
		b.probe = func(ctx context.Context) bool {
			w.mu.Lock()
			n++
			first := n == 1
			w.mu.Unlock()
			if first {
				_ = g.hold(ctx)
				return false
			}
			return later
		}
	})
}

// TestSingleFlight decides that concurrent callers share one tick (lib/process-supervision.js:1083-1084, 1198): every
// caller gets the same whole result and the tick's work happens once; a caller that leaves changes nothing for the
// others, and when the last waiting caller leaves the tick goes on, on the monitor's lifetime and not the caller's, so
// that a caller who comes later joins it; the next call after the shared tick runs a new one.
func TestSingleFlight(t *testing.T) {
	tests := map[string]struct {
		callers int
		// leave is the index of a caller whose context is cancelled while the tick is held, or -1.
		leave int
		// rejoin attaches one more caller after the one that left.
		rejoin bool
		// refused makes the shared tick a refusal with a retry time instead of a recovery.
		refused bool
	}{
		"success: two callers share one tick":                            {callers: 2, leave: -1},
		"success: five callers share one tick":                           {callers: 5, leave: -1},
		"success: a caller that joined and left changes nothing":         {callers: 2, leave: 1},
		"success: the first caller leaving does not end the shared tick": {callers: 3, leave: 0},
		"success: callers of a refused tick share its whole result":      {callers: 3, leave: -1, refused: true},
		"success: the last waiting caller leaving does not end the tick": {callers: 1, leave: 0, rejoin: true, refused: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				w.listens(false)
				if tt.refused {
					w.set(func(b *behaviour) { b.binary = func() bool { return false } })
				}
				g := newGate()
				t.Cleanup(g.open)
				w.holdFirstProbe(g, true)
				m := newMonitor(t, w.options(true))
				at := w.now()
				var calls []*pending[proxyrecovery.Result]
				var cancels []context.CancelFunc
				join := func() {
					ctx, cancel := context.WithCancel(t.Context())
					t.Cleanup(cancel)
					cancels = append(cancels, cancel)
					calls = append(calls, goRecover(m, ctx))
				}
				for i := range tt.callers {
					join()
					if i == 0 {
						reached(t, g, "the first probe")
					}
					synctest.Wait()
				}
				for i, c := range calls {
					if c.finished() {
						t.Errorf("caller %d returned while the shared tick was held", i)
					}
				}
				if tt.leave >= 0 {
					cancels[tt.leave]()
					synctest.Wait()
					if !calls[tt.leave].finished() {
						t.Fatalf("caller %d still waits after its context ended", tt.leave)
					}
					if _, err := calls[tt.leave].result(t, "the caller that left"); !errors.Is(err, context.Canceled) {
						t.Errorf("the caller that left got error %v, want context.Canceled", err)
					}
					for i, c := range calls {
						if i != tt.leave && c.finished() {
							t.Errorf("caller %d returned when caller %d left", i, tt.leave)
						}
					}
					if ctx := g.heldContext(); ctx == nil || ctx.Err() != nil {
						t.Errorf("the held probe's context ended when caller %d left", tt.leave)
					}
				}
				if tt.rejoin {
					join()
					synctest.Wait()
					if calls[len(calls)-1].finished() {
						t.Error("a caller who came after the last one left returned while the tick was held")
					}
					if n := w.count("probe"); n != 1 {
						t.Errorf("a caller who came after the last one left started another tick: %d probes", n)
					}
				}
				g.open()
				var results []proxyrecovery.Result
				for i, c := range calls {
					if i == tt.leave {
						continue
					}
					res, err := c.result(t, "a sharing caller")
					if err != nil {
						t.Errorf("caller %d: %v", i, err)
					}
					results = append(results, res)
				}
				want := []string{
					"probe", "listening", "record:proxy-recovery-started", "binary", "listening", "start",
					"record:proxy-started", "probe", "record:proxy-recovery-finished",
				}
				if tt.refused {
					want = []string{"probe", "listening", "record:proxy-recovery-started", "binary", "record:proxy-recovery-finished"}
					wantResult := proxyrecovery.Result{State: proxyrecovery.BinaryMissing, RetryAt: at.Add(time.Second)}
					for i, res := range results {
						if diff := gocmp.Diff(wantResult, res); diff != "" {
							t.Errorf("result of sharing caller %d (-want +got):\n%s", i, diff)
						}
					}
				} else if results[0].State != proxyrecovery.Recovered {
					t.Errorf("the shared result = %+v, want recovered", results[0])
				}
				for i, res := range results[1:] {
					if diff := gocmp.Diff(results[0], res); diff != "" {
						t.Errorf("result of sharing caller %d differs from the first's (-first +this):\n%s", i+1, diff)
					}
				}
				if diff := gocmp.Diff(want, w.sequence()); diff != "" {
					t.Errorf("calls of the shared tick (-want +got):\n%s", diff)
				}
				if !tt.refused {
					if res := tick(t, m); res.State != proxyrecovery.Healthy {
						t.Errorf("the next call = %+v, want a new tick that is healthy", res)
					}
					if diff := gocmp.Diff([]string{"probe"}, w.sequence()); diff != "" {
						t.Errorf("calls of the next tick (-want +got):\n%s", diff)
					}
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}

// heldAt returns behaviour that holds the named step at g, and the configuration of a tick that reaches every step:
// a live, unhealthy listener whose holder this root's record names, then a start, then an unanswering proxy. The held
// call parks: when its context ends it keeps waiting until the test opens g, and then answers as a call that failed;
// a call that is let go before answers what keeps the tick going.
func heldAt(w *world, step string, g *gate) {
	w.listens(true)
	w.owner(ownership.Owner{Class: ownership.Ours, PID: 4242}, nil)
	w.place(proxyRecord(4242))
	var probes, samples int
	w.set(func(b *behaviour) {
		b.probe = func(ctx context.Context) bool {
			w.mu.Lock()
			probes++
			n := probes
			w.mu.Unlock()
			if step == "the first probe" && n == 1 || step == "the second probe" && n == 2 || step == "the probe after the start" && n == 3 {
				_ = g.park(ctx)
			}
			return false
		}
		b.listening = func(ctx context.Context) (bool, error) {
			w.mu.Lock()
			samples++
			n := samples
			w.mu.Unlock()
			if step == "the first listener sample" && n == 1 || step == "the second listener sample" && n == 2 {
				if err := g.park(ctx); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		if step == "the owner lookup" {
			b.resolve = func(ctx context.Context) (ownership.Owner, error) {
				if err := g.park(ctx); err != nil {
					return ownership.Owner{Class: ownership.Unknown}, err
				}
				return ownership.Owner{Class: ownership.Ours, PID: 4242}, nil
			}
		}
		if step == "the record read" {
			b.recorded = func(ctx context.Context, pid int) (procinfo.Record, error) {
				if err := g.park(ctx); err != nil {
					return procinfo.Record{}, err
				}
				return proxyRecord(pid), nil
			}
		}
		if step == "the stop of the holder" {
			b.stopOwned = func(ctx context.Context, r procinfo.Record) error {
				if r.PID == 4242 {
					if err := g.park(ctx); err != nil {
						return err
					}
				}
				w.exit(r.PID)
				return nil
			}
		}
		if step == "the release check" {
			var checks int
			b.waitRelease = func(ctx context.Context) bool {
				w.mu.Lock()
				checks++
				first := checks == 1
				w.mu.Unlock()
				return !first || g.park(ctx) == nil
			}
		}
		if step == "the start" {
			b.start = func(ctx context.Context) (proxyrecovery.Child, error) {
				if err := g.park(ctx); err != nil {
					return proxyrecovery.Child{}, err
				}
				return w.spawn(), nil
			}
		}
	})
}

// TestCallerCancellation decides that a caller whose context ends gets its error at once, at every step a tick can
// wait in, and that nothing is stopped or started on an answer that came back because a context ended. The tick is
// then ended by Stop, which waits for the held call to return before it returns; the cases do not decide which
// context a collaborator received.
func TestCallerCancellation(t *testing.T) {
	steps := []string{
		"the first probe", "the first listener sample", "the second probe", "the second listener sample", "the owner lookup",
		"the record read", "the stop of the holder", "the release check", "the start", "the probe after the start",
	}
	tests := map[string]struct{ step string }{}
	for _, s := range steps {
		tests["success: cancelled during "+s] = struct{ step string }{s}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				g := newGate()
				t.Cleanup(g.open)
				heldAt(w, tt.step, g)
				m := newMonitor(t, w.options(true))
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				p := goRecover(m, ctx)
				reached(t, g, tt.step)
				synctest.Wait()
				if p.finished() {
					t.Fatalf("Recover returned before its context ended")
				}
				startsBefore, stopsBefore := w.count("start"), w.count("stop-owned")
				cancel()
				synctest.Wait()
				if !p.finished() {
					t.Fatalf("Recover still waits in %s after its context ended", tt.step)
				}
				if _, err := p.result(t, "Recover"); !errors.Is(err, context.Canceled) {
					t.Errorf("Recover error = %v, want context.Canceled", err)
				}
				if n := w.count("start") - startsBefore; n != 0 {
					t.Errorf("%d proxies were started after the context ended", n)
				}
				if n := w.count("stop-owned") - stopsBefore; n != 0 {
					t.Errorf("%d stops were made after the context ended", n)
				}

				// Stop ends whatever is left of the tick. It waits for the held call, which has not returned, and no proxy
				// is started after it; a proxy whose start was under way when the context ended is the monitor's own and
				// is stopped.
				s := goStop(m, t.Context())
				synctest.Wait()
				if s.finished() {
					t.Errorf("Stop returned while the tick was still held in %s", tt.step)
				}
				g.open()
				if _, err := s.result(t, "Stop"); err != nil {
					t.Errorf("Stop: %v", err)
				}
				if n := w.count("start") - startsBefore; n != 0 {
					t.Errorf("%d proxies were started after Stop", n)
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

// TestEndedContext decides that a context which has already ended starts no tick: the caller gets its error and
// nothing is asked.
func TestEndedContext(t *testing.T) {
	tests := map[string]struct {
		cause error
	}{
		"error: a cancelled context":         {cause: context.Canceled},
		"error: a context past its deadline": {cause: context.DeadlineExceeded},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bubble(t, func(t *testing.T) {
				w := newWorld(t)
				m := newMonitor(t, w.options(true))
				var ctx context.Context
				var cancel context.CancelFunc
				if errors.Is(tt.cause, context.DeadlineExceeded) {
					ctx, cancel = context.WithDeadline(t.Context(), time.Now())
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				cancel()
				<-ctx.Done()
				_, err := goRecover(m, ctx).result(t, "Recover")
				if !errors.Is(err, tt.cause) {
					t.Errorf("Recover error = %v, want %v", err, tt.cause)
				}
				synctest.Wait()
				if got := w.sequence(); len(got) != 0 {
					t.Errorf("an ended context still called %q", got)
				}
				if err := stop(t, m); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
		})
	}
}
