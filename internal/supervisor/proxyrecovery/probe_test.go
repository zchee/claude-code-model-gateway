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

//go:build unix

package proxyrecovery_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor/proxyrecovery"
)

// answer is how the fake proxy answers one request.
type answer struct {
	status   int
	location string
	// delay passes before the status line is written.
	delay time.Duration
	// body replaces the default JSON body when it is not nil; an empty, non-nil body is an empty one.
	body []byte
	// short declares a longer body than it writes and then closes the connection.
	short bool
	// chunked sends the body with chunked transfer coding and no Content-Length.
	chunked bool
	// gzipped marks the body, sent as it is, with Content-Encoding: gzip.
	gzipped bool
	// stall writes the head and part of a declared body, then nothing more until the client leaves.
	stall bool
	// chunks, when nonzero, writes the declared body in that many parts, one every interval, so that bytes keep
	// arriving.
	chunks   int
	interval time.Duration
}

// head is the part of a request the fake proxy saw.
type head struct {
	Line      string
	Host      string
	UserAgent string
}

// fakeProxy is an HTTP/1.1 listener on loopback, written by hand so that the test controls every byte and every
// connection. It offers to keep each connection alive and serves further requests on it as long as the client sends
// them.
type fakeProxy struct {
	l         net.Listener
	port      uint16
	route     func(path string, port uint16) answer
	done      chan struct{}
	requested chan struct{}
	// departed is closed when a handler first sees the client leave a connection it was still answering.
	departed  chan struct{}
	accepting sync.WaitGroup
	handlers  sync.WaitGroup
	reqOnce   sync.Once
	leftOnce  sync.Once
	stopOnce  sync.Once
	mu        sync.Mutex
	conns     []net.Conn
	heads     []head
	accepted  int
}

func startProxy(t *testing.T, route func(path string, port uint16) answer) *fakeProxy {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	p := &fakeProxy{
		l: l, port: uint16(l.Addr().(*net.TCPAddr).Port), route: route,
		done: make(chan struct{}), requested: make(chan struct{}), departed: make(chan struct{}),
	}
	p.accepting.Go(p.serve)
	t.Cleanup(p.shutdown)
	return p
}

// shutdown ends the fake proxy in an order that leaves nothing behind: the listener is closed and the accept loop
// joined before any connection is closed, so every connection is registered before the drain; then every registered
// connection is closed, which also ends a probe still reading from one; then every handler is joined.
func (p *fakeProxy) shutdown() {
	p.stopOnce.Do(func() {
		close(p.done)
		_ = p.l.Close()
		p.accepting.Wait()
		p.mu.Lock()
		conns := p.conns
		p.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		p.handlers.Wait()
	})
}

func (p *fakeProxy) serve() {
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.accepted++
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		p.handlers.Go(func() { p.handle(c) })
	}
}

func (p *fakeProxy) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_ = req.Body.Close()
		p.mu.Lock()
		p.heads = append(p.heads, head{
			Line: fmt.Sprintf("%s %s %s", req.Method, req.RequestURI, req.Proto), Host: req.Host, UserAgent: req.UserAgent(),
		})
		p.mu.Unlock()
		p.reqOnce.Do(func() { close(p.requested) })
		a := p.route(req.URL.Path, p.port)
		if !p.pause(a.delay) || !p.answer(c, br, a) {
			return
		}
	}
}

// pause waits d, or less when the fake proxy shuts down, and reports whether it waited the whole time.
func (p *fakeProxy) pause(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-p.done:
		return false
	}
}

// defaultBody is the body of an answer that names none.
var defaultBody = []byte(`{"data":[]}`)

