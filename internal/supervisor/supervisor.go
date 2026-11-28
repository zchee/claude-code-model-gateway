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

// Package supervisor serves the public listener in front of the request worker.
package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/relay"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

const (
	// shutdownGrace bounds how long a cancelled Serve waits for in-flight requests before closing their connections.
	shutdownGrace = 5 * time.Second
)

// newLeaseHandler constructs the handler for an already bound public port that reaches workers only through leases
// from workers, never by a port number. The recipient of a relayed request is the generation a lease reached: a body
// taken by one generation is sent again only to a newer generation and only before response headers were relayed.
// lost is called once, with the generation that took the body, when a request is answered with the 503 that forbids
// a retry: when that generation is still current as the attempts run out, and also when the connection of the last
// attempt reached a generation and then failed, whether or not that generation is still current, as the source does.
func newLeaseHandler(publicPort int, workers workerlink.Source, token, version string, pid int, lost func(workerlink.Generation)) (http.Handler, error) {
	return NewLeaseRelay(publicPort, workers, token, version, pid, lost, nil)
}

// NewLeaseRelay constructs the handler for an already bound public port that reaches workers only through leases from
// workers, as newLeaseHandler describes; delivery is the relay package's. It answers the public routes itself: the
// Host check, the token check of /restart and /drain, and /restart.
//
// beforeDrain is called for an authenticated public POST /drain, once for each lease acquired for that request, after
// the lease is handed over and before any byte is written on it; nil means no pre-step. An error closes that lease
// unused and the attempt counts as refused: nothing was sent, so no generation took the request and no loss is
// reported. The foreground supervisor uses it to withdraw the generation's admission before the worker receives the
// drain.
func NewLeaseRelay(publicPort int, workers workerlink.Source, token, version string, pid int, lost func(workerlink.Generation), beforeDrain func(workerlink.Lease) error) (http.Handler, error) {
	if !validToken(token) {
		return nil, errors.New("model-gateway: the supervisor needs a valid local control token before serving")
	}
	if publicPort <= 0 || publicPort > 65535 || workers == nil {
		return nil, errors.New("model-gateway: the supervisor needs its public port and a worker lease source")
	}
	options := relay.Options{Workers: workers, Wait: waitAttempt, Version: version, SupervisorPID: pid}
	if lost != nil {
		options.Lost = func(l relay.Lost) { lost(l.Generation) }
	}
	ordinary, err := relay.New(options)
	if err != nil {
		return nil, err
	}
	drain := ordinary
	if beforeDrain != nil {
		options.Workers = drainSource{workers: workers, before: beforeDrain}
		if drain, err = relay.New(options); err != nil {
			return nil, err
		}
	}
	return &leaseHandler{publicPort: publicPort, token: token, relay: ordinary, drain: drain}, nil
}

// leaseHandler answers the public routes and hands every other request to the relay; an authenticated drain goes to
// a relay whose leases first pass the drain's pre-step.
type leaseHandler struct {
	publicPort int
	token      string
	relay      http.Handler
	drain      http.Handler
}

func (h *leaseHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pathOnly, ok := admitPublic(w, r, h.publicPort, h.token)
	if !ok {
		return
	}
	if pathOnly == "/drain" && r.Method == http.MethodPost {
		// One drain request retires at most one generation: its attempts share one record of the generation the
		// pre-step retired for it.
		h.drain.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), drainRequestKey{}, &drainRequest{})))
		return
	}
	h.relay.ServeHTTP(w, r)
}

// drainSource gives the drain relay its leases: each lease the underlying source hands over passes before first; a
// lease it refuses is closed unused and the refusal is the attempt's result.
//
// Once before has accepted a lease for a drain request, that request has retired that lease's generation, and no
// later attempt of the same request reaches any generation: each one is given a lease that stands for the retired
// generation and fails before a byte is written, so the relay never delivers the drain to a replacement and, when the
// retired generation ended without an answer, answers the request as one that generation took, with the 503 that
// forbids a resend.
//
// This breaks the workerlink.Source contract on purpose: the stand-in lease belongs to the generation the relay asked
// to exclude, which may have ended. It is a private delivery rule for one public drain request and holds only for the
// relay's current attempt loop (internal/supervisor/relay, deliver); drainSource is not a source to reuse elsewhere.
// The stand-in's failed reads and writes also replace the class of the error that ended the real attempt, so a loss
// journal that records an error type, or a stop-aware wait, must not be wired through it unchanged.
type drainSource struct {
	workers workerlink.Source
	before  func(workerlink.Lease) error
}

// drainRequestKey is the private context key of a drain request's drainRequest.
type drainRequestKey struct{}

// drainRequest records the generation the pre-step retired for one drain request.
type drainRequest struct {
	mu    sync.Mutex
	taken *takenLease
}

func (d drainSource) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	req, _ := ctx.Value(drainRequestKey{}).(*drainRequest)
	if req != nil {
		req.mu.Lock()
		taken := req.taken
		req.mu.Unlock()
		if taken != nil {
			return taken, nil
		}
	}
	lease, err := d.workers.Acquire(ctx, exclude)
	if err != nil || lease == nil {
		return lease, err
	}
	if err := d.before(lease); err != nil {
		_ = lease.Close()
		return nil, err
	}
	if req != nil {
		req.mu.Lock()
		req.taken = &takenLease{generation: lease.Generation(), pid: lease.PID(), authority: lease.Authority(), exited: lease.Exited(), local: lease.LocalAddr(), remote: lease.RemoteAddr()}
		req.mu.Unlock()
	}
	return lease, nil
}

