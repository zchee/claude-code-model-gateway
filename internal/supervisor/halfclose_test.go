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
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// These tests decide what the public listener does when a client ends its side of the connection, through the
// production handler (NewLeaseRelay) with a lease source and workers the test runs itself. The measured source
// (docs/port/baseline-node.md, "Half-close after a complete request") answers a client's half-close after a complete
// request with its own orderly end at once and sends nothing further, while the worker's request runs to its end.
//
// What the half-close does to delivery depends on the delivery's state. An exchange is committed when the lease
// source has handed a lease over to the relay. Before that, and after an exchange that failed, the half-close stops
// new delivery attempts: no generation receives the request afterwards. A committed exchange that is live runs to the
// worker's end of its answer, and nothing more goes to the client; if it fails instead, the request is not delivered
// again, since no new delivery starts after a half-close. A reset, or an end of input before the request was
// complete, is a cancellation in every state: the worker leg is closed and no new delivery is attempted.

const (
	hcToken     = "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
	hcWorkerPID = 4242
	// hcGuard turns a server that neither answers nor ends into a failure; it is never a pacing delay.
	hcGuard = 30 * time.Second
	// hcEndBound bounds the wait for the listener's own end after the client's half-close. The measured source ends
	// within a millisecond; the bound is generous for a loaded host and only turns a listener that waits for the
	// worker's answer into a failure.
	hcEndBound = 10 * time.Second
	// hcTeardown bounds the join of the public listener in cleanup, after every hold was released and every lease
	// closed.
	hcTeardown = 15 * time.Second
	// hcChunk and hcChunks shape the worker's answer: a stream of hcChunks events of hcChunk bytes each, flushed one
	// by one, about the size of the measured fixture stream.
	hcChunk  = 4096
	hcChunks = 64
	// hcOrderly is the client's end of stream without a socket error.
	hcOrderly = "orderly end of stream"
)

// hcFirstEvent is the first event of a streamed answer, sent before the client half-closes.
const hcFirstEvent = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"

// hcEvent returns the i-th event of the worker's answer, exactly hcChunk bytes long.
func hcEvent(i int) []byte {
	prefix := fmt.Sprintf("event: content_block_delta\ndata: {\"index\":%d,\"pad\":\"", i)
	const suffix = "\"}\n\n"
	return []byte(prefix + strings.Repeat("p", hcChunk-len(prefix)-len(suffix)) + suffix)
}

// hcLog is the order in which the workers and the lease source saw the end of an exchange.
type hcLog struct {
	mu     sync.Mutex
	events []string
}

// add appends event and returns its position in the order.
func (l *hcLog) add(event string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	return len(l.events) - 1
}

func (l *hcLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

const (
	hcWorkerFinished = "the worker finished its answer"
	hcLeaseClosed    = "the supervisor closed the worker leg"
	hcDropStarted    = "the worker began to drop the request"
)

// hcAnswer is how a worker answers a request whose body it has read to the end.
type hcAnswer int

const (
	// hcPending: nothing before the first release; then the head and the whole body.
	hcPending hcAnswer = iota + 1
	// hcHeadLater: the head alone at the first release, the body at the second.
	hcHeadLater
	// hcStreaming: the head and the first event at once; the rest of the body at the first release.
	hcStreaming
	// hcStreamingSplit: the head and the first half of the first event at once, its second half at the second
	// release, the rest of the body at the first release.
	hcStreamingSplit
	// hcDrop: the generation ends, then its worker closes the connection without a response.
	hcDrop
	// hcDropLater: as hcDrop, but only at the first release; a worker whose request context ends first reports so.
	hcDropLater
)

// hcWorker is a loopback HTTP worker whose answer the test releases step by step. Its handler waits for each release
// or for the end of its own request context, which ends when the supervisor closes the worker leg.
type hcWorker struct {
	name     string
	addr     string
	log      *hcLog
	answer   hcAnswer
	requests atomic.Int32
	arrived  chan string
	// headSent is closed when an hcHeadLater answer has flushed its head.
	headSent chan struct{}
	gates    [2]chan struct{}
	release  [2]func()
	// result receives how the first request's handler ended; done is closed after that.
	result chan string
	done   chan struct{}
	finish func(string)
	// onDrop runs before an hcDrop worker closes its connection.
	onDrop func()
}

func startHCWorker(t *testing.T, name string, log *hcLog, answer hcAnswer) *hcWorker {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := &hcWorker{
		name: name, addr: ln.Addr().String(), log: log, answer: answer,
		arrived: make(chan string, 1), headSent: make(chan struct{}), result: make(chan string, 1), done: make(chan struct{}),
		onDrop: func() {},
	}
	var doneOnce sync.Once
	w.finish = func(outcome string) {
		doneOnce.Do(func() {
			w.result <- outcome
			close(w.done)
		})
	}
	for i := range w.gates {
		gate := make(chan struct{})
		w.gates[i] = gate
		w.release[i] = sync.OnceFunc(func() { close(gate) })
	}
	srv := &http.Server{Handler: http.HandlerFunc(w.serve)}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		w.releaseAll()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("worker %s did not shut down within 5s: %v", name, errors.Join(err, srv.Close()))
		}
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return w
}

func (w *hcWorker) releaseAll() {
	for _, release := range w.release {
		release()
	}
}

// echo is the value of the X-Probe-Echo field of this worker's answers.
func (w *hcWorker) echo() string { return "half-close-worker-" + w.name }

