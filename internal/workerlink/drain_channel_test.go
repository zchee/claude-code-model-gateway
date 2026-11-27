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
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// drainChannel takes the reservation's drain channel and returns the parent connection and the child's end as
// the worker would hold it after import; the transferred file itself is closed, as after a start.
func drainChannel(t *testing.T, res *workerlink.Reservation) (parent, child net.Conn) {
	t.Helper()
	parent, file, err := res.DrainChannel()
	if file != nil {
		t.Cleanup(func() { _ = file.Close() })
	}
	if err != nil {
		t.Fatalf("DrainChannel: %v", err)
	}
	if parent == nil || file == nil {
		t.Fatalf("DrainChannel = %v, %v; want both ends", parent, file)
	}
	child = fileConn(t, file)
	if err := file.Close(); err != nil {
		t.Fatalf("closing the transferred file: %v", err)
	}
	return parent, child
}

func writeAll(t *testing.T, c net.Conn, s string) {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(watchdog))
	if _, err := c.Write([]byte(s)); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
	_ = c.SetWriteDeadline(time.Time{})
}

// TestDrainChannel checks the drain channel a reservation creates: the child's end is a close-on-exec, connected,
// non-listening local stream socket; both directions carry bytes; the channel can be taken only once; once every
// handle of the child's end is closed the parent reads end of file, so the reservation keeps no copy of the child's
// end; and End closes the parent's end.
func TestDrainChannel(t *testing.T) {
	listens := newFakeListens()
	reg := workerlink.NewRegistry(listens.listen, (&immediateDials{t: t}).dial)
	res := reserve(t, reg, "gen-a", 41001)
	parent, file, err := res.DrainChannel()
	if file != nil {
		t.Cleanup(func() { _ = file.Close() })
	}
	if err != nil {
		t.Fatalf("DrainChannel: %v", err)
	}
	if parent == nil || file == nil {
		t.Fatalf("DrainChannel = %v, %v; want both ends", parent, file)
	}
	if diff := gocmp.Diff(socketKind{Family: "unix", Type: unix.SOCK_STREAM}, describeSocket(t, file)); diff != "" {
		t.Errorf("child end (-want +got):\n%s", diff)
	}
	checkCloseOnExec(t, file, "child end of the drain channel")
	if again, f, err := res.DrainChannel(); err == nil || again != nil || f != nil {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("second DrainChannel = %v, %v, %v; want an error and no handles", again, f, err)
	}
	child := fileConn(t, file)
	writeAll(t, parent, "to-child\n")
	if got := readExactly(t, child, 9); got != "to-child\n" {
		t.Errorf("child read %q", got)
	}
	writeAll(t, child, "to-parent\n")
	if got := readExactly(t, parent, 10); got != "to-parent\n" {
		t.Errorf("parent read %q", got)
	}
	// The transferred file and the test's connection over it are every handle of the child's end the caller got.
	if err := child.Close(); err != nil {
		t.Fatalf("closing the child's connection: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing the transferred file: %v", err)
	}
	_ = parent.SetReadDeadline(time.Now().Add(watchdog))
	if n, err := parent.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("parent read with the reservation live and the child's end closed = %d, %v; want end of file, the reservation must hold no copy of the child's end", n, err)
	}
	_ = parent.SetReadDeadline(time.Time{})

	// End closes the parent's end: a second reservation whose child end stays open reads end of file at End.
	other := reserve(t, reg, "gen-b", 41002)
	_, otherChild := drainChannel(t, other)
	if err := other.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	_ = otherChild.SetReadDeadline(time.Now().Add(watchdog))
	if n, err := otherChild.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("child read after End = %d, %v; want end of file, End closes the parent's end", n, err)
	}
}