// answer writes the response a describes and reports whether the connection may serve another request.
func (p *fakeProxy) answer(c net.Conn, br *bufio.Reader, a answer) bool {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nConnection: keep-alive\r\n", a.status, http.StatusText(a.status))
	if a.location != "" {
		fmt.Fprintf(&b, "Location: %s\r\n", a.location)
	}
	if a.gzipped {
		b.WriteString("Content-Encoding: gzip\r\n")
	}
	body := defaultBody
	if a.body != nil {
		body = a.body
	}
	switch {
	case a.status == http.StatusNoContent || a.status == http.StatusNotModified:
		b.WriteString("\r\n")
		_, err := io.WriteString(c, b.String())
		return err == nil
	case a.chunked:
		b.WriteString("Transfer-Encoding: chunked\r\n\r\n")
		if _, err := io.WriteString(c, b.String()); err != nil {
			return false
		}
		for part := range slices.Chunk(body, 64<<10) {
			if _, err := fmt.Fprintf(c, "%x\r\n%s\r\n", len(part), part); err != nil {
				return false
			}
		}
		_, err := io.WriteString(c, "0\r\n\r\n")
		return err == nil
	case a.short:
		fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body)+50, body)
		_, _ = io.WriteString(c, b.String())
		return false
	case a.stall:
		b.WriteString("Content-Length: 100\r\n\r\n{")
		if _, err := io.WriteString(c, b.String()); err != nil {
			return false
		}
		// Nothing more is written. The read ends when the client closes the connection or the fake proxy shuts down.
		_, _ = br.ReadByte()
		p.left()
		return false
	case a.chunks > 0:
		const part = "        "
		fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", a.chunks*len(part))
		if _, err := io.WriteString(c, b.String()); err != nil {
			return false
		}
		for range a.chunks {
			if !p.pause(a.interval) {
				return false
			}
			if _, err := io.WriteString(c, part); err != nil {
				p.left()
				return false
			}
		}
		return true
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(body))
	if _, err := io.WriteString(c, b.String()); err != nil {
		return false
	}
	_, err := c.Write(body)
	return err == nil
}

// left records that the client left a connection the fake proxy was still answering; a connection the fake proxy
// closes itself at shutdown is not recorded.
func (p *fakeProxy) left() {
	select {
	case <-p.done:
		return
	default:
	}
	p.leftOnce.Do(func() { close(p.departed) })
}

func (p *fakeProxy) seen() (int, []head) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted, append([]head(nil), p.heads...)
}

// lines returns the request lines the fake proxy saw.
func (p *fakeProxy) lines() []string {
	_, heads := p.seen()
	lines := make([]string, 0, len(heads))
	for _, h := range heads {
		lines = append(lines, h.Line)
	}
	return lines
}

// sawDeparture reports whether the fake proxy saw the client leave a connection it was still answering. A writing
// handler sees the departure only at its next write, so it waits for it, bounded by probeAbort; the timing of the
// probe itself is measured on the client's side.
func (p *fakeProxy) sawDeparture() bool {
	select {
	case <-p.departed:
		return true
	case <-time.After(probeAbort):
		return false
	}
}

// boundNotListening holds a loopback TCP port with a socket that is bound but never listens, so that a connection to
// it is refused and no other program can take the port while the test runs.
func boundNotListening(t *testing.T) uint16 {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind on loopback: %v", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	port := uint16(sa.(*syscall.SockaddrInet4).Port)
	// The precondition, shown with the operation the probe performs: a connection to the port is refused.
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		_ = c.Close()
		t.Fatalf("a connection to the bound, non-listening port %d was accepted", port)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("a connection to the bound, non-listening port %d failed with %v, not a refusal", port, err)
	}
	return port
}

// newProbe constructs the production probe; a panic fails the test, so that a declaration that is not implemented
// never counts as an answer.
func newProbe(t *testing.T, port uint16) func(context.Context) bool {
	t.Helper()
	var fn func(context.Context) bool
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		fn = proxyrecovery.ModelsProbe(port)
	}()
	if panicked != nil {
		t.Fatalf("ModelsProbe panicked: %v", panicked)
	}
	if fn == nil {
		t.Fatal("ModelsProbe returned no probe")
	}
	return fn
}

// probeAbort bounds one probe call in real time, far above its two seconds. It decides no expectation: it only ends
// a call that would otherwise never return.
const probeAbort = 10 * time.Second

// probeCall is one call of a probe running in its own goroutine. entered and returned are read by that goroutine
// around the call itself, so the call's duration does not include the scheduling of the test.
type probeCall struct {
	done              chan struct{}
	answered          bool
	panicked          any
	entered, returned time.Time
}

