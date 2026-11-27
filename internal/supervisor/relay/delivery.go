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

package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

const (
	// attemptWaits and attemptPause are the source's delivery schedule: a first attempt and at most eighty more, each
	// 50 ms after the previous one (lib/commands.js:2360-2362,2389).
	attemptWaits = 80
	attemptPause = 50 * time.Millisecond
	// maxInterim is the number of interim heads passed over before a worker's final head; the next one fails the
	// attempt, so that a worker that never sends its final head cannot hold the request for ever.
	maxInterim = 5
	// maxHeadBytes bounds each response head of the worker, interim or final, so that a worker cannot make the relay
	// buffer an unbounded head: the bytes of its status line and field lines with their line ends, the empty line that
	// ends the head not included. The value is the default head limit of Node's HTTP client; the way the bytes are
	// counted is this port's own rule, not a copy of Node's.
	maxHeadBytes = 16 << 10
	// writeFailureGrace bounds how long the relay still waits for a final head once the request write has failed
	// while the head is being read: bytes the worker already sent can still arrive, but a worker left waiting for
	// request bytes that will never come must not hold the attempt. The grace applies before the head only: a write
	// that fails after the final head was settled does not bound the committed body, which has no timeout.
	writeFailureGrace = time.Second
)

var (
	errTooManyInterim = errors.New("relay: the worker sent too many interim responses")
	errProtocolSwitch = errors.New("relay: the worker switched protocols on a relayed request")
	errInvalidStatus  = errors.New("relay: the worker answered with a status below 100")
	errHeadTooLarge   = errors.New("relay: the worker's response head exceeds its bound")
	errExpired        = errors.New("relay: the grace for the worker's head after a failed request write ended")
)

// taker is the generation that a delivery attempt reached: it may have read the request and spent it upstream.
type taker struct {
	generation workerlink.Generation
	pid        int
	exited     <-chan struct{}
	// errorType is the class of the failure that ended the attempt.
	errorType string
}

// current reports whether the generation is still the source's current worker: alive, also while it drains, until it
// exits or is finally stopped (lib/commands.js:2625,2651).
func (t *taker) current() bool {
	select {
	case <-t.exited:
		return false
	default:
		return true
	}
}

// failure is how the attempts ended without a final response head.
type failure struct {
	// departed is set when the client left; it is owed nothing.
	departed bool
	// taker is the generation that holds the request, or nil when the failure is retryable.
	taker *taker
}

// exchange is a worker leg whose final response head was read. The caller relays resp.Body and then calls finish. The
// request writer may still be sending: it belongs to the exchange until finish has joined it.
type exchange struct {
	leg  *leg
	resp *http.Response
	// wrote is closed when the request writer has ended.
	wrote      <-chan struct{}
	finishOnce sync.Once
}

// finish ends the exchange. Invariant: the leg is closed before the writer is joined, so that a write the worker never
// reads ends instead of holding the handler. It may be called any number of times.
func (ex *exchange) finish() {
	ex.finishOnce.Do(func() {
		ex.leg.release()
		<-ex.wrote
	})
}

// waitResult is the outcome of one wait between two attempts.
type waitResult int

const (
	// resumed means the wait ran its course.
	resumed waitResult = iota
	// left means the client departed; no attempt follows.
	left
	// stopped means the supervisor is stopping; the attempts end at once.
	stopped
)

// pause waits between two attempts. Options.Wait returns the context's error when the client departs and any other
// error when the supervisor stops; the context is consulted after every wait, so that a departure is never taken for
// a stop or overlooked.
func (h *handler) pause(ctx context.Context) waitResult {
	err := h.wait(ctx, attemptPause)
	switch {
	case ctx.Err() != nil:
		return left
	case err != nil:
		return stopped
	}
	return resumed
}

