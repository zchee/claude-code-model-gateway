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

package relay_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor/relay"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

const (
	// headBound is the largest response head, interim or final, the relay reads from a worker: Node's default limit
	// for the head of an HTTP response (http.maxHeaderSize), which bounds the source's worker leg.
	headBound = 16384
	// readSlack is how far past a point the relay may have read from the worker leg when it stops there: one read
	// buffer's worth.
	readSlack = 64 << 10
	// floodBudget is how much a fake worker that never ends what it sends offers before it gives up. A relay that keeps
	// reading takes all of it, so it shows as reading far past readSlack without the test holding more than this.
	floodBudget = 4 << 20
	// largeBody is the size of a worker body after a small head.
	largeBody = 4 << 20
)

// sendCounted writes s on the worker's side of c, adds the bytes the relay took to taken and reports whether all of s
// was written. A write on the in-memory pipe completes only as far as the relay reads it, so taken counts exactly the
// bytes the relay consumed.
func sendCounted(c *workerConn, s string, taken *atomic.Int64) bool {
	_ = c.conn.SetWriteDeadline(time.Now().Add(workerWriteBudget))
	n, err := io.WriteString(c.conn, s)
	taken.Add(int64(n))
	if err != nil {
		c.writeFailed.Store(true)
		return false
	}
	return true
}

// flood writes unit on c again and again until the relay stops taking it or taken has reached floodBudget.
func flood(c *workerConn, unit string, taken *atomic.Int64) {
	for taken.Load() < floodBudget {
		if !sendCounted(c, unit, taken) {
			return
		}
	}
}

// paddedHead returns a response head of the status line, the fields and an X-Pad field padded so that the head counts
// exactly n bytes toward the bound. Every line ends with eol, CRLF or a bare LF. The count takes every byte of the
// status line and of each field line together with that line's terminator, and leaves out the empty line that ends
// the head, so the last counted byte is the terminator of the X-Pad line.
func paddedHead(status string, fields []string, n int, eol string) string {
	lines := append([]string{status}, fields...)
	lines = append(lines, "X-Pad: ")
	size := 0
	for _, l := range lines {
		size += len(l) + len(eol)
	}
	if n < size {
		panic(fmt.Sprintf("a head of %d counted bytes cannot hold %q", n, lines))
	}
	lines[len(lines)-1] += strings.Repeat("p", n-size)
	return strings.Join(lines, eol) + eol + eol
}

// headCount returns the bytes of head that count toward the bound: everything before the empty line that ends it.
func headCount(head string) int {
	for _, end := range []string{"\r\n\r\n", "\n\n"} {
		if i := strings.Index(head, end); i >= 0 {
			return i + len(end)/2
		}
	}
	panic(fmt.Sprintf("no end of head in %.80q", head))
}

// padOf returns the X-Pad value of a head paddedHead made.
func padOf(head string) string {
	_, after, _ := strings.Cut(head, "\nX-Pad: ")
	v, _, _ := strings.Cut(after, "\n")
	return strings.TrimSuffix(v, "\r")
}

// headWorker returns a worker behaviour that reads the request, submits it and sends answer, counting what the relay
// takes in taken. When unit is set it then sends unit again and again, so that what answer started never ends.
func headWorker(answer, unit string, taken *atomic.Int64) func(c *workerConn) {
	return func(c *workerConn) {
		defer close(c.finished)
		defer c.close()
		if !c.readRequest() {
			return
		}
		c.submit()
		if !sendCounted(c, answer, taken) || unit == "" {
			return
		}
		flood(c, unit, taken)
	}
}

// TestWorkerHeadBound decides the bound on each response head from a worker. Counted toward a head's 16384 bytes are
// every byte of the status line and of each field line together with that line's terminator, CRLF or a bare LF; the
// empty line that ends the head is not counted. A head of exactly 16384 counted bytes is relayed or passed over, one of
// 16385 fails, and every interim head and the final head has a budget of its own. A head over the bound fails that
// connection exactly as a malformed head does: for a worker that took the request and is still current it is the
// taken-request 503 with one Lost report after the attempts' waits, as TestInterimResponses decides for its malformed
// heads and TestDeliveryAttempts for a still-current taker. The relay stops reading a head that never ends within
// readSlack of the bound; no exact stop byte is required. The body is not bounded by it.
//
// Rejected: a relay that reads a head without a bound or with a larger one (the never-ending heads, every 16385-byte
// head); one that leaves line terminators out of the count, or counts a CR but not the LF after it (the 16385-byte
// heads whose byte over the bound is the LF of a CRLF or a bare LF); one that also counts the empty line or the end of
// the head beyond it, or whose bound is below 16384 (the heads of exactly 16384 bytes, with CRLF and with bare LF);
// one that bounds the heads of an answer together instead of each (five interim heads of 16384 bytes); one that also
// bounds the body (4 MiB after a small head); one that answers an oversized head otherwise than a malformed one
// (status, X-Should-Retry, body, the Lost report, the waits); and one that copies an interim head's fields into the
// final one.
func TestWorkerHeadBound(t *testing.T) {
	const final = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
	exactCRLF := paddedHead("HTTP/1.1 200 OK", []string{"Content-Length: 2"}, headBound, "\r\n")
	exactLF := paddedHead("HTTP/1.1 200 OK", []string{"Content-Length: 2"}, headBound, "\n")
	exactInterim := paddedHead("HTTP/1.1 103 Early Hints", nil, headBound, "\r\n")
	overCRLF := paddedHead("HTTP/1.1 200 OK", []string{"Content-Length: 2"}, headBound+1, "\r\n")
	overLF := paddedHead("HTTP/1.1 200 OK", []string{"Content-Length: 2"}, headBound+1, "\n")
	overInterim := paddedHead("HTTP/1.1 103 Early Hints", nil, headBound+1, "\r\n")
	// overByField is a head of exactly 16384 counted bytes and one more field line, so that the byte over the bound is
	// the first character of that line.
	overByField := strings.TrimSuffix(exactCRLF, "\r\n") + "X-After: 1\r\n\r\n"
	// Each fixture head is checked against the rule before it is used: its counted bytes, its last counted byte (the LF
	// that ends its last line) and, for a head over the bound, the byte over it.
	for name, h := range map[string]struct {
		head string
		// counted is the number of counted bytes; over is the byte at the 16385th place, zero for a head at the bound.
		counted int
		over    byte
	}{
		"exact with CRLF":          {head: exactCRLF, counted: headBound},
		"exact with bare LF":       {head: exactLF, counted: headBound},
		"exact interim":            {head: exactInterim, counted: headBound},
		"one over with CRLF":       {head: overCRLF, counted: headBound + 1, over: '\n'},
		"one over with bare LF":    {head: overLF, counted: headBound + 1, over: '\n'},
		"one over interim":         {head: overInterim, counted: headBound + 1, over: '\n'},
		"one over by a field line": {head: overByField, counted: headBound + 12, over: 'X'},
	} {
		n := headCount(h.head)
		ok := n == h.counted && h.head[n-1] == '\n'
		if h.over != 0 {
			// A LF over the bound ends the line that crosses it; any other byte over it starts a new line, so the
			// 16384th counted byte is then the LF of the line before.
			lineEndsAtBound := h.head[headBound-1] == '\n'
			ok = ok && h.head[headBound] == h.over && lineEndsAtBound == (h.over != '\n')
		}
		if !ok {
			t.Fatalf("fixture head %q counts %d bytes with %q around the bound; want %d counted bytes, over byte %q", name, n, h.head[headBound-2:min(len(h.head), headBound+1)], h.counted, h.over)
		}
	}
	many := make([]string, 0, 600)
	for i := range 600 {
		many = append(many, fmt.Sprintf("X-Field-%04d: %s", i, strings.Repeat("v", 24)))
	}
	largeHead := "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: " + strconv.Itoa(largeBody) + "\r\nConnection: close\r\n\r\n"
	padAbsent := func(t *testing.T, head http.Header) {
		t.Helper()
		if v, ok := head["X-Pad"]; ok {
			t.Errorf("the relayed head carries the X-Pad field of an interim head (%d bytes)", len(strings.Join(v, "")))
		}
	}
	padKept := func(head string) func(t *testing.T, relayed http.Header) {
		return func(t *testing.T, relayed http.Header) {
			t.Helper()
			if got, want := relayed.Get("X-Pad"), padOf(head); got != want {
				t.Errorf("X-Pad of the relayed head has %d bytes; want the worker's %d", len(got), len(want))
			}
		}
	}
	tests := map[string]struct {
		// answer is what the worker sends after reading the request; flood, when set, follows it again and again, so
		// that the head answer started never ends.
		answer, flood string
		status        int
		retry         string
		body          string
		// lost is set for a failed connection of a worker that took the request.
		lost bool
		// check decides more of the relayed head.
		check func(t *testing.T, head http.Header)
	}{
		"success: a final head of exactly 16384 counted bytes with CRLF line ends is relayed": {
			answer: exactCRLF + "ok",
			status: http.StatusOK,
			body:   "ok",
			check:  padKept(exactCRLF),
		},
		"success: a final head of exactly 16384 counted bytes with bare LF line ends is relayed": {
			answer: exactLF + "ok",
			status: http.StatusOK,
			body:   "ok",
			check:  padKept(exactLF),
		},
		"success: five interim heads of 16384 counted bytes each are passed over": {
			answer: strings.Repeat(exactInterim, 5) + final,
			status: http.StatusOK,
			body:   "ok",
			check:  padAbsent,
		},
		"success: a body of 4 MiB after a small head is relayed in full": {
			answer: largeHead + strings.Repeat("b", largeBody),
			status: http.StatusOK,
			body:   strings.Repeat("b", largeBody),
		},
		"error: a final head whose CRLF ends one byte over the bound fails the attempt": {
			answer: overCRLF + "ok",
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: a final head whose bare LF is the one byte over the bound fails the attempt": {
			answer: overLF + "ok",
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: a final head with one more field line after 16384 counted bytes fails the attempt": {
			answer: overByField + "ok",
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: a final head over the bound in many small fields fails the attempt": {
			answer: "HTTP/1.1 200 OK\r\n" + strings.Join(many, "\r\n") + "\r\nContent-Length: 2\r\n\r\nok",
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: an interim head one counted byte over the bound fails the attempt": {
			answer: overInterim + final,
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: a final head that never ends in one field is read only up to the bound": {
			answer: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nX-Pad: ",
			flood:  strings.Repeat("p", 1024),
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: a final head that never ends in many small fields is read only up to the bound": {
			answer: "HTTP/1.1 200 OK\r\n",
			flood:  strings.Repeat("X-Flood: "+strings.Repeat("f", 24)+"\r\n", 32),
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
		"error: an interim head that never ends is read only up to the bound": {
			answer: "HTTP/1.1 100 Continue\r\nX-Pad: ",
			flood:  strings.Repeat("p", 1024),
			status: http.StatusServiceUnavailable,
			retry:  "false",
			body:   lostBody,
			lost:   true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				h := w.handler(t)
				var taken atomic.Int64
				g := w.start("gen-a", 41001, 4101, headWorker(tt.answer, tt.flood, &taken))
				cw := newClientWriter()
				o := await(t, serve(h, cw, postMessages(t.Context(), requestBody)), "the relay's answer")
				synctest.Wait()
				mustReturn(t, o)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{tt.status}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if got.body != tt.body {
					t.Errorf("body of %d bytes differs from the %d bytes wanted (first 80: %.80q)", len(got.body), len(tt.body), got.body)
				}
				checkAnswerHead(t, got.head, tt.status, tt.retry)
				if tt.check != nil {
					tt.check(t, got.head)
				}
				switch n := taken.Load(); {
				case tt.flood != "" && n > headBound+readSlack:
					t.Errorf("the relay read %d bytes of a head that never ends; want at most %d, the bound and one read buffer", n, headBound+readSlack)
				case !tt.lost && n != int64(len(tt.answer)):
					t.Errorf("the relay read %d of the %d bytes of the worker's answer; want all of them", n, len(tt.answer))
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				if got := [2]int32{g.reads.Load(), w.upstream.Load()}; got != [2]int32{1, 1} {
					t.Errorf("the worker read %d requests and submitted %d upstream; want one of each", got[0], got[1])
				}
				wantWaits := 0
				var wantLost []relay.Lost
				if tt.lost {
					wantWaits = attemptWaits
					wantLost = []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}}
				}
				if n := len(w.waitCalls()); n != wantWaits {
					t.Errorf("Wait was called %d times; want %d", n, wantWaits)
				}
				checkLost(t, w.lostReports(), wantLost)
				checkAlternation(t, w.callOrder())
				checkLeasesClosed(t, w)
			})
		})
	}
}

