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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"runtime/debug"
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

// Every test of this package runs inside a synctest bubble with no real socket in it: the worker generations come from
// memLinks, an in-memory lease source with the admission rules of workerlink.Registry, and every lease is an in-memory
// pipe to a fake worker, so time is virtual and "nothing more happens" is proved by synctest.Wait, never by sleeping.
// The Unix-only TestRealRegistry runs representative cases through the real workerlink.Registry as well.

const (
	// publicAuthority is the Host a client sends to the supervisor's public listener.
	publicAuthority = "127.0.0.1:18764"
	// testVersion and testSupervisorPID are the supervisor identity the relay adds to the worker's health document.
	testVersion       = "9.9.9-relay-test"
	testSupervisorPID = 4242
	// attemptWait and attemptWaits are the source's pause between two delivery attempts and their number
	// (lib/commands.js:2360-2362,2389): the first attempt plus at most eighty more.
	attemptWait  = 50 * time.Millisecond
	attemptWaits = 80
	// watchdog only aborts a hang. It is virtual time: a relay waiting on its own timers runs them first.
	watchdog = time.Hour
	// workerWriteBudget bounds each write of the fake worker, so that a relay that never reads the worker's answer
	// shows as a failed write instead of a blocked bubble.
	workerWriteBudget = 10 * time.Minute
)

// The source's fixed answers (lib/commands.js:2279-2282,2432-2440), byte for byte as JSON.stringify writes them.
const (
	undeliveredBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway could not deliver this request to the shim worker; retry it shortly"}}`
	lostBody        = `{"type":"error","error":{"type":"api_error","message":"model-gateway lost the shim worker connection after this request reached the model; that answer cannot be recovered, and retrying automatically would pay for a second inference, so send it again yourself if you still want one"}}`
	unreadableBody  = `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway could not read the request body"}}`
)

// okAnswer is a complete worker answer on a leg that ends with the response.
const okAnswer = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"

// errStopping stands for the supervisor ending the waits between attempts, as the source's isStopped does
// (lib/commands.js:2359).
var errStopping = errors.New("supervisor stopping")

// errPrivateLeg is a worker-leg failure whose text names a private host, port and detail; none of it may reach a
// Lost report's error type.
var errPrivateLeg = errors.New("worker leg broke at secret-host.invalid:47123: private detail 7f3a")

// privateMarkers are the parts of errPrivateLeg and of the worker addresses that a sanitized error type never holds.
var privateMarkers = []string{"secret-host", "47123", "private detail", "7f3a", "127.0.0.1", "4100"}

// inBubble runs body in its own synctest bubble over the in-memory links and tears the world down inside it, also
// after t.Fatal.
func inBubble(t *testing.T, body func(t *testing.T, w *world)) {
	t.Helper()
	inBubbleWith(t, newMemLinks, body)
}

// inBubbleWith is inBubble over the links that mk makes for the world.
func inBubbleWith(t *testing.T, mk func(w *world) links, body func(t *testing.T, w *world)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		w := newWorld(t, mk)
		defer w.close()
		body(t, w)
	})
}

// reservation is what the tests do with the reservation of one generation; *workerlink.Reservation and memRes both
// provide it.
type reservation interface {
	Port() uint16
	Publish(pid int) error
	Retire()
	End() error
}

// links is the source of leases a world uses, with the reservation of new generations.
type links interface {
	workerlink.Source
	reserve(ctx context.Context, generation workerlink.Generation, port uint16) (reservation, error)
}

// await waits for a value on ch; the watchdog only aborts a hang.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(watchdog):
		t.Fatalf("%s did not happen within %v", what, watchdog)
		var zero T
		return zero
	}
}

// ready returns the value on ch when it is already there.
func ready[T any](ch <-chan T) (T, bool) {
	select {
	case v := <-ch:
		return v, true
	default:
		var zero T
		return zero, false
	}
}