// deliver runs the source's delivery attempts (lib/commands.js:2345-2391) with a generation lease as the identity of
// the worker that took the request. A lease is a delivery: it is latched before the request is written, and no
// generation is sent the request twice, because a worker that took it may have spent it upstream.
//
// Each of the attempts has one of three forms:
//   - The generation that took the request is still current: the request stays with it, and the relay waits without
//     asking for a lease. When the attempts run out, or a stop arrives, the request is lost to that generation.
//   - No generation holds it, or the one that took it has ended: the relay asks for a lease on any other generation.
//     A refusal forgets an earlier acceptance, so that exhaustion is then retryable.
//   - A lease was handed over: the request is written and the final head read. A failure makes its generation the
//     taker; when it happens on the last attempt, or a stop arrives during the wait after it, the request is lost to
//     that generation even if it has ended by then, as the source decides at the moment of the failure
//     (lib/commands.js:2384).
//
// A departure ends the attempts at once, whatever the form, and is owed nothing.
func (h *handler) deliver(ctx context.Context, r *http.Request, target string, body []byte) (*exchange, failure) {
	var held *taker
	for remaining := attemptWaits; ; remaining-- {
		if held != nil && held.current() {
			if remaining == 0 {
				return nil, failure{taker: held}
			}
			switch h.pause(ctx) {
			case left:
				return nil, failure{departed: true}
			case stopped:
				// The source decides a stop at the next attempt: the request stays lost to a generation still current
				// and is retryable once that generation has ended (lib/commands.js:2363-2370).
				if held.current() {
					return nil, failure{taker: held}
				}
				return nil, failure{}
			}
			continue
		}
		var exclude workerlink.Generation
		if held != nil {
			exclude = held.generation
		}
		lease, err := h.workers.Acquire(ctx, exclude)
		switch {
		case err != nil && lease != nil:
			// A lease that comes with an error is not used, but it is still a connection to close.
			_ = lease.Close()
			lease = nil
		case err == nil && lease == nil:
			err = workerlink.ErrUnavailable
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, failure{departed: true}
			}
			held = nil
			if remaining == 0 {
				return nil, failure{}
			}
			switch h.pause(ctx) {
			case left:
				return nil, failure{departed: true}
			case stopped:
				return nil, failure{}
			}
			continue
		}
		ex, t, err := attempt(ctx, lease, r, target, body)
		if err == nil {
			return ex, failure{}
		}
		if ctx.Err() != nil {
			return nil, failure{departed: true}
		}
		if remaining == 0 {
			return nil, failure{taker: t}
		}
		switch h.pause(ctx) {
		case left:
			return nil, failure{departed: true}
		case stopped:
			return nil, failure{taker: t}
		}
		held = t
	}
}

// attempt runs one exchange on a lease just handed over. The leg's context watcher is installed before anything else
// touches the lease, and a deferred release covers the lease until the exchange is committed, also when something
// panics in between. The taker is returned for every exchange that failed: the lease was a delivery to it.
func attempt(ctx context.Context, lease workerlink.Lease, r *http.Request, target string, body []byte) (ex *exchange, t *taker, err error) {
	l := watchLeg(ctx, lease)
	defer func() {
		if ex == nil {
			l.release()
		}
	}()
	t = &taker{generation: lease.Generation(), pid: lease.PID(), exited: lease.Exited()}
	if err := ctx.Err(); err != nil {
		return nil, t, err
	}
	ex, err = exchangeOn(ctx, l, r, target, body)
	if err != nil {
		t.errorType = errorType(err)
	}
	return ex, t, err
}

