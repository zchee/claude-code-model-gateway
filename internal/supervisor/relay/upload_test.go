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
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// mib is one mebibyte.
const mib = 1 << 20

// checkNotForwarded fails when anything of the request reached a worker or was reported lost: an upload that was not
// read completely is never relayed, and an upload failure is not a worker failure (test/shim-request.test.js:60-61).
func checkNotForwarded(t *testing.T, w *world, g *generation) {
	t.Helper()
	if calls := w.src.acquireCalls(); len(calls) != 0 {
		t.Errorf("the relay asked for a worker %d times; an upload that was not read completely never reaches one", len(calls))
	}
	if n := g.reads.Load(); n != 0 {
		t.Errorf("the worker read %d requests; want none", n)
	}
	checkLost(t, w.lostReports(), nil)
}

// TestUploadFailure decides a request body that cannot be read completely, and a complete one whose client has gone
// (lib/commands.js:2267-2283,2311-2323). A departure (a reset, an early end, a cancelled read, or any failure once the
// request context has ended) gets nothing written; any other read failure while the client is there gets the fixed,
// non-retryable 400 without the failure's own text. None of them reaches a worker.
func TestUploadFailure(t *testing.T) {
	private := errors.New("private body-read detail")
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	tests := map[string]struct {
		// body builds the request body; cancel ends the request context.
		body func(cancel context.CancelCauseFunc) io.Reader
		// gone ends the request context before the relay runs.
		gone   error
		status int
	}{
		"error: an aborted request is discarded without an answer": {
			body: func(cancel context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: io.ErrUnexpectedEOF, before: func() { cancel(context.Canceled) }}
			},
		},
		"error: a connection reset is discarded without an answer": {
			body: func(context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: reset}
			},
		},
		"error: a premature close is discarded without an answer": {
			body: func(context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: io.ErrUnexpectedEOF}
			},
		},
		"error: a cancelled read is discarded without an answer": {
			body: func(context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: context.Canceled}
			},
		},
		"error: a read failure after the response was destroyed is discarded without an answer": {
			body: func(context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: private}
			},
			gone: context.Canceled,
		},
		"error: a read failure after the response ended is discarded without an answer": {
			body: func(cancel context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: private, before: func() { cancel(context.Canceled) }}
			},
		},
		"error: a read failure after the response socket closed is discarded without an answer": {
			body: func(context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: private}
			},
			gone: net.ErrClosed,
		},
		"error: a read failure on a closed connection is discarded without an answer": {
			body: func(context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: fmt.Errorf("read tcp: %w", net.ErrClosed)}
			},
		},
		"error: another read failure while the client is there answers a fixed client error": {
			body: func(context.CancelCauseFunc) io.Reader {
				return &failingBody{prefix: []byte("partial upload"), err: private}
			},
			status: http.StatusBadRequest,
		},
		"error: a completed upload is not forwarded to a destroyed response": {
			body: func(context.CancelCauseFunc) io.Reader { return strings.NewReader("complete") },
			gone: context.Canceled,
		},
		"error: a completed upload is not forwarded once the response socket closed": {
			body: func(cancel context.CancelCauseFunc) io.Reader {
				u := newUpload(8, 'c')
				u.hook = func() error {
					cancel(net.ErrClosed)
					return io.EOF
				}
				return u
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				if tt.gone != nil {
					cancel(tt.gone)
				}
				r := clientRequest(ctx, http.MethodPost, "/v1/messages", http.Header{"Content-Type": {"application/json"}}, tt.body(cancel), -1)
				cw := newClientWriter()
				o := await(t, serve(w.handler(t), cw, r), "the relay to finish the request")
				synctest.Wait()
				got := cw.snapshot()
				checkNotForwarded(t, w, g)
				if tt.status == 0 {
					mustEnd(t, o)
					if len(got.statuses) != 0 || got.body != "" {
						t.Errorf("the relay wrote %v %q for a departed client; want nothing", got.statuses, got.body)
					}
					return
				}
				mustReturn(t, o)
				if diff := gocmp.Diff([]int{tt.status}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				want := http.Header{"Connection": {"close"}, "Content-Type": {"application/json"}, "X-Should-Retry": {"false"}}
				if diff := gocmp.Diff(want, pick(got.head, "Connection", "Content-Type", "X-Should-Retry")); diff != "" {
					t.Errorf("head of the 400 (-want +got):\n%s", diff)
				}
				if got.body != unreadableBody {
					t.Errorf("body = %q; want %q", got.body, unreadableBody)
				}
				if strings.Contains(got.body, private.Error()) {
					t.Errorf("the answer leaks the read failure: %q", got.body)
				}
			})
		})
	}
}

