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
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// acquireResult is the outcome of an Acquire run in its own goroutine.
type acquireResult struct {
	lease workerlink.Lease
	err   error
}

func acquireAsync(ctx context.Context, src workerlink.Source, exclude workerlink.Generation) <-chan acquireResult {
	out := make(chan acquireResult, 1)
	go func() {
		lease, err := src.Acquire(ctx, exclude)
		out <- acquireResult{lease, err}
	}()
	return out
}

// ready returns the value on ch when it is already there.
func ready[T any](ch <-chan T) (T, bool) {
	select {
	case v := <-ch:
		return v, true
	default:
		var zero T
		return zero, false
	}
}

// release answers every dial not answered yet with an error, so that a failing test cannot leave a dial parked.
func (d *gatedDials) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.all {
		answer(a, dialReply{err: errors.New("test finished")})
	}
}

// answer answers one parked attempt; a reply channel holds one answer, so a second answer is dropped.
func answer(a *dialAttempt, r dialReply) {
	select {
	case a.reply <- r:
	default:
	}
}

// TestAcquireAdmission checks when the registry refuses a lease: nothing reserved, nothing published, the only
// eligible generation excluded, a retired generation, an ended one, and a caller whose context was cancelled or
// whose deadline has passed. A refusal gives no lease, answers without advancing the bubble's clock (the relay owns
// any waiting) and, except for the caller that has gone, never dials.
func TestAcquireAdmission(t *testing.T) {
	tests := map[string]struct {
		setup   func(t *testing.T, reg *workerlink.Registry)
		exclude workerlink.Generation
		// context derives the caller's context; nil keeps the test's.
		context func(t *testing.T, ctx context.Context) context.Context
		want    error
	}{
		"error: nothing reserved": {
			setup: func(*testing.T, *workerlink.Registry) {},
			want:  workerlink.ErrUnavailable,
		},
		"error: reserved but not published": {
			setup: func(t *testing.T, reg *workerlink.Registry) { reserve(t, reg, "gen-a", 41001) },
			want:  workerlink.ErrUnavailable,
		},
		"error: the only eligible generation is excluded": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				publish(t, reserve(t, reg, "gen-a", 41001), 4101)
			},
			exclude: "gen-a",
			want:    workerlink.ErrUnavailable,
		},
		"error: process identifier zero is not published": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				if err := reserve(t, reg, "gen-a", 41001).Publish(0); err == nil {
					t.Error("Publish(0) succeeded")
				}
			},
			want: workerlink.ErrUnavailable,
		},
		"error: negative process identifier is not published": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				if err := reserve(t, reg, "gen-a", 41001).Publish(-1); err == nil {
					t.Error("Publish(-1) succeeded")
				}
			},
			want: workerlink.ErrUnavailable,
		},
		"error: retired": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				res := reserve(t, reg, "gen-a", 41001)
				publish(t, res, 4101)
				res.Retire()
			},
			want: workerlink.ErrRetired,
		},
		"error: retired twice": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				res := reserve(t, reg, "gen-a", 41001)
				publish(t, res, 4101)
				res.Retire()
				res.Retire()
			},
			want: workerlink.ErrRetired,
		},
		"error: retired generation published again": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				res := reserve(t, reg, "gen-a", 41001)
				publish(t, res, 4101)
				res.Retire()
				if err := res.Publish(4101); !errors.Is(err, workerlink.ErrRetired) {
					t.Errorf("Publish after Retire = %v, want %v; a retired reservation is never published again", err, workerlink.ErrRetired)
				}
			},
			want: workerlink.ErrRetired,
		},
		"error: retired before publication": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				res := reserve(t, reg, "gen-a", 41001)
				res.Retire()
				if err := res.Publish(4101); !errors.Is(err, workerlink.ErrRetired) {
					t.Errorf("Publish after Retire = %v, want %v; a late publication must not reopen admission", err, workerlink.ErrRetired)
				}
			},
			want: workerlink.ErrRetired,
		},
		"error: ended": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				res := reserve(t, reg, "gen-a", 41001)
				publish(t, res, 4101)
				if err := res.End(); err != nil {
					t.Fatalf("End: %v", err)
				}
			},
			want: workerlink.ErrUnavailable,
		},
		"error: retired and then ended": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				res := reserve(t, reg, "gen-a", 41001)
				publish(t, res, 4101)
				res.Retire()
				if err := res.End(); err != nil {
					t.Fatalf("End: %v", err)
				}
			},
			want: workerlink.ErrUnavailable,
		},
		"error: caller already cancelled": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				publish(t, reserve(t, reg, "gen-a", 41001), 4101)
			},
			context: func(_ *testing.T, ctx context.Context) context.Context {
				ctx, cancel := context.WithCancel(ctx)
				cancel()
				return ctx
			},
			want: context.Canceled,
		},
		"error: caller's deadline already passed": {
			setup: func(t *testing.T, reg *workerlink.Registry) {
				publish(t, reserve(t, reg, "gen-a", 41001), 4101)
			},
			context: func(t *testing.T, ctx context.Context) context.Context {
				ctx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			want: context.DeadlineExceeded,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				listens := newFakeListens()
				dials := &immediateDials{t: t}
				reg := workerlink.NewRegistry(listens.listen, dials.dial)
				tt.setup(t, reg)
				ctx := t.Context()
				if tt.context != nil {
					ctx = tt.context(t, ctx)
				}
				start := time.Now()
				lease, err := reg.Acquire(ctx, tt.exclude)
				if waited := time.Since(start); waited != 0 {
					t.Errorf("Acquire took %v of the bubble's time; a refusal answers at once", waited)
				}
				if lease != nil {
					_ = lease.Close()
					t.Errorf("Acquire returned a lease to %q", lease.Generation())
				}
				if !errors.Is(err, tt.want) {
					t.Fatalf("Acquire error = %v, want %v", err, tt.want)
				}
				if tt.context == nil && dials.count() != 0 {
					t.Errorf("a refused Acquire dialed %d times", dials.count())
				}
				listens.checkNeverAccepted(t)
			})
		})
	}
}

