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

//go:build process && (linux || darwin)

package serveworker

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// sweepStopBound is how long the worker may take to return once its parent ended during its final sweep. A sweep
// that looks at the stop between two accepts needs milliseconds; the bound is for a loaded venue, and it is shorter
// than the time the endless source keeps supplying connections, so a sweep that ignores the stop is still sweeping
// at the bound.
const sweepStopBound = 4 * time.Second

// sweepSource is the listener the worker accepts through in the interrupt cases: the gate, except that once the
// worker announced its final sweep every time the sweep asks for a connection, through Accept or through a raw Control
// callback, the source first dials one onto the worker's real listening socket, so that the queue is not empty when
// the sweep looks. Accept then takes that connection off the socket through its raw descriptor, so that a deadline the
// worker set does not refuse a connection that is ready. The first ask runs first, before it dials, so that every
// connection the sweep receives arrived after it.
type sweepSource struct {
	*gatedListener
	ctx    context.Context
	public string
	mu     sync.Mutex
	// first runs inside the first ask of the sweep.
	first func()
	// supply is how many asks dial a connection; prefill dials that many at the first ask and none later.
	supply  int
	prefill bool
	pause   time.Duration
	// sweeping, asks, dialed and taken describe what the sweep did so far.
	sweeping bool
	asks     int
	dialed   int
	taken    int
	clients  []net.Conn
	closed   chan struct{}
	closing  sync.Once
}

func newSweepSource(ctx context.Context, g *gatedListener) *sweepSource {
	return &sweepSource{gatedListener: g, ctx: ctx, public: g.Addr().String(), closed: make(chan struct{})}
}

// configure sets what the sweep's asks do; the test calls it before it answers the drain.
func (s *sweepSource) configure(first func(), supply int, prefill bool, pause time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.first, s.supply, s.prefill, s.pause = first, supply, prefill, pause
}

// SweepStarting records the announcement and passes it on to the gate.
func (s *sweepSource) SweepStarting() {
	s.mu.Lock()
	s.sweeping = true
	s.mu.Unlock()
	s.gatedListener.SweepStarting()
}

func (s *sweepSource) isSweeping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweeping
}

// ask is one request of the sweep for a connection: it runs the first function on the first ask and dials what the
// configuration says.
func (s *sweepSource) ask() {
	s.mu.Lock()
	s.asks++
	n, first, pause := s.asks, s.first, s.pause
	dials := 0
	switch {
	case s.prefill && n == 1:
		dials = s.supply
	case !s.prefill && n <= s.supply:
		dials = 1
	}
	s.mu.Unlock()
	if n == 1 && first != nil {
		first()
	}
	if n > 1 && dials > 0 && pause > 0 {
		select {
		case <-time.After(pause):
		case <-s.closed:
			return
		}
	}
	for range dials {
		s.dial()
	}
}

// dial opens one connection to the worker's listener that carries a complete health request, and keeps its client.
func (s *sweepSource) dial() {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp4", s.public)
	if err != nil {
		return
	}
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: "+s.public+"\r\nConnection: close\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	s.mu.Lock()
	s.dialed++
	s.clients = append(s.clients, conn)
	s.mu.Unlock()
}

// pending reports whether a dialed connection may still wait in the queue for Accept to take.
func (s *sweepSource) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.taken < s.dialed
}

// takeQueued takes one queued connection off the real listening socket through its descriptor.
func (s *sweepSource) takeQueued() (net.Conn, bool) {
	sc, ok := s.Listener.(syscall.Conn)
	if !ok {
		return nil, false
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return nil, false
	}
	for range 200 {
		nfd := -1
		var acceptErr error
		if err := rc.Control(func(fd uintptr) {
			syscall.ForkLock.RLock()
			defer syscall.ForkLock.RUnlock()
			nfd, _, acceptErr = unix.Accept(int(fd))
			if acceptErr == nil {
				unix.CloseOnExec(nfd)
			}
		}); err != nil {
			return nil, false
		}
		switch {
		case acceptErr == nil:
			f := os.NewFile(uintptr(nfd), "swept connection")
			conn, err := net.FileConn(f)
			_ = f.Close()
			if err != nil {
				return nil, false
			}
			s.mu.Lock()
			s.taken++
			s.mu.Unlock()
			return conn, true
		case errors.Is(acceptErr, unix.EAGAIN), errors.Is(acceptErr, unix.EINTR), errors.Is(acceptErr, unix.ECONNABORTED):
			time.Sleep(time.Millisecond)
		default:
			return nil, false
		}
	}
	return nil, false
}

func (s *sweepSource) Accept() (net.Conn, error) {
	if s.isSweeping() {
		s.ask()
		if s.pending() {
			if conn, ok := s.takeQueued(); ok {
				return conn, nil
			}
		}
	}
	return s.gatedListener.Accept()
}

