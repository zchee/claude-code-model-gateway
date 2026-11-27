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

//go:build linux || darwin

package workerlink_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// slowSetterConn holds the first deadline call made once the held Write has begun, which is the cancellation's attempt
// to end that Write, at the setter's entry: before the deadline is applied. The call announces itself on setterIn and
// waits for release; then it fails with fail when that is set, and otherwise applies the deadline to the gated end and
// returns nil; setterDone is closed when it returns. Deadline calls before the Write began, the arm, pass straight
// through. Every later call after the Write began fails with fail too, when it is set. It counts the calls to Close.
type slowSetterConn struct {
	*gatedConn
	fail       error
	setterIn   chan struct{}
	setterGo   chan struct{}
	setterDone chan struct{}
	closes     atomic.Int32

	first, once sync.Once
}

func newSlowSetterConn(g *gatedConn, fail error) *slowSetterConn {
	return &slowSetterConn{gatedConn: g, fail: fail, setterIn: make(chan struct{}), setterGo: make(chan struct{}), setterDone: make(chan struct{})}
}

// setter runs one deadline call: apply sets the deadline on the gated end.
func (c *slowSetterConn) setter(apply func() error) error {
	if !isClosed(c.entered) {
		return apply()
	}
	parked := false
	c.first.Do(func() {
		parked = true
		close(c.setterIn)
	})
	if parked {
		defer close(c.setterDone)
		<-c.setterGo
	}
	if c.fail != nil {
		return c.fail
	}
	return apply()
}

func (c *slowSetterConn) SetWriteDeadline(t time.Time) error {
	return c.setter(func() error { return c.gatedConn.SetWriteDeadline(t) })
}

func (c *slowSetterConn) SetDeadline(t time.Time) error {
	return c.setter(func() error { return c.gatedConn.SetDeadline(t) })
}

func (c *slowSetterConn) Close() error {
	c.closes.Add(1)
	return c.gatedConn.Close()
}

func (c *slowSetterConn) release() { c.once.Do(func() { close(c.setterGo) }) }

// TestAcknowledgeDrainSlowCancellationSetter checks how a Write that returns a timeout is attributed when the
// caller's cancellation is still setting its passed deadline. The Write is held at the gate and the caller cancels
// before the armed deadline; the cancellation's deadline call is held at its entry, before that deadline is applied.
// Then the armed deadline expires and the Write chooses its timeout, held at the return barrier.
//   - When the Write returns before the setter has applied the passed deadline (the test lets the Write return, lets
//     the bubble's clock move on, and only then lets the setter return nil), the timeout came from the deadline that
//     was already armed: the budget's error, classified exactly as the budget's own error measured on a sibling
//     reservation, or, when the caller's own earlier deadline was the armed one, context.DeadlineExceeded. It is not
//     the cancellation.
//   - When the setter completes before the Write returns its timeout, the two overlap; the rule for that overlap is that
//     the caller's cancellation wins.
//
// The bubble's clock moves on between the Write's return and the setter's, so that an implementation that orders the
// two by time sees them apart.
func TestAcknowledgeDrainSlowCancellationSetter(t *testing.T) {
	tests := map[string]struct {
		// callerDeadline gives the caller a deadline of half the budget, which is the armed one.
		callerDeadline bool
		// setterFirst lets the setter complete before the Write returns its timeout.
		setterFirst bool
	}{
		"error: the Write returns the budget's timeout before the cancellation's deadline is applied":                  {},
		"error: the Write returns the caller deadline's timeout before the cancellation's deadline is applied":         {callerDeadline: true},
		"error: the cancellation's deadline is applied before the Write returns its timeout, so the cancellation wins": {setterFirst: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			guardedBubble(t, func(t *testing.T, at func(string)) {
				at("setting up the reservations")
				r := newWrappedRegistry(t, nil)
				var slow *slowSetterConn
				g := r.reserve(t, "gen-a", 41001, 4101, reserveOptions{park: true, wrap: func(gc *gatedConn) net.Conn {
					slow = newSlowSetterConn(gc, nil)
					return slow
				}})
				sibling := r.reserve(t, "gen-sibling", 41003, 4103, reserveOptions{park: true})

				at("the budget's own error on the sibling")
				sibAck := startAck(t.Context(), sibling.res)
				await(t, sibling.parent.entered, "the sibling acknowledgment's write")
				budgetErr := awaitLeftWrite(t, sibling.gatedReservation, sibAck)
				if budgetErr == nil {
					t.Fatal("the sibling AcknowledgeDrain reported success for a line the worker never read")
				}
				sibEntry, _ := sibling.parent.heldSpan()
				budget := sibling.parent.interruption().Sub(sibEntry)
				if err := sibling.res.End(); err != nil {
					t.Fatalf("End of the sibling: %v", err)
				}

				ctx, cancel := context.WithCancel(t.Context())
				if tt.callerDeadline {
					cancel()
					if budget <= 0 {
						t.Logf("the budget measured on the sibling is %v; no shorter caller deadline can be placed, so the case was not constructed and nothing is checked", budget)
						return
					}
					ctx, cancel = context.WithTimeout(t.Context(), budget/2)
				}
				defer cancel()
				ack := startAck(ctx, g.res)
				at("waiting for the acknowledgment's Write")
				await(t, g.parent.entered, "the acknowledgment's write")
				cancel()
				at("the cancellation's deadline call held at its entry")
				await(t, slow.setterIn, "the cancellation's deadline call on the parent end")
				at("the armed deadline expires and the Write chooses its timeout")
				await(t, g.parent.returning, "the timeout of the armed deadline")
				synctest.Wait()
				if ack.poll() {
					t.Fatalf("AcknowledgeDrain returned %v while its Write had not returned yet", ack.err)
				}
				if tt.setterFirst {
					at("the setter completes before the Write returns")
					slow.release()
					await(t, slow.setterDone, "the return of the cancellation's deadline call")
					synctest.Wait()
					g.parent.releaseReturns()
				} else {
					at("the Write returns before the setter has applied its deadline")
					g.parent.releaseReturns()
					await(t, g.parent.exited, "the return of the timed-out Write")
					synctest.Wait()
					if ack.poll() {
						t.Fatalf("AcknowledgeDrain returned %v while its cancellation was still setting the deadline", ack.err)
					}
					// Virtual time: the setter's return comes strictly after the Write's return.
					time.Sleep(time.Millisecond)
					slow.release()
				}
				err := ack.wait(t, "AcknowledgeDrain")
				if err == nil {
					t.Fatal("AcknowledgeDrain reported success for a Write that timed out")
				}
				switch {
				case tt.setterFirst:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("AcknowledgeDrain = %v; the cancellation's deadline was in force before the Write returned, so the cancellation wins: want %v", err, context.Canceled)
					}
				case tt.callerDeadline:
					if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
						t.Errorf("AcknowledgeDrain = %v; the Write timed out on the caller's own deadline before the cancellation's deadline was applied: want %v and not %v", err, context.DeadlineExceeded, context.Canceled)
					}
				default:
					if diff := gocmp.Diff(classification(budgetErr), classification(err)); diff != "" {
						t.Errorf("the Write timed out on the budget before the cancellation's deadline was applied, yet its error is classified differently from the budget's own error (-budget +got):\n%s\nbudget's own error: %v\nerror: %v", diff, budgetErr, err)
					}
				}
				checkFailedAckKeeps(t, r.reg, g.gatedReservation)
			})
		})
	}
}

