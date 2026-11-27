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

package serveshim_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const (
	// recoveryBound is the ruled time after a public drain retired a generation that never announced its drain, at
	// which the supervisor stops that generation's worker.
	recoveryBound = 10 * time.Second
	// recoveryEarly is how far before recoveryBound, counted from the moment the public drain was sent, the worker must
	// still run: a margin for the retirement's lag behind the sending.
	recoveryEarly = time.Second
	// recoveryLate is the venue margin after recoveryBound, counted from the first refused request after the drain.
	recoveryLate = 8 * time.Second
	// recoveryWatch is how long past recoveryBound a case that forbids a recovery stop keeps watching. A recovery
	// armed at the retirement would have fired recoveryWatch before the watch ends.
	recoveryWatch = 4 * time.Second
	// recoveryDrainBound is how long after its generation is gone a pending public drain may wait for its answer. A
	// supervisor that answers when the generation ends needs moments; the bound is for a loaded venue.
	recoveryDrainBound = 10 * time.Second
)

// recoveryStderr is the supervisor's stderr in the recovery cases: it collects what is written, and once armed every
// Write blocks until free is called, as a pipe that nobody reads and that is already full blocks its writer.
type recoveryStderr struct {
	buf ownedBuffer
	mu  sync.Mutex
	// armed makes every later Write block.
	armed bool
	// entered is closed when the first Write blocked.
	entered     chan struct{}
	enterOnce   sync.Once
	release     chan struct{}
	releaseOnce sync.Once
}

func newRecoveryStderr() *recoveryStderr {
	return &recoveryStderr{entered: make(chan struct{}), release: make(chan struct{})}
}

// arm makes every Write from now on block until free is called.
func (w *recoveryStderr) arm() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armed = true
}

// free lets every blocked and later Write through. It is idempotent.
func (w *recoveryStderr) free() { w.releaseOnce.Do(func() { close(w.release) }) }

// blocked reports whether a Write has blocked so far.
func (w *recoveryStderr) blocked() bool {
	select {
	case <-w.entered:
		return true
	default:
		return false
	}
}

func (w *recoveryStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	armed := w.armed
	w.mu.Unlock()
	if armed {
		w.enterOnce.Do(func() { close(w.entered) })
		<-w.release
	}
	return w.buf.Write(p)
}

