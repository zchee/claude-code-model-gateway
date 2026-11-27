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
	"runtime/debug"
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
	want := socketKind{Family: "unix", Type: unix.SOCK_STREAM, Listening: wantListening(false), Connected: true}
	if diff := gocmp.Diff(want, describeSocket(t, file)); diff != "" {
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
// exactly one retired line for its own generation and releases the supervisor's copy of the listener; it leaves the
// lease issued before it working in both directions and its Exited channel untouched until End closes it, never
// reopens admission, and a repeated call writes nothing. A second reservation's acknowledgment carries that reservation's generation and
// reaches only that reservation's channel.
func TestAcknowledgeDrain(t *testing.T) {
	reg := workerlink.NewRegistry(realListen, realDial)
	res := reserve(t, reg, "gen-a", 0)
	port := res.Port()
	childL := childListener(t, res)
	_, child := drainChannel(t, res)
	publish(t, res, 4101)
	lease := acquire(t, reg, "")
	// The worker's end of the lease, such as the connection the supervisor sends its drain request on.
	worker := acceptOne(t, childL)
	res.Retire()
	writeAll(t, lease, "after retirement\n")
	if got := readExactly(t, worker, 17); got != "after retirement\n" {
		t.Fatalf("the worker read %q on a lease taken before the retirement", got)
	}
	// The worker's copy is gone, so from here only the supervisor's copy holds the address.
	if err := childL.Close(); err != nil {
		t.Fatalf("closing the child's copy: %v", err)
	}
	checkBindRefused(t, port, "before the acknowledgment")
	exited := lease.Exited()
	woke := exitWaiters(lease)
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
	// The lease issued before the acknowledgment still carries bytes both ways, and its Exited channel, captured
	// before the acknowledgment, neither closed nor delivered a value.
	writeAll(t, lease, "after the acknowledgment\n")
	if got := readExactly(t, worker, 25); got != "after the acknowledgment\n" {
		t.Errorf("the worker read %q on the lease after the acknowledgment", got)
	}
	writeAll(t, worker, "answer\n")
	if got := readExactly(t, lease, 7); got != "answer\n" {
		t.Errorf("the lease read %q after the acknowledgment", got)
	}
	if !untouched(exited) {
		t.Error("the Exited channel captured before the acknowledgment closed or delivered a value")
	}
	if _, ok := ready(woke); ok {
		t.Error("an Exited waiter started before the acknowledgment woke")
	}
	if l, err := reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, workerlink.ErrRetired) {
		if l != nil {
			_ = l.Close()
		}
		t.Errorf("Acquire after the acknowledgment = %v, %v; want %v", l, err, workerlink.ErrRetired)
	}

	// A second reservation, with a generation of another length, acknowledged on its own channel.
	second := reserve(t, reg, "gen-b-second", 0)
	if err := childListener(t, second).Close(); err != nil {
		t.Fatalf("closing the second child's copy: %v", err)
	}
	_, secondChild := drainChannel(t, second)
	publish(t, second, 4102)
	second.Retire()
	if err := second.AcknowledgeDrain(t.Context()); err != nil {
		t.Fatalf("AcknowledgeDrain of the second reservation: %v", err)
	}
	wantSecond := frame("retired", "gen-b-second")
	if got := readExactly(t, secondChild, len(wantSecond)); got != wantSecond {
		t.Errorf("second acknowledgment %q, want %q", got, wantSecond)
	}
	if n := pending(t, secondChild.(syscall.Conn)); n != 0 {
		t.Errorf("%d more bytes on the second channel after its own line", n)
	}
	if n := pending(t, child.(syscall.Conn)); n != 0 {
		t.Errorf("%d bytes reached the first channel from the second reservation's acknowledgment", n)
	}
	if err := res.End(); err != nil {
		t.Errorf("End: %v", err)
	}
	// The waiter captured before the acknowledgment saw nothing until End; End wakes it by closing the channel, so
	// its receive reports closure. A value it received earlier, before it could tell, would be reported here instead.
	checkExitWaitersWoke(t, woke, 1)
	if err := second.End(); err != nil {
		t.Errorf("End of the second reservation: %v", err)
	}
}