// closed reports whether ch is closed.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// world is one supervisor's view of its worker generations: the real registry, a counting source in front of it, the
// fake workers behind it and the counters the tests compare.
type world struct {
	t     *testing.T
	links links
	src   *countingSource
	// done is closed by close, so that every parked fake ends.
	done chan struct{}
	// upstream counts the requests the fake workers submitted to their provider.
	upstream atomic.Int32

	mu    sync.Mutex
	gens  []*generation
	byKey map[workerlink.Generation]*generation
	// target is the generation last published at an address, the one a dial to that address reaches.
	target map[string]*generation
	// dialGate, when set, runs inside every successful dial before its connection is returned.
	dialGate func(ctx context.Context, g *generation) error
	waits    []time.Duration
	// events is the order of the relay's Acquire ("acquire") and Wait ("wait") calls.
	events []string
	// onWait, when set, runs at the start of every Wait call with its one-based number; an error it returns is
	// Wait's result.
	onWait func(n int) error
	// midWait, when set, runs halfway through every Wait call, while the relay is inside it; an error it returns is
	// Wait's result, so that a stop can arrive during a wait rather than at its start.
	midWait func(n int) error
	lost    []relay.Lost
}

func newWorld(t *testing.T, mk func(w *world) links) *world {
	w := &world{
		t:      t,
		done:   make(chan struct{}),
		byKey:  map[workerlink.Generation]*generation{},
		target: map[string]*generation{},
	}
	w.links = mk(w)
	w.src = &countingSource{inner: w.links, note: w.note}
	return w
}

// note records one call of the relay in the order the calls were made.
func (w *world) note(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event)
}

func (w *world) callOrder() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.events)
}

// options returns relay options wired to this world.
func (w *world) options() relay.Options {
	return relay.Options{
		Workers:       w.src,
		Lost:          w.recordLost,
		Wait:          w.wait,
		Version:       testVersion,
		SupervisorPID: testSupervisorPID,
	}
}

// handler constructs the relay for this world.
func (w *world) handler(t *testing.T) http.Handler {
	t.Helper()
	return newRelay(t, w.options())
}

func (w *world) close() {
	close(w.done)
	w.mu.Lock()
	gens := slices.Clone(w.gens)
	w.mu.Unlock()
	for _, g := range gens {
		g.end()
	}
}

func (w *world) recordLost(l relay.Lost) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lost = append(w.lost, l)
}

func (w *world) lostReports() []relay.Lost {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.lost)
}

func (w *world) wait(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.events = append(w.events, "wait")
	w.waits = append(w.waits, d)
	n := len(w.waits)
	hook, mid := w.onWait, w.midWait
	w.mu.Unlock()
	if hook != nil {
		if err := hook(n); err != nil {
			return err
		}
	}
	if mid != nil {
		half := time.NewTimer(d / 2)
		select {
		case <-half.C:
		case <-ctx.Done():
			half.Stop()
			return ctx.Err()
		}
		if err := mid(n); err != nil {
			return err
		}
		d -= d / 2
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *world) waitCalls() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.waits)
}

func (w *world) setOnWait(hook func(n int) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.onWait = hook
}

func (w *world) setMidWait(hook func(n int) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.midWait = hook
}

func (w *world) setDialGate(gate func(ctx context.Context, g *generation) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dialGate = gate
}

// dial is the registry's dial function: it reaches the generation last published at address.
func (w *world) dial(ctx context.Context, network, address string) (net.Conn, error) {
	w.mu.Lock()
	g := w.target[address]
	w.mu.Unlock()
	if g == nil {
		return nil, refused(network)
	}
	return w.dialTo(ctx, network, g)
}

// dialGeneration is the in-memory links' dial: it reaches the generation by name.
func (w *world) dialGeneration(ctx context.Context, name workerlink.Generation) (net.Conn, error) {
	w.mu.Lock()
	g := w.byKey[name]
	w.mu.Unlock()
	if g == nil {
		return nil, refused("tcp4")
	}
	return w.dialTo(ctx, "tcp4", g)
}

