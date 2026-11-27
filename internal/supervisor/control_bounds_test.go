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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

const (
	// ctlDirectPort is the bound port the directly called dispatchers are built for.
	ctlDirectPort = 18764
	// ctlStopReadLimit is the most entity bytes the dispatcher may take from a stop body: the bound plus the one byte
	// that shows the entity is over it.
	ctlStopReadLimit = ctlStopBodyLimit + 1
	// ctlReadErrorStatus is the answer to an authenticated stop whose entity could not be read to its end. The frozen
	// stop tests answer every authenticated entity that is neither an accepted empty object nor over the bound with
	// an empty 400, a truncated object included; an entity cut short by a failing read is not an empty object either.
	ctlReadErrorStatus = http.StatusBadRequest
)

// ctlEffectPanic is the value the failing stop effect panics with.
const ctlEffectPanic = "the shutdown request could not be recorded"

// errCtlBodyFailure is the read error a probe body reports in place of the end of its entity.
var errCtlBodyFailure = errors.New("the client's connection failed in the middle of the entity")

// ctlProbeBody is a request body that hands out data and records every Read: how many calls, how many bytes they
// asked for and how many bytes it handed out. After data it reports end, or io.EOF when end is nil; with
// endWithLast it reports end together with the bytes that complete data, otherwise on the call after them.
type ctlProbeBody struct {
	data []byte
	// perRead caps the bytes one Read hands out; zero fills the caller's whole buffer.
	perRead     int
	end         error
	endWithLast bool

	mu    sync.Mutex
	off   int
	calls int
	asked int
}

// endOfEntity is what the body reports after its data.
func (b *ctlProbeBody) endOfEntity() error {
	if b.end == nil {
		return io.EOF
	}
	return b.end
}

func (b *ctlProbeBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.asked += len(p)
	if len(p) == 0 {
		return 0, nil
	}
	if b.off >= len(b.data) {
		return 0, b.endOfEntity()
	}
	n := len(p)
	if b.perRead > 0 {
		n = min(n, b.perRead)
	}
	n = copy(p[:n], b.data[b.off:])
	b.off += n
	if b.off == len(b.data) && b.endWithLast {
		return n, b.endOfEntity()
	}
	return n, nil
}

func (*ctlProbeBody) Close() error { return nil }

// ctlProbeUse is what a probe body saw of its reader.
type ctlProbeUse struct{ Calls, Asked, Handed int }

func (b *ctlProbeBody) use() ctlProbeUse {
	b.mu.Lock()
	defer b.mu.Unlock()
	return ctlProbeUse{Calls: b.calls, Asked: b.asked, Handed: b.off}
}

// ctlPadded is an empty JSON object of exactly n bytes, padded inside with JSON white space.
func ctlPadded(n int) []byte {
	return []byte("{" + strings.Repeat(" ", n-2) + "}")
}

