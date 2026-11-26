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
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"io"
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

	// endpoint is the port of the worker that may receive requests, or 0. It is set only after the current worker
	// reported its listener for its own generation and its pid record was published, and it is cleared under mu
	// whenever that worker stops being current. A port number alone, configured or not, never makes a worker eligible.
	endpoint atomic.Int64

	// publishMu serializes the final current-worker check with the pid record write, so that a slow publication of an
	// earlier worker can never land after its replacement's.
	publishMu sync.Mutex

	mu          sync.Mutex
	stopped     bool
	current     *workerProc
	respawn     *time.Timer
	reportTimer *time.Timer
	// reportEpoch identifies the installed report timer; a callback carrying another epoch is stale and does nothing.
	reportEpoch  uint64
	stderrWrites sync.Mutex
	tasks        sync.WaitGroup
}

type workerProc struct {
	cmd        *exec.Cmd
	pid        int
	generation string
	live       *os.File
	report     *os.File
	exited     chan struct{}
}

func (s *shim) port() int { return int(s.endpoint.Load()) }

func (s *shim) logf(format string, args ...any) {
	s.stderrWrites.Lock()
	defer s.stderrWrites.Unlock()
	_, _ = fmt.Fprintf(s.stderr, format, args...)
}

// fail hands a fatal error to Run, which holds one. A failure that arrives while another one is pending is not the
// one Run returns, so it is printed here as a warning instead of being lost. Every caller hands its failure over
// before Run looks at the inbox for the last time: under mu while the supervisor is not stopped, or from a task that
// Run awaits.
func (s *shim) fail(err error) {
	select {
	case s.fatal <- err:
	default:
		s.warnFatal(err)
	}
}

// warnFatal prints a fatal error that Run does not return as one line with one "model-gateway: " prefix. The error
// Run returns is printed by its caller only.
func (s *shim) warnFatal(err error) {
	const prefix = "model-gateway: "
	line := strings.ReplaceAll(strings.TrimRight(err.Error(), "\n"), "\n", "; ")
	// An error that wraps or joins causes repeats the prefix of every cause that carries one; the line keeps one.
	line = strings.ReplaceAll(strings.ReplaceAll(line, ": "+prefix, ": "), "; "+prefix, "; ")
	for strings.HasPrefix(line, prefix) {
		line = line[len(prefix):]
	}
	s.logf("%s%s\n", prefix, line)
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

// startWorker launches one worker unless the supervisor is stopping or a worker is still running
// (lib/commands.js:2593-2641).
func (s *shim) startWorker() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.current != nil {
		return
	}
	s.endpoint.Store(0)
	if s.reportTimer == nil {
		// An installed timer is kept across failed launches, so the deadline counts from the first of them.
		s.reportEpoch++
		epoch := s.reportEpoch
		s.reportTimer = time.AfterFunc(reportTimeout, func() { s.reportExpired(epoch) })
	}
	w, err := s.spawn()
	if err != nil {
		s.fail(fmt.Errorf("model-gateway: could not start the shim worker: %w", err))
		return
	}
	s.current = w
	s.files.Record("worker-started", map[string]any{"component": "supervisor", "pid": s.pid, "startedAt": s.startedAt, "child": map[string]any{"component": "worker", "pid": w.pid}})
	s.tasks.Go(func() { s.readReport(w) })
	s.tasks.Go(func() { s.awaitExit(w) })
}