// startRecoveryShim is startOwnedShim with stderr as the supervisor's stderr and hooks installed. Its cleanup frees
// stderr before it stops the supervisor, so that a blocked warning never keeps the stop from finishing.
func startRecoveryShim(t *testing.T, stderr *recoveryStderr, hooks serveshim.Hooks) *ownedShim {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(raw[:])
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// sun_path is short; the test's temporary directory can exceed it.
	sockDir, err := os.MkdirTemp("", "mg-rec-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(sockDir); err != nil {
			t.Error(err)
		}
	})
	controlPath := filepath.Join(sockDir, "c.sock")
	var lc net.ListenConfig
	control, err := lc.Listen(t.Context(), "unix", controlPath)
	if err != nil {
		t.Fatal(err)
	}
	s := &ownedShim{state: state, token: token, ready: make(chan int, 1), done: make(chan struct{}), stderr: &stderr.buf, children: make(chan *ownedChild, 8)}
	var accepted sync.WaitGroup
	var connsMu sync.Mutex
	var conns []net.Conn
	stopping := make(chan struct{})
	accepted.Go(func() {
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
			_ = conn.SetDeadline(time.Now().Add(40 * time.Second))
			lines := bufio.NewScanner(conn)
			if !lines.Scan() {
				continue
			}
			v, ok := strings.CutPrefix(lines.Text(), "hello ")
			pid, err := strconv.Atoi(v)
			if !ok || err != nil {
				continue
			}
			c := &ownedChild{pid: pid, conn: conn, lines: lines}
			if lines.Scan() {
				line := lines.Text()
				if v, ok := strings.CutPrefix(line, "inherited "); ok {
					port, gen, _ := strings.Cut(v, " ")
					c.port, _ = strconv.Atoi(port)
					c.generation = gen
				} else {
					c.refusal = line
				}
			} else {
				c.refusal = "the child ended before it published what it adopted"
				c.unpublished = true
			}
			select {
			case s.children <- c:
			case <-stopping:
				return
			}
		}
	})

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(exe, '\'') {
		t.Fatalf("test executable path %q cannot be quoted for the launcher script", exe)
	}
	// The launcher runs this binary again as the owned eligibility child, which TestEndpointEligibility dispatches on
	// the control key; the pattern must name that test exactly, or the child runs no test and never reports.
	launcher := filepath.Join(home, "worker-launcher")
	script := "#!/bin/sh\nexec '" + exe + "' -test.run='^TestEndpointEligibility$'\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	overrides := map[string]string{"MODEL_GATEWAY_STATE_DIR": state}
	fixture := gatewaytest.NewEnv(home, nil, map[string]string{ownedControlKey: controlPath}, overrides)
	entries := make([]string, 0, len(fixture))
	for k, v := range fixture {
		entries = append(entries, k+"="+v)
	}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	s.cancel = cancel
	runCtx := serveshim.WithHooks(ctx, hooks)
	var watched sync.WaitGroup
	s.watched = &watched
	var watchErr error
	watched.Go(func() {
		lines := bufio.NewScanner(pr)
		for lines.Scan() {
			if v, ok := strings.CutPrefix(lines.Text(), "model-gateway shim supervisor listening on 127.0.0.1:"); ok {
				if n, err := strconv.Atoi(v); err == nil {
					select {
					case s.ready <- n:
					default:
					}
				}
			}
		}
		watchErr = lines.Err()
		_ = pr.Close()
	})
	s.startedAt = time.Now()
	go func() {
		defer close(s.done)
		s.result = serveshim.Run(runCtx, serveshim.Options{
			Environment: entries, Home: func() (string, error) { return home, nil }, Executable: launcher,
			Version: fixtureVersion, Stdout: pw, Stderr: stderr,
		})
		s.doneAt = time.Now()
		_ = pw.Close()
	}()
	t.Cleanup(func() {
		stderr.free()
		cancel()
		<-s.done
		watched.Wait()
		if watchErr != nil {
			t.Errorf("read the supervisor's stdout: %v", watchErr)
		}
		close(stopping)
		_ = control.Close()
		connsMu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		connsMu.Unlock()
		accepted.Wait()
	})
	return s
}

// recoveryExits records the pids whose exit the supervisor observed, with the time it observed each.
type recoveryExits struct {
	mu   sync.Mutex
	seen map[int][]time.Time
	// notify receives a value after each observation; it never blocks the supervisor.
	notify chan struct{}
}

func newRecoveryExits() *recoveryExits {
	return &recoveryExits{seen: make(map[int][]time.Time), notify: make(chan struct{}, 1)}
}

// observe is the ExitObserved hook.
func (e *recoveryExits) observe(pid int) {
	e.mu.Lock()
	e.seen[pid] = append(e.seen[pid], time.Now())
	e.mu.Unlock()
	select {
	case e.notify <- struct{}{}:
	default:
	}
}

// count returns how many exits of pid the supervisor observed so far.
func (e *recoveryExits) count(pid int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.seen[pid])
}

// await returns whether an exit of pid was observed before deadline.
func (e *recoveryExits) await(pid int, deadline time.Time) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for e.count(pid) == 0 {
		select {
		case <-e.notify:
		case <-timer.C:
			return e.count(pid) != 0
		}
	}
	return true
}

// recoveryAlive asks a worker for its request counts and requires the answer want ("requests ..."): the worker is
// alive and its parent liveness never ended. A farewell instead says that the supervisor stopped it.
func recoveryAlive(t *testing.T, c *ownedChild, want, while string) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c.conn, "requests\n"); err != nil {
		t.Fatalf("ask worker %d for its counts %s: %v", c.pid, while, err)
	}
	if !c.lines.Scan() {
		t.Fatalf("worker %d closed its control connection %s without answering: %v", c.pid, while, c.lines.Err())
	}
	got := c.lines.Text()
	if farewell, ok := strings.CutPrefix(got, "parent gone "); ok {
		t.Fatalf("worker %d was stopped by its supervisor %s (farewell counts %q); want it running", c.pid, while, farewell)
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("counts of worker %d %s (-want +got):\n%s", c.pid, while, diff)
	}
}