// serve reads the relayed request, signals its arrival and answers as w.answer says. It reports "complete" only when
// every write and flush of the whole answer succeeded while its request context was still alive.
func (w *hcWorker) serve(rw http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.finish("request body: " + err.Error())
		return
	}
	w.requests.Add(1)
	select {
	case w.arrived <- string(body):
	default:
	}
	rc := http.NewResponseController(rw)
	if w.answer == hcDrop || w.answer == hcDropLater {
		if w.answer == hcDropLater {
			select {
			case <-w.gates[0]:
			case <-r.Context().Done():
				w.finish("abandoned before its drop")
				return
			}
			w.log.add(hcDropStarted)
		}
		w.onDrop()
		conn, _, err := rc.Hijack()
		if err != nil {
			w.finish("hijack: " + err.Error())
			return
		}
		_ = conn.Close()
		w.finish("dropped")
		return
	}
	outcome := func() string {
		head := func() error {
			rw.Header().Set("Content-Type", "text/event-stream")
			rw.Header().Set("X-Probe-Echo", w.echo())
			rw.WriteHeader(http.StatusOK)
			return rc.Flush()
		}
		send := func(p []byte) error {
			if _, err := rw.Write(p); err != nil {
				return err
			}
			return rc.Flush()
		}
		events := func(from int) error {
			for i := from; i < hcChunks; i++ {
				if err := send(hcEvent(i)); err != nil {
					return err
				}
			}
			return nil
		}
		wait := func(gate chan struct{}) bool {
			select {
			case <-gate:
				return true
			case <-r.Context().Done():
				return false
			}
		}
		switch w.answer {
		case hcPending:
			if !wait(w.gates[0]) {
				return "abandoned before its answer"
			}
			if err := head(); err != nil {
				return "head: " + err.Error()
			}
			if err := events(0); err != nil {
				return "body: " + err.Error()
			}
		case hcHeadLater:
			if !wait(w.gates[0]) {
				return "abandoned before its head"
			}
			if err := head(); err != nil {
				return "head: " + err.Error()
			}
			close(w.headSent)
			if !wait(w.gates[1]) {
				return "abandoned after its head"
			}
			if err := events(0); err != nil {
				return "body: " + err.Error()
			}
		case hcStreaming, hcStreamingSplit:
			if err := head(); err != nil {
				return "head: " + err.Error()
			}
			first := []byte(hcFirstEvent)
			if w.answer == hcStreamingSplit {
				half := len(first) / 2
				if err := send(first[:half]); err != nil {
					return "first half of the first event: " + err.Error()
				}
				if !wait(w.gates[1]) {
					return "abandoned inside its first event"
				}
				first = first[half:]
			}
			if err := send(first); err != nil {
				return "first event: " + err.Error()
			}
			if !wait(w.gates[0]) {
				return "abandoned during its stream"
			}
			if err := events(1); err != nil {
				return "body: " + err.Error()
			}
		}
		if err := r.Context().Err(); err != nil {
			return "request context ended at the end of the answer: " + err.Error()
		}
		return "complete"
	}()
	if outcome == "complete" {
		w.log.add(hcWorkerFinished)
	}
	w.finish(outcome)
}

// hcGen is one worker generation of the lease source.
type hcGen struct {
	name      workerlink.Generation
	worker    *hcWorker
	available atomic.Bool
	exited    chan struct{}
	end       func()
}

// newHCGen returns a generation served by worker, available as given.
func newHCGen(name string, worker *hcWorker, available bool) *hcGen {
	g := &hcGen{name: workerlink.Generation("half-close-" + name), worker: worker, exited: make(chan struct{})}
	g.end = sync.OnceFunc(func() { close(g.exited) })
	g.available.Store(available)
	return g
}

// hcHold stops the relay at one chosen point until it is released or, where the point can see it, the request's
// context ends.
type hcHold struct {
	on      bool
	held    chan struct{}
	gate    chan struct{}
	release func()
}

func newHCHold() *hcHold {
	h := &hcHold{held: make(chan struct{}, 1), gate: make(chan struct{})}
	h.release = sync.OnceFunc(func() { close(h.gate) })
	return h
}

func hcSignal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// hcSource is a lease source over the test's generations. It refuses a request for a lease while no generation other
// than the excluded one is available and alive, as the registry does when no generation is eligible.
type hcSource struct {
	gens []*hcGen
	log  *hcLog

	acquires atomic.Int32
	// acquired is signalled after every request for a lease.
	acquired chan struct{}

	// refusalAt, when positive, makes that request for a lease hold its refusal (refusal); refusalSawEnd is closed
	// when the request's context ends while it is held.
	refusalAt     int32
	refusal       *hcHold
	refusalSawEnd chan struct{}
	// handOver holds the hand-over of a connected lease inside the acquisition; firstUse holds the relay's first
	// operation on a handed-over lease; writes holds every write on a lease.
	handOver *hcHold
	firstUse *hcHold
	writes   *hcHold

	mu     sync.Mutex
	leases []*hcLease
}

func newHCSource(log *hcLog, gens ...*hcGen) *hcSource {
	return &hcSource{
		gens: gens, log: log, acquired: make(chan struct{}, 1),
		refusal: newHCHold(), refusalSawEnd: make(chan struct{}),
		handOver: newHCHold(), firstUse: newHCHold(), writes: newHCHold(),
	}
}

func (s *hcSource) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	n := s.acquires.Add(1)
	defer hcSignal(s.acquired)
	if n == s.refusalAt {
		hcSignal(s.refusal.held)
		select {
		case <-ctx.Done():
			close(s.refusalSawEnd)
			return nil, ctx.Err()
		case <-s.refusal.gate:
		}
		return nil, workerlink.ErrUnavailable
	}
	var g *hcGen
	for _, c := range s.gens {
		select {
		case <-c.exited:
			continue
		default:
		}
		if c.available.Load() && c.name != exclude {
			g = c
			break
		}
	}
	if g == nil {
		return nil, workerlink.ErrUnavailable
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp4", g.worker.addr)
	if err != nil {
		return nil, err
	}
	if s.handOver.on {
		hcSignal(s.handOver.held)
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return nil, ctx.Err()
		case <-s.handOver.gate:
		}
	}
	l := &hcLease{Conn: conn, gen: g, src: s, closed: make(chan struct{})}
	s.mu.Lock()
	s.leases = append(s.leases, l)
	s.mu.Unlock()
	return l, nil
}

// leased returns the leases handed out so far, to every generation or, when gen is not empty, to that one.
func (s *hcSource) leased(gen workerlink.Generation) []*hcLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*hcLease
	for _, l := range s.leases {
		if gen == "" || l.gen.name == gen {
			out = append(out, l)
		}
	}
	return out
}

// teardown lets every hold of the fixture go and closes every lease, so that no handler can stay on a worker leg.
func (s *hcSource) teardown() {
	for _, h := range []*hcHold{s.refusal, s.handOver, s.firstUse, s.writes} {
		h.release()
	}
	for _, g := range s.gens {
		g.worker.releaseAll()
	}
	for _, l := range s.leased("") {
		_ = l.Close()
	}
}

