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

//go:build process && (darwin || linux)

package serveshim_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// joinRun waits for Run to return after it was told to stop, failing the case when it does not within a watchdog.
func joinRun(t *testing.T, s *ownedShim) {
	t.Helper()
	// A watchdog only: it ends a case whose Run never returns.
	if !s.awaitStop(time.Now().Add(20 * time.Second)) {
		t.Fatal("Run did not return within 20s of its stop")
	}
}

// TestRunHonoursCancellationDuringFirstLaunch checks that a stop which arrives while the supervisor's first worker
// launch is held before anything is reserved for it is acted on at once, and is honoured when the launch is let go.
// While the launch is still held, the supervisor must record that no worker may start any more; it may wait for the
// held launch before it closes its public listener, as the source waits for outstanding work. Once the launch is let
// go, Run returns with the orderly stop's result and no worker process was ever started. Run may join the launch
// before it returns; a launch that outlives Run fails the package's goroutine leak check.
func TestRunHonoursCancellationDuringFirstLaunch(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	letGo := sync.OnceFunc(func() { close(release) })
	inhibited := make(chan struct{})
	markInhibited := sync.OnceFunc(func() { close(inhibited) })
	var starts atomic.Int32
	s := startOwnedShim(t, nil, withHooks(serveshim.Hooks{
		Launching: func() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		},
		Starting:      func() { starts.Add(1) },
		StopInhibited: markInhibited,
	}))
	t.Cleanup(letGo)
	select {
	case <-entered:
	case <-s.done:
		t.Fatalf("the supervisor stopped before it launched a worker: %v", s.result)
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor never launched its first worker")
	}
	s.cancel()
	// The launch stays held until the stop has recorded its inhibition, so that the inhibition cannot come from a Run
	// that first finished the launch.
	select {
	case <-inhibited:
	case <-time.After(10 * time.Second):
		t.Error("the supervisor did not record its stop within 10s of the cancellation while the first launch was held; want the stop acted on without waiting for the launch")
	}
	letGo()
	joinRun(t, s)
	if s.result != nil {
		t.Errorf("Run returned %v after its cancellation; want the orderly stop's nil", s.result)
	}
	if n := starts.Load(); n != 0 {
		t.Errorf("%d worker processes were started for a supervisor that was stopped during its first launch; want none", n)
	}
}

// TestReadinessPrecedesFirstWorkerStart checks that the supervisor prints its readiness line before it starts its
// first worker process, as the source binds, prints the line and only then starts the worker. The first start is held
// in the Starting hook; the line must reach the test while the start is still held.
func TestReadinessPrecedesFirstWorkerStart(t *testing.T) {
	t.Parallel()
	starting, proceed := make(chan struct{}, 1), make(chan struct{})
	letGo := sync.OnceFunc(func() { close(proceed) })
	var first atomic.Bool
	s := startOwnedShim(t, nil, withHooks(serveshim.Hooks{
		Starting: func() {
			if !first.CompareAndSwap(false, true) {
				return
			}
			starting <- struct{}{}
			<-proceed
		},
	}))
	t.Cleanup(letGo)
	select {
	case <-starting:
	case <-s.done:
		t.Fatalf("the supervisor stopped before it started a worker: %v", s.result)
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor never started its first worker")
	}
	select {
	case <-s.ready:
	case <-s.done:
		t.Errorf("the supervisor stopped while its first start was held, before it printed readiness: %v", s.result)
	case <-time.After(10 * time.Second):
		t.Error("the readiness line did not arrive within 10s while the first worker start was held; want it printed before the worker starts")
	}
	letGo()
	s.cancel()
	joinRun(t, s)
}

// fenceObservation is what a check of the start fence found at a worker start.
type fenceObservation struct {
	acquired bool
	err      error
}