// TestEarlyAnswer decides a worker that sends its complete answer without reading the request body, while the upload
// is larger than the leg can hold unread: the in-memory pipe holds nothing, so every byte of the 4 MiB upload waits
// for the worker, and 4 MiB is also above what a loopback socket buffers. The answer is relayed to the client, no
// loss is reported, the request is delivered once, ServeHTTP returns and every lease is closed, whether the worker then
// keeps the connection open and unread or closes it. A relay goroutine still writing the upload would hold the lease
// open, which checkLeasesClosed shows before the world's teardown closes the worker's end. Rejected: a relay that
// writes the whole body before it reads the answer (the worker's answer never gets read, so its write fails and the
// relay reports a loss and answers 503) and one that takes the failed upload write for a lost request although the
// answer arrived.
func TestEarlyAnswer(t *testing.T) {
	const early = `{"type":"error","error":{"type":"authentication_error","message":"answered before the upload was read"}}`
	answer := "HTTP/1.1 401 Unauthorized\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(early)) + "\r\nConnection: close\r\n\r\n" + early
	tests := map[string]struct {
		// readHead makes the worker read the request head before it answers; otherwise it reads nothing.
		readHead bool
		// closeAfter makes the worker close right after its answer; otherwise it keeps the connection open and
		// unread until the test ends.
		closeAfter bool
	}{
		"success: a worker that reads only the request head and answers is relayed": {
			readHead: true,
		},
		"success: a worker that reads nothing and answers is relayed": {},
		"success: a worker that reads only the request head, answers and closes is relayed": {
			readHead:   true,
			closeAfter: true,
		},
		"success: a worker that reads nothing, answers and closes is relayed": {
			closeAfter: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				h := w.handler(t)
				g := w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if tt.readHead {
						tp := textproto.NewReader(c.br)
						if _, err := tp.ReadLine(); err != nil {
							return
						}
						if _, err := tp.ReadMIMEHeader(); err != nil {
							return
						}
					}
					if !c.send(answer) || tt.closeAfter {
						return
					}
					<-c.g.w.done
				})
				cw := newClientWriter()
				r := clientRequest(t.Context(), http.MethodPost, "/v1/messages", http.Header{"Content-Type": {"application/json"}}, newUpload(4*mib, 'u'), 4*mib)
				o := await(t, serve(h, cw, r), "the relayed early answer")
				synctest.Wait()
				mustReturn(t, o)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{http.StatusUnauthorized}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if got.body != early {
					t.Errorf("body = %q; want the worker's %q", got.body, early)
				}
				checkAnswerHead(t, got.head, http.StatusUnauthorized, "")
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				if n := len(g.connections()); n != 1 {
					t.Errorf("the worker got %d connections; want 1", n)
				}
				if n := len(w.waitCalls()); n != 0 {
					t.Errorf("Wait was called %d times; an answered request needs no second attempt", n)
				}
				checkLost(t, w.lostReports(), nil)
				checkLeasesClosed(t, w)
			})
		})
	}
}

// departure ends the request context of one request from inside the relay's read of the worker leg: the first Read
// that returns bytes past the first after bytes of the worker's answer marks the client writer and cancels the
// context before it returns them. A read on the in-memory pipe never spans two writes of the worker, so when the
// worker writes those first bytes and the rest separately, that read carries only the rest, and the relay holds it
// only after the client has departed, however it watches the context.
type departure struct {
	cw     *clientWriter
	cancel context.CancelFunc
	after  int64
	read   atomic.Int64
	fired  atomic.Bool
	once   sync.Once
}

// departingSource hands out the leases of inner with their reads watched by d.
type departingSource struct {
	inner workerlink.Source
	d     *departure
}

func (s departingSource) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	l, err := s.inner.Acquire(ctx, exclude)
	if err != nil {
		return l, err
	}
	return &departingLease{Lease: l, d: s.d}, nil
}

// departingLease is a lease whose reads can end the request context.
type departingLease struct {
	workerlink.Lease
	d *departure
}

func (l *departingLease) Read(p []byte) (int, error) {
	n, err := l.Lease.Read(p)
	if n > 0 && l.d.read.Add(int64(n)) > l.d.after {
		l.d.once.Do(func() {
			l.d.cw.mark()
			l.d.cancel()
			l.d.fired.Store(true)
		})
	}
	return n, err
}