func goProbe(fn func(context.Context) bool, ctx context.Context) *probeCall {
	c := &probeCall{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer func() {
			c.returned = time.Now()
			c.panicked = recover()
		}()
		c.entered = time.Now()
		c.answered = fn(ctx)
	}()
	return c
}

// wait returns the call's answer and duration. A call that has not returned after probeAbort is ended by abort,
// which shuts the fake proxy down and so closes every connection the probe holds; the call is then joined, bounded
// again, and the test fails. A probe without any deadline therefore fails the test instead of hanging it.
func (c *probeCall) wait(t *testing.T, abort func()) (bool, time.Duration) {
	t.Helper()
	timer := time.NewTimer(probeAbort)
	defer timer.Stop()
	select {
	case <-c.done:
		if c.panicked != nil {
			t.Fatalf("the models probe panicked: %v", c.panicked)
		}
		return c.answered, c.returned.Sub(c.entered)
	case <-timer.C:
	}
	abort()
	select {
	case <-c.done:
		t.Fatalf("the probe returned only when the fake proxy closed its connections, %v after it was called: it has no deadline", probeAbort)
	case <-time.After(probeAbort):
		t.Fatalf("the probe did not return within %v, not even after the fake proxy closed its connections", 2*probeAbort)
	}
	return false, 0
}

// joinProbe waits, bounded by probeAbort, for a call the test has already failed, so that it ends with the test.
func joinProbe(t *testing.T, c *probeCall) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(probeAbort):
		t.Errorf("the probe did not return within %v after the fake proxy closed its connections", probeAbort)
	}
}

// call runs one probe call to its end.
func call(t *testing.T, fn func(context.Context) bool, ctx context.Context, abort func()) (bool, time.Duration) {
	t.Helper()
	return goProbe(fn, ctx).wait(t, abort)
}

// The bounds, measured by the probe's caller, of a call that must end at the probe's two-second deadline. The lower
// one shows the probe did not give up early; the upper one leaves a second and a half for the scheduler and still
// separates the deadline from the slower answers the cases offer.
const (
	deadlineEarliest = 1800 * time.Millisecond
	deadlineLatest   = 3500 * time.Millisecond
)

// bodyBound is the bound on the encoded body of a models answer, the one the readiness check applies.
const bodyBound = 1 << 20

// gzipped returns size bytes of text compressed with gzip at its best compression.
func gzipped(size int) []byte {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		panic(err)
	}
	if _, err := zw.Write(bytes.Repeat([]byte("x"), size)); err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// TestModelsProbe decides the production probe (lib/process-supervision.js:993-997 with fetchUrl at :25-43): one GET
