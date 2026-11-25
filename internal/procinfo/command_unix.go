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

//go:build linux || darwin

package procinfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type commandGroup struct {
	observe func(pid int) (exited bool, err error)
	signal  func(int, unix.Signal) error
}

// darwinCommand is the sole waiter for its direct child. The process must not
// ignore SIGCHLD or run a wait-for-any-child reaper. Under those invariants,
// observing the unreaped child pins both its PID and its process group ID until
// this goroutine calls Wait. Descendants that leave the group are not covered.
// WaitDelay bounds pipe draining, not the wait for a child stuck in the kernel.
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	group := commandGroup{observe: leaderExited, signal: unix.Kill}
	cleanupErr := group.stop(ctx, cmd.Process.Pid)
	waitErr := cmd.Wait()
	if cleanupErr != nil {
		return nil, cleanupErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stderr.Len() != 0 {
		return nil, ErrUnknown
	}
	return stdout.Bytes(), waitErr
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
