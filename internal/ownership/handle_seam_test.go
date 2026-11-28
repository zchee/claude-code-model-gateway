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

package ownership

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// handleTWatchdog aborts a row whose calls never return; it proves nothing about what they did.
const handleTWatchdog = 30 * time.Second

// handleTFake is a process reference that records what a Handle does through it, and every use of it after its
// release.
type handleTFake struct {
	signalErr, closeErr error
	// entered and release, when set, hold the first signal in flight: entered is closed when it starts, and it
	// returns once release is closed.
	entered, release chan struct{}
	// held, when set, is the Handle whose lock every signal and every release must find taken: each tries the lock
	// and reports a violation when it gets it.
	held *Handle
	// touched is written by a signal after its hold and by every release, before either synchronizes with
	// anything, so that a release that is not ordered after a signal in flight is a data race the race detector
	// reports on any schedule.
	touched  int
	inFlight atomic.Bool
	released atomic.Bool

	mu         sync.Mutex
	signals    []syscall.Signal
	closes     int
	violations []string
}

func (f *handleTFake) violation(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.violations = append(f.violations, fmt.Sprintf(format, args...))
}

// unheld reports a violation when the lock of the Handle f serves is free while what runs.
func (f *handleTFake) unheld(what string) {
	if f.held != nil && f.held.mu.TryLock() {
		f.held.mu.Unlock()
		f.violation("%s ran while the handle's lock was free", what)
	}
}

func (f *handleTFake) signal(sig syscall.Signal) error {
	f.unheld("the signal")
	if f.released.Load() {
		f.violation("signal %v sent through a released reference", sig)
	}
	f.inFlight.Store(true)
	if f.entered != nil {
		close(f.entered)
		<-f.release
		f.entered = nil
	}
	f.touched++
	f.inFlight.Store(false)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	return f.signalErr
}

func (f *handleTFake) close() error {
	f.unheld("the release")
	f.touched++
	if f.inFlight.Load() {
		f.violation("the reference was released while a signal was in flight")
	}
	f.released.Store(true)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return f.closeErr
}

// handleTUse is what the fake saw.
type handleTUse struct {
	Signals    []syscall.Signal
	Closes     int
	Violations []string
}

func (f *handleTFake) use() handleTUse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return handleTUse{Signals: slices.Clone(f.signals), Closes: f.closes, Violations: slices.Clone(f.violations)}
}

// handleTGuarded runs call and returns a panic as an error, for a goroutine in which t.Fatal must not be called.
func handleTGuarded(what string, call func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%s panicked: %v", what, p)
		}
	}()
	return call()
}

// handleTCall runs call and fails the row instead of the binary when it panics.
func handleTCall(t testing.TB, what string, call func() error) (err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("%s panicked: %v", what, p)
		}
	}()
	return call()
}

// handleTResult says whether err is nil, matches want, or is something else.
func handleTResult(err, want error) string {
	switch {
	case err == nil:
		return "nil"
	case want != nil && errors.Is(err, want):
		return "matches " + want.Error()
	}
	return "unexpected: " + err.Error()
}

// handleTExpect is the result handleTResult gives for an error matching want, or for nil when want is nil.
func handleTExpect(want error) string {
	if want == nil {
		return "nil"
	}
	return "matches " + want.Error()
}

