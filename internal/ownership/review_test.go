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

package ownership_test

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

const (
	// unrelatedName is a record of the same state root that the stop under test does not name.
	unrelatedName = "unrelated"
	// busyWatchdog only turns a call that waits on a held exclusion into a failure. It is well past the source's
	// fifteen-second lifecycle poll (lib/lifecycle-lock.js:65-73), so that no bounded wait comes near it.
	busyWatchdog = 45 * time.Second
	// busyWithin is the longest a refusal over a held exclusion may take without a caller deadline: the default probe
	// budget with a margin for a loaded host.
	busyWithin = 10 * time.Second
	// defaultProbeMS is the ownership probe budget of a context without a deadline on POSIX
	// (lib/process-supervision.js:349-354); the timeout refusal names it.
	defaultProbeMS = 2000
	// impossiblePID is above every kernel's process number limit, so that a signal delivered natively by mistake
	// reaches no process of the host the tests run on.
	impossiblePID = 1 << 30
)

// outcome is what a call returned, or the value it panicked with.
type outcome struct {
	err      error
	panicked any
}

// capture runs call and returns its error or its panic, for goroutines in which t.Fatal must not be called and for
// cases that must tell a panic apart from an error.
func capture(call func() error) (o outcome) {
	defer func() { o.panicked = recover() }()
	o.err = call()
	return o
}

// removal is what a parked RemoveIf returned once it was let go.
type removal struct {
	removed bool
	outcome
}

// holder is a RemoveIf held after its match and before its unlink, while the store's exclusion is held.
type holder struct {
	// held is closed once the remover is in its hook.
	held <-chan struct{}
	// result yields what RemoveIf returned once it was let go, or that it returned without reaching the hook.
	result <-chan removal
	// finished is closed once the remover's goroutine ended.
	finished <-chan struct{}
	// release lets the remover go; it may be called any number of times.
	release func()
}

// startHolder starts RemoveIf(name, expected) on a copy of s whose hook holds it, without waiting for it. It never
// fails the test, so that it may be started from a callback on another goroutine.
func startHolder(s ownership.Store, name string, expected procinfo.Record) holder {
	held, let := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hooked := ownership.WithBeforeRemove(s, func() {
		once.Do(func() { close(held) })
		<-let
	})
	done, finished := make(chan removal, 1), make(chan struct{})
	go func() {
		defer close(finished)
		var r removal
		r.outcome = capture(func() (err error) {
			r.removed, err = hooked.RemoveIf(name, expected)
			return err
		})
		done <- r
	}()
	return holder{held: held, result: done, finished: finished, release: sync.OnceFunc(func() { close(let) })}
}

// parkRemover starts RemoveIf(name, expected) on a copy of s whose hook holds it after its match and before its
// unlink, while the store's exclusion is held. It returns once the remover is held, a function that lets it go, and
// the remover's result. The remover is always let go and joined when the test ends.
func parkRemover(t *testing.T, s ownership.Store, name string, expected procinfo.Record) (release func(), result <-chan removal) {
	t.Helper()
	h := startHolder(s, name, expected)
	t.Cleanup(func() {
		h.release()
		<-h.finished
	})
	select {
	case <-h.held:
	case r := <-h.result:
		t.Fatalf("RemoveIf(%s) returned %v, %v (panic: %v) without reaching the hook between its match and its unlink", name, r.removed, r.err, r.panicked)
	}
	return h.release, h.result
}

// TestStopRefusesWhileTheStoreIsBusy decides that a stop which cannot take the store's exclusion, because another
// operation of the same state root holds it, neither waits without bound nor signals on a decision the record may no
// longer support: it refuses with an error that claims neither a lost ownership nor a surviving process, keeps the
// record, and a later stop, once the holder is gone, succeeds. Without a caller deadline the refusal comes within the
// default budget plus a margin for a loaded host, which rejects a wait bounded only by something far longer.
func TestStopRefusesWhileTheStoreIsBusy(t *testing.T) {
	tests := map[string]struct {
		// deadline, when positive, is the caller's own.
		deadline time.Duration
		// within, when positive, is the longest the refusal may take.
		within time.Duration
	}{
		"error: a held exclusion with a caller deadline refuses without a signal":          {deadline: 2 * time.Second},
		"error: a held exclusion without a caller deadline still refuses without a signal": {within: busyWithin},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := stateRoot(t)
			h := newHost(workerSample(workerPID), workerSample(decoyPID))
			h.stamp = true
			h.ports = map[uint16]int{workerPort: -1}
			expected := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
			seed(t, root, workerName, expected)
			unrelated := recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
			unrelated.Generation = "unrelated-generation"
			seed(t, root, unrelatedName, unrelated)
			before := readRaw(t, root, workerName)
			release, holder := parkRemover(t, ownership.Store{Root: root}, unrelatedName, unrelated)
			ctx := t.Context()
			if tt.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.deadline)
				defer cancel()
			}
			a := authorityOn(root, h)
			done := make(chan outcome, 1)
			started := time.Now()
			go func() { done <- capture(func() error { return a.Stop(ctx, workerName, expected) }) }()
			var got outcome
			waited := false
			select {
			case got = <-done:
			case <-time.After(busyWatchdog):
				waited = true
				release()
				got = <-done
			}
			elapsed := time.Since(started)
			t.Logf("the refused stop took %v; its evidence calls: %s", elapsed, describe(h.snapshot()))
			if tt.within > 0 && elapsed > tt.within {
				t.Errorf("Stop refused after %v; want within %v (the default budget is %d ms)", elapsed, tt.within, defaultProbeMS)
			}
			if got.panicked != nil {
				t.Fatalf("Stop panicked: %v", got.panicked)
			}
			if waited {
				t.Errorf("Stop was still waiting %v after it started while the store's exclusion was held; it returned %v only once the holder was released", busyWatchdog, got.err)
			} else {
				select {
				case r := <-holder:
					t.Fatalf("the holder of the exclusion returned (%v, %v, panic %v) before it was released", r.removed, r.err, r.panicked)
				default:
				}
			}
			switch {
			case got.err == nil:
				t.Error("Stop succeeded while the store's exclusion was held")
			case errors.Is(got.err, ownership.ErrNotOwned), errors.Is(got.err, ownership.ErrStillRunning):
				t.Errorf("Stop = %v; a busy store neither disproves ownership nor confirms a survivor, so it must match neither ErrNotOwned nor ErrStillRunning", got.err)
			}
			if signals := h.signals(); len(signals) != 0 {
				t.Errorf("Stop sent %v while the store's exclusion was held", signals)
			}
			if diff := gocmp.Diff(before, readRaw(t, root, workerName)); diff != "" {
				t.Errorf("the stored record files changed (-before +after):\n%s", diff)
			}
			release()
			if r := <-holder; r.panicked != nil || r.err != nil || !r.removed {
				t.Errorf("the holder's RemoveIf of the unrelated record = %v, %v (panic: %v); want it removed", r.removed, r.err, r.panicked)
			}
			if err := stop(t, a, t.Context(), workerName, expected); err != nil {
				t.Fatalf("Stop once the holder was released: %v\nevents: %s", err, describe(h.snapshot()))
			}
			if diff := gocmp.Diff([]sent{{workerPID, syscall.SIGTERM}}, h.signals()); diff != "" {
				t.Errorf("signals of the stop after the release (-want +got):\n%s", diff)
			}
			checkAbsent(t, root, workerName)
		})
	}
}

// TestResolveDefaultBudget decides that a caller context without a deadline still gets the default probe budget, and
// that evidence arriving after that budget ended classifies nothing, even when it would have classified the holder as
// ours or as another install's: the result is the timeout refusal with the observed process number.
func TestResolveDefaultBudget(t *testing.T) {
	late := defaultProbeMS*time.Millisecond + time.Millisecond
	tests := map[string]struct {
		// prepare arranges the late evidence and returns a check that the arrangement was used, or nil.
		prepare func(t *testing.T, root string, h *fakeHost, a *ownership.Authority) (used func() bool)
		wantPID int
	}{
		"error: an absent listener reported after the default budget": {
			prepare: func(t *testing.T, _ string, h *fakeHost, _ *ownership.Authority) func() bool {
				h.listener = []listenerAnswer{listenerState(procinfo.ListenerAbsent)}
				h.listenerDelay = late
				return nil
			},
		},
		"error: a matching sample of a recorded holder that arrives after the default budget is not ours": {
			prepare: func(t *testing.T, root string, h *fakeHost, _ *ownership.Authority) func() bool {
				seed(t, root, recordName, recordFor(root, supervisorSample(holderPID), roleSupervisor, publicPort))
				h.identityDelay = late
				return nil
			},
			wantPID: holderPID,
		},
		"error: a reading of another install's Node gateway that arrives after the default budget is not foreign": {
			prepare: func(t *testing.T, _ string, h *fakeHost, a *ownership.Authority) func() bool {
				h.processes[holderPID].id.Argv = nodeCommand(otherInstall + "/bin/model-gateway.js")
				var called atomic.Bool
				a.Legacy = func(argv []string) (string, string, bool) {
					called.Store(true)
					time.Sleep(late)
					return otherInstall, thisInstall, false
				}
				return called.Load
			},
			wantPID: holderPID,
		},
		"error: a reading of this install's Node gateway that arrives after the default budget is not legacy": {
			prepare: func(t *testing.T, _ string, h *fakeHost, a *ownership.Authority) func() bool {
				h.processes[holderPID].id.Argv = nodeCommand(thisInstall + "/bin/model-gateway.js")
				var called atomic.Bool
				a.Legacy = func(argv []string) (string, string, bool) {
					called.Store(true)
					time.Sleep(late)
					return thisInstall, thisInstall, true
				}
				return called.Load
			},
			wantPID: holderPID,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := stateRoot(t)
				h := newHost(supervisorSample(holderPID))
				h.listener = []listenerAnswer{identified(holderPID)}
				a := authorityOn(root, h)
				used := tt.prepare(t, root, h, &a)
				got, err := resolve(t, a, t.Context(), recordName, publicPort)
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if used != nil && !used() {
					t.Fatalf("Resolve never asked for the late evidence; owner %+v, events: %s", got, describe(h.snapshot()))
				}
				if diff := gocmp.Diff(owner{ownership.Unknown, tt.wantPID}, owner{got.Class, got.PID}); diff != "" {
					t.Errorf("owner (-want +got):\n%s\nevents: %s", diff, describe(h.snapshot()))
				}
				if diff := gocmp.Diff(timeoutReason(publicPort, tt.wantPID, defaultProbeMS), got.Reason); diff != "" {
					t.Errorf("refusal text (-want +got):\n%s", diff)
				}
				if n := h.count("signal"); n != 0 {
					t.Errorf("Resolve sent %d signals", n)
				}
			})
		})
	}
}

