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
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

const (
	// lossBound is how long a worker may take to return once its parent ended before it served. A worker that stops at
	// once needs milliseconds. The bound leaves room for a fixture child's own start on a venue loaded with many parallel
	// worker processes (process and runtime start, the control token, the collaborators: a few seconds at worst), and it
	// stays far below the fixture runner's thirty-second kill, so a worker that waits without bound is told apart from a
	// slow one with the runner's lifetime left for the checks that follow.
	lossBound = 10 * time.Second
	// lossHold is the longest the test keeps a worker's readiness line from completing while it waits for the worker's
	// accept loop to take the queued probes. It only stages the window; reaching it proves nothing.
	lossHold = 3 * time.Second
	// lossReadWatchdog bounds a read of a probe, of the report pipe or of the drain channel. Every such read is made
	// when its end is already due; reaching the watchdog fails the case and never proves anything.
	lossReadWatchdog = 5 * time.Second
	// lossChildLifetime is the fixture runner's lifetime of a worker child; it kills the child then.
	lossChildLifetime = 30 * time.Second
	// lossPostRoom is the part of a child's lifetime kept for the checks after its exit.
	lossPostRoom = 8 * time.Second
	// lossFirstChildFD is the child's descriptor number of the first file the test passes: the fixture runner puts its
	// private directory at 3, and the worker helper's owned mode shifts nothing.
	lossFirstChildFD = 4
	lossGeneration   = "parent-loss-generation"
	lossToken        = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	lossReadyPrefix  = "model-gateway shim listening on 127.0.0.1:"
	// lossMessageBody is a native message request, which the worker forwards to the fake Anthropic upstream.
	lossMessageBody = `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	// lossUpstreamReply is the fake upstream's complete answer to a message request.
	lossUpstreamReply = `{"id":"msg_loss","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	// lossCatalog is the fake upstream's one-page model catalog.
	lossCatalog = `{"data":[{"id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6"}],"has_more":false}`
)

var lossReadyPattern = regexp.MustCompile(`^model-gateway shim listening on 127\.0\.0\.1:([0-9]+)(?:\s|$)`)

// lossStage is the point of the worker's start at which the parent ends, or would end in a control.
type lossStage int

const (
	// lossBeforeLaunch ends the parent before the worker process exists.
	lossBeforeLaunch lossStage = iota + 1
	// lossWhileWrapping ends the parent while Run is held inside the wrap of its imported listener, which it makes
	// before its accept loop can take a connection and before its listening report.
	lossWhileWrapping
	// lossBeforeFirstAccept ends the parent after the listening report was read, while the accept loop is held before
	// its first connection.
	lossBeforeFirstAccept
	// lossStandalone runs a standalone worker that has no parent: it checks the probes and the fake upstream themselves.
	lossStandalone
)

// lossCase is one row of TestParentEOFBeforeServing.
type lossCase struct {
	// child runs the worker in a fixture child process; otherwise Run runs in this process.
	child bool
	stage lossStage
	// parentEnds closes the parent's write end of the liveness pipe, the end of file the contract names parent loss;
	// the parent keeps its other descriptors, the report pipe's read end included.
	parentEnds bool
	// fullDeath also closes what a dead parent closes besides: the report pipe's read end, the drain channel's parent
	// end and the parent's copy of the listener.
	fullDeath bool
	// lockHeld has this test process hold the worker's Unix socket reclaim lock for the whole case, beyond the bound.
	lockHeld bool
	// tokenFIFO replaces the control token file with a FIFO that nobody writes, a startup input that blocks.
	tokenFIFO bool
	// endWhileBlocked launches the worker with its parent alive and ends the parent once the worker has opened the
	// token FIFO; parentEnds is then false.
	endWhileBlocked bool
	// standalone runs an in-process stage with a standalone worker, which binds its own listener and has no parent.
	standalone bool
	// productionExit launches the production-exit child, whose Options.Exit is os.Exit as in the production binary,
	// instead of the worker helper, which leaves Exit nil. Only the production exit is promised to end a worker whose
	// startup step blocks; its exit status is then the evidence, as a hard exit prints no return marker.
	productionExit bool
}

// TestParentEOFBeforeServing checks a supervised worker whose parent ends before it serves. Before the worker starts
// its servers, writes its port report or prints its readiness line, it checks its parent's liveness synchronously: its
// admission boundary. A parent that ended before the boundary gets a clean exit within lossBound, zero report bytes,
// no readiness line, and no handler response or side effect, also when a startup step would otherwise wait: another
// process holds the Unix socket reclaim lock beyond the bound, or the control token is a FIFO that nobody writes. A
// parent that ends after the boundary, once the report was written, gets an exit within the bound and no drain notice;
// what the worker admitted or printed before it noticed is not pinned.
//
// Every connection the test opens carries one complete request, queued on the supervisor-owned listener before the
// worker could take it; a successful connect alone is not admission, since the parent owns the listener. A "probe"
// pair is a health request, which any handler answers at once, and a native message request, which the worker
// forwards to a fake upstream. What decides "not answered": once the worker has returned and every other copy of its
// listener is closed, every probe's read ends (end of file or reset) with zero bytes; a read that reaches its watchdog
// fails the case, as a copy of the listener is still open. The fake upstream's count of message requests corroborates:
// an arrival proves that a handler ran; zero arrivals cannot prove that none did, because a handler that was cut before
// it forwarded leaves no trace there.
//
// A blocked startup step can only be ended by the hard exit through Options.Exit, which the production binary wires to
// os.Exit; with Exit nil no bounded exit is promised. The rows whose startup step can block therefore run the
// production-exit child and take its exit status 0 within the bound as the clean exit, since a hard exit prints no
// return marker; the other child rows run the worker helper, whose clean stop is a return from Run.
//
// The parent-loss rows have control rows with a live parent, in which every probe is answered and the upstream receives
// every message request, so that a staging which cannot deliver a request cannot make a parent-loss row pass. A control
// holds the worker at the same stage as its parent-loss row, except that the token FIFO rows have none, since a worker
// waiting on that input cannot serve. The controls run the worker helper also where the parent-loss row runs the
// production-exit child: the two differ only in Options.Exit and in how they are launched, and a live parent gives the
// worker no reason to call Exit. The standalone rows check the probes, the fake upstream, the socket lock path and the
// in-process staging on any tree. In every row whose parent ends before the boundary the order is forced by a barrier:
// the child does not exist yet, or it waits on a FIFO the test controls, or Run is held inside the wrap of its
// listener and has written no report and printed nothing.
func TestParentEOFBeforeServing(t *testing.T) {
	tests := map[string]lossCase{
		// Rejects a worker that reads its liveness pipe only after it bound, served, reported and printed its readiness
		// line, which then reports to a reader that is still open, prints, and admits the queued probes and the later
		// ones; and a worker that waits on any step of its start once its parent has ended.
		"error: a parent that ended before the launch gets a clean exit with no report, no readiness and no answer": {
			child: true, stage: lossBeforeLaunch, parentEnds: true,
		},
		// Rejects a worker that waits on its report write, its drain channel or its listener when every parent descriptor
		// is gone, as after the parent's death, instead of exiting cleanly within the bound.
		"error: a parent that died before the launch with every descriptor closed gets a clean exit and no answer": {
			child: true, stage: lossBeforeLaunch, parentEnds: true, fullDeath: true,
		},
		// Rejects a worker that reaches a wait on the Unix socket reclaim lock, which another process holds beyond the
		// bound, before or instead of its liveness check, so that even the production exit cannot end it within the
		// bound; and one that ends with a status other than 0, or reports, prints readiness or answers a request.
		"error: a parent that ended before the launch while another process holds the socket lock gets a clean exit": {
			child: true, stage: lossBeforeLaunch, parentEnds: true, lockHeld: true, productionExit: true,
		},
		// Rejects a worker that opens or reads its control token before its liveness check and so waits on a FIFO that
		// nobody writes after its parent has ended, beyond the production exit's reach, and one that refuses the FIFO
		// with status 1 although its parent ended before its boundary: the exit must be status 0.
		"error: a parent that ended before the launch while the control token is a FIFO nobody writes gets a clean exit": {
			child: true, stage: lossBeforeLaunch, parentEnds: true, tokenFIFO: true, productionExit: true,
		},
		// Rejects a worker whose wait on a blocking startup input is not ended by its parent's end through the
		// production exit: the parent ends once the worker has opened the token FIFO, and the worker must still exit
		// within the bound, before its boundary, with status 0, no report, no readiness and no answer. A worker that
		// refuses the FIFO while its parent is alive must exit with status 1 and a token diagnostic, never 0.
		"error: a parent that ends while the worker waits on a control token FIFO gets an exit within the bound": {
			child: true, stage: lossBeforeLaunch, tokenFIFO: true, endWhileBlocked: true, productionExit: true,
		},
		// Rejects a worker that takes the Unix socket reclaim lock with a blocking wait: with another process holding the
		// lock it never reports, which is the unbounded startup step a parent's end could not interrupt. The worker serves
		// TCP and refuses the socket with the ordinary address-in-use diagnostic.
		"success: with the parent alive and the socket lock held by another process the worker serves TCP": {
			child: true, stage: lossBeforeLaunch, lockHeld: true,
		},
		// Control of the child rows: the same staging with a live parent and a free lock serves every probe over TCP
		// and serves the socket.
		"success: with the parent alive and the socket lock free the worker serves every probe": {
			child: true, stage: lossBeforeLaunch,
		},
		// Rejects a worker that does not check its parent's liveness after it imported its listener and before it
		// starts its servers, and a worker that waits after its parent's end. The parent ends while Run is held inside
		// the wrap of its listener. A worker whose check comes before the wrap passed its boundary with a live parent;
		// one whose check comes after it must stop at the boundary. So: when the worker wrote no report and printed no
		// readiness line, no probe may have been answered; either way it returns within the bound with no notice.
		"error: the parent ends while the worker wraps its imported listener and it stops within the bound": {
			stage: lossWhileWrapping, parentEnds: true,
		},
		// Control: the same hold with a live parent serves every probe.
		"success: with the parent alive the worker held while wrapping its listener serves every probe": {
			stage: lossWhileWrapping,
		},
		// Rejects a worker that, after its report, does not stop within the bound when its parent ends while its accept
		// loop is held before the first connection, or that writes a drain notice then. What it admits after its
		// boundary is not pinned.
		"error: the parent ends after the report while the first accept is held and the worker stops within the bound": {
			stage: lossBeforeFirstAccept, parentEnds: true,
		},
		// Control: the same hold with a live parent serves every probe once the gate opens.
		"success: with the parent alive the worker held before its first accept serves every probe": {
			stage: lossBeforeFirstAccept,
		},
		// Validates the fixture on any tree: a standalone worker answers both probes, the fake upstream receives the
		// message request, and the socket's reclaim lock file is where the lock rows hold it.
		"success: a standalone worker answers the probes and locks the socket path the lock rows hold": {
			stage: lossStandalone,
		},
		// Validate the in-process staging on any tree with a standalone worker, whose listener Run already passes
		// through the accept gate: the hold inside the wrap, the closed gate and its opening, the counting listener and
		// the held readiness line, which stands in for the report. Every probe is answered and reaches the upstream.
		"success: a standalone worker held while wrapping its listener serves every probe": {
			stage: lossWhileWrapping, standalone: true,
		},
		"success: a standalone worker held before its first accept serves every probe": {
			stage: lossBeforeFirstAccept, standalone: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			switch {
			case tc.stage == lossStandalone:
				runStandaloneLoss(t)
			case tc.child:
				runChildLoss(t, tc)
			default:
				runInProcessLoss(t, tc)
			}
		})
	}
}