// /v1/models to 127.0.0.1 on the port, true only for a 200 whose body has been read to its end within two seconds.
// The content of the body is not judged: an empty body or one that is not JSON counts, a body cut short does not. The
// two seconds bound the whole call: the head, the whole body and every redirect. A timer that restarts whenever bytes
// arrive, or one that restarts for each redirect, is not that bound. A body over 1 MiB is no answer, as for the
// readiness check, where the source would read it whole. The bound counts the body's bytes as sent: a chunked body
// that declares no length is bounded as well, and a gzip body is counted before any decoding.
func TestModelsProbe(t *testing.T) {
	status := func(code int) func(string, uint16) answer {
		return func(string, uint16) answer { return answer{status: code} }
	}
	body := func(b []byte) func(string, uint16) answer {
		return func(string, uint16) answer { return answer{status: http.StatusOK, body: b} }
	}
	// The gzip body is prepared once, before any probe runs, so that its compression is no part of a call's two
	// seconds; the handler only writes these bytes.
	compressed := gzipped(4 * bodyBound)
	if len(compressed) >= bodyBound {
		t.Fatalf("test precondition: the gzip body is %d bytes as sent, want under the %d-byte bound", len(compressed), bodyBound)
	}
	tests := map[string]struct {
		route func(path string, port uint16) answer
		// refused makes the port one that refuses connections instead of the fake proxy's.
		refused bool
		want    bool
		// deadline makes the call one that must end at the two-second bound.
		deadline bool
		// observed requires that the fake proxy saw the probe leave the connection it was still answering.
		observed bool
	}{
		"success: a 200 is true": {route: status(http.StatusOK), want: true},
		"success: a 200 after one second is true": {
			route: func(string, uint16) answer { return answer{status: http.StatusOK, delay: time.Second} },
			want:  true,
		},
		"success: a complete 200 whose body is not JSON is true": {route: body([]byte("models listed")), want: true},
		"success: a complete 200 with an empty body is true":     {route: body([]byte{}), want: true},
		"success: a 200 whose body is exactly the bound is true": {route: body([]byte(strings.Repeat("x", bodyBound))), want: true},
		"error: a 200 whose body is one byte over the bound is false": {
			route: body([]byte(strings.Repeat("x", bodyBound+1))),
		},
		"error: a complete chunked 200 without a length one byte over the bound is false": {
			route: func(string, uint16) answer {
				return answer{status: http.StatusOK, chunked: true, body: bytes.Repeat([]byte("x"), bodyBound+1)}
			},
		},
		"success: a gzip 200 under the bound as sent whose decoded text is over it is true": {
			route: func(string, uint16) answer {
				return answer{status: http.StatusOK, gzipped: true, body: compressed}
			},
			want: true,
		},
		"error: a 200 whose connection closes before its declared length is false": {
			route: func(string, uint16) answer { return answer{status: http.StatusOK, short: true} },
		},
		"error: a 201 is false":             {route: status(http.StatusCreated)},
		"error: a 204 is false":             {route: status(http.StatusNoContent)},
		"error: a 404 is false":             {route: status(http.StatusNotFound)},
		"error: a 500 is false":             {route: status(http.StatusInternalServerError)},
		"error: a 503 is false":             {route: status(http.StatusServiceUnavailable)},
		"error: nothing listening is false": {refused: true},
		"error: a 200 after two and a half seconds is false at the two-second bound": {
			route:    func(string, uint16) answer { return answer{status: http.StatusOK, delay: 2500 * time.Millisecond} },
			deadline: true,
		},
		"error: a 200 whose body stalls is false at the two-second bound": {
			route:    func(string, uint16) answer { return answer{status: http.StatusOK, stall: true} },
			deadline: true,
			observed: true,
		},
		"error: a 200 whose body keeps arriving for four seconds is false at the two-second bound": {
			route: func(string, uint16) answer {
				return answer{status: http.StatusOK, chunks: 40, interval: 100 * time.Millisecond}
			},
			deadline: true,
			observed: true,
		},
		"error: a redirect and its target that each answer in 1.2 seconds are false at the two-second bound": {
			route: func(path string, _ uint16) answer {
				if path == "/v1/models" {
					return answer{status: http.StatusFound, location: "/v1/models/moved", delay: 1200 * time.Millisecond}
				}
				return answer{status: http.StatusOK, delay: 1200 * time.Millisecond}
			},
			deadline: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var port uint16
			var p *fakeProxy
			abort := func() {}
			if tt.refused {
				port = boundNotListening(t)
			} else {
				p = startProxy(t, tt.route)
				port, abort = p.port, p.shutdown
			}
			got, took := call(t, newProbe(t, port), t.Context(), abort)
			if got != tt.want {
				t.Errorf("probe = %v, want %v (took %v)", got, tt.want, took)
			}
			if tt.deadline && (took < deadlineEarliest || took > deadlineLatest) {
				t.Errorf("the probe took %v, want the two-second bound (%v to %v)", took, deadlineEarliest, deadlineLatest)
			}
			if tt.observed && !p.sawDeparture() {
				t.Errorf("the fake proxy never saw the probe leave the connection it was still answering, within %v", probeAbort)
			}
		})
	}
}