// recoveryFarewell waits until deadline for the farewell a worker sends when its parent liveness ends.
func recoveryFarewell(t *testing.T, c *ownedChild, deadline time.Time, what string) string {
	t.Helper()
	_ = c.conn.SetReadDeadline(deadline)
	if !c.lines.Scan() {
		t.Fatalf("%s: worker %d sent no farewell by %v (%v)", what, c.pid, time.Until(deadline), c.lines.Err())
	}
	return c.lines.Text()
}

// recoveryUntil waits until at, unless the supervisor stops first, which fails the case.
func recoveryUntil(t *testing.T, s *ownedShim, at time.Time) {
	t.Helper()
	if s.awaitStop(at) {
		t.Fatalf("the supervisor stopped during the watch: %v; stderr: %s", s.result, s.stderr)
	}
}

// recoveryInFlight starts a probe that the worker holds inside its handler, and returns it once the worker holds it.
func recoveryInFlight(t *testing.T, s *ownedShim, child *ownedChild) *ownedInFlight {
	t.Helper()
	child.command(t, "hold", "armed")
	p := s.startProbe(t, rand.Text())
	child.command(t, "await-held", "held")
	return p
}

// recoveryAnswered requires the held probe's answer to be the worker's own.
func recoveryAnswered(t *testing.T, p *ownedInFlight, child *ownedChild, what string) {
	t.Helper()
	var a ownedAnswer
	select {
	case a = <-p.answer:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: no answer within 10s of its release", what)
	}
	if a.err != nil {
		t.Fatalf("%s: %v; want the worker's answer", what, a.err)
	}
	if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: child.pid}, a.result); diff != "" {
		t.Errorf("%s (-want +got):\n%s", what, diff)
	}
}

// recoveryDrainAnswer is what the client of one public drain received: its status, every X-Should-Retry value, the
// Content-Length the answer declared (-1 when it declared none), how many body bytes were read and the error that
// ended reading the body before its end, or the error that ended the request; and when it ended.
type recoveryDrainAnswer struct {
	status   int
	retry    []string
	length   int64
	bodyRead int64
	bodyErr  error
	err      error
	at       time.Time
}

// recoveryPublicDrain sends the authenticated public drain as publicDrain does and returns the whole answer, headers
// included. The budget is longer than any watch of these cases, so that the request ends by the supervisor's answer.
func recoveryPublicDrain(ctx context.Context, s *ownedShim) recoveryDrainAnswer {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+s.public+"/drain", strings.NewReader("{}"))
	if err != nil {
		return recoveryDrainAnswer{err: err, at: time.Now()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Model-Gateway-Control-Token", s.token)
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return recoveryDrainAnswer{err: err, at: time.Now()}
	}
	n, bodyErr := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return recoveryDrainAnswer{
		status: resp.StatusCode, retry: resp.Header.Values("X-Should-Retry"), length: resp.ContentLength, bodyRead: n,
		bodyErr: bodyErr, at: time.Now(),
	}
}

// recoveryDrain sends the public drain from another goroutine; the cleanup joins it.
func recoveryDrain(t *testing.T, s *ownedShim) <-chan recoveryDrainAnswer {
	t.Helper()
	drained := make(chan recoveryDrainAnswer, 1)
	go func() {
		drained <- recoveryPublicDrain(context.WithoutCancel(t.Context()), s)
		// A case may read the result itself; the close lets the cleanup's join return after that read too.
		close(drained)
	}()
	t.Cleanup(func() { <-drained })
	return drained
}

