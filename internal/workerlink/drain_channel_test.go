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
	"os"
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
// non-listening local stream socket; both directions carry bytes; the channel can be taken only once; and End
// closes the parent's end.
func TestDrainChannel(t *testing.T) {
	listens := newFakeListens()
	reg := workerlink.NewRegistry(listens.listen, (&immediateDials{t: t}).dial)
	res := reserve(t, reg, "gen-a", 41001)
	parent, file, err := res.DrainChannel()
	if err != nil {
		t.Fatalf("DrainChannel: %v", err)
	}
	if parent == nil || file == nil {
		t.Fatalf("DrainChannel = %v, %v; want both ends", parent, file)
	}
	t.Cleanup(func() { _ = file.Close() })
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
	if err := res.End(); err != nil {
		t.Fatalf("End: %v", err)
	}
	_ = child.SetReadDeadline(time.Now().Add(watchdog))
	if n, err := child.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("child read after End = %d, %v; want end of file, End closes the parent's end", n, err)
	}
}

// TestAcknowledgeDrain checks the graceful path on real sockets: after retirement the acknowledgment writes
// exactly one retired line for this generation and only then releases the supervisor's copy of the listener;
// it leaves the lease and its Exited channel alone, never reopens admission, and a repeated call writes nothing.
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

// ackFixture is a published generation with one dial parked on it and the child's end of its drain channel.
type ackFixture struct {
	reg      *workerlink.Registry
	res      *workerlink.Reservation
	listener *fakeListener
	dials    *gatedDials
	attempt  *dialAttempt
	out      <-chan acquireResult
	child    net.Conn
}

// newAckFixture must run inside a bubble.
func newAckFixture(t *testing.T, honourCancel, pinned bool) *ackFixture {
	t.Helper()
	listens := newFakeListens()
	f := &ackFixture{dials: newGatedDials(honourCancel)}
	f.reg = workerlink.NewRegistry(listens.listen, f.dials.dial)
	f.res = reserve(t, f.reg, "gen-a", 41001)
	f.listener = listens.at(t, 41001)
	_, f.child = drainChannel(t, f.res)
	publish(t, f.res, 4101)
	t.Cleanup(f.dials.release)
	t.Cleanup(func() { listens.checkNeverAccepted(t) })
	if pinned {
		f.out = acquireAsync(t.Context(), f.reg, "")
		f.attempt = <-f.dials.attempts
	}
	return f
}

func ackAsync(ctx context.Context, res *workerlink.Reservation) <-chan error {
	out := make(chan error, 1)
	go func() { out <- res.AcknowledgeDrain(ctx) }()
	return out
}

// TestAcknowledgeDrainWaitsForPinnedDial checks that the acknowledgment waits for a dial that pinned the
// generation before its retirement: until that dial is handed over nothing is written and the listener stays,
// and the handed-over lease survives the acknowledgment.
func TestAcknowledgeDrainWaitsForPinnedDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newAckFixture(t, false, true)
		f.res.Retire()
		acked := ackAsync(t.Context(), f.res)
		synctest.Wait()
		if err, ok := ready(acked); ok {
			t.Fatalf("AcknowledgeDrain returned %v while a pinned dial was in progress", err)
		}
		if n := pending(t, f.child.(syscall.Conn)); n != 0 {
			t.Fatalf("%d bytes written on the drain channel before the pinned dial finished", n)
		}
		if isClosed(f.listener.closed) {
			t.Fatal("the listener was released before the pinned dial finished")
		}
		conn := newTrackedConn(t)
		answer(f.attempt, dialReply{conn: conn})
		synctest.Wait()
		r, ok := ready(f.out)
		if !ok || r.err != nil || r.lease == nil {
			t.Fatalf("pinned Acquire = %+v (returned %t); want the handed-over lease", r, ok)
		}
		t.Cleanup(func() { _ = r.lease.Close() })
		err, ok := ready(acked)
		if !ok {
			t.Fatal("AcknowledgeDrain did not return after the pinned dial was handed over")
		}
		if err != nil {
			t.Fatalf("AcknowledgeDrain: %v", err)
		}
		want := frame("retired", "gen-a")
		if n := pending(t, f.child.(syscall.Conn)); n != len(want) {
			t.Fatalf("%d bytes on the drain channel, want the %d-byte acknowledgment", n, len(want))
		}
		if got := readExactly(t, f.child, len(want)); got != want {
			t.Errorf("acknowledgment %q, want %q", got, want)
		}
		if !isClosed(f.listener.closed) {
			t.Error("the acknowledgment did not release the supervisor's listener")
		}
		if isClosed(conn.closed) || isClosed(r.lease.Exited()) {
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
		pinned       bool
		retire       bool
		interrupt    func(t *testing.T, f *ackFixture, cancel context.CancelFunc)
		want         error
		admission    error
	}{
		"error: caller cancels while a dial is pinned": {
			pinned:    true,
			retire:    true,
			interrupt: func(_ *testing.T, _ *ackFixture, cancel context.CancelFunc) { cancel() },
			want:      context.Canceled,
			admission: workerlink.ErrRetired,
		},
		"error: generation ends while a dial is pinned": {
			honourCancel: true,
			pinned:       true,
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
				synctest.Wait()
				if tt.interrupt != nil {
					if err, ok := ready(acked); ok {
						t.Fatalf("AcknowledgeDrain returned %v while a pinned dial was in progress", err)
					}
					tt.interrupt(t, f, cancel)
					synctest.Wait()
				}
				err, ok := ready(acked)
				if !ok {
					t.Fatal("AcknowledgeDrain did not return")
				}
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

// fillUntilBlocked writes to c until the peer's buffers are full, then clears the write deadline.
func fillUntilBlocked(t *testing.T, c net.Conn) {
	t.Helper()
	chunk := make([]byte, 64<<10)
	for range 1 << 12 {
		_ = c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := c.Write(chunk); err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("filling the drain channel: %v", err)
			}
			_ = c.SetWriteDeadline(time.Time{})
			return
		}
	}
	t.Fatal("the drain channel never filled")
}

// TestAcknowledgeDrainBoundedWhenPeerStopsReading checks that the acknowledgment returns an error within the
// caller's deadline when the worker no longer reads its channel, keeps the listener and keeps admission closed.
func TestAcknowledgeDrainBoundedWhenPeerStopsReading(t *testing.T) {
	reg := workerlink.NewRegistry(realListen, realDial)
	res := reserve(t, reg, "gen-a", 0)
	port := res.Port()
	if err := childListener(t, res).Close(); err != nil {
		t.Fatalf("closing the child's copy: %v", err)
	}
	parent, _ := drainChannel(t, res)
	// The test stands in for a worker that stopped reading: it fills the channel so that no further write fits.
	fillUntilBlocked(t, parent)
	publish(t, res, 4101)
	res.Retire()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	acked := ackAsync(ctx, res)
	var err error
	select {
	case err = <-acked:
	case <-time.After(watchdog):
		t.Fatal("AcknowledgeDrain did not return when its peer stopped reading")
	}
	if err == nil {
		t.Fatal("AcknowledgeDrain reported success for a write that did not fit")
	}
	checkBindRefused(t, port, "after a failed acknowledgment")
	if l, err := reg.Acquire(t.Context(), ""); l != nil || !errors.Is(err, workerlink.ErrRetired) {
		t.Errorf("Acquire after the failed acknowledgment = %v, %v; want %v", l, err, workerlink.ErrRetired)
	}
}