func refused(network string) error {
	return &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// dialTo opens one connection to the fake worker of g, after the dial gate.
func (w *world) dialTo(ctx context.Context, network string, g *generation) (net.Conn, error) {
	w.mu.Lock()
	gate := w.dialGate
	w.mu.Unlock()
	if g.dialErr != nil {
		return nil, refused(network)
	}
	if gate != nil {
		if err := gate(ctx, g); err != nil {
			return nil, err
		}
	}
	relayEnd, workerEnd := net.Pipe()
	c := g.accept(workerEnd)
	go g.worker(c)
	return relayEnd, nil
}

// generation is one fake worker process: its reservation, its behaviour for each connection and its counters.
type generation struct {
	w      *world
	name   workerlink.Generation
	pid    int
	res    reservation
	worker func(c *workerConn)
	// dialErr makes every dial to this generation fail as a refused connection.
	dialErr error
	// reads counts requests this worker read completely; submitted counts those it sent to its provider.
	reads     atomic.Int32
	submitted atomic.Int32

	mu      sync.Mutex
	conns   []*workerConn
	endOnce sync.Once
}

// reserve reserves a generation at port for process pid; worker runs for every connection that reaches it.
func (w *world) reserve(name workerlink.Generation, port uint16, pid int, worker func(c *workerConn)) *generation {
	w.t.Helper()
	res, err := w.links.reserve(w.t.Context(), name, port)
	if err != nil {
		w.t.Fatalf("Reserve(%q, %d): %v", name, port, err)
	}
	g := &generation{w: w, name: name, pid: pid, res: res, worker: worker}
	w.mu.Lock()
	w.gens = append(w.gens, g)
	w.byKey[name] = g
	w.mu.Unlock()
	return g
}

// start reserves and publishes a generation.
func (w *world) start(name workerlink.Generation, port uint16, pid int, worker func(c *workerConn)) *generation {
	w.t.Helper()
	g := w.reserve(name, port, pid, worker)
	g.publish()
	return g
}

func (g *generation) authority() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(g.res.Port())))
}

// publish makes the generation eligible. The address is mapped to it before Publish, because a dial of the real
// registry can follow Publish at once, and the mapping is taken back when Publish fails. The world's lock is held
// across the mapping, Publish and the rollback, and every dial reads the mapping under it, so that no dial can see a
// generation whose publication is not decided. Publish does no I/O and calls nothing of the world.
func (g *generation) publish() {
	g.w.mu.Lock()
	defer g.w.mu.Unlock()
	previous, had := g.w.target[g.authority()]
	g.w.target[g.authority()] = g
	if err := g.res.Publish(g.pid); err != nil {
		if had {
			g.w.target[g.authority()] = previous
		} else {
			delete(g.w.target, g.authority())
		}
		g.w.t.Errorf("Publish(%q, %d): %v", g.name, g.pid, err)
	}
}

// retire withdraws the generation's admission, as a drain does; the worker process stays alive.
func (g *generation) retire() { g.res.Retire() }

// end is the death of the worker process: the registry ends the generation and every socket the process held closes.
func (g *generation) end() {
	g.endOnce.Do(func() {
		if err := g.res.End(); err != nil {
			g.w.t.Errorf("End(%q): %v", g.name, err)
		}
		g.mu.Lock()
		conns := slices.Clone(g.conns)
		g.mu.Unlock()
		for _, c := range conns {
			c.close()
		}
	})
}

func (g *generation) accept(conn net.Conn) *workerConn {
	c := &workerConn{g: g, conn: conn, br: bufio.NewReader(conn), relayClosed: make(chan struct{}), finished: make(chan struct{})}
	g.mu.Lock()
	g.conns = append(g.conns, c)
	g.mu.Unlock()
	return c
}

// connections returns the connections that reached this generation, in order.
func (g *generation) connections() []*workerConn {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.conns)
}

