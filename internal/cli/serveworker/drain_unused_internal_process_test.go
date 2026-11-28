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

//go:build process && (linux || darwin)

package serveworker

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

const (
	// wrk3TBound ends a wait for an event that is already due; reaching it fails the case and proves nothing.
	wrk3TBound = 10 * time.Second
	// wrk3TStage is how long a test keeps a write held after the event that a wrong worker would answer by ending that
	// write or by returning. It only gives such a worker the opportunity to do so while the write is still held; a
	// correct worker is never waited for by it, and reaching it proves nothing.
	wrk3TStage = 2 * time.Second
	// wrk3TUnusedWatchdog is the hang guard of two events: the close of a never-used connection, from the drain's
	// acknowledgement, and the end of the departed stream's provider request, from the health response's release. The worker closes the connection at the first shutdown poll at which it
	// is strictly older than five whole seconds; net/http grows its poll interval with jitter, so the watchdog promises
	// no instant and only leaves room for a loaded venue.
	wrk3TUnusedWatchdog = 20 * time.Second
	// wrk3TRunLifetime bounds a worker this file starts in the test process; its cleanup ends it earlier. Its expiry is a
	// failure of the test's own infrastructure, never a product result.
	wrk3TRunLifetime = 60 * time.Second
	// wrk3TStreamBody is a native streaming message request.
	wrk3TStreamBody = `{"model":"claude-sonnet-4-6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	// wrk3TStreamHead is the head of the fake upstream's stream.
	wrk3TStreamHead = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream\",\"type\":\"message\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10}}}\n\n"
)

// wrk3TWrite writes data to conn within the bound.
func wrk3TWrite(t *testing.T, conn net.Conn, data string) {
	t.Helper()
	if err := conn.SetWriteDeadline(time.Now().Add(wrk3TBound)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, data); err != nil {
		t.Fatalf("write the request: %v", err)
	}
}

// wrk3TPost writes one complete POST of body to path on conn, which the worker closes after its answer.
func wrk3TPost(t *testing.T, conn net.Conn, public, path, body string) {
	t.Helper()
	wrk3TWrite(t, conn, "POST "+path+" HTTP/1.1\r\nHost: "+public+"\r\nContent-Type: application/json\r\nContent-Length: "+
		strconv.Itoa(len(body))+"\r\nConnection: close\r\n\r\n"+body)
}

// wrk3TAwaitEnd reads conn one byte at a time until the worker ends it or deadline passes. The first byte that arrives
// fails the case at once.
func wrk3TAwaitEnd(t *testing.T, conn net.Conn, deadline time.Time, what string) {
	t.Helper()
	if err := conn.SetReadDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	for {
		n, err := conn.Read(b[:])
		switch {
		case n > 0:
			t.Fatalf("%s: the connection received the byte %q; want it ended without a byte", what, b[:n])
		case errors.Is(err, os.ErrDeadlineExceeded):
			t.Fatalf("%s: the connection was still open at its deadline", what)
		case errors.Is(err, io.EOF), errors.Is(err, syscall.ECONNRESET):
			return
		case err != nil:
			t.Fatalf("%s: read: %v; want end of file or a reset", what, err)
		}
	}
}

// wrk3TRow runs one table row and fails that row, not the package's test binary, when the code under test panics in
// the row's goroutine. A panic of Run, which runs in its own goroutine, is caught by wrk3TStartRun.
func wrk3TRow(t *testing.T, row func(t *testing.T)) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("the row panicked: %v", r)
		}
	}()
	row(t)
}

// wrk3TRun is Run started in this process.
type wrk3TRun struct {
	done chan struct{}
	// err is Run's result, or the panic that ended it; panicked records the panic. Both are written before done closes.
	err      error
	panicked atomic.Bool
	stdout   *lossBuffer
	// port receives the port of the TCP readiness line once.
	port chan string
	// lifetime is Run's context; its cause is expired once wrk3TRunLifetime has passed. expiredAtReturn records, before
	// done closes, whether that expiry had come when Run returned; a later expiry is never Run's.
	lifetime        context.Context
	expired         error
	expiredAtReturn bool
	// watched is a held write whose end Run's goroutine samples the moment Run returns, before done closes, into
	// heldEndedAtReturn.
	watched           atomic.Pointer[wrk3THold]
	heldEndedAtReturn bool
}

// wrk3TStartRun starts Run with ctx, env and home, writing its diagnostics to stderr. Cleanup cancels Run and waits for
// it within the bound; a Run that ignores its cancellation is reported and stays behind, as a goroutine of the test
// binary. Callers register their own releases after this call, so that they run before that wait.
func wrk3TStartRun(t *testing.T, ctx context.Context, env gatewaytest.Environment, home string, stderr io.Writer) *wrk3TRun {
	t.Helper()
	expired := errors.New("test infrastructure: the in-process worker's lifetime expired")
	ctx, cancel := context.WithCancelCause(ctx)
	lifetime := time.AfterFunc(wrk3TRunLifetime, func() { cancel(expired) })
	r := &wrk3TRun{done: make(chan struct{}), stdout: new(lossBuffer), port: make(chan string, 1), lifetime: ctx, expired: expired}
	stdout := writerFunc(func(p []byte) (int, error) {
		if line, ok := strings.CutPrefix(string(p), lossReadyPrefix); ok {
			port, _, _ := strings.Cut(strings.TrimSpace(line), " ")
			select {
			case r.port <- port:
			default:
			}
		}
		return r.stdout.Write(p)
	})
	go func() {
		defer close(r.done)
		defer func() {
			if p := recover(); p != nil {
				r.panicked.Store(true)
				r.err = fmt.Errorf("Run panicked: %v", p)
			}
			if h := r.watched.Load(); h != nil {
				r.heldEndedAtReturn = h.ended.Load()
			}
			// The lifetime ends here, before done is published: a timer that had not fired is stopped, and only an
			// expiry whose cause had already reached Run's context counts as one.
			r.expiredAtReturn = !lifetime.Stop() && errors.Is(context.Cause(ctx), expired)
		}()
		r.err = Run(ctx, Options{
			Lookup:  func(k string) (string, bool) { v, ok := env[k]; return v, ok },
			Home:    func() (string, error) { return home, nil },
			Cwd:     func() (string, error) { return home, nil },
			Version: "fixture-build",
			Stdout:  stdout,
			Stderr:  stderr,
		})
	}()
	t.Cleanup(func() {
		if r.expiredNow() {
			t.Errorf("%v during this row; its failures are not product results", expired)
		}
		lifetime.Stop()
		cancel(context.Canceled)
		if !wrk3TObserved(r.done, time.Now().Add(wrk3TBound)) {
			t.Errorf("Run did not return %v after its context was canceled; it stays behind", wrk3TBound)
			return
		}
		if r.panicked.Load() {
			t.Errorf("%v", r.err)
		}
	})
	return r
}

// awaitReady returns the port of the worker's TCP readiness line, failing the case when Run returns first or the bound
// passes.
func (r *wrk3TRun) awaitReady(t *testing.T, stderr fmt.Stringer) string {
	t.Helper()
	select {
	case port := <-r.port:
		return port
	case <-r.done:
		t.Fatalf("the worker returned before its readiness line: %v\nstdout:\n%s\nstderr:\n%s", r.err, r.stdout, stderr)
	case <-time.After(wrk3TBound):
		select {
		case port := <-r.port:
			return port
		default:
		}
		t.Fatalf("the worker printed no readiness line within %v\nstdout:\n%s\nstderr:\n%s", wrk3TBound, r.stdout, stderr)
	}
	return ""
}

// returned reports whether Run has returned, without waiting.
func (r *wrk3TRun) returned() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// expiredNow reports whether Run's lifetime has expired: as recorded at its return once Run returned, and as its
// context's cause before.
func (r *wrk3TRun) expiredNow() bool {
	if r.returned() {
		return r.expiredAtReturn
	}
	return errors.Is(context.Cause(r.lifetime), r.expired)
}

// requireLifetime fails the case as an infrastructure failure, not a product result, when Run's lifetime has expired.
// A row calls it before it judges anything the expiry's immediate stop could have caused.
func (r *wrk3TRun) requireLifetime(t *testing.T, what string) {
	t.Helper()
	if r.expiredNow() {
		t.Fatalf("%v before %s; not a product result", r.expired, what)
	}
}

// awaitReturn waits within the bound for Run to return, failing the case when it does not, and as an infrastructure
// failure when Run's lifetime had expired when it returned. It returns Run's result.
func (r *wrk3TRun) awaitReturn(t *testing.T, what string, stderr fmt.Stringer) error {
	t.Helper()
	if !wrk3TObserved(r.done, time.Now().Add(wrk3TBound)) {
		r.requireLifetime(t, what)
		t.Fatalf("Run was still running %v after %s\nstderr:\n%s", wrk3TBound, what, stderr)
	}
	r.requireLifetime(t, what)
	return r.err
}

// wrk3TConn is the record of one connection the worker's TCP accept loop took: when its accept returned, and when the
// worker first began to close it. A record lives from its accept until that close, so a later connection from the same
// client address gets a record of its own.
type wrk3TConn struct {
	acceptedAt time.Time
	closeOnce  sync.Once
	// closed is closed at the entry of the worker's first Close; closedAt is written before.
	closed   chan struct{}
	closedAt time.Time
}

// wrk3TTap records the connections the worker's TCP accept loop takes, by the client's address, can park the first
// write the worker makes on one of them, and records the end of the worker's Close of the listener. It wraps the
// listener through the accept gate the package's tests may put into Run's context; the gate is always open, and its
// deadline and raw descriptor stay the listener's, so a supervised worker's final sweep works through it unchanged.
type wrk3TTap struct {
	mu      sync.Mutex
	live    map[string]*wrk3TConn
	waiting map[string]chan *wrk3TConn
	holds   map[string]*wrk3THold
	// listenerClosed fires once the worker's Close of the wrapped listener has returned.
	listenerClosed *wrk3TSignal
}

// wrk3THold parks the first write the worker makes on one connection until released. Without untilRelease the worker's
// close of that connection cuts the parked write, which then fails without sending a byte; with it the write stays
// parked through the close, as a write that only the test can end, and is then made on the closed connection.
type wrk3THold struct {
	entered, release *wrk3TSignal
	untilRelease     bool
	// cut records that the worker closed the connection while the write was parked; ended that the parked write ended.
	cut, ended atomic.Bool
}

func wrk3TNewTap() *wrk3TTap {
	return &wrk3TTap{
		live: make(map[string]*wrk3TConn), waiting: make(map[string]chan *wrk3TConn), holds: make(map[string]*wrk3THold),
		listenerClosed: wrk3TNewSignal(),
	}
}

// record returns a channel that receives the record of the connection from client once the accept loop took it: the
// live record of that address, or the next one accepted.
func (tap *wrk3TTap) record(client string) <-chan *wrk3TConn {
	ch := make(chan *wrk3TConn, 1)
	tap.mu.Lock()
	defer tap.mu.Unlock()
	if rec, ok := tap.live[client]; ok {
		ch <- rec
		return ch
	}
	tap.waiting[client] = ch
	return ch
}

// hold parks the first write the worker makes on the connection from client. The caller releases it in cleanup.
func (tap *wrk3TTap) hold(client string, untilRelease bool) *wrk3THold {
	h := &wrk3THold{entered: wrk3TNewSignal(), release: wrk3TNewSignal(), untilRelease: untilRelease}
	tap.mu.Lock()
	tap.holds[client] = h
	tap.mu.Unlock()
	return h
}

func (tap *wrk3TTap) takeHold(client string) *wrk3THold {
	tap.mu.Lock()
	defer tap.mu.Unlock()
	h := tap.holds[client]
	delete(tap.holds, client)
	return h
}

// gate is the function the test puts into Run's context under acceptGateKey.
func (tap *wrk3TTap) gate(ln net.Listener) net.Listener {
	return &wrk3TTapListener{gatedListener: newGatedListener(ln), tap: tap}
}

type wrk3TTapListener struct {
	*gatedListener
	tap *wrk3TTap
}

func (l *wrk3TTapListener) Accept() (net.Conn, error) {
	conn, err := l.gatedListener.Accept()
	if err != nil {
		return conn, err
	}
	rec := &wrk3TConn{acceptedAt: time.Now(), closed: make(chan struct{})}
	client := conn.RemoteAddr().String()
	l.tap.mu.Lock()
	l.tap.live[client] = rec
	if ch, ok := l.tap.waiting[client]; ok {
		delete(l.tap.waiting, client)
		ch <- rec
	}
	l.tap.mu.Unlock()
	return &wrk3TTapConn{Conn: conn, tap: l.tap, client: client, rec: rec}, nil
}

func (l *wrk3TTapListener) Close() error {
	err := l.gatedListener.Close()
	l.tap.listenerClosed.fire()
	return err
}

// wrk3TTapConn is one accepted connection as the worker sees it. Its local address stays the TCP listener's, which the
// drain's authorization reads.
type wrk3TTapConn struct {
	net.Conn
	tap    *wrk3TTap
	client string
	rec    *wrk3TConn
}

func (c *wrk3TTapConn) Write(p []byte) (int, error) {
	h := c.tap.takeHold(c.client)
	if h == nil {
		return c.Conn.Write(p)
	}
	h.entered.fire()
	defer h.ended.Store(true)
	if h.untilRelease {
		<-h.release.ch
		return c.Conn.Write(p)
	}
	select {
	case <-h.release.ch:
	case <-c.rec.closed:
		h.cut.Store(true)
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

func (c *wrk3TTapConn) Close() error {
	c.rec.closeOnce.Do(func() {
		c.rec.closedAt = time.Now()
		close(c.rec.closed)
		c.tap.mu.Lock()
		if c.tap.live[c.client] == c.rec {
			delete(c.tap.live, c.client)
		}
		c.tap.mu.Unlock()
	})
	return c.Conn.Close()
}

// wrk3TSignal is a channel that is closed once.
type wrk3TSignal struct {
	once sync.Once
	ch   chan struct{}
}

func wrk3TNewSignal() *wrk3TSignal { return &wrk3TSignal{ch: make(chan struct{})} }

func (s *wrk3TSignal) fire() { s.once.Do(func() { close(s.ch) }) }

// wrk3TObserved reports whether ch has closed by deadline. When the timer fires first, ch is checked once more, so an
// event that happened before the observer woke up is never reported as a hang.
func wrk3TObserved(ch <-chan struct{}, deadline time.Time) bool {
	if awaitLoss(ch, deadline) {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// wrk3TAwait waits for ch until deadline, failing the case with what when it passes first.
func wrk3TAwait(t *testing.T, ch <-chan struct{}, deadline time.Time, what string) {
	t.Helper()
	if !wrk3TObserved(ch, deadline) {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// wrk3TDial opens a connection to address within the bound; the case closes it at cleanup.
func wrk3TDial(t *testing.T, address string) net.Conn {
	t.Helper()
	dialer := net.Dialer{Timeout: wrk3TBound}
	conn, err := dialer.DialContext(t.Context(), "tcp4", address)
	if err != nil {
		t.Fatalf("dial %s: %v", address, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// wrk3TMessage is the part of a message response the drain test compares.
type wrk3TMessage struct {
	Status     int            `json:"-"`
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []wrk3TContent `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      wrk3TUsage     `json:"usage"`
}

