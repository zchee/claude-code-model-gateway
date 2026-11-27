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
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// watchdog bounds a wait on a positive event; it aborts a hang and proves nothing about what did not happen.
const watchdog = 10 * time.Second

func loopback(port uint16) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
}

// listenTCP binds a listener the test keeps until cleanup.
func listenTCP(t *testing.T, network, address string) *net.TCPListener {
	t.Helper()
	l, err := new(net.ListenConfig).Listen(t.Context(), network, address)
	if err != nil {
		t.Fatalf("listen %s %s: %v", network, address, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.(*net.TCPListener)
}

func tcpPort(t *testing.T, addr net.Addr) uint16 {
	t.Helper()
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("address %v is not TCP", addr)
	}
	return uint16(tcp.Port)
}

// realListen is the production listen function.
func realListen(ctx context.Context, network, address string) (net.Listener, error) {
	return new(net.ListenConfig).Listen(ctx, network, address)
}

// realDial is the production dial function.
func realDial(ctx context.Context, network, address string) (net.Conn, error) {
	return new(net.Dialer).DialContext(ctx, network, address)
}

// checkBindRefused asserts that the address is still held: a second bind of it fails with EADDRINUSE.
func checkBindRefused(t *testing.T, port uint16, when string) {
	t.Helper()
	l, err := realListen(t.Context(), "tcp4", loopback(port))
	if err == nil {
		_ = l.Close()
		t.Fatalf("%s: a second bind of %s succeeded; the reserved address was released", when, loopback(port))
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("%s: second bind of %s: %v, want EADDRINUSE", when, loopback(port), err)
	}
}

// bindAndKeep asserts that the address is free by binding it; the test keeps the listener until cleanup.
func bindAndKeep(t *testing.T, port uint16, when string) *net.TCPListener {
	t.Helper()
	l, err := realListen(t.Context(), "tcp4", loopback(port))
	if err != nil {
		t.Fatalf("%s: bind of %s: %v; the address is still held", when, loopback(port), err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.(*net.TCPListener)
}

// checkConsumed asserts that a function closed the file it was given.
func checkConsumed(t *testing.T, f *os.File) {
	t.Helper()
	if f == nil {
		return
	}
	if err := f.Close(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the input file was left open (Close = %v); it must be consumed on success and on failure", err)
	}
}

// control runs fn on the descriptor behind c without changing its blocking mode.
func control(t *testing.T, c syscall.Conn, fn func(fd int)) {
	t.Helper()
	raw, err := c.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	if err := raw.Control(func(fd uintptr) { fn(int(fd)) }); err != nil {
		t.Fatalf("Control: %v", err)
	}
}

func checkCloseOnExec(t *testing.T, c syscall.Conn, what string) {
	t.Helper()
	control(t, c, func(fd int) {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatalf("%s: F_GETFD: %v", what, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Errorf("%s: descriptor %d is not close-on-exec", what, fd)
		}
	})
}

// pending returns the number of bytes queued for reading on c, up to 4096, without consuming them or blocking.
func pending(t *testing.T, c syscall.Conn) int {
	t.Helper()
	n := -1
	control(t, c, func(fd int) {
		buf := make([]byte, 4096)
		got, _, err := unix.Recvfrom(fd, buf, unix.MSG_PEEK|unix.MSG_DONTWAIT)
		switch {
		case errors.Is(err, unix.EAGAIN):
			n = 0
		case err != nil:
			t.Fatalf("peek: %v", err)
		default:
			n = got
		}
	})
	return n
}

// socketKind describes a socket descriptor: its family, type and whether it listens.
type socketKind struct {
	Family    string
	Type      int
	Listening bool
	Port      int
}

func describeSocket(t *testing.T, c syscall.Conn) socketKind {
	t.Helper()
	var kind socketKind
	control(t, c, func(fd int) {
		typ, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
		if err != nil {
			t.Fatalf("SO_TYPE: %v", err)
		}
		kind.Type = typ
		listening, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
		if err != nil {
			t.Fatalf("SO_ACCEPTCONN: %v", err)
		}
		kind.Listening = listening != 0
		sa, err := unix.Getsockname(fd)
		if err != nil {
			t.Fatalf("getsockname: %v", err)
		}
		switch sa := sa.(type) {
		case *unix.SockaddrInet4:
			kind.Family, kind.Port = net.IP(sa.Addr[:]).String(), sa.Port
		case *unix.SockaddrUnix:
			kind.Family = "unix"
		default:
			kind.Family = "other"
		}
	})
	return kind
}

// socketPair returns both ends of a fresh local socket pair of the given type as files the test owns.
func socketPair(t *testing.T, typ int) (*os.File, *os.File) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, typ, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	a, b := os.NewFile(uintptr(fds[0]), "pair-a"), os.NewFile(uintptr(fds[1]), "pair-b")
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

// fileConn returns an independent connection over a duplicate of f; f stays the caller's.
func fileConn(t *testing.T, f *os.File) net.Conn {
	t.Helper()
	c, err := net.FileConn(f)
	if err != nil {
		t.Fatalf("FileConn: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readExactly reads n bytes under the watchdog deadline.
func readExactly(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(watchdog))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v (got %q)", n, err, buf)
	}
	_ = c.SetReadDeadline(time.Time{})
	return string(buf)
}

// acceptOne accepts one connection on l under the watchdog deadline.
func acceptOne(t *testing.T, l *net.TCPListener) net.Conn {
	t.Helper()
	_ = l.SetDeadline(time.Now().Add(watchdog))
	c, err := l.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	_ = l.SetDeadline(time.Time{})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// childListener imports the reservation's listener duplicate as the worker would, and closes the duplicate.
func childListener(t *testing.T, res *workerlink.Reservation) *net.TCPListener {
	t.Helper()
	f, err := res.File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	l, err := net.FileListener(f)
	if closeErr := f.Close(); closeErr != nil {
		t.Fatalf("closing the duplicate: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("FileListener: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l.(*net.TCPListener)
}

// isClosed reports whether ch is closed, without waiting.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// fakeListener is a listener the registry may hold but must never accept on. It records closing and accepting.
type fakeListener struct {
	addr     *net.TCPAddr
	once     sync.Once
	closed   chan struct{}
	accepts  atomic.Int32
	closings atomic.Int32
}

func newFakeListener(port uint16) *fakeListener {
	return &fakeListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}, closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeListener) Close() error {
	l.closings.Add(1)
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return l.addr }

// listenCall is one call of an injected listen function.
type listenCall struct {
	Network, Address string
}

// fakeListens hands out fake listeners on the requested ports and records every call.
type fakeListens struct {
	mu        sync.Mutex
	calls     []listenCall
	listeners map[string]*fakeListener
}

func newFakeListens() *fakeListens { return &fakeListens{listeners: map[string]*fakeListener{}} }

func (f *fakeListens) listen(_ context.Context, network, address string) (net.Listener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, listenCall{network, address})
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, errors.New("fake listens need a fixed port")
	}
	l := newFakeListener(uint16(port))
	f.listeners[address] = l
	return l, nil
}

func (f *fakeListens) at(t *testing.T, port uint16) *fakeListener {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.listeners[loopback(port)]
	if l == nil {
		t.Fatalf("no listener was bound at %s", loopback(port))
	}
	return l
}

// checkNeverAccepted asserts that no fake listener was ever accepted on.
func (f *fakeListens) checkNeverAccepted(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for address, l := range f.listeners {
		if n := l.accepts.Load(); n != 0 {
			t.Errorf("the registry accepted %d times on %s; it must never accept", n, address)
		}
	}
}

// trackedConn is one end of an in-memory connection that records being closed.
type trackedConn struct {
	net.Conn
	peer   net.Conn
	once   sync.Once
	closed chan struct{}
}

func newTrackedConn(t *testing.T) *trackedConn {
	a, b := net.Pipe()
	c := &trackedConn{Conn: a, peer: b, closed: make(chan struct{})}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	_ = c.peer.Close()
	return c.Conn.Close()
}

// dialAttempt is one parked call of a gated dial function. The test answers it through reply.
type dialAttempt struct {
	ctx              context.Context
	network, address string
	reply            chan dialReply
}

type dialReply struct {
	conn net.Conn
	err  error
}

// gatedDials parks every dial until the test answers it. With honourCancel a parked dial also returns when its
// context ends; without it the dial ignores cancellation, as a slow connect that completes anyway.
type gatedDials struct {
	honourCancel bool
	attempts     chan *dialAttempt
	count        atomic.Int32
	mu           sync.Mutex
	all          []*dialAttempt
}

func newGatedDials(honourCancel bool) *gatedDials {
	return &gatedDials{honourCancel: honourCancel, attempts: make(chan *dialAttempt, 16)}
}

func (d *gatedDials) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.count.Add(1)
	a := &dialAttempt{ctx: ctx, network: network, address: address, reply: make(chan dialReply, 1)}
	d.mu.Lock()
	d.all = append(d.all, a)
	d.mu.Unlock()
	d.attempts <- a
	if d.honourCancel {
		select {
		case r := <-a.reply:
			return r.conn, r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r := <-a.reply
	return r.conn, r.err
}

// immediateDials answers every dial at once with a fresh tracked connection, or with err when it is set.
type immediateDials struct {
	t     *testing.T
	mu    sync.Mutex
	calls []listenCall
	conns []*trackedConn
	err   error
}

func (d *immediateDials) dial(_ context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, listenCall{network, address})
	if d.err != nil {
		return nil, d.err
	}
	c := newTrackedConn(d.t)
	d.conns = append(d.conns, c)
	return c, nil
}

func (d *immediateDials) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.calls)
}

// endAtCleanup ends a reservation when the test finishes, whatever the test already did.
func endAtCleanup(t *testing.T, res *workerlink.Reservation) {
	t.Cleanup(func() { _ = res.End() })
}

// reserve reserves a generation and ends it at cleanup.
func reserve(t *testing.T, reg *workerlink.Registry, generation workerlink.Generation, port uint16) *workerlink.Reservation {
	t.Helper()
	res, err := reg.Reserve(t.Context(), generation, port)
	if err != nil {
		t.Fatalf("Reserve(%q, %d): %v", generation, port, err)
	}
	if res == nil {
		t.Fatalf("Reserve(%q, %d) returned no reservation", generation, port)
	}
	endAtCleanup(t, res)
	return res
}

func publish(t *testing.T, res *workerlink.Reservation, pid int) {
	t.Helper()
	if err := res.Publish(pid); err != nil {
		t.Fatalf("Publish(%d): %v", pid, err)
	}
}