// workerConn is the worker's end of one relayed connection.
type workerConn struct {
	g    *generation
	conn net.Conn
	br   *bufio.Reader

	// line and header are the request line and the header fields as the relay wrote them; body is the request body.
	line   string
	header textproto.MIMEHeader
	body   []byte
	// relayClosed is closed when the relay closed its end after the request; bytesAfter counts anything it sent
	// after the request instead.
	relayClosed chan struct{}
	bytesAfter  atomic.Int64
	// writeFailed records a write of the worker's answer that did not complete.
	writeFailed atomic.Bool
	// finished is closed when the worker's behaviour has returned.
	finished  chan struct{}
	closeOnce sync.Once
}

// readRequest reads the relayed request completely and counts it; it reports false when the connection ended first.
func (c *workerConn) readRequest() bool {
	tp := textproto.NewReader(c.br)
	line, err := tp.ReadLine()
	if err != nil {
		return false
	}
	if line == "" {
		c.g.w.t.Errorf("the relay sent an empty request line")
		return false
	}
	header, err := tp.ReadMIMEHeader()
	if err != nil {
		return false
	}
	c.line, c.header = line, header
	if te := header.Values("Transfer-Encoding"); len(te) > 0 {
		c.g.w.t.Errorf("the relay sent Transfer-Encoding %q; the worker leg carries the buffered body with its length", te)
		return false
	}
	if lengths := header.Values("Content-Length"); len(lengths) > 1 {
		c.g.w.t.Errorf("the relay sent %d Content-Length fields %q; want at most one", len(lengths), lengths)
		return false
	} else if len(lengths) == 1 {
		n, err := strconv.ParseInt(lengths[0], 10, 64)
		if err != nil || n < 0 {
			c.g.w.t.Errorf("the relay sent Content-Length %q", lengths)
			return false
		}
		c.body = make([]byte, n)
		if _, err := io.ReadFull(c.br, c.body); err != nil {
			return false
		}
	}
	c.g.reads.Add(1)
	go c.watchRelay()
	return true
}

// watchRelay waits for the relay's end to close after the request; the relay has nothing more to send on this leg.
func (c *workerConn) watchRelay() {
	buf := make([]byte, 512)
	for {
		n, err := c.br.Read(buf)
		c.bytesAfter.Add(int64(n))
		if err != nil {
			// The peer's close ends the pipe's stream with io.EOF; the worker's own close gives io.ErrClosedPipe.
			if errors.Is(err, io.EOF) {
				close(c.relayClosed)
			}
			return
		}
	}
}

// submit sends the request to the fake provider.
func (c *workerConn) submit() {
	c.g.submitted.Add(1)
	c.g.w.upstream.Add(1)
}

// send writes part of the worker's answer and reports whether all of it was written.
func (c *workerConn) send(s string) bool {
	_ = c.conn.SetWriteDeadline(time.Now().Add(workerWriteBudget))
	if _, err := io.WriteString(c.conn, s); err != nil {
		c.writeFailed.Store(true)
		return false
	}
	return true
}

// close ends the worker's side of the connection, as its process does when it exits or ends the answer.
func (c *workerConn) close() {
	c.closeOnce.Do(func() {
		_ = c.conn.Close()
	})
}

// answering returns a worker behaviour that reads the request, submits it and sends answer in one piece.
func answering(answer string) func(c *workerConn) {
	return func(c *workerConn) {
		defer close(c.finished)
		defer c.close()
		if !c.readRequest() {
			return
		}
		c.submit()
		c.send(answer)
	}
}

// failing returns a worker behaviour that reads the request, submits it when submit is set, runs between and then
// drops the connection without an answer.
func failing(submit bool, between func(c *workerConn)) func(c *workerConn) {
	return func(c *workerConn) {
		defer close(c.finished)
		defer c.close()
		if !c.readRequest() {
			return
		}
		if submit {
			c.submit()
		}
		if between != nil {
			between(c)
		}
	}
}

// acquireCall is one Acquire the relay made and what it got.
type acquireCall struct {
	Exclude workerlink.Generation
	Got     workerlink.Generation
	Failed  bool
}