// TestDepartureAtAnswerEnd decides a client that departs just as the worker's answer ends cleanly: the departure
// comes inside the relay's read of the end of the answer, before that read returns. A relayed head has committed the
// answer, so ServeHTTP must end by http.ErrAbortHandler, which breaks the client connection instead of ending the
// response as complete, and nothing more is written; a health answer, which is written only once the whole document
// was read and decoded, is not written at all. Rejected: a relay that returns normally because the worker's answer
// ended cleanly (every relayed case, the terminating chunk alone included), one that writes or flushes the bytes it
// read after the departure (the last event, the last bytes of a body), and one that answers the health request after
// the departure.
func TestDepartureAtAnswerEnd(t *testing.T) {
	const document = `{"ok":true}`
	const body = `{"content":"all but the last bytes, then the rest"}`
	healthHead := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(document)) + "\r\nConnection: close\r\n\r\n"
	const chunkedHealthHead = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n"
	bodyHead := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n"
	tests := map[string]struct {
		// health sends GET /healthz instead of a streamed POST /v1/messages.
		health bool
		// first is what the worker sends before the departure; relayed is the part of its body the client holds
		// flushed then; tail ends the answer and is read only after the departure.
		first, relayed, tail string
	}{
		"error: a stream whose terminating chunk arrives after the departure is broken": {
			first:   sseHead + chunk(sseFirst) + chunk(sseSecond),
			relayed: sseFirst + sseSecond,
			tail:    "0\r\n\r\n",
		},
		"error: a stream whose last event arrives after the departure is broken without it": {
			first:   sseHead + chunk(sseFirst),
			relayed: sseFirst,
			tail:    chunk(sseLast) + "0\r\n\r\n",
		},
		"error: a body whose last bytes arrive after the departure is broken without them": {
			first:   bodyHead + body[:len(body)-4],
			relayed: body[:len(body)-4],
			tail:    body[len(body)-4:],
		},
		"success: a health document whose last byte arrives after the departure is not answered": {
			health: true,
			first:  healthHead + document[:len(document)-1],
			tail:   document[len(document)-1:],
		},
		"success: a chunked health document whose end arrives after the departure is not answered": {
			health: true,
			first:  chunkedHealthHead + chunk(document),
			tail:   "0\r\n\r\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cw := newClientWriter()
				d := &departure{cw: cw, cancel: cancel, after: int64(len(tt.first))}
				o := w.options()
				o.Workers = departingSource{inner: w.src, d: d}
				h := newRelay(t, o)
				w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if !c.readRequest() {
						return
					}
					if !tt.health {
						c.submit()
					}
					if !c.send(tt.first) {
						return
					}
					// The limit only keeps a relay that never passes the first part on from parking the worker.
					if tt.relayed != "" && !cw.until(flushedBody(len(tt.relayed)), time.Minute) {
						t.Errorf("before the departure the client did not get %q flushed; the relay must pass on what it received", tt.relayed)
						return
					}
					c.send(tt.tail)
				})
				r := postMessages(ctx, `{"stream":true}`)
				if tt.health {
					r = clientRequest(ctx, http.MethodGet, "/healthz", nil, nil, 0)
				}
				out := await(t, serve(h, cw, r), "the relay to finish")
				synctest.Wait()
				if tt.health {
					mustEnd(t, out)
				} else {
					mustAbort(t, out)
				}
				if !d.fired.Load() {
					t.Error("the relay never read the end of the worker's answer, so the client never departed")
				}
				if n := cw.callsAfterMark(); n != 0 {
					t.Errorf("the relay made %d calls on the response after the client departed; want none", n)
				}
				got := cw.snapshot()
				if got.body != tt.relayed {
					t.Errorf("the client holds %q; want only %q, what it had before it departed", got.body, tt.relayed)
				}
				wantStatuses := []int{http.StatusOK}
				if tt.health {
					wantStatuses = nil
				}
				if diff := gocmp.Diff(wantStatuses, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				if n := len(w.src.acquireCalls()); n != 1 {
					t.Errorf("Acquire was called %d times; a departed client causes no new attempt", n)
				}
				checkLost(t, w.lostReports(), nil)
				checkReleased(t, w)
			})
		})
	}
}

// TestDepartureDuringLost decides a client that departs while the relay reports the loss of its request: the report
// stands and nothing is written on the response after it. Rejected: a relay that writes its taken-request 503 after
// the Lost callback without looking at the request context again. A relay that writes the 503 before it reports the
// loss passes here; TestLostBeforeAnswer decides that order.
func TestDepartureDuringLost(t *testing.T) {
	tests := map[string]struct{}{
		"success: a client that departs while its loss is reported gets nothing more": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cw := newClientWriter()
				o := w.options()
				o.Lost = func(l relay.Lost) {
					w.recordLost(l)
					cw.mark()
					cancel()
				}
				h := newRelay(t, o)
				w.start("gen-a", 41001, 4101, failing(true, nil))
				out := await(t, serve(h, cw, postMessages(ctx, requestBody)), "the relay to finish")
				synctest.Wait()
				mustEnd(t, out)
				checkLost(t, w.lostReports(), []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}})
				if n := cw.callsAfterMark(); n != 0 {
					t.Errorf("the relay made %d calls on the response after the client departed during the loss report; want none", n)
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				checkLeasesClosed(t, w)
			})
		})
	}
}

// TestLostBeforeAnswer decides the order of the loss report and the taken-request 503: the source reports the loss
// before it answers (lib/commands.js:2354 runs inside the rejection that lib/commands.js:2449 answers), so nothing is
// written on the response when Lost is called, and the 503 follows it. Rejected: a relay that answers first and
// reports the loss afterwards, which a departure during the report could not stop.
func TestLostBeforeAnswer(t *testing.T) {
	tests := map[string]struct{}{
		"success: the loss is reported before the 503 is written": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				cw := newClientWriter()
				var atReport []snapshot
				o := w.options()
				o.Lost = func(l relay.Lost) {
					w.recordLost(l)
					atReport = append(atReport, cw.snapshot())
				}
				h := newRelay(t, o)
				w.start("gen-a", 41001, 4101, failing(true, nil))
				out := await(t, serve(h, cw, postMessages(t.Context(), requestBody)), "the relay's answer")
				synctest.Wait()
				mustReturn(t, out)
				checkLost(t, w.lostReports(), []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}})
				for i, s := range atReport {
					if len(s.statuses) != 0 || s.body != "" || len(s.flushes) != 0 {
						t.Errorf("at loss report %d the client already held %v %q; the loss is reported before the answer", i+1, s.statuses, s.body)
					}
				}
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{http.StatusServiceUnavailable}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if got.body != lostBody {
					t.Errorf("body = %q; want %q", got.body, lostBody)
				}
				checkAnswerHead(t, got.head, http.StatusServiceUnavailable, "false")
				checkLeasesClosed(t, w)
			})
		})
	}
}

// errFlush is the failure of a flush to a client connection that broke.
var errFlush = errors.New("flush to a broken client connection failed")

// flushFailingWriter is a client writer whose every flush reports an error through FlushError, which is what
// http.ResponseController reaches. Its plain Flush and its writes keep succeeding, so the reported error is the only
// sign of the failure. The first failed flush marks the writer, so that every later call is counted.
type flushFailingWriter struct {
	*clientWriter
}

func (f flushFailingWriter) FlushError() error {
	f.Flush()
	f.mark()
	return errFlush
}

// failMarkingWriter marks the client writer at its first failed Write, so that every later call is counted.
type failMarkingWriter struct {
	*clientWriter
}

func (f failMarkingWriter) Write(p []byte) (int, error) {
	n, err := f.clientWriter.Write(p)
	if err != nil {
		f.mark()
	}
	return n, err
}

// unflushableWriter is a client writer that cannot flush: it has none of the methods http.ResponseController looks
// for, so a flush through it reports http.ErrNotSupported.
type unflushableWriter struct {
	c *clientWriter
}

func (u unflushableWriter) Header() http.Header { return u.c.Header() }

func (u unflushableWriter) Write(p []byte) (int, error) { return u.c.Write(p) }

func (u unflushableWriter) WriteHeader(code int) { u.c.WriteHeader(code) }