// TestAcquireLease checks the lease of an eligible generation: its fields, the one dial to the reserved address,
// its connection, an open Exited channel and Close releasing the connection.
func TestAcquireLease(t *testing.T) {
	listens := newFakeListens()
	dials := &immediateDials{t: t}
	reg := workerlink.NewRegistry(listens.listen, dials.dial)
	publish(t, reserve(t, reg, "gen-a", 41001), 4101)
	lease := acquire(t, reg, "")
	if diff := gocmp.Diff(leaseFields{"gen-a", 4101, "127.0.0.1:41001"}, fieldsOf(lease)); diff != "" {
		t.Errorf("lease (-want +got):\n%s", diff)
	}
	if len(dials.calls) != 1 || dials.calls[0].Address != "127.0.0.1:41001" || (dials.calls[0].Network != "tcp4" && dials.calls[0].Network != "tcp") {
		t.Fatalf("dials = %+v, want one TCP dial of 127.0.0.1:41001", dials.calls)
	}
	conn := dials.conns[0]
	go func() { _, _ = lease.Write([]byte("x")) }()
	_ = conn.peer.SetReadDeadline(time.Now().Add(watchdog))
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn.peer, buf); err != nil || string(buf) != "x" {
		t.Fatalf("peer read %q, %v; the lease must be the dialed connection", buf, err)
	}
	if isClosed(lease.Exited()) {
		t.Error("Exited is closed for a live generation")
	}
	if err := lease.Close(); err != nil {
		t.Errorf("lease Close: %v", err)
	}
	if !isClosed(conn.closed) {
		t.Error("closing the lease left its connection open")
	}
	listens.checkNeverAccepted(t)
}

// TestAcquireDialFailure checks that a failed dial is reported with its cause and gives no lease, and that it
// does not withdraw the generation: the next Acquire dials again.
func TestAcquireDialFailure(t *testing.T) {
	errDial := errors.New("connect failed")
	listens := newFakeListens()
	dials := &immediateDials{t: t, err: errDial}
	reg := workerlink.NewRegistry(listens.listen, dials.dial)
	publish(t, reserve(t, reg, "gen-a", 41001), 4101)
	lease, err := reg.Acquire(t.Context(), "")
	if lease != nil {
		_ = lease.Close()
	}
	if lease != nil || !errors.Is(err, errDial) {
		t.Fatalf("Acquire = %v, %v; want no lease and %v", lease, err, errDial)
	}
	dials.mu.Lock()
	dials.err = nil
	dials.mu.Unlock()
	if got := acquire(t, reg, "").Generation(); got != "gen-a" {
		t.Errorf("lease after a failed dial reached %q, want gen-a", got)
	}
	if dials.count() != 2 {
		t.Errorf("dials = %d, want 2", dials.count())
	}
}

