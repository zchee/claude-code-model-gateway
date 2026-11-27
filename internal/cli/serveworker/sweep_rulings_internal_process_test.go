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
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// sweepBacklogTarget is the number of queued connections the backlog case stages when the system allows it: more than
// 4096, so that a sweep with a fixed bound of that size, the listen backlog Linux uses by default, cannot pass.
const sweepBacklogTarget = 4200

// listenBacklog is the backlog Go's listen call uses on this system, read where the runtime reads it.
func listenBacklog(t *testing.T) int {
	t.Helper()
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/sys/net/core/somaxconn")
		if err != nil {
			t.Fatalf("read the listen backlog: %v", err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			t.Fatalf("parse the listen backlog %q: %v", data, err)
		}
		// Go caps the backlog where the kernel's own field ends.
		return min(n, 1<<16-1)
	default:
		// The sysctl that holds it on macOS has no portable reader in this package's build; 128, the system's default,
		// is a value the case can stay below.
		t.Log("the listen backlog is not read on this system; assuming the default of 128")
		return 128
	}
}

// sweepWorker is a supervised worker started in this process with its accept loop behind a gate.
type sweepWorker struct {
	h      *inProcessHandoff
	run    *lossRun
	public string
	gate   *gatedListener
}

// startSweepWorker starts Run on a fresh in-process handoff. wrap turns the gate into the listener the worker
// accepts through; nil uses the gate itself.
func startSweepWorker(t *testing.T, ctx context.Context, wrap func(*gatedListener) net.Listener) *sweepWorker {
	t.Helper()
	h := newInProcessHandoff(t)
	gates := make(chan *gatedListener, 1)
	ctx = context.WithValue(ctx, acceptGateKey{}, func(ln net.Listener) net.Listener {
		g := newGatedListener(ln)
		select {
		case gates <- g:
		default:
		}
		if wrap != nil {
			return wrap(g)
		}
		return g
	})
	r := startLossRun(ctx, h.env, h.home, false)
	w := &sweepWorker{h: h, run: r, public: "127.0.0.1:" + strconv.Itoa(int(h.port))}
	t.Cleanup(func() {
		_ = h.liveW.Close()
		if !awaitLoss(r.done, time.Now().Add(lossBound)) {
			t.Errorf("Run did not return %v after its parent's end of file", lossBound)
		}
	})
	select {
	case port := <-r.port:
		if port != strconv.Itoa(int(h.port)) {
			t.Fatalf("the worker serves on port %s; want the inherited port %d", port, h.port)
		}
	case <-r.done:
		t.Fatalf("the worker stopped before it was ready: %v\nstderr:\n%s", r.err, r.stderr)
	case <-time.After(lossBound):
		t.Fatalf("the worker printed no readiness line within %v\nstderr:\n%s", lossBound, r.stderr)
	}
	select {
	case w.gate = <-gates:
	default:
		t.Fatal("the worker serves without its listener passing through the accept gate")
	}
	return w
}

// keepAlive returns a connection the worker accepted while its gate was open, on which a later drain request travels.
func (w *sweepWorker) keepAlive(t *testing.T) (*http.Transport, *http.Request) {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	health, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+w.public+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(health)
	if err != nil {
		t.Fatalf("health on the kept-alive connection: %v", err)
	}
	// Read to its end, so the transport keeps the connection for the drain request.
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("health on the kept-alive connection: status %d, %v; want a complete 200 answer", resp.StatusCode, err)
	}
	drain, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+w.public+"/drain", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	drain.Header.Set("Content-Type", "application/json")
	drain.Header.Set("X-Model-Gateway-Control-Token", sweepToken)
	return tr, drain
}

// drainAndAnswer sends the drain on the kept-alive connection, reads the worker's notice and answers it.
func (w *sweepWorker) drainAndAnswer(t *testing.T, ctx context.Context, tr *http.Transport, drain *http.Request) {
	t.Helper()
	resp, err := tr.RoundTrip(drain)
	if err != nil {
		t.Fatalf("drain on the kept-alive connection: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("drain status %d; want 202", resp.StatusCode)
	}
	tr.CloseIdleConnections()
	if err := w.h.drain.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	msg, err := workerlink.DecodeDrain(bufio.NewReader(w.h.drain))
	if err != nil || msg != (workerlink.DrainMessage{Type: workerlink.Draining, Generation: "sweep-generation"}) {
		t.Fatalf("drain notice %+v, %v; want draining for sweep-generation", msg, err)
	}
	w.gate.allowSweep()
	w.h.res.Retire()
	if err := w.h.res.AcknowledgeDrain(ctx); err != nil {
		t.Fatalf("answer the drain notice: %v", err)
	}
}

// requireHealthAnswers reads every queued connection's answer and fails the case unless each is the worker's own
// health answer. It reports the failures in one line, as there can be thousands.
func requireHealthAnswers(t *testing.T, queued []net.Conn) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var failed int
	var first string
	for i, conn := range queued {
		_ = conn.SetDeadline(deadline)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		ok := err == nil
		if ok {
			var body struct {
				OK bool `json:"ok"`
			}
			err = json.UnmarshalRead(resp.Body, &body)
			_ = resp.Body.Close()
			ok = err == nil && resp.StatusCode == http.StatusOK && body.OK
			if !ok && err == nil {
				err = errors.New("status " + strconv.Itoa(resp.StatusCode))
			}
		}
		if !ok {
			failed++
			if first == "" {
				first = "connection " + strconv.Itoa(i) + ": " + err.Error()
			}
		}
	}
	if failed != 0 {
		t.Errorf("%d of %d connections queued before the answer were not served by the worker's health handler; first: %s", failed, len(queued), first)
	}
}