// ctlDirectRequest builds a request as the HTTP server hands it to a handler: the raw target in RequestURI, the wire
// Host and the parsed header. A negative length is an entity of unknown length, which on the wire is chunked.
func ctlDirectRequest(t *testing.T, method, target string, header http.Header, body io.ReadCloser, length int64) *http.Request {
	t.Helper()
	u, err := url.ParseRequestURI(target)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{
		Method: method, URL: u, RequestURI: target, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: header, Body: body, ContentLength: length, Host: "127.0.0.1:" + strconv.Itoa(ctlDirectPort),
		RemoteAddr: "127.0.0.1:50123",
	}
	if length < 0 {
		req.TransferEncoding = []string{"chunked"}
	} else {
		header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	return req.WithContext(t.Context())
}

// ctlStopHeader is the header of an authenticated stop: this root's token and the exact JSON content type.
func ctlStopHeader() http.Header {
	return http.Header{"Content-Type": {"application/json"}, ctlTokenName: {ctlToken}}
}

// ctlDirectStop is an authenticated stop request carrying body, of the given length.
func ctlDirectStop(t *testing.T, body io.ReadCloser, length int64) *http.Request {
	t.Helper()
	return ctlDirectRequest(t, http.MethodPost, "/internal/stop", ctlStopHeader(), body, length)
}

// ctlDirectIdentity is the identity challenge for nonce.
func ctlDirectIdentity(t *testing.T, nonce string) *http.Request {
	t.Helper()
	return ctlDirectRequest(t, http.MethodGet, "/internal/identity", http.Header{ctlNonceName: {nonce}}, http.NoBody, 0)
}

// ctlDirectGateway builds a dispatcher for ctlDirectPort whose callbacks count into a fixture of its own.
func ctlDirectGateway(t *testing.T, stopHook func()) (*ctlFixture, http.Handler) {
	t.Helper()
	f := &ctlFixture{stopHook: stopHook}
	h, err := ctlNewGateway(t, ctlOptions(f, ctlDirectPort))
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return f, h
}

// ctlCall hands one request to the dispatcher and returns the recorded answer; a panic fails the test.
func ctlCall(t *testing.T, h http.Handler, req *http.Request) (ctlAnswer, []byte) {
	t.Helper()
	rec := ctlNoPanic(t, "ServeHTTP", func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	})
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	return ctlAnswer{StatusCode: resp.StatusCode, Header: resp.Header}, rec.Body.Bytes()
}

// ctlRefusal is the part of an answer a refusal is decided by, with the dispatcher's side effects.
type ctlRefusal struct {
	Status int
	Body   string
	Stops  int64
	Relays int64
}

// ctlCheckRefused decides an empty refusal with status that invoked neither Stop nor the relay.
func ctlCheckRefused(t *testing.T, f *ctlFixture, resp ctlAnswer, body []byte, status int) {
	t.Helper()
	got := ctlRefusal{Status: resp.StatusCode, Body: string(body), Stops: f.stops.Load(), Relays: f.relays.Load()}
	if diff := gocmp.Diff(ctlRefusal{Status: status}, got); diff != "" {
		t.Errorf("refusal (-want +got):\n%s", diff)
	}
}

// ctlCheckWithinReadLimit fails if the dispatcher took more bytes from the body than the bound lets it.
func ctlCheckWithinReadLimit(t *testing.T, body *ctlProbeBody) {
	t.Helper()
	if u := body.use(); u.Handed > ctlStopReadLimit {
		t.Errorf("the dispatcher took %d entity bytes in %d reads asking for %d; want at most %d", u.Handed, u.Calls, u.Asked, ctlStopReadLimit)
	}
}