// TestHandleDelivery decides what a Handle does through its process reference: a terminate is exactly SIGTERM, the
// private gone result is os.ErrProcessDone, any other delivery error is returned as it is, a closed handle and
// every copy of its pointer send nothing, and the reference is released once.
func TestHandleDelivery(t *testing.T) {
	type step struct {
		// call is "terminate" or "close"; copy, when set, makes it through a copy of the handle's pointer.
		call string
		copy bool
		// want is the sentinel the result must match; nil wants a nil result.
		want error
	}
	tests := map[string]struct {
		signalErr, closeErr error
		steps               []step
		// notWant is a sentinel no result may match.
		notWant error
		use     handleTUse
	}{
		"success: terminate sends exactly SIGTERM, and close releases the reference": {
			steps: []step{{call: "terminate"}, {call: "close"}},
			use:   handleTUse{Signals: []syscall.Signal{syscall.SIGTERM}, Closes: 1},
		},
		"error: the gone result of the reference is os.ErrProcessDone": {
			signalErr: errProcessGone,
			steps:     []step{{call: "terminate", want: os.ErrProcessDone}, {call: "close"}},
			use:       handleTUse{Signals: []syscall.Signal{syscall.SIGTERM}, Closes: 1},
		},
		"error: another delivery error is returned as it is and is not a gone process": {
			signalErr: syscall.EPERM,
			steps:     []step{{call: "terminate", want: syscall.EPERM}, {call: "close"}},
			notWant:   os.ErrProcessDone,
			use:       handleTUse{Signals: []syscall.Signal{syscall.SIGTERM}, Closes: 1},
		},
		"error: a closed handle sends nothing and reports it closed": {
			steps: []step{{call: "close"}, {call: "terminate", want: os.ErrClosed}},
			use:   handleTUse{Closes: 1},
		},
		"success: a second close returns nil and releases nothing twice": {
			steps: []step{{call: "close"}, {call: "close"}, {call: "terminate", want: os.ErrClosed}},
			use:   handleTUse{Closes: 1},
		},
		"error: a failed release is reported once and the handle stays closed": {
			closeErr: syscall.EBADF,
			steps:    []step{{call: "close", want: syscall.EBADF}, {call: "close"}, {call: "terminate", want: os.ErrClosed}},
			use:      handleTUse{Closes: 1},
		},
		"error: a copy of the pointer shares the close state": {
			steps: []step{{call: "close", copy: true}, {call: "terminate", want: os.ErrClosed}, {call: "close"}},
			use:   handleTUse{Closes: 1},
		},
		"error: closing through the original closes every copy": {
			steps: []step{{call: "close"}, {call: "terminate", copy: true, want: os.ErrClosed}, {call: "close", copy: true}},
			use:   handleTUse{Closes: 1},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handleTNoRealPath(t)
			f := &handleTFake{signalErr: tt.signalErr, closeErr: tt.closeErr}
			h := &Handle{process: f}
			copied := h
			var want, got []string
			for _, s := range tt.steps {
				target := h
				if s.copy {
					target = copied
				}
				var err error
				switch s.call {
				case "terminate":
					err = handleTCall(t, "Handle.Terminate", target.Terminate)
				case "close":
					err = handleTCall(t, "Handle.Close", target.Close)
				}
				if tt.notWant != nil && errors.Is(err, tt.notWant) {
					t.Errorf("%s returned %v, which matches %v", s.call, err, tt.notWant)
				}
				want = append(want, s.call+": "+handleTExpect(s.want))
				got = append(got, s.call+": "+handleTResult(err, s.want))
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("results (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.use, f.use()); diff != "" {
				t.Errorf("use of the process reference (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHandleCloseWaitsForTerminate decides that a Close racing a Terminate in flight releases the reference only after
// the signal returned, so that a released descriptor is never signalled through. Under the race detector an
// unordered release is a data race on any schedule; without it, a release seen while the signal is held is reported.
func TestHandleCloseWaitsForTerminate(t *testing.T) {
	tests := map[string]struct {
		signalErr error
		// terminate is the sentinel the held Terminate must match; nil wants a nil result.
		terminate error
		use       handleTUse
	}{
		"success: a close during a terminate releases after it": {
			use: handleTUse{Signals: []syscall.Signal{syscall.SIGTERM}, Closes: 1},
		},
		"error: a close during a terminate of a gone process releases after it": {
			signalErr: errProcessGone,
			terminate: os.ErrProcessDone,
			use:       handleTUse{Signals: []syscall.Signal{syscall.SIGTERM}, Closes: 1},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handleTNoRealPath(t)
			f := &handleTFake{signalErr: tt.signalErr, entered: make(chan struct{}), release: make(chan struct{})}
			entered := f.entered
			h := &Handle{process: f}
			terminated := make(chan error, 1)
			go func() { terminated <- handleTGuarded("Handle.Terminate", h.Terminate) }()
			select {
			case <-entered:
			case err := <-terminated:
				// The terminate returned without sending through the reference: nothing is in flight to race.
				close(f.release)
				t.Fatalf("Terminate returned %v before its signal was in flight", err)
			case <-time.After(handleTWatchdog):
				// The watchdog and a legitimate start or return can be ready together: look at both again before
				// declaring a hang.
				select {
				case <-entered:
				case err := <-terminated:
					close(f.release)
					t.Fatalf("Terminate returned %v before its signal was in flight", err)
				default:
					// The terminate is released and joined before the row fails, so it cannot outlive the row's model;
					// one that never returns is ended by the binary's timeout.
					close(f.release)
					<-terminated
					t.Fatal("Terminate neither signalled nor returned within the watchdog")
				}
			}
			closed := make(chan error, 1)
			go func() { closed <- handleTGuarded("Handle.Close", h.Close) }()
			close(f.release)
			var results [2]error
			workers := []chan error{terminated, closed}
			for i, done := range workers {
				select {
				case results[i] = <-done:
				case <-time.After(handleTWatchdog):
					// The watchdog and a legitimate return can be ready together: look again before declaring a hang.
					select {
					case results[i] = <-done:
					default:
						// Every worker still running is joined before the row fails, so none outlives the row's
						// model; one that never returns is ended by the binary's timeout.
						for _, rest := range workers[i:] {
							<-rest
						}
						t.Fatal("Terminate or Close did not return within the watchdog after the signal was released")
					}
				}
			}
			if diff := gocmp.Diff([]string{handleTExpect(tt.terminate), "nil"}, []string{handleTResult(results[0], tt.terminate), handleTResult(results[1], nil)}); diff != "" {
				t.Errorf("results of the terminate and the close (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.use, f.use()); diff != "" {
				t.Errorf("use of the process reference (-want +got):\n%s", diff)
			}
			if got := handleTResult(handleTCall(t, "Handle.Terminate", h.Terminate), os.ErrClosed); got != handleTExpect(os.ErrClosed) {
				t.Errorf("Terminate after the close: %s; want a result matching %v", got, os.ErrClosed)
			}
		})
	}
}

// TestHandleLockHeldAcrossCalls decides, with one caller and no scheduling, that the handle's one lock is held while
// the signal runs and while the release runs: the process reference tries that lock from inside each call, and
// getting it is a violation. A handle that takes the lock only around its own bookkeeping fails here without the race
// detector.
func TestHandleLockHeldAcrossCalls(t *testing.T) {
	tests := map[string]struct {
		signalErr error
		// calls are the methods called in order on one handle.
		calls []string
		want  []string
		use   handleTUse
	}{
		"success: the terminate's signal and the close's release run under the lock": {
			calls: []string{"terminate", "close"},
			want:  []string{"terminate: nil", "close: nil"},
			use:   handleTUse{Signals: []syscall.Signal{syscall.SIGTERM}, Closes: 1},
		},
		"error: the signal of a gone process runs under the lock as well": {
			signalErr: errProcessGone,
			calls:     []string{"terminate", "close"},
			want:      []string{"terminate: matches " + os.ErrProcessDone.Error(), "close: nil"},
			use:       handleTUse{Signals: []syscall.Signal{syscall.SIGTERM}, Closes: 1},
		},
		"success: a close without a terminate releases under the lock": {
			calls: []string{"close"},
			want:  []string{"close: nil"},
			use:   handleTUse{Closes: 1},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handleTNoRealPath(t)
			f := &handleTFake{signalErr: tt.signalErr}
			h := &Handle{process: f}
			f.held = h
			var got []string
			for _, call := range tt.calls {
				switch call {
				case "terminate":
					got = append(got, call+": "+handleTResult(handleTCall(t, "Handle.Terminate", h.Terminate), os.ErrProcessDone))
				case "close":
					got = append(got, call+": "+handleTResult(handleTCall(t, "Handle.Close", h.Close), nil))
				}
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("results of %v (-want +got):\n%s", tt.calls, diff)
			}
			if diff := gocmp.Diff(tt.use, f.use()); diff != "" {
				t.Errorf("use of the process reference (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHandleZero decides that the zero Handle is closed: it sends nothing, Terminate reports it closed, and Close is
// idempotent on it.
func TestHandleZero(t *testing.T) {
	tests := map[string]struct {
		// calls are the methods called in order on one zero Handle.
		calls []string
		want  []string
	}{
		"error: terminate on a zero handle reports it closed": {
			calls: []string{"terminate"},
			want:  []string{"os.ErrClosed"},
		},
		"success: close on a zero handle returns nil each time": {
			calls: []string{"close", "close"},
			want:  []string{"nil", "nil"},
		},
		"error: terminate after closing a zero handle reports it closed": {
			calls: []string{"close", "terminate", "close"},
			want:  []string{"nil", "os.ErrClosed", "nil"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handleTNoRealPath(t)
			h := new(Handle)
			var got []string
			for _, call := range tt.calls {
				switch call {
				case "terminate":
					got = append(got, handleTKind(handleTCall(t, "Handle.Terminate", h.Terminate)))
				case "close":
					got = append(got, handleTKind(handleTCall(t, "Handle.Close", h.Close)))
				}
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("results of %v (-want +got):\n%s", tt.calls, diff)
			}
		})
	}
}