// hcLease is one connection to a worker; it records when the supervisor closes it and how many bytes were written.
type hcLease struct {
	net.Conn
	gen       *hcGen
	src       *hcSource
	closed    chan struct{}
	closeOnce sync.Once
	useOnce   sync.Once
	written   atomic.Int64
	// closedAt is the position of the lease's close in the source's log; it is set before closed is closed.
	closedAt int
}

// use holds the relay's first operation on the lease while the source holds first uses; a close ends the hold.
func (l *hcLease) use() {
	if !l.src.firstUse.on {
		return
	}
	l.useOnce.Do(func() {
		hcSignal(l.src.firstUse.held)
		select {
		case <-l.src.firstUse.gate:
		case <-l.closed:
		}
	})
}

func (l *hcLease) Generation() workerlink.Generation { l.use(); return l.gen.name }
func (l *hcLease) PID() int                          { l.use(); return hcWorkerPID }
func (l *hcLease) Authority() string                 { l.use(); return l.gen.worker.addr }
func (l *hcLease) Exited() <-chan struct{}           { l.use(); return l.gen.exited }
func (l *hcLease) LocalAddr() net.Addr               { l.use(); return l.Conn.LocalAddr() }
func (l *hcLease) RemoteAddr() net.Addr              { l.use(); return l.Conn.RemoteAddr() }

func (l *hcLease) SetDeadline(d time.Time) error      { l.use(); return l.Conn.SetDeadline(d) }
func (l *hcLease) SetReadDeadline(d time.Time) error  { l.use(); return l.Conn.SetReadDeadline(d) }
func (l *hcLease) SetWriteDeadline(d time.Time) error { l.use(); return l.Conn.SetWriteDeadline(d) }

func (l *hcLease) Read(p []byte) (int, error) {
	l.use()
	return l.Conn.Read(p)
}

// Write holds the write while the source holds writes; a close of the lease ends the wait.
func (l *hcLease) Write(p []byte) (int, error) {
	l.use()
	if l.src.writes.on {
		hcSignal(l.src.writes.held)
		select {
		case <-l.src.writes.gate:
		case <-l.closed:
			return 0, net.ErrClosed
		}
	}
	n, err := l.Conn.Write(p)
	l.written.Add(int64(n))
	return n, err
}

func (l *hcLease) Close() error {
	l.closeOnce.Do(func() {
		l.closedAt = l.src.log.add(hcLeaseClosed)
		close(l.closed)
	})
	return l.Conn.Close()
}

func (l *hcLease) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

// hcObserved signals when the production handler has returned, by panic or not, for the first request it served,
// after running onReturn.
type hcObserved struct {
	inner    http.Handler
	returned chan struct{}
	once     sync.Once
	onReturn func()
	// onDeadline, when set, is told every read deadline the handler requests through its response's controller.
	onDeadline func(time.Time)
}

func (h *hcObserved) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer h.once.Do(func() {
		if h.onReturn != nil {
			h.onReturn()
		}
		close(h.returned)
	})
	if h.onDeadline != nil {
		w = hcDeadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w), record: h.onDeadline}
	}
	h.inner.ServeHTTP(w, r)
}

// hcDeadlineWriter passes a handler's response through unchanged and records the read deadlines it requests.
type hcDeadlineWriter struct {
	http.ResponseWriter
	rc     *http.ResponseController
	record func(time.Time)
}

func (w hcDeadlineWriter) FlushError() error                  { return w.rc.Flush() }
func (w hcDeadlineWriter) SetWriteDeadline(d time.Time) error { return w.rc.SetWriteDeadline(d) }
func (w hcDeadlineWriter) Unwrap() http.ResponseWriter        { return w.ResponseWriter }
func (w hcDeadlineWriter) SetReadDeadline(d time.Time) error {
	w.record(d)
	return w.rc.SetReadDeadline(d)
}

func (h *hcObserved) hasReturned() bool {
	select {
	case <-h.returned:
		return true
	default:
		return false
	}
}

// hcServe runs supervisor.Serve with the production lease relay in front of src and returns the public address and
// the handler's return signal.
func hcServe(t *testing.T, src *hcSource) (string, *hcObserved) {
	t.Helper()
	return hcServeWrapped(t, src, nil, nil, nil)
}

// hcServeWrapped is hcServe with the public listener wrapped by wrap, onReturn run at the handler's return and
// onDeadline told every read deadline the handler requests, when they are not nil. Its cleanup lets every hold of the fixture go and closes every lease before it waits, for at most
// hcTeardown, for the listener to return.
func hcServeWrapped(t *testing.T, src *hcSource, wrap func(net.Listener) net.Listener, onReturn func(), onDeadline func(time.Time)) (string, *hcObserved) {
	t.Helper()
	var cfg net.ListenConfig
	ln, err := cfg.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := supervisor.NewLeaseRelay(ln.Addr().(*net.TCPAddr).Port, src, hcToken, "1.2.3", 37, nil, nil)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	h := &hcObserved{inner: inner, returned: make(chan struct{}), onReturn: onReturn, onDeadline: onDeadline}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	served := make(chan error, 1)
	public := net.Listener(ln)
	if wrap != nil {
		public = wrap(ln)
	}
	go func() { served <- supervisor.Serve(ctx, public, h) }()
	t.Cleanup(func() {
		src.teardown()
		cancel()
		timer := time.NewTimer(hcTeardown)
		defer timer.Stop()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("public listener: %v", err)
			}
		case <-timer.C:
			t.Errorf("the public listener did not return within %v of its cancellation, with every hold released and every lease closed (its handler returned: %t)", hcTeardown, h.hasReturned())
		}
		_ = ln.Close()
	})
	return ln.Addr().String(), h
}

// hcDial opens the client connection; it stays open, read side included, until the test ends, so that every byte the
// listener writes after the client's half-close would reach it.
func hcDial(t *testing.T, public string) *net.TCPConn {
	t.Helper()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp4", public)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(hcGuard)); err != nil {
		t.Fatal(err)
	}
	return conn.(*net.TCPConn)
}