// TestOutputFailure decides failures of the public response: a flush that reports an error after the head and a
// write of the health document that fails end the relayed answer by http.ErrAbortHandler, with no call on the response
// after the failure, no more read from the worker than a read buffer past what it had sent by then, every lease closed
// and no Lost report. A writer that cannot flush at all is not a failure: it gets the whole answer. Rejected: a relay
// that ignores the error of http.ResponseController's Flush or flushes through http.Flusher, which hides it (it reads
// the worker's stream on), one that returns normally after a failed flush or write, one that writes again after the
// failure, one that reports either failure as a lost request, and one that takes a writer without flushing for a
// failure (aborts or answers short) or needs http.Flusher.
//
// For the health document the bound on further reads decides nothing: the relay has read the whole document before
// it writes it.
func TestOutputFailure(t *testing.T) {
	const document = `{"ok":true}`
	healthAnswer := "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(document)) + "\r\nConnection: close\r\n\r\n" + document
	tests := map[string]struct {
		run func(t *testing.T, w *world)
	}{
		"error: a flush that reports an error after the head ends the stream": {
			run: func(t *testing.T, w *world) {
				h := w.handler(t)
				cw := newClientWriter()
				var after atomic.Int64
				w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if !c.readRequest() {
						return
					}
					c.submit()
					if !c.send(sseHead + chunk(sseFirst)) {
						return
					}
					// More of the stream is sent only once the relay has flushed and seen the error; the limit only
					// keeps a relay that never flushes from parking the worker.
					if !cw.until(func(s snapshot) bool { return len(s.flushes) > 0 }, time.Minute) {
						t.Error("the relay never flushed the relayed stream")
						return
					}
					flood(c, strings.Repeat(chunk(sseSecond), 16), &after)
					if after.Load() >= floodBudget {
						c.send("0\r\n\r\n")
					}
				})
				out := await(t, serve(h, flushFailingWriter{cw}, postMessages(t.Context(), `{"stream":true}`)), "the relay to finish")
				synctest.Wait()
				mustAbort(t, out)
				if n := cw.callsAfterMark(); n != 0 {
					t.Errorf("the relay made %d calls on the response after its flush failed; want none", n)
				}
				if n := after.Load(); n > readSlack {
					t.Errorf("the relay read %d more bytes of the stream after its flush failed; want at most %d", n, readSlack)
				}
				if diff := gocmp.Diff([]int{http.StatusOK}, cw.snapshot().statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				checkOutputFailure(t, w)
			},
		},
		"error: a failed write of the health document ends the answer": {
			run: func(t *testing.T, w *world) {
				h := w.handler(t)
				w.start("gen-a", 41001, 4101, answering(healthAnswer))
				cw := newClientWriter()
				cw.failWritesFrom(1)
				out := await(t, serve(h, failMarkingWriter{cw}, clientRequest(t.Context(), http.MethodGet, "/healthz", nil, nil, 0)), "the relay to finish")
				synctest.Wait()
				mustAbort(t, out)
				got := cw.snapshot()
				if got.writes != 1 {
					t.Errorf("the relay called Write %d times; after the failed write of the document it writes nothing more", got.writes)
				}
				if n := cw.callsAfterMark(); n != 0 {
					t.Errorf("the relay made %d calls on the response after its write failed; want none", n)
				}
				if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				checkOutputFailure(t, w)
			},
		},
		"success: a writer that cannot flush gets the whole stream": {
			run: func(t *testing.T, w *world) {
				h := w.handler(t)
				w.start("gen-a", 41001, 4101, answering(sseHead+chunk(sseFirst)+chunk(sseSecond)+chunk(sseLast)+"0\r\n\r\n"))
				cw := newClientWriter()
				out := await(t, serve(h, unflushableWriter{cw}, postMessages(t.Context(), `{"stream":true}`)), "the relayed stream")
				synctest.Wait()
				mustReturn(t, out)
				checkUnflushed(t, w, cw, sseFirst+sseSecond+sseLast)
			},
		},
		"success: a writer that cannot flush gets the whole answer": {
			run: func(t *testing.T, w *world) {
				h := w.handler(t)
				w.start("gen-a", 41001, 4101, answering(okAnswer))
				cw := newClientWriter()
				out := await(t, serve(h, unflushableWriter{cw}, postMessages(t.Context(), requestBody)), "the relayed answer")
				synctest.Wait()
				mustReturn(t, out)
				checkUnflushed(t, w, cw, "ok")
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, tt.run)
		})
	}
}

// checkOutputFailure fails unless the request went to gen-a once, no loss was reported and every lease is closed once
// the fake worker is done.
func checkOutputFailure(t *testing.T, w *world) {
	t.Helper()
	if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
		t.Errorf("deliveries by generation (-want +got):\n%s", diff)
	}
	if n := len(w.src.acquireCalls()); n != 1 {
		t.Errorf("Acquire was called %d times; a failed public response causes no new attempt", n)
	}
	checkLost(t, w.lostReports(), nil)
	checkReleased(t, w)
}

// checkUnflushed fails unless the client got the worker's 200 with body, from one delivery and without a loss.
func checkUnflushed(t *testing.T, w *world, cw *clientWriter, body string) {
	t.Helper()
	got := cw.snapshot()
	if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" {
		t.Errorf("statuses written (-want +got):\n%s", diff)
	}
	if got.body != body {
		t.Errorf("body = %q; want the whole %q", got.body, body)
	}
	checkAnswerHead(t, got.head, http.StatusOK, "")
	checkOneDelivery(t, w)
	checkLost(t, w.lostReports(), nil)
}

// errStrayLease is a failure a worker source reports together with a lease.
var errStrayLease = errors.New("worker source failed after opening a connection")

// strayLeaseSource is a worker source that, on the attempts stray selects, returns the lease of inner together with
// an error, as a source that opened a connection and then failed would. Every such lease is watched by a probe that
// counts the raw bytes the relay writes to it.
type strayLeaseSource struct {
	inner workerlink.Source
	stray func(n int) bool
	calls atomic.Int32

	mu     sync.Mutex
	probes []*legProbe
}

func (s *strayLeaseSource) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	n := int(s.calls.Add(1))
	l, err := s.inner.Acquire(ctx, exclude)
	if err == nil && s.stray(n) {
		p := newLegProbe(0)
		s.mu.Lock()
		s.probes = append(s.probes, p)
		s.mu.Unlock()
		return &probedLease{Lease: l, p: p}, errStrayLease
	}
	return l, err
}

// strayProbes returns the probes of the leases returned with an error, in order.
func (s *strayLeaseSource) strayProbes() []*legProbe {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.probes)
}

// TestLeaseWithError decides a worker source that returns a lease together with an error while the request context is
// live: the relay closes that lease, sends nothing on it and treats the attempt as refused, so the next attempt
// follows after one wait with no generation excluded, and attempts that are all refused that way end in the retryable
// 503 without a loss. Not one raw byte reaches such a lease: the fake worker records a request line only once a whole
// head arrived, so the lease's own probe counts what the relay wrote to it. Rejected: a relay that uses the lease
// because it is not nil (the request reaches the worker on the first connection), one that writes any of the request
// to it before it looks at the error, one that leaves it open, one that counts it as a delivery to its generation (it
// excludes that generation or reports a loss), and one that answers at once instead of trying again.
func TestLeaseWithError(t *testing.T) {
	tests := map[string]struct {
		stray func(n int) bool
		// status and body are the public answer; conns is the number of connections to gen-a, reads the requests it
		// read and waits the number of Wait calls.
		status int
		body   string
		conns  int
		reads  int32
		waits  int
	}{
		"success: a lease returned with an error is closed and the next attempt delivers": {
			stray:  func(n int) bool { return n == 1 },
			status: http.StatusOK,
			body:   "ok",
			conns:  2,
			reads:  1,
			waits:  1,
		},
		"error: leases returned with an error in every attempt leave the failure retryable": {
			stray:  func(int) bool { return true },
			status: http.StatusServiceUnavailable,
			body:   undeliveredBody,
			conns:  attemptWaits + 1,
			waits:  attemptWaits,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				o := w.options()
				src := &strayLeaseSource{inner: w.src, stray: tt.stray}
				o.Workers = src
				h := newRelay(t, o)
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				cw := newClientWriter()
				out := await(t, serve(h, cw, postMessages(t.Context(), requestBody)), "the relay's answer")
				synctest.Wait()
				mustReturn(t, out)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{tt.status}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if got.body != tt.body {
					t.Errorf("body = %q; want %q", got.body, tt.body)
				}
				checkAnswerHead(t, got.head, tt.status, "")
				strays := src.strayProbes()
				if want := tt.conns - int(tt.reads); len(strays) != want {
					t.Errorf("%d leases were returned with an error; want %d", len(strays), want)
				}
				for i, p := range strays {
					if n := p.written.Load(); n != 0 {
						t.Errorf("the relay wrote %d raw bytes to lease %d, which came with an error; want none", n, i+1)
					}
					if !p.closed.Load() {
						t.Errorf("lease %d, which came with an error, was not closed", i+1)
					}
				}
				conns := g.connections()
				if len(conns) != tt.conns {
					t.Fatalf("gen-a got %d connections; want %d", len(conns), tt.conns)
				}
				for i, c := range conns {
					stray := i < len(conns)-1 || tt.status != http.StatusOK
					if stray && c.line != "" {
						t.Errorf("connection %d came with an error and still carried %q", i, c.line)
					}
				}
				if got := [2]int32{g.reads.Load(), w.upstream.Load()}; got != [2]int32{tt.reads, tt.reads} {
					t.Errorf("the worker read %d requests and submitted %d upstream; want %d of each", got[0], got[1], tt.reads)
				}
				for i, call := range w.src.acquireCalls() {
					if call.Exclude != "" {
						t.Errorf("Acquire call %d excluded %q; a refused attempt leaves no generation that took the request", i+1, call.Exclude)
					}
				}
				waits := w.waitCalls()
				if len(waits) != tt.waits {
					t.Errorf("Wait was called %d times; want %d", len(waits), tt.waits)
				}
				for i, d := range waits {
					if d != attemptWait {
						t.Errorf("Wait call %d paused %v; want %v", i+1, d, attemptWait)
					}
				}
				checkAlternation(t, w.callOrder())
				checkLost(t, w.lostReports(), nil)
				checkLeasesClosed(t, w)
			})
		})
	}
}