// checkFence tries to take the state root's start fence for a moment and reports whether it could.
func checkFence(ctx context.Context, state string) fenceObservation {
	fence, err := lifecycle.NewStartFence(state)
	if err != nil {
		return fenceObservation{err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	releaseFence, err := fence.Acquire(ctx)
	if err == nil {
		_ = releaseFence()
		return fenceObservation{acquired: true}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fenceObservation{}
	}
	return fenceObservation{err: err}
}

// TestWorkerStartHoldsStartFence checks that every worker process, the first and a replacement after a crash, is
// started only while the supervisor holds its state root's start fence, so that a deliberate stop that publishes its
// marker under the same fence is ordered against every start. At the moment of each start another acquisition of the
// fence must not succeed.
func TestWorkerStartHoldsStartFence(t *testing.T) {
	t.Parallel()
	seen := make(chan fenceObservation, 4)
	fenceCtx := t.Context()
	var state string
	s := startOwnedShim(t, nil, withHooks(serveshim.Hooks{Starting: func() {
		select {
		case seen <- checkFence(fenceCtx, state):
		default:
		}
	}}), func(dir string) { state = dir })
	observe := func(which string) {
		t.Helper()
		select {
		case o := <-seen:
			if o.acquired {
				t.Errorf("the start fence could be acquired while the supervisor started its %s worker; want it held across the start", which)
			}
			if o.err != nil {
				t.Errorf("the fence check at the %s worker's start failed with %v; want only the timeout of a held fence", which, o.err)
			}
		case <-s.done:
			t.Fatalf("the supervisor stopped before it started its %s worker: %v", which, s.result)
		case <-time.After(10 * time.Second):
			t.Fatalf("the supervisor never started its %s worker", which)
		}
	}
	observe("first")
	first := s.nextChild(t)
	first.tell(t, "crash")
	first.exited(t)
	observe("replacement")
}

// TestStopMarkerPreventsWorkerStarts checks that the supervisor starts no worker while its state root carries the
// stop marker: neither a first worker when the marker exists before Run, nor a replacement when the marker is
// published while the first worker's crash is held before the supervisor acts on it. Each case waits until the
// supervisor has carried out the decision the marker governs and requires that it started nothing; only then is Run
// stopped, and the starts are counted over Run's whole life.
func TestStopMarkerPreventsWorkerStarts(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		// seed publishes the marker before Run starts.
		seed bool
		// wantStarts is the number of worker processes started over Run's life.
		wantStarts int32
	}{
		"error: a marker present at the supervisor's start prevents the first worker":           {seed: true, wantStarts: 0},
		"error: a marker published while a crash is held prevents the replacement worker start": {wantStarts: 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var starts atomic.Int32
			// decisions carries, for every decision whether to start a worker, whether one was started.
			decisions := make(chan bool, 8)
			hold := newExitHold()
			var state string
			prepare := func(dir string) {
				state = dir
				if tc.seed {
					if err := (lifecycle.Files{StateDir: dir}).Stop(); err != nil {
						t.Fatal(err)
					}
				}
			}
			s := startOwnedShim(t, nil, withHooks(serveshim.Hooks{
				LaunchEnded: func(started bool) {
					select {
					case decisions <- started:
					default:
					}
				},
				Starting:     func() { starts.Add(1) },
				ExitObserved: hold.observe,
			}), prepare)
			t.Cleanup(hold.free)
			if !tc.seed {
				first := s.nextChild(t)
				select {
				case started := <-decisions:
					if !started {
						t.Fatal("the supervisor decided not to start its first worker without a stop marker")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the supervisor's first start decision was not carried out within 10s")
				}
				first.tell(t, "crash")
				first.exited(t)
				select {
				case <-hold.held:
				case <-time.After(10 * time.Second):
					t.Fatal("the supervisor did not report the crashed worker's exit within 10s")
				}
				if err := (lifecycle.Files{StateDir: state}).Stop(); err != nil {
					t.Fatal(err)
				}
				hold.free()
			}
			// The decision the marker governs, carried out before anything else stops the supervisor: it starts
			// nothing. A supervisor that stops by itself instead of deciding is correct too.
			select {
			case started := <-decisions:
				if started {
					t.Error("the supervisor started a worker although the stop marker was present")
				}
			case <-s.done:
			case <-time.After(10 * time.Second):
				t.Error("the supervisor neither decided about the worker start the stop marker governs nor stopped within 10s")
			}
			// Exits are no longer held: a supervisor that started a worker past the marker must be able to tear it
			// down, so that this case reports the start rather than a supervisor parked in the exit hook.
			hold.free()
			s.cancel()
			joinRun(t, s)
			if n := starts.Load(); n != tc.wantStarts {
				t.Errorf("%d worker processes were started; want %d with the stop marker present", n, tc.wantStarts)
			}
		})
	}
}
