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
	"os"
	"slices"
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
	n, err := peekPending(c)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	return n
}

// peeked returns the bytes queued for reading on c, up to 4096, without consuming them or blocking.
func peeked(t *testing.T, c syscall.Conn) string {
	t.Helper()
	var queued string
	control(t, c, func(fd int) {
		buf := make([]byte, 4096)
		for {
			n, _, err := unix.Recvfrom(fd, buf, unix.MSG_PEEK|unix.MSG_DONTWAIT)
			switch {
			case err == nil:
				queued = string(buf[:n])
				return
			case errors.Is(err, unix.EINTR):
			case errors.Is(err, unix.EAGAIN):
				return
			default:
				t.Fatalf("peek: %v", err)
			}
		}
	})
	return queued
}

// peekPending is pending without a test: it may run on a goroutine of the code under test.
func peekPending(c syscall.Conn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	n := -1
	var peekErr error
	if err := raw.Control(func(fd uintptr) {
		buf := make([]byte, 4096)
		got, _, err := unix.Recvfrom(int(fd), buf, unix.MSG_PEEK|unix.MSG_DONTWAIT)
		switch {
		case errors.Is(err, unix.EAGAIN):
			n = 0
		case err != nil:
			peekErr = err
		default:
			n = got
		}
	}); err != nil {
		return -1, err
	}
	return n, peekErr
}

// openDescriptors returns the sorted numbers of the descriptors open in this process. It reads the names of
// /dev/fd, without a stat of each entry: on macOS os.ReadDir stats the entries and drops the ones that fail, such as a
// kqueue descriptor, where Readdirnames returns every name. The listing also names the descriptors the enumeration
// itself held: the directory's own, and on macOS the duplicate Go makes of it for fdopendir (Go
// src/os/dir_darwin.go:36, src/internal/poll/fd_opendir_darwin.go:15-18), which /dev/fd lists like any other (xnu
// bsd/miscfs/devfs/devfs_fdesc_support.c:694-713). Both are closed with the directory, so after that each listed
// number is asked F_GETFD: EBADF means it is closed now and it is left out (fcntl looks the number up at xnu
// bsd/kern/kern_descrip.c:2871, and fp_lookup returns EBADF for a closed one at :1524-1530); any other error fails;
// a descriptor of any kind that is still open, a kqueue included, answers (the F_GETFD case reads only the
// descriptor's flags, kern_descrip.c:2922-2925).
func openDescriptors() ([]int, error) {
	dir, err := os.Open("/dev/fd")
	if err != nil {
		return nil, err
	}
	names, err := dir.Readdirnames(-1)
	if closeErr := dir.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	fds := make([]int, 0, len(names))
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err != nil {
			return nil, fmt.Errorf("descriptor name %q: %w", name, err)
		}
		switch _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); {
		case errors.Is(err, unix.EBADF):
		case err != nil:
			return nil, fmt.Errorf("F_GETFD of descriptor %d: %w", fd, err)
		default:
			fds = append(fds, fd)
		}
	}
	slices.Sort(fds)
	return fds, nil
}

// descriptorChange is how the set of open descriptors changed between two readings.
type descriptorChange struct {
	Opened, Closed []int
}

func changeBetween(before, after []int) descriptorChange {
	change := descriptorChange{Opened: []int{}, Closed: []int{}}
	for _, fd := range after {
		if !slices.Contains(before, fd) {
			change.Opened = append(change.Opened, fd)
		}
	}
	for _, fd := range before {
		if !slices.Contains(after, fd) {
			change.Closed = append(change.Closed, fd)
		}
	}
	return change
}

// listenState is what the system says about a socket's listening state. It is empty where the system does not
// answer the question: macOS does not implement the SO_ACCEPTCONN socket option (see listeningOf).
type listenState string

const (
	isListening  listenState = "listening"
	notListening listenState = "not listening"
)

