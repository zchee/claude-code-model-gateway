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
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := tcpPort(t, l.Addr())
	f, err := l.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	c, err := realDial(t.Context(), "tcp4", loopback(port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	checkBindRefused(t, port, "with only the duplicate open")
	if err := f.Close(); err != nil {
		t.Fatalf("close duplicate: %v", err)
	}
	bindAndKeep(t, port, "after the last copy closed")
}

// TestFixtureFilledChannelBlocksWrites checks that a filled channel accepts no further byte, so an
// acknowledgment written to it can only end through its deadline.
func TestFixtureFilledChannelBlocksWrites(t *testing.T) {
	a, _ := socketPair(t, unix.SOCK_STREAM)
	c := fileConn(t, a)
	fillUntilBlocked(t, c)
	_ = c.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := c.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write to a filled channel: %v, want a deadline error", err)
	}
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
		ignoring := newGatedDials(false)
		go func() {
			_, err := ignoring.dial(t.Context(), "tcp4", "127.0.0.1:41001")
			errs <- err
		}()
		<-ignoring.attempts
		synctest.Wait()
		if _, ok := ready(errs); ok {
			t.Fatal("a gated dial that ignores cancellation returned unanswered")
		}
		ignoring.release()
		synctest.Wait()
		if err, ok := ready(errs); !ok || err == nil {
			t.Fatalf("released gated dial = %v (returned %t)", err, ok)
		}

		l := newFakeListener(41001)
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
		synctest.Wait()
		if err, ok := ready(accepted); !ok || !errors.Is(err, net.ErrClosed) {
			t.Fatalf("fake Accept after Close = %v (returned %t)", err, ok)
		}
		tc := newTrackedConn(t)
		_ = tc.Close()
		if !isClosed(tc.closed) {
			t.Fatal("tracked connection did not record Close")
		}
	})
}