// hcReadEnd reads until the connection ends or bound passes and returns the bytes read and how the read ended.
func hcReadEnd(conn net.Conn, r io.Reader, bound time.Duration) ([]byte, string) {
	_ = conn.SetReadDeadline(time.Now().Add(bound))
	data, err := io.ReadAll(r)
	_ = conn.SetReadDeadline(time.Now().Add(hcGuard))
	return data, hcEndKind(err)
}

func hcEndKind(err error) string {
	switch {
	case err == nil:
		return hcOrderly
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "no end within the bound"
	}
	return "read error: " + err.Error()
}

// hcAwait waits for signal; it fails the test early when the production handler returned first, or when hcGuard
// passes.
func hcAwait(t *testing.T, signal <-chan struct{}, h *hcObserved, what string) {
	t.Helper()
	timer := time.NewTimer(hcGuard)
	defer timer.Stop()
	select {
	case <-signal:
		return
	default:
	}
	select {
	case <-signal:
	case <-h.returned:
		select {
		case <-signal:
		default:
			t.Fatalf("the supervisor's handler returned before %s", what)
		}
	case <-timer.C:
		t.Fatalf("%s did not happen within %v", what, hcGuard)
	}
}

// hcAwaitArrival waits for the worker to have read the whole relayed request and returns its body.
func hcAwaitArrival(t *testing.T, w *hcWorker, h *hcObserved) string {
	t.Helper()
	timer := time.NewTimer(hcGuard)
	defer timer.Stop()
	select {
	case body := <-w.arrived:
		return body
	case <-h.returned:
		select {
		case body := <-w.arrived:
			return body
		default:
			t.Fatalf("the supervisor's handler returned before worker %s received the request", w.name)
		}
	case <-timer.C:
		t.Fatalf("worker %s received no request within %v", w.name, hcGuard)
	}
	return ""
}

func hcResult(t *testing.T, w *hcWorker) string {
	t.Helper()
	timer := time.NewTimer(hcGuard)
	defer timer.Stop()
	select {
	case r := <-w.result:
		return r
	case <-timer.C:
		t.Fatalf("worker %s's handler did not end within %v", w.name, hcGuard)
	}
	return ""
}

func hcRequest(public, request string) string { return strings.ReplaceAll(request, "{own}", public) }

// hcRequests are the three framings of a complete request.
var hcRequests = map[string]struct{ request, body string }{
	"a body framed by Content-Length": {request: "POST /v1/messages HTTP/1.1\r\nHost: {own}\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}", body: "{}"},
	"a chunked body and its trailer":  {request: "POST /v1/messages HTTP/1.1\r\nHost: {own}\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n0\r\nX-Trailer: end\r\n\r\n", body: "{}"},
	"a request without a body":        {request: "GET /v1/models HTTP/1.1\r\nHost: {own}\r\n\r\n"},
}

// hcStreamHead reads the relayed head of a chunked 200 from w and fails the test otherwise. The body is not closed:
// closing it would read the stream to its end; its frames are read by hand with hcReadFrames instead.
func hcStreamHead(t *testing.T, br *bufio.Reader, w *hcWorker) {
	t.Helper()
	resp, err := http.ReadResponse(br, nil) //nolint:bodyclose // The body is the open stream; see the comment above.
	if err != nil {
		t.Fatalf("read the relayed head: %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Probe-Echo") != w.echo() || !slices.Equal(resp.TransferEncoding, []string{"chunked"}) {
		t.Fatalf("relayed head %q, echo %q, transfer encoding %q; want worker %s's chunked 200", resp.Status, resp.Header.Get("X-Probe-Echo"), resp.TransferEncoding, w.name)
	}
}

// hcReadFrames reads whole chunk frames, size line, data and closing CRLF, appending their data to payload until it
// equals want, however the relay split it into chunks. It fails the test on a frame that is not a data chunk or on
// data that is not a prefix of want, and it takes no byte after the last frame from the reader.
func hcReadFrames(t *testing.T, br *bufio.Reader, payload *[]byte, want string) {
	t.Helper()
	for len(*payload) < len(want) {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read a chunk size line after %q: %v", *payload, err)
		}
		size, err := strconv.ParseUint(strings.TrimSuffix(line, "\r\n"), 16, 31)
		if err != nil || size == 0 || !strings.HasSuffix(line, "\r\n") {
			t.Fatalf("chunk size line %q after %q; want the size of a data chunk", line, *payload)
		}
		if size > uint64(len(want)-len(*payload)) {
			t.Fatalf("chunk of %d bytes after %q; at most %d bytes of %q remain", size, *payload, len(want)-len(*payload), want)
		}
		frame := make([]byte, size+2)
		if _, err := io.ReadFull(br, frame); err != nil {
			t.Fatalf("read a chunk of %d bytes: %v", size, err)
		}
		if !bytes.HasSuffix(frame, []byte("\r\n")) {
			t.Fatalf("chunk %q does not end with CRLF", frame)
		}
		*payload = append(*payload, frame[:size]...)
		if !strings.HasPrefix(want, string(*payload)) {
			t.Fatalf("relayed data %q; want a prefix of %q", *payload, want)
		}
	}
}

// hcHalfClosed is what one case observed around the client's half-close.
type hcHalfClosed struct {
	// ClientEnd is how the client's read ended after its half-close, and BytesAfterHalfClose how many bytes it
	// received from the half-close to that end and after it, until the handler had returned.
	ClientEnd           string
	BytesAfterHalfClose int
	// LeaseOpenAtClientEnd records that the worker leg was still open when the client saw the listener's end.
	LeaseOpenAtClientEnd bool
	// Worker is how the worker's handler ended.
	Worker string
	// Order is the order of the worker's end and the leg's close.
	Order []string
}