// countingSource records every Acquire and every lease handed to the relay.
type countingSource struct {
	inner workerlink.Source

	mu     sync.Mutex
	calls  []acquireCall
	leases []*trackedLease
	// fail, when set, answers an Acquire instead of the registry with its error.
	fail func(n int) error
	// legErr, when set, replaces the error of every failed read on a lease, as a broken worker leg reports it.
	legErr error
	// note, when set, records each Acquire in the world's call order.
	note func(event string)
}

func (s *countingSource) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	if s.note != nil {
		s.note("acquire")
	}
	s.mu.Lock()
	n := len(s.calls) + 1
	fail := s.fail
	s.mu.Unlock()
	var lease workerlink.Lease
	var err error
	if fail != nil {
		err = fail(n)
	}
	if err == nil {
		lease, err = s.inner.Acquire(ctx, exclude)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	call := acquireCall{Exclude: exclude, Failed: err != nil}
	if err != nil {
		s.calls = append(s.calls, call)
		return nil, err
	}
	call.Got = lease.Generation()
	s.calls = append(s.calls, call)
	tracked := &trackedLease{Lease: lease, closed: make(chan struct{}), legErr: s.legErr}
	s.leases = append(s.leases, tracked)
	return tracked, nil
}

func (s *countingSource) acquireCalls() []acquireCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// delivered returns the generation of every lease handed to the relay, in order. A lease is a delivery: the relay
// latches delivery when it has the connection, before it writes.
func (s *countingSource) delivered() []workerlink.Generation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []workerlink.Generation
	for _, l := range s.leases {
		out = append(out, l.Generation())
	}
	return out
}

func (s *countingSource) handed() []*trackedLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.leases)
}

func (s *countingSource) setLegErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legErr = err
}

func (s *countingSource) setFail(fail func(n int) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

// trackedLease records the relay closing a lease.
type trackedLease struct {
	workerlink.Lease
	once   sync.Once
	closed chan struct{}
	legErr error
}

func (l *trackedLease) Read(p []byte) (int, error) {
	n, err := l.Lease.Read(p)
	if err != nil && l.legErr != nil {
		err = l.legErr
	}
	return n, err
}

func (l *trackedLease) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Lease.Close()
}

// clientWriter is the client's side of one public response: what the relay set and wrote, and when it flushed.
type clientWriter struct {
	mu       sync.Mutex
	header   http.Header
	statuses []int
	// head is the header as it was when the status was set.
	head    http.Header
	body    bytes.Buffer
	flushes []flushRecord
	writes  int
	// failFrom is the number of the first Write that fails, as a write to a departed client does; zero never fails.
	failFrom int
	// afterMark counts the Write, WriteHeader and Flush calls made after mark.
	marked    bool
	afterMark int
	// progress is closed and replaced at every call, so that a barrier can wait for the response to reach a state.
	progress chan struct{}
	// readDeadline, when set, receives the read deadline of the request, as the public listener's connection does.
	readDeadline func(time.Time) error
}

type flushRecord struct {
	at      time.Time
	status  int
	bodyLen int
}

func newClientWriter() *clientWriter {
	return &clientWriter{header: make(http.Header), progress: make(chan struct{})}
}

// SetReadDeadline is what http.ResponseController reaches: it sets the read deadline of the request body.
func (c *clientWriter) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	set := c.readDeadline
	c.mu.Unlock()
	if set == nil {
		return http.ErrNotSupported
	}
	return set(t)
}

// until waits until cond holds for what the client has received and reports whether it did within limit. It is a
// barrier for pacing a fake worker; limit only keeps a broken relay from parking the worker for ever.
func (c *clientWriter) until(cond func(s snapshot) bool, limit time.Duration) bool {
	deadline := time.After(limit)
	for {
		c.mu.Lock()
		ok := cond(c.snapshotLocked())
		ch := c.progress
		c.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ch:
		case <-deadline:
			return false
		}
	}
}

