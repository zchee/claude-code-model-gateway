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

// Package serveshim runs the foreground supervisor command.
package serveshim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

// Options supplies the command's environment, home lookup, child executable, version and output streams.
type Options struct {
	Environment []string
	Home        func() (string, error)
	Executable  string
	Version     string
	Stdout      io.Writer
	Stderr      io.Writer
}

// Run serves until cancellation, an orderly stop, or a startup failure.
//
// It publishes the control token and its own pid record, binds the public listener, prints exactly one readiness line
// on Stdout and starts the worker with the report and liveness pipes. SIGTERM, SIGINT and, under the test guard,
// the fixture channel's shutdown message stop it in order: the stop is recorded, the worker is disconnected and
// awaited, then the listener closes (lib/commands.js:2547-2575,2895-2905). Run returns nil after an orderly stop and an
// error when the supervisor stops with exit status 1. It prints only non-fatal diagnostics on Stderr; the caller
// reports the returned error.
func Run(ctx context.Context, opts Options) error {
	env := newEnvironment(opts.Environment)
	stdout, stderr := writerOr(opts.Stdout), writerOr(opts.Stderr)
	resolver := paths.New(env.lookup, opts.Home, func() (string, error) {
		return "", errors.New("the supervisor does not resolve paths against a working directory")
	})
	state, err := resolver.StateDir()
	if err != nil {
		return fmt.Errorf("model-gateway: could not resolve the gateway state directory: %w", err)
	}
	if !filepath.IsAbs(state) || filepath.Clean(state) != state {
		return fmt.Errorf("model-gateway: the gateway state directory %q must be a clean absolute path", state)
	}
	if err := os.MkdirAll(filepath.Join(state, "logs"), 0o777); err != nil {
		return fmt.Errorf("model-gateway: could not create the gateway state directory: %w", err)
	}
	token, err := controlauth.EnsureToken(filepath.Join(state, "control-token"), rand.Reader)
	if err != nil {
		return fmt.Errorf("model-gateway: could not load the local control token: %w", err)
	}
	executable := opts.Executable
	if executable == "" || !filepath.IsAbs(executable) {
		return errors.New("model-gateway: the supervisor needs the absolute path of the worker executable")
	}
	publicPort, err := portSetting(config.PublicPort.Get(env.lookup), "CODEX_GATEWAY_PORT")
	if err != nil {
		return err
	}
	workerPort, err := portSetting(config.SupervisorWorkerPort(env.lookup), "CODEX_GATEWAY_WORKER_PORT")
	if err != nil {
		return err
	}
	if workerPort != 0 && workerPort == publicPort {
		return samePortError(workerPort)
	}

	s := &shim{
		lifetime: context.WithoutCancel(ctx), env: env, state: state, executable: executable, version: opts.Version, stderr: stderr,
		pid: os.Getpid(), startedAt: isoNow(), files: lifecycle.Files{StateDir: state},
		configuredPort: workerPort, fatal: make(chan error, 1),
	}
	s.files.Record("supervisor-started", map[string]any{"component": "supervisor", "pid": s.pid, "startedAt": s.startedAt})
	exitCode := 1
	defer func() {
		s.files.Record("supervisor-exit", map[string]any{"component": "supervisor", "pid": s.pid, "startedAt": s.startedAt, "exitCode": exitCode})
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	fixture, err := s.watchFixtureChannel()
	if err != nil {
		return err
	}
	defer fixture.close()

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort)))
	if err != nil {
		return fmt.Errorf("model-gateway: shim supervisor cannot bind 127.0.0.1:%d: %w", publicPort, err)
	}
	defer func() { _ = listener.Close() }()
	bound := listener.Addr().(*net.TCPAddr).Port
	if workerPort == bound {
		// The public listener would relay to itself, or the worker could never bind its port.
		return samePortError(bound)
	}
	handler, err := supervisor.NewHandler(bound, s.port, token, opts.Version, s.pid)
	if err != nil {
		return err
	}
	if err := s.publishSelf(ctx, bound); err != nil {
		// A supervisor that cannot publish its own record does not serve untracked.
		return fmt.Errorf("model-gateway: shim supervisor could not publish its pid record: %w", err)
	}

	serveCtx, stopServe := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServe()
	served := make(chan error, 1)
	go func() { served <- supervisor.Serve(serveCtx, listener, handler) }()
	_, _ = fmt.Fprintf(stdout, "model-gateway shim supervisor listening on 127.0.0.1:%d\n", bound)
	s.startWorker()

	var stopErr error
	signalName := ""
	listenerDone := false
	for waiting := true; waiting; {
		select {
		case <-ctx.Done():
			waiting = false
		case sig := <-signals:
			signalName, waiting = signalLabel(sig), false
		case stop := <-fixture.result:
			if stop {
				signalName, waiting = "fixture-shutdown", false
			}
		case err := <-served:
			listenerDone = true
			stopErr, waiting = listenerStopped(err), false
		case err := <-s.fatal:
			stopErr, waiting = err, false
		}
	}
	s.stop(signalName)
	stopServe()
	// A failure that was not the reason to stop is not returned; it is printed once so that it is not lost. The
	// chosen stop path alone decides the exit status and the records.
	if !listenerDone {
		if err := <-served; err != nil {
			s.warnFatal(listenerStopped(err))
		}
	}
	s.wait()
	select {
	case err := <-s.fatal:
		s.warnFatal(err)
	default:
	}
	if stopErr != nil {
		return stopErr
	}
	exitCode = 0
	return nil
}

