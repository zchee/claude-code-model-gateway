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

package serveshim

import (
	"bufio"
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

const (
	// reportTimeout is how long a worker has to report its listener (lib/commands.js:2488). The source applies it only
	// without a configured port; here it applies to a configured port too, which a worker must also report before it
	// receives a request.
	reportTimeout = 5 * time.Second
	// maxReport is the longest accepted report line, newline included.
	maxReport = 256
	// respawnDelay is the pause before a worker that exited on its own is started again (lib/commands.js:2639).
	respawnDelay = 50 * time.Millisecond
	// exitWait bounds each wait for a stopping worker before it is signalled (lib/commands.js:2532-2546,2568-2571).
	exitWait = 3 * time.Second
	// birthWait bounds the sampling of a fresh child's kernel birth time for its pid record.
	birthWait = time.Second
)

type shim struct {
	lifetime       context.Context
	env            environment
	state          string
	executable     string
	version        string
	stderr         io.Writer
	pid            int
	startedAt      string
	files          lifecycle.Files
	configuredPort int
	fatal          chan error

	// workers holds the listener and drain channel the supervisor reserves for each worker generation, and gives the
	// relay its leases. A generation becomes eligible only after its current worker reported the reserved port for its
	// own generation and its pid record was published; a port number alone, configured or not, never makes a worker
	// eligible, and the relay never reaches a worker by its port. It is created on first use.
	workers     *workerlink.Registry
	workersOnce sync.Once

	// prep is the context of every launch's preparation, the reservation and the wait for the start fence; the stop
	// cancels it, so that a contested fence cannot hold a stopping supervisor. A worker's own lifetime is not bound to
	// it. It is created on first use.
	prep       context.Context
	prepCancel context.CancelFunc
	prepOnce   sync.Once

	// publishMu serializes the final current-worker check with the pid record write, so that a slow publication of an
	// earlier worker can never land after its replacement's.
	publishMu sync.Mutex

	mu      sync.Mutex
	stopped bool
	current *workerProc
	// launching is set while a launch runs outside mu, so that no second launch begins meanwhile.
	launching bool
	// eligible is the current worker while its generation is published for relay, or nil.
	eligible    *workerProc
	respawn     *time.Timer
	reportTimer *time.Timer
	// reportEpoch identifies the installed report timer; a callback carrying another epoch is stale and does nothing.
	reportEpoch uint64
	// timedOut records that the report timeout was decided. It holds until the supervisor stops: no worker becomes
	// eligible after it and no report timer is armed again, so one run reports at most one timeout.
	timedOut     bool
	stderrWrites sync.Mutex
	tasks        sync.WaitGroup

	// startCmd starts a prepared worker command; nil means (*exec.Cmd).Start. Only the package's tests set it, to hold
	// a launch at a barrier of their own.
	startCmd func(*exec.Cmd) error
}

type workerProc struct {
	cmd        *exec.Cmd
	pid        int
	generation string
	// res is the generation's reservation: its listener, which the worker accepts on, and its drain channel, whose
	// parent end drainConn is.
	res       *workerlink.Reservation
	drainConn net.Conn
	live      *os.File
	report    *os.File
	// ending is set once the supervisor observed the worker's exit and is about to end its generation.
	ending atomic.Bool
	exited chan struct{}
	// liveOnce closes live once, from whichever of the stop, a failed drain acknowledgment or the exit comes first.
	liveOnce sync.Once
	// drain is the generation's drain state; guarded by the shim's mu.
	drain drainState
	// startRecorded is closed once the worker's start is journaled.
	startRecorded chan struct{}
}

// drainState is what the supervisor decided about one generation's drain: whether its notice was validated, whether
// it ended, and whether a public drain's recovery bound expired; once set, none of them is cleared. armed identifies
// the bound in timer: a firing timer acts only while armed still has the value it was given.
type drainState struct {
	noticed, ended, expired bool
	armed                   uint64
	timer                   *time.Timer
}

// disconnectLive closes the supervisor's end of the worker's liveness pipe, which the worker takes as its parent's
// departure.
func (w *workerProc) disconnectLive() { w.liveOnce.Do(func() { _ = w.live.Close() }) }

// registry returns the supervisor's worker registry, creating it on first use.
func (s *shim) registry() *workerlink.Registry {
	s.workersOnce.Do(func() {
		var lc net.ListenConfig
		var dialer net.Dialer
		s.workers = workerlink.NewRegistry(lc.Listen, dialer.DialContext)
	})
	return s.workers
}

// preparation returns the launches' preparation context, creating it on first use.
func (s *shim) preparation() context.Context {
	s.prepOnce.Do(func() { s.prep, s.prepCancel = context.WithCancel(s.lifetime) })
	return s.prep
}

// cancelPreparation ends every launch's preparation.
func (s *shim) cancelPreparation() {
	s.preparation()
	s.prepCancel()
}

// relayWorkers is the lease source the relay asks for the current worker generation.
type relayWorkers struct{ s *shim }

// Acquire passes the relay's request to the registry and reports to the relayLookup hook what the registry selected.
// The registry answers a context that already ended without looking at any generation, and a lookup that finds no
// eligible generation with its bare ErrUnavailable or ErrRetired; every other answer, a lease or the failure of the
// connection to a selected generation, follows a selection. A request that ended first is no lookup and is not
// reported. A lease completed after the supervisor's stop is closed unused: once the stop is recorded, no lease is
// handed out.
func (r relayWorkers) Acquire(ctx context.Context, exclude workerlink.Generation) (workerlink.Lease, error) {
	lease, err := r.s.registry().Acquire(ctx, exclude)
	if hook := hooksOf(r.s.lifetime).relayLookup; hook != nil {
		switch {
		case err != nil && err == ctx.Err(): //nolint:errorlint // The registry returns the context's own error, unwrapped, when it looked at nothing.
		case err == workerlink.ErrUnavailable || err == workerlink.ErrRetired: //nolint:errorlint // The registry returns these sentinels bare exactly when no generation was eligible.
			hook(false)
		default:
			hook(true)
		}
	}
	if err == nil && r.s.isStopped() {
		_ = lease.Close()
		return nil, fmt.Errorf("model-gateway: the supervisor is stopping: %w", workerlink.ErrUnavailable)
	}
	return lease, err
}

// retireForDrain withdraws the admission of the generation lease reached before the relay sends it an authenticated
// drain request, so that the worker has no admission left once it receives the drain. Only the current worker's
// generation is retired; a lease to any other is refused.
func (s *shim) retireForDrain(lease workerlink.Lease) error {
	s.mu.Lock()
	w := s.current
	stopped := s.stopped
	s.mu.Unlock()
	if stopped || w == nil || workerlink.Generation(w.generation) != lease.Generation() {
		return errors.New("model-gateway: the drain reached a worker generation that is not the current one")
	}
	w.res.Retire()
	s.mu.Lock()
	if s.eligible == w {
		s.eligible = nil
	}
	s.mu.Unlock()
	s.armDrainRecovery(w)
	return nil
}

// drainRecoveryBound is how long a worker whose generation a public drain retired has to announce its drain before
// the supervisor stops it. The drain is sent on the lease right after the retirement and the worker announces it on
// receipt, so the bound only has to cover a delivery that failed after the retirement; it is larger than the relay's
// whole attempt window (81 attempts 50 ms apart) plus the worker's drain exchange budget of five seconds, so that a
// worker whose announcement waits behind either is not stopped.
const drainRecoveryBound = 10 * time.Second

// armDrainRecovery bounds how long w may stay retired by a public drain without announcing it: should the drain never
// reach it, admission stays withdrawn, the drain is not sent again, and when the bound passes only that worker is
// stopped, as the immediate stop does; its exit then ends its generation.
//
// The generation's drain state is decided under mu and never undone: once its notice was validated or it ended, no
// bound is armed; a bound acts only if it is still the one armed and the notice has not been validated, and it claims
// the expiry in the same critical section, so that the notice and the expiry are ordered there. The process stopped
// is the one captured in w, never a replacement.
func (s *shim) armDrainRecovery(w *workerProc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// No bound for a worker that is no longer the current one, whose exit was observed, whose notice was validated,
	// or once the supervisor stops, which also stops an armed bound: no timer outlives Run.
	if s.stopped || s.current != w || w.ending.Load() || w.drain.noticed || w.drain.ended || w.drain.expired || w.drain.timer != nil {
		return
	}
	w.drain.armed++
	armed := w.drain.armed
	w.drain.timer = time.AfterFunc(drainRecoveryBound, func() {
		s.mu.Lock()
		if s.stopped || w.drain.armed != armed || w.drain.noticed || w.drain.ended {
			s.mu.Unlock()
			return
		}
		w.drain.expired = true
		w.drain.timer = nil
		s.tasks.Add(1)
		s.mu.Unlock()
		defer s.tasks.Done()
		// The worker's stop is requested first (its liveness ended, then signals, each with a bounded wait for its exit);
		// the line follows, so that a stderr that blocks cannot keep it running.
		s.disconnect(w)
		s.logf("model-gateway: the shim worker did not announce the public drain within %v; requested its stop\n", drainRecoveryBound)
	})
}

// endDrainRecovery records a transition of w's drain state under mu and disarms any armed bound: the validation of the
// worker's notice (noticed) or the end of its generation. It reports false when the expiry was claimed first.
func (s *shim) endDrainRecovery(w *workerProc, noticed bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if noticed {
		if w.drain.expired {
			return false
		}
		w.drain.noticed = true
	} else {
		w.drain.ended = true
	}
	w.drain.armed++
	if w.drain.timer != nil {
		w.drain.timer.Stop()
		w.drain.timer = nil
	}
	return true
}

func (s *shim) logf(format string, args ...any) {
	s.stderrWrites.Lock()
	defer s.stderrWrites.Unlock()
	_, _ = fmt.Fprintf(s.stderr, format, args...)
}

// fail hands a fatal error to Run, which holds one. A failure that arrives while another one is pending is not the
// one Run returns, so it is printed here as a warning instead of being lost. It is called from a task that Run awaits,
// without mu held; a caller holding mu uses failLocked.
func (s *shim) fail(err error) {
	select {
	case s.fatal <- err:
	default:
		s.warnFatal(err)
	}
}

// failLocked hands a fatal error to Run while mu is held and the supervisor is not stopped, so the hand-over is decided
// before Run looks at the inbox for the last time. Nothing is written while mu is held: when another failure is
// pending, the returned function prints err as a warning and the caller runs it after releasing mu. Run's final wait
// covers that write.
func (s *shim) failLocked(err error) (warn func()) {
	select {
	case s.fatal <- err:
		return func() {}
	default:
	}
	s.tasks.Add(1)
	return func() {
		defer s.tasks.Done()
		s.warnFatal(err)
	}
}

// warnFatal prints a fatal error that Run does not return as one line with one "model-gateway: " prefix. The error
// Run returns is printed by its caller only.
func (s *shim) warnFatal(err error) {
	const prefix = "model-gateway: "
	line := strings.ReplaceAll(strings.TrimRight(causeText(err, prefix), "\n"), "\n", "; ")
	for strings.HasPrefix(line, prefix) {
		line = line[len(prefix):]
	}
	s.logf("%s%s\n", prefix, line)
}

// causeText returns err's message with prefix dropped from the start of each wrapped cause's own message, which an
// error that wraps or joins causes would otherwise repeat. A cause is recognised only where its wrapper puts it: at the
// end of a wrapping message, or as one whole line of a joined one. Any other text, including data such as a path that
// contains prefix, is kept as it is.
func causeText(err error, prefix string) string {
	msg := err.Error()
	own := func(cause error) string {
		text := causeText(cause, prefix)
		for strings.HasPrefix(text, prefix) {
			text = text[len(prefix):]
		}
		return text
	}
	switch e := err.(type) { //nolint:errorlint // Each level of the chain is visited in turn; only err's own causes are matched here.
	case interface{ Unwrap() error }:
		if cause := e.Unwrap(); cause != nil {
			if head, ok := strings.CutSuffix(msg, cause.Error()); ok {
				return head + own(cause)
			}
		}
	case interface{ Unwrap() []error }:
		causes := e.Unwrap()
		lines := make([]string, len(causes))
		for i, cause := range causes {
			if cause == nil {
				return msg
			}
			lines[i] = cause.Error()
		}
		if msg == strings.Join(lines, "\n") {
			for i, cause := range causes {
				lines[i] = own(cause)
			}
			return strings.Join(lines, "\n")
		}
	}
	return msg
}

// publishSelf writes the supervisor's own pid record before it reports ready.
func (s *shim) publishSelf(ctx context.Context, publicPort int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	generation := ""
	if g := s.env.startupGeneration(); g != nil {
		generation = *g
	}
	if generation == "" {
		generation = newGeneration()
	}
	image, birth, err := s.evidence(ctx, s.pid, exe)
	if err != nil {
		return err
	}
	return s.writeRecord("guardian", "supervisor", s.pid, strings.Join(os.Args, " "), generation, publicPort, image, birth)
}

// evidence identifies a process for its pid record: the executable's image and the kernel's birth time of pid.
func (s *shim) evidence(ctx context.Context, pid int, executable string) (procinfo.Image, time.Time, error) {
	image, err := imageOf(executable)
	if err != nil {
		return procinfo.Image{}, time.Time{}, err
	}
	probe := procinfo.New(s.env.lookup)
	birthCtx, cancel := context.WithTimeout(ctx, birthWait)
	defer cancel()
	for {
		birth, err := probe.Birth(birthCtx, pid)
		if err == nil {
			return image, birth, nil
		}
		select {
		case <-birthCtx.Done():
			return procinfo.Image{}, time.Time{}, err
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *shim) writeRecord(name, role string, pid int, command, generation string, port int, image procinfo.Image, birth time.Time) error {
	return procinfo.WriteRecord(s.state, name, procinfo.Record{
		PID: pid, Command: command, StartedAt: procinfo.LegacyStart(birth), Role: role, StateRoot: s.state,
		Generation: generation, Port: uint16(port), Version: s.version, Image: image,
	})
}

// errStartInhibited is the result of a launch that started no process because the supervisor is stopping or the
// state root carries the stop marker; it is no failure.
var errStartInhibited = errors.New("model-gateway: worker start inhibited")

// startWorker launches one worker unless the supervisor is stopping, a worker is still running or another launch is
// under way (lib/commands.js:2593-2641). The launch runs outside mu, so that a stop, a report deadline or another
// start request is decided while it runs; a process whose start was under way when the stop came is disconnected and
// awaited.
func (s *shim) startWorker() {
	started := false
	if hook := hooksOf(s.lifetime).launchEnded; hook != nil {
		defer func() { hook(started) }()
	}
	s.mu.Lock()
	if s.stopped || s.current != nil || s.launching {
		s.mu.Unlock()
		return
	}
	s.launching = true
	if s.reportTimer == nil && !s.timedOut {
		// An installed timer is kept across failed launches, so the deadline counts from the first of them.
		s.reportEpoch++
		epoch := s.reportEpoch
		s.reportTimer = time.AfterFunc(reportTimeout, func() { s.reportExpired(epoch) })
	}
	s.mu.Unlock()
	// A launch that panics clears its mark on the way out; the spawn has released what it held.
	launched := false
	defer func() {
		if !launched {
			s.mu.Lock()
			s.launching = false
			s.mu.Unlock()
		}
	}()

	w, err := s.spawn()

	s.mu.Lock()
	launched = true
	s.launching = false
	switch {
	case errors.Is(err, errStartInhibited):
		s.mu.Unlock()
		return
	case err != nil && s.stopped:
		// A launch that failed after the stop has nothing left to report to.
		s.mu.Unlock()
		return
	case err != nil:
		warn := s.failLocked(fmt.Errorf("model-gateway: could not start the shim worker: %w", err))
		s.mu.Unlock()
		warn()
		return
	}
	started = true
	// The journal line is written after the lock is released; the exit watcher waits for it, so that a worker's
	// exit is never journaled before its start.
	recordStart := func() {
		defer close(w.startRecorded)
		s.files.Record("worker-started", map[string]any{"component": "supervisor", "pid": s.pid, "startedAt": s.startedAt, "child": map[string]any{"component": "worker", "pid": w.pid}})
	}
	if s.stopped {
		// The start was under way when the stop came: the process exists, so it is disconnected and awaited.
		s.tasks.Go(func() { s.awaitExit(w) })
		s.tasks.Go(func() { s.disconnect(w) })
		s.mu.Unlock()
		recordStart()
		// Liveness first, so that the worker sees its parent's end before its report could fail.
		w.disconnectLive()
		_ = w.report.Close()
		return
	}
	s.current = w
	s.tasks.Go(func() { s.readReport(w) })
	s.tasks.Go(func() { s.readDrain(w) })
	s.tasks.Go(func() { s.awaitExit(w) })
	s.mu.Unlock()
	recordStart()
}

func (s *shim) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// spawn reserves a generation and starts its worker with the report pipe, the liveness pipe, the generation's
// listener and the child's end of its drain channel. The process is started under the state root's start fence,
// after a last check that the supervisor is not stopping and the stop marker is absent. Whatever the start was to
// hand over is closed in the supervisor once the start returned; on any failure the reservation is ended.
func (s *shim) spawn() (*workerProc, error) {
	if hook := hooksOf(s.lifetime).launching; hook != nil {
		hook()
	}
	if s.isStopped() {
		return nil, errStartInhibited
	}
	log, err := openLog(filepath.Join(s.state, "logs", "shim.log"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = log.Close() }()
	generation := newGeneration()
	res, err := s.registry().Reserve(s.preparation(), workerlink.Generation(generation), uint16(s.configuredPort))
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = res.End()
		}
	}()
	listener, err := res.File()
	if err != nil {
		return nil, err
	}
	parent, child, err := res.DrainChannel()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	transfer := []*os.File{nil, nil, listener, child}
	// Whatever is not handed over is released on every way out, a panic of the Starting hook or of the start
	// included: the transfer files always, the supervisor's ends of the pipes unless the worker was started.
	closeTransfer := sync.OnceFunc(func() {
		for _, f := range transfer {
			if f != nil {
				_ = f.Close()
			}
		}
	})
	defer closeTransfer()
	reportR, reportW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if !ok {
			_ = reportR.Close()
		}
	}()
	transfer[0] = reportW
	liveR, liveW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if !ok {
			_ = liveW.Close()
		}
	}()
	transfer[1] = liveR
	// The worker's lifetime is ended by the ordered stop, never by cancelling the command's context.
	cmd := exec.CommandContext(s.lifetime, s.executable, "serve-worker")
	cmd.Env = s.env.workerEnvironment(int(res.Port()), generation)
	cmd.Stdout, cmd.Stderr = log, log
	// The child's ends become descriptors 3 to 6: the report writer, the liveness reader, the listener and the drain
	// channel. The supervisor holds the only write end of the liveness pipe.
	cmd.ExtraFiles = transfer
	err = s.startUnderFence(cmd)
	closeTransfer()
	if err != nil {
		return nil, err
	}
	ok = true
	return &workerProc{cmd: cmd, pid: cmd.Process.Pid, generation: generation, res: res, drainConn: parent, live: liveW, report: reportR, exited: make(chan struct{}), startRecorded: make(chan struct{})}, nil
}