// TestStoreRemovalKeepsAReplacedDirectory decides that a removal held between its match and its unlink, while the
// state directory is renamed away and a new one holding a successor record takes its path, unlinks nothing of the
// successor: the decision was made in the directory that is gone, so resolving the path again for the unlink would
// remove a record that was never matched. The store keeps working on the new directory afterwards.
func TestStoreRemovalKeepsAReplacedDirectory(t *testing.T) {
	root := stateRoot(t)
	s := ownership.Store{Root: root}
	old, successor := raceGenerations(root)
	seed(t, root, workerName, old)
	release, removed := parkRemover(t, s, workerName, old)
	if err := os.Rename(root, root+".replaced"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	seed(t, root, workerName, successor)
	want := readRaw(t, root, workerName)
	release()
	r := <-removed
	if r.panicked != nil {
		t.Fatalf("RemoveIf panicked: %v", r.panicked)
	}
	// Whether the removal reports the record of the directory that is gone as removed is not decided here.
	t.Logf("the held RemoveIf returned %v, %v", r.removed, r.err)
	if diff := gocmp.Diff(want, readRaw(t, root, workerName)); diff != "" {
		t.Errorf("the successor's files at the state root changed (-before the release +after):\n%s", diff)
	}
	checkStored(t, root, workerName, successor)
	next := successor
	next.Generation = "generation-after-the-replacement"
	if err := publish(t, s, workerName, next); err != nil {
		t.Fatalf("Publish on the new directory: %v", err)
	}
	checkStored(t, root, workerName, next)
	got, err := read(t, s, workerName)
	if err != nil {
		t.Fatalf("Read on the new directory: %v", err)
	}
	if diff := gocmp.Diff(next, got); diff != "" {
		t.Errorf("Read on the new directory (-want +got):\n%s", diff)
	}
}

// afterKill is the simulated host of a process whose evidence changes once it has been killed: its identity becomes
// unreadable while it still exists, or the caller's context ends during the first identity sample after the kill.
type afterKill struct {
	*fakeHost
	// hidden makes every identity sample after the kill inconclusive while the process stays listed and alive.
	hidden bool
	// cancel, when set, ends the caller's context during the first identity sample after the kill.
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (p *afterKill) killed() bool {
	for _, e := range p.snapshot() {
		if e.kind == "signal" && e.signal == syscall.SIGKILL {
			return true
		}
	}
	return false
}

// Identity answers as the native probe does when its context ends during a sample: inconclusive, with the reason.
func (p *afterKill) Identity(ctx context.Context, pid int) (procinfo.Identity, error) {
	if p.cancel != nil && pid == workerPID && p.killed() && p.fired.CompareAndSwap(false, true) {
		p.cancel()
	}
	id, err := p.fakeHost.Identity(ctx, pid)
	if err == nil && ctx.Err() != nil {
		return procinfo.Identity{}, fmt.Errorf("%w: %w", procinfo.ErrUnknown, ctx.Err())
	}
	return id, err
}

func (p *afterKill) signal(ctx context.Context, id procinfo.Identity, sig os.Signal) error {
	err := p.fakeHost.signal(ctx, id, sig)
	if sig == syscall.SIGKILL && p.hidden {
		p.mu.Lock()
		p.processes[id.PID].hiddenArgv = true
		p.mu.Unlock()
	}
	return err
}

// TestStopOutcomeAfterTheKill decides the result once the kill has been sent on proven ownership: an exit that can
// be neither confirmed nor refuted is neither ErrNotOwned nor ErrStillRunning and keeps the record; a context that
// ends while the exit is being confirmed is that context's end, not a survivor; a readable survivor of the same
// lifetime is ErrStillRunning and a confirmed exit is a stop.
func TestStopOutcomeAfterTheKill(t *testing.T) {
	tests := map[string]struct {
		survivesKill, hidden, cancel bool
		// wantErr is the error the result must match; neither asks for an error matching neither sentinel.
		wantErr error
		neither bool
	}{
		"error: an exit after the kill that stays unreadable is neither unowned nor still running": {
			survivesKill: true, hidden: true, neither: true,
		},
		"error: a context that ends while the exit after the kill is confirmed is that context's end": {
			survivesKill: true, cancel: true, wantErr: context.Canceled,
		},
		"error: a readable survivor of the same lifetime after the kill is still running": {
			survivesKill: true, wantErr: ownership.ErrStillRunning,
		},
		"success: a process that exits on the kill is stopped": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := stateRoot(t)
				h := newHost(workerSample(workerPID), workerSample(decoyPID))
				h.stamp = true
				h.ports = map[uint16]int{workerPort: -1}
				h.processes[workerPID].survives[syscall.SIGTERM] = true
				h.processes[workerPID].survives[syscall.SIGKILL] = tt.survivesKill
				expected := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
				seed(t, root, workerName, expected)
				before := readRaw(t, root, workerName)
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx, stopCancel := context.WithTimeout(parent, stopHorizon)
				defer stopCancel()
				p := &afterKill{fakeHost: h, hidden: tt.hidden}
				if tt.cancel {
					p.cancel = cancel
				}
				a := ownership.Authority{Records: ownership.Store{Root: root}, Processes: p, Signal: p.signal, Wait: timerWait, Legacy: noLegacy}
				err := stop(t, a, ctx, workerName, expected)
				events := h.snapshot()
				checkSamples(t, events)
				if diff := gocmp.Diff([]sent{{workerPID, syscall.SIGTERM}, {workerPID, syscall.SIGKILL}}, h.signals()); diff != "" {
					t.Errorf("signals (-want +got):\n%s\nevents: %s", diff, describe(events))
				}
				if tt.cancel && !p.fired.Load() {
					t.Errorf("no identity sample followed the kill, so the context was never ended; Stop = %v\nevents: %s", err, describe(events))
				}
				switch {
				case tt.neither:
					if err == nil || errors.Is(err, ownership.ErrNotOwned) || errors.Is(err, ownership.ErrStillRunning) {
						t.Errorf("Stop = %v; want an error matching neither ErrNotOwned nor ErrStillRunning\nevents: %s", err, describe(events))
					}
				case tt.wantErr != nil:
					if !errors.Is(err, tt.wantErr) {
						t.Errorf("Stop = %v; want an error matching %v\nevents: %s", err, tt.wantErr, describe(events))
					}
					if !errors.Is(tt.wantErr, ownership.ErrStillRunning) && errors.Is(err, ownership.ErrStillRunning) {
						t.Errorf("Stop = %v claims a confirmed survivor although the confirmation never finished", err)
					}
				default:
					if err != nil {
						t.Errorf("Stop = %v; want a confirmed stop\nevents: %s", err, describe(events))
					}
				}
				if tt.survivesKill {
					if diff := gocmp.Diff(before, readRaw(t, root, workerName)); diff != "" {
						t.Errorf("the record of a process whose exit was not confirmed changed (-before +after):\n%s", diff)
					}
				} else {
					checkAbsent(t, root, workerName)
				}
			})
		})
	}
}

// TestStoreUsableAfterAPanic decides that a panic inside the store's exclusion, from the authority's signal function
// or from the removal hook, or a signal function that ends its goroutine with runtime.Goexit, releases the exclusion:
// afterwards the same state root's publication, read and removal complete promptly. It rejects an unlock that is not
// deferred, which a panic or a goroutine exit skips.
func TestStoreUsableAfterAPanic(t *testing.T) {
	// Each case prepares root on the test goroutine and returns the call that reaches the panicking function.
	tests := map[string]func(t *testing.T, ctx context.Context, root string, called *atomic.Bool) func() error{
		"success: a signal function that panics leaves the store usable": func(t *testing.T, ctx context.Context, root string, called *atomic.Bool) func() error {
			h := newHost(workerSample(workerPID), workerSample(decoyPID))
			h.ports = map[uint16]int{workerPort: -1}
			expected := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
			seed(t, root, workerName, expected)
			a := authorityOn(root, h)
			a.Signal = func(context.Context, procinfo.Identity, os.Signal) error {
				called.Store(true)
				panic("the signal function panicked")
			}
			return func() error { return a.Stop(ctx, workerName, expected) }
		},
		"success: a signal function that ends its goroutine leaves the store usable": func(t *testing.T, ctx context.Context, root string, called *atomic.Bool) func() error {
			h := newHost(workerSample(workerPID), workerSample(decoyPID))
			h.ports = map[uint16]int{workerPort: -1}
			expected := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
			seed(t, root, workerName, expected)
			a := authorityOn(root, h)
			a.Signal = func(context.Context, procinfo.Identity, os.Signal) error {
				called.Store(true)
				runtime.Goexit()
				return nil
			}
			return func() error { return a.Stop(ctx, workerName, expected) }
		},
		"success: a removal hook that panics leaves the store usable": func(t *testing.T, _ context.Context, root string, called *atomic.Bool) func() error {
			old, _ := raceGenerations(root)
			seed(t, root, workerName, old)
			hooked := ownership.WithBeforeRemove(ownership.Store{Root: root}, func() {
				called.Store(true)
				panic("the removal hook panicked")
			})
			return func() error {
				_, err := hooked.RemoveIf(workerName, old)
				return err
			}
		},
	}
	for name, prepare := range tests {
		t.Run(name, func(t *testing.T) {
			root := stateRoot(t)
			ctx, cancel := context.WithTimeout(t.Context(), busyWatchdog)
			defer cancel()
			var called atomic.Bool
			call := prepare(t, ctx, root, &called)
			// The call runs on a goroutine of its own, so that a goroutine exit ends only that goroutine; returned tells
			// a return from such an exit.
			done := make(chan struct{})
			var got outcome
			returned := false
			go func() {
				defer close(done)
				got = capture(call)
				returned = true
			}()
			select {
			case <-done:
			case <-time.After(busyWatchdog):
				t.Fatalf("the call with the panicking function did not end within %v", busyWatchdog)
			}
			if !called.Load() {
				t.Fatalf("the panicking function was never called; the call returned %v (returned: %v) and panicked with %v", got.err, returned, got.panicked)
			}
			t.Logf("the call with the panicking function returned %v (returned: %v) and panicked with %v", got.err, returned, got.panicked)
			checkStoreUsable(t, root)
		})
	}
}