// errDrainTaken is the failure of every attempt of a drain request after the one whose generation it retired.
var errDrainTaken = errors.New("model-gateway: this drain request already retired a worker generation and is not sent to another")

// takenLease stands for the generation a drain request retired. It connects to nothing: every read and write fails at
// once with errDrainTaken, so an attempt on it writes no byte anywhere.
type takenLease struct {
	generation    workerlink.Generation
	pid           int
	authority     string
	exited        <-chan struct{}
	local, remote net.Addr
}

func (l *takenLease) Read([]byte) (int, error)          { return 0, errDrainTaken }
func (l *takenLease) Write([]byte) (int, error)         { return 0, errDrainTaken }
func (l *takenLease) Close() error                      { return nil }
func (l *takenLease) LocalAddr() net.Addr               { return l.local }
func (l *takenLease) RemoteAddr() net.Addr              { return l.remote }
func (l *takenLease) SetDeadline(time.Time) error       { return nil }
func (l *takenLease) SetReadDeadline(time.Time) error   { return nil }
func (l *takenLease) SetWriteDeadline(time.Time) error  { return nil }
func (l *takenLease) Generation() workerlink.Generation { return l.generation }
func (l *takenLease) PID() int                          { return l.pid }
func (l *takenLease) Authority() string                 { return l.authority }
func (l *takenLease) Exited() <-chan struct{}           { return l.exited }

// waitAttempt is the relay's pause between two delivery attempts: a timer that ends early, with the context's error,
// when the request ends.
func waitAttempt(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for i := range len(token) {
		if b := token[i]; (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func requestTarget(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

func refuse(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
}

// admitPublic answers what the public listener decides before relay: a request whose Host is not this listener's, an
// unauthenticated /restart or /drain, and /restart, which needs the worker process owner. It returns the request's
// path and whether the request goes on to the relay.
func admitPublic(w http.ResponseWriter, r *http.Request, publicPort int, token string) (string, bool) {
	port := strconv.Itoa(publicPort)
	if !controlauth.LocalRequestAllowed(r, []string{"127.0.0.1:" + port, "localhost:" + port}) {
		refuse(w)
		return "", false
	}
	pathOnly, _, _ := strings.Cut(requestTarget(r), "?")
	if pathOnly == "/restart" || pathOnly == "/drain" {
		if !controlauth.Authenticated(r, token, []int{publicPort}, false) {
			refuse(w)
			return "", false
		}
	}
	if pathOnly == "/restart" {
		// Replacing the worker on request needs the worker process owner, which this handler does not hold.
		w.WriteHeader(http.StatusNotImplemented)
		return "", false
	}
	return pathOnly, true
}

// clientDeparted reports whether a body read failed because the client went away, as the source distinguishes it
// (lib/commands.js:2311-2323).
func clientDeparted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed)
}

func utf16Units(s string) []uint16 {
	units := make([]uint16, 0, len(s))
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			units = append(units, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			continue
		}
		units = append(units, uint16(r))
	}
	return units
}

// FixtureShutdown consumes and closes the inherited fixture channel. Without a test guard it closes it unread.
// It reports whether the channel's first message is the fixture's shutdown message. Only the first message is
// decided, as the source listens for one message only (lib/commands.js:2900-2903): after any other first message the
// channel stays open and later messages are read and dropped until the channel ends or fails, so none of them stops
// the supervisor. A failure of the channel other than its end returns false with that error, also when it comes while
// the rest is drained after a first message that is not the shutdown message; a shutdown message that arrives
// together with a failure still returns true. The first message is read line by line through a buffered reader, so a
// channel that returns bufio.ErrBufferFull itself before that message is complete is read again, as for a line longer
// than the buffer; every other channel error, and any error after the first message, is returned as described.
func FixtureShutdown(channel io.ReadCloser, guarded bool) (bool, error) {
	defer func() { _ = channel.Close() }()
	if !guarded {
		return false, nil
	}
	reader := bufio.NewReader(channel)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if fixtureStop(line) {
				return true, nil
			}
			// The rest is drained through the reader itself, so an error it already holds is returned
			// and the channel is not read again after it. Peek(1) waits for input and never makes up
			// bufio.ErrBufferFull, so no error value of the channel is mistaken for the reader's
			// buffer state; it returns the channel's own errors, or io.ErrNoProgress when the channel
			// keeps returning neither data nor an error.
			for err == nil {
				if _, err = reader.Peek(1); err == nil {
					_, _ = reader.Discard(reader.Buffered())
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// fixtureStop applies message?.type === 'fixture-shutdown' to one channel message (lib/commands.js:2900-2904).
func fixtureStop(line []byte) bool {
	message, err := jsonx.DecodeWire(line)
	if err != nil || message.Kind != '{' {
		return false
	}
	for _, m := range slices.Backward(message.Members) {
		if slices.Equal(m.Name, utf16Units("type")) {
			return m.Value.Kind == '"' && slices.Equal(m.Value.Text, utf16Units("fixture-shutdown"))
		}
	}
	return false
}