// startUnderFence starts cmd while it holds the state root's start fence, unless the supervisor is stopping or the
// stop marker is present by then. The stop marker is published under the same fence, so a deliberate stop is ordered
// against every start.
func (s *shim) startUnderFence(cmd *exec.Cmd) error {
	fence, err := lifecycle.NewStartFence(s.state)
	if err != nil {
		return err
	}
	release, err := fence.Acquire(s.preparation())
	if err != nil {
		return fmt.Errorf("could not take the worker start fence: %w", err)
	}
	defer func() { _ = release() }()
	if s.isStopped() {
		return errStartInhibited
	}
	switch marked, err := s.files.StopMarkerPresent(); {
	case err != nil:
		return fmt.Errorf("could not check the stop marker: %w", err)
	case marked:
		return errStartInhibited
	}
	start := s.startCmd
	if start == nil {
		start = (*exec.Cmd).Start
	}
	if hook := hooksOf(s.lifetime).starting; hook != nil {
		hook()
	}
	return start(cmd)
}

// readReport accepts one listening report from the worker's own generation, publishes the worker's pid record, and
// only then makes the worker's generation eligible for relay. Until then the record carries port 0. The worker must
// report the port reserved for its generation, the configured one or the one the system chose; a current worker that
// reports another one stops the supervisor at once.
func (s *shim) readReport(w *workerProc) {
	defer func() { _ = w.report.Close() }()
	if err := s.publish(w, 0, false); err != nil {
		s.logf("model-gateway: could not publish the worker pid record: %v\n", err)
	}
	_ = w.report.SetReadDeadline(time.Now().Add(reportTimeout))
	data, err := io.ReadAll(io.LimitReader(w.report, maxReport+1))
	if err != nil {
		return
	}
	port, ok := parseReport(data, w.generation)
	if !ok {
		return
	}
	if reserved := int(w.res.Port()); port != reserved {
		if s.isCurrent(w) {
			if s.configuredPort != 0 {
				s.fail(fmt.Errorf("model-gateway: the shim worker reported port %d instead of CODEX_GATEWAY_WORKER_PORT %d", port, s.configuredPort))
			} else {
				s.fail(fmt.Errorf("model-gateway: the shim worker reported port %d instead of its reserved port %d", port, reserved))
			}
		}
		return
	}
	if err := s.publish(w, port, true); err != nil {
		if !s.isCurrent(w) {
			s.logf("model-gateway: could not publish the worker pid record: %v\n", err)
			return
		}
		// A worker the supervisor cannot track is not served, as the supervisor does not serve untracked itself.
		s.fail(fmt.Errorf("model-gateway: the shim worker could not be tracked: %w", err))
	}
}