// lossBuffer collects a stream and can be read while it is written.
type lossBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lossBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lossBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lossUpstream is the fake Anthropic upstream; messages counts the message requests it received.
type lossUpstream struct {
	base     string
	messages atomic.Int64
}

// newLossUpstream starts the fake upstream and points env's Anthropic upstream at it. The worker may list the catalog
// on its own at start, so only message requests are counted.
func newLossUpstream(t *testing.T, env gatewaytest.Environment) *lossUpstream {
	t.Helper()
	u := new(lossUpstream)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, lossCatalog)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		u.messages.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, lossUpstreamReply)
	})
	u.base = env.StartUpstream(t, mux)
	env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = u.base
	return u
}

// lossProbe is one connection that carries one complete request the test has not read an answer to.
type lossProbe struct {
	label   string
	message bool
	conn    net.Conn
}

// sendLossProbes dials address twice and writes one complete request on each connection without reading: a health
// request and a message request. when says when they were sent, for the failure messages.
func sendLossProbes(t *testing.T, address, when string) []lossProbe {
	t.Helper()
	requests := []struct {
		label   string
		message bool
		raw     string
	}{
		{"health request " + when, false, "GET /healthz HTTP/1.1\r\nHost: " + address + "\r\nConnection: close\r\n\r\n"},
		{
			"message request " + when, true,
			"POST /v1/messages HTTP/1.1\r\nHost: " + address + "\r\nContent-Type: application/json\r\nContent-Length: " +
				strconv.Itoa(len(lossMessageBody)) + "\r\nConnection: close\r\n\r\n" + lossMessageBody,
		},
	}
	var dialer net.Dialer
	probes := make([]lossProbe, 0, len(requests))
	for _, r := range requests {
		conn, err := dialer.DialContext(t.Context(), "tcp4", address)
		if err != nil {
			t.Fatalf("dial %s for the %s: %v", address, r.label, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := io.WriteString(conn, r.raw); err != nil {
			t.Fatalf("write the %s: %v", r.label, err)
		}
		probes = append(probes, lossProbe{label: r.label, message: r.message, conn: conn})
	}
	return probes
}

// lossMessageProbes counts the message requests among probes.
func lossMessageProbes(probes []lossProbe) int64 {
	var n int64
	for _, p := range probes {
		if p.message {
			n++
		}
	}
	return n
}

// requireLossUnanswered checks that every probe's connection ended without a byte of an answer. It is called once the
// worker has returned and every other copy of its listener is closed, so every read is due to end.
func requireLossUnanswered(t *testing.T, probes []lossProbe) {
	t.Helper()
	deadline := time.Now().Add(lossReadWatchdog)
	for _, p := range probes {
		if err := p.conn.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(p.conn)
		switch {
		case len(data) > 0:
			t.Errorf("the %s was answered after the parent's end: %q", p.label, lossTruncate(data))
		case errors.Is(err, os.ErrDeadlineExceeded):
			t.Errorf("the %s was neither answered nor ended %v after the worker returned and every other copy of the listener was closed: a copy of the listener is still open (an imported listener the worker did not close)", p.label, lossReadWatchdog)
		}
	}
}

// requireLossAnswered checks that every probe got a complete 200 answer.
func requireLossAnswered(t *testing.T, probes []lossProbe) {
	t.Helper()
	deadline := time.Now().Add(lossBound)
	for _, p := range probes {
		if err := p.conn.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(p.conn), nil)
		if err != nil {
			t.Errorf("the %s got no answer with the parent alive: %v", p.label, err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Errorf("the %s with the parent alive: status %d, body %q, %v; want a complete 200 answer", p.label, resp.StatusCode, lossTruncate(body), err)
		}
	}
}

func lossTruncate(data []byte) []byte {
	if len(data) > 200 {
		return data[:200]
	}
	return data
}

// requireLossNoNotice checks that the worker closed its end of the drain channel without writing a notice: a parent's
// end stops a worker with no notice.
func requireLossNoNotice(t *testing.T, drain net.Conn) {
	t.Helper()
	if err := drain.SetReadDeadline(time.Now().Add(lossReadWatchdog)); err != nil {
		t.Fatal(err)
	}
	if msg, err := workerlink.DecodeDrain(bufio.NewReader(drain)); !errors.Is(err, io.EOF) {
		t.Errorf("drain channel after the worker returned: message %+v, error %v; want end of file with no notice", msg, err)
	}
}

// readLossReport reads the listening report to its end of file and checks that it names port.
func readLossReport(t *testing.T, r *os.File, port uint16) {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(lossBound)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(io.LimitReader(r, 257))
	if err != nil {
		t.Fatalf("the report pipe did not reach end of file: %v", err)
	}
	var got struct {
		Type string `json:"type"`
		Port int    `json:"port"`
	}
	if err := json.Unmarshal(bytes.TrimSuffix(data, []byte("\n")), &got); err != nil {
		t.Fatalf("listening report %q: %v", data, err)
	}
	if diff := gocmp.Diff(struct {
		Type string
		Port int
	}{"listening", int(port)}, struct {
		Type string
		Port int
	}{got.Type, got.Port}); diff != "" {
		t.Fatalf("listening report (-want +got):\n%s", diff)
	}
}

// awaitLoss reports whether done closed before deadline.
func awaitLoss(done <-chan struct{}, deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// holdLossSocketLock takes the Unix socket reclaim lock of socket, the regular file socket+".lock", through an open
// file description of this process, so that the worker's own lock attempt contends with it as with another process's.
// The returned release is idempotent and also runs at cleanup.
func holdLossSocketLock(t *testing.T, socket string) func() {
	t.Helper()
	fd, err := unix.Open(socket+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatalf("open the socket lock: %v", err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("take the socket lock: %v", err)
	}
	release := sync.OnceFunc(func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	})
	t.Cleanup(release)
	return release
}

// lossHandoff is a supervised handoff for a worker child: the report pipe, the liveness pipe, the listener and the
// drain channel of a real workerlink reservation, announced by their variables. files[i] is the child's descriptor
// lossFirstChildFD+i.
type lossHandoff struct {
	env     gatewaytest.Environment
	res     *workerlink.Reservation
	drain   net.Conn
	files   []*os.File
	reportR *os.File
	liveW   *os.File
	port    uint16
	socket  string
	// token is the control token file of the worker's state directory.
	token string
}

func newLossHandoff(t *testing.T) *lossHandoff {
	t.Helper()
	// A nested test name alone can exceed the Unix socket pathname limit.
	home, err := os.MkdirTemp("", "mgw-loss-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	state := filepath.Join(env["CLAUDE_CONFIG_DIR"], "model-gateway")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(lossToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(home, "w.sock")
	env["CODEX_GATEWAY_SOCKET_PATH"] = socket
	var cfg net.ListenConfig
	var dialer net.Dialer
	res, err := workerlink.NewRegistry(cfg.Listen, dialer.DialContext).Reserve(t.Context(), lossGeneration, 0)
	if err != nil {
		t.Fatalf("reserve the worker's listener: %v", err)
	}
	t.Cleanup(func() { _ = res.End() })
	listener, err := res.File()
	if err != nil {
		t.Fatalf("take the worker's copy of its listener: %v", err)
	}
	drain, child, err := res.DrainChannel()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("take the worker's drain channel: %v", err)
	}
	reportR, reportW := lossPipe(t)
	liveR, liveW := lossPipe(t)
	h := &lossHandoff{
		env: env, res: res, drain: drain, files: []*os.File{reportW, liveR, listener, child},
		reportR: reportR, liveW: liveW, port: res.Port(), socket: socket, token: filepath.Join(state, "control-token"),
	}
	t.Cleanup(h.closeTransfer)
	fd := func(i int) string { return strconv.Itoa(lossFirstChildFD + i) }
	env["MODEL_GATEWAY_CONTROL_FDS"] = fd(0) + "," + fd(1)
	env[workerlink.ListenerFDEnv] = fd(2)
	env[workerlink.DrainFDEnv] = fd(3)
	env["MODEL_GATEWAY_WORKER_GENERATION"] = lossGeneration
	env["CODEX_GATEWAY_WORKER_PORT"] = strconv.Itoa(int(h.port))
	return h
}

func lossPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	return r, w
}

// closeTransfer closes the parent's copies of what the child inherits.
func (h *lossHandoff) closeTransfer() {
	for i, f := range h.files {
		if f != nil {
			_ = f.Close()
			h.files[i] = nil
		}
	}
}

// lossChild is one worker child.
type lossChild struct {
	// startedAt is taken just before the child is started. The worker helper's runner starts its lifetime after that,
	// so it kills the child no earlier than startedAt + lossChildLifetime. The production-exit child's lifetime starts
	// when launchProductionExit prepares it, just before startedAt, so it may be killed that much earlier; the only check
	// that counts on the later kill, the room kept for the checks after the exit, runs in helper rows alone.
	startedAt time.Time
	stderr    *lossBuffer
	// ready receives the port of the readiness line once; parked is closed when the production-exit child printed
	// productionExitParked; outputEnd is closed when stdout ended.
	ready     chan string
	parked    chan struct{}
	outputEnd chan struct{}
	mu        sync.Mutex
	output    []string
	// done is closed when the child exited; runErr is the runner's result.
	done   chan struct{}
	runErr error
}

// launch starts the worker helper's owned mode with h's descriptors and environment. The helper leaves Options.Exit
// nil, so its worker can only end by returning from Run.
func (h *lossHandoff) launch(t *testing.T) *lossChild {
	t.Helper()
	cmd, err := h.env.HelperCommand(t, "worker", "owned")
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = h.files
	return h.start(t, cmd, func() error { return h.env.RunFixtureWithin(cmd, lossChildLifetime) })
}

// start runs the prepared worker child cmd through run, which returns once the child exited. Its stdout is read
// continuously, as the child writes it into an unbuffered pipe.
func (h *lossHandoff) start(t *testing.T, cmd *exec.Cmd, run func() error) *lossChild {
	t.Helper()
	c := &lossChild{
		stderr: new(lossBuffer), ready: make(chan string, 1), parked: make(chan struct{}), outputEnd: make(chan struct{}),
		done: make(chan struct{}),
	}
	cmd.Stderr = c.stderr
	r, w := io.Pipe()
	cmd.Stdout = w
	go func() {
		defer close(c.outputEnd)
		scanner := bufio.NewScanner(r)
		parked := false
		for scanner.Scan() {
			line := scanner.Text()
			c.mu.Lock()
			c.output = append(c.output, line)
			c.mu.Unlock()
			if line == productionExitParked && !parked {
				parked = true
				close(c.parked)
			}
			if m := lossReadyPattern.FindStringSubmatch(line); len(m) > 0 {
				select {
				case c.ready <- m[1]:
				default:
				}
			}
		}
	}()
	c.startedAt = time.Now()
	go func() { defer close(c.done); c.runErr = run(); _ = w.Close() }()
	// HelperCommand uses t.Context(), which is canceled before cleanups run.
	t.Cleanup(func() { _ = h.liveW.Close(); <-c.done; _ = r.Close(); <-c.outputEnd })
	return c
}

func (c *lossChild) stdout() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.output, "\n")
}