// TestHalfCloseAfterCompleteRequest sends a complete request and half-closes the client connection once its exchange
// with the worker is committed and the worker holds its answer, in the measured cases: answer pending, head after the
// half-close, stream in progress (its first event in one chunk or split over two). The listener must end the client
// connection in order before the worker answers and send nothing further: no byte when the answer was pending or its
// head unsent, and after the first event of a stream no further byte, so no terminating chunk. It must keep the
// worker leg open until the worker has finished its answer and then close it. The control row, a client that stays,
// receives the worker's whole answer through the same fixture.
func TestHalfCloseAfterCompleteRequest(t *testing.T) {
	want := hcHalfClosed{
		ClientEnd: hcOrderly, LeaseOpenAtClientEnd: true, Worker: "complete",
		Order: []string{hcWorkerFinished, hcLeaseClosed},
	}
	tests := map[string]struct {
		framing string
		answer  hcAnswer
	}{
		"success: half-close after a body framed by Content-Length while the answer is pending": {framing: "a body framed by Content-Length", answer: hcPending},
		"success: half-close after a chunked body and its trailer while the answer is pending":  {framing: "a chunked body and its trailer", answer: hcPending},
		"success: half-close after a request without a body while the answer is pending":        {framing: "a request without a body", answer: hcPending},
		"success: half-close before the worker sends its head":                                  {framing: "a body framed by Content-Length", answer: hcHeadLater},
		"success: half-close after the first event of a stream":                                 {framing: "a body framed by Content-Length", answer: hcStreaming},
		"success: half-close after the first event of a stream that arrived in two chunks":      {framing: "a body framed by Content-Length", answer: hcStreamingSplit},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &hcLog{}
			worker := startHCWorker(t, "a", log, tc.answer)
			src := newHCSource(log, newHCGen("a", worker, true))
			public, h := hcServe(t, src)
			conn := hcDial(t, public)
			br := bufio.NewReader(conn)
			req := hcRequests[tc.framing]
			if _, err := io.WriteString(conn, hcRequest(public, req.request)); err != nil {
				t.Fatal(err)
			}
			if got := hcAwaitArrival(t, worker, h); got != req.body {
				t.Fatalf("the worker received the body %q; want %q", got, req.body)
			}
			switch tc.answer {
			case hcStreaming:
				hcStreamHead(t, br, worker)
				var payload []byte
				hcReadFrames(t, br, &payload, hcFirstEvent)
			case hcStreamingSplit:
				hcStreamHead(t, br, worker)
				var payload []byte
				hcReadFrames(t, br, &payload, hcFirstEvent[:len(hcFirstEvent)/2])
				worker.release[1]()
				hcReadFrames(t, br, &payload, hcFirstEvent)
			}
			if n := br.Buffered(); n != 0 {
				t.Fatalf("%d bytes beyond what the worker had sent arrived before the half-close; want none", n)
			}
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			var got hcHalfClosed
			after, end := hcReadEnd(conn, br, hcEndBound)
			got.ClientEnd = end
			leases := src.leased("")
			if len(leases) != 1 {
				t.Fatalf("the source handed out %d leases; want 1", len(leases))
			}
			got.LeaseOpenAtClientEnd = !leases[0].isClosed()
			worker.release[0]()
			if tc.answer == hcHeadLater {
				// A worker that was abandoned before its head never sends it; its result says so below.
				select {
				case <-worker.headSent:
					worker.release[1]()
				case <-worker.done:
				case <-time.After(hcGuard):
					t.Fatalf("the worker neither sent its head nor ended within %v", hcGuard)
				}
			}
			got.Worker = hcResult(t, worker)
			hcAwait(t, leases[0].closed, h, "the close of the worker leg")
			hcAwait(t, h.returned, h, "the handler's return")
			// Nothing written after the handler's return can belong to this request either.
			more, _ := hcReadEnd(conn, br, time.Second)
			got.BytesAfterHalfClose = len(after) + len(more)
			got.Order = log.all()
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("half-close after a complete request (-want +got):\n%s\nbytes after the half-close: %q", diff, append(after, more...))
			}
		})
	}
	t.Run("success: a client that stays receives the worker's whole answer", func(t *testing.T) {
		log := &hcLog{}
		worker := startHCWorker(t, "a", log, hcPending)
		src := newHCSource(log, newHCGen("a", worker, true))
		public, h := hcServe(t, src)
		conn := hcDial(t, public)
		if _, err := io.WriteString(conn, hcRequest(public, hcRequests["a body framed by Content-Length"].request)); err != nil {
			t.Fatal(err)
		}
		hcAwaitArrival(t, worker, h)
		worker.release[0]()
		got := hcReadWhole(t, conn, worker)
		hcAwait(t, h.returned, h, "the handler's return")
		got.Order = log.all()
		if diff := gocmp.Diff(hcWhole(worker), got); diff != "" {
			t.Errorf("answer to a client that stays (-want +got):\n%s", diff)
		}
	})
}

// hcStayed is what a client that stays receives.
type hcStayed struct {
	Status         int
	Echo           string
	Close          bool
	BodyIsTheWhole bool
	Worker         string
	Order          []string
}

// hcWhole is the whole answer of w as a client that stays receives it, with its order of events.
func hcWhole(w *hcWorker) hcStayed {
	return hcStayed{Status: http.StatusOK, Echo: w.echo(), Close: true, BodyIsTheWhole: true, Worker: "complete", Order: []string{hcWorkerFinished, hcLeaseClosed}}
}

// hcReadWhole reads one complete relayed answer of w; Order is left to the caller.
func hcReadWhole(t *testing.T, conn net.Conn, w *hcWorker) hcStayed {
	t.Helper()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read the relayed answer: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read the relayed answer's body: %v", err)
	}
	var whole bytes.Buffer
	for i := range hcChunks {
		whole.Write(hcEvent(i))
	}
	return hcStayed{
		Status: resp.StatusCode, Echo: resp.Header.Get("X-Probe-Echo"), Close: resp.Close,
		BodyIsTheWhole: bytes.Equal(body, whole.Bytes()), Worker: hcResult(t, w),
	}
}