// TestAcknowledgeDrain checks the graceful path on real sockets: after retirement the acknowledgment writes
// exactly one retired line for this generation and releases the supervisor's copy of the listener; it leaves the
// lease and its Exited channel alone, never reopens admission, and a repeated call writes nothing.
func TestAcknowledgeDrain(t *testing.T) {
	reg := workerlink.NewRegistry(realListen, realDial)
	res := reserve(t, reg, "gen-a", 0)
	port := res.Port()
	childL := childListener(t, res)
	_, child := drainChannel(t, res)
	publish(t, res, 4101)
	lease := acquire(t, reg, "")
	res.Retire()
	// A lease taken before the retirement, such as the one the supervisor sends its drain request on, still works.
	writeAll(t, lease, "after retirement\n")
	if got := readExactly(t, acceptOne(t, childL), 17); got != "after retirement\n" {
		t.Fatalf("the worker read %q on a lease taken before the retirement", got)
	}
	// The worker's copy is gone, so from here only the supervisor's copy holds the address.
	if err := childL.Close(); err != nil {
		t.Fatalf("closing the child's copy: %v", err)
	}
	checkBindRefused(t, port, "before the acknowledgment")
	if err := res.AcknowledgeDrain(t.Context()); err != nil {
		t.Fatalf("AcknowledgeDrain: %v", err)
	}
	want := frame("retired", "gen-a")
	if got := readExactly(t, child, len(want)); got != want {
		t.Errorf("acknowledgment %q, want %q", got, want)
	}
	bindAndKeep(t, port, "after the acknowledgment")
	_ = res.AcknowledgeDrain(t.Context())
	if n := pending(t, child.(syscall.Conn)); n != 0 {
		t.Errorf("%d more bytes on the drain channel after a repeated acknowledgment; it must write one line only", n)
	}
	if isClosed(lease.Exited()) {
		t.Error("the acknowledgment closed Exited")
	}
	if err := lease.Close(); err != nil {
		t.Errorf("lease Close after the acknowledgment: %v; the acknowledgment must not close leases", err)
	}
	if l, err := reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, workerlink.ErrRetired) {
		t.Errorf("Acquire after the acknowledgment = %v, %v; want %v", l, err, workerlink.ErrRetired)
	}
	if err := res.End(); err != nil {
		t.Errorf("End: %v", err)
	}
}

// ackFixture is a published generation with dials parked on it and the child's end of its drain channel.
type ackFixture struct {
	reg      *workerlink.Registry
	res      *workerlink.Reservation
	listener *fakeListener
	dials    *gatedDials
	attempts []*dialAttempt
	// out receives the outcome of every parked Acquire, in the order the dials settle.
	out   chan acquireResult
	child net.Conn
}

// newAckFixture must run inside a bubble. It parks pinned acquisitions on the generation before it returns.
func newAckFixture(t *testing.T, honourCancel bool, pinned int) *ackFixture {
	t.Helper()
	listens := newFakeListens()
	f := &ackFixture{dials: newGatedDials(honourCancel), out: make(chan acquireResult, pinned)}
	f.reg = workerlink.NewRegistry(listens.listen, f.dials.dial)
	f.res = reserve(t, f.reg, "gen-a", 41001)
	f.listener = listens.at(t, 41001)
	_, f.child = drainChannel(t, f.res)
	publish(t, f.res, 4101)
	t.Cleanup(f.dials.release)
	t.Cleanup(func() { listens.checkNeverAccepted(t) })
	ctx := t.Context()
	for range pinned {
		go func() {
			lease, err := f.reg.Acquire(ctx, "")
			f.out <- acquireResult{lease, err}
		}()
		f.attempts = append(f.attempts, <-f.dials.attempts)
	}
	return f
}

func ackAsync(ctx context.Context, res *workerlink.Reservation) <-chan error {
	out := make(chan error, 1)
	go func() { out <- res.AcknowledgeDrain(ctx) }()
	return out
}

// checkNoAcknowledgment asserts, at a point where the bubble is quiescent, that no acknowledgment has returned,
// nothing is on the drain channel and the listener is still held.
func (f *ackFixture) checkNoAcknowledgment(t *testing.T, when string, acks ...<-chan error) {
	t.Helper()
	synctest.Wait()
	for i, acked := range acks {
		if err, ok := ready(acked); ok {
			t.Fatalf("%s: acknowledgment %d returned %v", when, i+1, err)
		}
	}
	if n := pending(t, f.child.(syscall.Conn)); n != 0 {
		t.Fatalf("%s: %d bytes on the drain channel", when, n)
	}
	if isClosed(f.listener.closed) {
		t.Fatalf("%s: the listener was released", when)
	}
}