// flushedBody reports whether s holds at least n body bytes and has flushed them.
func flushedBody(n int) func(s snapshot) bool {
	return func(s snapshot) bool {
		return len(s.body) >= n && len(s.flushes) > 0 && s.flushes[len(s.flushes)-1].bodyLen >= n
	}
}

func (c *clientWriter) Header() http.Header { return c.header }

func (c *clientWriter) WriteHeader(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.note()
	c.statusLocked(code)
}

func (c *clientWriter) statusLocked(code int) {
	c.statuses = append(c.statuses, code)
	if len(c.statuses) == 1 {
		c.head = c.header.Clone()
	}
}

func (c *clientWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.note()
	if len(c.statuses) == 0 {
		c.statusLocked(http.StatusOK)
	}
	c.writes++
	if c.failFrom != 0 && c.writes >= c.failFrom {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}
	}
	return c.body.Write(p)
}

func (c *clientWriter) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.note()
	// A flush sends the head, so it commits the status as net/http does: 200 when none was set.
	if len(c.statuses) == 0 {
		c.statusLocked(http.StatusOK)
	}
	status := c.statuses[0]
	c.flushes = append(c.flushes, flushRecord{at: time.Now(), status: status, bodyLen: c.body.Len()})
}

func (c *clientWriter) note() {
	if c.marked {
		c.afterMark++
	}
	close(c.progress)
	c.progress = make(chan struct{})
}

// mark starts counting calls, so that a test can show nothing was written after a point.
func (c *clientWriter) mark() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.marked = true
}

func (c *clientWriter) callsAfterMark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.afterMark
}

func (c *clientWriter) failWritesFrom(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failFrom = n
}

// snapshot is what the client received.
type snapshot struct {
	statuses []int
	head     http.Header
	body     string
	flushes  []flushRecord
	// writes is the number of Write calls, the failed ones included.
	writes int
}

func (c *clientWriter) snapshot() snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *clientWriter) snapshotLocked() snapshot {
	return snapshot{statuses: slices.Clone(c.statuses), head: c.head.Clone(), body: c.body.String(), flushes: slices.Clone(c.flushes), writes: c.writes}
}

// outcome is how ServeHTTP ended.
type outcome struct {
	// panicked is the value ServeHTTP panicked with, or nil when it returned; stack is where it panicked.
	panicked any
	stack    []byte
	at       time.Time
	// readsAtExit is the number of body Reads still in progress when ServeHTTP ended, before the fixture closed the
	// body; -1 when the body does not count its Reads. net/http forbids reading the body after ServeHTTP returned.
	readsAtExit int32
}

// readCounter is a request body that counts its Reads in progress.
type readCounter interface {
	activeReads() int32
}

// aborted reports whether the handler ended by http.ErrAbortHandler, which breaks the client connection.
func (o outcome) aborted() bool {
	err, ok := o.panicked.(error)
	return ok && errors.Is(err, http.ErrAbortHandler)
}

// serve runs the handler in its own goroutine and reports how it ended. Like the public listener, it closes the
// request body once ServeHTTP has ended, however it ended (internal/supervisor/server.go:336-337).
func serve(h http.Handler, w http.ResponseWriter, r *http.Request) <-chan outcome {
	out := make(chan outcome, 1)
	go func() {
		defer func() {
			v := recover()
			var stack []byte
			if v != nil {
				stack = debug.Stack()
			}
			at := time.Now()
			reads := int32(-1)
			if rc, ok := r.Body.(readCounter); ok {
				reads = rc.activeReads()
			}
			// The close is the public listener's cleanup; it is not counted as the relay ending its own Reads.
			_ = r.Body.Close()
			out <- outcome{panicked: v, stack: stack, at: at, readsAtExit: reads}
		}()
		h.ServeHTTP(w, r)
	}()
	return out
}

// mustEnd fails unless the handler returned normally or ended by http.ErrAbortHandler; which of the two is not
// decided where it is called.
func mustEnd(t *testing.T, o outcome) {
	t.Helper()
	if o.panicked != nil && !o.aborted() {
		t.Errorf("ServeHTTP panicked with %v; want a normal return or http.ErrAbortHandler\n%s", o.panicked, o.stack)
	}
}