// pick returns the named fields of h.
func pick(h http.Header, names ...string) http.Header {
	out := make(http.Header)
	for _, name := range names {
		if v, ok := h[textproto.CanonicalMIMEHeaderKey(name)]; ok {
			out[textproto.CanonicalMIMEHeaderKey(name)] = v
		}
	}
	return out
}

// oversizeHead is the head of the source's oversize answer (lib/commands.js:2287).
var oversizeHead = http.Header{"Connection": {"close"}, "Content-Length": {"0"}, "X-Should-Retry": {"false"}}

// TestOversizedUpload decides a body above 64 MiB: a 413 without a body that tells the client not to resend, flushed
// at once, and the upload read on to its end rather than cut, so that the 413 can arrive; a departed client gets
// nothing. No worker is asked for (test/shim-request.test.js:179-191, test/supervisor-security.test.js:70-84).
func TestOversizedUpload(t *testing.T) {
	tests := map[string]struct {
		size     int64
		length   int64
		departed bool
	}{
		"error: one byte above 64 MiB is refused": {
			size:   64*mib + 1,
			length: 64*mib + 1,
		},
		"error: one byte above 64 MiB from a departed client writes nothing": {
			size:     64*mib + 1,
			length:   64*mib + 1,
			departed: true,
		},
		"error: a chunked upload of 128 MiB is refused and read to its end": {
			size:   128 * mib,
			length: -1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.departed {
					cancel()
				}
				body := newUpload(tt.size, 'x')
				cw := newClientWriter()
				cw.readDeadline = body.SetReadDeadline
				o := await(t, serve(w.handler(t), cw, clientRequest(ctx, http.MethodPost, "/v1/messages", nil, body, tt.length)), "the relay to finish the oversized request")
				synctest.Wait()
				checkNotForwarded(t, w, g)
				got := cw.snapshot()
				if tt.departed {
					mustEnd(t, o)
					if o.readsAtExit != 0 {
						t.Errorf("%d reads of the upload were still in progress when ServeHTTP ended", o.readsAtExit)
					}
					if len(got.statuses) != 0 || got.body != "" {
						t.Errorf("the relay wrote %v %q to a departed client; want nothing", got.statuses, got.body)
					}
					return
				}
				mustReturn(t, o)
				if diff := gocmp.Diff([]int{http.StatusRequestEntityTooLarge}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(oversizeHead, pick(got.head, "Connection", "Content-Length", "X-Should-Retry", "Content-Type")); diff != "" {
					t.Errorf("head of the 413 (-want +got):\n%s", diff)
				}
				if got.body != "" {
					t.Errorf("the 413 carries a body of %d bytes; want none", len(got.body))
				}
				if len(got.flushes) == 0 || got.flushes[0].status != http.StatusRequestEntityTooLarge {
					t.Errorf("flushes %+v; the 413 head must be flushed", got.flushes)
				}
				if o.readsAtExit != 0 {
					t.Errorf("%d reads of the upload were still in progress when ServeHTTP ended", o.readsAtExit)
				}
				if !body.ended.Load() || body.read.Load() != tt.size {
					t.Errorf("the relay read %d of %d upload bytes (ended %v); an upload that ends is read to its end", body.read.Load(), tt.size, body.ended.Load())
				}
			})
		})
	}
}