func samePortError(port int) error {
	return fmt.Errorf("model-gateway: CODEX_GATEWAY_WORKER_PORT %d is the public port; the worker needs a port of its own", port)
}

func listenerStopped(err error) error {
	return fmt.Errorf("model-gateway: the public listener stopped: %w", err)
}

func writerOr(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func isoNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func signalLabel(sig os.Signal) string {
	if sig == syscall.SIGINT {
		return "SIGINT"
	}
	return "SIGTERM"
}

func portSetting(value float64, name string) (int, error) {
	if math.IsNaN(value) || value < 0 || value > 65535 || value != math.Trunc(value) {
		return 0, fmt.Errorf("model-gateway: %s is not a valid port", name)
	}
	return int(value), nil
}

// environment is the command's environment as given; the last entry for a name wins, as in a process environment.
type environment struct {
	entries []string
	values  map[string]string
}

func newEnvironment(entries []string) environment {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
			values[name] = value
		}
	}
	return environment{entries: entries, values: values}
}

func (e environment) lookup(name string) (string, bool) {
	v, ok := e.values[name]
	return v, ok
}

// guarded reports whether the test guard is active: either marker is present, whatever its value.
func (e environment) guarded() bool {
	_, sandbox := e.values["MODEL_GATEWAY_TEST_SANDBOX"]
	_, runner := e.values["NODE_TEST_CONTEXT"]
	return sandbox || runner
}

// workerEnvironment removes the fixture channel and any inherited control settings, then adds the worker's own.
func (e environment) workerEnvironment(port int, generation string) []string {
	drop := map[string]bool{"NODE_CHANNEL_FD": true, "MODEL_GATEWAY_CONTROL_FDS": true, "MODEL_GATEWAY_WORKER_GENERATION": true, "CODEX_GATEWAY_WORKER_PORT": true}
	out := make([]string, 0, len(e.entries)+3)
	for _, entry := range e.entries {
		if name, _, _ := strings.Cut(entry, "="); !drop[name] {
			out = append(out, entry)
		}
	}
	return append(out, "CODEX_GATEWAY_WORKER_PORT="+strconv.Itoa(port), "MODEL_GATEWAY_CONTROL_FDS=3,4", "MODEL_GATEWAY_WORKER_GENERATION="+generation)
}

func newGeneration() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