// errLateSetter is the failure of the cancellation's deadline call in TestAcknowledgeDrainNoCloseAfterFinishedWrite.
var errLateSetter = errors.New("the deadline call failed after the write had finished")

// TestAcknowledgeDrainNoCloseAfterFinishedWrite checks a cancellation whose deadline call fails only after the write
// has finished. The Write is held at the gate, the caller cancels, and the cancellation's deadline call is held at its
// entry. Then the gate opens, the whole line goes out and the Write returns its full length; only after that does the
// deadline call return its error. The write no longer needs ending, so the parent end, which the manager is still
// reading, must not be closed (no Close call at all), and the acknowledgment, whose line went out completely,
// returns nil and releases the listener.
func TestAcknowledgeDrainNoCloseAfterFinishedWrite(t *testing.T) {
	tests := map[string]struct {
		generation workerlink.Generation
	}{
		"success: a cancellation setter that fails after the Write finished closes nothing": {generation: "gen-a"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			guardedBubble(t, func(t *testing.T, at func(string)) {
				at("setting up the reservation")
				r := newWrappedRegistry(t, nil)
				var slow *slowSetterConn
				g := r.reserve(t, tt.generation, 41001, 4101, reserveOptions{wrap: func(gc *gatedConn) net.Conn {
					slow = newSlowSetterConn(gc, errLateSetter)
					return slow
				}})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ack := startAck(ctx, g.res)
				at("waiting for the acknowledgment's Write")
				await(t, g.parent.entered, "the acknowledgment's write")
				cancel()
				at("the cancellation's deadline call held at its entry")
				await(t, slow.setterIn, "the cancellation's deadline call on the parent end")
				at("the Write finishes")
				g.parent.openGate()
				await(t, g.parent.exited, "the return of the Write")
				synctest.Wait()
				at("the deadline call fails after the Write finished")
				slow.release()
				err := ack.wait(t, "AcknowledgeDrain")
				if n := slow.closes.Load(); n != 0 || parentClosed(g.parent) {
					t.Errorf("the parent end was closed (%d Close calls) after its Write had finished; a deadline call that fails then must not start the fallback Close", n)
				}
				if err != nil {
					t.Errorf("AcknowledgeDrain = %v; want nil, the whole line went out before the cancellation could end the Write", err)
				}
				want := frame("retired", string(tt.generation))
				if got := readExactly(t, g.child, len(want)); got != want {
					t.Errorf("acknowledgment %q, want %q", got, want)
				}
				if err == nil && !isClosed(g.listener.closed) {
					t.Error("the acknowledgment succeeded but did not release the listener")
				}
			})
		})
	}
}
