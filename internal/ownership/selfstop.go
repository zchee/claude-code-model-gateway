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

package ownership

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
)

// stopSelfDial, when the package's own tests set it, opens every connection StopSelf makes, the stop connection and
// the health connection alike, instead of a loopback TCP dial. Nothing else sets it.
var stopSelfDial func(ctx context.Context, network, address string) (net.Conn, error)

// ErrUnproven means the listener did not prove that it holds this state root's control token. Only the identity
// challenge went over the control connection to it, and no credential was sent to it at all; the diagnostic health
// request StopSelf may send afterwards carries none either. Its error text is the refusal to print.
var ErrUnproven = errors.New("the listener could not be identified as this model-gateway install")

// ErrStopUnacknowledged means a proven supervisor did not acknowledge the stop. The stop is not sent again; the
// caller confirms the exit separately.
var ErrStopUnacknowledged = errors.New("the supervisor did not acknowledge the stop")

const (
	// stopSelfBudget bounds the whole exchange, every connection included, when the caller's context has no deadline.
	stopSelfBudget = 5 * time.Second
	// proofCap is the largest identity answer body read; a longer one is refused without reading it.
	proofCap = 1 << 20
	// headCap bounds the bytes read for one response's status line and header fields.
	headCap = 64 << 10
	// ackCap bounds the stop acknowledgment's body and healthCap the health document's.
	ackCap    = 4 << 10
	healthCap = 64 << 10
	// controlTokenHeader carries the control token; only the stop request, after a verified proof, sends it.
	controlTokenHeader = "X-Model-Gateway-Control-Token"
	// controlTokenLength is the token's spelling: 32 bytes in lowercase hexadecimal.
	controlTokenLength = 64
)

var (
	errInvalidToken  = errors.New("the control token is not 64 lowercase hexadecimal characters")
	errNotProof      = errors.New("the identity answer is not a valid proof")
	errTooLarge      = errors.New("the answer exceeds its size bound")
	errUnacknowledge = errors.New("the stop answer is not an acknowledgment")
)

// StopSelf asks the supervisor on the loopback port to stop itself. On one connection it sends a fresh identity
// challenge, verifies the proof against token, and only then sends the authenticated stop on that same connection.
// A nil error means the stop was acknowledged, not that the supervisor has exited. The whole exchange is bounded by
// the context, or by five seconds when the context has no deadline.
//
// When the listener is not proven, the error matches ErrUnproven and its text is the refusal to print. Only then,
// StopSelf sends one diagnostic GET /healthz on a separate connection; it carries neither the token nor a
// challenge, and healthPID is the supervisorPid that health document reports, or zero when none was reported. That
// number is a diagnostic only and never an authority to signal.
//
// Every connection is a direct TCP connection to 127.0.0.1, never through a proxy, never pooled or reused by
// another exchange. A redirect, a protocol switch or any other answer than a complete 200 proof that keeps the
// connection open is a refusal, and nothing is retried: in particular the stop is never sent twice. Each response is
// read through the connection's one buffered reader, its head and body within fixed bounds checked before an
// announced length is trusted. The challenge is 32 fresh bytes from crypto/rand for every call. The proof shows only
// that the listener holds this state root's token, not that it is the supervisor rather than another holder of the
// token. Once the budget is spent, no health request is sent.
//
// When the context ends during the exchange, the diagnostic included, the error also matches the context's error
// and, when the caller cancelled with a cause of its own, that cause: a refusal matches ErrUnproven, an
// unacknowledged stop ErrStopUnacknowledged, and a context that had already ended when StopSelf was called matches
// only the context's error and cause, since nothing was attempted. Every error's text is fixed words of this package:
// it never repeats the token, the challenge or the proof, never a byte the peer sent, and never the text of the
// caller's cause, which may hold anything.
func StopSelf(ctx context.Context, port uint16, token string) (healthPID int, err error) {
	if ended := observeEnd(ctx); ended != nil {
		return 0, &stopSelfError{text: "the stop was not attempted: the context had ended", errs: ended}
	}
	if port == 0 {
		return 0, fmt.Errorf("stop the supervisor on port 0: %w", fs.ErrInvalid)
	}
	if !canonicalToken(token) {
		return 0, errInvalidToken
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, stopSelfBudget)
		defer cancel()
	}
	x := &exchange{ctx: ctx, port: port, token: token, host: net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))}
	return x.run()
}