// mustReturn fails unless the handler returned normally.
func mustReturn(t *testing.T, o outcome) {
	t.Helper()
	if o.panicked != nil {
		t.Errorf("ServeHTTP panicked with %v; want a normal return\n%s", o.panicked, o.stack)
	}
}

// mustAbort fails unless the handler ended by http.ErrAbortHandler.
func mustAbort(t *testing.T, o outcome) {
	t.Helper()
	if !o.aborted() {
		t.Errorf("ServeHTTP ended with %v; want a panic with http.ErrAbortHandler, which resets the client connection\n%s", o.panicked, o.stack)
	}
}

// clientRequest builds the request the public listener hands to the relay. The target is the raw request target.
func clientRequest(ctx context.Context, method, target string, header http.Header, body io.Reader, length int64) *http.Request {
	u, err := url.ParseRequestURI(target)
	if err != nil {
		panic(fmt.Sprintf("test target %q: %v", target, err))
	}
	if header == nil {
		header = make(http.Header)
	}
	if body == nil {
		body = http.NoBody
	}
	r := &http.Request{
		Method:        method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          readCloser(body),
		ContentLength: length,
		Host:          publicAuthority,
		RequestURI:    target,
		RemoteAddr:    "127.0.0.1:50000",
	}
	return r.WithContext(ctx)
}

// readCloser keeps a body that closes itself, so that Close can interrupt a parked Read as net/http requires of a
// server's request body; any other body gets a Close that does nothing.
func readCloser(body io.Reader) io.ReadCloser {
	if rc, ok := body.(io.ReadCloser); ok {
		return rc
	}
	return io.NopCloser(body)
}

// postMessages builds a POST /v1/messages request with body.
func postMessages(ctx context.Context, body string) *http.Request {
	return clientRequest(ctx, http.MethodPost, "/v1/messages", http.Header{"Content-Type": {"application/json"}}, bytes.NewReader([]byte(body)), int64(len(body)))
}

// upload is a request body of fill bytes. Once all of them were read it ends at once (io.EOF), or after endsAfter,
// or, when open is set, never by itself: such a Read stays parked until Close, until the read deadline passes
// (os.ErrDeadlineExceeded, as on a connection) or until release. Close ends a parked Read, as closing a server's
// request body does, and every later Read fails. hook, when set, runs once when the fill bytes are spent and returns
// the error that Read gives then (io.EOF to end there). It never allocates the body.
type upload struct {
	remaining int64
	fill      byte
	endsAfter time.Duration
	open      bool
	hook      func() error
	release   <-chan struct{}
	read      atomic.Int64
	ended     atomic.Bool
	// reading counts the Read calls in progress; parked counts the Reads that waited past the fill bytes.
	reading   atomic.Int32
	parked    atomic.Int32
	closed    chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	deadline time.Time
	// changed is closed and replaced when the deadline is set.
	changed chan struct{}
	// endAt is when an upload with endsAfter ends; it is fixed at the first Read past the fill bytes.
	endAt time.Time
}

func newUpload(size int64, fill byte) *upload {
	return &upload{remaining: size, fill: fill, closed: make(chan struct{}), changed: make(chan struct{})}
}

func (u *upload) activeReads() int32 { return u.reading.Load() }

func (u *upload) Close() error {
	u.closeOnce.Do(func() { close(u.closed) })
	return nil
}

// SetReadDeadline sets the deadline of every Read, also one that is parked.
func (u *upload) SetReadDeadline(t time.Time) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.deadline = t
	close(u.changed)
	u.changed = make(chan struct{})
	return nil
}

