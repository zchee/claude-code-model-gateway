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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// holdStartFence takes the start fence of $HOME/.claude/model-gateway, reports "held <pid>", and keeps it
// until the parent sends R (then reports "released" and waits for any byte or end of input) or kills it.
// The lease is acquired under t.Context(), which no deadline ends while the child waits for the parent; the
// parent's fixture limit bounds the child's whole life instead.
func holdStartFence(t testing.TB, args []string) {
	t.Helper()
	if len(args) != 0 {
		t.Fatalf("unexpected helper arguments %q", args)
	}
	state := filepath.Join(os.Getenv("HOME"), ".claude", "model-gateway")
	held := tryFence(t, t.Context(), newFence(t, state))
	if held.err != nil {
		t.Fatalf("child Acquire: %v", held.err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "held %d\n", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	command, err := takeByte()
	if err != nil {
		t.Fatalf("waiting for the parent's command: %v", err)
	}
	if command != 'R' {
		t.Fatalf("unexpected command %q", command)
	}
	if err := unfence(t, held.release); err != nil {
		t.Fatalf("child release: %v", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "released"); err != nil {
		t.Fatal(err)
	}
	if _, err := takeByte(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

type fenceChild struct {
	cmd    *exec.Cmd
	input  *os.File
	lines  chan string
	done   chan struct{}
	err    error
	output bytes.Buffer
}

func startFenceChild(t *testing.T, env gatewaytest.Environment) *fenceChild {
	t.Helper()
	cmd, err := env.HelperCommand(t, "start-fence")
	noError(t, err)
	in, send, err := os.Pipe()
	noError(t, err)
	recv, out := io.Pipe()
	c := &fenceChild{cmd: cmd, input: send, lines: make(chan string, 4), done: make(chan struct{})}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	runDone := make(chan struct{})
	// Twenty seconds for the venue checks, seven for the protocol and one for teardown, as for the lock helper.
	go func() {
		c.err = env.RunFixtureWithin(cmd, 28*time.Second)
		_ = out.Close()
		_ = in.Close()
		close(runDone)
	}()
	go func() {
		defer close(c.done)
		scanner := bufio.NewScanner(recv)
		for scanner.Scan() {
			line := scanner.Text()
			fmt.Fprintln(&c.output, line)
			if strings.HasPrefix(line, "held ") || line == "released" {
				select {
				case c.lines <- line:
				default:
				}
			}
		}
		scanErr := scanner.Err()
		_ = recv.Close()
		<-runDone
		c.err = errors.Join(c.err, scanErr)
		close(c.lines)
	}()
	t.Cleanup(func() { _ = send.Close(); <-c.done })
	return c
}

func (c *fenceChild) next(t *testing.T) string {
	t.Helper()
	line, ok := <-c.lines
	if !ok {
		<-c.done
		t.Fatalf("the fence helper ended without its notice: %v\n%s", c.err, c.output.String())
	}
	return line
}

func TestStartFenceAcrossProcesses(t *testing.T) {
	tests := map[string]struct{ kill bool }{
		"success: holder releases while alive": {},
		"success: holder is killed":            {kill: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, ".claude", "model-gateway")
			noError(t, os.MkdirAll(state, 0o700))
			path := filepath.Join(state, fenceName)
			holder := startFenceChild(t, gatewaytest.NewEnv(home, nil, nil, nil))
			id, ok := strings.CutPrefix(holder.next(t), "held ")
			if !ok {
				t.Fatalf("unexpected helper notice; output:\n%s", holder.output.String())
			}
			pid, err := strconv.Atoi(id)
			noError(t, err)
			equal(t, holder.cmd.Process.Pid, pid)

			// The child, not a leftover of this process, holds the advisory lock.
			onlyCloseOnExec(t, path, 0)
			if !fenceHeld(t, path) {
				t.Fatal("the helper reported the fence held, but worker-start.lock is not locked")
			}
			// An unlocked reference to the file the other process holds, kept open across its end and this
			// process's reacquisition. While it is open that file cannot be freed and its inode number cannot be
			// given to a replacement, so a path that still matches it is the same file. The census below counts
			// it as the test's own descriptor.
			original := openReference(t, path)
			sameFile := func(when string) {
				t.Helper()
				if !sameFileAs(t, path, original) {
					t.Fatalf("%s, worker-start.lock is not the file the other process held; it was replaced", when)
				}
			}
			fence := newFence(t, state)
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				r := tryFence(t, ctx, fence)
				if !errors.Is(r.err, context.DeadlineExceeded) {
					t.Fatalf("Acquire while another process held the fence: %v, want context.DeadlineExceeded", r.err)
				}
			})
			onlyCloseOnExec(t, path, 0, original)

			if tt.kill {
				noError(t, holder.cmd.Process.Kill())
				<-holder.done
				exit, ok := errors.AsType[*exec.ExitError](holder.err)
				if !ok {
					t.Fatalf("holder was not killed: %v\n%s", holder.err, holder.output.String())
				}
				equal(t, syscall.SIGKILL, exit.Sys().(syscall.WaitStatus).Signal())
				// Nobody unlocked or removed anything: the kernel released the lock with the process.
				info, err := os.Lstat(path)
				noError(t, err)
				if !info.Mode().IsRegular() {
					t.Fatalf("fence path has mode %v after the holder died, want the retained regular file", info.Mode())
				}
			} else {
				_, err := io.WriteString(holder.input, "R")
				noError(t, err)
				equal(t, "released", holder.next(t))
			}
			// Before this process tries again, the same file is already free: nothing but the holder's end gave it up.
			sameFile("after the holder gave the fence up")
			if fenceHeld(t, path) {
				t.Fatal("worker-start.lock is still locked after its holder gave it up")
			}

			// The lease is acquired under t.Context(), which no deadline ends; the watchdog only aborts an
			// acquisition that never returns.
			release := leaseWithin(t, fence, "the fence the other process gave up")
			if !fenceHeld(t, path) {
				t.Fatal("Acquire returned without the advisory lock")
			}
			sameFile("after this process acquired the fence")
			onlyCloseOnExec(t, path, 1, original)
			noError(t, unfence(t, release))
			sameFile("after this process released the fence")
			onlyCloseOnExec(t, path, 0, original)

			if !tt.kill {
				_, err := io.WriteString(holder.input, "X")
				noError(t, err)
				<-holder.done
				if holder.err != nil {
					t.Fatalf("fence helper: %v\n%s", holder.err, holder.output.String())
				}
			}
		})
	}
}
