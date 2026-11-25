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

//go:build darwin || (linux && process)

package procinfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type commandGroup struct {
	observe func(pid int) (exited bool, err error)
	signal  func(int, unix.Signal) error
}

// darwinCommand is the sole waiter for its direct child. The process must not
// run a wait-for-any-child reaper or enable automatic reaping in any form,
// including ignoring SIGCHLD or setting SA_NOCLDWAIT. Under those invariants
// the unreaped child pins its PID and process group ID until this goroutine
// calls Wait. Descendants that leave the group are not covered. Refused cleanup
// sends no signal and waits for the child's natural exit. WaitDelay bounds only
// pipe draining, not natural exit or a kernel-stuck child: Listener still returns
// at its budget, but macOS Identity can return late.
func darwinCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Only this goroutine may signal the group and reap its leader. In particular,
	// do not give os/exec an independent context watcher that races with Wait.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	var stdout bytes.Buffer
	var stderr stderrSize
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	tool := filepath.Base(path)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", tool, err)
	}
	group := commandGroup{observe: leaderExited, signal: signalCommandGroup}
	cleanupErr := group.stop(ctx, cmd.Process.Pid)
	waitErr := cmd.Wait()
	status := cmd.ProcessState.ExitCode()
	diagnostic := stderr.diagnostic()
	if cleanupErr != nil {
		return nil, fmt.Errorf("%s cleanup (status %d%s): %w", tool, status, diagnostic, cleanupErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s (status %d%s): %w", tool, status, diagnostic, err)
	}
	if diagnostic != "" {
		return nil, fmt.Errorf("%s (status %d%s): %w", tool, status, diagnostic, ErrUnknown)
	}
	if waitErr != nil {
		return stdout.Bytes(), fmt.Errorf("%s (status %d): %w", tool, status, waitErr)
	}
	return stdout.Bytes(), nil
}

func (g commandGroup) stop(ctx context.Context, pid int) error {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		exited, err := g.observe(pid)
		if err != nil {
			return fmt.Errorf("%w: observe child %d: %w", ErrGroupCleanup, pid, err)
		}
		if exited {
			break
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	// Neither a previously observed exit nor cancellation establishes current
	// ownership. An empty, short or otherwise failed observation is a refusal.
	if _, err := g.observe(pid); err != nil {
		return fmt.Errorf("%w: verify child %d: %w", ErrGroupCleanup, pid, err)
	}
	if err := g.signal(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("%w: signal group %d: %w", ErrGroupCleanup, pid, err)
	}
	return nil
}