func (s *sweepSource) SyscallConn() (syscall.RawConn, error) {
	rc, err := s.gatedListener.SyscallConn()
	if err != nil || !s.isSweeping() {
		return rc, err
	}
	return sweepRawConn{RawConn: rc, s: s}, nil
}

func (s *sweepSource) Close() error {
	s.closing.Do(func() { close(s.closed) })
	return s.gatedListener.Close()
}

// stats returns how often the sweep asked and how many connections were dialed for it.
func (s *sweepSource) stats() (asks, dialed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asks, s.dialed
}

// snapshot returns the clients of every connection dialed so far.
func (s *sweepSource) snapshot() []net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]net.Conn(nil), s.clients...)
}

// closeClients closes every client; it runs at cleanup.
func (s *sweepSource) closeClients() {
	for _, c := range s.snapshot() {
		_ = c.Close()
	}
}

// sweepRawConn is the raw connection of the real listening socket, with an ask of the sweep before every Control
// callback. A listener's raw connection refuses Read and Write, so Control is the only way a sweep accepts through it.
type sweepRawConn struct {
	syscall.RawConn
	s *sweepSource
}

func (r sweepRawConn) Control(f func(fd uintptr)) error {
	r.s.ask()
	return r.RawConn.Control(f)
}

// sweepAnswers sorts what each client received: an answer (any byte), a close without a byte, or nothing before the
// watchdog (the connection was left open).
func sweepAnswers(clients []net.Conn) (answered, closed, open int, first string) {
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 64)
	for i, c := range clients {
		_ = c.SetReadDeadline(deadline)
		n, err := c.Read(buf)
		switch {
		case n > 0:
			answered++
			if first == "" {
				first = "connection " + strconv.Itoa(i) + " received " + strings.TrimSpace(string(buf[:n]))
			}
		case errors.Is(err, os.ErrDeadlineExceeded):
			open++
			if first == "" {
				first = "connection " + strconv.Itoa(i) + " was neither answered nor closed"
			}
		default:
			closed++
		}
	}
	return answered, closed, open, first
}

// TestImmediateStopInterruptsFinalSweep checks that the final sweep after an answered drain can be interrupted: while
// connections keep arriving, so that the sweep's accept never reports that it would block, the parent's end of file
// ends the sweep and the worker returns within a bound, and the connections the sweep had collected are closed
// without an answer from the worker's handler. The parent's end comes inside the sweep's first ask for a connection,
// before that connection is dialed, so every connection the sweep receives arrived after the stop. It rejects a sweep
// that looks at the stop only before it begins or only once the queue runs dry, which keeps sweeping past the bound,
// and one that hands the connections it took over to the handler after an immediate stop. The control row has no stop:
// the same source runs dry after three connections, and the sweep then serves all three.
//
// What it does not exercise: the stop is staged through the source before the sweep's accept call, or before a raw
// Control callback is entered; no stop ever arrives while the worker is inside one raw sweep loop. A sweep that
// accepts through the raw descriptor and loops inside one callback until the system reports that it would block runs
// dry after the one connection dialed before that callback; the case then still checks the stop, the bound and the
// closed connection, but neither a stop that arrives inside that loop nor connections that keep arriving during it.
// A private accept function in the worker's sweep, replaceable by the package's tests, would decide it.
func TestImmediateStopInterruptsFinalSweep(t *testing.T) {
	tests := map[string]struct {
		stop bool
	}{
		"error: the parent's end during a sweep that never runs dry ends it and closes what it collected unanswered": {stop: true},
		"success: the control, the same sweep without a stop, runs dry and serves the connections it collected":      {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			var src *sweepSource
			w := startSweepWorker(t, ctx, func(g *gatedListener) net.Listener {
				src = newSweepSource(t.Context(), g)
				return src
			})
			t.Cleanup(src.closeClients)
			stopped := make(chan struct{})
			var stoppedAt time.Time
			if tc.stop {
				// About six seconds of connections at one per five milliseconds, longer than sweepStopBound.
				src.configure(func() {
					stoppedAt = time.Now()
					_ = w.h.liveW.Close()
					close(stopped)
					// The worker's own watcher needs a moment to see the end of file; the sweep's first connection is
					// dialed only after it.
					time.Sleep(50 * time.Millisecond)
				}, 1200, false, 5*time.Millisecond)
			} else {
				src.configure(nil, 3, true, 0)
			}
			tr, drain := w.keepAlive(t)
			w.gate.closeGate()
			// The accept in progress takes this connection; the loop then parks at the closed gate.
			queuedRequest(t, w.public)
			select {
			case <-w.gate.parked:
			case <-time.After(10 * time.Second):
				t.Fatal("the worker's accept loop never came back to the gate")
			}
			w.drainAndAnswer(t, ctx, tr, drain)
			if !tc.stop {
				// The three connections are dialed at the sweep's first ask; the sweep then runs dry and the worker serves them.
				deadline := time.Now().Add(lossBound)
				for time.Now().Before(deadline) {
					if _, dialed := src.stats(); dialed >= 3 || awaitLoss(w.run.done, time.Now().Add(5*time.Millisecond)) {
						break
					}
				}
				clients := src.snapshot()
				if len(clients) != 3 {
					asks, dialed := src.stats()
					t.Fatalf("test infrastructure: the sweep asked %d times and %d connections were dialed for it; want three to stage the control; not a product failure", asks, dialed)
				}
				requireHealthAnswers(t, clients)
				if !awaitLoss(w.run.done, time.Now().Add(lossBound)) {
					t.Fatalf("the worker did not return within %v of serving what its sweep collected", lossBound)
				}
				if w.run.err != nil {
					t.Errorf("the worker returned %v after the drain; want nil", w.run.err)
				}
				return
			}
			select {
			case <-stopped:
			case <-w.run.done:
				select {
				case <-stopped:
				default:
					t.Fatalf("test infrastructure: the worker returned (%v) without its sweep asking for a connection, so the stop could not be placed inside the sweep; not a product failure", w.run.err)
				}
			case <-time.After(lossBound):
				t.Fatalf("test infrastructure: the sweep did not ask for a connection within %v of the answer (announced: %t); not a product failure", lossBound, w.gate.sweepStarted())
			}
			if !awaitLoss(w.run.done, stoppedAt.Add(sweepStopBound)) {
				asks, dialed := src.stats()
				t.Fatalf("the worker was still running %v after its parent's end during the final sweep (the sweep asked %d times, %d connections arrived); want the sweep interrupted", sweepStopBound, asks, dialed)
			}
			// The ended reservation drops whatever still waits in the queue, so that every client sees an end.
			_ = src.Close()
			_ = w.h.res.End()
			asks, dialed := src.stats()
			t.Logf("the interrupted sweep asked %d times; %d connections arrived after the stop; the worker returned %v", asks, dialed, w.run.err)
			answered, closed, open, first := sweepAnswers(src.snapshot())
			if answered != 0 || open != 0 {
				t.Errorf("of %d connections that arrived after the parent's end, %d were answered and %d left open (first: %s); want every one closed without an answer", dialed, answered, open, first)
			}
			if closed == 0 {
				t.Error("test infrastructure: no connection arrived for the sweep after the stop; not a product failure")
			}
		})
	}
}