func (u *upload) Read(p []byte) (int, error) {
	u.reading.Add(1)
	defer u.reading.Add(-1)
	if closed(u.closed) {
		return 0, http.ErrBodyReadAfterClose
	}
	if u.remaining > 0 {
		n := int(min(int64(len(p)), u.remaining))
		for i := range n {
			p[i] = u.fill
		}
		u.remaining -= int64(n)
		u.read.Add(int64(n))
		return n, nil
	}
	if u.hook != nil {
		hook := u.hook
		u.hook = nil
		if err := hook(); err != nil {
			if errors.Is(err, io.EOF) {
				u.ended.Store(true)
			}
			return 0, err
		}
	}
	return u.park()
}

// park waits for the end of the upload, for Close, for the read deadline or for release.
func (u *upload) park() (int, error) {
	if !u.open && u.endsAfter == 0 {
		u.ended.Store(true)
		return 0, io.EOF
	}
	u.parked.Add(1)
	u.mu.Lock()
	if u.endsAfter > 0 && u.endAt.IsZero() {
		u.endAt = time.Now().Add(u.endsAfter)
	}
	endAt := u.endAt
	u.mu.Unlock()
	for {
		u.mu.Lock()
		deadline, changed := u.deadline, u.changed
		u.mu.Unlock()
		now := time.Now()
		if !deadline.IsZero() && !now.Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
		if !endAt.IsZero() && !now.Before(endAt) {
			u.ended.Store(true)
			return 0, io.EOF
		}
		var wake <-chan time.Time
		next := endAt
		if !deadline.IsZero() && (next.IsZero() || deadline.Before(next)) {
			next = deadline
		}
		if !next.IsZero() {
			wake = time.After(next.Sub(now))
		}
		select {
		case <-u.closed:
			return 0, http.ErrBodyReadAfterClose
		case <-changed:
		case <-wake:
		case <-u.release:
			u.ended.Store(true)
			return 0, io.EOF
		}
	}
}

// failingBody returns prefix and then err from every later read; before runs once, just before the first error.
type failingBody struct {
	prefix []byte
	err    error
	before func()
	once   sync.Once
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	if b.before != nil {
		b.once.Do(b.before)
	}
	return 0, b.err
}

// checkOneDelivery fails unless the request went to gen-a once: one lease, one connection, one complete read, and
// every lease closed again.
func checkOneDelivery(t testing.TB, w *world) {
	t.Helper()
	if diff := gocmp.Diff([]workerlink.Generation{"gen-a"}, w.src.delivered()); diff != "" {
		t.Errorf("deliveries by generation (-want +got):\n%s", diff)
	}
	w.mu.Lock()
	g := w.byKey["gen-a"]
	w.mu.Unlock()
	if g == nil {
		t.Errorf("no generation gen-a")
		return
	}
	if n, reads := len(g.connections()), g.reads.Load(); n != 1 || reads != 1 {
		t.Errorf("gen-a has %d connections and read %d requests; want one of each", n, reads)
	}
	checkLeasesClosed(t, w)
}

// checkLeasesClosed fails for every lease the relay still holds.
func checkLeasesClosed(t testing.TB, w *world) {
	t.Helper()
	for i, l := range w.src.handed() {
		if !closed(l.closed) {
			t.Errorf("lease %d (generation %q) is still open after the request was finished", i, l.Generation())
		}
	}
}

// checkLost compares the lost reports. Of the error type it requires only that it is not empty and holds none of the
// private markers of the failure's text or the worker addresses.
func checkLost(t testing.TB, got, want []relay.Lost) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("Lost was called %d times with %+v; want %d reports %+v", len(got), got, len(want), want)
		return
	}
	for i := range want {
		if got[i].Generation != want[i].Generation || got[i].WorkerPID != want[i].WorkerPID {
			t.Errorf("Lost report %d = %+v; want generation %q and worker pid %d", i, got[i], want[i].Generation, want[i].WorkerPID)
		}
		if got[i].ErrorType == "" {
			t.Errorf("Lost report %d has an empty error type", i)
		}
		for _, m := range privateMarkers {
			if strings.Contains(got[i].ErrorType, m) {
				t.Errorf("Lost report %d has error type %q, which carries %q of the failure's text or address", i, got[i].ErrorType, m)
			}
		}
	}
}