func (s *shim) isCurrent(w *workerProc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.stopped && s.current == w
}

// publish writes w's pid record with port while w is still the current worker and, when eligible is set, then
// publishes w's generation for relay. The identity evidence is gathered first, outside every lock. A generation that
// was retired by a drain before this point stays out of relay: its late publication is refused and is no failure.
func (s *shim) publish(w *workerProc, port int, eligible bool) error {
	image, birth, err := s.evidence(s.lifetime, w.pid, s.executable)
	if err != nil {
		if !s.isCurrent(w) {
			return nil
		}
		return err
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if !s.isCurrent(w) {
		return nil
	}
	if err := s.writeRecord("shim", "worker", w.pid, s.executable+" serve-worker", w.generation, port, image, birth); err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	if hook := hooksOf(s.lifetime).published; hook != nil {
		hook(w.pid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.current != w || s.timedOut {
		return nil
	}
	if err := w.res.Publish(w.pid); err != nil {
		if errors.Is(err, workerlink.ErrRetired) || errors.Is(err, workerlink.ErrUnavailable) {
			return nil
		}
		return err
	}
	s.eligible = w
	if s.reportTimer != nil {
		s.reportTimer.Stop()
		s.reportTimer = nil
		s.reportEpoch++
	}
	return nil
}

// parseReport accepts exactly one newline-terminated line of at most 256 bytes naming this generation and a valid port.
func parseReport(data []byte, generation string) (int, bool) {
	if len(data) > maxReport || bytes.Count(data, []byte("\n")) != 1 || !bytes.HasSuffix(data, []byte("\n")) {
		return 0, false
	}
	var report struct {
		Type       string  `json:"type"`
		Port       float64 `json:"port"`
		Generation string  `json:"generation"`
	}
	if json.Unmarshal(data, &report) != nil || report.Type != "listening" || report.Generation != generation {
		return 0, false
	}
	if report.Port < 1 || report.Port > 65535 || report.Port != float64(int(report.Port)) {
		return 0, false
	}
	return int(report.Port), true
}

// reportExpired stops the supervisor when no worker reported its listener in time. A callback whose timer was stopped
// or replaced after it had already started finds another epoch and does nothing. The failure is decided and handed
// over under mu, which stop also takes: a stop that comes first leaves the callback another epoch, and a stop that
// comes after finds the failure already pending, so Run reports it or prints it as a warning. The decision is final
// until the stop: from then on no worker becomes eligible and no report timer is armed.
func (s *shim) reportExpired(epoch uint64) {
	s.mu.Lock()
	if epoch != s.reportEpoch || s.reportTimer == nil {
		s.mu.Unlock()
		return
	}
	s.reportTimer = nil
	warn := func() {}
	if !s.stopped && s.eligible == nil {
		s.timedOut = true
		warn = s.failLocked(fmt.Errorf("model-gateway: shim worker did not report its listener port within %dms", reportTimeout.Milliseconds()))
	}
	s.mu.Unlock()
	warn()
}

// awaitExit ends an exited worker's generation, journals its exit, tells its waiters and schedules its replacement.
// Until the supervisor acts on the exit it keeps the generation's listener, so no other program can take its port
// meanwhile. Then the worker stops being current and its generation is ended, which withdraws its admission, ends the
// relay's waits on it and releases its listener and drain channel, before the exit is journaled.
func (s *shim) awaitExit(w *workerProc) {
	err := w.cmd.Wait()
	holdExitObservation(s.lifetime, w.pid)
	w.ending.Store(true)
	s.endDrainRecovery(w, false)
	s.mu.Lock()
	if s.current == w {
		s.current = nil
	}
	if s.eligible == w {
		s.eligible = nil
	}
	s.mu.Unlock()
	if endErr := w.res.End(); endErr != nil {
		s.logf("model-gateway: could not release the shim worker's listener: %v\n", endErr)
	}
	fields := map[string]any{"component": "supervisor", "pid": s.pid, "startedAt": s.startedAt, "child": map[string]any{"component": "worker", "pid": w.pid}}
	fields["exitCode"], fields["signal"] = exitStatus(w.cmd.ProcessState, err)
	if w.startRecorded != nil {
		<-w.startRecorded
	}
	s.files.Record("worker-exit", fields)
	w.disconnectLive()
	close(w.exited)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.respawn = time.AfterFunc(respawnDelay, s.respawnWorker)
	}
}

// respawnWorker starts the replacement as one of the tasks Run awaits, unless the supervisor stopped meanwhile.
func (s *shim) respawnWorker() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.tasks.Add(1)
	s.mu.Unlock()
	defer s.tasks.Done()
	s.startWorker()
}

// readDrain reads the worker's drain channel. On the worker's drain notice for its own generation it retires the
// generation, so that no new lease is given for it, and answers once the leases already being dialled have settled.
// A worker whose notice cannot be answered is stopped. Any other well-formed frame is ignored; the reading ends with
// the channel, which ending the generation closes, or at a frame that cannot be read.
func (s *shim) readDrain(w *workerProc) {
	frames := bufio.NewReader(w.drainConn)
	for {
		msg, err := workerlink.DecodeDrain(frames)
		if err != nil {
			// The channel ended or carried a frame that cannot be read while the generation lives: no answer can
			// follow, so its admission is withdrawn; the worker itself is left running until it exits.
			if !w.ending.Load() {
				w.res.Retire()
				s.mu.Lock()
				if s.eligible == w {
					s.eligible = nil
				}
				s.mu.Unlock()
			}
			return
		}
		if msg != (workerlink.DrainMessage{Type: workerlink.Draining, Generation: workerlink.Generation(w.generation)}) {
			continue
		}
		w.res.Retire()
		s.mu.Lock()
		if s.eligible == w {
			s.eligible = nil
		}
		s.mu.Unlock()
		if !s.endDrainRecovery(w, true) {
			// A public drain's bound expired first and the worker is being stopped: the notice is not answered.
			return
		}
		if hook := hooksOf(s.lifetime).acknowledging; hook != nil {
			hook()
		}
		if err := w.res.AcknowledgeDrain(s.lifetime); err != nil {
			if w.ending.Load() {
				return
			}
			// The worker's stop is requested first (its liveness ended, then signals, each with a bounded wait for its
			// exit); the line follows, so that a stderr that blocks cannot keep it running.
			s.disconnect(w)
			s.logf("model-gateway: could not answer the shim worker's drain notice; requested its stop: %v\n", err)
			return
		}
	}
}

// disconnect stops one worker: it closes the worker's liveness pipe and waits for its exit, signalling it only if it
// does not exit (lib/commands.js:2537-2575).
func (s *shim) disconnect(w *workerProc) {
	w.disconnectLive()
	for _, sig := range []os.Signal{nil, syscall.SIGTERM, syscall.SIGKILL} {
		if sig != nil {
			_ = w.cmd.Process.Signal(sig)
		}
		timer := time.NewTimer(exitWait)
		select {
		case <-w.exited:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// testHooks are the points at which the package's tests hold or observe the supervisor. Only the package's tests put
// them into Run's context; without them every point does nothing.
type testHooks struct {
	// launching runs at the start of every attempt to launch a worker, before anything is reserved or opened for it.
	launching func()
	// starting runs immediately before the worker's process is started, inside whatever serializes worker starts.
	starting func()
	// published runs once the eligible pid record of a worker has been written and before the worker becomes
	// eligible for relay.
	published func(pid int)
	// exitObserved runs once the system has reported a worker's exit and before the supervisor acts on it.
	exitObserved func(pid int)
	// stopInhibited runs once a stop has recorded that no further worker may start, before the stop disconnects or
	// waits for anything.
	stopInhibited func()
	// relayLookup runs each time the relay actually asks the supervisor for a worker to deliver a request to, with
	// whether one was eligible. It reports lookups that happened; the relay is not required to make any, for instance
	// while a worker that took the request is current.
	relayLookup func(eligible bool)
	// launchEnded runs once each decision whether to start a worker, the first one or a replacement, has been carried
	// out, with whether a worker process was started; it runs without the supervisor's lock held.
	launchEnded func(started bool)
	// acknowledging runs just before the supervisor writes its answer to a worker's drain notice; the answer is written
	// only once it returns.
	acknowledging func()
}

// hooksKey is the private context key of a testHooks value.
type hooksKey struct{}

func hooksOf(ctx context.Context) testHooks {
	hooks, _ := ctx.Value(hooksKey{}).(testHooks)
	return hooks
}

// holdExitObservation runs the exitObserved hook, if any, with the pid of a worker whose exit the system has just
// reported, before the supervisor acts on that exit.
func holdExitObservation(ctx context.Context, pid int) {
	if hook := hooksOf(ctx).exitObserved; hook != nil {
		hook(pid)
	}
}

// exitStatus mirrors Node's exit event: an exit code, or null and the signal name.
func exitStatus(state *os.ProcessState, err error) (any, any) {
	if state == nil {
		return nil, nil
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return nil, signalName(status.Signal())
	}
	if err != nil && state.ExitCode() < 0 {
		return nil, nil
	}
	return state.ExitCode(), nil
}

// stop records the request, withdraws the worker's admission, disconnects the worker and waits for it to exit,
// signalling it only if it does not (lib/commands.js:2537-2575). A launch under way is not waited for: it starts no
// process once the stop is recorded, and a process whose start was already under way is disconnected by the launch.
func (s *shim) stop(signal string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	w := s.current
	s.eligible = nil
	// The inhibition withdraws the stopping worker's admission at once, before anything is journaled: from here on
	// no new lease is given for it.
	if w != nil && w.res != nil {
		w.res.Retire()
	}
	for _, t := range []*time.Timer{s.respawn, s.reportTimer} {
		if t != nil {
			t.Stop()
		}
	}
	s.reportTimer = nil
	s.reportEpoch++
	if w != nil {
		w.drain.armed++
		if w.drain.timer != nil {
			w.drain.timer.Stop()
			w.drain.timer = nil
		}
	}
	s.mu.Unlock()
	// A launch that is still preparing or waits for the start fence ends now.
	s.cancelPreparation()
	if hook := hooksOf(s.lifetime).stopInhibited; hook != nil {
		hook()
	}
	// The stop is journaled after the current worker's start, whose line its launch writes after releasing the lock.
	if w != nil && w.startRecorded != nil {
		<-w.startRecorded
	}
	if signal != "" {
		s.files.Record("supervisor-stop-requested", map[string]any{"component": "supervisor", "pid": s.pid, "startedAt": s.startedAt, "signal": signal})
		if w != nil {
			s.files.Record("worker-stop-requested", map[string]any{"component": "supervisor", "pid": s.pid, "child": map[string]any{"component": "worker", "pid": w.pid}, "signal": signal})
		}
	}
	if w == nil {
		return
	}
	s.disconnect(w)
}

// wait returns once every report reader and exit watcher has finished.
func (s *shim) wait() {
	s.mu.Lock()
	w := s.current
	s.mu.Unlock()
	if w != nil {
		_ = w.report.Close()
	}
	s.tasks.Wait()
}

func (e environment) startupGeneration() *string {
	if v, ok := e.lookup("MODEL_GATEWAY_STARTUP_GENERATION"); ok {
		return &v
	}
	return nil
}

// fixtureWatch delivers the fixture channel's verdict: true for the shutdown message, false when the channel ended.
type fixtureWatch struct {
	result chan bool
	file   io.Closer
	done   chan struct{}
}

func (f fixtureWatch) close() {
	if f.file != nil {
		_ = f.file.Close()
	}
	if f.done != nil {
		<-f.done
	}
}

// watchFixtureChannel reads the test harness's channel under the test guard; outside it the descriptor is closed
// unread. The channel is never passed to the worker.
func (s *shim) watchFixtureChannel() (fixtureWatch, error) {
	file, err := openFixtureChannel(s.env)
	if err != nil || file == nil {
		return fixtureWatch{}, err
	}
	watch := fixtureWatch{result: make(chan bool, 1), file: file, done: make(chan struct{})}
	guarded := s.env.guarded()
	go func() {
		defer close(watch.done)
		stop, _ := supervisor.FixtureShutdown(file, guarded)
		watch.result <- stop
	}()
	return watch, nil
}