// exchangeOn writes the request on the leg and reads the worker's final response head at the same time, because a
// worker may answer before it has read the body (a drain, a health probe, a refusal), or answer first and read the
// rest of the request afterwards.
//
// Invariant: a parsed final head wins over a failed or unfinished request write. The write is never stopped at the
// head: the writer passes to the committed exchange and keeps sending while the answer is relayed, and finish closes
// the leg and then joins it. A complete answer wins even when the request was only partly written; that never shows
// that the worker processed the whole request, and the request is never sent again.
//
// A request write that fails while the head is still being read, other than by the end of the request context, gives
// the head writeFailureGrace more to arrive. The grace is a timer that closes the leg, not a deadline: a lease is any
// connection the registry's dial returns, which need not accept deadlines, so no deadline setter is used on the leg
// at all. The grace's end and the acceptance of a final head are one decision (see requestWriter.decide): either the
// head was accepted, and the timer never touches the leg, or the grace expired, and no exchange is returned for the
// closed leg; the attempt then fails as a connection that took the request, classified by the write's error. A failed
// read stops the timer, closes the leg and joins the writer, also on a panic.
func exchangeOn(ctx context.Context, l *leg, r *http.Request, target string, body []byte) (ex *exchange, err error) {
	w := &requestWriter{leg: l, done: make(chan struct{})}
	go w.run(ctx, r, target, body)
	defer func() {
		if ex == nil {
			_ = w.settle()
			l.release()
			<-w.done
		}
	}()
	resp, err := readFinalHead(bufio.NewReader(l.lease), r.Method, w) //nolint:bodyclose // Closing the body would read an unfinished answer to its end; finish closes the leg instead.
	writeErr := w.settle()
	if err != nil {
		if writeErr != nil {
			return nil, writeErr
		}
		return nil, err
	}
	return &exchange{leg: l, resp: resp, wrote: w.done}, nil
}

// headState is the head's side of an exchange. It leaves pending exactly once, under requestWriter.mu.
type headState int

const (
	// pending means no final head has been accepted and the grace, if one runs, has not ended.
	pending headState = iota
	// accepted means a final head was parsed and accepted; the grace timer never closes the leg after it.
	accepted
	// expired means the grace ended first and its timer closes the leg; no head is accepted after it.
	expired
	// abandoned means the head's read ended without a final head for another reason; the timer does nothing after it.
	abandoned
)

// requestWriter writes the request on the leg in its own goroutine and keeps the head's state, which decides between
// the acceptance of a final head and the end of the grace after a write failure.
type requestWriter struct {
	leg  *leg
	done chan struct{}

	mu    sync.Mutex
	state headState
	// err is the write failure recorded while the head was pending; timer is its grace timer, and timerDone is closed
	// when the timer's function has run.
	err       error
	timer     *time.Timer
	timerDone chan struct{}

	settleOnce sync.Once
	settledErr error
}

// run writes the request and closes done. A failure that is not the request context's, while the head is pending,
// starts the grace timer. When it fires, it moves a pending head to expired under the mutex and then, outside it,
// closes the leg; when the head is no longer pending it does nothing.
func (w *requestWriter) run(ctx context.Context, r *http.Request, target string, body []byte) {
	defer close(w.done)
	err := writeRequest(w.leg.lease, r, target, w.leg.lease.Authority(), body)
	if err == nil || ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != pending {
		return
	}
	timerDone := make(chan struct{})
	w.err, w.timerDone = err, timerDone
	w.timer = time.AfterFunc(writeFailureGrace, func() {
		defer close(timerDone)
		w.mu.Lock()
		expire := w.state == pending
		if expire {
			w.state = expired
		}
		w.mu.Unlock()
		if expire {
			w.leg.closeLease()
		}
	})
}

// decide parses one raw head from memory and, when it is a final head, accepts it, in one critical section with the
// timer's expiry, so that a head is accepted only while the grace is pending and the grace expires only while no head
// is accepted. An interim head leaves the state pending. Nothing in it reads or closes a connection.
func (w *requestWriter) decide(head []byte, req *http.Request) (*http.Response, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == expired {
		return nil, errExpired
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(head)), req)
	if err != nil {
		return nil, err
	}
	// This body reads only the head's own bytes, so closing it never touches the leg.
	_ = resp.Body.Close()
	if resp.StatusCode >= http.StatusOK {
		w.state = accepted
	}
	return resp, nil
}