// awaitReady returns the port of the child's readiness line. It fails the case when the child exits without one or
// deadline passes first.
func (c *lossChild) awaitReady(t *testing.T, deadline time.Time) string {
	t.Helper()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case port := <-c.ready:
		return port
	case <-c.done:
		<-c.outputEnd
		select {
		case port := <-c.ready:
			return port
		default:
		}
		t.Fatalf("the worker exited without its readiness line: %v\nstdout:\n%s\nstderr:\n%s", c.runErr, c.stdout(), c.stderr)
	case <-timer.C:
		t.Fatalf("the worker printed no readiness line by %v after its launch (a wait at a startup step)\nstdout:\n%s\nstderr:\n%s", deadline.Sub(c.startedAt), c.stdout(), c.stderr)
	}
	return ""
}

// exitCode is the child's exit status once done is closed, or -1 when it did not exit with one.
func (c *lossChild) exitCode() int {
	if c.runErr == nil {
		return 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](c.runErr); ok {
		return exit.ExitCode()
	}
	return -1
}

// runChildLoss runs a child row: the parent ends before the launch, or the parent stays alive as the control.
func runChildLoss(t *testing.T, tc lossCase) {
	h := newLossHandoff(t)
	if tc.tokenFIFO {
		makeLossTokenFIFO(t, h.token)
	}
	up := newLossUpstream(t, h.env)
	public := "127.0.0.1:" + strconv.Itoa(int(h.port))
	releaseLock := func() {}
	if tc.lockHeld {
		releaseLock = holdLossSocketLock(t, h.socket)
	}
	if !tc.parentEnds && !tc.endWhileBlocked {
		c := h.launch(t)
		// Registered after the launch, so it runs before the child's cleanup waits for the child.
		t.Cleanup(releaseLock)
		port := c.awaitReady(t, c.startedAt.Add(lossBound))
		h.closeTransfer()
		if port != strconv.Itoa(int(h.port)) {
			t.Fatalf("readiness on port %s; want the inherited listener's port %d", port, h.port)
		}
		readLossReport(t, h.reportR, h.port)
		probes := sendLossProbes(t, public, "with the parent alive")
		requireLossAnswered(t, probes)
		if got, want := up.messages.Load(), lossMessageProbes(probes); got != want {
			t.Errorf("the fake upstream received %d message requests; want %d", got, want)
		}
		socketStatus, socketErr := lossUnixHealth(t.Context(), h.socket)
		if tc.lockHeld {
			if socketErr == nil {
				t.Errorf("the worker serves its Unix socket (status %d) although another process holds the socket lock", socketStatus)
			}
		} else if socketErr != nil || socketStatus != http.StatusOK {
			t.Errorf("health over the Unix socket with the lock free: status %d, %v; want 200", socketStatus, socketErr)
		}
		if left := time.Until(c.startedAt.Add(lossChildLifetime - lossPostRoom)); left < lossBound {
			t.Fatalf("test infrastructure: %v of the child's lifetime is left before the checks after its exit, and the bound is %v; nothing about the worker is judged; not a product failure", left, lossBound)
		}
		endedAt := time.Now()
		if err := h.liveW.Close(); err != nil {
			t.Fatal(err)
		}
		if !awaitLoss(c.done, endedAt.Add(lossBound)) {
			t.Fatalf("the worker was still running %v after its parent's end of file", lossBound)
		}
		<-c.outputEnd
		if c.runErr != nil || !strings.Contains(c.stdout(), "worker returned cleanly") {
			t.Errorf("the worker did not return cleanly after its parent's end of file: %v\nstdout:\n%s\nstderr:\n%s", c.runErr, c.stdout(), c.stderr)
		}
		if tc.lockHeld && !lossSocketRefused(c.stderr.String(), h.socket) {
			t.Errorf("no address-in-use diagnostic for the socket %s whose lock another process holds: %s", h.socket, c.stderr)
		}
		requireLossNoNotice(t, h.drain)
		return
	}

	probes := sendLossProbes(t, public, "queued before the launch")
	if tc.parentEnds {
		if err := h.liveW.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if tc.fullDeath {
		if err := h.reportR.Close(); err != nil {
			t.Fatal(err)
		}
		// The reservation's own copy of the listener and the parent end of the drain channel go with it.
		if err := h.res.End(); err != nil {
			t.Fatal(err)
		}
	}
	var c *lossChild
	if tc.productionExit {
		c = h.launchProductionExit(t, "")
	} else {
		c = h.launch(t)
	}
	t.Cleanup(releaseLock)
	deadline := c.startedAt.Add(lossBound)
	// refusedAlive records a worker that refused the token FIFO and exited while its parent was still alive.
	refusedAlive := false
	if tc.endWhileBlocked {
		// Bounded from the parent's end, which comes once the worker is waiting on the token FIFO.
		if opened := awaitLossFIFOReader(t, h.token, c.done, deadline); opened {
			deadline = time.Now().Add(lossBound)
			if err := h.liveW.Close(); err != nil {
				t.Fatal(err)
			}
		} else {
			refusedAlive = true
			t.Logf("the worker never waited on the token FIFO before it exited, with its parent alive: %v", c.runErr)
		}
	}
	probes = append(probes, sendLossProbes(t, public, "sent after the launch")...)
	if !awaitLoss(c.done, deadline) {
		t.Errorf("the worker was still running %v after its parent's end, which came before its admission boundary (a wait at a startup step)\nstdout:\n%s\nstderr:\n%s", lossBound, c.stdout(), c.stderr)
		return
	}
	<-c.outputEnd
	code := c.exitCode()
	tokenDiagnostic := strings.Contains(strings.ToLower(c.stdout()+"\n"+c.stderr.String()), "token")
	switch {
	case refusedAlive:
		// With its parent alive, a refused startup input is an error, never a clean exit.
		if code != 1 || !tokenDiagnostic {
			t.Errorf("the worker ended with %v (status %d) after refusing its token FIFO with its parent alive; want status 1 and a token diagnostic\nstdout:\n%s\nstderr:\n%s", c.runErr, code, c.stdout(), c.stderr)
		}
	case tc.endWhileBlocked && code == 1 && tokenDiagnostic:
		// The worker opened the FIFO without waiting and refused it as its parent ended; which came first is not
		// observable, so the status is not judged.
		t.Logf("the worker refused its token FIFO around its parent's end; its status is not judged: %v", c.runErr)
	case code != 0:
		t.Errorf("the worker ended with %v (status %d); want a clean exit, status 0, for a parent that ended before its admission boundary\nstdout:\n%s\nstderr:\n%s", c.runErr, code, c.stdout(), c.stderr)
	case !tc.productionExit && !strings.Contains(c.stdout(), "worker returned cleanly"):
		// The worker helper leaves Exit nil, so its clean stop is a return from Run.
		t.Errorf("the worker exited with status 0 but did not return from Run cleanly\nstdout:\n%s\nstderr:\n%s", c.stdout(), c.stderr)
	}
	t.Logf("worker status after its parent's end: %v", c.runErr)
	if port, ok := c.readiness(); ok {
		t.Errorf("the worker printed its readiness line on port %s after its parent ended before its admission boundary", port)
	}
	h.closeTransfer()
	if !tc.fullDeath {
		if data := lossReportBytes(t, h.reportR); len(data) != 0 {
			t.Errorf("the worker wrote %q on its report pipe after its parent ended before its admission boundary; want zero bytes", data)
		}
		requireLossNoNotice(t, h.drain)
		if err := h.res.End(); err != nil {
			t.Fatal(err)
		}
	}
	requireLossUnanswered(t, probes)
	if n := up.messages.Load(); n != 0 {
		t.Errorf("the fake upstream received %d message requests after the parent's end; want none", n)
	}
}

// readiness returns the port of the child's readiness line, once its stdout has ended.
func (c *lossChild) readiness() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range c.output {
		if m := lossReadyPattern.FindStringSubmatch(line); len(m) > 0 {
			return m[1], true
		}
	}
	return "", false
}