// TestOversizeGrace decides how long an oversized upload is read after the 413 head was flushed: until the upload
// ends, but no longer than 250 ms, after which the client connection is broken (lib/commands.js:2288-2294,
// test/supervisor-security.test.js:86-103). The bound is decided at one nanosecond on each side. A relay may bound
// its read by a read deadline (http.ResponseController reaches the request's connection) or by its own timer; the
// body supports both, and a Read parked when ServeHTTP has ended must end when the body is closed.
func TestOversizeGrace(t *testing.T) {
	tests := map[string]struct {
		// endsAfter is when the upload ends after its first 64 MiB and one byte; zero means it never ends.
		endsAfter time.Duration
	}{
		"error: an upload still running is cut off when the 250 ms grace ends": {},
		"success: an upload that ends within the grace finishes the 413 normally": {
			endsAfter: 100 * time.Millisecond,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				release := make(chan struct{})
				defer close(release)
				body := newUpload(64*mib+1, 'x')
				body.endsAfter, body.open, body.release = tt.endsAfter, tt.endsAfter == 0, release
				cw := newClientWriter()
				cw.readDeadline = body.SetReadDeadline
				start := time.Now()
				done := serve(w.handler(t), cw, clientRequest(t.Context(), http.MethodPost, "/v1/messages", nil, body, -1))
				synctest.Wait()
				got := cw.snapshot()
				if len(got.flushes) == 0 || got.flushes[0].status != http.StatusRequestEntityTooLarge || !got.flushes[0].at.Equal(start) {
					t.Fatalf("flushes %+v; want the 413 head flushed at once, before the grace", got.flushes)
				}
				if diff := gocmp.Diff(oversizeHead, pick(got.head, "Connection", "Content-Length", "X-Should-Retry")); diff != "" {
					t.Errorf("head of the 413 (-want +got):\n%s", diff)
				}
				var o outcome
				if tt.endsAfter > 0 {
					o = await(t, done, "the end of the 413")
					mustReturn(t, o)
					if got := o.at.Sub(start); got != tt.endsAfter {
						t.Errorf("the 413 finished %v after its head; want %v, when the upload ended", got, tt.endsAfter)
					}
				} else {
					time.Sleep(250*time.Millisecond - time.Nanosecond)
					synctest.Wait()
					if o, ok := ready(done); ok {
						t.Fatalf("the relay ended the 413 after %v (%v); the client has 250 ms to read it", o.at.Sub(start), o.panicked)
					}
					time.Sleep(time.Nanosecond)
					synctest.Wait()
					var ok bool
					o, ok = ready(done)
					if !ok {
						t.Fatal("the relay still reads the oversized upload when the 250 ms grace has ended")
					}
					mustAbort(t, o)
					if got := o.at.Sub(start); got != 250*time.Millisecond {
						t.Errorf("the relay ended the 413 %v after its head; want exactly 250ms", got)
					}
				}
				// The relay kept reading after the 413 head, and none of its Reads outlives ServeHTTP: net/http forbids
				// reading the body after the handler returned, so a reader left parked is not released by anyone it may
				// count on. The body's close by the public listener comes after this count.
				if n := body.parked.Load(); n == 0 {
					t.Error("the relay never read past the first 64 MiB and one byte of the upload; it must keep reading after the 413")
				}
				if o.readsAtExit != 0 {
					t.Errorf("%d reads of the upload were still in progress when ServeHTTP ended", o.readsAtExit)
				}
				checkNotForwarded(t, w, g)
			})
		})
	}
}

// TestForwardsCompletedUpload decides that a completed upload of 0 bytes, of text with a NUL and multibyte UTF-8, and
// of exactly 64 MiB reaches the worker byte for byte with its method and path, and that the worker's answer reaches
// the client (test/shim-request.test.js:268-297).
func TestForwardsCompletedUpload(t *testing.T) {
	tests := map[string]struct {
		body func() io.Reader
		size int64
		// want checks the bytes the worker received.
		want func(got []byte) bool
	}{
		"success: a 0-byte upload is forwarded unchanged": {
			body: func() io.Reader { return bytes.NewReader(nil) },
			size: 0,
			want: func(got []byte) bool { return len(got) == 0 },
		},
		"success: a 26-byte upload is forwarded unchanged": {
			body: func() io.Reader { return strings.NewReader("byte-preserving\x00雪 upload") },
			size: 26,
			want: func(got []byte) bool { return string(got) == "byte-preserving\x00雪 upload" },
		},
		"success: a 67108864-byte upload is forwarded unchanged": {
			body: func() io.Reader { return newUpload(64*mib, 'x') },
			size: 64 * mib,
			want: func(got []byte) bool { return len(got) == 64*mib && bytes.Count(got, []byte{'x'}) == 64*mib },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				cw := newClientWriter()
				o := await(t, serve(w.handler(t), cw, clientRequest(t.Context(), http.MethodPost, "/v1/messages", nil, tt.body(), tt.size)), "the relayed answer")
				synctest.Wait()
				mustReturn(t, o)
				conns := g.connections()
				if len(conns) != 1 {
					t.Fatalf("the worker got %d connections; want 1", len(conns))
				}
				c := conns[0]
				if c.line != "POST /v1/messages HTTP/1.1" {
					t.Errorf("request line = %q; want the method POST and the path /v1/messages", c.line)
				}
				if int64(len(c.body)) != tt.size || !tt.want(c.body) {
					t.Errorf("the worker received %d bytes that differ from the %d-byte upload", len(c.body), tt.size)
				}
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" {
					t.Errorf("statuses written (-want +got):\n%s", diff)
				}
				if got.body != "ok" {
					t.Errorf("body = %q; want \"ok\"", got.body)
				}
				if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
					t.Errorf("deliveries by generation (-want +got):\n%s", diff)
				}
				checkLeasesClosed(t, w)
			})
		})
	}
}