func (s *shim) spawn() (*workerProc, error) {
	log, err := os.OpenFile(filepath.Join(s.state, "logs", "shim.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return nil, err
	}
	defer func() { _ = log.Close() }()
	reportR, reportW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	liveR, liveW, err := os.Pipe()
	if err != nil {
		_, _ = reportR.Close(), reportW.Close()
		return nil, err
	}
	generation := newGeneration()
	// The worker's lifetime is ended by the ordered stop, never by cancelling the command's context.
	cmd := exec.CommandContext(s.lifetime, s.executable, "serve-worker")
	cmd.Env = s.env.workerEnvironment(s.configuredPort, generation)
	cmd.Stdout, cmd.Stderr = log, log
	// The child's ends become descriptors 3 and 4; the supervisor holds the only write end of the liveness pipe.
	cmd.ExtraFiles = []*os.File{reportW, liveR}
	err = cmd.Start()
	_, _ = reportW.Close(), liveR.Close()
	if err != nil {
		_, _ = reportR.Close(), liveW.Close()
		return nil, err
	}
	return &workerProc{cmd: cmd, pid: cmd.Process.Pid, generation: generation, live: liveW, report: reportR, exited: make(chan struct{})}, nil
}

// readReport accepts one listening report from the worker's own generation, publishes the worker's pid record, and
// only then makes the worker's port eligible for relay. Until then the record carries port 0. A configured port must
// be the port the worker reports; a current worker that reports another one stops the supervisor at once.
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
	if s.configuredPort != 0 && port != s.configuredPort {
		if s.isCurrent(w) {
			s.fail(fmt.Errorf("model-gateway: the shim worker reported port %d instead of CODEX_GATEWAY_WORKER_PORT %d", port, s.configuredPort))
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

// publish writes w's pid record with port while w is still the current worker and, when eligible is set, then makes
// port the relay endpoint. The identity evidence is gathered first, outside every lock.
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.current != w {
		return nil
	}
	s.endpoint.Store(int64(port))
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
// comes after finds the failure already pending, so Run reports it or prints it as a warning.
func (s *shim) reportExpired(epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.reportEpoch || s.reportTimer == nil {
		return
	}
	s.reportTimer = nil
	if !s.stopped && s.endpoint.Load() == 0 {
		s.fail(fmt.Errorf("model-gateway: shim worker did not report its listener port within %dms", reportTimeout.Milliseconds()))
	}
}

func (s *shim) awaitExit(w *workerProc) {
	err := w.cmd.Wait()
	fields := map[string]any{"component": "supervisor", "pid": s.pid, "startedAt": s.startedAt, "child": map[string]any{"component": "worker", "pid": w.pid}}
	fields["exitCode"], fields["signal"] = exitStatus(w.cmd.ProcessState, err)
	s.files.Record("worker-exit", fields)
	_ = w.live.Close()
	close(w.exited)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == w {
		s.current = nil
		s.endpoint.Store(0)
	}
	if !s.stopped {
		s.respawn = time.AfterFunc(respawnDelay, s.startWorker)
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

// stop records the request, disconnects the worker and waits for it to exit, signalling it only if it does not
// (lib/commands.js:2537-2575).
func (s *shim) stop(signal string) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.endpoint.Store(0)
	w := s.current
	for _, t := range []*time.Timer{s.respawn, s.reportTimer} {
		if t != nil {
			t.Stop()
		}
	}
	s.reportTimer = nil
	s.reportEpoch++
	s.mu.Unlock()
	if signal != "" {
		s.files.Record("supervisor-stop-requested", map[string]any{"component": "supervisor", "pid": s.pid, "startedAt": s.startedAt, "signal": signal})
		if w != nil {
			s.files.Record("worker-stop-requested", map[string]any{"component": "supervisor", "pid": s.pid, "child": map[string]any{"component": "worker", "pid": w.pid}, "signal": signal})
		}
	}
	if w == nil {
		return
	}
	_ = w.live.Close()
	for _, sig := range []os.Signal{nil, syscall.SIGTERM, syscall.SIGKILL} {
		if sig != nil {
			_ = w.cmd.Process.Signal(sig)
		}
		select {
		case <-w.exited:
			return
		case <-time.After(exitWait):
		}
	}
}

// wait returns once every report reader and exit watcher has finished.
func (s *shim) wait() {
	s.mu.Lock()
	if w := s.current; w != nil {
		_ = w.report.Close()
	}
	s.mu.Unlock()
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
