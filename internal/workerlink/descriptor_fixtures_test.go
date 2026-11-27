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
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// The tests in this file check the observations the package's tests rely on, against the operating system and
// the fakes alone, so that a wrong observation cannot pass for a wrong implementation. Of the package under test they
// call only the default drain channel pair, which is real code and no stub, and they probe its parent end only
// through net.Conn.

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

// TestFixtureDescribeSocket checks describeSocket against sockets the test builds: family, type, peer, port, and
// the listening state where the system reports it (on macOS it is not asked, see listeningOf).
func TestFixtureDescribeSocket(t *testing.T) {
	tests := map[string]struct {
		file func(t *testing.T) (*os.File, socketKind)
	}{
		"success: stream pair end": {file: func(t *testing.T) (*os.File, socketKind) {
			a, _ := socketPair(t, unix.SOCK_STREAM)
			return a, socketKind{Family: "unix", Type: unix.SOCK_STREAM, Listening: wantListening(false), Connected: true}
		}},
		"success: datagram pair end": {file: func(t *testing.T) (*os.File, socketKind) {
			a, _ := socketPair(t, unix.SOCK_DGRAM)
			return a, socketKind{Family: "unix", Type: unix.SOCK_DGRAM, Listening: wantListening(false), Connected: true}
		}},
		"success: unconnected stream socket": {file: func(t *testing.T) (*os.File, socketKind) {
			f, _ := unconnectedUnixStreamFile(t)
			return f, socketKind{Family: "unix", Type: unix.SOCK_STREAM, Listening: wantListening(false)}
		}},
		"success: listener duplicate": {file: func(t *testing.T) (*os.File, socketKind) {
			f, port := tcpListenerFile("tcp4", "127.0.0.1:0")(t)
			return f, socketKind{Family: "127.0.0.1", Type: unix.SOCK_STREAM, Listening: wantListening(true), Port: int(port)}
		}},
		"success: bound TCP socket that is not listening": {file: func(t *testing.T) (*os.File, socketKind) {
			f, port := boundTCPFile(t)
			return f, socketKind{Family: "127.0.0.1", Type: unix.SOCK_STREAM, Listening: wantListening(false), Port: int(port)}
		}},
		"success: connected stream": {file: func(t *testing.T) (*os.File, socketKind) {
			f, port := connectedTCPFile(t)
			return f, socketKind{Family: "127.0.0.1", Type: unix.SOCK_STREAM, Listening: wantListening(false), Connected: true, Port: int(port)}
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

// TestFixtureFilledChannelBlocksWrites checks the saturation the acknowledgment tests rely on, on the parent end of
// a drain channel pair made as drainPairs makes it: once saturate returns, a nonblocking one-byte send on that same
// end returns EAGAIN, and the peer then reads exactly the bytes saturate reported and nothing more.
func TestFixtureFilledChannelBlocksWrites(t *testing.T) {
	parent, file, err := newTestDrainPair()
	if parent != nil {
		t.Cleanup(func() { _ = parent.Close() })
	}
	if file != nil {
		t.Cleanup(func() { _ = file.Close() })
	}
	if err != nil {
		t.Fatalf("newTestDrainPair: %v", err)
	}
	sc, ok := parent.(syscall.Conn)
	if !ok {
		t.Fatalf("the test's parent end is a %T without a descriptor", parent)
	}
	peer := fileConn(t, file)
	filled := saturate(t, sc)
	if filled <= 0 {
		t.Fatalf("filled %d bytes", filled)
	}
	if refused, sent := refusesOneByte(t, sc); !refused {
		t.Fatalf("a one-byte send after saturation took %d byte(s); the channel was not full", sent)
	}
	if got := readExactly(t, peer, filled); len(got) != filled {
		t.Fatalf("peer read %d bytes, want %d", len(got), filled)
	}
	if n := pending(t, peer.(syscall.Conn)); n != 0 {
		t.Errorf("%d bytes beyond the reported fill", n)
	}
}

// TestFixtureCountsDescriptors checks the descriptor sets the balance checks rely on: opening a file adds exactly
// its descriptor number, and closing it restores the set.
func TestFixtureCountsDescriptors(t *testing.T) {
	before, err := openDescriptors()
	if err != nil {
		t.Fatalf("descriptors: %v", err)
	}
	f, err := os.Open(os.DevNull)
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	fd := int(f.Fd())
	open, err := openDescriptors()
	if err != nil {
		t.Fatalf("descriptors: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	after, err := openDescriptors()
	if err != nil {
		t.Fatalf("descriptors: %v", err)
	}
	want := []descriptorChange{{Opened: []int{fd}, Closed: []int{}}, {Opened: []int{}, Closed: []int{}}}
	if diff := gocmp.Diff(want, []descriptorChange{changeBetween(before, open), changeBetween(before, after)}); diff != "" {
		t.Errorf("descriptor changes with a file open and after closing it (-want +got):\n%s", diff)
	}
}

// TestFixtureDefaultDrainPair checks the pair a registry makes when no test replaced its factory: the child's end is
// a close-on-exec, connected local stream socket, the two ends carry bytes both ways, and closing every handle of the
// child's end gives the parent end of file, so the pair keeps no other copy of it. The parent end is used only as a
// net.Conn: the default may return any connection type, and that a child does not inherit it is checked in a child
// process (TestFixtureDefaultDrainPairNotInherited).
func TestFixtureDefaultDrainPair(t *testing.T) {
	parent, file, err := workerlink.NewDrainPair()
	if parent != nil {
		t.Cleanup(func() { _ = parent.Close() })
	}
	if file != nil {
		t.Cleanup(func() { _ = file.Close() })
	}
	if err != nil {
		t.Fatalf("NewDrainPair: %v", err)
	}
	want := socketKind{Family: "unix", Type: unix.SOCK_STREAM, Listening: wantListening(false), Connected: true}
	if diff := gocmp.Diff(want, describeSocket(t, file)); diff != "" {
		t.Errorf("child end (-want +got):\n%s", diff)
	}
	checkCloseOnExec(t, file, "child end")
	child := fileConn(t, file)
	writeAll(t, parent, "to-child\n")
	if got := readExactly(t, child, 9); got != "to-child\n" {
		t.Errorf("child read %q", got)
	}
	writeAll(t, child, "to-parent\n")
	if got := readExactly(t, parent, 10); got != "to-parent\n" {
		t.Errorf("parent read %q", got)
	}
	if err := child.Close(); err != nil {
		t.Fatalf("closing the child's connection: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing the child's file: %v", err)
	}
	_ = parent.SetReadDeadline(time.Now().Add(watchdog))
	if n, err := parent.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("parent read with every handle of the child's end closed = %d, %v; want end of file", n, err)
	}
}

// TestFixtureDrainPairs checks the test's pair factory: the call it is told to fail returns errPair and nothing
// else, the others return real pairs whose parent end the test keeps, gated when asked.
func TestFixtureDrainPairs(t *testing.T) {
	pairs := newDrainPairs(t, new(workerlink.Registry), true, 2)
	type outcome struct {
		Parent, Child, Gated bool
		Err                  error
	}
	var got []outcome
	for range 3 {
		parent, child, err := pairs.pair()
		got = append(got, outcome{Parent: parent != nil, Child: child != nil, Err: err})
		if parent != nil {
			_, got[len(got)-1].Gated = parent.(*gatedConn)
		}
	}
	want := []outcome{{Parent: true, Child: true, Gated: true}, {Err: errPair}, {Parent: true, Child: true, Gated: true}}
	if diff := gocmp.Diff(want, got, cmpopts.EquateErrors()); diff != "" {
		t.Errorf("factory calls (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff([]int{3, 2}, []int{pairs.count(), len(pairs.parents)}); diff != "" {
		t.Errorf("calls and kept parent ends (-want +got):\n%s", diff)
	}
	if pairs.gatedAt(t, 1).Conn != pairs.parent(t, 1) {
		t.Error("the second gated end does not wrap the second real parent end")
	}
}

// TestFixtureGatedConn checks the gated parent end inside a bubble, with returns parked: a Write announces its entry
// and waits; it ends when the gate opens (the bytes reach the real peer, and nothing parks), at a write deadline set
// before it or while it waits, at once for a deadline that is not in the future, and at once when the connection is
// closed. An interrupted Write announces, at the bubble's time it was interrupted, that it chose its error, and then
// does not return until the returns are released. Its entry and return are recorded at the bubble's times they
// happened; a write that did not go through delivers nothing; after the gate opened, later writes go through at once.
func TestFixtureGatedConn(t *testing.T) {
	tests := map[string]struct {
		// before runs before the Write starts; during runs once it has entered.
		before, during func(g *gatedConn)
		wait           time.Duration
		err            error
	}{
		"success: the gate opens": {
			during: func(g *gatedConn) { g.openGate() },
		},
		"error: deadline set before the write": {
			before: func(g *gatedConn) { _ = g.SetWriteDeadline(time.Now().Add(3 * time.Second)) },
			wait:   3 * time.Second,
			err:    os.ErrDeadlineExceeded,
		},
		"error: deadline set while the write waits": {
			during: func(g *gatedConn) { _ = g.SetDeadline(time.Now().Add(2 * time.Second)) },
			wait:   2 * time.Second,
			err:    os.ErrDeadlineExceeded,
		},
		"error: deadline already passed": {
			during: func(g *gatedConn) { _ = g.SetWriteDeadline(time.Now()) },
			err:    os.ErrDeadlineExceeded,
		},
		"error: closed while the write waits": {
			during: func(g *gatedConn) { _ = g.Close() },
			err:    net.ErrClosed,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, file, err := workerlink.NewDrainPair()
				if parent != nil {
					t.Cleanup(func() { _ = parent.Close() })
				}
				if file != nil {
					t.Cleanup(func() { _ = file.Close() })
				}
				if err != nil {
					t.Fatalf("NewDrainPair: %v", err)
				}
				peer := fileConn(t, file)
				g := newGatedConn(parent)
				t.Cleanup(func() { g.releaseReturns(); _ = g.Close() })
				g.parkOnReturn()
				if tt.before != nil {
					tt.before(g)
				}
				type written struct {
					n   int
					err error
				}
				out := make(chan written, 1)
				start := time.Now()
				go func() {
					n, err := g.Write([]byte("line\n"))
					out <- written{n, err}
				}()
				await(t, g.entered, "entry into Write")
				synctest.Wait()
				if w, ok := ready(out); ok {
					t.Fatalf("Write returned %d, %v before anything ended it", w.n, w.err)
				}
				if entry, exit := g.heldSpan(); !entry.Equal(start) || !exit.IsZero() || isClosed(g.exited) || isClosed(g.returning) {
					t.Fatalf("while the Write waits: entry %v (want the start %v), exit %v (want none), interrupted %t", entry, start, exit, isClosed(g.returning))
				}
				if tt.during != nil {
					tt.during(g)
				}
				if tt.err != nil {
					await(t, g.returning, "the interrupted Write's choice of its error")
					synctest.Wait()
					if w, ok := ready(out); ok || isClosed(g.exited) {
						t.Fatalf("the interrupted Write returned %d, %v before its return was released", w.n, w.err)
					}
					if got := g.interruption(); got.Sub(start) != tt.wait {
						t.Errorf("the Write was interrupted %v after it began, want %v", got.Sub(start), tt.wait)
					}
					g.releaseReturns()
				}
				w := await(t, out, "the end of the Write")
				if waited := time.Since(start); waited != tt.wait {
					t.Errorf("the Write ended after %v of the bubble's time, want %v", waited, tt.wait)
				}
				if entry, exit := g.heldSpan(); exit.Sub(entry) != tt.wait || !isClosed(g.exited) {
					t.Errorf("after the Write returned: span %v (want %v), exit announced %t", exit.Sub(entry), tt.wait, isClosed(g.exited))
				}
				if tt.err == nil && isClosed(g.returning) {
					t.Error("a Write that went through was announced as interrupted")
				}
				offered, writes := g.attempts()
				if diff := gocmp.Diff([]any{"line\n", 1}, []any{offered, writes}); diff != "" {
					t.Errorf("recorded attempts (-want +got):\n%s", diff)
				}
				if tt.err != nil {
					if w.n != 0 || !errors.Is(w.err, tt.err) {
						t.Errorf("Write = %d, %v; want nothing written and %v", w.n, w.err, tt.err)
					}
					if n := pending(t, peer.(syscall.Conn)); n != 0 {
						t.Errorf("%d bytes reached the peer from a write that did not go through", n)
					}
					return
				}
				if w.n != 5 || w.err != nil {
					t.Fatalf("Write = %d, %v; want the whole line", w.n, w.err)
				}
				if got := readExactly(t, peer, 5); got != "line\n" {
					t.Errorf("peer read %q", got)
				}
				later := make(chan written, 1)
				go func() {
					n, err := g.Write([]byte("more"))
					later <- written{n, err}
				}()
				if w := await(t, later, "a Write after the gate opened"); w.n != 4 || w.err != nil {
					t.Errorf("Write after the gate opened = %d, %v", w.n, w.err)
				}
				if waited := time.Since(start); waited != 0 {
					t.Errorf("writes through an open gate took %v of the bubble's time", waited)
				}
			})
		})
	}
}

// TestFixtureGatedConnHoldsTheLastByte checks the gated parent end with a hold offset, inside a bubble: however the
// bytes are split into Writes, every Write that ends before the held offset goes through at once and is not announced,
// and the Write that carries the byte at the offset is announced and waits until the gate opens. While it waits, the
// peer has exactly the bytes of the Writes that went through, none of the held one; after the gate opens the peer
// reads every byte in order.
func TestFixtureGatedConnHoldsTheLastByte(t *testing.T) {
	const line = "line\n"
	tests := map[string]struct {
		writes []string
	}{
		"success: one Write carries the whole line":          {writes: []string{line}},
		"success: the last byte comes in a Write of its own": {writes: []string{"line", "\n"}},
		"success: the held Write also carries earlier bytes": {writes: []string{"l", "in", "e\n"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, file, err := newTestDrainPair()
				if parent != nil {
					t.Cleanup(func() { _ = parent.Close() })
				}
				if file != nil {
					t.Cleanup(func() { _ = file.Close() })
				}
				if err != nil {
					t.Fatalf("newTestDrainPair: %v", err)
				}
				peer := fileConn(t, file)
				g := newGatedConn(parent)
				t.Cleanup(func() { _ = g.Close() })
				g.holdFrom(len(line) - 1)
				last := len(tt.writes) - 1
				for _, w := range tt.writes[:last] {
					if n, err := g.Write([]byte(w)); n != len(w) || err != nil {
						t.Fatalf("Write(%q) before the held byte = %d, %v; want it through at once", w, n, err)
					}
				}
				if isClosed(g.entered) {
					t.Fatal("a Write before the held byte was announced as the held one")
				}
				prefix := strings.Join(tt.writes[:last], "")
				out := make(chan error, 1)
				go func() {
					_, err := g.Write([]byte(tt.writes[last]))
					out <- err
				}()
				await(t, g.entered, "entry into the Write that carries the held byte")
				synctest.Wait()
				if err, ok := ready(out); ok {
					t.Fatalf("the Write that carries the held byte returned %v before the gate opened", err)
				}
				if isClosed(g.exited) {
					t.Fatal("the held Write was announced as returned before the gate opened")
				}
				if got := peeked(t, peer.(syscall.Conn)); got != prefix {
					t.Fatalf("while the held Write waits the peer has %q, want exactly the bytes of the Writes that went through, %q", got, prefix)
				}
				g.openGate()
				if err := await(t, out, "the held Write"); err != nil {
					t.Fatalf("the held Write after the gate opened: %v", err)
				}
				if got := readExactly(t, peer, len(line)); got != line {
					t.Errorf("peer read %q, want %q", got, line)
				}
				if offered, writes := g.attempts(); offered != line || writes != len(tt.writes) {
					t.Errorf("recorded %q in %d Writes, want %q in %d", offered, writes, line, len(tt.writes))
				}
			})
		})
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

// TestFixtureCloseRecorderCountsEveryWrite checks the recording writer the encoder tests use: every way of giving it
// bytes, io.WriteString of an empty string included, is a counted Write.
func TestFixtureCloseRecorderCountsEveryWrite(t *testing.T) {
	var w closeRecorder
	if _, err := io.WriteString(&w, ""); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if _, err := io.WriteString(&w, "ab"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if _, err := w.Write([]byte("c")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if diff := gocmp.Diff([]any{3, "abc"}, []any{w.writes, w.String()}); diff != "" {
		t.Errorf("writes and bytes (-want +got):\n%s", diff)
	}
}