// legWriteError is a failed write on the worker leg with errno, which is not the end of the request context.
func legWriteError(errno syscall.Errno) error {
	return &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", errno)}
}

// errNoDeadline is what the deadline setters of a lease that cannot take deadlines return.
var errNoDeadline = errors.New("this connection takes no deadlines")

// deadlineMode is how the deadline setters of a probed lease behave.
type deadlineMode int

const (
	// deadlinesWork passes every deadline on to the connection.
	deadlinesWork deadlineMode = iota
	// deadlinesFail makes every setter return errNoDeadline.
	deadlinesFail
	// deadlinesIgnored makes every setter report success and do nothing.
	deadlinesIgnored
)

// legProbe watches the relay's side of one lease: the raw bytes it wrote, the writes still in progress, its closes and,
// when failAfter is positive, a write failure with failErr (EPIPE when nil) once that many bytes have gone through.
// deadlines decides what its deadline setters do.
type legProbe struct {
	failAfter int64
	failErr   error
	deadlines deadlineMode
	written   atomic.Int64
	writing   atomic.Int32
	closed    atomic.Bool
	closes    atomic.Int32
	// failed is closed at the first write the probe failed; failedAt is when that was.
	failed   chan struct{}
	failOnce sync.Once
	mu       sync.Mutex
	failedAt time.Time
	// closedAt is when the relay first closed the lease.
	closedAt time.Time
}

func newLegProbe(failAfter int64) *legProbe {
	return &legProbe{failAfter: failAfter, failErr: legWriteError(syscall.EPIPE), failed: make(chan struct{})}
}

// deadline runs set as the probe's deadline mode says.
func (p *legProbe) deadline(set func() error) error {
	switch p.deadlines {
	case deadlinesFail:
		return errNoDeadline
	case deadlinesIgnored:
		return nil
	default:
		return set()
	}
}

func (p *legProbe) fail() {
	p.failOnce.Do(func() {
		p.mu.Lock()
		p.failedAt = time.Now()
		p.mu.Unlock()
		close(p.failed)
	})
}

// failureTime returns when the probe failed a write, or the zero time.
func (p *legProbe) failureTime() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failedAt
}

// probedLease is a lease whose writes and close a legProbe watches.
type probedLease struct {
	workerlink.Lease
	p *legProbe
}

func (l *probedLease) Write(b []byte) (int, error) {
	l.p.writing.Add(1)
	defer l.p.writing.Add(-1)
	if l.p.failAfter > 0 {
		room := l.p.failAfter - l.p.written.Load()
		if room <= 0 {
			l.p.fail()
			return 0, l.p.failErr
		}
		if int64(len(b)) > room {
			n, err := l.Lease.Write(b[:room])
			l.p.written.Add(int64(n))
			if err != nil {
				return n, err
			}
			l.p.fail()
			return n, l.p.failErr
		}
	}
	n, err := l.Lease.Write(b)
	l.p.written.Add(int64(n))
	return n, err
}

func (l *probedLease) Close() error {
	l.p.mu.Lock()
	if l.p.closedAt.IsZero() {
		l.p.closedAt = time.Now()
	}
	l.p.mu.Unlock()
	l.p.closed.Store(true)
	l.p.closes.Add(1)
	return l.Lease.Close()
}

// closeTime returns when the relay first closed the lease, or the zero time.
func (p *legProbe) closeTime() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closedAt
}

func (l *probedLease) SetDeadline(t time.Time) error {
	return l.p.deadline(func() error { return l.Lease.SetDeadline(t) })
}

func (l *probedLease) SetReadDeadline(t time.Time) error {
	return l.p.deadline(func() error { return l.Lease.SetReadDeadline(t) })
}

func (l *probedLease) SetWriteDeadline(t time.Time) error {
	return l.p.deadline(func() error { return l.Lease.SetWriteDeadline(t) })
}

// returnSample is what a handler wrapped by sampleAtReturn saw on the leases of its probe source when ServeHTTP
// returned or panicked: the writes still in progress and how often each lease was closed.
type returnSample struct {
	taken   bool
	writing int32
	closes  []int32
}

// sampleAtReturn wraps h so that into records the leases of src at the moment ServeHTTP ends, however it ends.
func sampleAtReturn(h http.Handler, src *probeSource, into *returnSample) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		defer func() {
			into.taken = true
			for _, p := range src.handed() {
				into.writing += p.writing.Load()
				into.closes = append(into.closes, p.closes.Load())
			}
		}()
		h.ServeHTTP(rw, r)
	})
}

// probeSource hands out the leases of inner, each watched by a probe: the one next returns for the nth lease, or one
// that only watches when next is nil or returns nil.
type probeSource struct {
	inner workerlink.Source
	next  func(n int) *legProbe

	mu     sync.Mutex
	probes []*legProbe
}

func (s *probeSource) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	l, err := s.inner.Acquire(ctx, exclude)
	if err != nil {
		return l, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var p *legProbe
	if s.next != nil {
		p = s.next(len(s.probes) + 1)
	}
	if p == nil {
		p = newLegProbe(0)
	}
	s.probes = append(s.probes, p)
	return &probedLease{Lease: l, p: p}, nil
}

func (s *probeSource) handed() []*legProbe {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.probes)
}

// waitClock records when the relay first called Wait, the end of its first attempt; the wait itself is the world's.
type waitClock struct {
	w     *world
	mu    sync.Mutex
	first time.Time
}

func (c *waitClock) wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	if c.first.IsZero() {
		c.first = time.Now()
	}
	c.mu.Unlock()
	return c.w.wait(ctx, d)
}

func (c *waitClock) firstWait() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.first
}

// checkTakenAndFailed fails unless the answer is the taken-request 503 of a still-current generation that failed before
// its final head: X-Should-Retry: false, lostBody, one Lost report for gen-a and the attempts' waits, from one
// delivery (the frozen delivery row "a generation that is still current is not sent the request again").
func checkTakenAndFailed(t *testing.T, w *world, cw *clientWriter) {
	t.Helper()
	got := cw.snapshot()
	if diff := gocmp.Diff([]int{http.StatusServiceUnavailable}, got.statuses); diff != "" {
		t.Errorf("statuses written (-want +got):\n%s", diff)
	}
	if got.body != lostBody {
		t.Errorf("body of %d bytes (first 80: %.80q); want the taken-request answer %q", len(got.body), got.body, lostBody)
	}
	checkAnswerHead(t, got.head, http.StatusServiceUnavailable, "false")
	if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
		t.Errorf("deliveries by generation (-want +got):\n%s", diff)
	}
	if n := len(w.waitCalls()); n != attemptWaits {
		t.Errorf("Wait was called %d times; want %d", n, attemptWaits)
	}
	checkLost(t, w.lostReports(), []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}})
	checkAlternation(t, w.callOrder())
}