// TestAnsweredDrainNoticeSuppressesRecovery checks that once a worker announced its drain and the supervisor validated
// and answered that notice, no recovery stop ever happens for that generation, although a public drain retired it:
// the worker keeps running well past ten seconds after the retirement, and a request it holds in flight across that
// moment is answered by it. It rejects a supervisor that arms the recovery at the retirement and does not disarm it
// when a valid notice of the same generation arrives, and one whose late timer acts on a generation that already
// announced. The last row is the control: the same public drain, held by the worker without a notice, ends in the
// recovery stop at the bound, so the watch of the other rows is long enough to see one.
//
// The ruled case in which a public drain's lease, acquired before the notice, reaches its retirement only after the
// notice was answered cannot be forced: no hook sits between the public drain's lease and its retirement, and the
// RelayLookup hook must not block. A hook that runs after the public drain acquired its lease and before it retires the
// generation, and that the test can hold, would decide it. Timeline of the watched rows, from the child's start: the
// drain about one second in, the watch to the drain plus fourteen seconds, the held request answered by about sixteen
// seconds, inside both the probe's twenty-second budget and the forty seconds of the child's control connection.
func TestAnsweredDrainNoticeSuppressesRecovery(t *testing.T) {
	tests := map[string]struct {
		run func(t *testing.T, s *ownedShim, exits *recoveryExits)
	}{
		"error: a notice answered while the public drain is held at the worker is never followed by a recovery stop": {func(t *testing.T, s *ownedShim, exits *recoveryExits) {
			child := rulingServingChild(t, s)
			inFlight := recoveryInFlight(t, s, child)
			child.command(t, "drain-hold", "armed")
			sentAt := time.Now()
			drained := recoveryDrain(t, s)
			if diff := gocmp.Diff("drain valid", child.command(t, "await-drain", "drain ")); diff != "" {
				t.Fatalf("the drain the supervisor delivered (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("answered", child.command(t, "notice", "")); diff != "" {
				t.Fatalf("the worker's drain notice (-want +got):\n%s", diff)
			}
			child.command(t, "release", "released")
			recoveryAnswered(t, inFlight, child, "the request held across the drain")
			child.command(t, "drain-respond", "responded")
			if a := <-drained; a.err != nil {
				t.Errorf("the public drain: %v", a.err)
			}
			recoveryUntil(t, s, sentAt.Add(recoveryBound+recoveryWatch))
			recoveryAlive(t, child, "requests 0 2 0 1", "after the watch past the recovery bound")
			if n := exits.count(child.pid); n != 0 {
				t.Errorf("the supervisor observed %d exits of the worker whose notice it answered; want none", n)
			}
		}},
		"error: a request in flight across the recovery bound survives a public drain whose notice was answered": {func(t *testing.T, s *ownedShim, exits *recoveryExits) {
			child := rulingServingChild(t, s)
			inFlight := recoveryInFlight(t, s, child)
			sentAt := time.Now()
			drained := recoveryDrain(t, s)
			if diff := gocmp.Diff("answered", child.command(t, "drain-result", "")); diff != "" {
				t.Fatalf("the worker's drain exchange (-want +got):\n%s", diff)
			}
			if a := <-drained; a.err != nil {
				t.Errorf("the public drain: %v", a.err)
			}
			// The probe was sent just before sentAt; its budget of twenty seconds covers the watch.
			recoveryUntil(t, s, sentAt.Add(recoveryBound+recoveryWatch-time.Second))
			recoveryAlive(t, child, "requests 0 2 0 1", "while it holds a request past the recovery bound")
			child.command(t, "release", "released")
			recoveryAnswered(t, inFlight, child, "the request held past the recovery bound")
			if n := exits.count(child.pid); n != 0 {
				t.Errorf("the supervisor observed %d exits of the worker whose notice it answered; want none", n)
			}
		}},
		"error: the control, a public drain held at the worker without a notice, ends in the recovery stop at the bound": {func(t *testing.T, s *ownedShim, exits *recoveryExits) {
			child := rulingServingChild(t, s)
			child.command(t, "drain-hold", "armed")
			sentAt := time.Now()
			recoveryDrain(t, s)
			if diff := gocmp.Diff("drain valid", child.command(t, "await-drain", "drain ")); diff != "" {
				t.Fatalf("the drain the supervisor delivered (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(ownedRetryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the public drain (-want +got):\n%s", diff)
			}
			retiredBy := time.Now()
			farewell := recoveryFarewell(t, child, retiredBy.Add(recoveryBound+recoveryLate), "the retired worker that never announced its drain")
			goneAt := time.Now()
			if diff := gocmp.Diff("parent gone 0 1 0 1", farewell); diff != "" {
				t.Errorf("the retired worker's farewell (-want +got):\n%s", diff)
			}
			if goneAt.Before(sentAt.Add(recoveryBound - recoveryEarly)) {
				t.Errorf("the retired worker was stopped %v after the public drain was sent; want no stop before %v after its retirement", goneAt.Sub(sentAt), recoveryBound)
			}
			if !exits.await(child.pid, time.Now().Add(10*time.Second)) {
				t.Error("the supervisor did not observe the retired worker's exit within 10s")
			}
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exits := newRecoveryExits()
			s := startRecoveryShim(t, newRecoveryStderr(), serveshim.Hooks{ExitObserved: exits.observe})
			tc.run(t, s, exits)
		})
	}
}

// TestRecoveryStopsDespiteBlockedWarning checks the recovery stop of a retired worker that never announced its drain
// while the supervisor's stderr blocks every write, as a full pipe that nobody reads does: the worker is still stopped
// once by the immediate path, not before ten seconds after the retirement and within a generous bound after it, and
// the supervisor observes its exit once its output flows again. It rejects a supervisor that writes its warning before
// it ends the worker's liveness, which would then never stop the worker. The blocked row also requires that the
// supervisor tried to write while blocked, so that the block was in its way; the control row, with stderr flowing,
// requires the warning to appear and the exit to be observed.
//
// The failed delivery is staged as in TestFailedPublicDrainStopsRetiredWorker. The block is armed just before the
// public drain is sent, so that the supervisor's start is not held. The case takes about twelve seconds.
func TestRecoveryStopsDespiteBlockedWarning(t *testing.T) {
	tests := map[string]struct {
		blocked bool
	}{
		"error: a recovery stop with the supervisor's stderr blocked still stops the retired worker at the bound": {blocked: true},
		"error: the control, a recovery stop with stderr flowing, stops the worker and writes a warning":          {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exits := newRecoveryExits()
			stderr := newRecoveryStderr()
			s := startRecoveryShim(t, stderr, serveshim.Hooks{ExitObserved: exits.observe})
			child := rulingServingChild(t, s)
			child.command(t, "hold-accept", "armed")
			mark := len(stderr.buf.String())
			if tc.blocked {
				stderr.arm()
			}
			sentAt := time.Now()
			recoveryDrain(t, s)
			child.command(t, "await-accept", "accept held")
			if diff := gocmp.Diff(ownedRetryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the public drain (-want +got):\n%s", diff)
			}
			retiredBy := time.Now()
			farewell := recoveryFarewell(t, child, retiredBy.Add(recoveryBound+recoveryLate), "the retired worker")
			goneAt := time.Now()
			if diff := gocmp.Diff("parent gone 0 1 0 0", farewell); diff != "" {
				t.Errorf("the retired worker's farewell (-want +got):\n%s", diff)
			}
			child.exited(t)
			if goneAt.Before(sentAt.Add(recoveryBound - recoveryEarly)) {
				t.Errorf("the retired worker was stopped %v after the public drain was sent; want no stop before %v after its retirement", goneAt.Sub(sentAt), recoveryBound)
			}
			if tc.blocked {
				select {
				case <-stderr.entered:
				case <-time.After(5 * time.Second):
					t.Error("the supervisor wrote nothing to its blocked stderr within 5s of the recovery stop; the block was never in its way")
				}
				if n := exits.count(child.pid); n > 1 {
					t.Errorf("the supervisor observed %d exits of the retired worker; want at most one", n)
				}
				stderr.free()
			} else {
				deadline := time.Now().Add(5 * time.Second)
				for len(stderr.buf.String()) == mark && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if len(stderr.buf.String()) == mark {
					t.Error("the supervisor wrote no warning within 5s of stopping the retired worker")
				}
				if stderr.blocked() {
					t.Error("test infrastructure: the unarmed stderr blocked; not a product failure")
				}
			}
			if !exits.await(child.pid, time.Now().Add(10*time.Second)) {
				t.Error("the supervisor did not observe the retired worker's exit within 10s of its output flowing")
			}
			if n := exits.count(child.pid); n != 1 {
				t.Errorf("the supervisor observed %d exits of the retired worker; want one", n)
			}
		})
	}
}