// checkStoreUsable publishes, reads and removes a record of root on another goroutine and fails unless all three
// succeed within the watchdog.
func checkStoreUsable(t *testing.T, root string) {
	t.Helper()
	s := ownership.Store{Root: root}
	fresh := recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
	fresh.Generation = "generation-after-the-panic"
	type result struct {
		published, readErr, removeErr error
		read                          procinfo.Record
		removed                       bool
		panicked                      any
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			r.panicked = recover()
			done <- r
		}()
		r.published = s.Publish(workerName, fresh)
		r.read, r.readErr = s.Read(workerName)
		r.removed, r.removeErr = s.RemoveIf(workerName, fresh)
	}()
	var r result
	select {
	case r = <-done:
	case <-time.After(busyWatchdog):
		t.Fatalf("publish, read and remove did not complete within %v; the panic left the store's exclusion held", busyWatchdog)
	}
	if r.panicked != nil {
		t.Fatalf("the store panicked after the earlier panic: %v", r.panicked)
	}
	if err := errors.Join(r.published, r.readErr, r.removeErr); err != nil {
		t.Fatalf("the store after the panic: publish %v, read %v, remove %v", r.published, r.readErr, r.removeErr)
	}
	if diff := gocmp.Diff(fresh, r.read); diff != "" {
		t.Errorf("Read after the panic (-want +got):\n%s", diff)
	}
	if !r.removed {
		t.Error("RemoveIf after the panic removed nothing")
	}
	checkAbsent(t, root, workerName)
}

const (
	// chunkedHead is the head of a chunked identity answer.
	chunkedHead = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"
	// endlessBound is the most a client may read of an answer whose framing never ends before it refuses it. It is
	// above the largest valid answer, a capped proof in one-byte chunks, so that no bounded reader comes near it.
	endlessBound = 16 << 20
)

// paddedProof is a valid proof answer body for the challenge of exactly size bytes, padded with insignificant white
// space before the closing brace.
func paddedProof(t *testing.T, challenge *http.Request, size int) string {
	t.Helper()
	body := ssProofBody(t, ssRequest{Header: challenge.Header}, ssGeneration)
	if size < len(body) {
		t.Fatalf("a proof body of %d bytes cannot be padded to %d", len(body), size)
	}
	padded := body[:len(body)-1] + strings.Repeat(" ", size-len(body)) + "}"
	if len(padded) != size {
		t.Fatalf("padded proof of %d bytes; want %d", len(padded), size)
	}
	return padded
}

// writeAsync writes s to the client from its own goroutine, since a client that refuses may stop reading, and
// returns a channel closed once the write ended. The connection is closed when the test ends.
func writeAsync(t *testing.T, peer *seamPeer, s string) <-chan struct{} {
	t.Helper()
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		_, _ = io.WriteString(peer.conn, s)
	}()
	t.Cleanup(func() {
		_ = peer.conn.Close()
		<-wrote
	})
	return wrote
}

// peerDrain ends every listener-side connection of the seam when a bubble's function returns, on any path: the ones
// the test received and any still waiting in peers. Each is closed and whatever its client wrote is read, since the
// seam's copy goroutine blocks until its capture is read and would otherwise stay blocked after the bubble's function
// returned, which synctest reports by aborting the whole test binary. synctest.Wait between rounds lets a connection
// that the client dials while the others end show up before the function returns.
type peerDrain struct {
	peers    <-chan *seamPeer
	received []*seamPeer
}

func (d *peerDrain) add(p *seamPeer) *seamPeer {
	d.received = append(d.received, p)
	return p
}

// end must be deferred on the bubble's own goroutine, so that it also runs after t.Fatal.
func (d *peerDrain) end() {
	for {
		for waiting := true; waiting; {
			select {
			case p := <-d.peers:
				d.received = append(d.received, p)
			default:
				waiting = false
			}
		}
		for _, p := range d.received {
			_ = p.conn.Close()
			_, _ = io.Copy(io.Discard, p.br)
			<-p.ended
		}
		d.received = nil
		synctest.Wait()
		if len(d.peers) == 0 {
			return
		}
	}
}

// answerHealth expects the diagnostic health request on the next connection and answers it with a process number.
func answerHealth(t *testing.T, d *peerDrain, peers <-chan *seamPeer, done <-chan seamResult) {
	t.Helper()
	health := d.add(seamPeerOrResult(t, peers, done))
	t.Cleanup(func() { _ = health.conn.Close() })
	if req := health.next(t); req.Method != http.MethodGet || req.URL.Path != "/healthz" {
		t.Fatalf("diagnostic request = %s %s; want GET /healthz", req.Method, req.URL.Path)
	}
	health.write(t, ssAnswer("200 OK", "application/json", fmt.Sprintf(`{"ok":true,"supervisorPid":%d}`, ssHealthPID)))
}

// TestStopSelfOneByteChunks decides the cap on the decoded proof, not on its framing: a valid proof of exactly the cap
// in one-byte chunks is accepted and the stop follows on the same connection, and one byte more in the same framing
// is refused without a stop; either way the client closes the control connection. It rejects a cap on the framing, an
// off-by-one cap, and a client that leaves the connection open.
func TestStopSelfOneByteChunks(t *testing.T) {
	tests := map[string]struct {
		size  int
		valid bool
	}{
		"success: a valid proof of exactly the cap in one-byte chunks is accepted": {size: ssProofCap, valid: true},
		"error: a valid proof one byte over the cap in one-byte chunks is refused": {size: ssProofCap + 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peers, done := seamRun(t, t.Context())
				d := &peerDrain{peers: peers}
				defer d.end()
				peer := d.add(seamPeerOrResult(t, peers, done))
				challenge := peer.next(t)
				wrote := writeAsync(t, peer, chunkedHead+ssChunks(paddedProof(t, challenge, tt.size), 1))
				if tt.valid {
					stop := peer.next(t)
					if stop.Method != http.MethodPost || stop.RequestURI != "/internal/stop" || stop.Header.Get("X-Model-Gateway-Control-Token") != ssToken {
						t.Fatalf("after the proof the client sent %s %s", stop.Method, stop.RequestURI)
					}
					peer.write(t, ssAnswer("202 Accepted", "application/json", `{"ok":true,"stopping":true}`))
				} else {
					answerHealth(t, d, peers, done)
				}
				r := <-done
				switch {
				case tt.valid && r.err != nil:
					t.Errorf("StopSelf = %d, %v; want the stop acknowledged", r.pid, r.err)
				case !tt.valid && (!errors.Is(r.err, ownership.ErrUnproven) || r.pid != ssHealthPID):
					t.Errorf("StopSelf = %d, %v; want %d and an error matching ErrUnproven", r.pid, r.err, ssHealthPID)
				}
				synctest.Wait()
				// The listener's reader ends only when the client closed its end; the writer may also have ended
				// because the client merely stopped reading.
				select {
				case <-peer.ended:
				default:
					t.Error("the listener never saw the control connection end after StopSelf returned")
				}
				select {
				case <-wrote:
				default:
					t.Error("the control connection is still open after StopSelf returned")
					_ = peer.conn.Close()
					<-wrote
				}
				want := []string{"GET /internal/identity"}
				if tt.valid {
					want = append(want, "POST /internal/stop")
				} else if strings.Contains(strings.ToLower(peer.bytes()), ssToken) {
					t.Errorf("the token was written to an unproven listener")
				}
				if diff := gocmp.Diff(want, peer.requests(t)); diff != "" {
					t.Errorf("requests on the stop connection (-want +got):\n%s", diff)
				}
				select {
				case extra := <-peers:
					d.add(extra)
					t.Errorf("an unexpected connection was opened; it carried %q", extra.bytes())
				default:
				}
			})
		})
	}
}