// TestRequestWriteFailure decides a worker leg whose write fails after part of the request, with an error that is not
// the request context's, while the worker keeps the connection open: after the failure the worker has exactly one
// second of bubble time to deliver a final head. A parsed final head wins, and its body is then relayed with no time
// limit. When no head came within the second, the attempt is a connection that took the request and failed: for a
// still-current generation the taken-request 503 with one Lost report after the attempts' waits, the first wait
// beginning exactly 1 s after the failed write, and the report's ErrorType the class of the write's error
// ("broken-pipe" for EPIPE, "connection-reset" for ECONNRESET). The relay keeps that second without relying on the
// lease's deadline setters: the rows with setters that always fail and with setters that succeed and do nothing give
// the same outcomes, and an ordinary exchange with a slow body over such a lease is relayed whole. In every row the
// lease is closed exactly once and no write on it is in progress when ServeHTTP returns.
//
// Rejected: a relay that waits for a head without a bound after a failed write, or for longer or shorter than 1 s (the
// silent rows pin the first wait at exactly 1 s, so a 750 ms grace fails; the 999 ms head rejects any grace shorter
// than the second); one that leaves a time limit armed on the body after the head won (the body at 3 s); one that
// classifies the failure by the grace ("timeout") or by the peer ("connection-closed") instead of the write's error;
// one that bounds the grace by a read deadline on the lease (the setters that fail or do nothing leave it unbounded,
// so it hangs or ends at the wrong time) or fails the attempt because a setter failed (the ordinary exchange); one that
// treats the failed write as a refused attempt (a retryable 503 without a Lost report); one that closes the lease
// twice or never; and one that returns while its request writer is still inside a write.
func TestRequestWriteFailure(t *testing.T) {
	const body = "the whole body, sent after the grace"
	head := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n"
	tests := map[string]struct {
		// errno is the error of the failed write; zero means no write fails and the worker reads the whole request.
		errno syscall.Errno
		// deadlines is what the lease's deadline setters do.
		deadlines deadlineMode
		// headAt and bodyAt, when headAt is positive, are when the worker sends the head and then the body, counted
		// from the failed write or, without one, from the end of the request.
		headAt, bodyAt time.Duration
		// errorType is the ErrorType of the Lost report of a silent row.
		errorType string
	}{
		"error: a silent worker after a write failing with EPIPE is a taken request lost as broken-pipe": {
			errno:     syscall.EPIPE,
			deadlines: deadlinesWork,
			errorType: "broken-pipe",
		},
		"error: a silent worker after a write failing with ECONNRESET is a taken request lost as connection-reset": {
			errno:     syscall.ECONNRESET,
			deadlines: deadlinesWork,
			errorType: "connection-reset",
		},
		"success: a head 999 ms after the failed write wins and its body at 3 s is relayed whole": {
			errno:     syscall.EPIPE,
			deadlines: deadlinesWork,
			headAt:    999 * time.Millisecond,
			bodyAt:    3 * time.Second,
		},
		"error: a silent worker over a lease whose deadline setters fail is lost after exactly 1 s": {
			errno:     syscall.EPIPE,
			deadlines: deadlinesFail,
			errorType: "broken-pipe",
		},
		"success: a head at 500 ms and a body at 3 s over a lease whose deadline setters fail are relayed whole": {
			errno:     syscall.EPIPE,
			deadlines: deadlinesFail,
			headAt:    500 * time.Millisecond,
			bodyAt:    3 * time.Second,
		},
		"success: an ordinary exchange with a slow body over a lease whose deadline setters fail is relayed whole": {
			deadlines: deadlinesFail,
			headAt:    time.Nanosecond,
			bodyAt:    3 * time.Second,
		},
		"error: a silent worker over a lease whose deadline setters do nothing is lost after exactly 1 s": {
			errno:     syscall.EPIPE,
			deadlines: deadlinesIgnored,
			errorType: "broken-pipe",
		},
		"success: a head at 500 ms and a body at 3 s over a lease whose deadline setters do nothing are relayed whole": {
			errno:     syscall.EPIPE,
			deadlines: deadlinesIgnored,
			headAt:    500 * time.Millisecond,
			bodyAt:    3 * time.Second,
		},
		"success: an ordinary exchange with a slow body over a lease whose deadline setters do nothing is relayed whole": {
			deadlines: deadlinesIgnored,
			headAt:    time.Nanosecond,
			bodyAt:    3 * time.Second,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				// With a failing write, sixteen bytes of the request line reach the worker and the write that would
				// carry more fails.
				probe := newLegProbe(0)
				if tt.errno != 0 {
					probe = newLegProbe(16)
					probe.failErr = legWriteError(tt.errno)
				}
				probe.deadlines = tt.deadlines
				clock := &waitClock{w: w}
				src := &probeSource{inner: w.src, next: func(n int) *legProbe {
					if n == 1 {
						return probe
					}
					return nil
				}}
				o := w.options()
				o.Workers = src
				o.Wait = clock.wait
				var sample returnSample
				h := sampleAtReturn(newRelay(t, o), src, &sample)
				w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					drained := make(chan struct{})
					var from time.Time
					if tt.errno != 0 {
						go func() {
							defer close(drained)
							_, _ = io.Copy(io.Discard, c.conn)
						}()
						select {
						case <-probe.failed:
						case <-c.g.w.done:
							c.close()
							<-drained
							return
						}
						from = probe.failureTime()
					} else {
						// readRequest leaves a goroutine of its own reading the rest of the leg.
						close(drained)
						if !c.readRequest() {
							c.close()
							return
						}
						from = time.Now()
					}
					// at waits until d after from; it reports false when the world ends first.
					at := func(d time.Duration) bool {
						select {
						case <-time.After(time.Until(from.Add(d))):
							return true
						case <-c.g.w.done:
							return false
						}
					}
					if tt.headAt > 0 && at(tt.headAt) && c.send(head) && at(tt.bodyAt) {
						c.send(body)
					}
					<-c.g.w.done
					c.close()
					<-drained
				})
				cw := newClientWriter()
				out := await(t, serve(h, cw, postMessages(t.Context(), requestBody)), "the relay's answer")
				synctest.Wait()
				mustReturn(t, out)
				failedAt := probe.failureTime()
				if tt.errno != 0 {
					if failedAt.IsZero() {
						t.Fatal("the relay never wrote more than 16 bytes of the request, so its write never failed")
					}
					if n := probe.written.Load(); n != 16 {
						t.Errorf("the worker received %d bytes of the request; want the 16 before the failure", n)
					}
				} else if !failedAt.IsZero() {
					t.Errorf("a write failed at %v although this lease fails none", failedAt)
				}
				if tt.headAt > 0 {
					got := cw.snapshot()
					if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" || got.body != body {
						t.Errorf("the client got %v %q; want the worker's 200 with its whole body %q", got.statuses, got.body, body)
					}
					checkAnswerHead(t, got.head, http.StatusOK, "")
					if n := len(w.waitCalls()); n != 0 {
						t.Errorf("Wait was called %d times; the relayed answer needs no second attempt", n)
					}
					checkLost(t, w.lostReports(), nil)
				} else {
					checkTakenAndFailed(t, w, cw)
					if first := clock.firstWait(); first.Sub(failedAt) != time.Second {
						t.Errorf("the first attempt ended %v after the failed write; want exactly 1s", first.Sub(failedAt))
					}
					if lost := w.lostReports(); len(lost) == 1 && lost[0].ErrorType != tt.errorType {
						t.Errorf("the Lost report has error type %q; want %q, the class of the failed write's error", lost[0].ErrorType, tt.errorType)
					}
				}
				if !sample.taken {
					t.Fatal("ServeHTTP's end was not sampled")
				}
				if diff := gocmp.Diff([]int32{1}, sample.closes); diff != "" {
					t.Errorf("closes of each lease when ServeHTTP returned (-want +got):\n%s", diff)
				}
				if sample.writing != 0 {
					t.Errorf("%d writes on the lease were still in progress when ServeHTTP returned", sample.writing)
				}
				if n := probe.closes.Load(); n != 1 {
					t.Errorf("the lease was closed %d times; want exactly once", n)
				}
				checkLeasesClosed(t, w)
			})
		})
	}
}

