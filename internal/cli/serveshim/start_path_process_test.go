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

//go:build process && (darwin || linux)

package serveshim

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// errLaunchRefused is what the tests' start function returns instead of starting a process.
var errLaunchRefused = errors.New("launch refused by the test")

// startPathShim returns a supervisor whose worker launches go through start, with a state directory that has its logs
// directory and an executable path that does not exist.
func startPathShim(t *testing.T, start func(*exec.Cmd) error, stderr io.Writer) *shim {
	t.Helper()
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &shim{
		lifetime: t.Context(), state: state, executable: filepath.Join(state, "no-such-worker"), version: "1.2.3",
		stderr: stderr, fatal: make(chan error, 1), files: lifecycle.Files{StateDir: state}, startCmd: start,
	}
}

// TestWorkerLaunchHoldsNoLock checks, in virtual time, that the start of a worker process happens outside the
// supervisor's mutex while the generation it starts is already reserved. While a launch is held at the start: the
// mutex is free; another start request launches nothing more; the report deadline is decided without waiting for the
// launch; and a stop returns without waiting for it. Once the held launch ends after the stop, no worker is current
// and every task of the supervisor has finished.
func TestWorkerLaunchHoldsNoLock(t *testing.T) {
	const timeoutText = "model-gateway: shim worker did not report its listener port within 5000ms"
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{}, 4)
		release := make(chan struct{})
		var launches atomic.Int32
		var stderr bytes.Buffer
		s := startPathShim(t, func(*exec.Cmd) error {
			launches.Add(1)
			entered <- struct{}{}
			<-release
			return errLaunchRefused
		}, &stderr)
		launched := make(chan struct{})
		go func() { s.startWorker(); close(launched) }()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("the first start request never reached the start of the process")
		}
		// A mutex held across the launch is detected without blocking on it: a goroutine blocked on a mutex would keep
		// the bubble from ever being idle.
		if !s.mu.TryLock() {
			t.Error("the supervisor's mutex is held while a worker process is being started")
			close(release)
			<-launched
			s.stop("")
			synctest.Wait()
			return
		}
		s.mu.Unlock()

		// The generation is reserved before the launch: another start request while it runs launches nothing.
		go s.startWorker()
		synctest.Wait()
		if diff := gocmp.Diff(int32(1), launches.Load()); diff != "" {
			t.Errorf("process starts after a second start request during a launch (-want +got):\n%s", diff)
		}

		// The report deadline passes while the launch is held; its expiry is decided without waiting for the launch.
		time.Sleep(reportTimeout + time.Millisecond)
		synctest.Wait()
		select {
		case err := <-s.fatal:
			if diff := gocmp.Diff(timeoutText, err.Error()); diff != "" {
				t.Errorf("failure handed to Run while the launch was held (-want +got):\n%s", diff)
			}
		default:
			t.Error("the report deadline passed during a held launch and no timeout was decided")
		}

		// A stop during the launch returns without waiting for the launch to end.
		stopped := make(chan struct{})
		go func() { s.stop("SIGTERM"); close(stopped) }()
		synctest.Wait()
		select {
		case <-stopped:
		default:
			t.Error("a stop during a launch waited for the launch")
		}

		close(release)
		<-launched
		<-stopped
		synctest.Wait()
		s.mu.Lock()
		current := s.current
		s.mu.Unlock()
		if current != nil {
			t.Errorf("a worker whose launch ended after the stop is current: pid %d", current.pid)
		}
		finished := make(chan struct{})
		go func() { s.wait(); close(finished) }()
		synctest.Wait()
		select {
		case <-finished:
		default:
			t.Error("the supervisor's tasks did not finish after the stop and the end of the held launch")
		}
	})
}