// TestModelsProbeCancellation decides that a caller whose context ends gets false promptly. The context ends only
// once the fake proxy has seen the request, whose answer stays held, so the case decides the cancellation of a request
// under way and not of a call that never started or had already ended.
func TestModelsProbeCancellation(t *testing.T) {
	tests := map[string]struct {
		// within bounds the return after the context ended.
		within time.Duration
	}{
		"error: a caller context that ends during the request returns false promptly": {within: time.Second},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := startProxy(t, func(string, uint16) answer { return answer{status: http.StatusOK, delay: time.Hour} })
			fn := newProbe(t, p.port)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			c := goProbe(fn, ctx)
			select {
			case <-p.requested:
			case <-c.done:
				t.Fatalf("the probe returned %v before the fake proxy saw its request (panic %v)", c.answered, c.panicked)
			case <-time.After(probeAbort):
				p.shutdown()
				joinProbe(t, c)
				t.Fatalf("the fake proxy saw no request within %v", probeAbort)
			}
			cancelled := time.Now()
			cancel()
			select {
			case <-c.done:
			case <-time.After(tt.within):
				// A completion published when the timer fired is judged by its recorded return time below; only a
				// probe still pending is aborted.
				select {
				case <-c.done:
				default:
					p.shutdown()
					joinProbe(t, c)
					t.Fatalf("the probe did not return within %v after its caller's context ended", tt.within)
				}
			}
			if c.panicked != nil {
				t.Fatalf("the models probe panicked: %v", c.panicked)
			}
			if c.answered {
				t.Error("a probe whose caller's context ended answered true")
			}
			if c.returned.Before(cancelled) {
				t.Errorf("the probe returned %v before its caller's context ended", cancelled.Sub(c.returned))
			}
			if after := c.returned.Sub(cancelled); after > tt.within {
				t.Errorf("the probe returned %v after its caller's context ended, want at most %v", after, tt.within)
			}
		})
	}
}

// TestModelsProbeRedirects decides the redirects the probe follows. The source reaches the probe through its general
// fetch helper (lib/process-supervision.js:29-33, 995), and the port's bounded redirect rule for that helper applies:
// only 301, 302, 303, 307 and 308 are followed, to the location they name, another listener included; at most ten
// requests are sent, the first included; a location that carries user information is refused without a request to
// it.
func TestModelsProbeRedirects(t *testing.T) {
	to := func(code int, location func(own, other uint16) string) func(string, uint16, uint16) answer {
		return func(path string, own, other uint16) answer {
			switch path {
			case "/v1/models":
				return answer{status: code, location: location(own, other)}
			case "/v1/models/moved":
				return answer{status: http.StatusOK}
			}
			return answer{status: http.StatusNotFound}
		}
	}
	relative := func(uint16, uint16) string { return "/v1/models/moved" }
	// chain redirects /v1/models to /hop/1 and each /hop/k to /hop/k+1, and answers 200 at /hop/last.
	chain := func(last int) func(string, uint16, uint16) answer {
		return func(path string, _, _ uint16) answer {
			if path == "/v1/models" {
				return answer{status: http.StatusFound, location: "/hop/1"}
			}
			var k int
			if _, err := fmt.Sscanf(path, "/hop/%d", &k); err != nil || k > last {
				return answer{status: http.StatusNotFound}
			}
			if k == last {
				return answer{status: http.StatusOK}
			}
			return answer{status: http.StatusFound, location: fmt.Sprintf("/hop/%d", k+1)}
		}
	}
	hops := func(n int) []string {
		paths := []string{"/v1/models"}
		for k := 1; k <= n; k++ {
			paths = append(paths, fmt.Sprintf("/hop/%d", k))
		}
		return paths
	}
	first := []string{"/v1/models"}
	followed := []string{"/v1/models", "/v1/models/moved"}
	tests := map[string]struct {
		route func(path string, own, other uint16) answer
		want  bool
		// paths are the request targets the probed listener must see, in order; elsewhere are those another
		// listener on another port must see, which answers 200 only for /v1/models/moved.
		paths, elsewhere []string
	}{
		"success: a 301 is followed to its location": {route: to(http.StatusMovedPermanently, relative), want: true, paths: followed},
		"success: a 302 is followed to its location": {route: to(http.StatusFound, relative), want: true, paths: followed},
		"success: a 303 is followed to its location": {route: to(http.StatusSeeOther, relative), want: true, paths: followed},
		"success: a 307 is followed to its location": {route: to(http.StatusTemporaryRedirect, relative), want: true, paths: followed},
		"success: a 308 is followed to its location": {route: to(http.StatusPermanentRedirect, relative), want: true, paths: followed},
		"success: an absolute location on the same listener is followed": {
			route: to(http.StatusFound, func(own, _ uint16) string { return fmt.Sprintf("http://127.0.0.1:%d/v1/models/moved", own) }),
			want:  true,
			paths: followed,
		},
		"success: an absolute location on another listener is followed there": {
			route: func(path string, _, other uint16) answer {
				if path == "/v1/models" {
					return answer{status: http.StatusFound, location: fmt.Sprintf("http://127.0.0.1:%d/v1/models/moved", other)}
				}
				return answer{status: http.StatusNotFound}
			},
			want:      true,
			paths:     first,
			elsewhere: []string{"/v1/models/moved"},
		},
		"error: a 300 with a location is not followed": {route: to(http.StatusMultipleChoices, relative), paths: first},
		"error: a 304 with a location is not followed": {route: to(http.StatusNotModified, relative), paths: first},
		"error: a 305 with a location is not followed": {route: to(http.StatusUseProxy, relative), paths: first},
		"error: a redirect without a location is false": {
			route: func(string, uint16, uint16) answer { return answer{status: http.StatusFound} },
			paths: first,
		},
		"success: the tenth request may answer":    {route: chain(9), want: true, paths: hops(9)},
		"error: an eleventh request is never sent": {route: chain(10), paths: hops(9)},
		"error: a location with user information is refused without a request to it": {
			route: to(http.StatusFound, func(own, _ uint16) string {
				return fmt.Sprintf("http://user:secret@127.0.0.1:%d/v1/models/moved", own)
			}),
			paths: first,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			other := startProxy(t, func(path string, _ uint16) answer {
				if path == "/v1/models/moved" {
					return answer{status: http.StatusOK}
				}
				return answer{status: http.StatusNotFound}
			})
			p := startProxy(t, func(path string, own uint16) answer { return tt.route(path, own, other.port) })
			got, took := call(t, newProbe(t, p.port), t.Context(), func() { p.shutdown(); other.shutdown() })
			if got != tt.want {
				t.Errorf("probe = %v, want %v (took %v)", got, tt.want, took)
			}
			requests := func(paths []string) []string {
				lines := make([]string, 0, len(paths))
				for _, path := range paths {
					lines = append(lines, "GET "+path+" HTTP/1.1")
				}
				return lines
			}
			if diff := gocmp.Diff(requests(tt.paths), p.lines()); diff != "" {
				t.Errorf("requests seen by the probed listener (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(requests(tt.elsewhere), other.lines()); diff != "" {
				t.Errorf("requests seen by the other listener (-want +got):\n%s", diff)
			}
			_, heads := other.seen()
			for _, h := range heads {
				if want := fmt.Sprintf("127.0.0.1:%d", other.port); h.Host != want {
					t.Errorf("the other listener was asked for Host %q, want %q", h.Host, want)
				}
			}
		})
	}
}

