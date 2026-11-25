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

package procinfo

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCommandWarningRefusal(t *testing.T) {
	tests := map[string]struct{ status int }{"error: warning with otherwise successful output": {0}, "error: warning with partial listing": {1}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			out, err := darwinCommand(t.Context(), "/bin/sh", "-c", `printf 'partial stdout'; printf 'private-first-line-marker\nsecond line\n' >&2; exit "$1"`, "fixture", strconv.Itoa(tt.status))
			if !errors.Is(err, ErrUnknown) || len(out) != 0 {
				t.Fatalf("accepted partial command: %q %v", out, err)
			}
			s := err.Error()
			if !strings.Contains(s, "status "+strconv.Itoa(tt.status)) || !strings.Contains(s, "stderr present") || strings.Contains(s, "private-first-line-marker") || strings.Contains(s, "second line") || strings.Contains(s, "partial stdout") {
				t.Fatalf("unexpected diagnostic: %s", s)
			}
		})
	}
}

func TestProbeLeaderExitsBeforeChild(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "held")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// The shell opens the FIFO before forking. Its child inherits the only writer
	// and both output pipes, regardless of whether it has reached exec at exit.
	out, err := darwinCommand(ctx, "/bin/sh", "-c", `exec 3>"$1"; /bin/sleep 8 & printf done; exit 0`, "probe", fifo)
	if err != nil || string(out) != "done" {
		t.Fatalf("leader exit: output=%q err=%v", out, err)
	}
	var buf [1]byte
	limit := time.Now().Add(time.Second)
	for {
		n, err := unix.Read(fd, buf[:])
		if err == nil && n == 0 {
			break
		}
		if !errors.Is(err, unix.EAGAIN) || time.Now().After(limit) {
			t.Fatalf("descendant retained FIFO or read failed: n=%d err=%v", n, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestGroupLostAfterExternalReap(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.CommandContext(t.Context(), "/bin/cat")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	var observations, signals int
	group := commandGroup{observe: func(pid int) (bool, error) {
		observations++
		exited, err := leaderExited(pid)
		if observations == 1 {
			if err != nil || exited {
				t.Fatalf("child was not running: exited=%t err=%v", exited, err)
			}
			// Simulate another waiter violating the sole-waiter invariant between the
			// last successful observation and cancellation. No PID reuse is necessary.
			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			cancel()
		}
		return exited, err
	}, signal: func(int, unix.Signal) error { signals++; return nil }}
	err = group.stop(ctx, cmd.Process.Pid)
	if !errors.Is(err, ErrGroupCleanup) || signals != 0 || observations != 2 {
		t.Errorf("externally reaped child: err=%v signals=%d observations=%d", err, signals, observations)
	}
}
