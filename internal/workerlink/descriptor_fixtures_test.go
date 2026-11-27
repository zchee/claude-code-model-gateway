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
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// The tests in this file check the observations the package's tests rely on, against the operating system and
// the fakes alone, so that a wrong observation cannot pass for a wrong implementation. They call nothing of the
// package under test.

func TestFixturePeekCountsWithoutConsuming(t *testing.T) {
	a, b := socketPair(t, unix.SOCK_STREAM)
	if n := pending(t, a); n != 0 {
		t.Fatalf("empty channel: pending = %d", n)
	}
	if _, err := b.Write([]byte("abcde")); err != nil {
		t.Fatalf("write: %v", err)
	}
	for i := range 2 {
		if n := pending(t, a); n != 5 {
			t.Fatalf("peek %d: pending = %d, want 5", i, n)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(a, buf); err != nil || string(buf) != "abcde" {
		t.Fatalf("read %q, %v; the peek consumed bytes", buf, err)
	}
	if n := pending(t, a); n != 0 {
		t.Fatalf("after the peer closed: pending = %d", n)
	}
}

func TestFixtureDescribeSocket(t *testing.T) {
	tests := map[string]struct {
		file func(t *testing.T) (*os.File, socketKind)
	}{
		"success: stream pair end": {file: func(t *testing.T) (*os.File, socketKind) {
			a, _ := socketPair(t, unix.SOCK_STREAM)
			return a, socketKind{Family: "unix", Type: unix.SOCK_STREAM}
		}},
		"success: datagram pair end": {file: func(t *testing.T) (*os.File, socketKind) {
			a, _ := socketPair(t, unix.SOCK_DGRAM)
			return a, socketKind{Family: "unix", Type: unix.SOCK_DGRAM}
		}},
		"success: listener duplicate": {file: func(t *testing.T) (*os.File, socketKind) {
			f, port := tcpListenerFile("tcp4", "127.0.0.1:0")(t)
			t.Cleanup(func() { _ = f.Close() })
			return f, socketKind{Family: "127.0.0.1", Type: unix.SOCK_STREAM, Listening: true, Port: int(port)}
		}},
		"success: connected stream": {file: func(t *testing.T) (*os.File, socketKind) {
			f, port := connectedTCPFile(t)
			t.Cleanup(func() { _ = f.Close() })
			return f, socketKind{Family: "127.0.0.1", Type: unix.SOCK_STREAM, Port: int(port)}
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, want := tt.file(t)
			if diff := gocmp.Diff(want, describeSocket(t, f)); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
			checkCloseOnExec(t, f, name)
		})
	}
}

// TestFixtureAddressFreedByLastCopy checks the bind observation: a duplicate alone keeps the address, and the
// address is free once the last copy closes, also with an unaccepted connection still queued.
func TestFixtureAddressFreedByLastCopy(t *testing.T) {
	l, err := realListen(t.Context(), "tcp4", "127.0.0.1:0")
	if l != nil {
		t.Cleanup(func() { _ = l.Close() })
	}
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := tcpPort(t, l.Addr())
	tcp, ok := l.(*net.TCPListener)
	if !ok {
		t.Fatalf("listen returned %T", l)
	}
	f, err := tcp.File()
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	c, err := realDial(t.Context(), "tcp4", loopback(port))
	if c != nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	checkBindRefused(t, port, "with only the duplicate open")
	if err := f.Close(); err != nil {
		t.Fatalf("close duplicate: %v", err)
	}
	bindAndKeep(t, port, "after the last copy closed")
}

// TestFixtureFilledChannelBlocksWrites checks the saturation the acknowledgment tests rely on, through a
// connection over a local stream socket pair as the drain channel is: once fillUntilBlocked returns, not one more
// byte fits, and the peer then reads exactly the bytes it reported and nothing more.
func TestFixtureFilledChannelBlocksWrites(t *testing.T) {
	a, b := socketPair(t, unix.SOCK_STREAM)
	c, peer := fileConn(t, a), fileConn(t, b)
	filled := fillUntilBlocked(t, c)
	if filled <= 0 {
		t.Fatalf("filled %d bytes", filled)
	}
	_ = c.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	if n, err := c.Write([]byte("x")); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write to a filled channel = %d, %v; want nothing written and a deadline error", n, err)
	}
	_ = c.SetWriteDeadline(time.Time{})
	if got := readExactly(t, peer, filled); len(got) != filled {
		t.Fatalf("peer read %d bytes, want %d", len(got), filled)
	}
	if n := pending(t, peer.(syscall.Conn)); n != 0 {
		t.Errorf("%d bytes beyond the reported fill", n)
	}
}

// TestFixtureCountsDescriptors checks the descriptor count the balance check relies on: one more open file is
// one more descriptor, and closing it restores the count.
func TestFixtureCountsDescriptors(t *testing.T) {
	before, err := countOpenDescriptors()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	f, err := os.Open(os.DevNull)
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	open, err := countOpenDescriptors()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	after, err := countOpenDescriptors()
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if diff := gocmp.Diff([]int{1, 0}, []int{open - before, after - before}); diff != "" {
		t.Errorf("count differences with a file open and after closing it (-want +got):\n%s", diff)
	}
}

// TestFixtureClosedChannel checks the closure observations: a closed channel is reported closed on every receive,
// a value sent on an open channel is not closure, and await lets a sleeping goroutine of the bubble run its timer.
func TestFixtureClosedChannel(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	checkClosedEachTime(t, func() <-chan struct{} { return closed }, "closed channel")
	sent := make(chan struct{}, 1)
	sent <- struct{}{}
	if isClosed(sent) {
		t.Error("a value sent on an open channel was reported as closure")
	}
	if isClosed(sent) {
		t.Error("an open, empty channel was reported closed")
	}
	synctest.Test(t, func(t *testing.T) {
		late := make(chan int, 1)
		go func() {
			time.Sleep(time.Second)
			late <- 7
		}()
		if got := await(t, late, "a value sent after a sleep"); got != 7 {
			t.Errorf("await = %d, want 7", got)
		}
	})
}

// TestFixtureBubble checks, inside a synctest bubble, the real-socket reads used there and the fakes' behaviour.
func TestFixtureBubble(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := socketPair(t, unix.SOCK_STREAM)
		ca, cb := fileConn(t, a), fileConn(t, b)
		writeAll(t, cb, "abc")
		if n := pending(t, ca.(syscall.Conn)); n != 3 {
			t.Fatalf("pending = %d, want 3", n)
		}
		if got := readExactly(t, ca, 3); got != "abc" {
			t.Fatalf("read %q", got)
		}

		honouring := newGatedDials(true)
		ctx, cancel := context.WithCancel(t.Context())
		errs := make(chan error, 2)
		go func() {
			_, err := honouring.dial(ctx, "tcp4", "127.0.0.1:41001")
			errs <- err
		}()
		<-honouring.attempts
		cancel()
		synctest.Wait()
		if err, ok := ready(errs); !ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled gated dial = %v (returned %t)", err, ok)
		}
		// The dial that ignores cancellation is checked with a context that is cancelled while it is parked.
		ignoring := newGatedDials(false)
		ignoredCtx, cancelIgnored := context.WithCancel(t.Context())
		go func() {
			_, err := ignoring.dial(ignoredCtx, "tcp4", "127.0.0.1:41001")
			errs <- err
		}()
		parked := <-ignoring.attempts
		cancelIgnored()
		synctest.Wait()
		if parked.ctx.Err() == nil {
			t.Fatal("the parked attempt does not see its context's cancellation")
		}
		if _, ok := ready(errs); ok {
			t.Fatal("a gated dial that ignores cancellation returned when its context was cancelled")
		}
		ignoring.release()
		if err := await(t, errs, "the released gated dial"); err == nil {
			t.Fatal("released gated dial returned no error")
		}

		l := newFakeListener(41001)
		var hookRuns atomic.Int32
		l.onClose(func() {
			if isClosed(l.closed) {
				t.Error("the close hook ran after the listener reported itself closed")
			}
			hookRuns.Add(1)
		})
		accepted := make(chan error, 1)
		go func() {
			_, err := l.Accept()
			accepted <- err
		}()
		synctest.Wait()
		if l.accepts.Load() != 1 {
			t.Fatal("fake Accept was not recorded")
		}
		if _, ok := ready(accepted); ok {
			t.Fatal("fake Accept returned before Close")
		}
		_ = l.Close()
		_ = l.Close()
		if err := await(t, accepted, "fake Accept after Close"); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("fake Accept after Close = %v", err)
		}
		if diff := gocmp.Diff([]int32{1, 2}, []int32{hookRuns.Load(), l.closings.Load()}); diff != "" {
			t.Errorf("close hook runs and closings after two Closes (-want +got):\n%s", diff)
		}
		tc := newTrackedConn(t)
		_ = tc.Close()
		if !isClosed(tc.closed) {
			t.Fatal("tracked connection did not record Close")
		}
	})
}