// wantListening is the state describeSocket reports for a socket that listens or does not, on this system.
func wantListening(listening bool) listenState {
	switch {
	case !listeningObservable:
		return ""
	case listening:
		return isListening
	default:
		return notListening
	}
}

// socketKind describes a socket descriptor: its family (the IPv4 address for an IPv4 socket), its type, whether it
// listens, whether it has a peer, and its port.
type socketKind struct {
	Family    string
	Type      int
	Listening listenState
	Connected bool
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
		if kind.Listening, err = listeningOf(fd); err != nil {
			t.Fatalf("listening state: %v", err)
		}
		switch _, err := unix.Getpeername(fd); {
		case err == nil:
			kind.Connected = true
		case !errors.Is(err, unix.ENOTCONN):
			t.Fatalf("getpeername: %v", err)
		}
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

// saturate fills the local stream socket behind c until it refuses a single byte, and returns the number of bytes
// it sent. Every send is a nonblocking send on c's own descriptor: the descriptor must already be nonblocking (Go
// makes every socket it polls nonblocking), and the call also carries MSG_DONTWAIT. The loop ends only when a
// one-byte send returns EAGAIN, so on return not one more byte fits until the peer reads. On Linux a nonblocking
// send that would block returns EAGAIN (send(2)); on macOS sosendcheck returns EWOULDBLOCK, which is EAGAIN there,
// for a socket in nonblocking mode (xnu bsd/kern/uipc_socket.c:1964-1966; fcntl O_NONBLOCK sets that mode through
// FIONBIO, bsd/kern/sys_socket.c:206).
func saturate(t *testing.T, c syscall.Conn) int {
	t.Helper()
	total := 0
	control(t, c, func(fd int) {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			t.Fatalf("F_GETFL: %v", err)
		}
		if flags&unix.O_NONBLOCK == 0 {
			t.Fatalf("descriptor %d is blocking; saturate needs a nonblocking socket", fd)
		}
		for _, size := range []int{64 << 10, 1} {
			chunk := make([]byte, size)
			for {
				n, err := unix.SendmsgN(fd, chunk, nil, nil, unix.MSG_DONTWAIT)
				if errors.Is(err, unix.EAGAIN) {
					break
				}
				if errors.Is(err, unix.EINTR) {
					continue
				}
				if err != nil {
					t.Fatalf("nonblocking send of %d bytes: %v", size, err)
				}
				total += n
				if total > 1<<30 {
					t.Fatal("the channel never filled")
				}
			}
		}
	})
	return total
}

// refusesOneByte reports whether a nonblocking one-byte send on c returns EAGAIN, without blocking. A send that
// succeeds is counted in sent.
func refusesOneByte(t *testing.T, c syscall.Conn) (refused bool, sent int) {
	t.Helper()
	control(t, c, func(fd int) {
		n, err := unix.SendmsgN(fd, []byte{'x'}, nil, nil, unix.MSG_DONTWAIT)
		switch {
		case errors.Is(err, unix.EAGAIN):
			refused = true
		case err != nil:
			t.Fatalf("nonblocking one-byte send: %v", err)
		default:
			sent = n
		}
	})
	return refused, sent
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
	if f != nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	l, err := net.FileListener(f)
	if l != nil {
		t.Cleanup(func() { _ = l.Close() })
	}
	if closeErr := f.Close(); closeErr != nil {
		t.Fatalf("closing the duplicate: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("FileListener: %v", err)
	}
	tcp, ok := l.(*net.TCPListener)
	if !ok {
		t.Fatalf("the duplicate is a %T listener, want TCP", l)
	}
	return tcp
}

// isClosed reports whether ch is closed, without waiting. A value sent on ch is not closure and reports false.
func isClosed(ch <-chan struct{}) bool {
	select {
	case _, ok := <-ch:
		return !ok
	default:
		return false
	}
}

// checkClosedEachTime asserts that the channel get returns is closed, on a second receive from the same channel
// and on a channel got again: closure, not a value sent once, wakes every waiter.
func checkClosedEachTime(t *testing.T, get func() <-chan struct{}, what string) {
	t.Helper()
	ch := get()
	for i := range 2 {
		if !isClosed(ch) {
			t.Errorf("%s: receive %d does not report a closed channel", what, i+1)
		}
	}
	if !isClosed(get()) {
		t.Errorf("%s: the channel got again is not closed", what)
	}
}

// await waits for the value on ch. In a synctest bubble the watchdog is virtual time, so code that waits on its
// own timers gets to run them before the test gives up; outside one it is real time. It only aborts a hang.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(watchdog):
		t.Fatalf("%s did not happen within %v", what, watchdog)
		var zero T
		return zero
	}
}