// TestRetireKeepsPinnedDial checks a dial that had pinned the generation before a drain retired it: retirement
// neither cancels it nor marks the generation exited, no new dial starts, and the dial's own outcome decides.
// A success is handed over as a lease to the retired generation; a failure is an error.
func TestRetireKeepsPinnedDial(t *testing.T) {
	errDial := errors.New("connect failed")
	tests := map[string]struct {
		fail bool
	}{
		"success: dial completes after retirement": {},
		"error: dial fails after retirement":       {fail: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				listens := newFakeListens()
				dials := newGatedDials(false)
				reg := workerlink.NewRegistry(listens.listen, dials.dial)
				res := reserve(t, reg, "gen-a", 41001)
				publish(t, res, 4101)
				t.Cleanup(dials.release)
				out := acquireAsync(t.Context(), reg, "")
				attempt := <-dials.attempts
				res.Retire()
				synctest.Wait()
				if attempt.ctx.Err() != nil {
					t.Errorf("Retire cancelled a dial that had already pinned the generation: %v", attempt.ctx.Err())
				}
				if r, ok := ready(out); ok {
					if r.lease != nil {
						_ = r.lease.Close()
					}
					t.Fatalf("Acquire returned %+v before its dial completed", r)
				}
				if lease, err := reg.Acquire(t.Context(), ""); lease != nil || !errors.Is(err, workerlink.ErrRetired) {
					if lease != nil {
						_ = lease.Close()
					}
					t.Fatalf("Acquire after Retire = %v, %v; want %v", lease, err, workerlink.ErrRetired)
				}
				if n := dials.count.Load(); n != 1 {
					t.Errorf("dials = %d after retirement, want 1", n)
				}
				conn := newTrackedConn(t)
				if tt.fail {
					answer(attempt, dialReply{err: errDial})
				} else {
					answer(attempt, dialReply{conn: conn})
				}
				r := <-out
				if r.lease != nil {
					t.Cleanup(func() { _ = r.lease.Close() })
				}
				synctest.Wait()
				if tt.fail {
					if r.lease != nil || !errors.Is(r.err, errDial) {
						t.Fatalf("Acquire = %v, %v; want no lease and %v", r.lease, r.err, errDial)
					}
					return
				}
				if r.err != nil || r.lease == nil {
					t.Fatalf("Acquire = %v, %v; a dial that pinned before retirement must be handed over", r.lease, r.err)
				}
				if diff := gocmp.Diff(leaseFields{"gen-a", 4101, "127.0.0.1:41001"}, fieldsOf(r.lease)); diff != "" {
					t.Errorf("handed-over lease (-want +got):\n%s", diff)
				}
				if isClosed(conn.closed) {
					t.Error("the handed-over connection was closed")
				}
				if isClosed(r.lease.Exited()) {
					t.Error("Exited is closed after a graceful retirement")
				}
				listens.checkNeverAccepted(t)
			})
		})
	}
}

// exitWaiters starts one waiter per lease on the Exited channel each lease returns now. A waiter reports whether
// its receive saw a closed channel, so that one value sent on a channel cannot pass for closure.
func exitWaiters(leases ...workerlink.Lease) <-chan bool {
	woke := make(chan bool, len(leases))
	for _, lease := range leases {
		ch := lease.Exited()
		go func() {
			_, ok := <-ch
			woke <- !ok
		}()
	}
	return woke
}

// checkExitWaitersWoke waits for n waiters and asserts that each one saw its channel closed.
func checkExitWaitersWoke(t *testing.T, woke <-chan bool, n int) {
	t.Helper()
	for i := range n {
		if closed := await(t, woke, "an Exited waiter"); !closed {
			t.Errorf("Exited waiter %d received a value from an open channel; End must close Exited", i+1)
		}
	}
}

// TestExitedFollowsEndNotRetire checks that waiters on the Exited channels of two leases stay asleep through
// retirement and all wake at End, because End closes those channels.
func TestExitedFollowsEndNotRetire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		listens := newFakeListens()
		dials := &immediateDials{t: t}
		reg := workerlink.NewRegistry(listens.listen, dials.dial)
		res := reserve(t, reg, "gen-a", 41001)
		publish(t, res, 4101)
		leases := []workerlink.Lease{acquire(t, reg, ""), acquire(t, reg, "")}
		woke := exitWaiters(leases...)
		res.Retire()
		synctest.Wait()
		if _, ok := ready(woke); ok {
			t.Fatal("an Exited waiter woke at retirement")
		}
		if err := res.End(); err != nil {
			t.Fatalf("End: %v", err)
		}
		checkExitWaitersWoke(t, woke, len(leases))
		for i, lease := range leases {
			checkClosedEachTime(t, lease.Exited, fmt.Sprintf("Exited of lease %d after End", i+1))
		}
	})
}

