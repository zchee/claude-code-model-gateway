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
	// reportTimeout is how long a worker without a configured port has to report its listener (lib/commands.js:2488).
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

	workerPort atomic.Int64

	mu           sync.Mutex
	stopped      bool
	current      *workerProc
	respawn      *time.Timer
	reportTimer  *time.Timer
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

func (s *shim) port() int { return int(s.workerPort.Load()) }

func (s *shim) logf(format string, args ...any) {
	s.stderrWrites.Lock()
	defer s.stderrWrites.Unlock()
	_, _ = fmt.Fprintf(s.stderr, format, args...)
}

func (s *shim) fail(err error) {
	select {
	case s.fatal <- err:
	default:
	}
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
	return s.writeRecord(ctx, "guardian", "supervisor", s.pid, strings.Join(os.Args, " "), exe, generation, publicPort)
}

func (s *shim) writeRecord(ctx context.Context, name, role string, pid int, command, executable, generation string, port int) error {
	image, err := imageOf(executable)
	if err != nil {
		return err
	}
	probe := procinfo.New(s.env.lookup)
	birthCtx, cancel := context.WithTimeout(ctx, birthWait)
	defer cancel()
	var birth time.Time
	for {
		if birth, err = probe.Birth(birthCtx, pid); err == nil {
			break
		}
		select {
		case <-birthCtx.Done():
			return err
		case <-time.After(10 * time.Millisecond):
		}
	}
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
	if s.configuredPort == 0 {
		s.workerPort.Store(0)
		if s.reportTimer == nil {
			s.reportTimer = time.AfterFunc(reportTimeout, s.reportExpired)
		}
	} else {
		s.workerPort.Store(int64(s.configuredPort))
	}
	w, err := s.spawn()
	if err != nil {
		s.logf("model-gateway: could not start the shim worker: %v\n", err)
		s.fail(err)
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

// readReport accepts one listening report from the worker's generation and publishes the worker's pid record.
func (s *shim) readReport(w *workerProc) {
	defer func() { _ = w.report.Close() }()
	command := s.executable + " serve-worker"
	if err := s.writeRecord(context.Background(), "shim", "worker", w.pid, command, s.executable, w.generation, s.configuredPort); err != nil {
		s.logf("model-gateway: could not publish the worker pid record: %v\n", err)
	}
	_ = w.report.SetReadDeadline(time.Now().Add(reportTimeout))
	data, err := io.ReadAll(io.LimitReader(w.report, maxReport+1))
	if err != nil {
		return
	}
	port, ok := parseReport(data, w.generation)
	if !ok || s.configuredPort != 0 {
		return
	}
	s.mu.Lock()
	if s.stopped || s.current != w {
		s.mu.Unlock()
		return
	}
	s.workerPort.Store(int64(port))
	if s.reportTimer != nil {
		s.reportTimer.Stop()
		s.reportTimer = nil
	}
	s.mu.Unlock()
	if err := s.writeRecord(context.Background(), "shim", "worker", w.pid, command, s.executable, w.generation, port); err != nil {
		s.logf("model-gateway: could not publish the worker pid record: %v\n", err)
	}
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

func (s *shim) reportExpired() {
	s.mu.Lock()
	expired := !s.stopped && s.configuredPort == 0 && s.workerPort.Load() == 0
	s.reportTimer = nil
	s.mu.Unlock()
	if expired {
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
	w := s.current
	for _, t := range []*time.Timer{s.respawn, s.reportTimer} {
		if t != nil {
			t.Stop()
		}
	}
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
func (s *shim) watchFixtureChannel() fixtureWatch {
	file := openFixtureChannel(s.env)
	if file == nil {
		return fixtureWatch{}
	}
	watch := fixtureWatch{result: make(chan bool, 1), file: file, done: make(chan struct{})}
	guarded := s.env.guarded()
	go func() {
		defer close(watch.done)
		stop, _ := supervisor.FixtureShutdown(file, guarded)
		watch.result <- stop
	}()
	return watch
}
