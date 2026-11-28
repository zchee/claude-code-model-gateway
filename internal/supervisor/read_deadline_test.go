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

package supervisor_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/relay"
)

// These tests decide that a handler served by supervisor.Serve can bound the read of its request body with
// http.ResponseController's SetReadDeadline, that the stop route of the control dispatcher does so, and that the
// relay's refusal of an oversized upload, which reads on within a grace bounded by such a deadline, works through
// the public listener. The stalled-body rows and one stalled upload run on real loopback sockets, so their times are
// real: a handler's deadline is rdDeadline from the moment it is set, and every wait for an event is bounded by
// rdBound, generous for a loaded host. The rows about a complete body, the exact moment a stalled trailer's read ends,
// and the oversize rows that decide a moment or a completion within the grace run on a staged connection whose client
// stream is present before the server reads it, in a synctest bubble, so no scheduling window decides them.

const (
	// rdDeadline is how far ahead a handler sets its read deadline.
	rdDeadline = 300 * time.Millisecond
	// rdBound bounds every wait for an event of the server; it only turns a missing deadline into a failure.
	rdBound = 10 * time.Second
	// rdWatch is how long after its deadline a handler whose body was complete keeps watching its request context. A
	// disturbance shows within it; its absence within it proves nothing by itself, which is why the next request on
	// the same connection must also be served.
	rdWatch = 300 * time.Millisecond
	// rdTimeout is the class of a body read that ended at a deadline.
	rdTimeout = "timeout"
	// rdSecond is the later request sent on the same connection.
	rdSecond = "GET /second HTTP/1.1\r\nHost: read-deadline.test\r\n\r\n"
)

// rdClass classifies the end of a body read. A timeout is an error that is, or wraps, os.ErrDeadlineExceeded or an
// error whose Timeout method reports true.
func rdClass(err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case err == nil:
		return "complete"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return rdTimeout
	}
	return "other error: " + err.Error()
}

// rdServer serves the test's first-request handler at /first and counts every other request it answers.
type rdServer struct {
	later atomic.Int32
	stop  func()
}

// rdServeOn serves ln until stop or cleanup; the join is bounded by rdBound.
func rdServeOn(t *testing.T, ln net.Listener, first http.HandlerFunc) *rdServer {
	t.Helper()
	s := &rdServer{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			first(w, r)
			return
		}
		s.later.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "6")
		_, _ = io.WriteString(w, "second")
	})
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- supervisor.Serve(ctx, ln, handler) }()
	s.stop = sync.OnceFunc(func() {
		cancel()
		timer := time.NewTimer(rdBound)
		defer timer.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("public listener: %v", err)
			}
		case <-timer.C:
			t.Errorf("the public listener did not return within %v of its cancellation", rdBound)
		}
		_ = ln.Close()
	})
	t.Cleanup(s.stop)
	return s
}

// rdServe serves a loopback listener and returns its address.
func rdServe(t *testing.T, first http.HandlerFunc) (string, *rdServer) {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln.Addr().String(), rdServeOn(t, ln, first)
}

func rdDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	// Registered after the server's cleanup, so it runs first: a handler still blocked on a body read whose
	// deadline never came is let go before the server waits for it.
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// rdAnswer is one response the client read.
type rdAnswer struct {
	Status int
	Close  bool
	Body   string
}

// rdParse returns the complete answers in raw and the bytes after them.
func rdParse(raw []byte) ([]rdAnswer, []byte) {
	var answers []rdAnswer
	r := bytes.NewReader(raw)
	br := bufio.NewReader(r)
	for {
		rest := raw[len(raw)-r.Len()-br.Buffered():]
		if len(rest) == 0 {
			return answers, nil
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			return answers, rest
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return answers, rest
		}
		answers = append(answers, rdAnswer{Status: resp.StatusCode, Close: resp.Close, Body: string(body)})
	}
}

// rdRead reads until the connection ends or bound passes, and returns the answers in what was read, any bytes after
// them, and whether the connection ended within the bound.
func rdRead(conn net.Conn, bound time.Duration) ([]rdAnswer, string, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(bound))
	raw, err := io.ReadAll(conn)
	answers, rest := rdParse(raw)
	return answers, string(rest), !errors.Is(err, os.ErrDeadlineExceeded)
}

// rdObservation is what the handler of the first request saw.
type rdObservation struct {
	// SetErr is the error of SetReadDeadline, empty when it was accepted.
	SetErr string
	// Read is the class of the body read's end, and Body what it read when it completed.
	Read string
	Body string
	// EarlyTimeout records a read that ended in a timeout before the deadline the handler installed.
	EarlyTimeout bool
	// ContextEnded records that the request context had ended when the handler sampled it.
	ContextEnded bool
}

func rdAwait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(rdBound)
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		t.Fatalf("%s did not happen within %v", what, rdBound)
	}
	var zero T
	return zero
}

// rdStalls are request beginnings that stall, each with the rest of the request and the body the whole request
// carries.
var rdStalls = map[string]struct{ sent, rest, body string }{
	"a body framed by Content-Length after its first bytes": {
		sent: "POST /first HTTP/1.1\r\nHost: read-deadline.test\r\nContent-Length: 10\r\n\r\nabc", rest: "defghij", body: "abcdefghij",
	},
	"a body framed by Content-Length before its first byte": {
		sent: "POST /first HTTP/1.1\r\nHost: read-deadline.test\r\nContent-Length: 10\r\n\r\n", rest: "abcdefghij", body: "abcdefghij",
	},
	"a chunked body inside its chunk data": {
		sent: "POST /first HTTP/1.1\r\nHost: read-deadline.test\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nab", rest: "cde\r\n0\r\n\r\n", body: "abcde",
	},
	"a chunked body inside a chunk size line": {
		sent: "POST /first HTTP/1.1\r\nHost: read-deadline.test\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n1", rest: "\r\nx\r\n0\r\n\r\n", body: "{}x",
	},
	"a chunked body inside its trailer section": {
		sent: "POST /first HTTP/1.1\r\nHost: read-deadline.test\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\nX-Trailer: v\r\n", rest: "\r\n", body: "{}",
	},
}