// fakeListener is a listener the registry may hold but must never accept on. It records closing and accepting,
// and runs the hook set by onClose once, at the first Close, before it reports itself closed.
type fakeListener struct {
	addr     net.Addr
	once     sync.Once
	closed   chan struct{}
	accepts  atomic.Int32
	closings atomic.Int32
	mu       sync.Mutex
	hook     func()
}

func newFakeListener(port uint16) *fakeListener {
	return newFakeListenerAt(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
}

func newFakeListenerAt(addr net.Addr) *fakeListener {
	return &fakeListener{addr: addr, closed: make(chan struct{})}
}

// onClose sets the hook the first Close runs.
func (l *fakeListener) onClose(hook func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hook = hook
}

func (l *fakeListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	<-l.closed
	return nil, net.ErrClosed
}

func (l *fakeListener) Close() error {
	l.closings.Add(1)
	l.once.Do(func() {
		l.mu.Lock()
		hook := l.hook
		l.mu.Unlock()
		if hook != nil {
			hook()
		}
		close(l.closed)
	})
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
	if res != nil {
		endAtCleanup(t, res)
	}
	if err != nil {
		t.Fatalf("Reserve(%q, %d): %v", generation, port, err)
	}
	if res == nil {
		t.Fatalf("Reserve(%q, %d) returned no reservation", generation, port)
	}
	return res
}

func publish(t *testing.T, res *workerlink.Reservation, pid int) {
	t.Helper()
	if err := res.Publish(pid); err != nil {
		t.Fatalf("Publish(%d): %v", pid, err)
	}
}

// gatedConn is the parent's end of a drain channel as a test supplies it through the registry's pair factory. It
// passes everything to a real end, except that a Write first records the bytes it was asked to write, and a Write
// whose bytes reach the held offset (counted over every byte offered so far, from zero; see holdFrom) then waits, as a
// write to a peer that has stopped reading does, until the test opens the gate (the bytes then go to the real end),
// its write deadline passes, or the connection is closed. A Write that ends before the held offset goes through at
// once. A deadline set before or during the Write is honoured, and one that is not in the future ends the Write at
// once with nothing written, as a net.Conn does. Write deadlines are enforced by the fake alone, on the bubble's
// clock, and never reach the backing connection. The connection records when the first held Write began and when it
// returned.
//
// With parkOnReturn, a Write that the deadline or Close interrupted has chosen its error and then waits, outside the
// connection's lock, until the test calls releaseReturns; only then does it return. The first such Write announces
// that point on returning. A caller that has not joined its Write therefore cannot have returned while the Write is
// parked there.
type gatedConn struct {
	net.Conn
	// entered is closed when the first held Write begins; exited when that Write returns.
	entered, exited chan struct{}
	// returning is closed when the first interrupted Write has chosen its error and parks; release is closed by
	// releaseReturns.
	returning, release chan struct{}

	mu sync.Mutex
	// changed is closed and replaced whenever the gate, the write deadline or closure changes.
	changed  chan struct{}
	open     bool
	closed   bool
	deadline time.Time
	// hold is the offset of the first byte whose Write waits; zero holds every Write with a byte in it.
	hold     int
	held     bool
	park     bool
	parked   bool
	released bool
	offered  []byte
	writes   int
	// enteredAt and exitedAt are the bubble's times of the first held Write's entry and return; interruptedAt is the
	// time the first interrupted Write chose its error.
	enteredAt, exitedAt, interruptedAt time.Time
}

func newGatedConn(real net.Conn) *gatedConn {
	return &gatedConn{
		Conn:      real,
		entered:   make(chan struct{}),
		exited:    make(chan struct{}),
		returning: make(chan struct{}),
		release:   make(chan struct{}),
		changed:   make(chan struct{}),
	}
}

func (c *gatedConn) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// holdFrom makes the Writes before offset go through at once and holds the Write that reaches it. Call it before
// the first Write.
func (c *gatedConn) holdFrom(offset int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hold = offset
}

// parkOnReturn makes every interrupted Write wait for releaseReturns before it returns. Call it before the first
// Write.
func (c *gatedConn) parkOnReturn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.park = true
}