// TestWorkerLeg decides the request the worker receives: the client's method and target byte for byte, the lease's
// authority as Host, the framing of the buffered body with Connection: close, the client's own fields (the control
// token included, which the worker checks itself for a relayed drain) and none of the fields that describe only the
// client's connection (lib/commands.js:2373-2376).
func TestWorkerLeg(t *testing.T) {
	token := strings.Repeat("ab", 32)
	tests := map[string]struct {
		method, target string
		header         http.Header
		body           string
		length         int64
		line           string
		want           textproto.MIMEHeader
	}{
		"success: a POST carries its target, the lease authority, its own fields and a framed body": {
			method: http.MethodPost,
			target: "/v1/messages?beta=true",
			header: http.Header{
				"Content-Type":                  {"application/json"},
				"Anthropic-Version":             {"2023-06-01"},
				"X-Model-Gateway-Control-Token": {token},
				"User-Agent":                    {"claude-cli/2.1.0"},
				"X-Multi":                       {"first", "second"},
				"Connection":                    {"keep-alive"},
				"Keep-Alive":                    {"timeout=5"},
				"Expect":                        {"100-continue"},
				"Trailer":                       {"X-Checksum"},
				"Transfer-Encoding":             {"chunked"},
			},
			body:   "hello",
			length: -1,
			line:   "POST /v1/messages?beta=true HTTP/1.1",
			want: textproto.MIMEHeader{
				"Host":                          {"127.0.0.1:41001"},
				"Content-Type":                  {"application/json"},
				"Anthropic-Version":             {"2023-06-01"},
				"X-Model-Gateway-Control-Token": {token},
				"User-Agent":                    {"claude-cli/2.1.0"},
				"X-Multi":                       {"first", "second"},
				"Content-Length":                {"5"},
				"Connection":                    {"close"},
			},
		},
		"success: an empty POST says its length is zero": {
			method: http.MethodPost,
			target: "/drain",
			header: http.Header{"X-Model-Gateway-Control-Token": {token}},
			line:   "POST /drain HTTP/1.1",
			want: textproto.MIMEHeader{
				"Host":                          {"127.0.0.1:41001"},
				"X-Model-Gateway-Control-Token": {token},
				"Content-Length":                {"0"},
				"Connection":                    {"close"},
			},
		},
		"success: a bodyless GET to an unexpected path is forwarded without a length": {
			method: http.MethodGet,
			target: "/unexpected-probe",
			line:   "GET /unexpected-probe HTTP/1.1",
			want: textproto.MIMEHeader{
				"Host":       {"127.0.0.1:41001"},
				"Connection": {"close"},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T, w *world) {
				g := w.start("gen-a", 41001, 4101, answering(okAnswer))
				cw := newClientWriter()
				r := clientRequest(t.Context(), tt.method, tt.target, tt.header, strings.NewReader(tt.body), tt.length)
				o := await(t, serve(w.handler(t), cw, r), "the relayed answer")
				synctest.Wait()
				mustReturn(t, o)
				conns := g.connections()
				if len(conns) != 1 {
					t.Fatalf("the worker got %d connections; want 1", len(conns))
				}
				c := conns[0]
				if c.line != tt.line {
					t.Errorf("request line = %q; want %q", c.line, tt.line)
				}
				if diff := gocmp.Diff(tt.want, c.header); diff != "" {
					t.Errorf("fields on the worker leg (-want +got):\n%s", diff)
				}
				if string(c.body) != tt.body {
					t.Errorf("body on the worker leg = %q; want %q", c.body, tt.body)
				}
				if n := c.bytesAfter.Load(); n != 0 {
					t.Errorf("the relay sent %d bytes after the request; the leg carries one request", n)
				}
				got := cw.snapshot()
				if diff := gocmp.Diff([]int{http.StatusOK}, got.statuses); diff != "" || got.body != "ok" {
					t.Errorf("the client got %v %q; want the worker answer 200 \"ok\"", got.statuses, got.body)
				}
				checkOneDelivery(t, w)
			})
		})
	}
}