// TestAcknowledgeDrainWaitsForPinnedDial checks that the acknowledgment waits for every dial that pinned the
// generation before its retirement, one that succeeds and one that fails; that two acknowledgers racing behind
// the pins write one line in total; that the complete line is on the channel before the listener is released;
// and that the handed-over lease survives the acknowledgment.
func TestAcknowledgeDrainWaitsForPinnedDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		errDial := errors.New("connect failed")
		f := newAckFixture(t, false, 2)
		want := frame("retired", "gen-a")
		var atRelease atomic.Int64
		atRelease.Store(-2)
		f.listener.onClose(func() {
			n, err := peekPending(f.child.(syscall.Conn))
			if err != nil {
				n = -1
			}
			atRelease.Store(int64(n))
		})
		f.res.Retire()
		first, second := ackAsync(t.Context(), f.res), ackAsync(t.Context(), f.res)
		f.checkNoAcknowledgment(t, "with two pinned dials in progress", first, second)

		conn := newTrackedConn(t)
		answer(f.attempts[0], dialReply{conn: conn})
		handed := await(t, f.out, "the Acquire whose dial succeeded")
		if handed.err != nil || handed.lease == nil {
			t.Fatalf("pinned Acquire = %+v; want the handed-over lease", handed)
		}
		t.Cleanup(func() { _ = handed.lease.Close() })
		f.checkNoAcknowledgment(t, "with one pinned dial still in progress", first, second)

		answer(f.attempts[1], dialReply{err: errDial})
		if failed := await(t, f.out, "the Acquire whose dial failed"); failed.lease != nil || !errors.Is(failed.err, errDial) {
			t.Fatalf("pinned Acquire = %+v; want no lease and %v", failed, errDial)
		}
		errs := []error{await(t, first, "the first acknowledgment"), await(t, second, "the second acknowledgment")}
		if errs[0] != nil && errs[1] != nil {
			t.Fatalf("both acknowledgments failed: %v", errs)
		}
		if n := pending(t, f.child.(syscall.Conn)); n != len(want) {
			t.Fatalf("%d bytes on the drain channel after two acknowledgments, want the one %d-byte line", n, len(want))
		}
		if got := readExactly(t, f.child, len(want)); got != want {
			t.Errorf("acknowledgment %q, want %q", got, want)
		}
		if !isClosed(f.listener.closed) {
			t.Error("the acknowledgment did not release the supervisor's listener")
		}
		if got := atRelease.Load(); got != int64(len(want)) {
			t.Errorf("bytes on the drain channel when the listener was released = %d, want the complete %d-byte line", got, len(want))
		}
		if isClosed(conn.closed) || isClosed(handed.lease.Exited()) {
			t.Error("the acknowledgment closed the handed-over lease or its Exited channel")
		}
	})
}

// TestAcknowledgeDrainRefuses checks the acknowledgments that must not be sent: the caller gives up or the
// generation ends while a pinned dial is in progress, or admission was never retired. None writes a line or
// releases the listener on its own, and none reopens admission.
func TestAcknowledgeDrainRefuses(t *testing.T) {
	tests := map[string]struct {
		honourCancel bool
		pinned       int
		retire       bool
		interrupt    func(t *testing.T, f *ackFixture, cancel context.CancelFunc)
		want         error
		admission    error
	}{
		"error: caller cancels while a dial is pinned": {
			pinned:    1,
			retire:    true,
			interrupt: func(_ *testing.T, _ *ackFixture, cancel context.CancelFunc) { cancel() },
			want:      context.Canceled,
			admission: workerlink.ErrRetired,
		},
		"error: generation ends while a dial is pinned": {
			honourCancel: true,
			pinned:       1,
			retire:       true,
			interrupt: func(t *testing.T, f *ackFixture, _ context.CancelFunc) {
				if err := f.res.End(); err != nil {
					t.Errorf("End: %v", err)
				}
			},
			admission: workerlink.ErrUnavailable,
		},
		"error: admission was never retired": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newAckFixture(t, tt.honourCancel, tt.pinned)
				if tt.retire {
					f.res.Retire()
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				acked := ackAsync(ctx, f.res)
				if tt.interrupt != nil {
					f.checkNoAcknowledgment(t, "with a pinned dial in progress", acked)
					tt.interrupt(t, f, cancel)
				}
				err := await(t, acked, "AcknowledgeDrain")
				if err == nil {
					t.Fatal("AcknowledgeDrain reported success")
				}
				if tt.want != nil && !errors.Is(err, tt.want) {
					t.Errorf("AcknowledgeDrain error = %v, want %v", err, tt.want)
				}
				if n := pending(t, f.child.(syscall.Conn)); n != 0 {
					t.Errorf("%d bytes written on the drain channel by a failed acknowledgment", n)
				}
				ended := errors.Is(tt.admission, workerlink.ErrUnavailable)
				if !ended && isClosed(f.listener.closed) {
					t.Error("a failed acknowledgment released the listener")
				}
				if tt.admission == nil {
					return
				}
				if l, err := f.reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, tt.admission) {
					t.Errorf("Acquire after the failed acknowledgment = %v, %v; want %v", l, err, tt.admission)
				}
			})
		})
	}
}