// TestFinalSweepServesWholeBacklog checks that the final sweep after an answered drain serves every connection that
// waits in the listener's queue, however many: more than 4096 where the system's listen backlog lets the test queue
// that many. It rejects a sweep that takes at most a fixed number of connections, one that stops at its first empty
// poll before the queue is drained, and one whose already-expired deadline refuses queued sockets. Where the backlog
// or the descriptor limit does not allow more than 4096, the case queues what they allow and says that the larger
// number could not be staged.
func TestFinalSweepServesWholeBacklog(t *testing.T) {
	backlog := listenBacklog(t)
	// The queue holds backlog+1 established connections on Linux; a margin keeps the test below every variant.
	n := min(sweepBacklogTarget, backlog-16)
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rl); err != nil {
		t.Fatal(err)
	}
	// Each queued connection costs two descriptors in this process: the client's and, once swept, the worker's.
	n = min(n, int(rl.Cur-512)/2)
	if n <= sweepBacklogTarget-200 {
		t.Logf("more than 4096 queued connections cannot be staged here: listen backlog %d, descriptor limit %d; queueing %d", backlog, rl.Cur, n)
	}
	if n < 3 {
		t.Fatalf("test infrastructure: a listen backlog of %d leaves no room for queued connections; not a product failure", backlog)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	w := startSweepWorker(t, ctx, nil)
	tr, drain := w.keepAlive(t)
	w.gate.closeGate()
	queued := []net.Conn{queuedRequest(t, w.public)}
	select {
	case <-w.gate.parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker's accept loop never came back to the gate")
	}
	for len(queued) < n {
		queued = append(queued, queuedRequest(t, w.public))
	}
	w.drainAndAnswer(t, ctx, tr, drain)
	requireHealthAnswers(t, queued)
	if !w.gate.sweepStarted() {
		t.Error("the queued connections were served without the worker announcing its final sweep")
	}
	if !awaitLoss(w.run.done, time.Now().Add(lossBound)) {
		t.Fatalf("the worker did not return within %v of serving its queue", lossBound)
	}
	if w.run.err != nil {
		t.Errorf("the worker returned %v after the drain; want nil", w.run.err)
	}
}

// faultListener returns an injected accept error once when armed, and otherwise is the gate.
type faultListener struct {
	*gatedListener
	fault chan error
	// delivered is closed once the injected error was returned to the worker.
	delivered chan struct{}
	once      sync.Once
}

func (f *faultListener) Accept() (net.Conn, error) {
	select {
	case err := <-f.fault:
		f.once.Do(func() { close(f.delivered) })
		return nil, err
	default:
	}
	return f.gatedListener.Accept()
}

// temporaryAcceptError is a temporary accept failure as the net package reports it when the process has run out of
// descriptors: EMFILE from the accept system call. A connection aborted in the queue is not such an error, since the
// runtime retries that accept itself and the error it would wrap is not temporary.
func temporaryAcceptError(ln net.Listener) error {
	return &net.OpError{Op: "accept", Net: "tcp", Addr: ln.Addr(), Err: os.NewSyscallError("accept", syscall.EMFILE)}
}

// TestSweepAfterTemporaryAcceptError checks that a temporary accept error does not end the worker's admission: the
// listener keeps serving the next connection, and a later drain's final sweep still hands over the connections queued
// before its answer. It rejects a worker that treats any accept error, a temporary one included, as the end of
// serving, and one whose sweep no longer works after its accept loop saw an error. The injected error is checked to
// say that it is temporary before it is armed, so the case cannot pass or fail on an error the worker may stop on.
func TestSweepAfterTemporaryAcceptError(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var fault *faultListener
	w := startSweepWorker(t, ctx, func(g *gatedListener) net.Listener {
		fault = &faultListener{gatedListener: g, fault: make(chan error, 1), delivered: make(chan struct{})}
		return fault
	})
	tr, drain := w.keepAlive(t)
	injected := temporaryAcceptError(fault)
	if temp, ok := injected.(interface{ Temporary() bool }); !ok || !temp.Temporary() {
		t.Fatalf("test infrastructure: the injected accept error %v does not say that it is temporary; not a product failure", injected)
	}
	fault.fault <- injected
	// This request is taken by the accept in progress; the next accept gets the injected error.
	if status, err := lossGet(ctx, w.public); err != nil || status != http.StatusOK {
		t.Fatalf("health before the error: %d, %v; want 200", status, err)
	}
	select {
	case <-fault.delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("test infrastructure: the worker never called Accept again after a connection; the error could not be injected; not a product failure")
	}
	if status, err := lossGet(ctx, w.public); err != nil || status != http.StatusOK {
		t.Fatalf("health after the temporary accept error: %d, %v; want 200 from a listener that keeps serving", status, err)
	}
	w.gate.closeGate()
	queued := []net.Conn{queuedRequest(t, w.public)}
	select {
	case <-w.gate.parked:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker's accept loop never came back to the gate")
	}
	queued = append(queued, queuedRequest(t, w.public), queuedRequest(t, w.public))
	w.drainAndAnswer(t, ctx, tr, drain)
	requireHealthAnswers(t, queued)
	if !awaitLoss(w.run.done, time.Now().Add(lossBound)) {
		t.Fatalf("the worker did not return within %v of the drain", lossBound)
	}
	if w.run.err != nil {
		t.Errorf("the worker returned %v after the drain; want nil", w.run.err)
	}
}

