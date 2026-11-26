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

package serveshim_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/serveshim"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const (
	// orderControlKey names the Unix socket on which the ordering worker child greets the test and tells it when its
	// supervisor disconnected it. It is set only in the environment of a worker the supervisor under test starts.
	orderControlKey = "FAILURE_ORDER_CONTROL"
	// orderMisreportKey, when set, makes the child report a port other than its configured one as soon as it starts.
	orderMisreportKey = "FAILURE_ORDER_MISREPORT"
	// orderLingerKey, when set, keeps the child running after its supervisor disconnected it, until the test releases
	// it or the supervisor kills it. The child ignores SIGTERM, so the supervisor's stop stays in progress for both of
	// its exit waits, and the child answers "alive" on the control socket while it still runs.
	orderLingerKey = "FAILURE_ORDER_LINGER"
	// stopGuard bounds the wait for a supervisor's stop, which includes the wait for a lingering worker.
	stopGuard = 20 * time.Second
	// reportTimeoutText is the supervisor's diagnostic for a worker that did not report its listener in time.
	reportTimeoutText = "model-gateway: shim worker did not report its listener port within 5000ms"
)

// orderChild is the worker the supervisor under test starts through the launcher script. It never reports the port it
// was configured with: it reports another one when orderMisreportKey is set, and nothing otherwise. It says "parent
// gone" on the control socket when its supervisor disconnects it. On the control socket it answers "alive?" with
// "alive" and exits on "release" or when the test closes the socket.
func orderChild(ctx context.Context, control string) error {
	generation := os.Getenv("MODEL_GATEWAY_WORKER_GENERATION")
	if generation == "" || os.Getenv("MODEL_GATEWAY_CONTROL_FDS") != "3,4" {
		return errors.New("invalid worker descriptor environment")
	}
	report, live := os.NewFile(3, "port report"), os.NewFile(4, "parent liveness")
	if report == nil || live == nil {
		return errors.New("missing worker descriptors")
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", control)
	if err != nil {
		return err
	}
	var writes sync.Mutex
	send := func(line string) {
		writes.Lock()
		defer writes.Unlock()
		_, _ = io.WriteString(conn, line+"\n")
	}
	send("hello " + strconv.Itoa(os.Getpid()))
	if _, misreport := os.LookupEnv(orderMisreportKey); misreport {
		configured, err := strconv.Atoi(os.Getenv("CODEX_GATEWAY_WORKER_PORT"))
		if err != nil || configured == 0 {
			return fmt.Errorf("a misreporting child needs a configured worker port, got %q", os.Getenv("CODEX_GATEWAY_WORKER_PORT"))
		}
		other := configured%65535 + 1
		line, err := json.Marshal(map[string]any{"type": "listening", "port": other, "generation": generation})
		if err == nil {
			_, err = report.Write(append(line, '\n'))
		}
		if err = errors.Join(err, report.Close()); err != nil {
			return err
		}
	}
	_, linger := os.LookupEnv(orderLingerKey)
	if linger {
		signal.Ignore(syscall.SIGTERM)
	}
	go func() {
		_, _ = io.Copy(io.Discard, live)
		send("parent gone")
		if !linger {
			os.Exit(0)
		}
	}()
	commands := bufio.NewScanner(conn)
	for commands.Scan() {
		switch commands.Text() {
		case "alive?":
			send("alive")
		case "release":
			os.Exit(0)
		}
	}
	// The test closed its control socket.
	os.Exit(0)
	return nil
}

// orderedShim is a foreground supervisor running in this process with the ordering child as its worker. Unlike the
// supervisor of the eligibility tests, the test can request its stop at a moment of its choosing.
type orderedShim struct {
	public   string
	stop     context.CancelFunc
	ready    chan string
	readyAt  time.Time
	done     chan struct{}
	result   error
	stderr   *lockedBuffer
	children chan *fixtureChild
}

// startOrderedShim runs serveshim.Run with the given settings and the ordering child as its worker.
func startOrderedShim(t *testing.T, env map[string]string) *orderedShim {
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
	if err := os.WriteFile(filepath.Join(state, "control-token"), []byte(hex.EncodeToString(raw[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// sun_path is short; the test's temporary directory can exceed it.
	sockDir, err := os.MkdirTemp("", "mg-order-")
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
	s := &orderedShim{ready: make(chan string, 1), done: make(chan struct{}), stderr: new(lockedBuffer), children: make(chan *fixtureChild, 8)}
	var accepted sync.WaitGroup
	var connsMu sync.Mutex
	var conns []net.Conn
	accepted.Go(func() {
		for {
			conn, err := control.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
			_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
			lines := bufio.NewScanner(conn)
			if !lines.Scan() {
				continue
			}
			v, ok := strings.CutPrefix(lines.Text(), "hello ")
			pid, err := strconv.Atoi(v)
			if !ok || err != nil {
				continue
			}
			s.children <- &fixtureChild{pid: pid, conn: conn, lines: lines}
		}
	})

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(exe, '\'') {
		t.Fatalf("test executable path %q cannot be quoted for the launcher script", exe)
	}
	launcher := filepath.Join(home, "worker-launcher")
	script := "#!/bin/sh\nexec '" + exe + "' -test.run='^TestReportTimeoutAgainstStop$'\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	overrides := map[string]string{"MODEL_GATEWAY_STATE_DIR": state}
	maps.Copy(overrides, env)
	fixture := gatewaytest.NewEnv(home, nil, map[string]string{orderControlKey: controlPath}, overrides)
	entries := make([]string, 0, len(fixture))
	for k, v := range fixture {
		entries = append(entries, k+"="+v)
	}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	s.stop = cancel
	var watched sync.WaitGroup
	var watchErr error
	watched.Go(func() {
		lines := bufio.NewScanner(pr)
		for lines.Scan() {
			if v, ok := strings.CutPrefix(lines.Text(), "model-gateway shim supervisor listening on "); ok {
				select {
				case s.ready <- v:
				default:
				}
			}
		}
		watchErr = lines.Err()
		_ = pr.Close()
	})
	go func() {
		defer close(s.done)
		s.result = serveshim.Run(ctx, serveshim.Options{
			Environment: entries, Home: func() (string, error) { return home, nil }, Executable: launcher,
			Version: fixtureVersion, Stdout: pw, Stderr: s.stderr,
		})
		_ = pw.Close()
	}()
	t.Cleanup(func() {
		cancel()
		<-s.done
		watched.Wait()
		if watchErr != nil {
			t.Errorf("read the supervisor's stdout: %v", watchErr)
		}
		_ = control.Close()
		accepted.Wait()
		connsMu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		connsMu.Unlock()
	})
	return s
}

// awaitReady returns once the supervisor printed its readiness line, and its first worker greeted the test.
func (s *orderedShim) awaitReady(t *testing.T) *fixtureChild {
	t.Helper()
	select {
	case s.public = <-s.ready:
		s.readyAt = time.Now()
	case <-s.done:
		t.Fatalf("supervisor stopped before it was ready: %v; stderr: %s", s.result, s.stderr)
	}
	select {
	case c := <-s.children:
		return c
	case <-s.done:
		t.Fatalf("supervisor stopped before starting a worker: %v; stderr: %s", s.result, s.stderr)
	case <-t.Context().Done():
		t.Fatal("test ended before the supervisor started a worker")
	}
	return nil
}

// awaitStop waits for the supervisor to stop, failing the test when it is still running after stopGuard.
func (s *orderedShim) awaitStop(t *testing.T) {
	t.Helper()
	timer := time.NewTimer(stopGuard)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-timer.C:
		t.Fatalf("supervisor still running %v after it was expected to stop; stderr: %s", stopGuard, s.stderr)
	}
}

// TestReportTimeoutAgainstStop checks how a worker that never reports its listener and a stop request decide the
// supervisor's outcome. A stop that comes first ends the supervisor with status 0 and nothing on stderr. A report
// deadline that passes first stops it with the timeout, which Run returns and does not print itself. When the stop
// request arrives at the deadline, either may win, and only these outcomes are accepted: the stop came first (status
// 0, nothing on stderr); the timeout came first (Run returns exactly the timeout, nothing on stderr); or the timeout
// was decided but the stop won the selection of the reason to stop (status 0, the timeout printed once as a warning,
// and nothing else). Any other error or line fails the case.
//
// Which side wins at the deadline is decided by the scheduler. These requests straddle the deadline so that both
// orders occur; a timeout that is decided and then neither returned nor printed cannot be told apart from a stop
// that came first, so that loss is not asserted here.
func TestReportTimeoutAgainstStop(t *testing.T) {
	if control := os.Getenv(orderControlKey); control != "" {
		// This process is a worker the supervisor under test started, not the test itself.
		if err := orderChild(t.Context(), control); err != nil {
			t.Fatal(err)
		}
		return
	}
	tests := map[string]struct {
		run func(t *testing.T)
	}{
		"success: stop requested well before the report deadline ends with status 0 and no diagnostic": {func(t *testing.T) {
			s := startOrderedShim(t, nil)
			child := s.awaitReady(t)
			s.stop()
			if diff := gocmp.Diff("parent gone", child.farewell(t)); diff != "" {
				t.Errorf("worker's view of the stop (-want +got):\n%s", diff)
			}
			s.awaitStop(t)
			if s.result != nil {
				t.Errorf("supervisor stopped with %v; want exit status 0 after a stop request", s.result)
			}
			if diff := gocmp.Diff("", s.stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		}},
		"error: report deadline without a stop request returns the timeout and prints nothing": {func(t *testing.T) {
			s := startOrderedShim(t, nil)
			s.awaitReady(t)
			s.awaitStop(t)
			if s.result == nil || s.result.Error() != reportTimeoutText {
				t.Errorf("supervisor stopped with %v; want exactly the report timeout %q", s.result, reportTimeoutText)
			}
			// The error Run returns is printed by its caller; a copy here would print it twice.
			if diff := gocmp.Diff("", s.stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		}},
	}
	// Stop requests from two milliseconds before the deadline, counted from readiness, to one and a half after it.
	for i := range 8 {
		offset := time.Duration(i-4) * 500 * time.Microsecond
		tests[fmt.Sprintf("success: stop requested %v from the report deadline reports the timeout at most once", offset)] = struct {
			run func(t *testing.T)
		}{func(t *testing.T) {
			s := startOrderedShim(t, nil)
			s.awaitReady(t)
			at := time.NewTimer(time.Until(s.readyAt.Add(reportDeadline + offset)))
			select {
			case <-at.C:
				s.stop()
			case <-s.done:
				at.Stop()
			}
			s.awaitStop(t)
			stderr := s.stderr.String()
			switch {
			case s.result == nil && stderr == "":
				// The stop came first.
			case s.result != nil && s.result.Error() == reportTimeoutText && stderr == "":
				// The timeout came first; the caller of Run prints it.
			case s.result == nil && stderr == reportTimeoutText+"\n":
				// The timeout was decided, and the stop won the selection; the timeout is printed once as a warning.
			default:
				t.Errorf("supervisor stopped with %v and stderr %q; want nil and nothing on stderr, exactly the report timeout %q and nothing on stderr, or nil and that timeout printed once as the only line", s.result, stderr, reportTimeoutText)
			}
		}}
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// Each case waits for its own worker's report deadline; the cases need not wait in turn.
			t.Parallel()
			tc.run(t)
		})
	}
}