// TestAcknowledgeDrainBoundedWhenPeerStopsReading checks an acknowledgment whose line cannot be written because
// the worker stopped reading: the test saturates the reservation's own channel until it refuses a single byte.
// The acknowledgment then ends when the caller cancels (with no deadline, so only the caller's cancellation can
// end it), when the caller's deadline passes, or when the generation ends. While it is blocked, unrelated
// registry work completes. A failed acknowledgment writes no byte, keeps the listener and keeps admission closed.
func TestAcknowledgeDrainBoundedWhenPeerStopsReading(t *testing.T) {
	tests := map[string]struct {
		// context returns the caller's context; interrupt ends the blocked acknowledgment.
		context   func(ctx context.Context) (context.Context, context.CancelFunc)
		interrupt func(t *testing.T, res *workerlink.Reservation, cancel context.CancelFunc)
		want      error
		ended     bool
	}{
		"error: caller cancels a context without a deadline": {
			context:   context.WithCancel,
			interrupt: func(_ *testing.T, _ *workerlink.Reservation, cancel context.CancelFunc) { cancel() },
			want:      context.Canceled,
		},
		"error: caller's deadline passes": {
			context: func(ctx context.Context) (context.Context, context.CancelFunc) {
				return context.WithTimeout(ctx, 200*time.Millisecond)
			},
		},
		"error: generation ends": {
			context: context.WithCancel,
			interrupt: func(t *testing.T, res *workerlink.Reservation, _ context.CancelFunc) {
				within(t, "End during a blocked acknowledgment", func() {
					if err := res.End(); err != nil {
						t.Errorf("End: %v", err)
					}
				})
			},
			ended: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			reg := workerlink.NewRegistry(realListen, realDial)
			res := reserve(t, reg, "gen-a", 0)
			port := res.Port()
			if err := childListener(t, res).Close(); err != nil {
				t.Fatalf("closing the child's copy: %v", err)
			}
			parent, child := drainChannel(t, res)
			// The test stands in for a worker that stopped reading.
			filled := fillUntilBlocked(t, parent)
			publish(t, res, 4101)
			res.Retire()
			ctx, cancel := tt.context(t.Context())
			defer cancel()
			acked := ackAsync(ctx, res)

			// Unrelated registry work does not wait for the blocked channel write.
			var other *workerlink.Reservation
			within(t, "Reserve during a blocked acknowledgment", func() {
				var err error
				if other, err = reg.Reserve(t.Context(), "gen-b", 0); err != nil {
					t.Errorf("Reserve gen-b: %v", err)
				}
			})
			if other == nil {
				t.FailNow()
			}
			endAtCleanup(t, other)
			within(t, "Publish and Acquire during a blocked acknowledgment", func() {
				if err := other.Publish(4102); err != nil {
					t.Errorf("Publish gen-b: %v", err)
					return
				}
				lease, err := reg.Acquire(t.Context(), "")
				if err != nil || lease == nil || lease.Generation() != "gen-b" {
					t.Errorf("Acquire = %v, %v; want a lease to gen-b", lease, err)
				}
				if lease != nil {
					_ = lease.Close()
				}
			})
			within(t, "End of another generation during a blocked acknowledgment", func() {
				if err := other.End(); err != nil {
					t.Errorf("End gen-b: %v", err)
				}
			})

			if tt.interrupt != nil {
				tt.interrupt(t, res, cancel)
			}
			err := await(t, acked, "AcknowledgeDrain")
			if err == nil {
				t.Fatal("AcknowledgeDrain reported success for a line that did not fit")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("AcknowledgeDrain error = %v, want %v", err, tt.want)
			}
			// The channel held exactly the filler: the failed acknowledgment wrote no byte of its line.
			if got := readExactly(t, child, filled); len(got) != filled {
				t.Fatalf("read %d filler bytes, want %d", len(got), filled)
			}
			if n := pending(t, child.(syscall.Conn)); n != 0 {
				t.Errorf("%d bytes after the filler; a failed acknowledgment must write nothing", n)
			}
			if tt.ended {
				bindAndKeep(t, port, "after End")
				if l, err := reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, workerlink.ErrUnavailable) {
					t.Errorf("Acquire after End = %v, %v; want %v", l, err, workerlink.ErrUnavailable)
				}
				return
			}
			checkBindRefused(t, port, "after a failed acknowledgment")
			if l, err := reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, workerlink.ErrRetired) {
				t.Errorf("Acquire after the failed acknowledgment = %v, %v; want %v", l, err, workerlink.ErrRetired)
			}
		})
	}
}