// TestDuplexWorker decides a worker that sends its final head at once, then reads the whole declared request body
// slowly in pieces and only after its last byte sends its answer body. The public head is relayed before the worker
// has finished reading the request: the worker reads no byte of the body before the client has the head's status and
// a flush of it, so a relay that holds the head back until the request is written stalls the worker, which then gives
// up. The relay keeps writing the request after the head was relayed, the worker receives every request byte and the
// client the complete answer. The control is a worker that sends its head and body at once and never reads: its answer
// is complete, ServeHTTP returns, and when it returns the lease is closed and no write on it is still in progress, so
// the request writer was joined before the return. Rejected: a relay that relays or flushes the head only once the
// request was written (the gate on the head's flush), one that stops writing the request once a final head arrived,
// or closes the leg then (the worker never gets the whole body, so it never sends its answer body), and one that
// returns while its request writer is still inside a write or before it closed the lease.
func TestDuplexWorker(t *testing.T) {
	const size = 4 * mib
	const answerBody = "complete after the whole upload"
	head := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: " + strconv.Itoa(len(answerBody)) + "\r\nConnection: close\r\n\r\n"
	tests := map[string]struct {
		// duplex makes the worker read the whole body between its head and its body; otherwise it sends both at once
		// and reads nothing.
		duplex bool
	}{
		"success: a worker that reads the whole body after its head gets every byte and its answer is relayed": {duplex: true},
		"success: a worker that answers at once and never reads is relayed and the request writer is joined":   {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				src := &probeSource{inner: w.src}
				o := w.options()
				o.Workers = src
				h := newRelay(t, o)
				var received, foreign atomic.Int64
				cw := newClientWriter()
				w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if !tt.duplex {
						if c.send(head + answerBody) {
							<-c.g.w.done
						}
						return
					}
					tp := textproto.NewReader(c.br)
					if _, err := tp.ReadLine(); err != nil {
						return
					}
					header, err := tp.ReadMIMEHeader()
					if err != nil {
						return
					}
					length, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64)
					if err != nil {
						t.Errorf("the relay sent Content-Length %q", header.Get("Content-Length"))
						return
					}
					if !c.send(head) {
						return
					}
					// The limit only keeps a relay that holds the head back from parking the worker for ever.
					if !cw.until(func(s snapshot) bool {
						return len(s.statuses) > 0 && len(s.flushes) > 0 && s.flushes[0].status == http.StatusOK
					}, time.Minute) {
						t.Error("the client had no flushed head while the worker had not read the request body; the head must be relayed first")
						return
					}
					buf := make([]byte, 64<<10)
					for received.Load() < length {
						n, err := c.br.Read(buf[:min(int64(len(buf)), length-received.Load())])
						foreign.Add(int64(n - bytes.Count(buf[:n], []byte{'d'})))
						received.Add(int64(n))
						if err != nil {
							return
						}
						// The pause paces the worker's reads; it decides nothing.
						select {
						case <-time.After(time.Millisecond):
						case <-c.g.w.done:
							return
						}
					}
					c.send(answerBody)
				})
				var writingAtReturn int32
				var closedAtReturn bool
				wrapped := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
					defer func() {
						for _, p := range src.handed() {
							writingAtReturn += p.writing.Load()
							closedAtReturn = p.closed.Load()
						}
					}()
					h.ServeHTTP(rw, r)
				})
				r := clientRequest(t.Context(), http.MethodPost, "/v1/messages", http.Header{"Content-Type": {"application/json"}}, newUpload(size, 'd'), size)
				out := await(t, serve(wrapped, cw, r), "the relayed answer")
				synctest.Wait()
				mustReturn(t, out)
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if got.body != answerBody {
					t.Errorf("body = %q; want the worker's %q", got.body, answerBody)
				}
				checkAnswerHead(t, got.head, http.StatusOK, "")
				if tt.duplex {
					if n, bad := received.Load(), foreign.Load(); n != size || bad != 0 {
						t.Errorf("the worker received %d of the %d request bytes, %d of them not the client's; want all of them", n, size, bad)
					}
				} else {
					if len(src.handed()) != 1 {
						t.Errorf("%d leases were handed out; want 1", len(src.handed()))
					}
					if writingAtReturn != 0 {
						t.Errorf("%d writes on the lease were still in progress when ServeHTTP returned; the request writer must be joined first", writingAtReturn)
					}
					if !closedAtReturn {
						t.Error("the lease was still open when ServeHTTP returned; the leg must be closed before the return")
					}
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				checkLost(t, w.lostReports(), nil)
				checkLeasesClosed(t, w)
			})
		})
	}
}

// headDepartingWriter is a client writer that ends the request context inside its first WriteHeader, after the status
// was set: the client departs between the head and the body. It counts the Write calls and body bytes that follow.
type headDepartingWriter struct {
	*clientWriter
	cancel      context.CancelFunc
	departed    atomic.Bool
	writesAfter atomic.Int32
	bytesAfter  atomic.Int64
}

func (d *headDepartingWriter) WriteHeader(code int) {
	d.clientWriter.WriteHeader(code)
	if d.departed.CompareAndSwap(false, true) {
		d.cancel()
	}
}

func (d *headDepartingWriter) Write(p []byte) (int, error) {
	if d.departed.Load() {
		d.writesAfter.Add(1)
		d.bytesAfter.Add(int64(len(p)))
	}
	return d.clientWriter.Write(p)
}

// TestDepartureBetweenHeadAndBody decides a client that departs while the relay writes the head of an answer it holds
// whole: the health answer it buffered and its own 503s. After that head no body is written, and ServeHTTP ends by
// http.ErrAbortHandler, as on the streamed path. The health document carries a failing status, so that the relay has
// to set the status by WriteHeader. Rejected: a relay that writes a buffered body after its head without looking at
// the request context again, and one that returns normally once the head was written for a departed client.
func TestDepartureBetweenHeadAndBody(t *testing.T) {
	const document = `{"ok":false}`
	healthAnswer := "HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(document)) + "\r\nConnection: close\r\n\r\n" + document
	tests := map[string]struct {
		// setup starts the world's workers; health sends GET /healthz instead of POST /v1/messages.
		setup  func(w *world)
		health bool
		lost   bool
	}{
		"error: a client that departs at the head of the buffered health answer gets no body": {
			setup:  func(w *world) { w.start("gen-a", 41001, 4101, answering(healthAnswer)) },
			health: true,
		},
		"error: a client that departs at the head of the relay's retryable 503 gets no body": {
			setup: func(*world) {},
		},
		"error: a client that departs at the head of the relay's taken-request 503 gets no body": {
			setup: func(w *world) { w.start("gen-a", 41001, 4101, failing(true, nil)) },
			lost:  true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				h := w.handler(t)
				tt.setup(w)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cw := newClientWriter()
				dw := &headDepartingWriter{clientWriter: cw, cancel: cancel}
				r := postMessages(ctx, requestBody)
				if tt.health {
					r = clientRequest(ctx, http.MethodGet, "/healthz", nil, nil, 0)
				}
				out := await(t, serve(h, dw, r), "the relay to finish")
				synctest.Wait()
				if !dw.departed.Load() {
					t.Fatal("the relay never called WriteHeader, so the client never departed")
				}
				mustAbort(t, out)
				if n, b := dw.writesAfter.Load(), dw.bytesAfter.Load(); n != 0 || b != 0 {
					t.Errorf("the relay made %d Write calls with %d body bytes after the client departed at the head; want none", n, b)
				}
				if diff := gocmp.Diff([]int{http.StatusServiceUnavailable}, cw.snapshot().statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				var want []relay.Lost
				if tt.lost {
					want = []relay.Lost{{Generation: "gen-a", WorkerPID: 4101}}
				}
				checkLost(t, w.lostReports(), want)
				checkReleased(t, w)
			})
		})
	}
}

// TestCertainHeadOverflow decides a head whose overflow is certain before any line end: 16384 counted bytes, whose
// last line ended there, and one more byte that is neither CR nor LF, after which the worker keeps the connection open
// and sends nothing. That byte starts a line that can only be counted, so the relay refuses at once, with no virtual
// time passing, as for its other 16385-byte heads. The controls are the same 16384 bytes followed by the empty line
// that ends the head, split across two writes with a pause: CR, pause, LF, or pause, CR LF. They are relayed. Rejected:
// a relay that waits for the end of the line before it counts it (it hangs on the silent worker), one that bounds that
// wait by a timer (time passes), one that counts the CR of the empty line toward the bound (the CR, pause, LF
// control), and one that refuses a head because its end came in a separate read.
func TestCertainHeadOverflow(t *testing.T) {
	exact := strings.TrimSuffix(paddedHead("HTTP/1.1 200 OK", []string{"Content-Length: 2"}, headBound, "\r\n"), "\r\n")
	if n := headCount(exact + "\r\n"); n != headBound || !strings.HasSuffix(exact, "\r\n") {
		t.Fatalf("fixture head counts %d bytes; want %d ending in CRLF", n, headBound)
	}
	tests := map[string]struct {
		// parts are written one after another after the 16384 counted bytes, with a 100 ms pause between two.
		parts []string
		// refused is set when the head must be refused at once.
		refused bool
	}{
		"error: one byte after 16384 counted bytes that starts a line is refused at once": {
			parts:   []string{"X"},
			refused: true,
		},
		"success: the CR of the empty line, a pause and its LF end a head of 16384 counted bytes": {
			parts: []string{"\r", "\nok"},
		},
		"success: a pause before the empty line of a head of 16384 counted bytes is waited for": {
			parts: []string{"", "\r\nok"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				clock := &waitClock{w: w}
				o := w.options()
				o.Wait = clock.wait
				h := newRelay(t, o)
				var lastSent atomic.Int64
				w.start("gen-a", 41001, 4101, func(c *workerConn) {
					defer close(c.finished)
					defer c.close()
					if !c.readRequest() {
						return
					}
					c.submit()
					if !c.send(exact) {
						return
					}
					for i, part := range tt.parts {
						if i > 0 {
							// The pause paces the worker; it decides nothing.
							select {
							case <-time.After(100 * time.Millisecond):
							case <-c.g.w.done:
								return
							}
						}
						if part != "" && !c.send(part) {
							return
						}
						lastSent.Store(time.Now().UnixNano())
					}
					<-c.g.w.done
				})
				cw := newClientWriter()
				out := await(t, serve(h, cw, postMessages(t.Context(), requestBody)), "the relay's answer")
				synctest.Wait()
				mustReturn(t, out)
				if !tt.refused {
					got := cw.snapshot()
					if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" || got.body != "ok" {
						t.Errorf("the client got %v %q; want the worker's 200 \"ok\"", got.statuses, got.body)
					}
					if got, want := got.head.Get("X-Pad"), padOf(exact); got != want {
						t.Errorf("X-Pad of the relayed head has %d bytes; want the worker's %d", len(got), len(want))
					}
					checkLost(t, w.lostReports(), nil)
					checkLeasesClosed(t, w)
					return
				}
				checkTakenAndFailed(t, w, cw)
				sent := time.Unix(0, lastSent.Load())
				if first := clock.firstWait(); lastSent.Load() == 0 || !first.Equal(sent) {
					t.Errorf("the first attempt ended %v after the byte that made the overflow certain; want at once", first.Sub(sent))
				}
				checkLeasesClosed(t, w)
			})
		})
	}
}