// TestHandlerReadDeadlineEndsAStalledBody sends a request whose body stalls and lets the handler bound the body read
// with a read deadline: the read must end with a timeout no earlier than that deadline, wherever in the body's framing
// the client stalled, and the request's context must still be live then. The handler is held after it saw the
// timeout; meanwhile the client sends the rest of the body and a later request. Once the handler is let go the
// connection must end, after the handler's answer when it gives one, and the later request is never dispatched.
func TestHandlerReadDeadlineEndsAStalledBody(t *testing.T) {
	type stalled struct {
		Observed rdObservation
		// LateWriteErr is the error of the client's write of the rest and the later request while the handler was held.
		LateWriteErr string
		Answers      []rdAnswer
		Unparsed     string
		Ended        bool
		LaterServed  int32
	}
	timedOut := rdObservation{Read: rdTimeout}
	tests := map[string]struct {
		// stall names the request in rdStalls; answer makes the handler answer after the timeout.
		stall  string
		answer bool
		want   stalled
	}{
		"error: a body framed by Content-Length after its first bytes ends at the handler's read deadline": {
			stall: "a body framed by Content-Length after its first bytes", want: stalled{Observed: timedOut, Ended: true},
		},
		"error: a body framed by Content-Length before its first byte ends at the handler's read deadline": {
			stall: "a body framed by Content-Length before its first byte", want: stalled{Observed: timedOut, Ended: true},
		},
		"error: a chunked body inside its chunk data ends at the handler's read deadline": {
			stall: "a chunked body inside its chunk data", want: stalled{Observed: timedOut, Ended: true},
		},
		"error: a chunked body inside a chunk size line ends at the handler's read deadline": {
			stall: "a chunked body inside a chunk size line", want: stalled{Observed: timedOut, Ended: true},
		},
		"error: a chunked body inside its trailer section ends at the handler's read deadline": {
			stall: "a chunked body inside its trailer section", want: stalled{Observed: timedOut, Ended: true},
		},
		"error: a handler that answers after the timeout ends the connection after its answer": {
			stall: "a body framed by Content-Length after its first bytes", answer: true,
			want: stalled{Observed: timedOut, Answers: []rdAnswer{{Status: http.StatusRequestTimeout, Close: true, Body: "late"}}, Ended: true},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			stall := rdStalls[tc.stall]
			observed := make(chan rdObservation, 1)
			held := make(chan struct{})
			release := sync.OnceFunc(func() { close(held) })
			addr, s := rdServe(t, func(w http.ResponseWriter, r *http.Request) {
				d := time.Now().Add(rdDeadline)
				if err := http.NewResponseController(w).SetReadDeadline(d); err != nil {
					observed <- rdObservation{SetErr: err.Error()}
					return
				}
				_, err := io.ReadAll(r.Body)
				returned := time.Now()
				obs := rdObservation{Read: rdClass(err), ContextEnded: r.Context().Err() != nil}
				obs.EarlyTimeout = obs.Read == rdTimeout && returned.Before(d)
				observed <- obs
				<-held
				if tc.answer {
					w.WriteHeader(http.StatusRequestTimeout)
					_, _ = io.WriteString(w, "late")
				}
			})
			// Registered after the server's cleanup, so it runs first.
			t.Cleanup(release)
			conn := rdDial(t, addr)
			if _, err := io.WriteString(conn, stall.sent); err != nil {
				t.Fatal(err)
			}
			var got stalled
			got.Observed = rdAwait(t, observed, "the end of the handler's body read")
			if _, err := io.WriteString(conn, stall.rest+rdSecond); err != nil {
				got.LateWriteErr = err.Error()
			}
			release()
			got.Answers, got.Unparsed, got.Ended = rdRead(conn, rdBound)
			s.stop()
			got.LaterServed = s.later.Load()
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("stalled body under a handler's read deadline (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHandlerBodyFramingControls sends each stalled request of the deadline test whole, with a later request, to a
// handler that sets no deadline: the body is read to its end and both requests are answered on the connection. It
// shows that the deadline rows fail, where they fail, for the deadline and not for the framing.
func TestHandlerBodyFramingControls(t *testing.T) {
	type answered struct {
		Answers  []rdAnswer
		Unparsed string
		Ended    bool
	}
	tests := map[string]struct {
		// stall names the request in rdStalls.
		stall string
	}{
		"success: a body framed by Content-Length after its first bytes, sent whole without a deadline, is read and answered": {stall: "a body framed by Content-Length after its first bytes"},
		"success: a body framed by Content-Length before its first byte, sent whole without a deadline, is read and answered": {stall: "a body framed by Content-Length before its first byte"},
		"success: a chunked body inside its chunk data, sent whole without a deadline, is read and answered":                  {stall: "a chunked body inside its chunk data"},
		"success: a chunked body inside a chunk size line, sent whole without a deadline, is read and answered":               {stall: "a chunked body inside a chunk size line"},
		"success: a chunked body inside its trailer section, sent whole without a deadline, is read and answered":             {stall: "a chunked body inside its trailer section"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			stall := rdStalls[tc.stall]
			addr, _ := rdServe(t, func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return
				}
				reply := "first:" + string(body)
				w.Header().Set("Content-Length", strconv.Itoa(len(reply)))
				_, _ = io.WriteString(w, reply)
			})
			conn := rdDial(t, addr)
			if _, err := io.WriteString(conn, stall.sent+stall.rest+"GET /second HTTP/1.1\r\nHost: read-deadline.test\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			var got answered
			got.Answers, got.Unparsed, got.Ended = rdRead(conn, rdBound)
			want := answered{Answers: []rdAnswer{{Status: http.StatusOK, Body: "first:" + stall.body}, {Status: http.StatusOK, Close: true, Body: "second"}}, Ended: true}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("request sent whole without a deadline (-want +got):\n%s", diff)
			}
		})
	}
}

// rdAwaitAnswers waits until the server has written n complete answers on conn, or has closed it, and returns them.
func rdAwaitAnswers(t *testing.T, conn *hbStagedConn, n int) []rdAnswer {
	t.Helper()
	timer := time.NewTimer(rdBound)
	defer timer.Stop()
	for {
		raw, _ := conn.written()
		answers, _ := rdParse(raw)
		if len(answers) >= n {
			return answers
		}
		select {
		case <-conn.wrote:
		case <-conn.done:
			raw, _ := conn.written()
			answers, _ := rdParse(raw)
			return answers
		case <-timer.C:
			t.Fatalf("the server wrote %d of %d answers within %v: %q", len(answers), n, rdBound, raw)
		}
	}
}

// TestHandlerReadDeadlineAfterACompleteBody sets a read deadline that expires after the request body was read to its
// end, while the listener watches the connection for the client's departure. Nothing may come of it: the request
// context stays alive, the answer keeps the connection, and the next request on the same connection is served. The
// whole first request is staged before the server reads it, so the body's read is complete before any deadline can
// pass, and the whole exchange runs in a synctest bubble, so the deadline and the watch are on a virtual clock. A
// request without a body has the departure watch running before its handler starts, so the deadline is armed under
// it.
func TestHandlerReadDeadlineAfterACompleteBody(t *testing.T) {
	type complete struct {
		Observed rdObservation
		Answers  []rdAnswer
		Served   int32
	}
	const (
		withLength  = "POST /first HTTP/1.1\r\nHost: read-deadline.test\r\nContent-Length: 2\r\n\r\n{}"
		withTrailer = "POST /first HTTP/1.1\r\nHost: read-deadline.test\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\nX-Trailer: v\r\n\r\n"
		bodyless    = "GET /first HTTP/1.1\r\nHost: read-deadline.test\r\n\r\n"
	)
	tests := map[string]struct {
		request, body string
		// beforeRead sets the deadline before the body is read, to expire while the handler works afterwards;
		// otherwise a deadline already in the past is set after the read.
		beforeRead bool
	}{
		"success: a deadline already in the past, set after the body was read": {request: withLength, body: "{}"},
		"success: a deadline set before the read that expires while the handler works after it": {
			request: withLength, body: "{}", beforeRead: true,
		},
		"success: a deadline set before reading a chunked body and its trailer expires while the handler works after it": {
			request: withTrailer, body: "{}", beforeRead: true,
		},
		"success: a deadline armed under the departure watch of a request without a body expires while the handler works": {
			request: bodyless, beforeRead: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				want := complete{
					Observed: rdObservation{Read: "complete", Body: tc.body},
					Answers:  []rdAnswer{{Status: http.StatusOK, Body: "first"}, {Status: http.StatusOK, Body: "second"}},
					Served:   1,
				}
				observed := make(chan rdObservation, 1)
				conn := newHBStagedConn(tc.request, 0, 0)
				t.Cleanup(func() { _ = conn.Close() })
				s := rdServeOn(t, newHBStagedListener(conn), func(w http.ResponseWriter, r *http.Request) {
					rc := http.NewResponseController(w)
					var obs rdObservation
					set := func(d time.Time) {
						if err := rc.SetReadDeadline(d); err != nil && obs.SetErr == "" {
							obs.SetErr = err.Error()
						}
					}
					d := time.Now().Add(rdDeadline / 3)
					if tc.beforeRead {
						set(d)
					}
					body, err := io.ReadAll(r.Body)
					obs.Read, obs.Body = rdClass(err), string(body)
					if !tc.beforeRead {
						d = time.Now().Add(-time.Second)
						set(d)
					}
					// The watch lasts rdWatch past the deadline, or past now when the deadline is already behind.
					watchUntil := time.Now().Add(rdWatch)
					if d.After(time.Now()) {
						watchUntil = d.Add(rdWatch)
					}
					watch := time.NewTimer(time.Until(watchUntil))
					select {
					case <-r.Context().Done():
						obs.ContextEnded = true
					case <-watch.C:
					}
					watch.Stop()
					observed <- obs
					w.Header().Set("Content-Length", "5")
					_, _ = io.WriteString(w, "first")
				})
				var got complete
				got.Observed = rdAwait(t, observed, "the handler's observation")
				got.Answers = rdAwaitAnswers(t, conn, 1)
				if len(got.Answers) == 1 && !got.Answers[0].Close {
					conn.feed(rdSecond)
					got.Answers = rdAwaitAnswers(t, conn, 2)
				}
				s.stop()
				got.Served = s.later.Load()
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("read deadline after a complete body (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// TestHandlerReadDeadlineZeroClears sets a read deadline and clears it with the zero time before any body byte has
// arrived; the client's body is sent only after the cleared deadline has passed, and the read must still succeed and
// the connection carry the next request. It runs on a staged connection in a synctest bubble, on a virtual clock, and
// on a real loopback connection, where the server's read of the late body goes through the operating system's poll
// deadline.
func TestHandlerReadDeadlineZeroClears(t *testing.T) {
	type cleared struct {
		Observed rdObservation
		// LateWriteErr is the error of the client's write of the body after the cleared deadline.
		LateWriteErr string
		Answers      []rdAnswer
	}
	want := cleared{
		Observed: rdObservation{Read: "complete", Body: "{}"},
		Answers:  []rdAnswer{{Status: http.StatusOK, Body: "first"}, {Status: http.StatusOK, Body: "second"}},
	}
	const head = "POST /first HTTP/1.1\r\nHost: read-deadline.test\r\nContent-Length: 2\r\n\r\n"
	// handler sets a deadline and clears it, tells the test the deadline it cleared, then reads the body.
	handler := func(deadlines chan<- time.Time, observed chan<- rdObservation) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			d := time.Now().Add(rdDeadline)
			for _, set := range []time.Time{d, {}} {
				if err := rc.SetReadDeadline(set); err != nil {
					deadlines <- time.Time{}
					observed <- rdObservation{SetErr: err.Error()}
					return
				}
			}
			deadlines <- d
			body, err := io.ReadAll(r.Body)
			observed <- rdObservation{Read: rdClass(err), Body: string(body)}
			w.Header().Set("Content-Length", "5")
			_, _ = io.WriteString(w, "first")
		}
	}
	// pastTheDeadline waits until the cleared deadline lies well in the past; the read's success is the evidence.
	pastTheDeadline := func(t *testing.T, deadlines <-chan time.Time) {
		t.Helper()
		if d := rdAwait(t, deadlines, "the handler's deadline and its clearing"); !d.IsZero() {
			time.Sleep(time.Until(d.Add(rdDeadline)))
		}
	}
	tests := map[string]struct{ loopback bool }{
		"success: on a staged connection on a virtual clock a deadline cleared with the zero time no longer bounds the body read": {},
		"success: on a real loopback connection a deadline cleared with the zero time no longer bounds the body read":             {loopback: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if tc.loopback {
				deadlines, observed := make(chan time.Time, 1), make(chan rdObservation, 1)
				addr, s := rdServe(t, handler(deadlines, observed))
				conn := rdDial(t, addr)
				br := bufio.NewReader(conn)
				if _, err := io.WriteString(conn, head); err != nil {
					t.Fatal(err)
				}
				pastTheDeadline(t, deadlines)
				var got cleared
				if _, err := io.WriteString(conn, "{}"); err != nil {
					got.LateWriteErr = err.Error()
				}
				got.Observed = rdAwait(t, observed, "the handler's observation")
				for range 2 {
					_ = conn.SetReadDeadline(time.Now().Add(rdBound))
					resp, err := http.ReadResponse(br, nil)
					if err != nil {
						break
					}
					body, err := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if err != nil {
						break
					}
					got.Answers = append(got.Answers, rdAnswer{Status: resp.StatusCode, Close: resp.Close, Body: string(body)})
					if resp.Close {
						break
					}
					if len(got.Answers) == 1 {
						if _, err := io.WriteString(conn, rdSecond); err != nil {
							break
						}
					}
				}
				s.stop()
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("cleared read deadline on a loopback connection (-want +got):\n%s", diff)
				}
				return
			}
			synctest.Test(t, func(t *testing.T) {
				deadlines, observed := make(chan time.Time, 1), make(chan rdObservation, 1)
				conn := newHBStagedConn(head, 0, 0)
				t.Cleanup(func() { _ = conn.Close() })
				s := rdServeOn(t, newHBStagedListener(conn), handler(deadlines, observed))
				pastTheDeadline(t, deadlines)
				conn.feed("{}")
				var got cleared
				got.Observed = rdAwait(t, observed, "the handler's observation")
				got.Answers = rdAwaitAnswers(t, conn, 1)
				if len(got.Answers) == 1 && !got.Answers[0].Close {
					conn.feed(rdSecond)
					got.Answers = rdAwaitAnswers(t, conn, 2)
				}
				s.stop()
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("cleared read deadline on a staged connection (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// rdStallFallback ends a stop entity that stalls with no read deadline set, so that a dispatcher without one fails the
// test instead of blocking its bubble forever.
const rdStallFallback = time.Hour

var errRdStalled = errors.New("the stop entity stalled and no read deadline ended the read")

// rdDeadlineWriter is a response writer that accepts a read deadline, as the public listener's writer does, and
// tells the request body about every change.
type rdDeadlineWriter struct {
	*httptest.ResponseRecorder
	mu       sync.Mutex
	deadline time.Time
	changed  chan struct{}
}

func (w *rdDeadlineWriter) SetReadDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = d
	close(w.changed)
	w.changed = make(chan struct{})
	return nil
}

func (w *rdDeadlineWriter) current() (time.Time, <-chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.deadline, w.changed
}

// rdStopBody hands out data and then ends, or stalls until the writer's read deadline passes and fails as a
// connection read does at its deadline. It records the deadline in force at its first read and what ended a stall.
type rdStopBody struct {
	w        *rdDeadlineWriter
	data     []byte
	complete bool

	off        int
	read       bool
	atFirst    time.Time
	stallEnded string
}

func (b *rdStopBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		b.atFirst, _ = b.w.current()
	}
	if b.off < len(b.data) {
		n := copy(p, b.data[b.off:])
		b.off += n
		return n, nil
	}
	if b.complete {
		return 0, io.EOF
	}
	for {
		d, changed := b.w.current()
		if !d.IsZero() && !time.Now().Before(d) {
			b.stallEnded = "the dispatcher's read deadline"
			return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
		}
		wait := rdStallFallback
		if !d.IsZero() {
			wait = time.Until(d)
		}
		timer := time.NewTimer(wait)
		select {
		case <-changed:
			timer.Stop()
		case <-timer.C:
			if d.IsZero() {
				b.stallEnded = "no read deadline"
				return 0, errRdStalled
			}
		}
	}
}

func (*rdStopBody) Close() error { return nil }

// TestControlStopBoundsItsBodyRead decides that the stop route bounds the read of its entity with a read deadline set
// through the response's controller before the first body read. No document states the bound's value, so the test
// pins only that a deadline in the future is in force when the read begins, that a stalled entity ends at it without
// a stop and with the answer to an unreadable entity, an empty 400, and that an entity that arrives in time is
// acknowledged.
func TestControlStopBoundsItsBodyRead(t *testing.T) {
	type stopRead struct {
		// DeadlineAtFirstRead records that a read deadline later than the request's start was in force at the
		// entity's first read.
		DeadlineAtFirstRead bool
		StallEnded          string
		Status              int
		Body                string
		Stops               int64
	}
	tests := map[string]struct {
		data     string
		complete bool
		want     stopRead
	}{
		"error: a stop entity that stalls ends at the dispatcher's read deadline without a stop": {
			data: "{",
			want: stopRead{DeadlineAtFirstRead: true, StallEnded: "the dispatcher's read deadline", Status: ctlReadErrorStatus},
		},
		"success: a stop entity that arrives within the bound is acknowledged": {
			data: "{}", complete: true,
			want: stopRead{DeadlineAtFirstRead: true, Status: http.StatusAccepted, Body: `{"ok":true,"stopping":true}`, Stops: 1},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, h := ctlDirectGateway(t, nil)
				w := &rdDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), changed: make(chan struct{})}
				body := &rdStopBody{w: w, data: []byte(tc.data), complete: tc.complete}
				req := ctlDirectStop(t, body, 2)
				start := time.Now()
				ctlNoPanic(t, "ServeHTTP", func() struct{} {
					h.ServeHTTP(w, req)
					return struct{}{}
				})
				got := stopRead{
					DeadlineAtFirstRead: !body.atFirst.IsZero() && body.atFirst.After(start),
					StallEnded:          body.stallEnded,
					Status:              w.Code,
					Body:                w.Body.String(),
					Stops:               f.stops.Load(),
				}
				if diff := gocmp.Diff(tc.want, got); diff != "" {
					t.Errorf("stop entity read (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// rdGateListener hands the public listener connections that hold the server's reads at a stream offset until the
// test releases them and record, on the server's side, what the server does around an oversized upload.
type rdGateListener struct {
	net.Listener
	// gate is read at each accept, so that the test can set it once it knows the request.
	gate  *atomic.Int64
	conns chan *rdGateConn
	// limit, when positive, is the stream offset whose crossing by the server's reads is timed.
	limit int64
}

func (l rdGateListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return conn, err
	}
	c := &rdGateConn{Conn: conn, gate: l.gate.Load(), limit: l.limit, held: make(chan struct{}, 1), open: make(chan struct{}), closed: make(chan struct{})}
	c.release = sync.OnceFunc(func() { close(c.open) })
	select {
	case l.conns <- c:
	default:
	}
	return c, nil
}

// rdGateConn is the server's side of one connection. No read crosses gate before release; a read at gate waits for
// it and signals held. It records the bytes read, the bytes read when the server first wrote, every read deadline
// set, every read that ended in a timeout with the read deadline in force when it ended, and the close, which closes
// closed. The end of the first response head written and every timed-out read also get a number from one sequence,
// so that their order is known without comparing clocks. When limit is positive it records when a read first brought
// the bytes read to limit.
type rdGateConn struct {
	net.Conn
	gate    int64
	limit   int64
	held    chan struct{}
	open    chan struct{}
	release func()
	closed  chan struct{}

	mu          sync.Mutex
	read        int64
	readAtWrite int64 // The bytes read when the server first wrote, once wrote is set.
	wrote       bool
	deadlines   []rdDeadlineSet
	// effective is the read deadline in force on the connection: the last one the server set.
	effective time.Time
	timeouts  []rdTimedOut
	closedAt  time.Time
	crossedAt time.Time
	// seq numbers the ordered events; head holds the bytes written until the first head's end, whose number is
	// headAt and whose time is headWrittenAt once it has been written.
	seq           int
	head          []byte
	headAt        int
	headWrittenAt time.Time
}

// rdDeadlineSet is one read deadline the server set on the connection, when it set it, and, where the connection
// numbers its events, its number in the sequence.
type rdDeadlineSet struct {
	at, deadline time.Time
	seq          int
}

// rdTimedOut is one read that ended in a timeout: when, under which read deadline, and its number in the sequence.
type rdTimedOut struct {
	at, deadline time.Time
	seq          int
}

func (c *rdGateConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	read := c.read
	c.mu.Unlock()
	select {
	case <-c.open:
	default:
		if read >= c.gate {
			select {
			case c.held <- struct{}{}:
			default:
			}
			<-c.open
		} else if int64(len(p)) > c.gate-read {
			p = p[:c.gate-read]
		}
	}
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	before := c.read
	c.read += int64(n)
	if c.limit > 0 && before < c.limit && c.read >= c.limit {
		c.crossedAt = time.Now()
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		c.seq++
		c.timeouts = append(c.timeouts, rdTimedOut{at: time.Now(), deadline: c.effective, seq: c.seq})
	}
	c.mu.Unlock()
	return n, err
}

func (c *rdGateConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if !c.wrote {
		c.wrote, c.readAtWrite = true, c.read
	}
	c.mu.Unlock()
	n, err := c.Conn.Write(p)
	c.mu.Lock()
	if c.headAt == 0 {
		c.head = append(c.head, p[:n]...)
		if bytes.Contains(c.head, []byte("\r\n\r\n")) {
			c.seq++
			c.headAt, c.headWrittenAt = c.seq, time.Now()
		}
	}
	c.mu.Unlock()
	return n, err
}

func (c *rdGateConn) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, rdDeadlineSet{at: time.Now(), deadline: d})
	c.effective = d
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(d)
}

// SetDeadline sets the read deadline as well, so it is recorded as one.
func (c *rdGateConn) SetDeadline(d time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, rdDeadlineSet{at: time.Now(), deadline: d})
	c.effective = d
	c.mu.Unlock()
	return c.Conn.SetDeadline(d)
}

func (c *rdGateConn) Close() error {
	c.mu.Lock()
	first := c.closedAt.IsZero()
	if first {
		c.closedAt = time.Now()
	}
	c.mu.Unlock()
	if first {
		close(c.closed)
	}
	c.release()
	return c.Conn.Close()
}

// CloseWrite keeps the half-close the server uses before it closes a connection.
func (c *rdGateConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *rdGateConn) bytesRead() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read
}

// rdWriteZeros writes n zero bytes to conn.
func rdWriteZeros(conn net.Conn, n int64) error {
	buf := make([]byte, 1<<20)
	for n > 0 {
		m, err := conn.Write(buf[:min(int64(len(buf)), n)])
		n -= int64(m)
		if err != nil {
			return err
		}
	}
	return nil
}

// TestOversizeUploadGrace pins the relay's refusal of an upload above relay.BodyLimit through the production handler
// on the public listener, on a real loopback socket, as it behaves once the listener supports a handler's read
// deadline. The server's reads are held exactly at relay.BodyLimit bytes of the body, and the client's byte that
// exceeds the bound is queued before the hold is let go; nothing follows it, so the upload stalls. The server must have
// read the excess byte when it first writes. The 413 head carries Connection: close, Content-Length: 0 and
// X-Should-Retry: false. The relay's grace is identified by the deadline the relay itself requests through its
// response's controller, which the listener must apply to the connection while the handler runs: the stalled body read
// ends in a timeout while that deadline is the one in force, after the complete head was written and before the
// handler returns, and the listener closes the connection no earlier than it. The upload is not delivered to a worker.
// The grace's exact value is the relay's own test's (relay/upload_test.go, on a virtual clock), and the moment of the
// close and an upload that completes within the grace are decided on a virtual clock by
// TestOversizeUploadGraceOnAVirtualClock; this one shows that Serve applies and honours the relay's deadline on a
// socket.
func TestOversizeUploadGrace(t *testing.T) {
	type oversize struct {
		Status  int
		Close   bool
		Length  string
		NoRetry string
		// ExcessByteReadBeforeAnswer records that the server had read the byte past the bound when it first wrote.
		ExcessByteReadBeforeAnswer bool
		BytesAfterHead             int
		Ended                      string
		// ReadAtReturn records that the server had read every byte the client wrote when the handler returned.
		ReadAtReturn bool
		// RelayRequestedDeadline records a read deadline the relay requested through its response's controller, and
		// AppliedToTheConnection that the connection was given exactly that deadline before the handler returned.
		RelayRequestedDeadline bool
		AppliedToTheConnection bool
		// TimedOutUnderIt records a read of the connection that ended in a timeout while the read deadline in force
		// was exactly that deadline, no earlier than it and no later than the handler's return; HeadBeforeTimeout that
		// the complete 413 head had been written before that read timed out; ClosedAtOrAfter that the listener closed
		// the connection no earlier than that deadline.
		TimedOutUnderIt   bool
		HeadBeforeTimeout bool
		ClosedAtOrAfter   bool
		Leases            int
	}
	tests := map[string]struct {
		// tail is the part of the declared length past the byte that exceeds the bound; the client never writes it.
		tail int64
		want oversize
	}{
		"error: a stalled upload's body read times out under the relay's deadline and the connection is closed after it": {
			tail: 1 << 20,
			want: oversize{
				Status: http.StatusRequestEntityTooLarge, Close: true, Length: "0", NoRetry: "false",
				ExcessByteReadBeforeAnswer: true, Ended: "ended", ReadAtReturn: true, RelayRequestedDeadline: true,
				AppliedToTheConnection: true, TimedOutUnderIt: true, HeadBeforeTimeout: true, ClosedAtOrAfter: true,
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &hcLog{}
			worker := startHCWorker(t, "a", log, hcPending)
			src := newHCSource(log, newHCGen("a", worker, true))
			conns := make(chan *rdGateConn, 1)
			var gate atomic.Int64
			var server atomic.Pointer[rdGateConn]
			var mu sync.Mutex
			var readAtReturn int64
			var returnedAt time.Time
			var requested []time.Time
			public, h := hcServeWrapped(t, src,
				func(ln net.Listener) net.Listener { return rdGateListener{Listener: ln, gate: &gate, conns: conns} },
				func() {
					mu.Lock()
					defer mu.Unlock()
					returnedAt = time.Now()
					if c := server.Load(); c != nil {
						readAtReturn = c.bytesRead()
					}
				},
				func(d time.Time) {
					mu.Lock()
					defer mu.Unlock()
					requested = append(requested, d)
				})
			requestHead := "POST /v1/messages HTTP/1.1\r\nHost: " + public + "\r\nContent-Type: application/json\r\nContent-Length: " +
				strconv.FormatInt(relay.BodyLimit+1+tc.tail, 10) + "\r\n\r\n"
			gate.Store(int64(len(requestHead)) + relay.BodyLimit)
			total := int64(len(requestHead)) + relay.BodyLimit + 1
			conn := hcDial(t, public)
			written := make(chan error, 1)
			go func() {
				if _, err := io.WriteString(conn, requestHead); err != nil {
					written <- err
					return
				}
				written <- rdWriteZeros(conn, total-int64(len(requestHead)))
			}()
			var c *rdGateConn
			select {
			case c = <-conns:
			case <-time.After(rdBound):
				t.Fatalf("the public listener accepted no connection within %v", rdBound)
			}
			server.Store(c)
			// Registered after the listener's cleanup, so it runs first.
			t.Cleanup(c.release)
			hcAwait(t, c.held, h, "the server's read at exactly the body limit")
			// Everything past the held point, the excess byte, is queued before the hold is let go.
			select {
			case err := <-written:
				if err != nil {
					t.Fatalf("write the upload: %v", err)
				}
			case <-time.After(rdBound):
				t.Fatalf("the upload past the body limit was not queued within %v", rdBound)
			}
			c.release()
			got := oversize{}
			br := bufio.NewReader(conn)
			// A connection broken before its refusal arrived leaves the head's fields empty.
			if resp, err := http.ReadResponse(br, nil); err == nil {
				_ = resp.Body.Close()
				got.Status, got.Close = resp.StatusCode, resp.Close
				got.Length, got.NoRetry = resp.Header.Get("Content-Length"), resp.Header.Get("X-Should-Retry")
			}
			more, end := hcReadEnd(conn, br, rdBound)
			got.BytesAfterHead, got.Ended = len(more), end
			if end != "no end within the bound" {
				got.Ended = "ended"
			}
			hcAwait(t, h.returned, h, "the handler's return")
			select {
			case <-c.closed:
			case <-time.After(rdBound):
				t.Fatalf("the listener did not close the connection within %v", rdBound)
			}
			mu.Lock()
			var grace time.Time
			for _, d := range requested {
				if !d.IsZero() {
					grace = d
					break
				}
			}
			got.ReadAtReturn = readAtReturn == total
			got.RelayRequestedDeadline = !grace.IsZero()
			returned := returnedAt
			mu.Unlock()
			c.mu.Lock()
			got.ExcessByteReadBeforeAnswer = c.wrote && c.readAtWrite >= c.gate+1
			for _, d := range c.deadlines {
				if !grace.IsZero() && d.deadline.Equal(grace) && !d.at.After(returned) {
					got.AppliedToTheConnection = true
				}
			}
			if got.AppliedToTheConnection {
				// The first such read is the stalled drain's timeout; the head must precede that one, not a later read.
				for _, to := range c.timeouts {
					if to.deadline.Equal(grace) && !to.at.Before(grace) && !to.at.After(returned) {
						got.TimedOutUnderIt = true
						got.HeadBeforeTimeout = c.headAt != 0 && c.headAt < to.seq
						break
					}
				}
				got.ClosedAtOrAfter = !c.closedAt.Before(grace)
			}
			deadlines, timeouts, closedAt, readAtWrite := c.deadlines, c.timeouts, c.closedAt, c.readAtWrite
			c.mu.Unlock()
			got.Leases = len(src.leased(""))
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("oversized upload (-want +got):\n%s\nserver read %d of %d bytes by the handler's return and %d at its first write (held at %d); relay requested %v; connection deadlines %v; timed-out reads %v; handler returned %v; closed %v",
					diff, readAtReturn, total, readAtWrite, c.gate, requested, deadlines, timeouts, returned, closedAt)
			}
		})
	}
}

// rdStagedRecorder is a staged connection that numbers, under the staged connection's own mutex, each assignment of
// its read deadline and the handler's return, so that their order is the order in which they took effect.
type rdStagedRecorder struct {
	*hbStagedConn
	// Guarded by hbStagedConn.mu.
	seq      int
	assigned []rdDeadlineSet
	returned int
}

// SetReadDeadline assigns the deadline as hbStagedConn does and numbers the assignment while it holds the lock.
func (c *rdStagedRecorder) SetReadDeadline(d time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = d
	close(c.changed)
	c.changed = make(chan struct{})
	c.seq++
	c.assigned = append(c.assigned, rdDeadlineSet{at: time.Now(), deadline: d, seq: c.seq})
	return nil
}

func (c *rdStagedRecorder) SetDeadline(d time.Time) error { return c.SetReadDeadline(d) }

// markReturn numbers the handler's return in the same sequence, once.
func (c *rdStagedRecorder) markReturn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.returned == 0 {
		c.seq++
		c.returned = c.seq
	}
}

// record returns the numbered deadline assignments and the number of the handler's return, zero before it.
func (c *rdStagedRecorder) record() ([]rdDeadlineSet, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.assigned), c.returned
}

// rdRecordingListener accepts its one staged connection as the recorder that wraps it.
type rdRecordingListener struct {
	*hbStagedListener
	rec *rdStagedRecorder
}

func (l rdRecordingListener) Accept() (net.Conn, error) {
	if _, err := l.hbStagedListener.Accept(); err != nil {
		return nil, err
	}
	return l.rec, nil
}

// TestOversizeUploadGraceOnAVirtualClock pins the relay's refusal of an upload above relay.BodyLimit through the
// production handler and the public listener on a staged connection in a synctest bubble, so that the relay's grace
// and the listener's own timers run on a virtual clock and no scheduling of the host decides a row. The whole client
// stream is staged before the server reads it; no worker generation exists, and the relay asks for no lease. The 413
// head carries Connection: close, Content-Length: 0 and X-Should-Retry: false, the server has read the excess byte when
// it first writes, and the listener applies the deadline the relay requests before the handler returns: the staged
// connection numbers each assignment of its read deadline and the handler's return under its own mutex, so the order in
// which they took effect is decided without comparing equal virtual times.
//
// An upload whose tail is queued is read to its end within the grace: every staged byte is read by the handler's
// return and no read times out under the relay's deadline. The connection then ends in order: nothing follows the
// complete head, no staged byte is left unread when the listener closes it, so the close cannot turn into a reset, and
// the close is observed. Which socket operations lead to that end is the listener's choice. A stalled upload's
// complete head is written at the virtual instant the server's read crosses the byte past the bound, and the relay's
// deadline is exactly relay.OversizeGrace after that instant, so a listener that holds the relay in its flush cannot
// shift the grace; the drain times out under that deadline after the head, and the listener closes the connection at
// exactly that deadline: a close that waits for a further timer, such as the listener's own linger, comes later on the
// virtual clock and fails.
func TestOversizeUploadGraceOnAVirtualClock(t *testing.T) {
	type staged struct {
		Status  int
		Close   bool
		Length  string
		NoRetry string
		// ExcessByteReadBeforeAnswer records that the server had read the byte past the bound when it first wrote.
		ExcessByteReadBeforeAnswer bool
		BytesAfterHead             int
		// ReadAtReturn records that the server had read every staged byte when the handler returned.
		ReadAtReturn bool
		// RelayRequestedDeadline records a read deadline the relay requested through its response's controller, and
		// AppliedToTheConnection that the connection was given exactly that deadline before the handler returned.
		RelayRequestedDeadline bool
		AppliedToTheConnection bool
		// TimedOutUnderIt records a read that ended in a timeout while the read deadline in force was exactly the
		// relay's, no earlier than it and no later than the handler's return; HeadBeforeTimeout that the complete head
		// had been written before the first such read timed out.
		TimedOutUnderIt   bool
		HeadBeforeTimeout bool
		// HeadAtTheCrossing records that the complete head was written at the virtual instant the server's read crossed
		// the byte past the bound; it is decided for the stalled upload only.
		HeadAtTheCrossing bool
		// GraceFromTheCrossing records that the relay's deadline is exactly relay.OversizeGrace after the crossing
		// instant; it is decided for the stalled upload only.
		GraceFromTheCrossing bool
		// ClosedAtTheGrace records that the listener closed the connection at exactly the relay's deadline; it is
		// decided for the stalled upload only.
		ClosedAtTheGrace bool
		// Unread counts the staged bytes the server had not read when it closed the connection.
		Unread   int64
		Acquires int32
	}
	head := staged{
		Status: http.StatusRequestEntityTooLarge, Close: true, Length: "0", NoRetry: "false",
		ExcessByteReadBeforeAnswer: true, ReadAtReturn: true, RelayRequestedDeadline: true, AppliedToTheConnection: true,
	}
	tests := map[string]struct {
		// tail is the part of the declared length past the byte that exceeds the bound; it is staged unless stalls is
		// set, when the stream ends at that byte and the client's side stays open.
		tail   int64
		stalls bool
		want   staged
	}{
		"error: an upload whose tail is queued is read to its end within the grace and the connection ends in order": {
			tail: 16 << 10,
			want: head,
		},
		"error: a stalled upload's drain times out under the relay's deadline and the connection is closed at that instant": {
			tail: 1 << 20, stalls: true,
			want: func() staged {
				s := head
				s.HeadAtTheCrossing, s.GraceFromTheCrossing, s.TimedOutUnderIt, s.HeadBeforeTimeout, s.ClosedAtTheGrace = true, true, true, true, true
				return s
			}(),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				src := newHCSource(&hcLog{})
				inner, err := supervisor.NewLeaseRelay(hbAddr.Port, src, hcToken, "1.2.3", 37, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				requestHead := "POST /v1/messages HTTP/1.1\r\nHost: " + hbAddr.String() + "\r\nContent-Type: application/json\r\nContent-Length: " +
					strconv.FormatInt(relay.BodyLimit+1+tc.tail, 10) + "\r\n\r\n"
				body := relay.BodyLimit + 1 + tc.tail
				if tc.stalls {
					body = relay.BodyLimit + 1
				}
				stage := newHBStagedConn(requestHead+string(make([]byte, body)), 0, 0)
				total := int64(len(requestHead)) + body
				// stagedRead is the count of staged bytes the server has read.
				stagedRead := func() int64 {
					stage.mu.Lock()
					defer stage.mu.Unlock()
					return int64(stage.off)
				}
				var mu sync.Mutex
				var readAtReturn int64
				var returnedAt time.Time
				var requested []time.Time
				rec := &rdStagedRecorder{hbStagedConn: stage}
				h := &hcObserved{
					inner: inner, returned: make(chan struct{}),
					onReturn: func() {
						rec.markReturn()
						mu.Lock()
						defer mu.Unlock()
						returnedAt, readAtReturn = time.Now(), stagedRead()
					},
					onDeadline: func(d time.Time) {
						mu.Lock()
						defer mu.Unlock()
						requested = append(requested, d)
					},
				}
				// The gate is never reached, so the recording connection holds no read.
				var gate atomic.Int64
				gate.Store(math.MaxInt64)
				conns := make(chan *rdGateConn, 1)
				ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
				done := make(chan error, 1)
				go func() {
					ln := rdGateListener{
						Listener: rdRecordingListener{hbStagedListener: newHBStagedListener(stage), rec: rec}, gate: &gate, conns: conns,
						limit: int64(len(requestHead)) + relay.BodyLimit + 1,
					}
					done <- supervisor.Serve(ctx, ln, h)
				}()
				t.Cleanup(func() {
					// The staged connection is closed first, so that no read of the server outlasts the join.
					_ = stage.Close()
					cancel()
					timer := time.NewTimer(rdBound)
					defer timer.Stop()
					select {
					case err := <-done:
						if err != nil {
							t.Errorf("public listener: %v", err)
						}
					case <-timer.C:
						t.Errorf("the public listener did not return within %v of its cancellation, with its connection closed", rdBound)
					}
				})
				c := rdAwait(t, conns, "the public listener's accept")
				rdAwait(t, h.returned, "the handler's return")
				rdAwait(t, c.closed, "the listener's close of the connection")
				var got staged
				raw, _ := stage.written()
				br := bufio.NewReader(bytes.NewReader(raw))
				got.BytesAfterHead = len(raw)
				if resp, err := http.ReadResponse(br, nil); err == nil {
					_ = resp.Body.Close()
					got.Status, got.Close = resp.StatusCode, resp.Close
					got.Length, got.NoRetry = resp.Header.Get("Content-Length"), resp.Header.Get("X-Should-Retry")
					rest, _ := io.ReadAll(br)
					got.BytesAfterHead = len(rest)
				}
				mu.Lock()
				var grace time.Time
				for _, d := range requested {
					if !d.IsZero() {
						grace = d
						break
					}
				}
				got.ReadAtReturn = readAtReturn == total
				got.RelayRequestedDeadline = !grace.IsZero()
				returned := returnedAt
				mu.Unlock()
				got.Unread = total - stagedRead()
				assigned, returnedSeq := rec.record()
				for _, d := range assigned {
					if !grace.IsZero() && d.deadline.Equal(grace) && returnedSeq != 0 && d.seq < returnedSeq {
						got.AppliedToTheConnection = true
					}
				}
				c.mu.Lock()
				got.ExcessByteReadBeforeAnswer = c.wrote && c.readAtWrite >= int64(len(requestHead))+relay.BodyLimit+1
				if got.AppliedToTheConnection {
					for _, to := range c.timeouts {
						if to.deadline.Equal(grace) && !to.at.Before(grace) && !to.at.After(returned) {
							got.TimedOutUnderIt = true
							got.HeadBeforeTimeout = c.headAt != 0 && c.headAt < to.seq
							break
						}
					}
				}
				if tc.stalls {
					got.HeadAtTheCrossing = c.headAt != 0 && !c.crossedAt.IsZero() && c.headWrittenAt.Equal(c.crossedAt)
					got.GraceFromTheCrossing = !c.crossedAt.IsZero() && grace.Equal(c.crossedAt.Add(relay.OversizeGrace))
					got.ClosedAtTheGrace = !grace.IsZero() && c.closedAt.Equal(grace)
				}
				timeouts, closedAt, readAtWrite := c.timeouts, c.closedAt, c.readAtWrite
				crossedAt, headWrittenAt := c.crossedAt, c.headWrittenAt
				c.mu.Unlock()
				got.Acquires = src.acquires.Load()
				if diff := gocmp.Diff(tc.want, got); diff != "" {
					t.Errorf("oversized upload on a virtual clock (-want +got):\n%s\nserver read %d of %d bytes by the handler's return and %d at its first write; relay requested %v; staged deadline assignments %v; timed-out reads %v; handler returned %v (event %d); limit crossed %v; head written %v; closed %v",
						diff, readAtReturn, total, readAtWrite, requested, assigned, timeouts, returned, returnedSeq, crossedAt, headWrittenAt, closedAt)
				}
			})
		})
	}
}