// TestControlStopBodyReadBound decides how far the dispatcher reads a stop entity of unknown length: it takes at
// most the bound plus one byte from the body, however the body hands its bytes out and however much more it holds,
// accepts an empty object that fills the bound and refuses one byte more with an empty 413.
func TestControlStopBodyReadBound(t *testing.T) {
	const far = 1 << 20
	tests := map[string]struct {
		body       *ctlProbeBody
		wantStatus int
	}{
		"success: an empty object that fills the bound, read with whole buffers": {
			body: &ctlProbeBody{data: ctlPadded(ctlStopBodyLimit)}, wantStatus: http.StatusAccepted,
		},
		"success: an empty object that fills the bound, read one byte at a time": {
			body: &ctlProbeBody{data: ctlPadded(ctlStopBodyLimit), perRead: 1}, wantStatus: http.StatusAccepted,
		},
		"success: an empty object that fills the bound, its end reported with the last byte": {
			body: &ctlProbeBody{data: ctlPadded(ctlStopBodyLimit), endWithLast: true}, wantStatus: http.StatusAccepted,
		},
		"error: an empty object one byte over the bound": {
			body: &ctlProbeBody{data: ctlPadded(ctlStopReadLimit)}, wantStatus: http.StatusRequestEntityTooLarge,
		},
		"error: an empty object one byte over the bound, read one byte at a time": {
			body: &ctlProbeBody{data: ctlPadded(ctlStopReadLimit), perRead: 1}, wantStatus: http.StatusRequestEntityTooLarge,
		},
		"error: an entity far over the bound, read with whole buffers": {
			body: &ctlProbeBody{data: ctlPadded(far)}, wantStatus: http.StatusRequestEntityTooLarge,
		},
		"error: an entity far over the bound, read one byte at a time": {
			body: &ctlProbeBody{data: ctlPadded(far), perRead: 1}, wantStatus: http.StatusRequestEntityTooLarge,
		},
		"error: an entity far over the bound, read in uneven pieces": {
			body: &ctlProbeBody{data: ctlPadded(far), perRead: 1000}, wantStatus: http.StatusRequestEntityTooLarge,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, h := ctlDirectGateway(t, nil)
			resp, body := ctlCall(t, h, ctlDirectStop(t, tt.body, -1))
			ctlCheckWithinReadLimit(t, tt.body)
			if tt.wantStatus == http.StatusAccepted {
				ctlCheckStopped(t, resp, body)
				if diff := gocmp.Diff(ctlStopBodyLimit, tt.body.use().Handed); diff != "" {
					t.Errorf("entity bytes taken before accepting (-want +got):\n%s", diff)
				}
				if n := f.stops.Load(); n != 1 {
					t.Errorf("Stop invoked %d times; want 1", n)
				}
				return
			}
			ctlCheckRefused(t, f, resp, body, tt.wantStatus)
		})
	}
}