// ackRun is one AcknowledgeDrain call running in its own goroutine; poll and wait record its result once.
type ackRun struct {
	done     chan error
	returned bool
	err      error
	// at is the time AcknowledgeDrain returned and writing the number of Write calls on the watched parent end still
	// in progress then, both taken in the calling goroutine the moment the call returned, before anything else runs
	// there. They are set before the result is sent, so they may be read once poll or wait has seen it.
	at      time.Time
	writing int
}

// startAck starts AcknowledgeDrain; parent, when not nil, is the gated parent end whose Writes in progress are
// counted at the return.
func startAck(ctx context.Context, res *workerlink.Reservation, parent *gatedConn) *ackRun {
	a := &ackRun{done: make(chan error, 1)}
	go func() {
		err := res.AcknowledgeDrain(ctx)
		if parent != nil {
			a.writing = parent.writing()
		}
		a.at = time.Now()
		a.done <- err
	}()
	return a
}

// poll reports whether the call has returned, recording its result.
func (a *ackRun) poll() bool {
	if !a.returned {
		if err, ok := ready(a.done); ok {
			a.returned, a.err = true, err
		}
	}
	return a.returned
}

func (a *ackRun) wait(t *testing.T, what string) error {
	t.Helper()
	if !a.returned {
		a.err = await(t, a.done, what)
		a.returned = true
	}
	return a.err
}

// ackFixture is a published generation with dials parked on it, the gated parent end of its drain channel and the
// child's end.
type ackFixture struct {
	reg      *workerlink.Registry
	res      *workerlink.Reservation
	listener *fakeListener
	dials    *gatedDials
	attempts []*dialAttempt
	// out[i] receives the outcome of the Acquire whose dial is attempts[i].
	out    []chan acquireResult
	parent *gatedConn
	child  net.Conn
}

// newAckFixture must run inside a bubble. It parks pinned acquisitions on the generation before it returns.
func newAckFixture(t *testing.T, honourCancel bool, pinned int) *ackFixture {
	t.Helper()
	listens := newFakeListens()
	f := &ackFixture{dials: newGatedDials(honourCancel)}
	f.reg = workerlink.NewRegistry(listens.listen, f.dials.dial)
	pairs := newDrainPairs(t, f.reg, true, 0)
	f.res = reserve(t, f.reg, "gen-a", 41001)
	// Registered after End's cleanup, so it runs first: no Write stays parked when an assertion fails.
	t.Cleanup(pairs.closeGated)
	f.parent = pairs.gatedAt(t, 0)
	f.listener = listens.at(t, 41001)
	_, f.child = drainChannel(t, f.res)
	publish(t, f.res, 4101)
	t.Cleanup(f.dials.release)
	t.Cleanup(func() { listens.checkNeverAccepted(t) })
	ctx := t.Context()
	for range pinned {
		out := make(chan acquireResult, 1)
		go func() {
			lease, err := f.reg.Acquire(ctx, "")
			out <- acquireResult{lease, err}
		}()
		f.out = append(f.out, out)
		f.attempts = append(f.attempts, <-f.dials.attempts)
	}
	return f
}

// checkBarrier asserts, at a point where the bubble is quiescent, that the acknowledgment barrier holds: no
// acknowledgment returned success, no Write of the drain channel began, and the listener is still held. With
// racing, one of several acknowledgers may already have failed (the loser of the race may give up at any time), but
// not every one; without it, no acknowledgment returned at all.
func (f *ackFixture) checkBarrier(t *testing.T, when string, racing bool, acks ...*ackRun) {
	t.Helper()
	synctest.Wait()
	returned := 0
	for i, a := range acks {
		if !a.poll() {
			continue
		}
		returned++
		if a.err == nil || !racing {
			t.Fatalf("%s: acknowledgment %d returned %v", when, i+1, a.err)
		}
	}
	if returned == len(acks) {
		t.Fatalf("%s: every acknowledgment returned an error", when)
	}
	if offered, writes := f.parent.attempts(); writes != 0 {
		t.Fatalf("%s: %d Write calls on the drain channel (%q)", when, writes, offered)
	}
	if isClosed(f.listener.closed) {
		t.Fatalf("%s: the listener was released", when)
	}
}