// settle ends the head's part in the exchange and returns the write failure recorded while the head was pending, or
// nil. A head that is still pending becomes abandoned, so that the timer does nothing. It then stops the grace timer
// and, when the timer had already fired, waits until its function has run, so that no timer function runs after the
// head's outcome is decided or after the handler has ended. Only the first call does this; later calls return the
// same result at once.
func (w *requestWriter) settle() error {
	w.settleOnce.Do(func() {
		w.mu.Lock()
		if w.state == pending {
			w.state = abandoned
		}
		timer, timerDone := w.timer, w.timerDone
		w.settledErr = w.err
		w.mu.Unlock()
		// The timer's function takes the mutex, so it is waited for outside it.
		if timer != nil && !timer.Stop() {
			<-timerDone
		}
	})
	return w.settledErr
}

// legExcluded are the client's fields that the worker leg writes itself or that describe only the client's
// connection.
var legExcluded = []string{"Host", "Content-Length", "Transfer-Encoding", "Trailer", "Connection", "Keep-Alive", "Expect"}

// writeRequest writes the buffered request on the worker leg (lib/commands.js:2373-2376): the client's method and
// target byte for byte, the lease's authority as Host, the client's other fields, the buffered length and
// Connection: close, because the leg carries this one request. POST, PUT and PATCH always state their length, an
// empty one included; a request of another method states it only when it has a body.
func writeRequest(conn io.Writer, r *http.Request, target, authority string, body []byte) error {
	header := r.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	// The map is matched by case-insensitive name, so that a key a direct caller did not canonicalize is dropped too.
	for key := range header {
		if slices.ContainsFunc(legExcluded, func(name string) bool { return strings.EqualFold(name, key) }) {
			delete(header, key)
		}
	}
	bw := bufio.NewWriter(conn)
	_, _ = fmt.Fprintf(bw, "%s %s HTTP/1.1\r\nHost: %s\r\n", r.Method, target, authority)
	_ = header.Write(bw)
	if len(body) > 0 || r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		_, _ = fmt.Fprintf(bw, "Content-Length: %d\r\n", len(body))
	}
	_, _ = bw.WriteString("Connection: close\r\n\r\n")
	_, _ = bw.Write(body)
	// A bufio.Writer keeps its first error, so Flush reports any failed write above.
	return bw.Flush()
}

// readFinalHead reads response heads until the final one. Up to maxInterim interim heads are passed over; one more, a
// protocol switch or a status below 100 fails the attempt. Each head is read within maxHeadBytes, outside any lock,
// and then parsed from memory and, when final, accepted by gate.decide in one critical section with the grace's
// expiry; a grace that expired first fails the attempt with errExpired. The accepted final head is parsed again over
// the bytes that follow it, so that the body, which is not bounded, is read from exactly where the head ended; that
// parse reads only the head's bytes from memory, and the leg is read only by the body's reads. The body of a failed
// head is never read: the caller closes the leg instead.
func readFinalHead(br *bufio.Reader, method string, gate *requestWriter) (*http.Response, error) {
	req := &http.Request{Method: method}
	for interim := 0; ; interim++ {
		head, err := readHead(br)
		if err != nil {
			return nil, err
		}
		resp, err := gate.decide(head, req)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode >= http.StatusOK:
			return http.ReadResponse(bufio.NewReader(io.MultiReader(bytes.NewReader(head), br)), req)
		case resp.StatusCode < http.StatusContinue:
			return nil, errInvalidStatus
		case resp.StatusCode == http.StatusSwitchingProtocols:
			return nil, errProtocolSwitch
		case interim == maxInterim:
			return nil, errTooManyInterim
		}
	}
}