// lossReportBytes reads the report pipe to its end of file. Every writer is closed by then: the parent's transfer
// copy by the test and the child's by its exit.
func lossReportBytes(t *testing.T, r *os.File) []byte {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(lossReadWatchdog)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(io.LimitReader(r, 257))
	if err != nil {
		t.Fatalf("the report pipe did not reach end of file: %v", err)
	}
	return data
}

// makeLossTokenFIFO replaces the control token file at path with a FIFO that nobody writes.
func makeLossTokenFIFO(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("make the control token a FIFO: %v", err)
	}
}

// awaitLossFIFOReader reports whether the worker opened the FIFO at path for reading before it exited or deadline
// passed. It asks by opening the FIFO for writing without blocking, which succeeds only while a reader has it open,
// and keeps that writer open without writing until the case ends, so that the worker, which may have waited in its
// open, then waits in its read. The polling only stages the moment of the parent's end; it proves nothing.
func awaitLossFIFOReader(t *testing.T, path string, done <-chan struct{}, deadline time.Time) bool {
	t.Helper()
	for {
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			t.Cleanup(func() { _ = unix.Close(fd) })
			return true
		}
		if !errors.Is(err, unix.ENXIO) {
			t.Fatalf("open the token FIFO for writing: %v", err)
		}
		select {
		case <-done:
			return false
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker neither opened the token FIFO nor exited within %v of its launch", lossBound)
		}
	}
}