// TestStopSelfRefusesEndlessFraming decides that chunked framing which never ends, whether it carries no data or one
// byte per long chunk header, is refused after a bounded number of bytes, with no stop sent and the health request on
// a connection of its own.
func TestStopSelfRefusesEndlessFraming(t *testing.T) {
	longExtension := "1;" + strings.Repeat("e", 1000) + "\r\nx\r\n"
	tests := map[string]struct {
		// prefix is written once and block repeated until the client stops reading or the bound is reached.
		prefix func(t *testing.T, challenge *http.Request) string
		block  string
	}{
		"error: one chunk extension that never ends": {
			prefix: func(*testing.T, *http.Request) string { return chunkedHead + "1;extension=" },
			block:  strings.Repeat("e", 64<<10),
		},
		"error: every chunk carrying one byte behind a long extension": {
			prefix: func(*testing.T, *http.Request) string { return chunkedHead },
			block:  strings.Repeat(longExtension, 64),
		},
		"error: a chunk size whose leading zeros never end": {
			prefix: func(*testing.T, *http.Request) string { return chunkedHead },
			block:  strings.Repeat("0", 64<<10),
		},
		"error: trailer fields after a valid proof's terminal chunk that never end": {
			prefix: func(t *testing.T, challenge *http.Request) string {
				body := ssProofBody(t, ssRequest{Header: challenge.Header}, ssGeneration)
				return chunkedHead + strings.TrimSuffix(ssChunks(body, 4096), "\r\n")
			},
			block: strings.Repeat("X-Pad: trailer\r\n", 4096),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				peers, done := seamRun(t, t.Context())
				d := &peerDrain{peers: peers}
				defer d.end()
				peer := d.add(seamPeerOrResult(t, peers, done))
				challenge := peer.next(t)
				prefix := tt.prefix(t, challenge)
				wrote := make(chan struct{})
				written, bounded := 0, false
				go func() {
					defer close(wrote)
					n, err := io.WriteString(peer.conn, prefix)
					written += n
					for err == nil && written < endlessBound {
						n, err = io.WriteString(peer.conn, tt.block)
						written += n
					}
					if err == nil {
						bounded = true
						_ = peer.conn.Close()
					}
				}()
				t.Cleanup(func() {
					_ = peer.conn.Close()
					<-wrote
				})
				answerHealth(t, d, peers, done)
				r := <-done
				if !errors.Is(r.err, ownership.ErrUnproven) || r.pid != ssHealthPID {
					t.Errorf("StopSelf = %d, %v; want %d and an error matching ErrUnproven", r.pid, r.err, ssHealthPID)
				}
				synctest.Wait()
				// The listener's reader ends only when the client closed its end; the writer may also have ended
				// because the client merely stopped reading.
				select {
				case <-peer.ended:
				default:
					t.Error("the listener never saw the control connection end after StopSelf returned")
				}
				select {
				case <-wrote:
				default:
					t.Error("the control connection is still open after StopSelf returned")
					_ = peer.conn.Close()
					<-wrote
				}
				if bounded {
					t.Errorf("the client read %d bytes of endless framing without refusing it; want fewer than %d", written, endlessBound)
				}
				if raw := peer.bytes(); strings.Contains(strings.ToLower(raw), ssToken) || strings.Contains(raw, "POST ") {
					t.Errorf("the client sent a stop or the token after endless framing: %q", raw)
				}
			})
		})
	}
}

// causeMarker is the text of the cause a caller gives for ending its context; no error may repeat it.
const causeMarker = "caller-cause-marker-7e2d"

// errCallerCause is the cause a caller gives for ending its context.
var errCallerCause = errors.New(causeMarker + ": the caller gave up")

// errCanceledCause and errDeadlineCause are causes that wrap the error their context reports, so that matching the
// context's error alone also matches the cause's chain but not the cause itself.
var (
	errCanceledCause = fmt.Errorf("%s: the caller withdrew: %w", causeMarker, context.Canceled)
	errDeadlineCause = fmt.Errorf("%s: the caller's time ran out: %w", causeMarker, context.DeadlineExceeded)
)

// checkEnded fails unless err reports why ctx ended: it matches ctx.Err(), context.Cause(ctx) and every error in
// want, it matches no error in exclude, and its text repeats neither the cause's text nor any of peerBytes. Both the
// error and the cause are asserted even when one wraps the other, so that a cause wrapping ctx.Err() is not excused.
func checkEnded(t *testing.T, ctx context.Context, err error, want, exclude []error, peerBytes ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("StopSelf succeeded after its context ended")
	}
	want = append(want, ctx.Err(), context.Cause(ctx))
	for _, w := range want {
		if !errors.Is(err, w) {
			t.Errorf("StopSelf = %v; want an error matching %v", err, w)
		}
	}
	for _, x := range exclude {
		if errors.Is(err, x) {
			t.Errorf("StopSelf = %v; it must not match %v here", err, x)
		}
	}
	for _, text := range append(peerBytes, causeMarker) {
		if strings.Contains(err.Error(), text) {
			t.Errorf("the error text %q repeats %q", err, text)
		}
	}
}

// TestStopSelfEndDuringTheDiagnostic decides that a caller's context ending while the diagnostic health document is
// awaited, after the listener failed the proof, keeps the refusal and reports why the context ended: ErrUnproven, not
// ErrStopUnacknowledged, with ctx.Err() and, when the caller gave one, the cause, never the cause's text and nothing
// the listener sent. It rejects a refusal that
// drops the cancellation or the cause from the error chain, and an error that copies the cause's or the peer's text.
func TestStopSelfEndDuringTheDiagnostic(t *testing.T) {
	const (
		proofMarker  = "proof-answer-marker-6b1f"
		healthMarker = "health-answer-marker-93ad"
	)
	tests := map[string]struct {
		// end ends the context once the client waits for the rest of the health document; nil leaves it to the
		// deadline.
		end func(cancel context.CancelCauseFunc)
		// deadline, when positive, is the caller's own, with cause when that is set.
		deadline time.Duration
		cause    error
	}{
		"error: a cancellation keeps the refusal and reports the cancellation": {
			end: func(cancel context.CancelCauseFunc) { cancel(nil) },
		},
		"error: a cancellation with a cause keeps the refusal and reports the cancellation and the cause": {
			end: func(cancel context.CancelCauseFunc) { cancel(errCallerCause) },
		},
		"error: a caller deadline keeps the refusal and reports the deadline": {
			deadline: 1500 * time.Millisecond,
		},
		"error: a caller deadline with a cause keeps the refusal and reports the deadline and the cause": {
			deadline: 1500 * time.Millisecond, cause: errCallerCause,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				if tt.deadline > 0 {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeoutCause(ctx, tt.deadline, tt.cause)
					defer stop()
				}
				peers, done := seamRun(t, ctx)
				d := &peerDrain{peers: peers}
				defer d.end()
				peer := d.add(seamPeerOrResult(t, peers, done))
				t.Cleanup(func() { _ = peer.conn.Close() })
				_ = peer.next(t)
				peer.write(t, ssAnswer("200 OK", "text/html", "<!doctype html><title>"+proofMarker+"</title>"))
				health := d.add(seamPeerOrResult(t, peers, done))
				t.Cleanup(func() { _ = health.conn.Close() })
				if req := health.next(t); req.URL.Path != "/healthz" {
					t.Fatalf("the second connection carried %s %s", req.Method, req.RequestURI)
				}
				health.write(t, "HTTP/1.1 200 OK\r\nX-Peer-Marker: "+healthMarker+"\r\nContent-Type: application/json\r\n")
				synctest.Wait()
				if tt.end != nil {
					select {
					case r := <-done:
						t.Fatalf("StopSelf returned %d, %v before its context ended", r.pid, r.err)
					default:
					}
					tt.end(cancel)
				}
				r := <-done
				checkEnded(t, ctx, r.err, []error{ownership.ErrUnproven}, []error{ownership.ErrStopUnacknowledged}, proofMarker, healthMarker, "X-Peer-Marker", "doctype")
				if r.pid != 0 {
					t.Errorf("health process number %d from an unfinished health document; want 0", r.pid)
				}
				synctest.Wait()
				select {
				case <-health.ended:
				default:
					t.Error("the client left the health connection open")
				}
			})
		})
	}
}