// TestControlStopBodyCountsBeforeTheReadError decides the order of the two facts a single Read can report: bytes that
// take the entity over the bound decide a 413 even when the same call also reports a read error, while a read error
// within the bound is a failed entity, never an accepted stop, whether the bytes before it form an empty object or
// not and whether it arrives with those bytes or after them. Only the bare io.EOF ends an entity: a read error that
// joins or wraps io.EOF, alone or with another failure, is still a read error.
func TestControlStopBodyCountsBeforeTheReadError(t *testing.T) {
	tests := map[string]struct {
		body       *ctlProbeBody
		wantStatus int
	}{
		"error: the byte that goes over the bound arrives with a read error": {
			body:       &ctlProbeBody{data: ctlPadded(ctlStopReadLimit), end: errCtlBodyFailure, endWithLast: true},
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		"error: the byte that goes over the bound arrives alone with a read error": {
			body:       &ctlProbeBody{data: ctlPadded(ctlStopReadLimit), perRead: 1, end: errCtlBodyFailure, endWithLast: true},
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		"error: the bytes that go over the bound arrive in uneven pieces, the last with a read error": {
			body:       &ctlProbeBody{data: ctlPadded(ctlStopReadLimit), perRead: 1000, end: errCtlBodyFailure, endWithLast: true},
			wantStatus: http.StatusRequestEntityTooLarge,
		},
		"error: a complete empty object followed by a read error": {
			body:       &ctlProbeBody{data: []byte("{}"), end: errCtlBodyFailure},
			wantStatus: ctlReadErrorStatus,
		},
		"error: a complete empty object whose last byte arrives with a read error": {
			body:       &ctlProbeBody{data: []byte("{}"), end: errCtlBodyFailure, endWithLast: true},
			wantStatus: ctlReadErrorStatus,
		},
		"error: an empty object that fills the bound, followed by a read error": {
			body:       &ctlProbeBody{data: ctlPadded(ctlStopBodyLimit), end: errCtlBodyFailure},
			wantStatus: ctlReadErrorStatus,
		},
		"error: an empty object that fills the bound, its last byte with a read error": {
			body:       &ctlProbeBody{data: ctlPadded(ctlStopBodyLimit), perRead: 1, end: errCtlBodyFailure, endWithLast: true},
			wantStatus: ctlReadErrorStatus,
		},
		"success: a complete empty object whose last byte arrives with the bare end of the entity": {
			body:       &ctlProbeBody{data: []byte("{}"), endWithLast: true},
			wantStatus: http.StatusAccepted,
		},
		"error: a complete empty object whose last byte arrives with the end joined to a read error": {
			body:       &ctlProbeBody{data: []byte("{}"), end: errors.Join(io.EOF, errCtlBodyFailure), endWithLast: true},
			wantStatus: ctlReadErrorStatus,
		},
		"error: a complete empty object whose last byte arrives with a read error that wraps the end": {
			body:       &ctlProbeBody{data: []byte("{}"), end: fmt.Errorf("%w: %w", errCtlBodyFailure, io.EOF), endWithLast: true},
			wantStatus: ctlReadErrorStatus,
		},
		"error: a complete empty object whose last byte arrives with an error that only wraps the end": {
			body:       &ctlProbeBody{data: []byte("{}"), end: fmt.Errorf("reading the stop entity: %w", io.EOF), endWithLast: true},
			wantStatus: ctlReadErrorStatus,
		},
		"error: an unfinished object followed by a read error": {
			body:       &ctlProbeBody{data: []byte("{" + strings.Repeat(" ", ctlStopBodyLimit-2)), end: errCtlBodyFailure},
			wantStatus: ctlReadErrorStatus,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, h := ctlDirectGateway(t, nil)
			resp, body := ctlCall(t, h, ctlDirectStop(t, tt.body, -1))
			ctlCheckWithinReadLimit(t, tt.body)
			if tt.wantStatus == http.StatusAccepted {
				ctlCheckStopped(t, resp, body)
				if n := f.stops.Load(); n != 1 {
					t.Errorf("Stop invoked %d times; want 1", n)
				}
				return
			}
			ctlCheckRefused(t, f, resp, body, tt.wantStatus)
		})
	}
}

// TestControlStopDeclaredLength decides that a declared length over the bound is refused from the head: an
// authenticated stop gets the empty 413 and an unauthenticated one the empty 403, and neither reads the body at all,
// while a chunked entity over the bound is refused by the streamed bound. The accepted case at the bound shows that
// the probe records the reads a dispatcher does make.
func TestControlStopDeclaredLength(t *testing.T) {
	over := ctlPadded(ctlStopReadLimit)
	tests := map[string]struct {
		header     http.Header
		body       *ctlProbeBody
		length     int64
		wantStatus int
		// wantNoReads requires that the body was never read; otherwise the reads stay within the bound.
		wantNoReads bool
	}{
		"success: an authenticated declared length at the bound": {
			header: ctlStopHeader(), body: &ctlProbeBody{data: ctlPadded(ctlStopBodyLimit)}, length: ctlStopBodyLimit,
			wantStatus: http.StatusAccepted,
		},
		"error: an authenticated declared length one byte over the bound": {
			header: ctlStopHeader(), body: &ctlProbeBody{data: over}, length: ctlStopReadLimit,
			wantStatus: http.StatusRequestEntityTooLarge, wantNoReads: true,
		},
		"error: an authenticated declared length far over the bound": {
			header: ctlStopHeader(), body: &ctlProbeBody{data: over}, length: 1 << 30,
			wantStatus: http.StatusRequestEntityTooLarge, wantNoReads: true,
		},
		"error: no token with a declared length over the bound": {
			header: http.Header{"Content-Type": {"application/json"}}, body: &ctlProbeBody{data: over}, length: 1 << 30,
			wantStatus: http.StatusForbidden, wantNoReads: true,
		},
		"error: another root's token with a declared length over the bound": {
			header: http.Header{"Content-Type": {"application/json"}, ctlTokenName: {ctlOtherToken}}, body: &ctlProbeBody{data: over},
			length: ctlStopReadLimit, wantStatus: http.StatusForbidden, wantNoReads: true,
		},
		"error: an Origin with a declared length over the bound": {
			header: http.Header{"Content-Type": {"application/json"}, ctlTokenName: {ctlToken}, "Origin": {"http://example.test"}},
			body:   &ctlProbeBody{data: over}, length: ctlStopReadLimit, wantStatus: http.StatusForbidden, wantNoReads: true,
		},
		"error: an authenticated chunked entity over the bound": {
			header: ctlStopHeader(), body: &ctlProbeBody{data: over}, length: -1,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, h := ctlDirectGateway(t, nil)
			resp, body := ctlCall(t, h, ctlDirectRequest(t, http.MethodPost, "/internal/stop", tt.header, tt.body, tt.length))
			use := tt.body.use()
			if tt.wantStatus == http.StatusAccepted {
				ctlCheckStopped(t, resp, body)
				if use.Calls == 0 {
					t.Error("an accepted stop was decided without reading its entity, so the probe records nothing")
				}
			} else {
				ctlCheckRefused(t, f, resp, body, tt.wantStatus)
			}
			if tt.wantNoReads {
				if diff := gocmp.Diff(ctlProbeUse{}, use); diff != "" {
					t.Errorf("the body was read although the head decides the answer (-want +got):\n%s", diff)
				}
				return
			}
			ctlCheckWithinReadLimit(t, tt.body)
		})
	}
}