// settle answers the pinned dial i, with a connection or with errDial, and returns the handed-over lease and its
// connection on success.
func (f *ackFixture) settle(t *testing.T, i int, errDial error) (workerlink.Lease, *trackedConn) {
	t.Helper()
	if errDial != nil {
		answer(f.attempts[i], dialReply{err: errDial})
		r := await(t, f.out[i], fmt.Sprintf("the pinned Acquire %d whose dial failed", i))
		if r.lease != nil {
			_ = r.lease.Close()
			t.Fatalf("pinned Acquire %d handed over a lease for a failed dial", i)
		}
		if !errors.Is(r.err, errDial) {
			t.Fatalf("pinned Acquire %d = %v; want %v", i, r.err, errDial)
		}
		return nil, nil
	}
	conn := newTrackedConn(t)
	answer(f.attempts[i], dialReply{conn: conn})
	r := await(t, f.out[i], fmt.Sprintf("the pinned Acquire %d whose dial succeeded", i))
	if r.lease != nil {
		t.Cleanup(func() { _ = r.lease.Close() })
	}
	if r.err != nil || r.lease == nil {
		t.Fatalf("pinned Acquire %d = %v, %v; want the handed-over lease", i, r.lease, r.err)
	}
	return r.lease, conn
}

// TestAcknowledgeDrainWaitsForPinnedDial checks that the acknowledgment waits for every dial that pinned the
// generation before its retirement, whichever settles first and whether the first succeeds or fails, while the other
// is shown to be still open; that of two acknowledgers racing behind the pins neither succeeds before the pins have
// settled and the Write that carries the line's last byte has gone through (the test holds that Write, however the
// line is split into Writes; the bytes before it go through), at least one then succeeds and one line is offered in
// total; that the complete line is on the channel before the listener is released; and that the handed-over lease
// keeps working and its Exited channel stays untouched. A real-time watchdog outside the bubble turns a lock that
// parks a goroutine of the bubble for good into a failure.
func TestAcknowledgeDrainWaitsForPinnedDial(t *testing.T) {
	tests := map[string]struct {
		// first is the pinned dial that settles first; firstFails makes it fail and the other succeed.
		first      int
		firstFails bool
	}{
		"success: the older pin settles first with a lease":       {first: 0},
		"success: the older pin settles first with a failed dial": {first: 0, firstFails: true},
		"success: the newer pin settles first with a lease":       {first: 1},
		"success: the newer pin settles first with a failed dial": {first: 1, firstFails: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var step atomic.Pointer[string]
			at := func(s string) { step.Store(&s) }
			stop := abortIfStuck(t.Name(), bubbleWatchdog, &step)
			defer stop()
			synctest.Test(t, func(t *testing.T) {
				errDial := errors.New("connect failed")
				at("parking the two pinned acquisitions")
				f := newAckFixture(t, false, 2)
				want := frame("retired", "gen-a")
				f.parent.holdFrom(len(want) - 1)
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
				at("two acknowledgments behind two pinned dials")
				acks := []*ackRun{startAck(t.Context(), f.res, f.parent), startAck(t.Context(), f.res, f.parent)}
				f.checkBarrier(t, "with two pinned dials in progress", true, acks...)

				outcome := func(fails bool) error {
					if fails {
						return errDial
					}
					return nil
				}
				other := 1 - tt.first
				at("settling the first pinned dial")
				handed, handedConn := f.settle(t, tt.first, outcome(tt.firstFails))
				// The other pin is still open: its dial is parked unanswered and uncancelled, its Acquire pending.
				synctest.Wait()
				if r, ok := ready(f.out[other]); ok {
					if r.lease != nil {
						_ = r.lease.Close()
					}
					t.Fatalf("the other pinned Acquire returned %+v before its dial was answered", r)
				}
				if err := f.attempts[other].ctx.Err(); err != nil {
					t.Fatalf("the other pinned dial was cancelled: %v", err)
				}
				f.checkBarrier(t, "with one pinned dial still in progress", true, acks...)
				at("settling the other pinned dial")
				if lease, conn := f.settle(t, other, outcome(!tt.firstFails)); lease != nil {
					handed, handedConn = lease, conn
				}
				exited := handed.Exited()
				woke := exitWaiters(handed)

				// Every pin has settled: an acknowledgment now writes its line, and the test holds the Write that carries
				// its last byte.
				at("waiting for the Write that carries the line's last byte")
				await(t, f.parent.entered, "the Write that carries the line's last byte")
				synctest.Wait()
				for i, a := range acks {
					if a.poll() && a.err == nil {
						t.Fatalf("acknowledgment %d succeeded before the Write that carries the line's last byte went through", i+1)
					}
				}
				if isClosed(f.listener.closed) {
					t.Fatal("the listener was released before the Write that carries the line's last byte went through")
				}
				at("the acknowledgments after the gate opened")
				f.parent.openGate()
				errs := []error{acks[0].wait(t, "the first acknowledgment"), acks[1].wait(t, "the second acknowledgment")}
				if errs[0] != nil && errs[1] != nil {
					t.Fatalf("both acknowledgments failed: %v", errs)
				}
				if offered, _ := f.parent.attempts(); offered != want {
					t.Errorf("bytes offered to the drain channel %q, want the one line %q", offered, want)
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

				// The handed-over lease is left alone: its Exited channel and waiter are untouched, it carries bytes.
				at("the checks on the handed-over lease")
				synctest.Wait()
				if _, ok := ready(woke); ok || !untouched(exited) {
					t.Error("the acknowledgment closed or signalled the handed-over lease's Exited channel")
				}
				wrote := make(chan error, 1)
				go func() {
					_, err := handed.Write([]byte("request\n"))
					wrote <- err
				}()
				if got := readExactly(t, handedConn.peer, 8); got != "request\n" {
					t.Errorf("the worker read %q on the handed-over lease", got)
				}
				if err := await(t, wrote, "the write on the handed-over lease"); err != nil {
					t.Errorf("write on the handed-over lease after the acknowledgment: %v", err)
				}
				if isClosed(handedConn.closed) {
					t.Error("the acknowledgment closed the handed-over lease's connection")
				}
			})
		})
	}
}