// TestShimLogOpenNeverBlocks checks that starting a worker opens logs/shim.log without blocking and refuses a path that
// is not a regular file, with a diagnostic that names the log, before any process is started. A FIFO without a reader
// would block an ordinary open for writing forever; a directory cannot take the worker's output. A missing log is
// created as a regular file and an existing regular log is used.
func TestShimLogOpenNeverBlocks(t *testing.T) {
	const launchFailure = "model-gateway: could not start the shim worker: "
	tests := map[string]struct {
		prepare func(t *testing.T, log string)
		refuse  bool
	}{
		"error: a FIFO without a reader is refused": {func(t *testing.T, log string) {
			if err := unix.Mkfifo(log, 0o600); err != nil {
				t.Fatal(err)
			}
		}, true},
		"error: a FIFO with a reader is refused although it opens": {func(t *testing.T, log string) {
			if err := unix.Mkfifo(log, 0o600); err != nil {
				t.Fatal(err)
			}
			reader, err := os.OpenFile(log, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reader.Close() })
		}, true},
		"error: a directory is refused": {func(t *testing.T, log string) {
			if err := os.Mkdir(log, 0o700); err != nil {
				t.Fatal(err)
			}
		}, true},
		"error: a symlink to a FIFO without a reader is refused": {func(t *testing.T, log string) {
			target := log + ".fifo"
			if err := unix.Mkfifo(target, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, log); err != nil {
				t.Fatal(err)
			}
		}, true},
		"error: a symlink to a directory is refused": {func(t *testing.T, log string) {
			target := log + ".dir"
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, log); err != nil {
				t.Fatal(err)
			}
		}, true},
		"error: a symlink to a regular file is refused": {func(t *testing.T, log string) {
			target := log + ".regular"
			if err := os.WriteFile(target, []byte("earlier\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, log); err != nil {
				t.Fatal(err)
			}
		}, true},
		"success: a missing log is created": {func(*testing.T, string) {}, false},
		"success: an existing regular log is used": {func(t *testing.T, log string) {
			if err := os.WriteFile(log, []byte("earlier\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var launches atomic.Int32
			var stderr bytes.Buffer
			// tried holds the files the refused start was to hand over, copied when the start was asked for, so that a
			// launch which empties the command's slice afterwards cannot make the check below pass by having nothing to
			// look at.
			var tried atomic.Pointer[[]*os.File]
			s := startPathShim(t, func(cmd *exec.Cmd) error {
				launches.Add(1)
				files := slices.Clone(cmd.ExtraFiles)
				tried.Store(&files)
				return errLaunchRefused
			}, &stderr)
			log := filepath.Join(s.state, "logs", "shim.log")
			tc.prepare(t, log)
			before := openDescriptors()
			returned := make(chan struct{})
			go func() { s.startWorker(); close(returned) }()
			// A watchdog only: an open that blocks would otherwise hang the package.
			select {
			case <-returned:
			case <-time.After(10 * time.Second):
				t.Errorf("starting a worker blocked for 10s on a log path prepared as %q", name)
				// A reader on the FIFO releases a blocked open, so the start can end and the case can clean up.
				if reader, err := os.OpenFile(log, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
					defer func() { _ = reader.Close() }()
				}
				<-returned
			}
			// The copies of what the worker would have inherited are closed as soon as the failed start returned.
			if files := tried.Load(); files != nil {
				checkTransferClosed(t, *files)
			}
			s.stop("")
			s.wait()
			// Whatever the launch opened or was handed is closed again: a failed start leaves no descriptor behind.
			if diff := gocmp.Diff(before, openDescriptors()); diff != "" {
				t.Errorf("open descriptors after a failed launch (-before +after):\n%s", diff)
			}
			var failure string
			select {
			case err := <-s.fatal:
				failure = err.Error()
			default:
			}
			if tc.refuse {
				if !strings.HasPrefix(failure, launchFailure) || !strings.Contains(failure, "shim.log") {
					t.Errorf("failure handed to Run %q; want the launch failure starting with %q and naming shim.log", failure, launchFailure)
				}
				if diff := gocmp.Diff(int32(0), launches.Load()); diff != "" {
					t.Errorf("process starts with a refused log (-want +got):\n%s", diff)
				}
				return
			}
			if diff := gocmp.Diff(launchFailure+errLaunchRefused.Error(), failure); diff != "" {
				t.Errorf("failure handed to Run (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(int32(1), launches.Load()); diff != "" {
				t.Errorf("process starts with a usable log (-want +got):\n%s", diff)
			}
			info, err := os.Lstat(log)
			if err != nil || !info.Mode().IsRegular() {
				t.Errorf("shim.log after the start: %v, %v; want a regular file", info, err)
			}
			// The log is opened for appending: what it held before the start is still there.
			if data, err := os.ReadFile(log); err != nil || (name == "success: an existing regular log is used" && !strings.HasPrefix(string(data), "earlier\n")) {
				t.Errorf("shim.log after the start holds %q (%v); want its earlier content kept", data, err)
			}
		})
	}
}

// stopLaunchControlKey names the Unix socket over which the worker of TestStopDuringLaunchDisconnectsWorker talks to
// the test. It is set only in the environment of that worker.
const stopLaunchControlKey = "START_PATH_FIXTURE_CONTROL"

// stopLaunchChild is the worker of TestStopDuringLaunchDisconnectsWorker: it says hello with its pid and says goodbye
// when its supervisor disconnects it, then exits.
func stopLaunchChild(ctx context.Context, control string) error {
	fds := strings.Split(os.Getenv("MODEL_GATEWAY_CONTROL_FDS"), ",")
	if len(fds) != 2 {
		return errors.New("invalid worker descriptor environment")
	}
	liveFD, err := strconv.Atoi(fds[1])
	if err != nil || liveFD <= 2 {
		return errors.New("invalid liveness descriptor")
	}
	live := os.NewFile(uintptr(liveFD), "parent liveness")
	// A hard deadline of its own: this child is started by the supervisor under test, not through gatewaytest, so
	// nothing else would end it if its supervisor never disconnected it.
	time.AfterFunc(20*time.Second, func() { os.Exit(2) })
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", control)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "hello "+strconv.Itoa(os.Getpid())+"\n"); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, live)
	_, _ = io.WriteString(conn, "parent gone\n")
	os.Exit(0)
	return nil
}

// TestStopDuringLaunchDisconnectsWorker checks that a stop which arrives while a worker process is being started is
// honoured without waiting for the start: the stop returns while the start is still held, and the process that the
// held start then creates is disconnected and awaited, so that no worker survives the supervisor's stop.
func TestStopDuringLaunchDisconnectsWorker(t *testing.T) {
	if control := os.Getenv(stopLaunchControlKey); control != "" {
		// This process is the worker the supervisor under test started, not the test itself.
		if err := stopLaunchChild(t.Context(), control); err != nil {
			t.Fatal(err)
		}
		return
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "mg-launch-")
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
	defer func() { _ = control.Close() }()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(exe, '\'') {
		t.Fatalf("test executable path %q cannot be quoted for the launcher script", exe)
	}
	launcher := filepath.Join(home, "worker-launcher")
	script := "#!/bin/sh\nexec '" + exe + "' -test.run='^TestStopDuringLaunchDisconnectsWorker$'\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var stderr bytes.Buffer
	// started is the command whose process the held start created, if any; startErr is the error of that start.
	// Both are written by the launch before it returns and read after launched is closed.
	var started atomic.Pointer[exec.Cmd]
	var startErr error
	// transfer holds the files the start hands over, copied when the start was asked for.
	var transfer []*os.File
	s := startPathShim(t, func(cmd *exec.Cmd) error {
		entered <- struct{}{}
		<-release
		transfer = slices.Clone(cmd.ExtraFiles)
		if err := cmd.Start(); err != nil {
			startErr = err
			return err
		}
		started.Store(cmd)
		// The child is started by the supervisor's own launch, so gatewaytest cannot run it; a child the supervisor
		// failed to end is killed here.
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
			}
		})
		return nil
	}, &stderr)
	s.lifetime = context.WithoutCancel(t.Context())
	s.executable = launcher
	fixture := gatewaytest.NewEnv(home, nil, map[string]string{stopLaunchControlKey: controlPath}, map[string]string{"MODEL_GATEWAY_STATE_DIR": s.state})
	entries := make([]string, 0, len(fixture))
	for k, v := range fixture {
		entries = append(entries, k+"="+v)
	}
	s.env = newEnvironment(entries)

	before := openDescriptors()
	launched := make(chan struct{})
	go func() { s.startWorker(); close(launched) }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the start request never reached the start of the process")
	}
	stopped := make(chan struct{})
	go func() { s.stop("SIGTERM"); close(stopped) }()
	// A case that fails before it waits for the stop still joins it, after the launch was let go.
	t.Cleanup(func() {
		unblock()
		select {
		case <-stopped:
		case <-time.After(20 * time.Second):
			t.Error("the stop did not return within 20s of the launch being let go")
		}
	})
	// A watchdog only: a stop that waits for the launch would otherwise hang the case.
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Error("a stop during a launch did not return while the launch was held")
	}
	unblock()
	<-launched
	// The held start was already under way when the stop came, so it runs to completion: a case whose start failed,
	// or that started nothing, decides nothing about a worker started past a stop.
	if startErr != nil {
		t.Fatalf("the held start of the worker process failed: %v", startErr)
	}
	if started.Load() == nil {
		t.Fatal("the held launch returned without starting the worker process; want the start that was under way to run")
	}
	// The parent's copies of what the worker inherited are closed as soon as the start has returned.
	checkTransferClosed(t, transfer)
	// The held start now creates the process; the stopped supervisor must disconnect it.
	accepted := make(chan net.Conn, 1)
	var acceptor sync.WaitGroup
	acceptor.Go(func() {
		conn, err := control.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	})
	defer acceptor.Wait()
	defer func() { _ = control.Close() }()
	var lines []string
	select {
	case conn, ok := <-accepted:
		if ok {
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			scanner := bufio.NewScanner(conn)
			for scanner.Scan() {
				lines = append(lines, scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				t.Errorf("read the worker's control connection: %v", err)
			}
			_ = conn.Close()
		}
	case <-time.After(10 * time.Second):
		t.Error("the worker started after the stop never reported to the test within 10s")
	}
	<-stopped
	s.wait()
	// The held start created a process: it must have been disconnected, and reaped by the supervisor.
	if diff := gocmp.Diff([]string{"parent gone"}, lines[min(1, len(lines)):]); diff != "" || len(lines) == 0 || !strings.HasPrefix(lines[0], "hello ") {
		t.Errorf("the worker started after the stop said %q; want hello, then parent gone when its supervisor disconnected it", lines)
	}
	// The supervisor reaped it: its own command carries the exit, which a process number could not prove.
	if cmd := started.Load(); cmd.ProcessState == nil {
		t.Errorf("the worker %d started after the stop was not waited for", cmd.Process.Pid)
	}
	// The worker's descriptors are closed and nothing reserved for it is left open.
	if diff := gocmp.Diff(before, openDescriptors()); diff != "" {
		t.Errorf("open descriptors after the stop (-before +after):\n%s", diff)
	}
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	if current != nil {
		t.Errorf("a worker is current after the stop: pid %d", current.pid)
	}
}