// wrk3TUsage is the usage of a message response.
type wrk3TUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// wrk3TContent is one content block of a message response.
type wrk3TContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// wrk3THealth is the part of a health response the drain test compares.
type wrk3THealth struct {
	Status int  `json:"-"`
	OK     bool `json:"ok"`
}

// wrk3TStreamStart is what a native stream's client reads before it leaves: the response's status and content type,
// and the name, type and message id of the stream's first event.
type wrk3TStreamStart struct {
	Status      int
	ContentType string
	Event       string
	Type        string
	ID          string
}

// wrk3TReadResponse reads one complete response from conn within the bound and returns its status and whole body.
func wrk3TReadResponse(conn net.Conn) (int, []byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(wrk3TBound)); err != nil {
		return 0, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, body, err
}

// wrk3TReadStreamStartAndLeave reads, within the bound, a stream response's head and its first event from conn, and
// then leaves as the stream's client: it closes conn while the stream is still open.
func wrk3TReadStreamStartAndLeave(conn net.Conn) (wrk3TStreamStart, error) {
	if err := conn.SetReadDeadline(time.Now().Add(wrk3TBound)); err != nil {
		_ = conn.Close()
		return wrk3TStreamStart{}, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		_ = conn.Close()
		return wrk3TStreamStart{}, err
	}
	// The connection closes before the body, so the client has left and the body's close does not wait for the rest of
	// a stream that stays open.
	defer func() { _ = conn.Close(); _ = resp.Body.Close() }()
	got := wrk3TStreamStart{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")}
	body := bufio.NewReader(io.LimitReader(resp.Body, 4096))
	var data string
	for {
		line, err := body.ReadString('\n')
		if err != nil {
			return got, fmt.Errorf("the first event of the stream: %w", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			got.Event = v
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			data = v
		}
	}
	var event struct {
		Type    string `json:"type"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return got, fmt.Errorf("the first event's data %q: %w", data, err)
	}
	got.Type, got.ID = event.Type, event.Message.ID
	return got, nil
}

// wrk3TPostDrain asks the worker at public to drain, with the control token, and returns the response's status.
func wrk3TPostDrain(ctx context.Context, public, token string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, wrk3TBound)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+public+"/drain", strings.NewReader("{}"))
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Model-Gateway-Control-Token", token)
	req.Header.Set("Content-Type", "application/json")
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, err
}

// wrk3THeldUpstream is a fake Anthropic upstream. A message request waits until released and is then answered; a
// streaming one gets the head of its stream at once and is then held open until the worker ends the upstream request.
type wrk3THeldUpstream struct {
	entered, release *wrk3TSignal
	// streamEntered fires once the stream's head was sent, streamEnded once the worker ended that request.
	streamEntered, streamEnded *wrk3TSignal
}

// wrk3TNewHeldUpstream starts the fake upstream and points env's Anthropic upstream at it. The release is registered as
// a cleanup after the server's own, so it runs first and the server's close does not wait for a parked request.
func wrk3TNewHeldUpstream(t *testing.T, env gatewaytest.Environment) *wrk3THeldUpstream {
	t.Helper()
	u := &wrk3THeldUpstream{entered: wrk3TNewSignal(), release: wrk3TNewSignal(), streamEntered: wrk3TNewSignal(), streamEnded: wrk3TNewSignal()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, lossCatalog)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.UnmarshalRead(r.Body, &req)
		_, _ = io.Copy(io.Discard, r.Body)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, wrk3TStreamHead)
			w.(http.Flusher).Flush()
			u.streamEntered.fire()
			<-r.Context().Done()
			u.streamEnded.fire()
			return
		}
		u.entered.fire()
		select {
		case <-u.release.ch:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, lossUpstreamReply)
	})
	env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = env.StartUpstream(t, mux)
	t.Cleanup(u.release.fire)
	return u
}

// TestDrainUnusedConnection checks what a drain does with four connections: a message request that is still being
// served, a native stream whose client has left while its provider still holds the stream open, a health request whose
// response is still being written, and a connection that never sent a byte. The message request's provider is held,
// the stream's client reads the stream's first event and leaves, the worker's first write of the health response is
// parked on its connection, and the never-used connection is accepted, all before the drain is requested. Then the
// message request, the last non-health request whose client is still there, is let finish: it must be answered in
// full while the health response is still held.
//
// The health response stays held until the first of two events a correct worker produces without waiting for that
// response, the end of the departed stream's provider request or the close of the never-used connection, or until the
// bound passes, and then for wrk3TStage more. Neither event is a condition of its release, so a worker that counts the
// health response among the live requests until it is written is not waited for. The parked write must not have been
// cut by then, and Run must not have returned. Then the health response is let go and must arrive complete. The
// never-used connection must be closed by the worker without a byte, under a hang guard from the drain's
// acknowledgement, and the provider request must end, under a hang guard of its own from the health response's
// release; a guard that expires is followed by one more look at its event, which is judged by the event, not by when
// the test woke up. The connection must be closed no earlier than when it is strictly older than five whole seconds:
// the whole seconds of the worker's first close of it, minus those of its accept, both recorded on the server's side of
// the connection, must be at least six. The message response is compared field by field with the fixture's reply, its
// usage included. Last, Run must return nil. An expiry of the in-process worker's lifetime is reported as a failure of
// the test's infrastructure, also before Run returns.
//
// It rejects a worker that, as the source does, ends its drain and closes every connection once the last tracked
// non-health response finished, which cuts the health response still being written; one whose drain waits for a
// connection that never sent a byte until its client leaves; one that closes such a connection before it is old
// enough, also one that closes it at a whole-second difference of five; and one that returns before the response it
// was writing ended. The supervised row reaches the five-second rule through the wait before its servers shut down,
// which comes before its wait for live requests, the standalone row through the servers' shutdown itself, which runs
// beside that wait.
//
// The worker's health handler returns before its buffered response is written, so whether the handler itself is
// counted among the live requests is not observable here; the guarantee decided is that its response completes.
func TestDrainUnusedConnection(t *testing.T) {
	tests := map[string]struct {
		// supervised runs a supervised worker on a real reservation's listener and answers its drain notice as its
		// supervisor; otherwise the worker is standalone and binds its own listener.
		supervised bool
		// wantStream is what the departed stream's client reads before it leaves.
		wantStream wrk3TStreamStart
		// wantMessage and wantHealth are the two responses the drain must let finish.
		wantMessage wrk3TMessage
		wantHealth  wrk3THealth
		// minUnusedSeconds is the least difference between the whole seconds of the worker's first close of the
		// never-used connection and those of its accept.
		minUnusedSeconds int64
	}{
		"success: a standalone worker's drain completes a held health response and closes a never-used connection after five seconds": {
			wantStream: wrk3TStreamStart{Status: http.StatusOK, ContentType: "text/event-stream", Event: "message_start", Type: "message_start", ID: "msg_stream"},
			wantMessage: wrk3TMessage{
				Status: http.StatusOK, ID: "msg_loss", Type: "message", Role: "assistant", Model: "claude-sonnet-4-6",
				Content: []wrk3TContent{{Type: "text", Text: "ok"}}, StopReason: "end_turn", Usage: wrk3TUsage{InputTokens: 1, OutputTokens: 1},
			},
			wantHealth:       wrk3THealth{Status: http.StatusOK, OK: true},
			minUnusedSeconds: 6,
		},
		"success: a supervised worker's drain completes a held health response and closes a never-used connection after five seconds": {
			supervised: true,
			wantStream: wrk3TStreamStart{Status: http.StatusOK, ContentType: "text/event-stream", Event: "message_start", Type: "message_start", ID: "msg_stream"},
			wantMessage: wrk3TMessage{
				Status: http.StatusOK, ID: "msg_loss", Type: "message", Role: "assistant", Model: "claude-sonnet-4-6",
				Content: []wrk3TContent{{Type: "text", Text: "ok"}}, StopReason: "end_turn", Usage: wrk3TUsage{InputTokens: 1, OutputTokens: 1},
			},
			wantHealth:       wrk3THealth{Status: http.StatusOK, OK: true},
			minUnusedSeconds: 6,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wrk3TRow(t, func(t *testing.T) {
				wrk3TRunDrain(t, tc.supervised, tc.wantStream, tc.wantMessage, tc.wantHealth, tc.minUnusedSeconds)
			})
		})
	}
}

func wrk3TRunDrain(t *testing.T, supervised bool, wantStream wrk3TStreamStart, wantMessage wrk3TMessage, wantHealth wrk3THealth, minUnusedSeconds int64) {
	var (
		env   gatewaytest.Environment
		home  string
		token string
		h     *inProcessHandoff
	)
	if supervised {
		h = newInProcessHandoff(t)
		env, home, token = h.env, h.home, sweepToken
	} else {
		env, home = lossStandaloneEnv(t)
		token = lossToken
	}
	up := wrk3TNewHeldUpstream(t, env)
	tap := wrk3TNewTap()
	stderr := new(lossBuffer)
	r := wrk3TStartRun(t, context.WithValue(t.Context(), acceptGateKey{}, tap.gate), env, home, stderr)
	public := "127.0.0.1:" + r.awaitReady(t, stderr)
	if h != nil && public != "127.0.0.1:"+strconv.Itoa(int(h.port)) {
		t.Fatalf("the supervised worker serves on %s; want the reserved port %d", public, h.port)
	}

	message := wrk3TDial(t, public)
	wrk3TPost(t, message, public, "/v1/messages", lossMessageBody)
	wrk3TAwait(t, up.entered.ch, time.Now().Add(wrk3TBound), "the message request to reach the held upstream")

	departed := wrk3TDial(t, public)
	wrk3TPost(t, departed, public, "/v1/messages", wrk3TStreamBody)
	wrk3TAwait(t, up.streamEntered.ch, time.Now().Add(wrk3TBound), "the native stream to reach the upstream")
	// The stream's client leaves while its provider still holds the stream open.
	start, err := wrk3TReadStreamStartAndLeave(departed)
	if err != nil {
		t.Fatalf("the start of the native stream: %v\nstderr:\n%s", err, stderr)
	}
	if diff := gocmp.Diff(wantStream, start); diff != "" {
		t.Fatalf("the start of the native stream (-want +got):\n%s", diff)
	}

	health := wrk3TDial(t, public)
	healthHold := tap.hold(health.LocalAddr().String(), false)
	t.Cleanup(healthHold.release.fire)
	wrk3TWrite(t, health, "GET /healthz HTTP/1.1\r\nHost: "+public+"\r\nConnection: close\r\n\r\n")
	wrk3TAwait(t, healthHold.entered.ch, time.Now().Add(wrk3TBound), "the worker to begin writing the health response")

	unused := wrk3TDial(t, public)
	var unusedRec *wrk3TConn
	accepted := tap.record(unused.LocalAddr().String())
	select {
	case unusedRec = <-accepted:
	case <-time.After(wrk3TBound):
		select {
		case unusedRec = <-accepted:
		default:
			t.Fatal("timed out waiting for the worker to accept the never-used connection")
		}
	}

	status, err := wrk3TPostDrain(t.Context(), public, token)
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("drain: status %d, %v; want 202\nstderr:\n%s", status, err, stderr)
	}
	if h != nil {
		if err := h.drain.SetReadDeadline(time.Now().Add(wrk3TBound)); err != nil {
			t.Fatal(err)
		}
		msg, err := workerlink.DecodeDrain(bufio.NewReader(h.drain))
		if err != nil {
			t.Fatalf("the drain notice: %v", err)
		}
		if diff := gocmp.Diff(workerlink.DrainMessage{Type: workerlink.Draining, Generation: "sweep-generation"}, msg); diff != "" {
			t.Fatalf("the drain notice (-want +got):\n%s", diff)
		}
		h.res.Retire()
		if err := h.res.AcknowledgeDrain(t.Context()); err != nil {
			t.Fatalf("answer the drain notice: %v", err)
		}
	}
	// The never-used connection's close is observed from the drain's acknowledgement on; its hang guard starts here
	// and shares no budget with the reads and the staging below.
	unusedDeadline := time.Now().Add(wrk3TUnusedWatchdog)

	up.release.fire()
	status, body, err := wrk3TReadResponse(message)
	if err != nil {
		t.Fatalf("the message request finishing during the drain: %v\nbody: %q\nstderr:\n%s", err, lossTruncate(body), stderr)
	}
	got := wrk3TMessage{Status: status}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the message response %q: %v", lossTruncate(body), err)
	}
	if diff := gocmp.Diff(wantMessage, got); diff != "" {
		t.Fatalf("the message request finishing during the drain (-want +got):\n%s\nstderr:\n%s", diff, stderr)
	}

	// The health response stays held until the first event a correct worker produces without it, or the bound, and
	// then for the staging window.
	select {
	case <-up.streamEnded.ch:
	case <-unusedRec.closed:
	case <-time.After(wrk3TBound):
	}
	time.Sleep(wrk3TStage)
	r.requireLifetime(t, "the held health response was judged")
	if healthHold.cut.Load() {
		t.Fatalf("the worker closed the connection of the health response it was still writing\nstderr:\n%s", stderr)
	}
	if r.returned() {
		t.Fatalf("Run returned (%v) while the health response it was writing was still parked", r.err)
	}

	healthHold.release.fire()
	status, body, err = wrk3TReadResponse(health)
	gotHealth := wrk3THealth{Status: status}
	if err == nil {
		err = json.Unmarshal(body, &gotHealth)
	}
	if err != nil {
		t.Errorf("the health response written during the drain: %v\nbody: %q\nstderr:\n%s", err, lossTruncate(body), stderr)
	} else if diff := gocmp.Diff(wantHealth, gotHealth); diff != "" {
		t.Errorf("the health response written during the drain (-want +got):\n%s", diff)
	}

	if !wrk3TObserved(unusedRec.closed, unusedDeadline) {
		r.requireLifetime(t, "the never-used connection was judged")
		t.Fatalf("the never-used connection was still open %v after the drain was acknowledged: the drain does not close a connection that never sent a byte\nstderr:\n%s", wrk3TUnusedWatchdog, stderr)
	}
	// The provider request's end has its own hang guard from the health response's release.
	if !wrk3TObserved(up.streamEnded.ch, time.Now().Add(wrk3TUnusedWatchdog)) {
		r.requireLifetime(t, "the departed stream's provider request was judged")
		t.Fatalf("the departed stream's provider request was still open %v after the health response was let go: the drain never ended the service once no live request was left\nstderr:\n%s", wrk3TUnusedWatchdog, stderr)
	}
	wrk3TAwaitEnd(t, unused, time.Now().Add(wrk3TBound), "the never-used connection the worker closed")
	if age := unusedRec.closedAt.Unix() - unusedRec.acceptedAt.Unix(); age < minUnusedSeconds {
		t.Errorf("the worker closed the never-used connection %d whole seconds after its accept (%v); want at least %d", age, unusedRec.closedAt.Sub(unusedRec.acceptedAt), minUnusedSeconds)
	}
	if err := r.awaitReturn(t, "the last response of its drain was let go", stderr); err != nil {
		t.Errorf("Run returned %v after the drain; want nil\nstderr:\n%s", err, stderr)
	}
}
