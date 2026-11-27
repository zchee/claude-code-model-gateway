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

package upstream_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const (
	// fakeUpstreamBase is the origin every in-memory upstream of these tests answers for; the
	// dial hooks never resolve it.
	fakeUpstreamBase = "http://upstream.test"

	okReply      = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 11\r\n\r\n{\"ok\":true}"
	okReplyBody  = `{"ok":true}`
	partialReply = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 64\r\n\r\ndata: first\n\n"

	// clientClosedText restates the text of errClientClosed, the error of a request made on, or
	// ended by, a closed Client; it wraps no other error.
	clientClosedText = "upstream client closed"
)

// messagesRequest is a POST with a body and no idempotency key header, which the documented retry
// rules of net/http make ineligible for a replay on another connection; that the Go 1.27.4 transport
// sends it once on the failures these tests simulate is observed behaviour.
func messagesRequest() upstream.Request {
	return upstream.Request{
		Target:        upstream.TargetAnthropic,
		BaseURL:       fakeUpstreamBase,
		Method:        http.MethodPost,
		RequestURI:    "/v1/messages",
		ClientHeaders: http.Header{"Content-Type": {"application/json"}},
		Body:          []byte(`{"model":"claude-sonnet-4-6"}`),
	}
}

// faultMode selects how a faultConn models I/O that completes at the moment the connection closes.
type faultMode int

const (
	// faultNone passes every operation through to the in-memory connection.
	faultNone faultMode = iota
	// faultHoldWrites makes a Write wait until the connection is closed and then report every byte
	// as written, like a write whose data left just as the connection closed. A Write that starts
	// after the close fails.
	faultHoldWrites
	// faultLateRead makes the first Read that fails because the connection was closed report one
	// received byte instead, like data that arrived just as the connection closed.
	faultLateRead
	// faultHoldEOF reports the end of the stream only once the connection has been closed, or after
	// one hour, like an end of stream that is received just before the inactivity deadline and seen
	// by the reader only after it.
	faultHoldEOF
)

// faultConn is the client end of an in-memory upstream connection. It records every Close call
// with its virtual time.
type faultConn struct {
	net.Conn
	mode faultMode

	// collect, with faultHoldEOF, makes a Read that receives bytes keep reading until the stream
	// ends, so the final bytes and the end of stream are reported by one call.
	collect atomic.Bool

	lateReadDone atomic.Bool
	writeEntered chan struct{}
	enterOnce    sync.Once
	closed       chan struct{}
	closeOnce    sync.Once

	mu     sync.Mutex
	closes []time.Time
}

func newFaultConn(c net.Conn, mode faultMode) *faultConn {
	return &faultConn{
		Conn:         c,
		mode:         mode,
		writeEntered: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (c *faultConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *faultConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	switch c.mode {
	case faultLateRead:
		if err != nil && len(p) > 0 && c.isClosed() && c.lateReadDone.CompareAndSwap(false, true) {
			p[0] = 'H'
			return 1, nil
		}
	case faultHoldEOF:
		if err == nil && n > 0 && c.collect.Load() {
			for err == nil && n < len(p) {
				var m int
				m, err = c.Conn.Read(p[n:])
				n += m
			}
		}
		if errors.Is(err, io.EOF) {
			select {
			case <-c.closed:
			case <-time.After(time.Hour):
			}
			return n, io.EOF
		}
	}
	return n, err
}

func (c *faultConn) Write(p []byte) (int, error) {
	c.enterOnce.Do(func() { close(c.writeEntered) })
	if c.mode == faultHoldWrites {
		if c.isClosed() {
			return 0, net.ErrClosed
		}
		<-c.closed
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func (c *faultConn) Close() error {
	c.mu.Lock()
	c.closes = append(c.closes, time.Now())
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// closeTimes reports the virtual time of every Close call so far.
func (c *faultConn) closeTimes() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.closes)
}

// pipeUpstream dials in-memory connections, runs serve on the upstream end of each in its own
// goroutine, and records the client ends it returned.
type pipeUpstream struct {
	mode  faultMode
	serve func(peer net.Conn, client *faultConn)
	peers sync.WaitGroup
	dials atomic.Int32

	mu    sync.Mutex
	conns []*faultConn
}

func (u *pipeUpstream) DialContext(context.Context, string, string) (net.Conn, error) {
	u.dials.Add(1)
	c1, c2 := net.Pipe()
	fc := newFaultConn(c1, u.mode)
	u.mu.Lock()
	u.conns = append(u.conns, fc)
	u.mu.Unlock()
	u.peers.Go(func() { u.serve(c2, fc) })
	return fc, nil
}

func (u *pipeUpstream) dialed() []*faultConn {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.conns)
}

// serveHTTP answers every request read from c with the reply that reply returns for it, until c
// fails, and then closes c. An empty reply answers nothing and waits for the next request.
func serveHTTP(c net.Conn, reply func(*http.Request) string) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		if r := reply(req); r != "" {
			if _, err := io.WriteString(c, r); err != nil {
				return
			}
		}
	}
}

// replyAfterFirstByte discards everything the client sends; once the first byte has arrived (so
// the transport has registered the request the reply answers) it writes reply. It closes c when the
// client has closed its end. An empty reply makes a silent upstream.
func replyAfterFirstByte(c net.Conn, reply string) {
	defer func() { _ = c.Close() }()
	first := make(chan struct{})
	firstOnce := sync.OnceFunc(func() { close(first) })
	var drain sync.WaitGroup
	drain.Go(func() {
		defer firstOnce()
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				firstOnce()
			}
			if err != nil {
				return
			}
		}
	})
	<-first
	if reply != "" {
		_, _ = io.WriteString(c, reply)
	}
	drain.Wait()
}