// TestEndCancelsPinnedDial checks End against a dial in progress on a generation that ends without a drain: End
// denies admission at once, cancels the dial, keeps the listener and does not return while the dial runs, closes
// a connection that completes anyway, and wakes the Exited waiters of every earlier lease. The pending Acquire
// may fail at once or after its dial settles; it never returns a lease.
func TestEndCancelsPinnedDial(t *testing.T) {
	tests := map[string]struct {
		honourCancel bool
	}{
		"error: dial honours the cancellation":           {honourCancel: true},
		"error: dial completes despite the cancellation": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				listens := newFakeListens()
				dials := newGatedDials(tt.honourCancel)
				reg := workerlink.NewRegistry(listens.listen, dials.dial)
				res := reserve(t, reg, "gen-a", 41001)
				publish(t, res, 4101)
				t.Cleanup(dials.release)
				var earlier []workerlink.Lease
				for i := range 2 {
					first := acquireAsync(t.Context(), reg, "")
					answer(<-dials.attempts, dialReply{conn: newTrackedConn(t)})
					r := await(t, first, "an earlier Acquire")
					if r.lease != nil {
						t.Cleanup(func() { _ = r.lease.Close() })
					}
					if r.err != nil || r.lease == nil {
						t.Fatalf("earlier Acquire %d = %v, %v", i+1, r.lease, r.err)
					}
					earlier = append(earlier, r.lease)
				}
				woke := exitWaiters(earlier...)
				out := acquireAsync(t.Context(), reg, "")
				attempt := <-dials.attempts
				listener := listens.at(t, 41001)
				ended := make(chan error, 1)
				go func() { ended <- res.End() }()
				await(t, attempt.ctx.Done(), "the cancellation of the dial in progress")
				late := newTrackedConn(t)
				var r acquireResult
				settled := false
				if !tt.honourCancel {
					synctest.Wait()
					// The pending Acquire may already have given up; it must not have produced a lease.
					if r, settled = ready(out); settled && r.lease != nil {
						_ = r.lease.Close()
						t.Fatalf("Acquire handed over a lease to %q after End", r.lease.Generation())
					}
					if err, ok := ready(ended); ok {
						t.Fatalf("End returned %v while a dial still pinned the generation", err)
					}
					if isClosed(listener.closed) {
						t.Fatal("End released the listener while a dial still pinned it")
					}
					before := dials.count.Load()
					if lease, err := reg.Acquire(t.Context(), ""); lease != nil || err == nil {
						if lease != nil {
							_ = lease.Close()
						}
						t.Fatalf("Acquire while End waits for the dial = %v, %v; End denies admission first", lease, err)
					}
					if n := dials.count.Load(); n != before {
						t.Errorf("Acquire while End waits for the dial dialed %d times", n-before)
					}
					answer(attempt, dialReply{conn: late})
				}
				if !settled {
					r = await(t, out, "the pending Acquire")
				}
				if r.lease != nil {
					_ = r.lease.Close()
					t.Fatalf("Acquire handed over a lease to %q after End", r.lease.Generation())
				}
				if r.err == nil {
					t.Fatal("Acquire returned neither a lease nor an error")
				}
				if err := await(t, ended, "End"); err != nil {
					t.Errorf("End: %v", err)
				}
				if !tt.honourCancel && !isClosed(late.closed) {
					t.Error("the connection that completed after End was left open")
				}
				if !isClosed(listener.closed) {
					t.Error("End did not release the listener")
				}
				checkExitWaitersWoke(t, woke, len(earlier))
				listens.checkNeverAccepted(t)
			})
		})
	}
}

// TestEndIsFinal checks that an ended reservation stays ended: a repeated End and a late Retire are harmless and
// the cleanup happens once, and Publish and DrainChannel refuse without handing out anything. What a repeated End
// returns is not decided here. File after End is decided on a real listener in TestReservedAddressHeldUntilEnd.
func TestEndIsFinal(t *testing.T) {
	listens := newFakeListens()
	dials := &immediateDials{t: t}
	reg := workerlink.NewRegistry(listens.listen, dials.dial)
	res := reserve(t, reg, "gen-a", 41001)
	if err := res.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	_ = res.End()
	listener := listens.at(t, 41001)
	if !isClosed(listener.closed) {
		t.Error("End did not release the listener")
	}
	if n := listener.closings.Load(); n != 1 {
		t.Errorf("the listener was closed %d times by two Ends, want once", n)
	}
	res.Retire()
	if err := res.Publish(4101); err == nil {
		t.Error("Publish after End succeeded")
	}
	if parent, child, err := res.DrainChannel(); err == nil || parent != nil || child != nil {
		if child != nil {
			_ = child.Close()
		}
		t.Errorf("DrainChannel after End = %v, %v, %v; want an error and no handles", parent, child, err)
	}
	if lease, err := reg.Acquire(t.Context(), ""); lease != nil || !errors.Is(err, workerlink.ErrUnavailable) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Errorf("Acquire after End = %v, %v; want %v", lease, err, workerlink.ErrUnavailable)
	}
	if dials.count() != 0 {
		t.Errorf("dials = %d, want 0", dials.count())
	}
}