// lossSocketRefused reports whether one line of stderr names socket together with an address-in-use cause.
func lossSocketRefused(stderr, socket string) bool {
	for line := range strings.Lines(stderr) {
		lower := strings.ToLower(line)
		if strings.Contains(line, socket) && (strings.Contains(lower, "eaddrinuse") || strings.Contains(lower, "address already in use")) {
			return true
		}
	}
	return false
}

// lossUnixHealth sends one health request over the Unix socket at path.
func lossUnixHealth(ctx context.Context, path string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, lossReadWatchdog)
	defer cancel()
	tr := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/healthz", nil)
	if err != nil {
		return 0, err
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return 0, err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, err
}

// lossRun is Run started in this process.
type lossRun struct {
	done   chan struct{}
	err    error
	stderr *lossBuffer
	// port receives the port of the readiness line once.
	port chan string
	// printing is closed when the worker began its readiness line; release lets that line complete.
	printing chan struct{}
	release  func()
}

// startLossRun starts Run with ctx and env. With hold set the worker's readiness line does not complete until release
// is called or ctx ends.
func startLossRun(ctx context.Context, env gatewaytest.Environment, home string, hold bool) *lossRun {
	r := &lossRun{done: make(chan struct{}), stderr: new(lossBuffer), port: make(chan string, 1), printing: make(chan struct{})}
	released := make(chan struct{})
	r.release = sync.OnceFunc(func() { close(released) })
	signalPrinting := sync.OnceFunc(func() { close(r.printing) })
	stdout := writerFunc(func(p []byte) (int, error) {
		if line, ok := strings.CutPrefix(string(p), lossReadyPrefix); ok {
			port, _, _ := strings.Cut(strings.TrimSpace(line), " ")
			select {
			case r.port <- port:
			default:
			}
			signalPrinting()
			if hold {
				select {
				case <-released:
				case <-ctx.Done():
				}
			}
		}
		return len(p), nil
	})
	go func() {
		defer close(r.done)
		r.err = Run(ctx, Options{
			Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
			Home:   func() (string, error) { return home, nil },
			Cwd:    func() (string, error) { return home, nil },
			Stdout: stdout,
			Stderr: r.stderr,
		})
	}()
	return r
}