// TestStopSelfEndsWithACause decides, by where the caller's context ends, what the error reports next to ctx.Err()
// and the caller's cause: a context already ended when StopSelf is called opens nothing and matches neither
// ErrUnproven nor ErrStopUnacknowledged; an end while the challenge's answer is awaited or while the proof is being
// read is a refusal, ErrUnproven and not ErrStopUnacknowledged; an end while the acknowledgment of the sent stop is
// awaited is ErrStopUnacknowledged and not ErrUnproven. The error never repeats the cause's text or the listener's
// bytes. It rejects an error chain that keeps only the outcome, only ctx.Err() or only the cause, an outcome that
// names the wrong point, and an error that copies the cause's text.
func TestStopSelfEndsWithACause(t *testing.T) {
	unproven := []error{ownership.ErrUnproven}
	unacknowledged := []error{ownership.ErrStopUnacknowledged}
	tests := map[string]struct {
		// park says where the context ends: "entry", "challenge", "proof" or "stop".
		park string
		// deadline, when set, ends the context with the cause instead of a cancellation with it.
		deadline bool
		// cause is the caller's cause; nil means errCallerCause.
		cause         error
		want, exclude []error
	}{
		"error: a cancellation whose cause wraps context.Canceled while the challenge's answer is awaited": {
			park: "challenge", cause: errCanceledCause, want: unproven, exclude: unacknowledged,
		},
		"error: a deadline whose cause wraps context.DeadlineExceeded while the challenge's answer is awaited": {
			park: "challenge", deadline: true, cause: errDeadlineCause, want: unproven, exclude: unacknowledged,
		},
		"error: a context cancelled with a cause before the call": {
			park: "entry", exclude: append(append([]error(nil), unproven...), unacknowledged...),
		},
		"error: a context whose deadline passed with a cause before the call": {
			park: "entry", deadline: true, exclude: append(append([]error(nil), unproven...), unacknowledged...),
		},
		"error: a cancellation with a cause while the challenge's answer is awaited": {
			park: "challenge", want: unproven, exclude: unacknowledged,
		},
		"error: a deadline with a cause while the challenge's answer is awaited": {
			park: "challenge", deadline: true, want: unproven, exclude: unacknowledged,
		},
		"error: a cancellation with a cause while the proof is being read": {
			park: "proof", want: unproven, exclude: unacknowledged,
		},
		"error: a deadline with a cause while the proof is being read": {
			park: "proof", deadline: true, want: unproven, exclude: unacknowledged,
		},
		"error: a cancellation with a cause while the acknowledgment is awaited": {
			park: "stop", want: unacknowledged, exclude: unproven,
		},
		"error: a deadline with a cause while the acknowledgment is awaited": {
			park: "stop", deadline: true, want: unacknowledged, exclude: unproven,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				cause := tt.cause
				if cause == nil {
					cause = errCallerCause
				}
				end := func() { cancel(cause) }
				if tt.deadline {
					// The deadline is a second away and the clock only moves once every goroutine waits, so it passes
					// exactly where the case parks.
					var stop context.CancelFunc
					ctx, stop = context.WithTimeoutCause(ctx, time.Second, cause)
					defer stop()
					end = func() {}
				}
				if tt.park == "entry" {
					end()
					if tt.deadline {
						<-ctx.Done()
					}
				}
				peers, done := seamRun(t, ctx)
				d := &peerDrain{peers: peers}
				defer d.end()
				var peerBytes []string
				if tt.park != "entry" {
					peer := d.add(seamPeerOrResult(t, peers, done))
					t.Cleanup(func() { _ = peer.conn.Close() })
					challenge := peer.next(t)
					switch tt.park {
					case "proof":
						// The proof answer stops three bytes before its end, so the client is still reading it.
						head, _ := seamProof(t, challenge)
						peer.write(t, head)
						peerBytes = append(peerBytes, ssGeneration)
					case "stop":
						head, tail := seamProof(t, challenge)
						peer.write(t, head+tail)
						if req := peer.next(t); req.Method != http.MethodPost || req.RequestURI != "/internal/stop" {
							t.Fatalf("after the proof the client sent %s %s", req.Method, req.RequestURI)
						}
						peerBytes = append(peerBytes, ssGeneration)
					}
					synctest.Wait()
					select {
					case r := <-done:
						t.Fatalf("StopSelf returned %d, %v before its context ended", r.pid, r.err)
					default:
					}
					end()
				}
				r := <-done
				checkEnded(t, ctx, r.err, tt.want, tt.exclude, peerBytes...)
				if tt.park == "entry" {
					select {
					case p := <-peers:
						d.add(p)
						t.Errorf("a connection was opened with an ended context; it carried %q", p.bytes())
					default:
					}
				}
			})
		})
	}
}

// TestNodeGatewayStandaloneLetterCase decides how a Node gateway outside any plugin cache compares with the current
// root. The source compares the two physical roots after lowercasing them (normalizedPath,
// lib/process-supervision.js:507-509,537), as install.SameInstallRoot does; so a root that differs from the current
// one only in letter case is this install's Node gateway, while a root elsewhere stays another install.
func TestNodeGatewayStandaloneLetterCase(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(home, "dev", "model-gateway")
	recased := filepath.Join(home, "dev", "Model-Gateway")
	elsewhere := filepath.Join(home, "dev2", "Model-Gateway")
	for _, dir := range []string{current, recased, elsewhere} {
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bin", "model-gateway.js"), []byte("'use strict';\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	predicate := newNodeGateway(t, current, nil)
	tests := map[string]struct {
		root      string
		want      nodeReading
		wantClass ownership.Class
		reason    string
	}{
		"success: the current standalone root is this install": {
			root: current, want: nodeReading{current, current, true},
			wantClass: ownership.LegacyNode, reason: legacyReason(publicPort, holderPID),
		},
		"success: a standalone root that differs from the current one only in letter case is this install": {
			root: recased, want: nodeReading{recased, current, true},
			wantClass: ownership.LegacyNode, reason: legacyReason(publicPort, holderPID),
		},
		"error: a standalone root elsewhere is another install": {
			root: elsewhere, want: nodeReading{elsewhere, current, false},
			wantClass: ownership.Foreign, reason: foreignReason(publicPort, holderPID, elsewhere, current),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			argv := nodeCommand(tt.root + "/bin/model-gateway.js")
			if diff := gocmp.Diff(tt.want, readNode(t, predicate, argv)); diff != "" {
				t.Errorf("reading of %q (-want +got):\n%s", argv, diff)
			}
			h := newHost(sampleOf(holderPID, procinfo.Image{Path: "/usr/bin/node", Device: 1, Inode: 7}, argv...))
			h.listener = []listenerAnswer{identified(holderPID)}
			a := authorityOn(stateRoot(t), h)
			a.Legacy = predicate
			got, err := resolve(t, a, t.Context(), recordName, publicPort)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			type verdict struct {
				Class  ownership.Class
				PID    int
				Reason string
			}
			if diff := gocmp.Diff(verdict{tt.wantClass, holderPID, tt.reason}, verdict{got.Class, got.PID, got.Reason}); diff != "" {
				t.Errorf("owner (-want +got):\n%s", diff)
			}
		})
	}
}

// TestStopWithoutASignalFunctionOverSuppliedEvidence decides that an authority whose evidence comes from a
// caller-supplied source but which has no signal function refuses the stop before any signal: built-in delivery needs
// the built-in process handles, which supplied evidence does not come with. The recorded process number is one no
// kernel issues, so that a mistaken native delivery reaches no process.
func TestStopWithoutASignalFunctionOverSuppliedEvidence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := stateRoot(t)
		h := newHost(workerSample(impossiblePID))
		h.stamp = true
		h.ports = map[uint16]int{workerPort: -1}
		expected := recordFor(root, workerSample(impossiblePID), roleWorker, workerPort)
		seed(t, root, workerName, expected)
		before := readRaw(t, root, workerName)
		a := authorityOn(root, h)
		a.Signal = nil
		ctx, cancel := context.WithTimeout(t.Context(), stopHorizon)
		defer cancel()
		err := stop(t, a, ctx, workerName, expected)
		if err == nil || errors.Is(err, ownership.ErrNotOwned) || errors.Is(err, ownership.ErrStillRunning) {
			t.Errorf("Stop = %v; want a refusal matching neither ErrNotOwned nor ErrStillRunning\nevents: %s", err, describe(h.snapshot()))
		}
		if h.processes[impossiblePID].gone(time.Now()) {
			t.Error("the simulated process was ended")
		}
		if diff := gocmp.Diff(before, readRaw(t, root, workerName)); diff != "" {
			t.Errorf("the stored record files changed (-before +after):\n%s", diff)
		}
	})
}

// resolveBudget is the caller's budget of the Resolve cases that run on the real clock, since a goroutine blocked on
// the store's exclusion in a system call would stop a synctest clock.
const resolveBudget = time.Second

// TestResolveAfterAnExpiredStoreWait decides that once Resolve's budget has expired while its record read waits for
// the store's exclusion, it starts no further evidence call and does not consult the Legacy reading: the result is the
// timeout refusal with the process number observed so far. The control, with the store free, reads the same holder as
// another install's Node gateway. It rejects a Resolve that, after the wait, carries on with the stale budget and
// classifies the holder anyway. Evidence calls are timed when they are entered, and the refusal is judged by its form:
// the budget figure it names depends on the scheduling of the real clock and is decided by TestResolveBudget.
func TestResolveAfterAnExpiredStoreWait(t *testing.T) {
	tests := map[string]struct{ busy bool }{
		"error: a record read held past the caller's deadline consults nothing further": {busy: true},
		"success: with the store free the holder is read as another install's":          {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := stateRoot(t)
			h := newHost(sampleOf(holderPID, procinfo.Image{Path: "/usr/bin/node", Device: 1, Inode: 7}, nodeCommand(otherInstall+"/bin/model-gateway.js")...))
			h.listener = []listenerAnswer{identified(holderPID)}
			a := authorityOn(root, h)
			entered := &enteredHost{fakeHost: h}
			a.Processes = entered
			var legacyCalls atomic.Int32
			a.Legacy = func([]string) (string, string, bool) {
				legacyCalls.Add(1)
				return otherInstall, thisInstall, false
			}
			var holderResult <-chan removal
			if tt.busy {
				unrelated := recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
				seed(t, root, unrelatedName, unrelated)
				_, holderResult = parkRemover(t, ownership.Store{Root: root}, unrelatedName, unrelated)
			}
			ctx, cancel := context.WithTimeout(t.Context(), resolveBudget)
			defer cancel()
			deadline, _ := ctx.Deadline()
			type result struct {
				owner ownership.Owner
				outcome
			}
			done := make(chan result, 1)
			go func() {
				var r result
				r.outcome = capture(func() (err error) {
					r.owner, err = a.Resolve(ctx, recordName, publicPort)
					return err
				})
				done <- r
			}()
			var r result
			select {
			case r = <-done:
			case <-time.After(busyWatchdog):
				t.Fatalf("Resolve did not return within %v; events: %s", busyWatchdog, describe(h.snapshot()))
			}
			if r.panicked != nil {
				t.Fatalf("Resolve panicked: %v", r.panicked)
			}
			if r.err != nil {
				t.Fatalf("Resolve: %v", r.err)
			}
			events := h.snapshot()
			if !tt.busy {
				want := struct {
					Class  ownership.Class
					PID    int
					Reason string
				}{ownership.Foreign, holderPID, foreignReason(publicPort, holderPID, otherInstall, thisInstall)}
				if diff := gocmp.Diff(want, struct {
					Class  ownership.Class
					PID    int
					Reason string
				}{r.owner.Class, r.owner.PID, r.owner.Reason}); diff != "" {
					t.Errorf("owner with the store free (-want +got):\n%s\nevents: %s", diff, describe(events))
				}
				if legacyCalls.Load() == 0 {
					t.Error("the Legacy reading was never consulted with the store free")
				}
				return
			}
			select {
			case hr := <-holderResult:
				t.Fatalf("the holder of the exclusion returned (%v, %v, panic %v) before Resolve did; the read was not held", hr.removed, hr.err, hr.panicked)
			default:
			}
			// The process number observed so far is the listener's holder once the listener was queried.
			wantPID := 0
			for _, e := range events {
				if e.kind == "listener" {
					wantPID = holderPID
				}
			}
			for _, call := range entered.calls() {
				if call.at.After(deadline) {
					t.Errorf("an evidence call (%s) was entered %v after the budget expired", call.kind, call.at.Sub(deadline))
				}
			}
			if diff := gocmp.Diff(owner{ownership.Unknown, wantPID}, owner{r.owner.Class, r.owner.PID}); diff != "" {
				t.Errorf("owner (-want +got):\n%s\nevents: %s", diff, describe(events))
			}
			if !timeoutForm(r.owner.Reason, publicPort, wantPID) {
				t.Errorf("refusal %q; want the timeout refusal for :%d and PID %d, whatever budget it names", r.owner.Reason, publicPort, wantPID)
			}
			if n := legacyCalls.Load(); n != 0 {
				t.Errorf("the Legacy reading was consulted %d times after the budget expired; want 0", n)
			}
		})
	}
}