// TestModelsProbeRequest decides what goes on the wire: the request line, the Host header and the User-Agent the
// source sends (http.get of http://127.0.0.1:<port>/v1/models with user-agent model-gateway,
// lib/process-supervision.js:28), and a fresh connection for every call of one probe even when the proxy offers to
// keep the connection alive (test/gateway-drain.test.js:815). The monitor calls one probe for every probe of a tick,
// so the probe is constructed once and called again.
func TestModelsProbeRequest(t *testing.T) {
	tests := map[string]struct {
		calls int
	}{
		"success: one call":                 {calls: 1},
		"success: two calls of one probe":   {calls: 2},
		"success: three calls of one probe": {calls: 3},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := startProxy(t, func(string, uint16) answer { return answer{status: http.StatusOK} })
			fn := newProbe(t, p.port)
			for i := range tt.calls {
				if got, _ := call(t, fn, t.Context(), p.shutdown); !got {
					t.Fatalf("call %d: the probe of a 200 was false", i+1)
				}
			}
			conns, heads := p.seen()
			if conns != tt.calls {
				t.Errorf("%d calls of one probe used %d connections, want one each", tt.calls, conns)
			}
			want := make([]head, tt.calls)
			for i := range want {
				want[i] = head{Line: "GET /v1/models HTTP/1.1", Host: fmt.Sprintf("127.0.0.1:%d", p.port), UserAgent: "model-gateway"}
			}
			if diff := gocmp.Diff(want, heads); diff != "" {
				t.Errorf("requests seen (-want +got):\n%s", diff)
			}
		})
	}
}