// lossAdmission records what the worker's accept loop does with the listener the test gave it.
type lossAdmission struct {
	// accepted receives one value for every connection the accept loop took.
	accepted chan struct{}
	// closed is closed once the worker closed the listener.
	closed    chan struct{}
	closeOnce sync.Once
}

// await returns once the accept loop took n connections, the worker closed the listener or returned, or deadline
// passed. It only stages the window in which the readiness line is held.
func (a *lossAdmission) await(n int, done <-chan struct{}, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for got := 0; got < n; {
		select {
		case <-a.accepted:
			got++
		case <-a.closed:
			return
		case <-done:
			return
		case <-timer.C:
			return
		}
	}
}

// lossCountingListener is the gate the worker accepts through, counting what its accept loop takes. The gate's other
// methods, the sweep announcement, the deadline and the raw connection, are the gate's own.
type lossCountingListener struct {
	*gatedListener
	adm *lossAdmission
}

func (l lossCountingListener) Accept() (net.Conn, error) {
	conn, err := l.gatedListener.Accept()
	if err == nil {
		select {
		case l.adm.accepted <- struct{}{}:
		default:
		}
	}
	return conn, err
}

func (l lossCountingListener) Close() error {
	l.adm.closeOnce.Do(func() { close(l.adm.closed) })
	return l.gatedListener.Close()
}