// doAndDrain sends req and returns the whole response body.
func doAndDrain(ctx context.Context, client *upstream.Client, req upstream.Request) (string, error) {
	resp, err := client.Do(ctx, req)
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	return string(body), err
}

func TestClientCloseRacingDialClosesConnection(t *testing.T) {
	// Each iteration closes a client whose only dial is returning its connection; run the race
	// tiers to see unsynchronized access between the two.
	const iterations = 64

	tests := map[string]struct {
		// afterCancel makes the dial return its connection only after Client.Close has canceled the
		// dial, so the client's lifetime has already ended when the connection is handed over.
		afterCancel bool
	}{
		"success: a connection a dial returns after Client.Close canceled it ends up closed": {
			afterCancel: true,
		},
		"success: a connection a dial returns while Client.Close runs ends up closed": {
			afterCancel: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				for i := range iterations {
					entered := make(chan struct{})
					enteredOnce := sync.OnceFunc(func() { close(entered) })
					release := make(chan struct{})
					releaseOnce := sync.OnceFunc(func() { close(release) })

					var peers sync.WaitGroup
					var connMu sync.Mutex
					var conns []*faultConn
					dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
						enteredOnce()
						if tt.afterCancel {
							select {
							case <-ctx.Done():
							case <-release:
							}
						} else {
							<-release
						}
						c1, c2 := net.Pipe()
						fc := newFaultConn(c1, faultNone)
						connMu.Lock()
						conns = append(conns, fc)
						connMu.Unlock()
						peers.Go(func() { replyAfterFirstByte(c2, "") })
						return fc, nil
					}

					client := upstream.NewClient(upstream.Options{DialContext: dial})
					done := make(chan error, 1)
					var reqWG sync.WaitGroup
					reqWG.Go(func() {
						resp, err := client.Do(t.Context(), messagesRequest())
						if resp != nil {
							_ = resp.Body.Close()
						}
						done <- err
					})

					select {
					case <-entered:
					case err := <-done:
						t.Fatalf("iteration %d: Do returned before dialing: %v", i, err)
					}

					if tt.afterCancel {
						_ = client.Close()
					} else {
						start := make(chan struct{})
						var racers sync.WaitGroup
						racers.Go(func() {
							<-start
							_ = client.Close()
						})
						racers.Go(func() {
							<-start
							releaseOnce()
						})
						close(start)
						racers.Wait()
					}

					var doErr error
					select {
					case doErr = <-done:
					case <-time.After(time.Minute):
						t.Errorf("iteration %d: Do still pending a minute after Client.Close", i)
						releaseOnce()
						doErr = <-done
					}
					releaseOnce()
					reqWG.Wait()
					synctest.Wait()

					// The public result of Do is checked, not which layer produced the closed-client
					// error.
					if doErr == nil {
						t.Errorf("iteration %d: Do returned no error for a request Client.Close ended", i)
					} else if diff := gocmp.Diff([]string{clientClosedText}, errorTexts(doErr)); diff != "" {
						t.Errorf("iteration %d: texts of the error of a request Client.Close ended (-want +got):\n%s", i, diff)
					}

					connMu.Lock()
					returned := slices.Clone(conns)
					connMu.Unlock()
					if len(returned) == 0 {
						t.Fatalf("iteration %d: the dial returned no connection", i)
					}
					for j, fc := range returned {
						if !fc.isClosed() {
							t.Errorf("iteration %d: connection %d returned by the dial is still open after Client.Close", i, j)
							_ = fc.Close()
						}
					}
					peers.Wait()
				}
			})
		})
	}
}