// openDescriptors lists the descriptor numbers open in this process below 65,536 or its descriptor limit, whichever
// is lower, without opening one to read them. A number whose inspection fails for any reason other than EBADF counts
// as open. The cap suffices while fewer than 65,536 descriptors are open, as a new descriptor takes the lowest free
// number.
func openDescriptors() []int {
	limit := 1 << 16
	var rl unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NOFILE, &rl) == nil && rl.Cur < uint64(limit) {
		limit = int(rl.Cur)
	}
	var open []int
	for n := range limit {
		if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err == nil || !errors.Is(err, unix.EBADF) {
			open = append(open, n)
		}
	}
	return open
}

// checkTransferClosed fails the test unless every file a start was to pass to the worker is already closed in this
// process.
func checkTransferClosed(t *testing.T, files []*os.File) {
	t.Helper()
	// The handoff is the report and liveness pipes on descriptors 3 and 4, then the listener and the drain channel.
	if len(files) != 4 {
		t.Errorf("the start was asked to hand the worker %d files; want 4: report, liveness, listener and drain channel", len(files))
	}
	for i, f := range files {
		if f == nil {
			t.Errorf("the worker's descriptor %d was to be handed over as nothing; want an open file", 3+i)
			continue
		}
		// Stat reports os.ErrClosed once the File was closed; SyscallConn does not, so it cannot tell.
		if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Errorf("the parent's copy of the worker's descriptor %d (%s) is still open after the start returned (Stat: %v)", 3+i, f.Name(), err)
		}
	}
}

