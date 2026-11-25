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

package lifecycle_test

import (
	"bufio"
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

func nativeIdentity(ctx context.Context, pid int) (lifecycle.ProcessState, string) {
	state, birth := nativeBirth(ctx, pid)
	if state != lifecycle.Alive {
		return state, ""
	}
	return state, procinfo.LegacyStart(birth)
}

// Both supported venues provide POSIX signal zero and SIGKILL wait status.
func nativeBirth(ctx context.Context, pid int) (lifecycle.ProcessState, time.Time) {
	probeCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	probe := procinfo.New(nil)
	alive, err := probe.Alive(probeCtx, pid)
	if err != nil {
		return lifecycle.Unknown, time.Time{}
	}
	if !alive {
		return lifecycle.Absent, time.Time{}
	}
	birthCtx, cancelBirth := context.WithTimeout(ctx, 4*time.Second)
	defer cancelBirth()
	birth, err := probe.Birth(birthCtx, pid)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return lifecycle.Absent, time.Time{}
	case err != nil:
		return lifecycle.Unknown, time.Time{}
	default:
		return lifecycle.Alive, birth
	}
}

func TestLegacyProcessInterop(t *testing.T) {
	env := gatewaytest.NewEnv(t.TempDir(), nil, map[string]string{"LC_ALL": "C", "TZ": "Asia/Tokyo"}, nil)
	oracle := startChild(t, env)
	equal(t, "ready", oracle.notice(t))
	oracle.send(t, "L")
	<-oracle.done
	if oracle.err != nil {
		t.Fatalf("legacy oracle: %v\n%s", oracle.err, oracle.output.String())
	}
}

func legacyOracle(t testing.TB) {
	t.Helper()
	equal(t, "C", os.Getenv("LC_ALL"))
	equal(t, "Asia/Tokyo", os.Getenv("TZ"))
	// DispatchHelper already entered this process's private directory in the sandbox.
	directory, err := os.Getwd()
	noError(t, err)
	lock := lockFor(directory)
	lock.PID = os.Getpid()
	host, err := os.Hostname()
	noError(t, err)
	lock.Host = host
	lock.Processes = lookup(nativeIdentity)
	lock.Timeout = new(time.Duration(0))
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "/bin/ps", "-ww", "-p", strconv.Itoa(lock.PID), "-o", "lstart=").Output()
	noError(t, err)
	birth := strings.TrimSpace(string(raw))
	if birth == "" {
		t.Fatal("ps returned no birth")
	}
	cancel()
	state, sample := nativeBirth(t.Context(), lock.PID)
	equal(t, lifecycle.Alive, state)
	if zone, offset := sample.Zone(); offset != 9*60*60 {
		t.Fatalf("native birth zone %q has UTC offset %d seconds, want +09:00; Asia/Tokyo time zone data may be missing", zone, offset)
	}
	equal(t, birth, procinfo.LegacyStart(sample))
	// The legacy encoder inserts these fields in this order; its birth text is taken from ps, not from our formatter.
	ownerBytes := func(name string) []byte {
		data, err := json.Marshal(struct {
			Token     string `json:"token"`
			PID       int    `json:"pid"`
			StartedAt string `json:"startedAt"`
			Host      string `json:"host"`
		}{name, lock.PID, birth, host})
		noError(t, err)
		return data
	}
	dir := filepath.Join(lock.StateDir, "lifecycle.lock")
	noError(t, os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, previous)
	old := ownerBytes(previous)
	noError(t, atomicfile.WriteFile(path, old, 0o600))
	_, err = lock.Acquire(t.Context())
	if err == nil {
		t.Fatal("displaced a live legacy owner")
	}
	equal(t, busyMessage(dir), err.Error())
	equal(t, string(old), bytesAt(t, path))
	noError(t, os.Remove(path))
	noError(t, os.Remove(dir))
	release := acquire(t, lock)
	defer release()
	equal(t, string(ownerBytes(token)), bytesAt(t, filepath.Join(dir, token)))
}