// graceEnd is one run of a worker whose final head becomes readable around the end of the grace that follows a failed
// request write, classified by its public status.
type graceEnd struct {
	// outcome is "accepted" for the worker's 200, "expired" for the relay's 503 and "mixed" for anything else.
	outcome string
	// problems lists every observation that does not belong to the outcome.
	problems []string
}

// TestHeadAtGraceEnd decides the end of the one-second grace after a failed request write and the acceptance of a final
// head as one decision with exactly two outcomes. Either the head is accepted: the client gets the worker's 200 and its
// whole body although the body comes 2 s after the grace, the lease is closed only after the body ended, the handler
// returns, and nothing is reported lost. Or the grace expired: the taken-request 503 (X-Should-Retry: false, lostBody,
// one Lost report as "broken-pipe", the 80 waits) and no byte of the worker's head or body reaches the client. The
// write fails with EPIPE and the head becomes readable at exactly 1 s of bubble time after it, written whole at that
// instant or with its last byte arriving then. Either side may win at that instant, so each staging runs 200 times in
// bubbles of their own and every run must be one outcome whole; the failure message counts the runs that ended each way.
// The neighbours are fixed: a head 1 ns before the end of the second is always accepted, one 1 ns after it always
// expired, each over 20 runs.
//
// Rejected: a relay that parses the head, is preempted before it records the acceptance and lets the grace timer close
// the leg (the 200 with a cut body or an abort, or the lease closed before the body ended); one that reports the loss
// and still relays the answer; one that writes the worker's status and then its own 503 or nothing; one that accepts a
// head after the second has passed (the 1 ns late control); and one whose grace is shorter than the second (the 1 ns
// early control).
func TestHeadAtGraceEnd(t *testing.T) {
	const body = "the whole body, two seconds after the grace"
	head := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nX-Worker-Head: yes\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n"
	tests := map[string]struct {
		// parts are written at the matching offsets after the failed write; the body follows at 3 s.
		parts []string
		at    []time.Duration
		// want is the only outcome allowed, or empty when either outcome is.
		want string
		runs int
	}{
		"success: a head written whole as the grace ends is one outcome whole in every run": {
			parts: []string{head},
			at:    []time.Duration{time.Second},
			runs:  200,
		},
		"success: a head whose last byte arrives as the grace ends is one outcome whole in every run": {
			parts: []string{head[:len(head)-1], head[len(head)-1:]},
			at:    []time.Duration{900 * time.Millisecond, time.Second},
			runs:  200,
		},
		"success: a head 1 ns before the grace ends is always accepted": {
			parts: []string{head},
			at:    []time.Duration{time.Second - time.Nanosecond},
			want:  "accepted",
			runs:  20,
		},
		"error: a head 1 ns after the grace ended is always refused": {
			parts: []string{head},
			at:    []time.Duration{time.Second + time.Nanosecond},
			want:  "expired",
			runs:  20,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			counts := map[string]int{}
			var first []string
			for run := range tt.runs {
				var r graceEnd
				inBubble(t, func(t *testing.T, w *world) { r = runGraceEnd(t, w, head, body, tt.parts, tt.at) })
				if t.Failed() {
					// The harness or the relay's construction failed; the run decides nothing more.
					return
				}
				if tt.want != "" && r.outcome != tt.want {
					r.problems = append(r.problems, fmt.Sprintf("the outcome is %s; this neighbour of the grace's end is always %s", r.outcome, tt.want))
				}
				if len(r.problems) > 0 {
					counts[r.outcome+" with problems"]++
					if first == nil {
						first = append([]string{fmt.Sprintf("run %d (%s):", run+1, r.outcome)}, r.problems...)
					}
					continue
				}
				counts[r.outcome]++
			}
			if counts["accepted"]+counts["expired"] != tt.runs {
				t.Errorf("of %d runs %d were accepted whole, %d expired whole, and the rest were not one outcome (%v); first such run:\n%s",
					tt.runs, counts["accepted"], counts["expired"], counts, strings.Join(first, "\n"))
			}
		})
	}
}

// runGraceEnd runs one request in w whose write fails with EPIPE after 16 bytes; the worker then writes parts at the
// offsets at after the failure and body at 3 s, and keeps the connection open. It returns the classified outcome.
func runGraceEnd(t *testing.T, w *world, head, body string, parts []string, at []time.Duration) graceEnd {
	probe := newLegProbe(16)
	probe.failErr = legWriteError(syscall.EPIPE)
	src := &probeSource{inner: w.src, next: func(n int) *legProbe {
		if n == 1 {
			return probe
		}
		return nil
	}}
	o := w.options()
	o.Workers = src
	h := newRelay(t, o)
	var bodyDone atomic.Int64
	w.start("gen-a", 41001, 4101, func(c *workerConn) {
		defer close(c.finished)
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			_, _ = io.Copy(io.Discard, c.conn)
		}()
		defer func() {
			c.close()
			<-drained
		}()
		select {
		case <-probe.failed:
		case <-c.g.w.done:
			return
		}
		from := probe.failureTime()
		until := func(d time.Duration) bool {
			select {
			case <-time.After(time.Until(from.Add(d))):
				return true
			case <-c.g.w.done:
				return false
			}
		}
		for i, part := range parts {
			if !until(at[i]) || !c.send(part) {
				<-c.g.w.done
				return
			}
		}
		if until(3*time.Second) && c.send(body) {
			bodyDone.Store(time.Now().UnixNano())
		}
		<-c.g.w.done
	})
	cw := newClientWriter()
	out := await(t, serve(h, cw, postMessages(t.Context(), requestBody)), "the relay's answer")
	synctest.Wait()
	got := cw.snapshot()
	var r graceEnd
	problem := func(format string, args ...any) { r.problems = append(r.problems, fmt.Sprintf(format, args...)) }
	if out.panicked != nil {
		problem("ServeHTTP ended with %v instead of returning", out.panicked)
	}
	if n := probe.closes.Load(); n != 1 {
		problem("the lease was closed %d times; want exactly once", n)
	}
	lost := w.lostReports()
	waits := len(w.waitCalls())
	switch {
	case slices.Equal(got.statuses, []int{http.StatusOK}):
		r.outcome = "accepted"
		if got.body != body {
			problem("the accepted answer reached the client as %q; want its whole body %q", got.body, body)
		}
		if done := bodyDone.Load(); done == 0 {
			problem("the worker could not send its body after the accepted head")
		} else if closedAt := probe.closeTime(); closedAt.Before(time.Unix(0, done)) {
			problem("the lease was closed %v after the failed write, before the body ended at %v", closedAt.Sub(probe.failureTime()), time.Unix(0, done).Sub(probe.failureTime()))
		}
		if len(lost) != 0 {
			problem("an accepted answer came with %d Lost reports %+v", len(lost), lost)
		}
		if waits != 0 {
			problem("an accepted answer came with %d waits", waits)
		}
	case slices.Equal(got.statuses, []int{http.StatusServiceUnavailable}):
		r.outcome = "expired"
		if got.body != lostBody {
			problem("the expired attempt answered %q; want the taken-request body", got.body)
		}
		if v := got.head.Values("X-Should-Retry"); !slices.Equal(v, []string{"false"}) {
			problem("the expired attempt answered X-Should-Retry %q; want \"false\"", v)
		}
		if v, ok := got.head["X-Worker-Head"]; ok {
			problem("the expired attempt carries the worker's head field X-Worker-Head %q", v)
		}
		if len(lost) != 1 || lost[0].Generation != "gen-a" || lost[0].WorkerPID != 4101 || lost[0].ErrorType != "broken-pipe" {
			problem("the expired attempt reported %+v; want one Lost report for gen-a, pid 4101, as broken-pipe", lost)
		}
		if waits != attemptWaits {
			problem("the expired attempt waited %d times; want %d", waits, attemptWaits)
		}
	default:
		r.outcome = "mixed"
		problem("the client got statuses %v with %d body bytes (first 80: %.80q)", got.statuses, len(got.body), got.body)
	}
	return r
}