// releaseReturns lets every parked and later interrupted Write return. It may be called more than once.
func (c *gatedConn) releaseReturns() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.released {
		c.released = true
		close(c.release)
	}
}

func (c *gatedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	reaches := len(c.offered)+len(p) > c.hold
	c.writes++
	c.offered = append(c.offered, p...)
	first := reaches && !c.held
	if first {
		c.held = true
		c.enteredAt = time.Now()
		close(c.entered)
	}
	// leave is called with c.mu held and releases it.
	leave := func() {
		if first {
			c.exitedAt = time.Now()
			close(c.exited)
		}
		c.mu.Unlock()
	}
	// interrupted returns err once the test lets it, when returns are parked; it is called with c.mu held.
	interrupted := func(err error) (int, error) {
		if c.park {
			if !c.parked {
				c.parked = true
				c.interruptedAt = time.Now()
				close(c.returning)
			}
			release := c.release
			c.mu.Unlock()
			<-release
			c.mu.Lock()
		}
		leave()
		return 0, err
	}
	for {
		switch {
		case c.closed:
			return interrupted(&net.OpError{Op: "write", Net: "unix", Err: net.ErrClosed})
		case !c.deadline.IsZero() && !time.Now().Before(c.deadline):
			return interrupted(&net.OpError{Op: "write", Net: "unix", Err: os.ErrDeadlineExceeded})
		case c.open || !reaches:
			c.mu.Unlock()
			n, err := c.Conn.Write(p)
			c.mu.Lock()
			leave()
			return n, err
		}
		changed, deadline := c.changed, c.deadline
		c.mu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			timer = time.NewTimer(time.Until(deadline))
			expired = timer.C
		}
		select {
		case <-changed:
		case <-expired:
		}
		if timer != nil {
			timer.Stop()
		}
		c.mu.Lock()
	}
}

func (c *gatedConn) setWriteDeadline(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	c.notifyLocked()
}

// SetDeadline keeps the write deadline in the fake and gives the backing connection only the read deadline. The
// backing connection's own timers run on the host's clock, not the bubble's, so a write deadline armed there could
// expire in real time while the bubble's clock stands still and fail a Write the fake let through.
func (c *gatedConn) SetDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.SetReadDeadline(t)
}

// SetWriteDeadline keeps the write deadline in the fake only; see SetDeadline.
func (c *gatedConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return nil
}

func (c *gatedConn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.notifyLocked()
	}
	c.mu.Unlock()
	return c.Conn.Close()
}

// openGate lets every waiting and later Write through to the real end.
func (c *gatedConn) openGate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.open = true
	c.notifyLocked()
}

// attempts returns the bytes every Write so far was asked to write, whether or not they went through, and the
// number of Write calls.
func (c *gatedConn) attempts() (offered string, writes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.offered), c.writes
}

// heldSpan returns the bubble's times of the first held Write's entry and return; exit is zero while it runs.
func (c *gatedConn) heldSpan() (entry, exit time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enteredAt, c.exitedAt
}