// TestSweepSourceStaging checks the source of the interrupt cases on any tree, driven by a sweep the test plays
// itself: nothing is dialed before the announcement; the first ask runs its function before it dials; Accept hands
// over a connection that carries a complete request, also with an already-expired deadline; a raw Control callback
// finds a connection waiting; with a prefilled supply the queue runs dry after it.
func TestSweepSourceStaging(t *testing.T) {
	tests := map[string]struct{}{
		"success: the sweep source supplies connections to Accept and to raw callbacks only once the sweep began": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			var cfg net.ListenConfig
			ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			src := newSweepSource(t.Context(), newGatedListener(ln))
			t.Cleanup(func() { _ = src.Close(); src.closeClients() })
			firstDialed := -1
			src.configure(func() { _, firstDialed = src.stats() }, 1000, false, 0)
			if _, dialed := src.stats(); dialed != 0 {
				t.Fatalf("%d connections dialed before the sweep began; want none", dialed)
			}
			src.SweepStarting()
			if err := src.SetDeadline(time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			for i := range 3 {
				conn, err := src.Accept()
				if err != nil {
					t.Fatalf("accept %d during the sweep with an expired deadline: %v", i, err)
				}
				line, err := bufio.NewReader(conn).ReadString('\n')
				_ = conn.Close()
				if err != nil || line != "GET /healthz HTTP/1.1\r\n" {
					t.Errorf("accept %d carried %q, %v; want a health request", i, line, err)
				}
			}
			if firstDialed != 0 {
				t.Errorf("the first ask's function saw %d connections dialed; want it to run before any", firstDialed)
			}
			if err := src.SetDeadline(time.Time{}); err != nil {
				t.Fatal(err)
			}
			rc, err := src.SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var got int
			var acceptErr error
			if err := rc.Control(func(fd uintptr) {
				nfd, _, err := unix.Accept(int(fd))
				if acceptErr = err; err == nil {
					got++
					_ = unix.Close(nfd)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if acceptErr != nil {
				t.Errorf("a raw accept during the sweep: %v; want a connection waiting", acceptErr)
			}
			if got != 1 {
				t.Errorf("a raw callback during the sweep took %d connections; want one waiting", got)
			}

			dry := newSweepSource(t.Context(), newGatedListener(ln))
			t.Cleanup(dry.closeClients)
			dry.configure(nil, 2, true, 0)
			dry.SweepStarting()
			if err := dry.SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				conn, err := dry.Accept()
				if err != nil {
					t.Fatalf("accept %d of the prefilled supply: %v", i, err)
				}
				_ = conn.Close()
			}
			if _, err := dry.Accept(); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("accept after the prefilled supply: %v; want the deadline's timeout of an empty queue", err)
			}
		})
	}
}