// within runs op and fails the test if it does not return while the test still holds a dial parked. The
// watchdog only aborts a hang; the evidence is that op returned.
func within(t *testing.T, what string, op func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		op()
	}()
	select {
	case <-done:
	case <-time.After(watchdog):
		t.Fatalf("%s did not return while a dial was in progress; no lock may be held across network I/O", what)
	}
}

// TestNoLockHeldAcrossDial checks that a dial in progress blocks none of the registry's other operations:
// another Acquire, Reserve, Retire, Publish and End all return while the test still holds the dial.
func TestNoLockHeldAcrossDial(t *testing.T) {
	listens := newFakeListens()
	gated := newGatedDials(false)
	immediate := &immediateDials{t: t}
	var calls atomic.Int32
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if calls.Add(1) == 1 {
			return gated.dial(ctx, network, address)
		}
		return immediate.dial(ctx, network, address)
	}
	reg := workerlink.NewRegistry(listens.listen, dial)
	a := reserve(t, reg, "gen-a", 41001)
	publish(t, a, 4101)
	t.Cleanup(gated.release)
	out := acquireAsync(t.Context(), reg, "")
	var attempt *dialAttempt
	select {
	case attempt = <-gated.attempts:
	case <-time.After(watchdog):
		t.Fatal("the first Acquire never dialed")
	}
	var second acquireResult
	within(t, "a second Acquire", func() { second.lease, second.err = reg.Acquire(t.Context(), "") })
	if second.lease != nil {
		t.Cleanup(func() { _ = second.lease.Close() })
	}
	if second.err != nil || second.lease == nil || second.lease.Generation() != "gen-a" {
		t.Fatalf("second Acquire = %v, %v; want a lease to gen-a", second.lease, second.err)
	}
	var c, b *workerlink.Reservation
	var errC, errB, errPublish error
	within(t, "Reserve", func() { c, errC = reg.Reserve(t.Context(), "gen-c", 41003) })
	if c != nil {
		endAtCleanup(t, c)
	}
	if errC != nil {
		t.Fatalf("Reserve gen-c: %v", errC)
	}
	within(t, "Retire", a.Retire)
	within(t, "Reserve and Publish", func() {
		if b, errB = reg.Reserve(t.Context(), "gen-b", 41002); errB == nil {
			errPublish = b.Publish(4102)
		}
	})
	if b != nil {
		endAtCleanup(t, b)
	}
	if errB != nil || errPublish != nil {
		t.Fatalf("Reserve and Publish gen-b: %v, %v", errB, errPublish)
	}
	var replacement acquireResult
	within(t, "Acquire of the replacement", func() { replacement.lease, replacement.err = reg.Acquire(t.Context(), "") })
	if replacement.lease != nil {
		t.Cleanup(func() { _ = replacement.lease.Close() })
	}
	if replacement.err != nil || replacement.lease == nil || replacement.lease.Generation() != "gen-b" {
		t.Fatalf("Acquire after the replacement = %v, %v; want a lease to gen-b", replacement.lease, replacement.err)
	}
	var errEnd error
	within(t, "End of another generation", func() { errEnd = c.End() })
	if errEnd != nil {
		t.Errorf("End gen-c: %v", errEnd)
	}
	answer(attempt, dialReply{conn: newTrackedConn(t)})
	var r acquireResult
	select {
	case r = <-out:
	case <-time.After(watchdog):
		t.Fatal("the parked Acquire did not return after its dial completed")
	}
	if r.lease != nil {
		t.Cleanup(func() { _ = r.lease.Close() })
	}
	if r.err != nil || r.lease == nil {
		t.Fatalf("parked Acquire = %v, %v; its dial pinned gen-a before retirement", r.lease, r.err)
	}
	if got := r.lease.Generation(); got != "gen-a" {
		t.Errorf("parked lease reached %q, want gen-a", got)
	}
}
