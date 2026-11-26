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
	"net"
	"sync/atomic"
	"time"
)

// socketInactivity is the upstream socket-inactivity deadline of the source runtime.
const socketInactivity = time.Hour

// dialFunc is the shape of http.Transport.DialContext.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// inactivityDialer wraps dial so that every connection it returns closes itself after one hour
// without socket activity. When lifetime is non-nil, an outstanding dial is cancelled and every
// connection is closed as soon as lifetime ends.
func inactivityDialer(dial dialFunc, lifetime context.Context) dialFunc {
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if lifetime != nil {
			// net/http detaches the dial from the request context, so the client's lifetime is
			// the only signal that ends a dial no request waits for any more.
			dialCtx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(lifetime, cancel)
			defer func() {
				stop()
				cancel()
			}()
			ctx = dialCtx
		}
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return newInactivityConn(conn, lifetime), nil
	}
}

// inactivityConn closes its connection once no read or write has made progress for
// socketInactivity, like a Node socket timeout, and from then on reports ErrTimeout for every
// failed operation.
type inactivityConn struct {
	net.Conn
	timer        *time.Timer
	expired      atomic.Bool
	stopLifetime func() bool
}

func newInactivityConn(conn net.Conn, lifetime context.Context) *inactivityConn {
	c := &inactivityConn{Conn: conn}
	c.timer = time.AfterFunc(socketInactivity, func() {
		c.expired.Store(true)
		_ = c.Conn.Close()
	})
	if lifetime != nil {
		c.stopLifetime = context.AfterFunc(lifetime, func() { _ = c.Close() })
	}
	return c
}

// touch records socket activity and moves the inactivity deadline.
func (c *inactivityConn) touch() {
	if !c.expired.Load() {
		c.timer.Reset(socketInactivity)
	}
}

// mapErr reports ErrTimeout for an operation that failed because the inactivity deadline closed
// the connection.
func (c *inactivityConn) mapErr(err error) error {
	if err != nil && c.expired.Load() {
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

// Write writes to the connection; starting a write and completing one both count as activity.
func (c *inactivityConn) Write(p []byte) (int, error) {
	c.touch()
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.touch()
	}
	return n, c.mapErr(err)
}

// Close stops the inactivity deadline and closes the connection.
func (c *inactivityConn) Close() error {
	c.timer.Stop()
	if c.stopLifetime != nil {
		c.stopLifetime()
	}
	return c.Conn.Close()
}