// TestFailedAcknowledgmentDisconnectsDespiteBlockedWarning checks a drain notice that the supervisor validated and whose
// acknowledgment then fails, while the supervisor's stderr blocks every write as a full pipe that nobody reads does:
// the supervisor still disconnects that generation's worker by the immediate path within a bound, so the worker says
// goodbye with the counts of what it served, although the warning about the failed acknowledgment cannot be written.
// Nothing else is stopped: the supervisor keeps running, and once its output flows again it observes the exit once
// and starts a replacement that serves. It rejects a supervisor that writes its warning before it disconnects the
// worker, which then never disconnects it while the write blocks. The blocked row also requires that the supervisor
// tried to write while blocked, so that the block was in its way; the control row, with stderr flowing, requires the
// warning to appear.
//
// The failed acknowledgment is staged as in the frozen row "a worker whose drain cannot be acknowledged is stopped and
// receives nothing more" of TestEndpointEligibility: the child announces its drain and closes its end of the drain
// channel, and the Acknowledging hook, which runs once the notice was validated and before the answer is written,
// holds the answer until that close has happened, so that the write fails on every system. The block is armed while
// the answer is held, after the supervisor's start and its validation of the notice. Whether the write that blocks
// comes after the disconnect is not observable apart from the bound: a supervisor that writes first never disconnects.
func TestFailedAcknowledgmentDisconnectsDespiteBlockedWarning(t *testing.T) {
	// disconnectBound is how long after the held answer is let go the worker may wait for its disconnection. A
	// supervisor whose write of the answer fails needs moments; the bound is for a loaded venue.
	const disconnectBound = 10 * time.Second
	tests := map[string]struct {
		blocked bool
	}{
		"error: a failed acknowledgment with the supervisor's stderr blocked still disconnects the worker":             {blocked: true},
		"error: the control, a failed acknowledgment with stderr flowing, disconnects the worker and writes a warning": {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exits := newRecoveryExits()
			stderr := newRecoveryStderr()
			ackGo, ackEntered := make(chan struct{}), make(chan struct{}, 1)
			letAck := sync.OnceFunc(func() { close(ackGo) })
			s := startRecoveryShim(t, stderr, serveshim.Hooks{
				ExitObserved: exits.observe,
				Acknowledging: func() {
					select {
					case ackEntered <- struct{}{}:
					default:
					}
					<-ackGo
				},
			})
			// Registered after the supervisor's cleanup, so it runs first and a failed case never leaves the answer held.
			t.Cleanup(letAck)
			child := rulingServingChild(t, s)
			child.command(t, "drop-notice", "dropped")
			select {
			case <-ackEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("the supervisor did not reach its answer to the worker's validated drain notice within 10s")
			}
			mark := len(stderr.buf.String())
			if tc.blocked {
				stderr.arm()
			}
			releasedAt := time.Now()
			letAck()
			farewell := recoveryFarewell(t, child, releasedAt.Add(disconnectBound), "the worker whose drain could not be acknowledged")
			if diff := gocmp.Diff("parent gone 0 1 0 0", farewell); diff != "" {
				t.Errorf("the farewell of the worker whose drain could not be acknowledged (-want +got):\n%s", diff)
			}
			child.exited(t)
			if tc.blocked {
				select {
				case <-stderr.entered:
				case <-time.After(5 * time.Second):
					t.Error("the supervisor wrote nothing to its blocked stderr within 5s of the disconnection; the block was never in its way")
				}
				if s.awaitStop(time.Now()) {
					t.Errorf("the supervisor stopped while its stderr was blocked: %v; want only the worker disconnected", s.result)
				}
				if n := exits.count(child.pid); n > 1 {
					t.Errorf("the supervisor observed %d exits of the worker; want at most one", n)
				}
				stderr.free()
			} else {
				deadline := time.Now().Add(5 * time.Second)
				for len(stderr.buf.String()) == mark && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				if len(stderr.buf.String()) == mark {
					t.Error("the supervisor wrote no warning within 5s of disconnecting the worker whose drain could not be acknowledged")
				}
				if stderr.blocked() {
					t.Error("test infrastructure: the unarmed stderr blocked; not a product failure")
				}
			}
			if !exits.await(child.pid, time.Now().Add(10*time.Second)) {
				t.Error("the supervisor did not observe the worker's exit within 10s of its output flowing")
			}
			if n := exits.count(child.pid); n != 1 {
				t.Errorf("the supervisor observed %d exits of the worker; want one", n)
			}
			replacement := s.nextChild(t)
			replacement.command(t, "serve", "serving")
			replacement.command(t, "report", "reported")
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: replacement.pid}, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request to the replacement after the failed acknowledgment (-want +got):\n%s", diff)
			}
			if s.awaitStop(time.Now()) {
				t.Errorf("the supervisor stopped: %v; want it running", s.result)
			}
		})
	}
}