// TestReportTimeoutDecisionIsFinalWithoutLease carries what the decided-timeout test decides without the shim's
// internal lease registry, in virtual time. Once the report deadline has passed and its timeout was handed to Run, the
// decision stays pending and final: a replacement launch whose start fails prints its own failure as the only warning,
// arms no new deadline, and two further whole deadlines decide no second timeout; the first timeout is the one Run
// still holds. Whether a late report publishes a record, and the timely and late lease checks, wait for the
// implementation's own registry seam.
func TestReportTimeoutDecisionIsFinalWithoutLease(t *testing.T) {
	const timeoutText = "model-gateway: shim worker did not report its listener port within 5000ms"
	const launchFailure = "model-gateway: could not start the shim worker: "
	synctest.Test(t, func(t *testing.T) {
		var stderr bytes.Buffer
		s := startPathShim(t, func(*exec.Cmd) error { return errLaunchRefused }, &stderr)
		takeFatal := func() string {
			select {
			case err := <-s.fatal:
				return err.Error()
			default:
				return ""
			}
		}
		defer s.stop("")
		// The first launch arms the report deadline; its failure is the first failure handed to Run.
		s.startWorker()
		synctest.Wait()
		if got := takeFatal(); !strings.HasPrefix(got, launchFailure) {
			t.Fatalf("first launch handed Run %q; want the launch failure starting with %q", got, launchFailure)
		}
		// The deadline passes with no eligible worker; the timeout is decided and handed to Run.
		time.Sleep(reportTimeout + time.Millisecond)
		synctest.Wait()
		// A replacement launch fails while the timeout is pending: its failure can only be a warning.
		s.startWorker()
		synctest.Wait()
		// Checked before any virtual time passes: an armed timer whose expiry does nothing would otherwise go unseen.
		s.mu.Lock()
		armed := s.reportTimer != nil
		s.mu.Unlock()
		if armed {
			t.Error("the replacement launch after the decided timeout armed a report timer")
		}
		// Two whole deadlines pass: a deadline armed again would expire and decide a second timeout.
		time.Sleep(2 * reportTimeout)
		synctest.Wait()
		if diff := gocmp.Diff(timeoutText, takeFatal()); diff != "" {
			t.Errorf("failure Run holds after two more deadlines (-want +got):\n%s", diff)
		}
		if got := takeFatal(); got != "" {
			t.Errorf("a second failure was handed to Run: %q", got)
		}
		var lines []string
		for line := range strings.Lines(stderr.String()) {
			if strings.HasPrefix(line, launchFailure) {
				line = launchFailure
			}
			lines = append(lines, line)
		}
		if diff := gocmp.Diff([]string{launchFailure}, lines); diff != "" {
			t.Errorf("stderr lines, a launch failure shortened to its prefix (-want +got):\n%s\nstderr: %q", diff, stderr.String())
		}
	})
}