// lossGet sends one health request on a new connection.
func lossGet(ctx context.Context, public string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+public+"/healthz", nil)
	if err != nil {
		return 0, err
	}
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// TestFailedExchangeLogsOnceAndSweepsNothing checks a drain exchange that fails, by a wrong answer or by no answer
// within its budget of five seconds. The worker writes exactly one diagnostic line of at most 512 bytes about the
// drain and sweeps nothing: no final sweep follows a failed exchange, the worker only closes its own copy of the
// listener. Connections queued on the shared socket before the failure, while the accept loop is held at a closed
// gate, are neither accepted nor answered by this worker: once Run has returned they still wait in the queue of the
// socket the parent keeps, with no byte of an answer, and the sweep was never announced.
//
// It rejects a worker that sweeps the queue after a failed exchange, one that accepts and closes the queued
// connections, and one that logs the failure more than once or without bound. Only the stderr lines written after the
// drain request count, since the worker's start may log unrelated warnings. The control is the frozen sweep test, in
// which the same queue is served after an answered exchange.
func TestFailedExchangeLogsOnceAndSweepsNothing(t *testing.T) {
	tests := map[string]struct {
		// answer is the line the parent writes after the notice; "" writes none.
		answer string
	}{
		"error: after a wrong answer one diagnostic line and nothing swept": {answer: `{"type":"retired","generation":"another-generation"}` + "\n"},
		"error: after no answer one diagnostic line and nothing swept":      {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			w := startSweepWorker(t, ctx, nil)
			tr, drain := w.keepAlive(t)
			w.gate.closeGate()
			// Taken by the accept in progress or queued; only the two after the gate parked are judged.
			_ = queuedRequest(t, w.public)
			select {
			case <-w.gate.parked:
			case <-time.After(10 * time.Second):
				t.Fatal("the worker's accept loop never came back to the gate")
			}
			// The first connection may have been taken by the accept that was in progress; these two wait in the queue.
			waiting := []net.Conn{queuedRequest(t, w.public), queuedRequest(t, w.public)}
			before := len(w.run.stderr.String())
			resp, err := tr.RoundTrip(drain)
			if err != nil {
				t.Fatalf("drain on the kept-alive connection: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("drain status %d; want 202", resp.StatusCode)
			}
			tr.CloseIdleConnections()
			if err := w.h.drain.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			msg, err := workerlink.DecodeDrain(bufio.NewReader(w.h.drain))
			if err != nil || msg != (workerlink.DrainMessage{Type: workerlink.Draining, Generation: "sweep-generation"}) {
				t.Fatalf("drain notice %+v, %v; want draining for sweep-generation", msg, err)
			}
			if tc.answer != "" {
				if _, err := w.h.drain.Write([]byte(tc.answer)); err != nil {
					t.Fatalf("write the wrong answer: %v", err)
				}
			}
			answeredAt := time.Now()
			// The exchange's budget is five seconds; the margin is for a loaded venue.
			if !awaitLoss(w.run.done, answeredAt.Add(5*time.Second+lossBound)) {
				t.Fatalf("the worker did not return within the exchange's budget and %v", lossBound)
			}
			if w.gate.sweepStarted() {
				t.Error("the worker announced a final sweep after a failed drain exchange; want none")
			}
			// Run has returned and closed its copy; the parent's copy keeps the socket, so a connection nobody took
			// still waits with nothing to read, while one the worker took and closed would read its end.
			for i, conn := range waiting {
				if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				var buf [1]byte
				n, err := conn.Read(buf[:])
				if n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Errorf("queued connection %d after the failed exchange: read %d bytes, %v; want it still waiting in the parent's queue, neither accepted nor answered", i, n, err)
				}
			}
			var lines []string
			for line := range strings.Lines(w.run.stderr.String()[before:]) {
				if strings.Contains(strings.ToLower(line), "drain") {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("diagnostics about the failed drain exchange: %q; want exactly one line", lines)
			}
			if len(lines[0]) > 512 {
				t.Errorf("drain diagnostic of %d bytes; want at most 512", len(lines[0]))
			}
		})
	}
}