// TestRecoveryLeavesReplacementRunning checks that a recovery armed for a retired generation never stops the
// generation that replaces it: after the retired worker was stopped by its recovery, or exited by itself before the
// bound, the supervisor starts and publishes a new worker, and that worker keeps running past the old bound and serves
// requests; no drain reaches it. The public drain that was pending for the retired generation is answered, within a
// bound after that generation is gone, with 503 and exactly one X-Should-Retry header whose value is false. It rejects
// a recovery timer that stops whatever worker is current when it fires instead of the generation it was armed for, one
// that is armed again or keeps a supervisor-wide flag, a supervisor that sends the pending drain on to the replacement,
// and one that ends the pending drain with a retryable 503, another status, a closed connection, or an incomplete
// answer: a head whose body cannot be read to its end, or a body shorter or longer than its declared Content-Length.
//
// In the first row the replacement must already have answered a request before the earliest moment the old recovery
// can fire, the drain's sending plus ten seconds; otherwise nothing is judged and the case fails as test
// infrastructure. The retired worker therefore exits about one second after the retirement rather than just before
// the bound: the exit only has to come before it, and the replacement needs the time to start. Timelines: the first
// row ends about fifteen seconds after the drain, the second about twenty-five seconds after it at the latest, inside
// the forty seconds of each child's control connection.
func TestRecoveryLeavesReplacementRunning(t *testing.T) {
	tests := map[string]struct {
		// exits makes the retired worker exit by itself before the bound.
		exits bool
	}{
		"error: a retired worker that exits before the bound is replaced and the old recovery never stops the replacement":  {exits: true},
		"error: a retired worker stopped at the bound is replaced and the replacement runs through another recovery period": {},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			exits := newRecoveryExits()
			s := startRecoveryShim(t, newRecoveryStderr(), serveshim.Hooks{ExitObserved: exits.observe})
			retired := rulingServingChild(t, s)
			retired.command(t, "hold-accept", "armed")
			sentAt := time.Now()
			drained := recoveryDrain(t, s)
			retired.command(t, "await-accept", "accept held")
			if diff := gocmp.Diff(ownedRetryable, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request after the public drain (-want +got):\n%s", diff)
			}
			retiredBy := time.Now()
			// watchEnd is when the replacement must still be running.
			var watchEnd time.Time
			if tc.exits {
				retired.command(t, "exit", "closed")
				retired.exited(t)
			} else {
				farewell := recoveryFarewell(t, retired, retiredBy.Add(recoveryBound+recoveryLate), "the retired worker")
				if diff := gocmp.Diff("parent gone 0 1 0 0", farewell); diff != "" {
					t.Errorf("the retired worker's farewell (-want +got):\n%s", diff)
				}
				retired.exited(t)
			}
			goneAt := time.Now()
			replacement := s.nextChild(t)
			replacement.command(t, "serve", "serving")
			replacement.command(t, "report", "reported")
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: replacement.pid}, s.probe(t, rand.Text())); diff != "" {
				t.Fatalf("request to the replacement (-want +got):\n%s", diff)
			}
			servedAt := time.Now()
			if tc.exits {
				if !servedAt.Before(sentAt.Add(recoveryBound)) {
					t.Fatalf("test infrastructure: the replacement first answered %v after the drain was sent, not before the old recovery could fire; nothing is judged; not a product failure", servedAt.Sub(sentAt))
				}
				watchEnd = retiredBy.Add(recoveryBound + recoveryWatch)
			} else {
				watchEnd = servedAt.Add(recoveryBound + recoveryWatch)
			}
			recoveryUntil(t, s, watchEnd)
			recoveryAlive(t, replacement, "requests 0 1 0 0", "after the watch past the old recovery bound")
			if diff := gocmp.Diff(ownedProbeResult{Status: http.StatusOK, ChildPID: replacement.pid}, s.probe(t, rand.Text())); diff != "" {
				t.Errorf("request to the replacement after the watch (-want +got):\n%s", diff)
			}
			if n := exits.count(replacement.pid); n != 0 {
				t.Errorf("the supervisor observed %d exits of the replacement; want none", n)
			}
			if n := exits.count(retired.pid); n != 1 {
				t.Errorf("the supervisor observed %d exits of the retired worker; want one", n)
			}
			// The pending public drain, which reached no generation but the retired one, ends with the final refusal.
			var answer recoveryDrainAnswer
			select {
			case answer = <-drained:
			case <-time.After(10 * time.Second):
				t.Fatal("the pending public drain was not answered within 10s of the watch's end")
			}
			if answer.err != nil {
				t.Fatalf("the pending public drain ended with %v; want the supervisor's answer", answer.err)
			}
			type final struct {
				Status int
				Retry  []string
			}
			if diff := gocmp.Diff(final{Status: http.StatusServiceUnavailable, Retry: []string{"false"}}, final{Status: answer.status, Retry: answer.retry}); diff != "" {
				t.Errorf("the answer to the pending public drain once its generation was gone (-want +got):\n%s", diff)
			}
			// The answer must be whole: a head followed by a cut connection is not an answer.
			if answer.bodyErr != nil {
				t.Errorf("the body of the answer to the pending public drain could not be read to its end after %d bytes: %v", answer.bodyRead, answer.bodyErr)
			}
			if answer.length >= 0 && answer.bodyRead != answer.length {
				t.Errorf("the answer to the pending public drain declared Content-Length %d and carried %d bytes", answer.length, answer.bodyRead)
			}
			if late := answer.at.Sub(goneAt); late > recoveryDrainBound {
				t.Errorf("the pending public drain was answered %v after the retired worker was gone; want within %v", late, recoveryDrainBound)
			}
		})
	}
}

// TestRecoveryStderrStaging checks the blocking stderr of the recovery cases on any tree: a Write before arming
// passes, a Write after arming blocks and is recorded until free, and then completes.
func TestRecoveryStderrStaging(t *testing.T) {
	tests := map[string]struct{}{
		"success: the recovery stderr passes writes until armed and then blocks them until freed": {},
	}
	for name := range tests {
		t.Run(name, func(t *testing.T) {
			w := newRecoveryStderr()
			if _, err := io.WriteString(w, "before\n"); err != nil {
				t.Fatal(err)
			}
			w.arm()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = io.WriteString(w, "blocked\n")
			}()
			select {
			case <-w.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("an armed write did not reach the block within 5s")
			}
			select {
			case <-done:
				t.Fatal("an armed write returned before free")
			default:
			}
			w.free()
			<-done
			if diff := gocmp.Diff("before\nblocked\n", w.buf.String()); diff != "" {
				t.Errorf("what the recovery stderr collected (-want +got):\n%s", diff)
			}
		})
	}
}