// closeAction says how a test ends the use of an upstream connection.
type closeAction int

const (
	closeBody closeAction = iota
	closeIdle
	closeClient
)

func TestClosedConnectionKeepsNoInactivityTimer(t *testing.T) {
	tests := map[string]struct {
		standalone bool
		mode       faultMode
		closeBy    closeAction
	}{
		"success: closing a response body while the request write completes leaves no inactivity timer": {
			mode:    faultHoldWrites,
			closeBy: closeBody,
		},
		"success: closing idle connections while a read completes leaves no inactivity timer": {
			standalone: true,
			mode:       faultLateRead,
			closeBy:    closeIdle,
		},
		"success: Client.Close while a read on an idle connection completes leaves no inactivity timer": {
			mode:    faultLateRead,
			closeBy: closeClient,
		},
		"success: closing a response body early leaves no inactivity timer": {
			mode:    faultNone,
			closeBy: closeBody,
		},
		"success: Client.Close of an idle connection leaves no inactivity timer": {
			mode:    faultNone,
			closeBy: closeClient,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reply := okReply
				if tt.closeBy == closeBody {
					reply = partialReply
				}
				up := &pipeUpstream{mode: tt.mode}
				if tt.mode == faultHoldWrites {
					// The request never reaches the upstream; it answers once the transport has
					// started writing, so the response belongs to the request.
					up.serve = func(peer net.Conn, fc *faultConn) {
						defer func() { _ = peer.Close() }()
						select {
						case <-fc.writeEntered:
						case <-fc.closed:
							return
						}
						if _, err := io.WriteString(peer, reply); err != nil {
							return
						}
						_, _ = io.Copy(io.Discard, peer)
					}
				} else {
					up.serve = func(peer net.Conn, _ *faultConn) {
						serveHTTP(peer, func(*http.Request) string { return reply })
					}
				}

				var (
					client *upstream.Client
					tr     *http.Transport
					body   io.ReadCloser
				)
				if tt.standalone {
					tr = upstream.NewTransport(upstream.Options{DialContext: up.DialContext})
					defer tr.CloseIdleConnections()
				} else {
					client = upstream.NewClient(upstream.Options{DialContext: up.DialContext})
					defer func() { _ = client.Close() }()
				}
				defer up.peers.Wait()

				if tt.standalone {
					req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fakeUpstreamBase+"/.well-known/openid-configuration", nil)
					if err != nil {
						t.Fatalf("NewRequestWithContext: %v", err)
					}
					resp, err := tr.RoundTrip(req) //nolint:bodyclose // The body is closed below, by the way each case ends the connection.
					if err != nil {
						t.Fatalf("RoundTrip returned error: %v", err)
					}
					body = resp.Body
				} else {
					resp, err := client.Do(t.Context(), messagesRequest())
					if err != nil {
						t.Fatalf("Do returned error: %v", err)
					}
					body = resp.Body
				}

				if tt.closeBy == closeBody {
					_ = body.Close()
				} else {
					got, err := io.ReadAll(body)
					_ = body.Close()
					if err != nil {
						t.Fatalf("reading response body: %v", err)
					}
					if diff := gocmp.Diff(okReplyBody, string(got)); diff != "" {
						t.Fatalf("response body (-want +got):\n%s", diff)
					}
					// Let the transport park the connection and start waiting for its next byte.
					synctest.Wait()
					if tt.closeBy == closeIdle {
						tr.CloseIdleConnections()
					} else {
						_ = client.Close()
					}
				}

				conns := up.dialed()
				if len(conns) != 1 {
					t.Fatalf("dialed %d connections, want 1", len(conns))
				}
				fc := conns[0]
				// net/http may first try briefly to drain an early-closed body before it closes the
				// connection.
				select {
				case <-fc.closed:
				case <-time.After(time.Minute):
					t.Fatal("the connection was not closed within a minute")
				}
				synctest.Wait()
				before := fc.closeTimes()
				closedAt := before[0]

				// A timer that outlives the connection fires one hour after it was last armed and
				// closes the connection again; time does not advance once the bubble's main
				// goroutine has returned, so wait here past that point.
				time.Sleep(2 * time.Hour)
				synctest.Wait()
				after := fc.closeTimes()
				if len(after) != len(before) {
					var late []time.Duration
					for _, at := range after[len(before):] {
						late = append(late, at.Sub(closedAt))
					}
					t.Errorf("connection closed again %v after it had been closed: an inactivity timer outlived the connection", late)
				}
			})
		})
	}
}

