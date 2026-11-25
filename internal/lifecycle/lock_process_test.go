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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// Both supported venues provide POSIX signal zero and SIGKILL wait status.
func nativeIdentity(ctx context.Context, pid int) (lifecycle.ProcessState, string) {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return lifecycle.Absent, ""
	} else if err != nil {
		return lifecycle.Unknown, ""
	}
	p, err := process.NewProcessWithContext(ctx, int32(pid))
	if err != nil {
		return lifecycle.Alive, ""
	}
	born, err := p.CreateTimeWithContext(ctx)
	if err != nil || born == 0 {
		return lifecycle.Alive, ""
	}
	return lifecycle.Alive, strconv.FormatInt(born, 10)
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
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	watchdog := time.AfterFunc(4*time.Second, func() { os.Exit(2) })
	defer watchdog.Stop()
	status := os.Stdout
	role, err := takeByte()
	if err != nil {
		t.Fatal(err)
	}
	if role != 'H' && role != 'C' {
		t.Fatal("unknown fixture role")
	}
	state := filepath.Join(os.Getenv("HOME"), ".claude", "model-gateway")
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	lock := lifecycle.Lock{StateDir: state, Host: host, PID: os.Getpid(), Timeout: new(2 * time.Second), Processes: lookup(func(pid int) (lifecycle.ProcessState, string) { return nativeIdentity(ctx, pid) })}
	if role == 'C' {
		first := true
		lock.Wait = func(ctx context.Context, d time.Duration) error {
			if first {
				first = false
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

func startChild(t *testing.T, env gatewaytest.Environment, role string) *child {
	t.Helper()
	cmd, err := env.HelperCommand(t, "lifecycle-lock")
	noError(t, err)
	in, send, err := os.Pipe()
	noError(t, err)
	recv, out := io.Pipe()
	c := &child{cmd: cmd, input: send, status: make(chan string, 2), done: make(chan struct{})}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	runDone := make(chan struct{})
	go func() { c.err = env.RunFixture(cmd); _ = out.Close(); _ = in.Close(); close(runDone) }()
	go func() {
		defer close(c.done)
		scanner := bufio.NewScanner(recv)
		for scanner.Scan() {
			s := scanner.Text()
			fmt.Fprintln(&c.output, s)
			if s == "busy" || strings.HasPrefix(s, "entered ") {
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
	c.send(t, role)
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

func TestProcessContention(t *testing.T) {
	tests := map[string]struct{ killed bool }{"success: ordered release": {}, "success: killed holder": {true}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			env := gatewaytest.NewEnv(home, nil, nil, nil)
			holder := startChild(t, env, "H")
			first := entered(t, holder)
			contender := startChild(t, env, "C")
			equal(t, "busy", contender.notice(t))
			state := filepath.Join(home, ".claude", "model-gateway")
			dir := filepath.Join(state, "lifecycle.lock")
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