// TestControlStopEffectPanics decides what follows a stop effect that panics: the effect's own panic value leaves
// the handler as it would reach the server, the effect is never invoked again, every later well-formed stop is
// refused with an empty 500, also one that overlapped the effect and only after it failed, and the identity proof and the relay
// keep working.
func TestControlStopEffectPanics(t *testing.T) {
	f, h := ctlDirectGateway(t, func() { panic(ctlEffectPanic) })
	recovered := func() (p any) {
		defer func() { p = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), ctlDirectStop(t, io.NopCloser(strings.NewReader("{}")), 2))
		return nil
	}()
	if diff := gocmp.Diff(any(ctlEffectPanic), recovered); diff != "" {
		t.Fatalf("the value that left the handler is not the stop effect's panic (-want +got):\n%s", diff)
	}
	if n := f.stops.Load(); n != 1 {
		t.Fatalf("Stop invoked %d times by the first stop; want 1", n)
	}
	tests := map[string]func(t *testing.T){
		"error: a later stop is refused without invoking the effect": func(t *testing.T) {
			ctlCheckFailedStop(t, f, h, "{}", 2)
		},
		"error: a later stop with JSON white space is refused without invoking the effect": func(t *testing.T) {
			ctlCheckFailedStop(t, f, h, " { } ", 5)
		},
		"error: a later chunked stop is refused without invoking the effect": func(t *testing.T) {
			ctlCheckFailedStop(t, f, h, "{}", -1)
		},
		"success: the identity proof still answers": func(t *testing.T) {
			nonce := ctlNonce(t)
			resp, body := ctlCall(t, h, ctlDirectIdentity(t, nonce))
			ctlCheckProof(t, resp, body, nonce)
		},
		"success: ordinary traffic still reaches the relay": func(t *testing.T) {
			before := f.relays.Load()
			resp, body := ctlCall(t, h, ctlDirectRequest(t, http.MethodGet, "/v1/models", http.Header{}, http.NoBody, 0))
			if resp.StatusCode != http.StatusOK || string(body) != "relayed" {
				t.Errorf("ordinary request got %d %q; want the relay's answer", resp.StatusCode, body)
			}
			if diff := gocmp.Diff(before+1, f.relays.Load()); diff != "" {
				t.Errorf("relay calls (-want +got):\n%s", diff)
			}
		},
	}
	for name, check := range tests {
		t.Run(name, check)
	}
	if n := f.stops.Load(); n != 1 {
		t.Errorf("Stop invoked %d times in all; want only the call that panicked", n)
	}
	t.Run("error: a stop overlapping the panicking effect is answered only after the panic, with the empty 500", ctlCheckOverlappingStops)
}