func TestCleanEndOfStreamAtInactivityExpiry(t *testing.T) {
	const (
		head    = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n"
		partOne = "data: first\n\n"
	)

	tests := map[string]struct {
		tail string
	}{
		"success: a stream whose end arrives just before the inactivity deadline is complete": {
			tail: "",
		},
		"success: final bytes and the end of stream arriving together just before the inactivity deadline are complete": {
			tail: "data: last\n\n",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// The body is delimited by the end of the stream, so the connection's end of
				// stream is what completes the response.
				up := &pipeUpstream{mode: faultHoldEOF}
				up.serve = func(peer net.Conn, _ *faultConn) {
					defer func() { _ = peer.Close() }()
					br := bufio.NewReader(peer)
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, req.Body)
					if _, err := io.WriteString(peer, head+partOne); err != nil {
						return
					}
					time.Sleep(time.Hour - time.Nanosecond)
					if tt.tail != "" {
						_, _ = io.WriteString(peer, tt.tail)
					}
				}

				client := upstream.NewClient(upstream.Options{DialContext: up.DialContext})
				defer func() {
					_ = client.Close()
					up.peers.Wait()
				}()

				start := time.Now()
				resp, err := client.Do(t.Context(), messagesRequest())
				if err != nil {
					t.Fatalf("Do returned error: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()

				got := make([]byte, len(partOne))
				if _, err := io.ReadFull(resp.Body, got); err != nil {
					t.Fatalf("reading first frame: %v", err)
				}
				if diff := gocmp.Diff(partOne, string(got)); diff != "" {
					t.Fatalf("first frame (-want +got):\n%s", diff)
				}
				conns := up.dialed()
				if len(conns) != 1 {
					t.Fatalf("dialed %d connections, want 1", len(conns))
				}
				conns[0].collect.Store(true)

				rest, err := io.ReadAll(resp.Body)
				if elapsed := time.Since(start); elapsed < time.Hour-time.Nanosecond {
					t.Fatalf("stream ended after %v, before the upstream ended it", elapsed)
				}
				if err != nil {
					t.Fatalf("complete response ending at the inactivity deadline failed: %v (ErrTimeout: %t)", err, errors.Is(err, upstream.ErrTimeout))
				}
				if diff := gocmp.Diff(tt.tail, string(rest)); diff != "" {
					t.Fatalf("rest of stream (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func TestIdlePooledConnectionEviction(t *testing.T) {
	// wantIdleTimeout restates idlePoolTimeout, the idle-pool timeout NewTransport sets.
	const wantIdleTimeout = 55 * time.Minute
	// keepAliveHintReply is okReply with a Keep-Alive hint far shorter than the idle-pool timeout;
	// the source's agent would act on the hint, the Go 1.27.4 transport ignores it (observed
	// implementation behaviour, not a documented guarantee).
	const keepAliveHintReply = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: keep-alive\r\nKeep-Alive: timeout=5\r\nContent-Length: 11\r\n\r\n{\"ok\":true}"

	// NewClient uses the transport policy of NewTransport, whose idle timeout is visible.
	idleTimeout := upstream.NewTransport(upstream.Options{}).IdleConnTimeout

	tests := map[string]struct {
		// warmReply answers the warm-up request; empty means okReply.
		warmReply string
		// replyDelay is how long the upstream waits after reading the warm-up request before it
		// writes the whole reply, which moves the connection's last I/O away from its dial.
		replyDelay time.Duration
		// hold is how long the warm-up response body stays unread after all of its bytes arrived,
		// which delays the moment the connection is parked but not its last I/O.
		hold time.Duration
		// idle is how long the parked connection waits for the second request, given the expected
		// idle-pool timeout.
		idle      func(idleTimeout time.Duration) time.Duration
		wantDials int32
		// wantFirstClose, when set, is when the first connection must be closed for the first time,
		// measured from the warm-up request, which dials it.
		wantFirstClose time.Duration
	}{
		"success: a connection whose upstream hinted a five-second Keep-Alive timeout is reused after just under the idle timeout": {
			warmReply: keepAliveHintReply,
			idle:      func(d time.Duration) time.Duration { return d - time.Nanosecond },
			wantDials: 1,
		},
		// Parked at 10 minutes, the pool would evict the connection at 65 minutes; the inactivity
		// deadline counts from the last I/O and closes it at 60 minutes, so the second request at
		// 61 minutes needs a fresh connection.
		"success: a connection parked after its response body was held for ten minutes closes one hour after its last I/O, before its idle timeout": {
			hold:           10 * time.Minute,
			idle:           func(time.Duration) time.Duration { return 51 * time.Minute },
			wantDials:      2,
			wantFirstClose: time.Hour,
		},
		// Dialed at 0, last I/O at 5 minutes when the reply arrives, parked at 15 minutes after a
		// ten-minute hold: an hour counted from the dial would close the connection at 60 minutes,
		// the pool would evict it at 70, and the inactivity deadline counted from the last I/O
		// closes it at 65. The second request at 66 minutes needs a fresh connection.
		"success: a connection whose reply arrived five minutes after its dial closes one hour after that reply, not after the dial": {
			replyDelay:     5 * time.Minute,
			hold:           10 * time.Minute,
			idle:           func(time.Duration) time.Duration { return 51 * time.Minute },
			wantDials:      2,
			wantFirstClose: 5*time.Minute + time.Hour,
		},
		"success: a connection idle for just under the idle timeout is reused": {
			idle:      func(d time.Duration) time.Duration { return d - time.Nanosecond },
			wantDials: 1,
		},
		"success: a connection idle for just over the idle timeout is not reused": {
			idle:      func(d time.Duration) time.Duration { return d + time.Nanosecond },
			wantDials: 2,
		},
		"success: a request with a body after just under one hour of idleness succeeds on a fresh connection": {
			idle:      func(time.Duration) time.Duration { return time.Hour - time.Nanosecond },
			wantDials: 2,
		},
		"success: a request with a body after exactly one hour of idleness succeeds on a fresh connection": {
			idle:      func(time.Duration) time.Duration { return time.Hour },
			wantDials: 2,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(wantIdleTimeout, idleTimeout); diff != "" {
				t.Fatalf("NewTransport IdleConnTimeout (-want +got):\n%s", diff)
			}
			if idleTimeout <= 0 || idleTimeout >= time.Hour {
				t.Fatalf("NewTransport IdleConnTimeout = %v, want a period in (0, %v) so the transport evicts an idle connection before its socket inactivity deadline can close it under a new request", idleTimeout, time.Hour)
			}
			synctest.Test(t, func(t *testing.T) {
				var served atomic.Int32
				up := &pipeUpstream{mode: faultNone}
				up.serve = func(peer net.Conn, _ *faultConn) {
					serveHTTP(peer, func(*http.Request) string {
						// The first request served is the warm-up.
						if served.Add(1) != 1 {
							return okReply
						}
						time.Sleep(tt.replyDelay)
						if tt.warmReply != "" {
							return tt.warmReply
						}
						return okReply
					})
				}
				client := upstream.NewClient(upstream.Options{DialContext: up.DialContext})
				defer func() {
					_ = client.Close()
					up.peers.Wait()
				}()

				start := time.Now()
				warm, err := client.Do(t.Context(), messagesRequest())
				if err != nil {
					t.Fatalf("warm-up request failed: %v", err)
				}
				// The whole reply arrives with its head, so a held body waits in the transport's
				// buffer and the connection sees no I/O meanwhile.
				time.Sleep(tt.hold)
				warmBody, err := io.ReadAll(warm.Body)
				if closeErr := warm.Body.Close(); err == nil {
					err = closeErr
				}
				if err != nil {
					t.Fatalf("reading the warm-up response body: %v", err)
				}
				if diff := gocmp.Diff(okReplyBody, string(warmBody)); diff != "" {
					t.Fatalf("warm-up response body (-want +got):\n%s", diff)
				}
				// The transport's idle timer starts when the connection is parked.
				synctest.Wait()

				idle := tt.idle(wantIdleTimeout)
				time.Sleep(idle)
				body, err := doAndDrain(t.Context(), client, messagesRequest())
				if err != nil {
					t.Fatalf("request with a body after %v of idleness failed: %v (ErrTimeout: %t)", idle, err, errors.Is(err, upstream.ErrTimeout))
				}
				if diff := gocmp.Diff(okReplyBody, body); diff != "" {
					t.Fatalf("response body (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.wantDials, up.dials.Load()); diff != "" {
					t.Fatalf("dials after %v of idleness with idle timeout %v (-want +got):\n%s", idle, idleTimeout, diff)
				}
				if tt.wantDials > 1 {
					synctest.Wait()
					if first := up.dialed()[0]; !first.isClosed() {
						t.Errorf("connection idle for %v (idle timeout %v) is still open", idle, idleTimeout)
					}
				}
				if tt.wantFirstClose > 0 {
					closes := up.dialed()[0].closeTimes()
					if len(closes) == 0 {
						t.Fatalf("first connection was never closed, want a close %v after its dial", tt.wantFirstClose)
					}
					if diff := gocmp.Diff(tt.wantFirstClose, closes[0].Sub(start)); diff != "" {
						t.Errorf("first close of the first connection, measured from its dial (-want +got):\n%s", diff)
					}
				}
			})
		})
	}
}

func TestZeroLengthWritesDoNotExtendInactivity(t *testing.T) {
	// The transport's dial hook hands out the connection the inactivity deadline governs, so the
	// test writes on that connection directly.
	writesAt := []time.Duration{20 * time.Minute, 40 * time.Minute, 59 * time.Minute}

	tests := map[string]struct {
		payload    []byte
		wantExpiry time.Duration
	}{
		"error: nil writes leave a silent connection to expire one hour after it was opened": {
			payload:    nil,
			wantExpiry: time.Hour,
		},
		"error: empty writes leave a silent connection to expire one hour after it was opened": {
			payload:    []byte{},
			wantExpiry: time.Hour,
		},
		"success: one-byte writes move the inactivity deadline": {
			payload:    []byte("x"),
			wantExpiry: 59*time.Minute + time.Hour,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var peers sync.WaitGroup
				tr := upstream.NewTransport(upstream.Options{DialContext: func(context.Context, string, string) (net.Conn, error) {
					c1, c2 := net.Pipe()
					peers.Go(func() { replyAfterFirstByte(c2, "") })
					return c1, nil
				}})
				start := time.Now()
				conn, err := tr.DialContext(t.Context(), "tcp", "upstream.test:80")
				if err != nil {
					t.Fatalf("transport dial: %v", err)
				}
				readErr := make(chan error, 1)
				var readers sync.WaitGroup
				defer func() {
					_ = conn.Close()
					readers.Wait()
					peers.Wait()
				}()
				readers.Go(func() {
					_, err := conn.Read(make([]byte, 1))
					readErr <- err
				})

				for _, at := range writesAt {
					time.Sleep(at - time.Since(start))
					n, err := conn.Write(tt.payload)
					if err != nil {
						t.Fatalf("write of %d bytes at %v: %v", len(tt.payload), at, err)
					}
					if diff := gocmp.Diff(len(tt.payload), n); diff != "" {
						t.Fatalf("bytes written at %v (-want +got):\n%s", at, diff)
					}
				}

				time.Sleep(tt.wantExpiry - time.Nanosecond - time.Since(start))
				synctest.Wait()
				select {
				case err := <-readErr:
					t.Fatalf("connection failed at %v, before %v: %v", time.Since(start), tt.wantExpiry, err)
				default:
				}

				time.Sleep(time.Nanosecond)
				synctest.Wait()
				select {
				case err := <-readErr:
					if !errors.Is(err, upstream.ErrTimeout) {
						t.Fatalf("read at the inactivity deadline = %v, want ErrTimeout", err)
					}
				default:
					t.Fatalf("connection still open at %v; writes of %d bytes at %v must not extend its inactivity deadline past %v", time.Since(start), len(tt.payload), writesAt, tt.wantExpiry)
				}
			})
		})
	}
}

func TestDialWithoutCompletionTimesOut(t *testing.T) {
	tests := map[string]struct {
		standalone bool
	}{
		"error: Client.Do whose dial never completes fails with ErrTimeout after the inactivity period": {
			standalone: false,
		},
		"error: a standalone transport round trip whose dial never completes fails with ErrTimeout after the inactivity period": {
			standalone: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan struct{})
				enteredOnce := sync.OnceFunc(func() { close(entered) })
				release := make(chan struct{})
				releaseOnce := sync.OnceFunc(func() { close(release) })
				// Like net.Dialer, the dial reports only its context's error; it never completes on
				// its own.
				dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
					enteredOnce()
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-release:
						return nil, errors.New("dial fixture released")
					}
				}

				done := make(chan error, 1)
				var reqWG sync.WaitGroup
				var closeAll func()
				if tt.standalone {
					tr := upstream.NewTransport(upstream.Options{DialContext: dial})
					closeAll = tr.CloseIdleConnections
					reqWG.Go(func() {
						req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fakeUpstreamBase+"/.well-known/openid-configuration", nil)
						if err != nil {
							done <- err
							return
						}
						resp, err := tr.RoundTrip(req)
						if resp != nil {
							_ = resp.Body.Close()
						}
						done <- err
					})
				} else {
					client := upstream.NewClient(upstream.Options{DialContext: dial})
					closeAll = func() { _ = client.Close() }
					reqWG.Go(func() {
						resp, err := client.Do(t.Context(), messagesRequest())
						if resp != nil {
							_ = resp.Body.Close()
						}
						done <- err
					})
				}
				defer func() {
					releaseOnce()
					closeAll()
					reqWG.Wait()
				}()

				select {
				case <-entered:
				case err := <-done:
					t.Fatalf("request returned before dialing: %v", err)
				}
				start := time.Now()

				time.Sleep(time.Hour - time.Nanosecond)
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("request failed %v into its dial, before the inactivity period: %v", time.Since(start), err)
				default:
				}

				time.Sleep(time.Nanosecond)
				synctest.Wait()
				select {
				case err := <-done:
					if !errors.Is(err, upstream.ErrTimeout) {
						t.Fatalf("request whose dial never completed = %v, want ErrTimeout", err)
					}
				default:
					t.Fatalf("request still dialing %v after the dial started", time.Since(start))
				}
			})
		})
	}
}