func takeByte() (byte, error) { var b [1]byte; _, err := io.ReadFull(os.Stdin, b[:]); return b[0], err }

func appendEntry(state, verb string) error {
	path := filepath.Join(state, "ledger")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return atomicfile.WriteFile(path, fmt.Appendf(data, "%s %d\n", verb, os.Getpid()), 0o600)
}

func runLockChild(t testing.TB, args []string) {
	t.Helper()
	if len(args) != 0 {
		t.Fatal("unexpected helper arguments")
	}
	status := os.Stdout
	if _, err := fmt.Fprintln(status, "ready"); err != nil {
		t.Fatal(err)
	}
	role, err := takeByte()
	if err != nil {
		t.Fatal(err)
	}
	if role == 'L' {
		legacyOracle(t)
		return
	}
	// Both helpers have finished the venue checks before the parent assigns roles.
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Second)
	defer cancel()
	const phaseLimit = 7 * time.Second
	watchdog := time.AfterFunc(phaseLimit, func() { os.Exit(2) })
	defer watchdog.Stop()
	state := filepath.Join(os.Getenv("HOME"), ".claude", "model-gateway")
	switch role {
	case 'A', 'B':
		files := filesFor(state)
		for i := range 32 {
			files.Record("writer-"+string(role), map[string]any{"pid": i})
		}
		return
	case 'M':
		mask := syscall.Umask(0o027)
		defer syscall.Umask(mask)
		files := filesFor(state)
		if err := files.WriteStartupFailure(os.Getpid(), nil, "failure"); err != nil {
			t.Fatal(err)
		}
		files.Record("mode", nil)
		return
	case 'H', 'C':
	default:
		t.Fatal("unknown fixture role")
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	lock := lifecycle.Lock{StateDir: state, Host: host, PID: os.Getpid(), Timeout: new(6 * time.Second), Processes: lookup(func(ctx context.Context, pid int) (lifecycle.ProcessState, string) {
		state, birth := nativeIdentity(ctx, pid)
		if role == 'C' && pid != os.Getpid() {
			if _, err := fmt.Fprintf(status, "observed %d %d %s\n", pid, state, birth); err != nil {
				t.Fatal(err)
			}
		}
		return state, birth
	})}
	if role == 'C' {
		attempts := 0
		lock.Wait = func(ctx context.Context, d time.Duration) error {
			if attempts < 2 {
				watchdog.Reset(phaseLimit)
				attempts++
				if _, err := fmt.Fprintln(status, "busy"); err != nil {
					return err
				}
				b, err := takeByte()
				if err != nil {
					return err
				}
				if b != 'R' {
					return errors.New("missing retry command")
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(d):
				return nil
			}
		}
	}
	err = lock.With(ctx, func(context.Context) error {
		dir := filepath.Join(state, "lifecycle.lock")
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			return fmt.Errorf("got %d owner entries", len(entries))
		}
		data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
		if err != nil {
			return err
		}
		var owner lifecycle.Owner
		if err := json.Unmarshal(data, &owner); err != nil {
			return err
		}
		if owner.PID != os.Getpid() {
			return fmt.Errorf("owner pid %d, want %d", owner.PID, os.Getpid())
		}
		if err := appendEntry(state, "enter"); err != nil {
			return err
		}
		watchdog.Reset(phaseLimit)
		if _, err := fmt.Fprintf(status, "entered %d\n", os.Getpid()); err != nil {
			return err
		}
		cmd, err := takeByte()
		if err != nil {
			return err
		}
		if cmd != 'G' {
			return errors.New("unexpected critical-section command")
		}
		return appendEntry(state, "exit")
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGatewayFixtureHelper(t *testing.T) {
	gatewaytest.DispatchHelper(t, map[string]gatewaytest.HelperFunc{"lifecycle-lock": runLockChild})
}

type child struct {
	cmd    *exec.Cmd
	input  *os.File
	status chan string
	done   chan struct{}
	err    error
	output bytes.Buffer
}

func startChild(t *testing.T, env gatewaytest.Environment) *child {
	t.Helper()
	cmd, err := env.HelperCommand(t, "lifecycle-lock")
	noError(t, err)
	in, send, err := os.Pipe()
	noError(t, err)
	recv, out := io.Pipe()
	c := &child{cmd: cmd, input: send, status: make(chan string, 2), done: make(chan struct{})}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	runDone := make(chan struct{})
	// Budget twenty seconds collectively for account and listener checks (two ss
	// calls on Linux, one lsof on Darwin), seven for the protocol and one for
	// teardown. Slower guards fail the test rather than extend this bound.
	const limit = 28 * time.Second
	go func() { c.err = env.RunFixtureWithin(cmd, limit); _ = out.Close(); _ = in.Close(); close(runDone) }()
	go func() {
		defer close(c.done)
		scanner := bufio.NewScanner(recv)
		for scanner.Scan() {
			s := scanner.Text()
			fmt.Fprintln(&c.output, s)
			if s == "ready" || s == "busy" || strings.HasPrefix(s, "observed ") || strings.HasPrefix(s, "entered ") {
				select {
				case c.status <- s:
				default:
				}
			}
		}
		scanErr := scanner.Err()
		_ = recv.Close()
		<-runDone
		c.err = errors.Join(c.err, scanErr)
		close(c.status)
	}()
	t.Cleanup(func() { _ = send.Close(); <-c.done })
	return c
}

func (c *child) send(t *testing.T, command string) {
	t.Helper()
	_, err := io.WriteString(c.input, command)
	noError(t, err)
}

func (c *child) notice(t *testing.T) string {
	t.Helper()
	s, ok := <-c.status
	if !ok {
		<-c.done
		t.Fatalf("missing helper notification; child: %v\n%s", c.err, c.output.String())
	}
	return s
}

func entered(t *testing.T, c *child) int {
	t.Helper()
	line := c.notice(t)
	id, ok := strings.CutPrefix(line, "entered ")
	if !ok {
		t.Fatalf("expected entry notification: %q", line)
	}
	pid, err := strconv.Atoi(id)
	noError(t, err)
	if pid <= 0 {
		t.Fatalf("invalid child pid: %d", pid)
	}
	return pid
}

func TestConcurrentJournalAppend(t *testing.T) {
	home := t.TempDir()
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	dir := filepath.Join(home, ".claude", "model-gateway", "logs")
	noError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "lifecycle.jsonl")
	const seed = `{"event":"seed"}`
	noError(t, atomicfile.WriteFile(path, []byte(seed+"\n"), 0o600))
	original, err := os.Open(path)
	noError(t, err)
	t.Cleanup(func() {
		if err := original.Close(); err != nil {
			t.Error(err)
		}
	})
	a, b := startChild(t, env), startChild(t, env)
	equal(t, "ready", a.notice(t))
	equal(t, "ready", b.notice(t))
	a.send(t, "A")
	b.send(t, "B")
	<-a.done
	<-b.done
	noError(t, a.err)
	noError(t, b.err)
	data, err := io.ReadAll(original)
	noError(t, err)
	equal(t, string(data), bytesAt(t, path))
	text, terminated := strings.CutSuffix(string(data), "\n")
	equal(t, true, terminated)
	got := strings.Split(text, "\n")
	equal(t, 65, len(got))
	equal(t, seed, got[0])
	want := make([]string, 0, 64)
	for _, writer := range []string{"A", "B"} {
		for i := range 32 {
			want = append(want, fmt.Sprintf(`{"at":"1970-01-01T00:00:00.123Z","event":"writer-%s","pid":%d}`, writer, i))
		}
	}
	slices.Sort(want)
	slices.Sort(got[1:])
	equal(t, want, got[1:])
}

func TestStateCreationModes(t *testing.T) {
	home := t.TempDir()
	env := gatewaytest.NewEnv(home, nil, nil, nil)
	state := filepath.Join(home, ".claude", "model-gateway")
	noError(t, os.MkdirAll(state, 0o700))
	writer := startChild(t, env)
	equal(t, "ready", writer.notice(t))
	writer.send(t, "M")
	<-writer.done
	if writer.err != nil {
		t.Fatalf("mode writer: %v\n%s", writer.err, writer.output.String())
	}
	for _, path := range []string{"shim-supervisor-failure.txt", "logs/lifecycle.jsonl"} {
		info, err := os.Stat(filepath.Join(state, path))
		noError(t, err)
		equal(t, fs.FileMode(0o640), info.Mode().Perm())
	}
}

func TestProcessContention(t *testing.T) {
	tests := map[string]struct{ killed bool }{"success: ordered release": {}, "success: killed holder": {true}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			holder, contender := startChild(t, env), startChild(t, env)
			equal(t, "ready", holder.notice(t))
			equal(t, "ready", contender.notice(t))
			holder.send(t, "H")
			first := entered(t, holder)
			state := filepath.Join(home, ".claude", "model-gateway")
			dir := filepath.Join(state, "lifecycle.lock")
			entries, err := os.ReadDir(dir)
			noError(t, err)
			equal(t, 1, len(entries))
			path := filepath.Join(dir, entries[0].Name())
			data := bytesAt(t, path)
			var owner lifecycle.Owner
			noError(t, json.Unmarshal([]byte(data), &owner))
			if owner.StartedAt == nil || *owner.StartedAt == "" {
				t.Fatal("missing holder birth")
			}
			equal(t, first, owner.PID)
			contender.send(t, "C")
			for attempt := range 2 {
				if attempt != 0 {
					contender.send(t, "R")
				}
				equal(t, fmt.Sprintf("observed %d %d %s", first, lifecycle.Alive, *owner.StartedAt), contender.notice(t))
				equal(t, "busy", contender.notice(t))
				equal(t, data, bytesAt(t, path))
				equal(t, fmt.Sprintf("enter %d\n", first), bytesAt(t, filepath.Join(state, "ledger")))
			}
			if tc.killed {
				// Both the sandbox ledger and pipe must identify our own exec.Cmd child.
				equal(t, fmt.Sprintf("enter %d\n", first), bytesAt(t, filepath.Join(state, "ledger")))
				equal(t, first, holder.cmd.Process.Pid)
				noError(t, holder.cmd.Process.Kill())
				<-holder.done
				exit, ok := errors.AsType[*exec.ExitError](holder.err)
				if !ok {
					t.Fatalf("holder was not killed: %v", holder.err)
				}
				equal(t, syscall.SIGKILL, exit.Sys().(syscall.WaitStatus).Signal())
				owners, err := os.ReadDir(dir)
				noError(t, err)
				equal(t, 1, len(owners))
			} else {
				holder.send(t, "G")
				<-holder.done
				noError(t, holder.err)
			}
			contender.send(t, "R")
			if tc.killed {
				equal(t, fmt.Sprintf("observed %d %d ", first, lifecycle.Absent), contender.notice(t))
			}
			second := entered(t, contender)
			if first == second {
				t.Fatal("both entrants have the same pid")
			}
			contender.send(t, "G")
			<-contender.done
			noError(t, contender.err)
			want := fmt.Sprintf("enter %d\nexit %d\nenter %d\nexit %d\n", first, first, second, second)
			if tc.killed {
				want = fmt.Sprintf("enter %d\nenter %d\nexit %d\n", first, second, second)
			}
			equal(t, want, bytesAt(t, filepath.Join(state, "ledger")))
			mustBeAbsent(t, dir)
		})
	}
}