// ctlOrderedRecorder records a response and whether the handler began it before the parked effect was released.
type ctlOrderedRecorder struct {
	*httptest.ResponseRecorder
	released *atomic.Bool

	mu            sync.Mutex
	answered      bool
	beforeRelease bool
}

func (r *ctlOrderedRecorder) note() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.answered {
		r.answered, r.beforeRelease = true, !r.released.Load()
	}
}

func (r *ctlOrderedRecorder) WriteHeader(code int) {
	r.note()
	r.ResponseRecorder.WriteHeader(code)
}

func (r *ctlOrderedRecorder) Write(p []byte) (int, error) {
	r.note()
	return r.ResponseRecorder.Write(p)
}

func (r *ctlOrderedRecorder) Flush() {
	r.note()
	r.ResponseRecorder.Flush()
}

// ctlServed is the outcome of one handler call made off the test goroutine.
type ctlServed struct {
	rec       *ctlOrderedRecorder
	recovered any
	// returnedBeforeRelease reports that the handler returned before the parked effect was released.
	returnedBeforeRelease bool
}

// ctlServeAsync calls the handler on a goroutine of its own, closing entered just before the call, and delivers the
// outcome, a panic included, on the returned channel. It never touches the test, so that no failure is raised off
// the test goroutine.
func ctlServeAsync(h http.Handler, req *http.Request, released *atomic.Bool, entered chan<- struct{}) <-chan ctlServed {
	done := make(chan ctlServed, 1)
	go func() {
		rec := &ctlOrderedRecorder{ResponseRecorder: httptest.NewRecorder(), released: released}
		defer func() {
			done <- ctlServed{rec: rec, recovered: recover(), returnedBeforeRelease: !released.Load()}
		}()
		close(entered)
		h.ServeHTTP(rec, req)
	}()
	return done
}

// ctlAsyncCall is one handler call started off the test goroutine and, once collected, its outcome.
type ctlAsyncCall struct {
	done <-chan ctlServed
	out  *ctlServed
}

// ctlCheckOverlappingStops overlaps two well-formed stops on one dispatcher: the first one's effect is parked, the
// second call is entered, and only then is the effect released to panic. Whatever the dispatcher does with the second
// stop while the first runs, it must not answer it before the effect has failed, and once it has failed the answer is
// the empty 500 without a second invocation of the effect. The case decides overlapping requests; it does not place
// the second stop at any particular point inside the dispatcher. The watchdog only turns a hang into a failure.
func ctlCheckOverlappingStops(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	letEffectPanic := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	var hookCalls atomic.Int64
	f, h := ctlDirectGateway(t, func() {
		// Only the first invocation parks and panics; a dispatcher that invokes the effect again is caught by the
		// count, not by a hang.
		if hookCalls.Add(1) != 1 {
			return
		}
		close(entered)
		<-release
		panic(ctlEffectPanic)
	})
	firstReq := ctlDirectStop(t, io.NopCloser(strings.NewReader("{}")), 2)
	secondReq := ctlDirectStop(t, io.NopCloser(strings.NewReader("{}")), 2)
	var first, second ctlAsyncCall
	// Whatever ends the test, the effect is released and every started call gets a bounded time to return, so that
	// no handler goroutine outlives it.
	defer func() {
		letEffectPanic()
		grace := time.NewTimer(ctlWatchdog)
		defer grace.Stop()
		for _, c := range []*ctlAsyncCall{&first, &second} {
			if c.done == nil || c.out != nil {
				continue
			}
			select {
			case <-c.done:
			case <-grace.C:
				t.Error("a started handler call did not return after the effect was released")
				return
			}
		}
	}()
	watchdog := time.NewTimer(ctlWatchdog)
	defer watchdog.Stop()
	collect := func(c *ctlAsyncCall, what string) {
		select {
		case out := <-c.done:
			c.out = &out
		case <-watchdog.C:
			t.Fatalf("%s did not return", what)
		}
	}

	first.done = ctlServeAsync(h, firstReq, &released, make(chan struct{}))
	select {
	case <-entered:
	case out := <-first.done:
		first.out = &out
		t.Fatalf("the first stop returned without its effect parking: status %d, panic %v", out.rec.Code, out.recovered)
	case <-watchdog.C:
		t.Fatal("the first stop did not invoke the effect")
	}
	secondEntered := make(chan struct{})
	second.done = ctlServeAsync(h, secondReq, &released, secondEntered)
	select {
	case <-secondEntered:
	case <-watchdog.C:
		t.Fatal("the second stop's call was not entered")
	}
	letEffectPanic()
	collect(&first, "the first stop, after its effect was released,")
	collect(&second, "the second stop, after the effect was released,")

	if diff := gocmp.Diff(any(ctlEffectPanic), first.out.recovered); diff != "" {
		t.Errorf("the value that left the first stop is not the effect's panic (-want +got):\n%s", diff)
	}
	if second.out.recovered != nil {
		t.Fatalf("the second stop panicked: %v", second.out.recovered)
	}
	if second.out.rec.beforeRelease || second.out.returnedBeforeRelease {
		t.Errorf("the second stop was answered (began %t, returned %t) before the effect had failed", second.out.rec.beforeRelease, second.out.returnedBeforeRelease)
	}
	got := ctlRefusal{Status: second.out.rec.Code, Body: second.out.rec.Body.String(), Stops: f.stops.Load(), Relays: f.relays.Load()}
	if diff := gocmp.Diff(ctlRefusal{Status: http.StatusInternalServerError, Stops: 1}, got); diff != "" {
		t.Errorf("the stop overlapping the failed effect (-want +got):\n%s", diff)
	}
}