// enteredCall is one evidence call, timed when the authority entered it.
type enteredCall struct {
	kind string
	at   time.Time
}

// enteredHost times every evidence call when it is entered, before the simulated host takes its lock or answers.
type enteredHost struct {
	*fakeHost
	mu      sync.Mutex
	entries []enteredCall
}

func (p *enteredHost) enter(kind string) {
	at := time.Now()
	p.mu.Lock()
	p.entries = append(p.entries, enteredCall{kind, at})
	p.mu.Unlock()
}

func (p *enteredHost) calls() []enteredCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.entries)
}

func (p *enteredHost) Identity(ctx context.Context, pid int) (procinfo.Identity, error) {
	p.enter("identity")
	return p.fakeHost.Identity(ctx, pid)
}

func (p *enteredHost) Table(ctx context.Context) (map[int]procinfo.Identity, error) {
	p.enter("table")
	return p.fakeHost.Table(ctx)
}

func (p *enteredHost) Alive(ctx context.Context, pid int) (bool, error) {
	p.enter("alive")
	return p.fakeHost.Alive(ctx, pid)
}

func (p *enteredHost) ListenerEvidence(ctx context.Context, port uint16) (procinfo.Listener, error) {
	p.enter("listener")
	return p.fakeHost.ListenerEvidence(ctx, port)
}

// timeoutForm reports whether reason is the timeout refusal for port and pid naming some budget in milliseconds.
func timeoutForm(reason string, port uint16, pid int) bool {
	const figure = 987654321
	prefix, suffix, _ := strings.Cut(timeoutReason(port, pid, figure), strconv.Itoa(figure))
	middle, ok := strings.CutPrefix(reason, prefix)
	if !ok {
		return false
	}
	digits, ok := strings.CutSuffix(middle, suffix)
	if !ok || digits == "" {
		return false
	}
	_, err := strconv.ParseUint(digits, 10, 64)
	return err == nil
}

// goneWatch records whether the authority was told that the recorded process is gone, by an identity sample that
// found no process or by a liveness answer of false.
type goneWatch struct {
	*fakeHost
	gone atomic.Bool
}

func (p *goneWatch) Identity(ctx context.Context, pid int) (procinfo.Identity, error) {
	id, err := p.fakeHost.Identity(ctx, pid)
	if pid == workerPID && errors.Is(err, os.ErrNotExist) {
		p.gone.Store(true)
	}
	return id, err
}

func (p *goneWatch) Alive(ctx context.Context, pid int) (bool, error) {
	alive, err := p.fakeHost.Alive(ctx, pid)
	if pid == workerPID && err == nil && !alive {
		p.gone.Store(true)
	}
	return alive, err
}

// TestStopRetirementWhileTheStoreIsBusy decides the stop whose process is gone after the signal but whose record
// cannot be retired, because another operation holds the store's exclusion until the caller's deadline has passed:
// the error is the caller's context.DeadlineExceeded, matches neither ErrNotOwned nor ErrStillRunning, and the record
// files are exactly as they were, so that a later stop retires them. The process exits only once the holder is
// confirmed to hold the exclusion, and the stop must have been told that the process is gone before the outcome is
// judged, so that the deadline cannot end an exit wait instead of the retirement. It rejects a stop that reports the
// gone process as still running or as not owned, that claims success without retiring the record, or that removes the
// files without the exclusion.
func TestStopRetirementWhileTheStoreIsBusy(t *testing.T) {
	root := stateRoot(t)
	h := newHost(workerSample(workerPID), workerSample(decoyPID))
	h.stamp = true
	h.ports = map[uint16]int{workerPort: -1}
	// The process survives the terminate until the test makes it exit, so that the stop pauses through Wait.
	h.processes[workerPID].survives[syscall.SIGTERM] = true
	expected := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
	seed(t, root, workerName, expected)
	unrelated := recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
	unrelated.Generation = "unrelated-generation"
	seed(t, root, unrelatedName, unrelated)
	before := readRaw(t, root, workerName)
	var (
		mu      sync.Mutex
		hold    *holder
		parked  bool
		parking sync.Once
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if hold != nil {
			hold.release()
			<-hold.finished
		}
	})
	watch := &goneWatch{fakeHost: h}
	a := authorityOn(root, h)
	a.Processes = watch
	// The first pause after the terminate parks a removal of an unrelated record, which holds the store's exclusion
	// from then on; only once it is confirmed to hold it does the process exit, so that the exit is observed while
	// the exclusion is already taken.
	a.Wait = func(ctx context.Context, d time.Duration) error {
		if len(h.signals()) > 0 {
			parking.Do(func() {
				started := startHolder(ownership.Store{Root: root}, unrelatedName, unrelated)
				mu.Lock()
				hold = &started
				mu.Unlock()
				select {
				case <-started.held:
					mu.Lock()
					parked = true
					mu.Unlock()
					h.mu.Lock()
					h.processes[workerPID].exited = true
					h.mu.Unlock()
				case <-started.result:
				case <-time.After(busyWithin):
				}
			})
		}
		return timerWait(ctx, d)
	}
	// The caller's deadline passes well inside the default budget after the exit, so that it, and not a bound of the
	// store's own, ends the wait for the exclusion.
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	done := make(chan outcome, 1)
	go func() { done <- capture(func() error { return a.Stop(ctx, workerName, expected) }) }()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(busyWatchdog):
		mu.Lock()
		if hold != nil {
			hold.release()
		}
		mu.Unlock()
		got = <-done
		t.Errorf("Stop did not return within %v after its caller's deadline", busyWatchdog)
	}
	events := h.snapshot()
	if got.panicked != nil {
		t.Fatalf("Stop panicked: %v", got.panicked)
	}
	mu.Lock()
	wasParked := parked
	mu.Unlock()
	if !wasParked {
		t.Fatalf("no removal could hold the store's exclusion during a pause after the terminate; Stop = %v\nevents: %s", got.err, describe(events))
	}
	if !watch.gone.Load() {
		t.Fatalf("the stop was never told that the process is gone before it returned %v, so its retirement was never reached\nevents: %s", got.err, describe(events))
	}
	if diff := gocmp.Diff([]sent{{workerPID, syscall.SIGTERM}}, h.signals()); diff != "" {
		t.Errorf("signals (-want +got):\n%s\nevents: %s", diff, describe(events))
	}
	switch {
	case !errors.Is(got.err, context.DeadlineExceeded):
		t.Errorf("Stop = %v; want an error matching the caller's context.DeadlineExceeded\nevents: %s", got.err, describe(events))
	case errors.Is(got.err, ownership.ErrNotOwned), errors.Is(got.err, ownership.ErrStillRunning):
		t.Errorf("Stop = %v; a record that could not be retired neither disproves ownership nor confirms a survivor", got.err)
	}
	if diff := gocmp.Diff(before, readRaw(t, root, workerName)); diff != "" {
		t.Errorf("the record files changed although the exclusion was never granted (-before +after):\n%s", diff)
	}
	mu.Lock()
	hold.release()
	result := hold.result
	mu.Unlock()
	if r := <-result; r.panicked != nil || r.err != nil || !r.removed {
		t.Errorf("the holder's RemoveIf of the unrelated record = %v, %v (panic: %v); want it removed", r.removed, r.err, r.panicked)
	}
	if err := stop(t, a, t.Context(), workerName, expected); err != nil {
		t.Errorf("a later stop of the gone process = %v; want its record retired", err)
	}
	checkAbsent(t, root, workerName)
}

// TestStoreRemoveIfRefusesAnInvalidRecord decides that RemoveIf removes only a valid record: stored files whose
// process number, generation and start token match the expected record but which fail the record validation are
// refused with an error and kept exactly. It rejects a removal that matches the three fields without validating the
// rest of what it read.
func TestStoreRemoveIfRefusesAnInvalidRecord(t *testing.T) {
	tests := map[string]func(*procinfo.Record){
		"error: a start text that is not a timestamp":       func(r *procinfo.Record) { r.StartedAt = "unknown" },
		"error: a start text whose weekday disagrees":       func(r *procinfo.Record) { r.StartedAt = "Sun Feb  3 22:15:09 2001" },
		"error: a record of another state root":             func(r *procinfo.Record) { r.StateRoot = "/elsewhere/state" },
		"error: a record without a command":                 func(r *procinfo.Record) { r.Command = "" },
		"error: a record whose executable has no inode":     func(r *procinfo.Record) { r.Image.Inode = 0 },
		"error: a record whose executable path is relative": func(r *procinfo.Record) { r.Image.Path = "bin/model-gateway" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			root := stateRoot(t)
			expected := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
			stored := expected
			mutate(&stored)
			data, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, workerName+".pid.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, workerName+".pid"), []byte(strconv.Itoa(stored.PID)), 0o600); err != nil {
				t.Fatal(err)
			}
			before := readRaw(t, root, workerName)
			removed, err := removeIf(t, ownership.Store{Root: root}, workerName, expected)
			if err == nil || removed {
				t.Errorf("RemoveIf of an invalid record = %v, %v; want an error and nothing removed", removed, err)
			}
			if diff := gocmp.Diff(before, readRaw(t, root, workerName)); diff != "" {
				t.Errorf("the invalid record's files changed (-before +after):\n%s", diff)
			}
		})
	}
}