// TestHalfCloseBeforeDelivery half-closes a complete request while no worker generation is eligible, so before any
// lease was handed to the relay. The relay's first request for a lease holds its refusal; the half-close is sent
// while it is held. The listener must end the client connection in order and send nothing; only once that end, the
// listener's observation of the half-close, has reached the client is the generation made eligible and the refusal
// let go. The half-close stops new delivery attempts: the relay asks for no further lease, so a relay that went on
// retrying until its attempts ran out fails, and the generation is never handed the request.
func TestHalfCloseBeforeDelivery(t *testing.T) {
	type waited struct {
		ClientEnd   string
		ClientBytes int
		Leases      int
		// Acquires counts the relay's requests for a lease.
		Acquires int32
	}
	tests := map[string]struct{ framing string }{
		"error: half-close after a body framed by Content-Length ends the delivery attempts": {framing: "a body framed by Content-Length"},
		"error: half-close after a request without a body ends the delivery attempts":        {framing: "a request without a body"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &hcLog{}
			worker := startHCWorker(t, "a", log, hcPending)
			gen := newHCGen("a", worker, false)
			src := newHCSource(log, gen)
			src.refusalAt = 1
			public, h := hcServe(t, src)
			conn := hcDial(t, public)
			br := bufio.NewReader(conn)
			if _, err := io.WriteString(conn, hcRequest(public, hcRequests[tc.framing].request)); err != nil {
				t.Fatal(err)
			}
			hcAwait(t, src.refusal.held, h, "the relay's first request for a lease")
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			var got waited
			data, end := hcReadEnd(conn, br, hcEndBound)
			got.ClientEnd, got.ClientBytes = end, len(data)
			gen.available.Store(true)
			src.refusal.release()
			hcAwait(t, h.returned, h, "the handler's return")
			got.Leases, got.Acquires = len(src.leased("")), src.acquires.Load()
			if diff := gocmp.Diff(waited{ClientEnd: hcOrderly, Acquires: 1}, got); diff != "" {
				t.Errorf("half-close before delivery (-want +got):\n%s\nrequests for a lease: %d", diff, src.acquires.Load())
			}
		})
	}
}