// TestAcknowledgeDrainRefuses checks the acknowledgments that must not be sent: the caller gives up or the
// generation ends while a pinned dial is in progress, or admission was never retired. None calls Write on the drain
// channel or releases the listener on its own, and none reopens admission.
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
				ack := startAck(ctx, f.res, f.parent)
				if tt.interrupt != nil {
					f.checkBarrier(t, "with a pinned dial in progress", false, ack)
					tt.interrupt(t, f, cancel)
				}
				err := ack.wait(t, "AcknowledgeDrain")
				if err == nil {
					t.Fatal("AcknowledgeDrain reported success")
				}
				if tt.want != nil && !errors.Is(err, tt.want) {
					t.Errorf("AcknowledgeDrain error = %v, want %v", err, tt.want)
				}
				if offered, writes := f.parent.attempts(); writes != 0 {
					t.Errorf("a failed acknowledgment called Write %d times (%q); it must write nothing", writes, offered)
				}
				ended := errors.Is(tt.admission, workerlink.ErrUnavailable)
				if !ended && isClosed(f.listener.closed) {
					t.Error("a failed acknowledgment released the listener")
				}
				if tt.admission == nil {
					return
				}
				if l, err := f.reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, tt.admission) {
					if l != nil {
						_ = l.Close()
					}
					t.Errorf("Acquire after the failed acknowledgment = %v, %v; want %v", l, err, tt.admission)
				}
			})
		})
	}
}

