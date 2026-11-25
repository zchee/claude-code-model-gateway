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
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

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
	pid := cmd.Process.Pid
	observeErr := waitForLeader(ctx, pid)
	// The leader (even as a zombie) pins its numeric process group until Wait.
	// ECHILD means we no longer own a waitable child; never signal that number.
	var killErr error
	if !errors.Is(observeErr, unix.ECHILD) {
		killErr = unix.Kill(-pid, unix.SIGKILL)
	}
	waitErr := cmd.Wait() // Reap first, then drain both output pipes under WaitDelay.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if observeErr != nil {
		return nil, observeErr
	}
	if killErr != nil && !errors.Is(killErr, unix.ESRCH) {
		return nil, killErr
	}
	if stderr.Len() != 0 {
		return nil, ErrUnknown
	}
	return stdout.Bytes(), waitErr
}

func waitForLeader(ctx context.Context, pid int) error {
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		exited, err := leaderExited(pid)
		if !errors.Is(err, unix.EINTR) && (err != nil || exited) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