// staleDeadlineContext reports a deadline that has already passed while it stays live: Done stays open and Err stays
// nil, as a context created before a forward step of the wall clock looks until its timer fires.
type staleDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c staleDeadlineContext) Deadline() (time.Time, bool) { return c.deadline, true }

// TestStopSelfWithAContextThatNeverReports decides StopSelf given a context whose deadline has passed by the clock but
// which never reports its end: it returns without waiting for the context, and either the stop was acknowledged or the
// listener was refused (a dial that compares the deadline with the clock may fail before any connection exists). A
// refusal matches ErrUnproven and none of context.Canceled, context.DeadlineExceeded and ErrStopUnacknowledged, since
// the context never ended and no stop was sent; the listener received neither the control token nor a stop request.
// It rejects a client that waits on Done because a clock comparison said the deadline had passed, and one that sends
// the token or the stop after a refused proof.
func TestStopSelfWithAContextThatNeverReports(t *testing.T) {
	f := ssStart(t, ssProven(t))
	ctx := staleDeadlineContext{Context: t.Context(), deadline: time.Now().Add(-time.Minute)}
	type result struct {
		pid int
		outcome
	}
	done, finished := make(chan result, 1), make(chan struct{})
	go func() {
		defer close(finished)
		var r result
		r.outcome = capture(func() (err error) { r.pid, err = ownership.StopSelf(ctx, f.port, ssToken); return err })
		done <- r
	}()
	// The test's context ends before the cleanups run, which releases a client that waits on Done.
	t.Cleanup(func() { <-finished })
	var r result
	select {
	case r = <-done:
	case <-time.After(ssWatchdog):
		t.Fatalf("StopSelf with a context whose deadline passed but which never reports did not return within %v", ssWatchdog)
	}
	if r.panicked != nil {
		t.Fatalf("StopSelf panicked: %v", r.panicked)
	}
	conns := f.record()
	if r.err == nil {
		if diff := gocmp.Diff([][]string{{"GET /internal/identity", "POST /internal/stop"}}, ssTranscripts(conns)); diff != "" {
			t.Errorf("an acknowledged stop's requests per connection (-want +got):\n%s", diff)
		}
		return
	}
	if !errors.Is(r.err, ownership.ErrUnproven) {
		t.Errorf("StopSelf = %d, %v; want the stop acknowledged or a refusal matching ErrUnproven", r.pid, r.err)
	}
	for _, excluded := range []error{context.Canceled, context.DeadlineExceeded, ownership.ErrStopUnacknowledged} {
		if errors.Is(r.err, excluded) {
			t.Errorf("StopSelf = %v matches %v, although the context never ended and no stop was sent", r.err, excluded)
		}
	}
	for i, c := range conns {
		raw := c.received.String()
		if strings.Contains(strings.ToLower(raw), ssToken) {
			t.Errorf("connection %d of a refused stop received the control token: %q", i, raw)
		}
		if strings.Contains(raw, "POST ") || strings.Contains(raw, "/internal/stop") {
			t.Errorf("connection %d of a refused stop received a stop request: %q", i, raw)
		}
	}
}

// TestStopWithAContextThatNeverReports decides that Stop, given a context whose deadline has passed by the clock but
// which never reports its end, neither hangs nor keeps the store's exclusion while it waits: Stop is held in its first
// pause after the terminate until a publication of another record of the same root has completed, which proves that
// the two overlapped, and then returns. It rejects waiting on Done because a clock comparison said the deadline had
// passed, and holding the store's exclusion during the stop's wait.
func TestStopWithAContextThatNeverReports(t *testing.T) {
	root := stateRoot(t)
	h := newHost(workerSample(workerPID), workerSample(decoyPID))
	h.stamp = true
	h.ports = map[uint16]int{workerPort: -1}
	h.processes[workerPID].survives[syscall.SIGTERM] = true
	expected := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
	seed(t, root, workerName, expected)
	a := authorityOn(root, h)
	parked, let := make(chan struct{}), make(chan struct{})
	letGo := sync.OnceFunc(func() { close(let) })
	var parking sync.Once
	// The first pause after the terminate holds Stop until the publication has completed.
	a.Wait = func(ctx context.Context, d time.Duration) error {
		if len(h.signals()) > 0 {
			parking.Do(func() {
				close(parked)
				<-let
			})
		}
		return timerWait(ctx, d)
	}
	ctx := staleDeadlineContext{Context: t.Context(), deadline: time.Now().Add(-time.Minute)}
	stopped, finished := make(chan outcome, 1), make(chan struct{})
	go func() {
		defer close(finished)
		stopped <- capture(func() error { return a.Stop(ctx, workerName, expected) })
	}()
	// The test's context ends and Stop is let go before the stop is joined.
	t.Cleanup(func() { <-finished })
	t.Cleanup(letGo)
	var early *outcome
	select {
	case <-parked:
	case got := <-stopped:
		early = &got
	case <-time.After(busyWatchdog):
		t.Fatalf("Stop neither paused after a signal nor returned within %v; events: %s", busyWatchdog, describe(h.snapshot()))
	}
	if early != nil && early.panicked != nil {
		t.Fatalf("Stop panicked: %v", early.panicked)
	}
	other := recordFor(root, workerSample(decoyPID), roleWorker, workerPort)
	other.Generation = "published-during-the-stop"
	published := make(chan outcome, 1)
	go func() {
		published <- capture(func() error { return (ownership.Store{Root: root}).Publish(unrelatedName, other) })
	}()
	// The pause holds Stop until the publication has completed, so its completion proves the overlap; the watchdog
	// only turns a publication that waits for the stop's exclusion into a failure.
	select {
	case got := <-published:
		if got.panicked != nil || got.err != nil {
			t.Errorf("a publication of another record while the stop waited = %v (panic: %v)", got.err, got.panicked)
		}
	case <-time.After(busyWatchdog):
		letGo()
		<-published
		t.Fatalf("a publication of another record did not complete within %v while Stop was paused; the stop kept the store's exclusion", busyWatchdog)
	}
	if early == nil {
		select {
		case got := <-stopped:
			t.Fatalf("Stop returned %v (panic: %v) while it was held in its pause", got.err, got.panicked)
		default:
		}
		letGo()
		select {
		case got := <-stopped:
			if got.panicked != nil {
				t.Fatalf("Stop panicked: %v", got.panicked)
			}
			t.Logf("Stop with a context that never reports returned %v; events: %s", got.err, describe(h.snapshot()))
		case <-time.After(busyWatchdog):
			t.Fatalf("Stop with a context that never reports did not return within %v; events: %s", busyWatchdog, describe(h.snapshot()))
		}
	} else {
		// Returning before any pause is allowed only as a refusal: the process ignores the terminate and was not
		// killed, so a success then claims an exit that never happened.
		if early.err == nil {
			t.Errorf("Stop reported success before any pause although the process ignores the terminate; events: %s", describe(h.snapshot()))
		} else {
			t.Logf("Stop with a context that never reports refused before any pause, so there was no wait to overlap: %v", early.err)
		}
	}
	checkStored(t, root, unrelatedName, other)
}

// scriptedConn is a health connection whose answer is already there: every read succeeds at once, all of the answer
// but its last byte comes first, and the read that supplies the last byte runs onFinal before it returns (1, nil).
type scriptedConn struct {
	answer  []byte
	onFinal func()
	mu      sync.Mutex
	pos     int
	written bytes.Buffer
	// finals counts the successful reads that supplied the last byte.
	finals int
	closed bool
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch last := len(c.answer) - 1; {
	case c.closed:
		return 0, net.ErrClosed
	case len(p) == 0:
		return 0, nil
	case c.pos < last:
		n := copy(p, c.answer[c.pos:last])
		c.pos += n
		return n, nil
	case c.pos == last:
		c.onFinal()
		p[0] = c.answer[last]
		c.pos++
		c.finals++
		return 1, nil
	default:
		return 0, io.EOF
	}
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return c.written.Write(p)
}