// TestHandlerReadDeadlineBoundsATrailerOnAVirtualClock stalls a chunked body inside its trailer section on a staged
// connection in a synctest bubble, so that the clock is virtual and the moment a read returns is exact. The handler
// sets a read deadline before it reads the body; the read must end with a timeout at exactly that deadline, not at a
// later one the trailer's parser installs, and the request context must still be live then. The loopback row of
// TestHandlerReadDeadlineEndsAStalledBody decides the same stall on a socket, where only a lower bound can be judged.
func TestHandlerReadDeadlineBoundsATrailerOnAVirtualClock(t *testing.T) {
	type trailer struct {
		// SetErr is the error of SetReadDeadline, empty when it was accepted.
		SetErr string
		Read   string
		// AtTheDeadline records that the read returned at exactly the deadline the handler set.
		AtTheDeadline bool
		ContextEnded  bool
	}
	tests := map[string]struct {
		// stall names the request in rdStalls.
		stall string
	}{
		"error: a chunked body inside its trailer section ends at exactly the handler's read deadline": {stall: "a chunked body inside its trailer section"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				observed := make(chan trailer, 1)
				conn := newHBStagedConn(rdStalls[tc.stall].sent, 0, 0)
				rdServeOn(t, newHBStagedListener(conn), func(w http.ResponseWriter, r *http.Request) {
					d := time.Now().Add(rdDeadline)
					if err := http.NewResponseController(w).SetReadDeadline(d); err != nil {
						observed <- trailer{SetErr: err.Error()}
						return
					}
					_, err := io.ReadAll(r.Body)
					observed <- trailer{Read: rdClass(err), AtTheDeadline: time.Now().Equal(d), ContextEnded: r.Context().Err() != nil}
				})
				// Registered after the server's cleanup, so it runs first: a read still waiting is let go before the join.
				t.Cleanup(func() { _ = conn.Close() })
				var got trailer
				// The guard is far beyond any deadline of the listener, so that a read ended by a later deadline is
				// reported with its time instead of as a missing event.
				select {
				case got = <-observed:
				case <-time.After(rdStallFallback):
					t.Fatalf("the handler's body read did not end within %v", rdStallFallback)
				}
				if diff := gocmp.Diff(trailer{Read: rdTimeout, AtTheDeadline: true}, got); diff != "" {
					t.Errorf("stalled trailer under a handler's read deadline on a virtual clock (-want +got):\n%s", diff)
				}
			})
		})
	}
}