// canonicalToken reports whether token is spelled as the control token is stored.
func canonicalToken(token string) bool {
	if len(token) != controlTokenLength {
		return false
	}
	for i := range len(token) {
		if c := token[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// unprovenError is a refusal of a listener that did not prove itself. Its text is the source's refusal
// (lib/process-supervision.js:613); it matches ErrUnproven and, when the exchange was ended by the context, the
// context's error and the caller's cause.
type unprovenError struct {
	port  uint16
	pid   int
	ended []error
}

func (e *unprovenError) Error() string { return unidentifiedRefusal(e.port, e.pid) }

func (e *unprovenError) Unwrap() []error { return append([]error{ErrUnproven}, e.ended...) }

// stopSelfError is an error of StopSelf whose text is fixed by this package and whose chain holds errs: a sentinel
// and, when the context ended, the context's error and the caller's cause.
type stopSelfError struct {
	text string
	errs []error
}

func (e *stopSelfError) Error() string { return e.text }

func (e *stopSelfError) Unwrap() []error { return e.errs }

// observeEnd reads ctx once: nil while it is live; once it has ended, its error and its cause, which is the caller's
// own when it cancelled with one and otherwise the same error again. Both are kept, so that a cause that itself wraps
// the context's error is still matched as itself. Neither is ever nil, since an ended context has both. A result is
// finalized on exactly one such observation, so its chain never mixes two moments.
func observeEnd(ctx context.Context) []error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	return []error{err, context.Cause(ctx)}
}

// unidentifiedRefusal is the source's refusal for a holder that could not be identified as this install.
func unidentifiedRefusal(port uint16, pid int) string {
	observed := "unknown"
	if pid != 0 {
		observed = strconv.Itoa(pid)
	}
	return fmt.Sprintf("could not confirm the owner of :%d (last observed PID %s); the owning process could not be identified as this model-gateway install; left the listener untouched", port, observed)
}

// exchange is one StopSelf call; ctx carries its whole budget.
type exchange struct {
	ctx   context.Context
	port  uint16
	token string
	host  string
}

// allowance is a connection's reader with a byte allowance. Each response sets the allowance for its head before
// reading it and adds the allowance for its body once the head is parsed, so a peer that never stops sending is cut
// off at a fixed bound whatever it announces.
type allowance struct {
	r    io.Reader
	left int64
}

func (a *allowance) Read(p []byte) (int, error) {
	if a.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > a.left {
		p = p[:a.left]
	}
	n, err := a.r.Read(p)
	a.left -= int64(n)
	return n, err
}

// framed is the wire allowance for a body of at most limit decoded bytes. The decoded bytes are capped by the body's
// own reader; this bound is for the framing around them, kept separately: five bytes per decoded byte, which is what
// one-byte chunks cost (size line and closing line), and 64 KiB for the terminal chunk and trailer fields. A valid body
// at the cap therefore passes in chunks of any size written plainly; chunk extensions or size lines padded with
// leading zeros cost more and can exceed this allowance below the cap, and framing that never ends is cut off.
func framed(limit int64) int64 { return limit + 5*limit + 64<<10 }

func (x *exchange) run() (int, error) {
	dialed, proven, err := x.control()
	switch {
	case !dialed:
		return 0, &unprovenError{port: x.port, ended: observeEnd(x.ctx)}
	case !proven:
		pid := x.healthPID()
		// The refusal is final only now, after the diagnostic, whatever ended either step; one observation of the
		// context decides what the error also matches.
		return pid, &unprovenError{port: x.port, pid: pid, ended: observeEnd(x.ctx)}
	case err == nil:
		return 0, nil
	}
	// The peer has just received the token and could echo it in whatever it answered, so nothing it sent is quoted.
	if ended := observeEnd(x.ctx); ended != nil {
		return 0, &stopSelfError{text: "the supervisor did not acknowledge the stop: the context ended while the acknowledgment was awaited", errs: append([]error{ErrStopUnacknowledged}, ended...)}
	}
	return 0, fmt.Errorf("%w: %w", ErrStopUnacknowledged, errUnacknowledge)
}

// control runs the exchange on the control connection: the challenge and, when the proof holds, the one stop. It
// reports whether the connection was opened and whether the listener proved itself; err is the refusal or the
// unacknowledged stop. The connection's interruption is released and then the connection closed by deferred calls,
// so both happen on every exit, a panic from the connection's Read or Write included, and both have happened before
// StopSelf goes on to the diagnostic: no later end of the context can act on this connection.
func (x *exchange) control() (dialed, proven bool, err error) {
	conn, err := x.dial()
	if err != nil {
		return false, false, err
	}
	defer func() { _ = conn.Close() }()
	release := x.bind(conn)
	defer release()
	allow := &allowance{r: conn}
	br := bufio.NewReader(allow)
	if err := x.prove(conn, allow, br); err != nil {
		return true, false, err
	}
	return true, true, x.requestStop(conn, allow, br)
}

func (x *exchange) dial() (net.Conn, error) {
	if stopSelfDial != nil {
		return stopSelfDial(x.ctx, "tcp4", x.host)
	}
	var d net.Dialer
	return d.DialContext(x.ctx, "tcp4", x.host)
}

// bind bounds every operation on conn by the exchange's context: when the context ends, whatever read or write is
// pending is interrupted.
//
// No deadline is taken from ctx.Deadline(): the context's own completion, its deadline included, interrupts the I/O,
// so an operation that ends because of the context ends only after the context reports its end, and its errors are
// then simply read. The interruption runs in a goroutine of its own; release stops it or, when it has already
// started, waits for it to finish, so nothing of it outlives the exchange. release may be called more than once.
func (x *exchange) bind(conn net.Conn) (release func()) {
	interrupted := make(chan struct{})
	stop := context.AfterFunc(x.ctx, func() {
		defer close(interrupted)
		_ = conn.SetDeadline(time.Unix(1, 0))
	})
	// Once only: a second call after the first stopped the callback before it ran would wait for a close that never
	// comes.
	return sync.OnceFunc(func() {
		if !stop() {
			<-interrupted
		}
	})
}

// expired reports whether the exchange's budget is spent, which is when its context has ended.
func (x *exchange) expired() bool { return x.ctx.Err() != nil }

// prove sends the identity challenge and verifies the answer. Nothing else is written to the connection before the
// answer has been read in full and verified.
func (x *exchange) prove(conn net.Conn, allow *allowance, br *bufio.Reader) error {
	var challenge [controlauth.IdentityNonceBytes]byte
	_, _ = rand.Read(challenge[:])
	nonce := hex.EncodeToString(challenge[:])
	request := "GET /internal/identity HTTP/1.1\r\nHost: " + x.host + "\r\n" + controlauth.IdentityNonceHeader + ": " + nonce + "\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return err
	}
	// The stop must follow on this connection, so an answer that closes it is no proof.
	body, err := readAnswer(allow, br, http.MethodGet, http.StatusOK, true, proofCap)
	if err != nil {
		return err
	}
	var answer struct {
		Generation string `json:"generation"`
		Proof      string `json:"proof"`
	}
	if err := json.Unmarshal(body, &answer, json.RejectUnknownMembers(true)); err != nil {
		return errNotProof
	}
	if answer.Generation == "" || !controlauth.VerifySupervisorIdentity(x.token, nonce, answer.Generation, answer.Proof) {
		return errNotProof
	}
	// Bytes after the proof that nobody asked for would be read as the stop's answer, so a peer that sent any gets no
	// stop.
	if br.Buffered() > 0 {
		return errNotProof
	}
	return nil
}

// requestStop sends the authenticated stop on the proven connection, once, and reads its acknowledgment.
func (x *exchange) requestStop(conn net.Conn, allow *allowance, br *bufio.Reader) error {
	if err := x.ctx.Err(); err != nil {
		return err
	}
	request := "POST /internal/stop HTTP/1.1\r\nHost: " + x.host + "\r\n" + controlTokenHeader + ": " + x.token + "\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
	if _, err := io.WriteString(conn, request); err != nil {
		return err
	}
	body, err := readAnswer(allow, br, http.MethodPost, http.StatusAccepted, false, ackCap)
	if err != nil {
		return err
	}
	var ack struct {
		OK       bool `json:"ok"`
		Stopping bool `json:"stopping"`
	}
	if err := json.Unmarshal(body, &ack); err != nil || !ack.OK || !ack.Stopping {
		return errUnacknowledge
	}
	return nil
}

// readAnswer reads one response from br: a head within headCap, then, when its status is want (and the connection
// stays open, when keepOpen requires it), a body of at most limit bytes. An announced length over the limit is
// refused before any of the body is read, and a chunked body is refused as soon as it passes the limit, without
// waiting for its end.
func readAnswer(allow *allowance, br *bufio.Reader, method string, want int, keepOpen bool, limit int64) ([]byte, error) {
	// Bytes already buffered count against this head as well, so a peer cannot stretch it by sending early.
	allow.left = max(headCap-int64(br.Buffered()), 0)
	resp, err := http.ReadResponse(br, &http.Request{Method: method})
	if err != nil {
		return nil, err
	}
	defer closeBody(allow, resp)
	if resp.StatusCode != want {
		return nil, fmt.Errorf("answer status %d", resp.StatusCode)
	}
	if keepOpen && resp.Close {
		return nil, errors.New("the answer closes the connection")
	}
	allow.left += framed(limit)
	return readBody(resp.Body, resp.ContentLength, limit)
}

// closeBody closes a response's body without reading the rest of an unfinished one: closing reads a body to its end,
// so its connection's allowance is spent first, and the close stops at once instead of waiting for bytes a refused
// peer holds back. The connection itself is closed by its owner.
func closeBody(allow *allowance, resp *http.Response) {
	allow.left = 0
	_ = resp.Body.Close()
}

// readBody reads a body of the announced length, or of unknown length up to limit bytes.
func readBody(r io.Reader, length, limit int64) ([]byte, error) {
	if length > limit {
		return nil, errTooLarge
	}
	if length >= 0 {
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		return body, nil
	}
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errTooLarge
	}
	return body, nil
}

// healthPID asks the refused listener's /healthz, on a connection of its own that carries no credential and no
// challenge, for the process number it reports. Any failure is zero: the number is a diagnostic only. Nothing the
// peer sent is ever returned; whether the context ended is observed once by the caller when the refusal is final.
func (x *exchange) healthPID() int {
	if x.expired() {
		return 0
	}
	conn, err := x.dial()
	if err != nil {
		return 0
	}
	defer func() { _ = conn.Close() }()
	release := x.bind(conn)
	defer release()
	if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: "+x.host+"\r\nConnection: close\r\n\r\n"); err != nil {
		return 0
	}
	allow := &allowance{r: conn, left: headCap}
	resp, err := http.ReadResponse(bufio.NewReader(allow), &http.Request{Method: http.MethodGet})
	if err != nil {
		return 0
	}
	defer closeBody(allow, resp)
	allow.left += framed(healthCap)
	body, err := readBody(resp.Body, resp.ContentLength, healthCap)
	if err != nil {
		return 0
	}
	var doc struct {
		SupervisorPID int64 `json:"supervisorPid"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.SupervisorPID <= 0 || doc.SupervisorPID > math.MaxInt32 {
		return 0
	}
	return int(doc.SupervisorPID)
}
