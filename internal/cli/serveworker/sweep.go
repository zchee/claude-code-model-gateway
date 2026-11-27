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

package serveworker

import (
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// sweepListener is the supervised worker's TCP listener as its servers see it. Until the final sweep it passes every
// accept to the listener it wraps. The sweep stops that ordinary accept, takes what is already queued on the listener
// without waiting for another connection, and hands those connections to the servers through Accept, so they are
// served as any other connection; the servers then find nothing more until the listener closes.
type sweepListener struct {
	net.Listener
	mu sync.Mutex
	// changed is closed and replaced whenever the fields below change.
	changed   chan struct{}
	accepting bool
	sweeping  bool
	// parked records that the servers' accept waits during the sweep with nothing queued: each connection Accept
	// returned before has been registered by its server.
	parked bool
	// ended records that the server's Serve loop on this listener has returned; a temporary accept error, which the
	// server retries, does not end it.
	ended  bool
	closed bool
	queue  []net.Conn
}

func newSweepListener(ln net.Listener) *sweepListener {
	return &sweepListener{Listener: ln, changed: make(chan struct{})}
}

// changedLocked wakes every wait on the listener's state.
func (l *sweepListener) changedLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *sweepListener) Accept() (net.Conn, error) {
	for {
		l.mu.Lock()
		switch {
		case len(l.queue) > 0:
			conn := l.queue[0]
			l.queue = l.queue[1:]
			l.parked = false
			l.changedLocked()
			l.mu.Unlock()
			return conn, nil
		case l.closed:
			l.mu.Unlock()
			return nil, &net.OpError{Op: "accept", Net: "tcp", Addr: l.Addr(), Err: net.ErrClosed}
		case l.sweeping:
			if !l.parked {
				l.parked = true
				l.changedLocked()
			}
			wait := l.changed
			l.mu.Unlock()
			<-wait
			continue
		}
		l.accepting = true
		l.mu.Unlock()
		conn, err := l.Listener.Accept()
		// The deadline that ends the ordinary accept for the sweep is not an error of the listener.
		l.mu.Lock()
		l.accepting = false
		retry := err != nil && l.sweeping && errors.Is(err, os.ErrDeadlineExceeded)
		l.changedLocked()
		l.mu.Unlock()
		if retry {
			continue
		}
		return conn, err
	}
}

// serveEnded records that the server's Serve loop on this listener has returned.
func (l *sweepListener) serveEnded() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended = true
	l.changedLocked()
}

// SyscallConn is the wrapped listener's, so that the listener's descriptor can be reached through the wrapper.
func (l *sweepListener) SyscallConn() (syscall.RawConn, error) {
	sc, ok := l.Listener.(syscall.Conn)
	if !ok {
		return nil, errors.New("the worker's listener has no descriptor")
	}
	return sc.SyscallConn()
}

func (l *sweepListener) Close() error {
	l.mu.Lock()
	queued := l.queue
	l.queue = nil
	if !l.closed {
		l.closed = true
		l.changedLocked()
	}
	l.mu.Unlock()
	for _, conn := range queued {
		_ = conn.Close()
	}
	return l.Listener.Close()
}

// sweep ends the ordinary accept and hands the connections already queued on the listener to the servers. It returns
// once the servers have taken all of them and come back for more, once the server stopped serving, or once now is
// closed. The queue is read without waiting for a
// connection that has not arrived: a deadline already passed would refuse even queued connections in Go, so the
// queue is read on the descriptor itself until it reports that nothing is ready.
func (l *sweepListener) sweep(now <-chan struct{}) error {
	deadline, ok := l.Listener.(interface{ SetDeadline(time.Time) error })
	if !ok {
		return errors.New("the worker's listener has no accept deadline for its final sweep")
	}
	l.mu.Lock()
	l.sweeping = true
	l.changedLocked()
	l.mu.Unlock()
	if err := deadline.SetDeadline(aLongTimeAgo); err != nil {
		return err
	}
	// The ordinary accept in progress, if any, ends at the deadline; wait until it has returned.
	if !l.await(now, func() bool { return !l.accepting }) {
		return nil
	}
	if err := deadline.SetDeadline(time.Time{}); err != nil {
		return err
	}
	conns, err := acceptQueued(l.Listener, now)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		return err
	}
	l.queue = append(l.queue, conns...)
	l.changedLocked()
	l.mu.Unlock()
	l.await(now, func() bool { return (len(l.queue) == 0 && l.parked) || l.closed || l.ended })
	return err
}

// await returns true once done reports true under the listener's lock, or false when now is closed first.
func (l *sweepListener) await(now <-chan struct{}, done func() bool) bool {
	for {
		l.mu.Lock()
		if done() {
			l.mu.Unlock()
			return true
		}
		wait := l.changed
		l.mu.Unlock()
		select {
		case <-wait:
		case <-now:
			return false
		}
	}
}