// TestHalfCloseAtTheDeliveryBoundary fixes the point at which an exchange is committed: the successful hand-over of a
// lease to the relay. A half-close while the hand-over is still held inside the source's acquisition stops delivery;
// a half-close after it, while the relay's first operation on the handed-over lease is held, or its first write,
// leaves the exchange to run to the worker's end of its answer. In every row no request byte has reached the worker
// when the client half-closes.
func TestHalfCloseAtTheDeliveryBoundary(t *testing.T) {
	type boundary struct {
		ClientEnd           string
		BytesAfterHalfClose int
		// LeaseOpenAtClientEnd records that a handed-over lease was still open when the client saw the listener's end.
		LeaseOpenAtClientEnd bool
		Delivered            string
		Worker               string
		// BytesToTheWorker counts the bytes written on every lease when no delivery is wanted; LeasesOpen counts the
		// leases still open at the handler's return.
		BytesToTheWorker int64
		LeasesOpen       int
		Order            []string
	}
	const (
		inAcquisition = iota + 1
		atFirstUse
		atFirstWrite
	)
	preserved := boundary{
		ClientEnd: hcOrderly, LeaseOpenAtClientEnd: true, Delivered: "{}", Worker: "complete",
		Order: []string{hcWorkerFinished, hcLeaseClosed},
	}
	tests := map[string]struct {
		hold int
		want boundary
	}{
		"error: half-close while the acquisition has not handed the lease over stops delivery": {
			hold: inAcquisition, want: boundary{ClientEnd: hcOrderly, Worker: "never received the request"},
		},
		"success: half-close while the relay's first operation on the handed-over lease is held preserves the exchange": {
			hold: atFirstUse, want: preserved,
		},
		"success: half-close while the relay's first write on the handed-over lease is held preserves the exchange": {
			hold: atFirstWrite, want: preserved,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &hcLog{}
			worker := startHCWorker(t, "a", log, hcPending)
			src := newHCSource(log, newHCGen("a", worker, true))
			hold := map[int]*hcHold{inAcquisition: src.handOver, atFirstUse: src.firstUse, atFirstWrite: src.writes}[tc.hold]
			hold.on = true
			public, h := hcServe(t, src)
			conn := hcDial(t, public)
			br := bufio.NewReader(conn)
			if _, err := io.WriteString(conn, hcRequest(public, hcRequests["a body framed by Content-Length"].request)); err != nil {
				t.Fatal(err)
			}
			hcAwait(t, hold.held, h, "the hold of the delivery")
			if err := conn.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			var got boundary
			after, end := hcReadEnd(conn, br, hcEndBound)
			got.ClientEnd = end
			if leases := src.leased(""); len(leases) == 1 {
				got.LeaseOpenAtClientEnd = !leases[0].isClosed()
			}
			hold.release()
			got.Worker = "never received the request"
			select {
			case got.Delivered = <-worker.arrived:
			case <-h.returned:
				// The supervisor gave the request up; a worker that received it all the same is reported below.
				select {
				case got.Delivered = <-worker.arrived:
				default:
				}
			case <-time.After(hcGuard):
				t.Fatalf("the worker received no request and the handler did not return within %v", hcGuard)
			}
			if got.Delivered != "" {
				worker.release[0]()
				got.Worker = hcResult(t, worker)
			}
			hcAwait(t, h.returned, h, "the handler's return")
			for _, l := range src.leased("") {
				got.BytesToTheWorker += l.written.Load()
				if !l.isClosed() {
					got.LeasesOpen++
				}
			}
			got.Order = log.all()
			if tc.hold == inAcquisition {
				// A lease handed over after the half-close may exist, closed unused; whether it does is the
				// implementation's choice, so only its bytes and its close are decided.
				got.Order = nil
			} else {
				// The preserved rows deliver; their bytes are the request itself.
				got.BytesToTheWorker = 0
			}
			more, _ := hcReadEnd(conn, br, time.Second)
			got.BytesAfterHalfClose = len(after) + len(more)
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("half-close at the delivery boundary (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHalfCloseAfterAFailedDelivery lets the first generation take the request and fail before any response head,
// ending its generation, so that the relay may resend it once to a replacement that is not yet eligible. The relay's
// next request for a lease, its second, holds its refusal. A half-close sent while it is held stops new delivery
// attempts: there is no live exchange to preserve. Only once the listener's end has reached the client is the
// replacement made eligible and the refusal let go, and the relay asks for no further lease, so a relay that went on
// retrying until its attempts ran out fails, and the replacement never receives the buffered request. The control,
// without the half-close and with the same hold, has the replacement receive the request's body exactly once.
func TestHalfCloseAfterAFailedDelivery(t *testing.T) {
	type failed struct {
		ClientEnd      string
		ClientBytes    int
		FirstLeases    int
		FirstRequests  int32
		SecondLeases   int
		SecondRequests int32
		SecondBody     string
		// Acquires counts the relay's requests for a lease.
		Acquires int32
		Answer   hcStayed
	}
	tests := map[string]struct {
		halfClose bool
		want      func(second *hcWorker) failed
	}{
		"error: half-close after the first generation failed sends nothing to the replacement": {
			halfClose: true,
			want: func(*hcWorker) failed {
				return failed{ClientEnd: hcOrderly, FirstLeases: 1, FirstRequests: 1, Acquires: 2}
			},
		},
		"success: without a half-close the replacement receives the request exactly once": {
			want: func(second *hcWorker) failed {
				answer := hcWhole(second)
				answer.Order = nil
				return failed{FirstLeases: 1, FirstRequests: 1, SecondLeases: 1, SecondRequests: 1, SecondBody: "{}", Acquires: 3, Answer: answer}
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &hcLog{}
			first := startHCWorker(t, "a", log, hcDrop)
			second := startHCWorker(t, "b", log, hcPending)
			genA, genB := newHCGen("a", first, true), newHCGen("b", second, false)
			first.onDrop = genA.end
			src := newHCSource(log, genA, genB)
			// The first request for a lease reaches the first generation; the second, after its failure, is held.
			src.refusalAt = 2
			public, h := hcServe(t, src)
			conn := hcDial(t, public)
			br := bufio.NewReader(conn)
			if _, err := io.WriteString(conn, hcRequest(public, hcRequests["a body framed by Content-Length"].request)); err != nil {
				t.Fatal(err)
			}
			hcAwaitArrival(t, first, h)
			leases := src.leased(genA.name)
			if len(leases) != 1 {
				t.Fatalf("the first generation was handed %d leases; want 1", len(leases))
			}
			hcAwait(t, leases[0].closed, h, "the close of the failed worker leg")
			hcAwait(t, src.refusal.held, h, "the relay's request for a lease after the failure")
			var got failed
			if tc.halfClose {
				if err := conn.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				data, end := hcReadEnd(conn, br, hcEndBound)
				got.ClientEnd, got.ClientBytes = end, len(data)
			}
			genB.available.Store(true)
			src.refusal.release()
			if !tc.halfClose {
				got.SecondBody = hcAwaitArrival(t, second, h)
				second.release[0]()
				got.Answer = hcReadWhole(t, conn, second)
			}
			hcAwait(t, h.returned, h, "the handler's return")
			got.FirstLeases, got.FirstRequests = len(src.leased(genA.name)), first.requests.Load()
			got.SecondLeases, got.SecondRequests = len(src.leased(genB.name)), second.requests.Load()
			got.Acquires = src.acquires.Load()
			if diff := gocmp.Diff(tc.want(second), got); diff != "" {
				t.Errorf("half-close after a failed delivery (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHalfCloseDuringAnExchangeThatFails half-closes while a committed exchange is live and lets that exchange fail
// afterwards. The first generation holds the complete request; the client half-closes and must see the listener's
// orderly end, with no byte, while the worker leg is still open. Only then is the replacement made eligible and the
// first worker let go: it logs the start of its drop, its generation ends, and it closes its connection before any
// response head. The supervisor must close the first leg only after that start, so that a leg closed early does not
// pass for the worker's own failure. The half-close forbids a new delivery, so the failed leg is closed, the handler
// returns, and the relay asks for no further lease:
// the replacement never receives the request. The control, without the half-close and with the same order of
// events, has the replacement receive the request's body exactly once.
func TestHalfCloseDuringAnExchangeThatFails(t *testing.T) {
	type failing struct {
		ClientEnd string
		// ClientBytes counts the bytes the client received from its half-close until the handler had returned.
		ClientBytes int
		// LeaseOpenBeforeTheFailure records that the first worker leg was still open when the first worker was let go.
		LeaseOpenBeforeTheFailure bool
		// First is how the first worker's handler ended, and DropBeforeTheLegClosed records that the first worker
		// began to drop the request, before its generation ended, earlier than the supervisor closed its leg.
		First                  string
		DropBeforeTheLegClosed bool
		FirstLeases            int
		FirstRequests          int32
		SecondLeases           int
		SecondRequests         int32
		SecondBody             string
		// Acquires counts the relay's requests for a lease.
		Acquires int32
		Answer   hcStayed
	}
	tests := map[string]struct {
		halfClose bool
		want      func(second *hcWorker) failing
	}{
		"error: half-close during a committed exchange that then fails sends nothing to the replacement": {
			halfClose: true,
			want: func(*hcWorker) failing {
				return failing{ClientEnd: hcOrderly, LeaseOpenBeforeTheFailure: true, First: "dropped", DropBeforeTheLegClosed: true, FirstLeases: 1, FirstRequests: 1, Acquires: 1}
			},
		},
		"success: without a half-close the replacement receives the request of the failed exchange exactly once": {
			want: func(second *hcWorker) failing {
				answer := hcWhole(second)
				answer.Order = nil
				return failing{
					LeaseOpenBeforeTheFailure: true, First: "dropped", DropBeforeTheLegClosed: true, FirstLeases: 1, FirstRequests: 1,
					SecondLeases: 1, SecondRequests: 1, SecondBody: "{}", Acquires: 2, Answer: answer,
				}
			},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &hcLog{}
			first := startHCWorker(t, "a", log, hcDropLater)
			second := startHCWorker(t, "b", log, hcPending)
			genA, genB := newHCGen("a", first, true), newHCGen("b", second, false)
			first.onDrop = genA.end
			src := newHCSource(log, genA, genB)
			public, h := hcServe(t, src)
			conn := hcDial(t, public)
			br := bufio.NewReader(conn)
			if _, err := io.WriteString(conn, hcRequest(public, hcRequests["a body framed by Content-Length"].request)); err != nil {
				t.Fatal(err)
			}
			if body := hcAwaitArrival(t, first, h); body != "{}" {
				t.Fatalf("the first worker received the body %q; want %q", body, "{}")
			}
			leases := src.leased(genA.name)
			if len(leases) != 1 {
				t.Fatalf("the first generation was handed %d leases; want 1", len(leases))
			}
			var got failing
			var after []byte
			if tc.halfClose {
				if err := conn.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				after, got.ClientEnd = hcReadEnd(conn, br, hcEndBound)
			}
			got.LeaseOpenBeforeTheFailure = !leases[0].isClosed()
			genB.available.Store(true)
			first.release[0]()
			got.First = hcResult(t, first)
			hcAwait(t, leases[0].closed, h, "the close of the failed worker leg")
			if dropAt := slices.Index(log.all(), hcDropStarted); dropAt >= 0 {
				got.DropBeforeTheLegClosed = dropAt < leases[0].closedAt
			}
			// A relay that resends reaches the replacement; its arrival is reported below rather than waited for.
			select {
			case got.SecondBody = <-second.arrived:
			case <-h.returned:
				select {
				case got.SecondBody = <-second.arrived:
				default:
				}
			case <-time.After(hcGuard):
				t.Fatalf("the replacement received no request and the handler did not return within %v", hcGuard)
			}
			if got.SecondBody != "" {
				second.release[0]()
				if !tc.halfClose {
					got.Answer = hcReadWhole(t, conn, second)
				}
			}
			hcAwait(t, h.returned, h, "the handler's return")
			if tc.halfClose {
				more, _ := hcReadEnd(conn, br, time.Second)
				got.ClientBytes = len(after) + len(more)
			}
			got.FirstLeases, got.FirstRequests = len(src.leased(genA.name)), first.requests.Load()
			got.SecondLeases, got.SecondRequests = len(src.leased(genB.name)), second.requests.Load()
			got.Acquires = src.acquires.Load()
			if diff := gocmp.Diff(tc.want(second), got); diff != "" {
				t.Errorf("half-close during an exchange that fails (-want +got):\n%s", diff)
			}
		})
	}
}

// TestProvenDepartureStopsDelivery decides the departures a half-close is told apart from: a reset of the client
// connection, and an end of input before the request was complete. After either, no new delivery is attempted, and a
// worker leg already open is closed without waiting for the worker's answer, before or during a stream. A reset while
// the relay waits for a worker is observed on the server's side, by a request for a lease held across it, before the
// worker is made eligible; the relay asks for no lease after that.
func TestProvenDepartureStopsDelivery(t *testing.T) {
	type departed struct {
		// LeaseClosedBeforeAnswer records that the worker leg was closed while the worker still held its answer.
		LeaseClosedBeforeAnswer bool
		// Leases counts every lease handed out; Acquires every request for one.
		Leases   int
		Acquires int32
		// DepartureSeenInAcquisition records that the request for a lease held across the reset ended with the
		// request's context.
		DepartureSeenInAcquisition bool
		ClientEnd                  string
		ClientBytes                int
	}
	const (
		heldPending = iota + 1
		heldStream
		waiting
		incomplete
	)
	tests := map[string]struct {
		request string
		state   int
		want    departed
	}{
		"error: a reset while the worker holds a pending answer closes the worker leg": {
			request: hcRequests["a body framed by Content-Length"].request, state: heldPending,
			want: departed{LeaseClosedBeforeAnswer: true, Leases: 1, Acquires: 1},
		},
		"error: a reset after the first event of a stream closes the worker leg": {
			request: hcRequests["a body framed by Content-Length"].request, state: heldStream,
			want: departed{LeaseClosedBeforeAnswer: true, Leases: 1, Acquires: 1},
		},
		"error: a reset while no worker is eligible ends the delivery attempts": {
			request: hcRequests["a body framed by Content-Length"].request, state: waiting,
			want: departed{Acquires: 2, DepartureSeenInAcquisition: true},
		},
		"error: end of input inside a body framed by Content-Length delivers nothing": {
			request: "POST /v1/messages HTTP/1.1\r\nHost: {own}\r\nContent-Length: 10\r\n\r\n{\"a\"", state: incomplete,
			want: departed{ClientEnd: hcOrderly},
		},
		"error: end of input inside a chunked body delivers nothing": {
			request: "POST /v1/messages HTTP/1.1\r\nHost: {own}\r\nTransfer-Encoding: chunked\r\n\r\n2\r\n{}\r\n", state: incomplete,
			want: departed{ClientEnd: hcOrderly},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &hcLog{}
			answer := hcPending
			if tc.state == heldStream {
				answer = hcStreaming
			}
			worker := startHCWorker(t, "a", log, answer)
			gen := newHCGen("a", worker, tc.state != waiting)
			src := newHCSource(log, gen)
			if tc.state == waiting {
				// The second request for a lease holds its refusal until the request's context ends.
				src.refusalAt = 2
			}
			public, h := hcServe(t, src)
			conn := hcDial(t, public)
			br := bufio.NewReader(conn)
			if _, err := io.WriteString(conn, hcRequest(public, tc.request)); err != nil {
				t.Fatal(err)
			}
			var got departed
			switch tc.state {
			case heldPending:
				hcAwaitArrival(t, worker, h)
			case heldStream:
				hcAwaitArrival(t, worker, h)
				hcStreamHead(t, br, worker)
				var payload []byte
				hcReadFrames(t, br, &payload, hcFirstEvent)
			case waiting:
				hcAwait(t, src.refusal.held, h, "the relay's second request for a lease")
			}
			if tc.state == incomplete {
				if err := conn.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				data, end := hcReadEnd(conn, br, hcEndBound)
				got.ClientEnd, got.ClientBytes = end, len(data)
			} else {
				// A zero linger time turns the close into a reset.
				if err := conn.SetLinger(0); err != nil {
					t.Fatal(err)
				}
				if err := conn.Close(); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.state {
			case heldPending, heldStream:
				leases := src.leased("")
				if len(leases) != 1 {
					t.Fatalf("the source handed out %d leases; want 1", len(leases))
				}
				hcAwait(t, leases[0].closed, h, "the close of the worker leg after the reset")
				got.LeaseClosedBeforeAnswer = true
			case waiting:
				hcAwait(t, src.refusalSawEnd, h, "the end of the request's context in the held request for a lease")
				got.DepartureSeenInAcquisition = true
				// A worker that becomes eligible after the observed reset must not be reached.
				gen.available.Store(true)
			}
			hcAwait(t, h.returned, h, "the handler's return")
			got.Leases, got.Acquires = len(src.leased("")), src.acquires.Load()
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("proven departure (-want +got):\n%s", diff)
			}
		})
	}
}