// readHead returns the raw bytes of one response head, its terminating empty line included, and leaves br at the
// first byte after it. The status line and field lines may hold maxHeadBytes together, counted with their line ends.
// The excess is refused as soon as it is certain, without waiting for a line end that may never come: an unfinished
// line that cannot be the head's empty line still needs its line end, so it fails once the counted bytes, its bytes so
// far and that line end exceed the bound. At most one buffer of the reader is read past the bound. A stream that ends
// inside the head fails with io.ErrUnexpectedEOF, or with io.EOF when it ended before the head began.
func readHead(br *bufio.Reader) ([]byte, error) {
	var head []byte
	// counted is the size of the complete lines so far; line is where the current line starts in head.
	counted, line := 0, 0
	for {
		if _, err := br.Peek(1); err != nil {
			if errors.Is(err, io.EOF) {
				if len(head) == 0 {
					return nil, io.EOF
				}
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		data, _ := br.Peek(br.Buffered())
		take, ended := 0, false
		for take < len(data) {
			i := bytes.IndexByte(data[take:], '\n')
			if i < 0 {
				head = append(head, data[take:]...)
				take = len(data)
				break
			}
			head = append(head, data[take:take+i+1]...)
			take += i + 1
			if blankLine(head[line:]) {
				ended = true
				break
			}
			counted += len(head) - line
			line = len(head)
			if counted > maxHeadBytes {
				return nil, errHeadTooLarge
			}
		}
		// Only the head's own bytes are consumed; what follows it stays in br for the body.
		_, _ = br.Discard(take)
		if ended {
			return head, nil
		}
		if partial := head[line:]; len(partial) > 0 && string(partial) != "\r" && counted+len(partial)+1 > maxHeadBytes {
			return nil, errHeadTooLarge
		}
	}
}

// blankLine reports whether line is the empty line that ends a head: CRLF, or a bare LF as Go's reader accepts it.
func blankLine(line []byte) bool {
	return string(line) == "\r\n" || string(line) == "\n"
}

// errorType returns the class of a worker-leg failure for a Lost report: one of a fixed set of words, never the
// failure's text, an address or a port.
func errorType(err error) string {
	switch {
	case errors.Is(err, errTooManyInterim):
		return "interim-limit"
	case errors.Is(err, errProtocolSwitch):
		return "protocol-switch"
	case errors.Is(err, errInvalidStatus):
		return "invalid-status"
	case errors.Is(err, errHeadTooLarge):
		return "head-too-large"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected-eof"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection-reset"
	case errors.Is(err, syscall.EPIPE):
		return "broken-pipe"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		return "connection-closed"
	}
	return "worker-leg-failure"
}

// leg is one lease on the worker leg. It closes the lease once: when the exchange is over, or as soon as the request's
// context ends, so that a read or write on the leg never stays parked for a departed client. Closing the leg ends the
// worker's own request context for this request; keeping the leg open after a client's half-close, so that the
// provider answer can complete, is a policy of the public listener's dispatcher, not of the relay.
type leg struct {
	lease       workerlink.Lease
	closeOnce   sync.Once
	releaseOnce sync.Once
	// stop deregisters the close on the context's end; watched is closed when that close, once started, has ended.
	stop    func() bool
	watched chan struct{}
}

// watchLeg returns the leg of lease, closed when ctx ends.
func watchLeg(ctx context.Context, lease workerlink.Lease) *leg {
	l := &leg{lease: lease, watched: make(chan struct{})}
	l.stop = context.AfterFunc(ctx, func() {
		defer close(l.watched)
		l.closeLease()
	})
	return l
}

func (l *leg) closeLease() {
	l.closeOnce.Do(func() { _ = l.lease.Close() })
}

// release closes the lease. It returns only after the close on the context's end has finished, if it had started, so
// that no goroutine of the request outlives the handler. It may be called any number of times; only the first call
// does anything, so a second call never waits for a watcher that was never started.
func (l *leg) release() {
	l.releaseOnce.Do(func() {
		if !l.stop() {
			<-l.watched
		}
		l.closeLease()
	})
}