func TestClientCloseStartsNoDial(t *testing.T) {
	// The transport allows 64 connections per host; further requests wait for a connection to
	// close and then dial. Client.Close closes the busy connections, so a waiting request could
	// reach the dial only after the client's lifetime has ended. Several rounds make a dial that
	// slips through visible.
	const (
		rounds = 8
		busy   = 64
		queued = 64
	)

	t.Run("error: requests waiting behind the connection limit start no dial once Client.Close was called", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			for round := range rounds {
				var dials, dialsAfterClose atomic.Int32
				var closing atomic.Bool
				var peers sync.WaitGroup
				dial := func(context.Context, string, string) (net.Conn, error) {
					if closing.Load() {
						dialsAfterClose.Add(1)
					}
					dials.Add(1)
					c1, c2 := net.Pipe()
					peers.Go(func() {
						serveHTTP(c2, func(*http.Request) string { return "" })
					})
					return c1, nil
				}

				client := upstream.NewClient(upstream.Options{DialContext: dial})
				errs := make(chan error, busy+queued)
				var reqWG sync.WaitGroup
				for range busy + queued {
					reqWG.Go(func() {
						resp, err := client.Do(t.Context(), messagesRequest())
						if resp != nil {
							_ = resp.Body.Close()
						}
						errs <- err
					})
				}
				synctest.Wait()
				if got := dials.Load(); got != busy {
					_ = client.Close()
					reqWG.Wait()
					peers.Wait()
					t.Fatalf("round %d: %d dials with %d requests outstanding, want %d", round, got, busy+queued, busy)
				}

				closing.Store(true)
				_ = client.Close()
				reqWG.Wait()
				peers.Wait()
				close(errs)

				if n := dialsAfterClose.Load(); n != 0 {
					t.Errorf("round %d: %d dial(s) started after Client.Close was called", round, n)
				}
				var succeeded int
				// otherTexts counts the error texts other than the closed-client text, by text.
				otherTexts := map[string]int{}
				for err := range errs {
					if err == nil {
						succeeded++
						continue
					}
					if text := err.Error(); text != clientClosedText {
						otherTexts[text]++
					}
				}
				if succeeded != 0 {
					t.Errorf("round %d: %d request(s) succeeded against a silent upstream after Client.Close", round, succeeded)
				}
				if diff := gocmp.Diff(map[string]int{}, otherTexts); diff != "" {
					t.Errorf("round %d: errors of requests Client.Close ended whose text is not %q (-want +got):\n%s", round, clientClosedText, diff)
				}
			}
		})
	})
}