// openLossGate lets the gate's parked and later accepts pass.
func openLossGate(g *gatedListener) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open = true
	g.wakeLocked()
}

// lossStandaloneEnv prepares the environment of a standalone worker, which has no parent and binds its own listener.
func lossStandaloneEnv(t *testing.T) (env gatewaytest.Environment, home string) {
	t.Helper()
	home, err := os.MkdirTemp("", "mgw-loss-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	env = gatewaytest.NewEnv(home, nil, nil, nil)
	state := filepath.Join(env["CLAUDE_CONFIG_DIR"], "model-gateway")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(lossToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env["CODEX_GATEWAY_SOCKET_PATH"] = filepath.Join(home, "w.sock")
	return env, home
}

// runInProcessLoss runs an in-process row. A supervised Run receives a real reservation's listener and drain channel
// as bare descriptor numbers of this process; a standalone Run binds its own listener. The test holds the worker at
// the row's stage through the private accept gate the package's tests may put into Run's context. A standalone row
// has no parent and no report: its held readiness line marks the point after the report, and its context ends it.
func runInProcessLoss(t *testing.T, tc lossCase) {
	var h *inProcessHandoff
	var env gatewaytest.Environment
	var home string
	if tc.standalone {
		env, home = lossStandaloneEnv(t)
	} else {
		h = newInProcessHandoff(t)
		env, home = h.env, h.home
	}
	up := newLossUpstream(t, env)
	runCtx, cancelRun := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancelRun()
	adm := &lossAdmission{accepted: make(chan struct{}, 64), closed: make(chan struct{})}
	wrapping := make(chan struct{})
	signalWrapping := sync.OnceFunc(func() { close(wrapping) })
	proceed := make(chan struct{})
	releaseWrap := sync.OnceFunc(func() { close(proceed) })
	gates := make(chan *gatedListener, 1)
	addresses := make(chan string, 1)
	ctx := context.WithValue(runCtx, acceptGateKey{}, func(ln net.Listener) net.Listener {
		select {
		case addresses <- ln.Addr().String():
		default:
		}
		g := newGatedListener(ln)
		switch tc.stage {
		case lossWhileWrapping:
			signalWrapping()
			select {
			case <-proceed:
			case <-runCtx.Done():
			}
		case lossBeforeFirstAccept:
			g.closeGate()
		}
		select {
		case gates <- g:
		default:
		}
		return lossCountingListener{gatedListener: g, adm: adm}
	})

	var public string
	var probes []lossProbe
	if h != nil {
		public = "127.0.0.1:" + strconv.Itoa(int(h.port))
		// Queued on the supervisor-owned listener before the worker exists; no accept can take them before the stage.
		probes = sendLossProbes(t, public, "queued before the worker started")
	}
	r := startLossRun(ctx, env, home, true)
	defer func() {
		cancelRun()
		releaseWrap()
		r.release()
		if !awaitLoss(r.done, time.Now().Add(lossBound)) {
			t.Errorf("Run did not return %v after its context was canceled", lossBound)
		}
	}()
	// stageWait fails the case when the worker returns or the bound passes before it reaches the stage.
	stageWait := func(reached <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-reached:
		case <-r.done:
			t.Fatalf("the worker returned before %s: %v\nstderr:\n%s", what, r.err, r.stderr)
		case <-time.After(lossBound):
			t.Fatalf("the worker did not reach %s within %v of its start\nstderr:\n%s", what, lossBound, r.stderr)
		}
	}
	// address is the worker's listener as the gate saw it, once the gate was reached.
	address := func() string {
		if public == "" {
			public = <-addresses
		}
		return public
	}
	when := "sent after the parent's end"
	if !tc.parentEnds {
		when = "sent at the stage with the parent alive"
	}
	var endedAt time.Time
	end := func() {
		endedAt = time.Now()
		if tc.parentEnds {
			if err := h.liveW.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	switch tc.stage {
	case lossWhileWrapping:
		stageWait(wrapping, "the wrap of its listener")
		end()
		probes = append(probes, sendLossProbes(t, address(), when)...)
		releaseWrap()
	case lossBeforeFirstAccept:
		gateReached := make(chan struct{})
		var g *gatedListener
		go func() {
			select {
			case g = <-gates:
				close(gateReached)
			case <-runCtx.Done():
			}
		}()
		stageWait(gateReached, "the wrap of its listener")
		probes = append(probes, sendLossProbes(t, address(), "queued while the first accept is held")...)
		if h != nil {
			readLossReport(t, h.reportR, h.port)
		} else {
			stageWait(r.printing, "its readiness line")
		}
		end()
		probes = append(probes, sendLossProbes(t, address(), when)...)
		openLossGate(g)
	}
	// While the readiness line is held, a worker that reads its liveness pipe only after that line keeps accepting.
	hold := time.NewTimer(time.Until(endedAt.Add(lossHold)))
	select {
	case <-r.printing:
		probes = append(probes, sendLossProbes(t, address(), "sent while the readiness line is held")...)
	case <-r.done:
	case <-hold.C:
	}
	hold.Stop()
	adm.await(len(probes), r.done, endedAt.Add(lossHold))
	r.release()

	if !tc.parentEnds {
		requireLossAnswered(t, probes)
		if got, want := up.messages.Load(), lossMessageProbes(probes); got != want {
			t.Errorf("the fake upstream received %d message requests; want %d", got, want)
		}
		stoppedAt := time.Now()
		if h == nil {
			cancelRun()
		} else if err := h.liveW.Close(); err != nil {
			t.Fatal(err)
		}
		if !awaitLoss(r.done, stoppedAt.Add(lossBound)) {
			t.Fatalf("Run was still running %v after it was told to stop", lossBound)
		}
		if r.err != nil {
			t.Errorf("Run returned %v after it was told to stop; want nil\nstderr:\n%s", r.err, r.stderr)
		}
		if h != nil {
			requireLossNoNotice(t, h.drain)
		}
		return
	}
	if !awaitLoss(r.done, endedAt.Add(lossBound)) {
		t.Errorf("Run was still running %v after its parent ended before it served (a wait at a startup step)\nstderr:\n%s", lossBound, r.stderr)
		return
	}
	t.Logf("Run returned %v after its parent's end", r.err)
	// Run owns the report descriptor it was handed and has closed it by now. In the first-accept rows the report was
	// read to its end before the parent's end, so the worker had passed its admission boundary with a live parent.
	report := lossReportBytes(t, h.reportR)
	printed := false
	select {
	case <-r.printing:
		printed = true
	default:
	}
	beforeBoundary := tc.stage == lossWhileWrapping && len(report) == 0 && !printed
	requireLossNoNotice(t, h.drain)
	if err := h.res.End(); err != nil {
		t.Fatal(err)
	}
	if !beforeBoundary {
		t.Logf("the worker passed its admission boundary before its parent's end (report %q, readiness line %t); what it admitted then is not pinned", report, printed)
		return
	}
	requireLossUnanswered(t, probes)
	if n := up.messages.Load(); n != 0 {
		t.Errorf("the fake upstream received %d message requests after the parent's end; want none", n)
	}
}

// runStandaloneLoss checks the fixture itself with a standalone worker, which has no parent: both probes are answered,
// the fake upstream receives the message request, and the worker's socket reclaim lock is the regular file
// socket+".lock" that holdLossSocketLock takes.
func runStandaloneLoss(t *testing.T) {
	env, home := lossStandaloneEnv(t)
	socket := env["CODEX_GATEWAY_SOCKET_PATH"]
	up := newLossUpstream(t, env)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	r := startLossRun(ctx, env, home, false)
	defer func() {
		cancel()
		if !awaitLoss(r.done, time.Now().Add(lossBound)) {
			t.Errorf("Run did not return %v after its context was canceled", lossBound)
		}
	}()
	var port string
	select {
	case port = <-r.port:
	case <-r.done:
		t.Fatalf("the standalone worker returned before its readiness line: %v\nstderr:\n%s", r.err, r.stderr)
	case <-time.After(lossBound):
		t.Fatalf("the standalone worker printed no readiness line within %v\nstderr:\n%s", lossBound, r.stderr)
	}
	probes := sendLossProbes(t, "127.0.0.1:"+port, "to a standalone worker")
	requireLossAnswered(t, probes)
	if got, want := up.messages.Load(), lossMessageProbes(probes); got != want {
		t.Errorf("the fake upstream received %d message requests; want %d", got, want)
	}
	if status, err := lossUnixHealth(t.Context(), socket); err != nil || status != http.StatusOK {
		t.Errorf("health over the standalone worker's Unix socket: status %d, %v; want 200", status, err)
	}
	if info, err := os.Lstat(socket + ".lock"); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the socket reclaim lock %s: %v, %v; want the regular file the lock rows hold", socket+".lock", info, err)
	}
	cancel()
	if !awaitLoss(r.done, time.Now().Add(lossBound)) {
		t.Fatalf("the standalone worker did not return %v after its context was canceled", lossBound)
	}
	if r.err != nil {
		t.Errorf("the standalone worker returned %v; want nil\nstderr:\n%s", r.err, r.stderr)
	}
}