func (c *scriptedConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (*scriptedConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
}

func (*scriptedConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(seamPort)}
}
func (*scriptedConn) SetDeadline(time.Time) error      { return nil }
func (*scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (*scriptedConn) SetWriteDeadline(time.Time) error { return nil }

// TestStopSelfCancelledWhileTheDiagnosticArrives decides that a cancellation with a cause made inside the read that
// completes the health document, which then succeeds, is still reported: the refusal matches ErrUnproven, ctx.Err()
// and the cause, and never repeats the cause's text, whatever the health document said. The seam counts the
// successful final read, so the case fails unless that path was the one taken. It rejects a client that looks at its
// context only when a read fails, and returns the plain refusal once the document was read in full.
func TestStopSelfCancelledWhileTheDiagnosticArrives(t *testing.T) {
	tests := map[string]string{
		"error: a health document that is not JSON":           "not a health document",
		"error: a health document naming some process number": `{"ok":true,"supervisorPid":31337}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				health := &scriptedConn{answer: []byte(ssAnswer("200 OK", "application/json", body)), onFinal: func() { cancel(errCallerCause) }}
				peers := make(chan *seamPeer, 1)
				d := &peerDrain{peers: peers}
				defer d.end()
				var dials atomic.Int32
				restore := ownership.SetStopSelfDial(func(context.Context, string, string) (net.Conn, error) {
					if dials.Add(1) == 1 {
						client, server := net.Pipe()
						peers <- newSeamPeer(server)
						return client, nil
					}
					return health, nil
				})
				t.Cleanup(restore)
				done := make(chan seamResult, 1)
				go func() {
					var r seamResult
					r.err = guarded("StopSelf", func() (err error) { r.pid, err = ownership.StopSelf(ctx, seamPort, ssToken); return err })
					done <- r
				}()
				peer := d.add(seamPeerOrResult(t, peers, done))
				t.Cleanup(func() { _ = peer.conn.Close() })
				_ = peer.next(t)
				peer.write(t, ssAnswer("200 OK", "text/html", "<!doctype html><title>dev server</title>"))
				r := <-done
				health.mu.Lock()
				finals, request := health.finals, health.written.String()
				health.mu.Unlock()
				if !strings.HasPrefix(request, "GET /healthz ") {
					t.Fatalf("the health connection carried %q; want the health request; StopSelf = %v", request, r.err)
				}
				if finals != 1 {
					t.Fatalf("the read that completes the health document succeeded %d times; want once, so that the cancellation came inside a successful read; StopSelf = %v", finals, r.err)
				}
				checkEnded(t, ctx, r.err, []error{ownership.ErrUnproven}, []error{ownership.ErrStopUnacknowledged}, "doctype", body)
			})
		})
	}
}

// connPanic is the value a panicking control connection panics with.
type connPanic struct{ op string }

// panickingConn is a control connection that panics in its first Read, its first Write, or the first Write after a
// read returned data, and records every call made on it, separately for the calls made after StopSelf returned.
type panickingConn struct {
	net.Conn
	// panicOn is "read", "write" or "write after read".
	panicOn string
	mu      sync.Mutex
	read    bool
	closes  int
	ended   bool
	late    []string
}

func (c *panickingConn) note(op string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if op == "close" {
		c.closes++
	}
	if c.ended {
		c.late = append(c.late, op)
	}
}

func (c *panickingConn) Read(p []byte) (int, error) {
	c.note("read")
	if c.panicOn == "read" {
		panic(connPanic{"read"})
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.read = true
		c.mu.Unlock()
	}
	return n, err
}

func (c *panickingConn) Write(p []byte) (int, error) {
	c.note("write")
	c.mu.Lock()
	read := c.read
	c.mu.Unlock()
	if c.panicOn == "write" || c.panicOn == "write after read" && read {
		panic(connPanic{c.panicOn})
	}
	return c.Conn.Write(p)
}

func (c *panickingConn) Close() error {
	c.note("close")
	return c.Conn.Close()
}

func (c *panickingConn) SetDeadline(t time.Time) error {
	c.note("set deadline")
	return c.Conn.SetDeadline(t)
}

func (c *panickingConn) SetReadDeadline(t time.Time) error {
	c.note("set read deadline")
	return c.Conn.SetReadDeadline(t)
}

func (c *panickingConn) SetWriteDeadline(t time.Time) error {
	c.note("set write deadline")
	return c.Conn.SetWriteDeadline(t)
}

// TestStopSelfControlConnectionPanics decides that a panic of the control connection, in the write of the challenge,
// in the read of the proof or in the write of the stop, propagates out of StopSelf with its own value, and that by
// then the connection was closed exactly once and nothing is left that touches it: a cancellation of the context, and
// the passing of the default budget, afterwards make no call on it at all. It rejects cleanup done by explicit calls
// after the exchange instead of deferred ones, and a context callback or timer left registered on a connection the
// call no longer owns. Known limit: the connection's panic is recovered only on the goroutine that calls StopSelf; an
// implementation that does its connection I/O on another goroutine would let that panic end the whole test binary.
func TestStopSelfControlConnectionPanics(t *testing.T) {
	tests := map[string]string{
		"error: the write of the challenge panics": "write",
		"error: the read of the proof panics":      "read",
		"error: the write of the stop panics":      "write after read",
	}
	for name, panicOn := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// A caller-owned deadline far in the future: StopSelf then works under the caller's context rather than one
				// of its own that it cancels while unwinding, so a callback left registered on it can only run when the
				// test cancels it, which happens after the connection is marked ended.
				ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Hour))
				defer cancel()
				var conn *panickingConn
				peers := make(chan *seamPeer, 1)
				d := &peerDrain{peers: peers}
				defer d.end()
				var dials atomic.Int32
				restore := ownership.SetStopSelfDial(func(context.Context, string, string) (net.Conn, error) {
					dials.Add(1)
					client, server := net.Pipe()
					peer := newSeamPeer(server)
					if conn != nil {
						_ = client.Close()
						_ = server.Close()
						return nil, errors.New("only one connection is expected")
					}
					conn = &panickingConn{Conn: client, panicOn: panicOn}
					peers <- peer
					return conn, nil
				})
				t.Cleanup(restore)
				type result struct {
					panicked any
					err      error
				}
				done := make(chan result, 1)
				go func() {
					var r result
					defer func() {
						r.panicked = recover()
						done <- r
					}()
					_, r.err = ownership.StopSelf(ctx, seamPort, ssToken)
				}()
				// The dial queues its peer before a panic in the first write can queue the result, so both may be ready
				// when the select runs; a result taken first is kept, and the peer it left queued is taken after it.
				var r result
				var peer *seamPeer
				ended := false
				select {
				case peer = <-peers:
				case r = <-done:
					ended = true
					select {
					case peer = <-peers:
					default:
						t.Fatalf("StopSelf ended without dialing: error %v, panic %v", r.err, r.panicked)
					}
				}
				d.add(peer)
				t.Cleanup(func() { _ = peer.conn.Close() })
				if panicOn == "write after read" {
					challenge := peer.next(t)
					head, tail := seamProof(t, challenge)
					peer.write(t, head+tail)
				}
				if !ended {
					r = <-done
				}
				// connPanic has an unexported field, which go-cmp refuses by panicking; it is comparable, so == decides.
				if got, ok := r.panicked.(connPanic); !ok || got != (connPanic{panicOn}) {
					t.Fatalf("the panic out of StopSelf = %#v, error %v; want %#v", r.panicked, r.err, connPanic{panicOn})
				}
				conn.mu.Lock()
				closes := conn.closes
				conn.ended = true
				conn.mu.Unlock()
				if closes != 1 {
					t.Errorf("the control connection was closed %d times by the time the panic left StopSelf; want exactly once", closes)
				}
				cancel()
				synctest.Wait()
				time.Sleep(2 * defaultBudget)
				synctest.Wait()
				conn.mu.Lock()
				late := slices.Clone(conn.late)
				conn.mu.Unlock()
				if len(late) != 0 {
					t.Errorf("after the panic left StopSelf, the cancellation and the default budget made the calls %q on the control connection; want none", late)
				}
				if n := dials.Load(); n != 1 {
					t.Errorf("%d connections were dialed; want only the control connection", n)
				}
			})
		})
	}
}

// TestStopRetirementRemovesOnlyPartly decides the stop whose retirement removes the record's JSON but cannot remove its
// mirror, because the mirror was replaced by a directory between the removal's match and its unlink: the stop
// reports an error matching neither ErrNotOwned nor ErrStillRunning, and the JSON is gone. It rejects a stop that
// reports success over a retirement that failed, and one that reports the gone process as not owned or as still
// running. Whether the error also differs from the outcome of a record kept whole is a matter of its wording, which
// is not decided here.
func TestStopRetirementRemovesOnlyPartly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := stateRoot(t)
		h := newHost(workerSample(workerPID), workerSample(decoyPID))
		h.stamp = true
		h.ports = map[uint16]int{workerPort: -1}
		expected := recordFor(root, workerSample(workerPID), roleWorker, workerPort)
		seed(t, root, workerName, expected)
		mirror := filepath.Join(root, workerName+".pid")
		var (
			mu      sync.Mutex
			ran     bool
			hookErr error
		)
		a := authorityOn(root, h)
		a.Records = ownership.WithBeforeRemove(ownership.Store{Root: root}, func() {
			mu.Lock()
			defer mu.Unlock()
			ran = true
			// A directory with an entry, so that neither an unlink nor an rmdir removes it.
			hookErr = errors.Join(os.Remove(mirror), os.Mkdir(mirror, 0o700), os.WriteFile(filepath.Join(mirror, "occupant"), []byte("x"), 0o600))
		})
		ctx, cancel := context.WithTimeout(t.Context(), stopHorizon)
		defer cancel()
		err := stop(t, a, ctx, workerName, expected)
		mu.Lock()
		hookRan, replaced := ran, hookErr
		mu.Unlock()
		if !hookRan {
			t.Fatalf("the retirement never reached the removal hook of the authority's store; Stop = %v\nevents: %s", err, describe(h.snapshot()))
		}
		if replaced != nil {
			t.Fatalf("replacing the mirror by a directory: %v", replaced)
		}
		if diff := gocmp.Diff([]sent{{workerPID, syscall.SIGTERM}}, h.signals()); diff != "" {
			t.Errorf("signals (-want +got):\n%s", diff)
		}
		switch {
		case err == nil:
			t.Error("Stop succeeded although the record's mirror could not be removed")
		case errors.Is(err, ownership.ErrNotOwned), errors.Is(err, ownership.ErrStillRunning):
			t.Errorf("Stop = %v; a partly failed retirement neither disproves ownership nor confirms a survivor", err)
		}
		if _, statErr := os.Lstat(filepath.Join(root, workerName+".pid.json")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("the record's JSON after the partly failed retirement: Lstat = %v; want it removed", statErr)
		}
		if info, statErr := os.Lstat(mirror); statErr != nil || !info.IsDir() {
			t.Errorf("the directory in the mirror's place after the retirement: %v, %v; want it kept", info, statErr)
		}
	})
}