// ctlCheckFailedStop sends a well-formed stop with entity after the stop effect failed and decides its answer: an
// empty 500 that neither invoked the effect again nor reached the relay.
func ctlCheckFailedStop(t *testing.T, f *ctlFixture, h http.Handler, entity string, length int64) {
	t.Helper()
	before := f.relays.Load()
	resp, body := ctlCall(t, h, ctlDirectStop(t, io.NopCloser(strings.NewReader(entity)), length))
	got := ctlRefusal{Status: resp.StatusCode, Body: string(body), Stops: f.stops.Load(), Relays: f.relays.Load() - before}
	if diff := gocmp.Diff(ctlRefusal{Status: http.StatusInternalServerError, Stops: 1}, got); diff != "" {
		t.Errorf("stop after a failed effect (-want +got):\n%s", diff)
	}
}

// TestControlRoutesByTheRawTargetOnly decides that the dispatcher chooses a control path from the raw request target
// alone: a request without one is ordinary traffic for the relay whatever its parsed URL says, and a request without
// a URL at all reaches the relay without the dispatcher looking at it, and when the raw target and the parsed URL
// disagree the raw target decides in both directions. The rows whose raw target is a control path are the positive
// control: those requests are the supervisor's.
func TestControlRoutesByTheRawTargetOnly(t *testing.T) {
	identityHeader := func() http.Header { return http.Header{ctlNonceName: {ctlNonce(t)}} }
	stopBody := func() io.ReadCloser { return io.NopCloser(strings.NewReader("{}")) }
	tests := map[string]struct {
		method     string
		rawTarget  string
		url        *url.URL
		header     http.Header
		body       func() io.ReadCloser
		length     int64
		wantStatus int
		wantRelays int64
		// wantSigned is how many proofs the dispatcher signed for the request.
		wantSigned int
	}{
		"success: an empty target whose URL names the identity path is relayed": {
			method: http.MethodGet, url: &url.URL{Path: "/internal/identity"}, header: identityHeader(),
			body: func() io.ReadCloser { return http.NoBody }, wantStatus: http.StatusOK, wantRelays: 1,
		},
		"success: an empty target whose URL names the stop path is relayed": {
			method: http.MethodPost, url: &url.URL{Path: "/internal/stop"}, header: ctlStopHeader(), body: stopBody, length: 2,
			wantStatus: http.StatusOK, wantRelays: 1,
		},
		"success: an empty target without a URL, with an identity challenge, is relayed": {
			method: http.MethodGet, header: identityHeader(), body: func() io.ReadCloser { return http.NoBody },
			wantStatus: http.StatusOK, wantRelays: 1,
		},
		"success: an empty target without a URL, with a stop's header and entity, is relayed": {
			method: http.MethodPost, header: ctlStopHeader(), body: stopBody, length: 2, wantStatus: http.StatusOK, wantRelays: 1,
		},
		"success: the identity path as the raw target is the supervisor's": {
			method: http.MethodGet, rawTarget: "/internal/identity", url: &url.URL{Path: "/internal/identity"}, header: identityHeader(),
			body: func() io.ReadCloser { return http.NoBody }, wantStatus: http.StatusOK, wantSigned: 1,
		},
		"success: an ordinary raw target is relayed although its URL names the identity path": {
			method: http.MethodGet, rawTarget: "/v1/models", url: &url.URL{Path: "/internal/identity"}, header: identityHeader(),
			body: func() io.ReadCloser { return http.NoBody }, wantStatus: http.StatusOK, wantRelays: 1,
		},
		"success: an ordinary raw target is relayed although its URL names the stop path": {
			method: http.MethodPost, rawTarget: "/v1/models", url: &url.URL{Path: "/internal/stop"}, header: ctlStopHeader(),
			body: stopBody, length: 2, wantStatus: http.StatusOK, wantRelays: 1,
		},
		"success: the identity path as the raw target is the supervisor's although its URL names another path": {
			method: http.MethodGet, rawTarget: "/internal/identity", url: &url.URL{Path: "/v1/models"}, header: identityHeader(),
			body: func() io.ReadCloser { return http.NoBody }, wantStatus: http.StatusOK, wantSigned: 1,
		},
		"success: the stop path as the raw target is the supervisor's although its URL names another path": {
			method: http.MethodPost, rawTarget: "/internal/stop", url: &url.URL{Path: "/v1/models"}, header: ctlStopHeader(),
			body: stopBody, length: 2, wantStatus: http.StatusAccepted,
		},
		"success: the stop path as the raw target is the supervisor's": {
			method: http.MethodPost, rawTarget: "/internal/stop", url: &url.URL{Path: "/internal/stop"}, header: ctlStopHeader(),
			body: stopBody, length: 2, wantStatus: http.StatusAccepted,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, h := ctlDirectGateway(t, nil)
			tt.header.Set("Content-Length", strconv.FormatInt(tt.length, 10))
			req := (&http.Request{
				Method: tt.method, URL: tt.url, RequestURI: tt.rawTarget, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
				Header: tt.header, Body: tt.body(), ContentLength: tt.length, Host: "127.0.0.1:" + strconv.Itoa(ctlDirectPort),
				RemoteAddr: "127.0.0.1:50123",
			}).WithContext(t.Context())
			resp, body := ctlCall(t, h, req)
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status %d, body %q; want %d", resp.StatusCode, body, tt.wantStatus)
			}
			if tt.wantRelays > 0 && string(body) != "relayed" {
				t.Errorf("body %q; want the relay's answer", body)
			}
			if diff := gocmp.Diff(tt.wantRelays, f.relays.Load()); diff != "" {
				t.Errorf("relay calls (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantSigned, len(f.signatures())); diff != "" {
				t.Errorf("signer calls (-want +got):\n%s", diff)
			}
			if tt.wantRelays > 0 {
				if n := f.stops.Load(); n != 0 {
					t.Errorf("a relayed request invoked Stop %d times", n)
				}
			}
		})
	}
}
