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

//go:build linux

package ownership

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// pidfdOpen is the pidfd_open system call that openProcess makes; the package's tests replace it to script its
// answers.
var pidfdOpen = unix.PidfdOpen

// pidfdSendSignal is the pidfd_send_signal system call that a pidfd handle makes; the package's tests replace it to
// record its calls.
var pidfdSendSignal = unix.PidfdSendSignal

// openProcess opens a pidfd for pid. Stop opens it before the second sample of a decision and signals through it, so
// the signal can reach only the process that held the number when the handle was opened: if that process is still
// alive when the signal is sent, it held the number throughout, including at the second sample, so the sample
// described it. If it has exited, the signal fails instead of reaching a process that reused the number.
//
// pidfd_open answering ENOSYS gets the numeric signal and its residual race. That happens on a kernel before Linux
// 5.3, and also on a newer kernel whose system-call filter (a seccomp profile of a container runtime, for example)
// refuses pidfd_open with ENOSYS; nothing else falls back.
func openProcess(pid int) (processHandle, error) {
	for {
		fd, err := pidfdOpen(pid, 0)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENOSYS):
			return numericProcess(pid), nil
		case errors.Is(err, unix.ESRCH):
			return nil, errProcessGone
		case err != nil:
			return nil, fmt.Errorf("open a handle on process %d: %w", pid, err)
		}
		return pidfd(fd), nil
	}
}

// pidfd is a Linux process file descriptor; it names one process for its whole lifetime, never a reused number.
type pidfd int

func (fd pidfd) signal(sig syscall.Signal) error {
	err := pidfdSendSignal(int(fd), sig, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return errProcessGone
	}
	return err
}

func (fd pidfd) close() error { return unix.Close(int(fd)) }
