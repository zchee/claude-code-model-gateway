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

package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// socketInactivity is the upstream socket-inactivity deadline of the source runtime.
const socketInactivity = time.Hour

// dialFunc is the shape of http.Transport.DialContext.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// inactivityDialer wraps dial so that every connection it returns closes itself after one hour
// without socket activity, and a connect that does not finish within that hour fails with
// ErrTimeout. When lifetime is non-nil, no dial starts once lifetime has ended, an outstanding dial
// is cancelled and every connection is closed as soon as lifetime ends.
func inactivityDialer(dial dialFunc, lifetime context.Context) dialFunc {
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if lifetime != nil && lifetime.Err() != nil {
			return nil, errClientClosed
		}
		// The source arms its socket timeout before the connect, so the connect is bounded too.
		dialCtx, cancel := context.WithTimeoutCause(ctx, socketInactivity, ErrTimeout)
		defer cancel()
		if lifetime != nil {
			// net/http detaches the dial from the request context, so the client's lifetime is
			// the only signal that ends a dial no request waits for any more.
			stop := context.AfterFunc(lifetime, cancel)
			defer stop()
		}
		conn, err := dial(dialCtx, network, addr)
		if err != nil {
			// A dialer reports its own error for an expired context (net.Dialer: an i/o timeout),
			// so the deadline is recognised by its cause.
			if errors.Is(context.Cause(dialCtx), ErrTimeout) {
				return nil, ErrTimeout
			}
			return nil, err
		}
		return newInactivityConn(conn, lifetime), nil
	}
}

// inactivityConn closes its connection once no read or write has made progress for
// socketInactivity, like a Node socket timeout, and from then on reports ErrTimeout for every
// failed operation other than a clean end of stream.
type inactivityConn struct {
	net.Conn
	expired atomic.Bool

	// mu orders Close, the inactivity timer, the lifetime callback and activity, so that nothing
	// re-arms the timer after the connection is closed.
	mu           sync.Mutex
	timer        *time.Timer
	closed       bool
	stopLifetime func() bool
}

func newInactivityConn(conn net.Conn, lifetime context.Context) *inactivityConn {
	c := &inactivityConn{Conn: conn}
	// The lifetime callback runs at once when lifetime has already ended; it takes mu first, so it
	// cannot observe the registrations half stored.
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timer = time.AfterFunc(socketInactivity, c.expire)
	if lifetime != nil {
		c.stopLifetime = context.AfterFunc(lifetime, func() { _ = c.Close() })
	}
	return c
}

// shutdown marks the connection closed and releases its timer and lifetime registration. It
// reports false when the connection was already closed, so the wrapped connection is closed once.
func (c *inactivityConn) shutdown(expired bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.closed = true
	if expired {
		// Stored before the wrapped connection closes, so the operation that fails sees it.
		c.expired.Store(true)
	}
	c.timer.Stop()
	if c.stopLifetime != nil {
		c.stopLifetime()
	}
	return true
}

// expire closes the connection when the inactivity deadline passes.
func (c *inactivityConn) expire() {
	if c.shutdown(true) {
		_ = c.Conn.Close()
	}
}

// touch records socket activity and moves the inactivity deadline of an open connection.
func (c *inactivityConn) touch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.timer.Reset(socketInactivity)
	}
}

// mapErr reports ErrTimeout for an operation that failed because the inactivity deadline closed
// the connection. A clean end of stream stays io.EOF: the response it ends is complete.
func (c *inactivityConn) mapErr(err error) error {
	if err != nil && !errors.Is(err, io.EOF) && c.expired.Load() {
		return ErrTimeout
	}
	return err
}

// Read reads from the connection; received bytes count as activity.
func (c *inactivityConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch()
	}
	return n, c.mapErr(err)
}

// Write writes to the connection; starting a write of at least one byte and completing one both
// count as activity.
func (c *inactivityConn) Write(p []byte) (int, error) {
	if len(p) > 0 {
		c.touch()
	}
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.touch()
	}
	return n, c.mapErr(err)
}

// Close stops the inactivity deadline and closes the connection. Later calls change nothing and
// return net.ErrClosed.
func (c *inactivityConn) Close() error {
	if !c.shutdown(false) {
		return net.ErrClosed
	}
	return c.Conn.Close()
}