// TestAcknowledgeDrainBoundedWhenPeerStopsReading checks, on a real socket pair, an acknowledgment whose line cannot
// be written because the worker stopped reading: the test saturates the reservation's own channel through its own
// handle on the parent end, until a nonblocking one-byte send there returns EAGAIN. With a context that never ends
// the acknowledgment still returns an error, within its own budget for the write; when the generation ends it
// returns an error too. The child then reads exactly the filler: no byte of the line was written. The listener is
// kept and admission stays retired, except after End.
func TestAcknowledgeDrainBoundedWhenPeerStopsReading(t *testing.T) {
	tests := map[string]struct {
		interrupt func(t *testing.T, res *workerlink.Reservation)
		ended     bool
	}{
		"error: the worker reads nothing and the context never ends": {},
		"error: the generation ends": {
			interrupt: func(t *testing.T, res *workerlink.Reservation) {
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
			pairs := newDrainPairs(t, reg, false, 0)
			res := reserve(t, reg, "gen-a", 0)
			port := res.Port()
			if err := childListener(t, res).Close(); err != nil {
				t.Fatalf("closing the child's copy: %v", err)
			}
			_, child := drainChannel(t, res)
			sc, ok := pairs.parent(t, 0).(syscall.Conn)
			if !ok {
				t.Fatalf("the real parent end is a %T without a descriptor", pairs.parent(t, 0))
			}
			// The test stands in for a worker that stopped reading.
			filled := saturate(t, sc)
			if refused, sent := refusesOneByte(t, sc); !refused {
				t.Fatalf("a one-byte send after saturation took %d byte(s)", sent)
			}
			publish(t, res, 4101)
			res.Retire()
			ack := startAck(t.Context(), res, nil)
			if tt.interrupt != nil {
				tt.interrupt(t, res)
			}
			// The watchdog of await is longer than the five seconds the write may take; it only aborts a hang.
			if err := ack.wait(t, "AcknowledgeDrain"); err == nil {
				t.Fatal("AcknowledgeDrain reported success for a line that did not fit")
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
					if l != nil {
						_ = l.Close()
					}
					t.Errorf("Acquire after End = %v, %v; want %v", l, err, workerlink.ErrUnavailable)
				}
				return
			}
			checkBindRefused(t, port, "after a failed acknowledgment")
			if l, err := reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, workerlink.ErrRetired) {
				if l != nil {
					_ = l.Close()
				}
				t.Errorf("Acquire after the failed acknowledgment = %v, %v; want %v", l, err, workerlink.ErrRetired)
			}
		})
	}
}

// bubbleWatchdog bounds in real time one bubble of a test that may park a goroutine on a lock of the code under test.
// A goroutine waiting for a sync.Mutex is not durably blocked for synctest, so while one waits the bubble's clock, and
// every watchdog inside the bubble with it, stands still; only a timer outside the bubble can then end the test.
// Correct code finishes such a bubble in milliseconds of real time, so a loaded runner does not come near this bound.
const bubbleWatchdog = 2 * time.Minute

// abortIfStuck starts a real-time watchdog outside any bubble. Unless stop is called within limit, it ends the test
// binary with a message naming the step recorded last and a dump of every goroutine, which shows what was still
// blocked. It proves nothing; it only turns a hang into a failure.
func abortIfStuck(what string, limit time.Duration, step *atomic.Pointer[string]) (stop func()) {
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			current := "no step recorded"
			if s := step.Load(); s != nil {
				current = *s
			}
			debug.SetTraceback("all")
			panic(fmt.Sprintf("%s: still blocked after %v of real time, at the step %q; a goroutine of the bubble waits for a lock that is not released, which stops the bubble's clock", what, limit, current))
		}
	}()
	return func() { close(done) }
}

// writeBudget is the most the acknowledgment's write may take: the five seconds the worker allows for the whole
// exchange. The exact budget below it is the implementation's.
const writeBudget = 5 * time.Second

// gatedReservation is a published, retired reservation whose drain channel's parent end is gated.
type gatedReservation struct {
	res      *workerlink.Reservation
	parent   *gatedConn
	listener *fakeListener
}

// reserveGated reserves generation on port with the next pair of pairs, takes its drain channel, publishes it for
// pid and retires it.
func reserveGated(t *testing.T, reg *workerlink.Registry, pairs *drainPairs, listens *fakeListens, generation workerlink.Generation, port uint16, pid int) gatedReservation {
	t.Helper()
	res := reserve(t, reg, generation, port)
	parent := pairs.gatedAt(t, pairs.count()-1)
	// Registered after End's cleanup, so it runs first: no Write stays parked when an assertion fails.
	t.Cleanup(func() { _ = parent.Close() })
	drainChannel(t, res)
	publish(t, res, pid)
	res.Retire()
	return gatedReservation{res: res, parent: parent, listener: listens.at(t, port)}
}