// interruption returns the bubble's time at which the first interrupted Write chose its error; zero before that.
func (c *gatedConn) interruption() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interruptedAt
}

// errPair is the failure drainPairs injects.
var errPair = errors.New("drain channel pair not created")

// newTestDrainPair creates a drain channel pair the test owns: a connected local stream socket pair, both
// descriptors marked close-on-exec under syscall.ForkLock, the parent's end as a connection over its own descriptor
// (nonblocking, as net.FileConn makes it) and the child's end as a file. The tests probe a parent's descriptor only
// on such a pair, so that the registry's default pair stays free to return a parent end of any net.Conn type.
func newTestDrainPair() (net.Conn, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		if _, err = unix.FcntlInt(uintptr(fds[0]), unix.F_SETFD, unix.FD_CLOEXEC); err == nil {
			_, err = unix.FcntlInt(uintptr(fds[1]), unix.F_SETFD, unix.FD_CLOEXEC)
		}
		if err != nil {
			_ = unix.Close(fds[0])
			_ = unix.Close(fds[1])
		}
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, err
	}
	parentFile := os.NewFile(uintptr(fds[0]), "test-drain-parent")
	parent, err := net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		_ = unix.Close(fds[1])
		return nil, nil, err
	}
	return parent, os.NewFile(uintptr(fds[1]), "test-drain-child"), nil
}

// drainPairs is a registry's drain channel pair factory under the test's control. Every pair is a real one, made by
// newTestDrainPair; the test keeps its own handle on each parent end. With gate set the parent end is handed to the
// registry wrapped in a gatedConn. The call whose number, counted from one, is fail returns errPair and creates
// nothing.
type drainPairs struct {
	t    *testing.T
	gate bool
	fail int

	mu      sync.Mutex
	calls   int
	parents []net.Conn
	gated   []*gatedConn
}

// newDrainPairs installs a factory on reg; it must be called before the first Reserve of reg.
func newDrainPairs(t *testing.T, reg *workerlink.Registry, gate bool, fail int) *drainPairs {
	p := &drainPairs{t: t, gate: gate, fail: fail}
	workerlink.ReplaceDrainPair(reg, p.pair)
	return p
}

func (p *drainPairs) pair() (net.Conn, *os.File, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == p.fail {
		return nil, nil, errPair
	}
	parent, child, err := newTestDrainPair()
	if err != nil {
		return nil, nil, err
	}
	// The reservation owns both ends; closing them again at the end of the test only keeps a leak from outliving it.
	p.t.Cleanup(func() { _ = parent.Close(); _ = child.Close() })
	p.parents = append(p.parents, parent)
	if !p.gate {
		return parent, child, nil
	}
	g := newGatedConn(parent)
	p.gated = append(p.gated, g)
	return g, child, nil
}

// parent returns the test's own handle on the real parent end of the i-th pair made, counted from zero.
func (p *drainPairs) parent(t *testing.T, i int) net.Conn {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.parents) {
		t.Fatalf("pair %d was not made; %d were", i, len(p.parents))
	}
	return p.parents[i]
}

// gatedAt returns the gated parent end of the i-th pair made, counted from zero.
func (p *drainPairs) gatedAt(t *testing.T, i int) *gatedConn {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.gated) {
		t.Fatalf("gated pair %d was not made; %d were", i, len(p.gated))
	}
	return p.gated[i]
}

// closeGated closes every gated parent end and lets parked returns go, so that no Write stays parked after a
// failed assertion.
func (p *drainPairs) closeGated() {
	p.mu.Lock()
	gated := slices.Clone(p.gated)
	p.mu.Unlock()
	for _, g := range gated {
		g.releaseReturns()
		_ = g.Close()
	}
}

func (p *drainPairs) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// untouched reports whether nothing happened on ch: it is neither closed nor holding a value.
func untouched(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	default:
		return true
	}
}