// TestAcknowledgeDrainWriteIsBounded checks, in a bubble and through a gated parent end, an acknowledgment that has
// begun its write to a worker that reads nothing. The test sees the Write begin, then runs unrelated registry work
// (Reserve, Publish, Acquire and End of another generation), which must complete while that Write is still in progress
// and the acknowledgment has not returned, so no registry lock is held across the write. The write then ends: with a
// context that never ends, through the acknowledgment's own budget, at most five seconds after the Write began; at
// once with the caller's cancellation, with that context's error; at the caller's deadline, with
// context.DeadlineExceeded, when that deadline provably comes first; and at once when the generation ends. Whatever
// ends it, no Write of the acknowledgment is still in progress when AcknowledgeDrain returns. A failed acknowledgment
// keeps the listener and admission retired, except after End. That bound is enforced by a write deadline or by
// closing the channel; the gated end honours both. The time a correct implementation lets pass before it calls Write
// is its own: every bound is taken from the Write's entry, never from the call.
func TestAcknowledgeDrainWriteIsBounded(t *testing.T) {
	tests := map[string]struct {
		// callerDeadline gives the caller's context a deadline inside the write: after the Write begins and before
		// the acknowledgment's own budget would end it, both observed first on a sibling reservation in the same
		// bubble.
		callerDeadline bool
		// interrupt ends the write; nil leaves it to the budget or to the caller's deadline.
		interrupt func(t *testing.T, res *workerlink.Reservation, cancel context.CancelFunc)
		// atOnce requires the acknowledgment to return without the bubble's clock moving after the interrupt.
		atOnce bool
		// budget requires the acknowledgment to return at most writeBudget after its Write began.
		budget bool
		want   error
		ended  bool
	}{
		"error: the budget is spent with a context that never ends": {
			budget: true,
		},
		"error: the caller cancels while the write waits": {
			interrupt: func(_ *testing.T, _ *workerlink.Reservation, cancel context.CancelFunc) { cancel() },
			atOnce:    true,
			want:      context.Canceled,
		},
		"error: the caller's deadline passes while the write waits": {
			callerDeadline: true,
		},
		"error: the generation ends while the write waits": {
			interrupt: func(t *testing.T, res *workerlink.Reservation, _ context.CancelFunc) {
				if err := res.End(); err != nil {
					t.Errorf("End: %v", err)
				}
			},
			atOnce: true,
			ended:  true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var step atomic.Pointer[string]
			at := func(s string) { step.Store(&s) }
			stop := abortIfStuck(t.Name(), bubbleWatchdog, &step)
			defer stop()
			synctest.Test(t, func(t *testing.T) {
				at("setting up the registry")
				listens := newFakeListens()
				reg := workerlink.NewRegistry(listens.listen, (&immediateDials{t: t}).dial)
				pairs := newDrainPairs(t, reg, true, 0)
				t.Cleanup(func() { listens.checkNeverAccepted(t) })

				var ctx context.Context
				var cancel context.CancelFunc
				// deadlineInside is set when the caller's deadline was placed strictly inside the write.
				deadlineInside := false
				if tt.callerDeadline {
					at("observing the budget on a sibling reservation")
					sibling := reserveGated(t, reg, pairs, listens, "gen-sibling", 41003, 4103)
					called := time.Now()
					sibAck := startAck(t.Context(), sibling.res, sibling.parent)
					await(t, sibling.parent.entered, "the sibling acknowledgment's write")
					if err := sibAck.wait(t, "the sibling AcknowledgeDrain"); err == nil {
						t.Fatal("the sibling AcknowledgeDrain reported success for a line the worker never read")
					}
					if sibAck.writing != 0 {
						t.Fatalf("%d Write calls of the sibling acknowledgment were still in progress when it returned", sibAck.writing)
					}
					entry, exit := sibling.parent.heldSpan()
					beforeWrite, window := entry.Sub(called), exit.Sub(entry)
					if err := sibling.res.End(); err != nil {
						t.Fatalf("End of the sibling: %v", err)
					}
					if half := window / 2; half > 0 {
						deadlineInside = true
						ctx, cancel = context.WithTimeout(t.Context(), beforeWrite+half)
					} else {
						// The budget leaves no time inside the write for a deadline to come first; which of the two
						// wins is then not decided, and only an error is required.
						t.Logf("the budget ended the write %v after it began; no caller deadline fits inside it", window)
						ctx, cancel = context.WithCancel(t.Context())
					}
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				defer cancel()

				at("reserving gen-a")
				g := reserveGated(t, reg, pairs, listens, "gen-a", 41001, 4101)
				ack := startAck(ctx, g.res, g.parent)
				at("waiting for the acknowledgment's write to begin")
				await(t, g.parent.entered, "the acknowledgment's write")
				entry, _ := g.parent.heldSpan()

				// Unrelated registry work while the write waits.
				at("Reserve of gen-b during the write")
				other, err := reg.Reserve(t.Context(), "gen-b", 41002)
				if other != nil {
					endAtCleanup(t, other)
				}
				if err != nil {
					t.Fatalf("Reserve gen-b during the write: %v", err)
				}
				at("Publish of gen-b during the write")
				if err := other.Publish(4102); err != nil {
					t.Fatalf("Publish gen-b during the write: %v", err)
				}
				at("Acquire during the write")
				lease, err := reg.Acquire(t.Context(), "")
				if lease != nil {
					t.Cleanup(func() { _ = lease.Close() })
				}
				if err != nil || lease == nil || lease.Generation() != "gen-b" {
					t.Fatalf("Acquire during the write = %v, %v; want a lease to gen-b", lease, err)
				}
				at("End of gen-b during the write")
				if err := other.End(); err != nil {
					t.Fatalf("End gen-b during the write: %v", err)
				}
				if isClosed(g.parent.exited) || ack.poll() {
					_, exit := g.parent.heldSpan()
					t.Fatalf("the unrelated registry work completed only after the acknowledgment's write ended (at %v after it began); it waited for the write", exit.Sub(entry))
				}
				synctest.Wait()
				if ack.poll() {
					t.Fatalf("AcknowledgeDrain returned %v before anything ended its write", ack.err)
				}

				interrupted := time.Now()
				at("the interruption of the write")
				if tt.interrupt != nil {
					tt.interrupt(t, g.res, cancel)
				}
				at("waiting for AcknowledgeDrain to return")
				err = ack.wait(t, "AcknowledgeDrain")
				at("the checks after the acknowledgment")
				if err == nil {
					t.Fatal("AcknowledgeDrain reported success for a line the worker never read")
				}
				if ack.writing != 0 {
					t.Errorf("%d Write calls of the acknowledgment were still in progress when it returned; an interrupted or expired write must have left Write", ack.writing)
				}
				if tt.want != nil && !errors.Is(err, tt.want) {
					t.Errorf("AcknowledgeDrain error = %v, want %v", err, tt.want)
				}
				if waited := ack.at.Sub(interrupted); tt.atOnce && waited != 0 {
					t.Errorf("AcknowledgeDrain returned %v of the bubble's time after it was interrupted, want at once", waited)
				}
				if took := ack.at.Sub(entry); tt.budget && took > writeBudget {
					t.Errorf("AcknowledgeDrain returned %v of the bubble's time after its write began, want at most %v", took, writeBudget)
				}
				if deadline, ok := ctx.Deadline(); deadlineInside && ok {
					switch {
					case ack.at.Before(deadline):
						// The acknowledgment's own budget ended the write first; its own error stands.
					case ack.at.Equal(deadline):
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Errorf("AcknowledgeDrain at the caller's deadline, which came before its budget = %v, want %v", err, context.DeadlineExceeded)
						}
					default:
						t.Errorf("AcknowledgeDrain returned %v after the caller's deadline", ack.at.Sub(deadline))
					}
				}
				if tt.ended {
					if !isClosed(g.listener.closed) {
						t.Error("End did not release the listener")
					}
					if l, err := reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, workerlink.ErrUnavailable) {
						if l != nil {
							_ = l.Close()
						}
						t.Errorf("Acquire after End = %v, %v; want %v", l, err, workerlink.ErrUnavailable)
					}
					return
				}
				if isClosed(g.listener.closed) {
					t.Error("a failed acknowledgment released the listener")
				}
				if l, err := reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, workerlink.ErrRetired) {
					if l != nil {
						_ = l.Close()
					}
					t.Errorf("Acquire after the failed acknowledgment = %v, %v; want %v", l, err, workerlink.ErrRetired)
				}
			})
		})
	}
}
