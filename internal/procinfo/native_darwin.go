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

package procinfo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

// Darwin's kern.proc.pid includes zombies. Reading SZOMB observes exit without
// consuming the wait status or releasing the leader's process group identity.
func leaderExited(pid int) (bool, error) {
	// SysctlKinfoProc also rejects an empty or short kernel result with EIO.
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false, err
	}
	if int(p.Proc.P_pid) != pid || int(p.Eproc.Ppid) != unix.Getpid() {
		return false, unix.ECHILD
	}
	switch p.Proc.P_stat { // sys/proc.h.
	case 1, 2, 3, 4: // SIDL, SRUN, SSLEEP, SSTOP.
		return false, nil
	case 5: // SZOMB.
		return true, nil
	default:
		return false, ErrUnknown
	}
}

func signalCommandGroup(group int, signal unix.Signal) error {
	err := unix.Kill(group, signal)
	if !errors.Is(err, unix.EPERM) {
		return err
	}
	// XNU killpg1 skips SZOMB members and returns EPERM when none can be
	// signaled. Recheck with a fresh kernel census, not an atomic observation
	// of the preceding signal. Require our unreaped zombie to be its only member.
	pid := -group
	members, queryErr := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pid)
	if queryErr != nil {
		// Only the signal errno classifies cleanup; a failed recheck is not ESRCH from kill.
		return fmt.Errorf("%w; kern.proc.pgrp recheck: %s", err, queryErr.Error())
	}
	if len(members) == 1 {
		p := members[0]
		if int(p.Proc.P_pid) == pid && int(p.Eproc.Ppid) == unix.Getpid() && int(p.Eproc.Pgid) == pid && p.Proc.P_stat == 5 {
			return nil
		}
	}
	return fmt.Errorf("%w; kern.proc.pgrp did not confirm a lone zombie leader (%d records)", err, len(members))
}

func procArgs(pid int) (string, []string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", nil, fmt.Errorf("kern.procargs2: %w", err)
	}
	path, argv, err := decodeProcArgs(raw)
	if err != nil {
		return "", nil, fmt.Errorf("decode kern.procargs2 (%d bytes): %w", len(raw), err)
	}
	return path, argv, nil
}

// kernelRecordKey carries a replacement for the kernel's process record read.
// Only tests set it, so they can present a record the kernel would not give.
type kernelRecordKey struct{}

// kernelRecord reads the kernel's process record of pid. SysctlKinfoProc
// rejects an empty or short kernel result with EIO.
func kernelRecord(ctx context.Context, pid int) (*unix.KinfoProc, error) {
	if read, ok := ctx.Value(kernelRecordKey{}).(func(int) (*unix.KinfoProc, error)); ok {
		return read(pid)
	}
	return unix.SysctlKinfoProc("kern.proc.pid", pid)
}

// darwinStartToken formats the start the kernel recorded for a process as
// seconds and six-digit microseconds. The kernel sets it at fork; a clock step
// does not rewrite it and an exec keeps it. A start outside the range of a
// time value yields an empty token.
func darwinStartToken(start unix.Timeval) StartToken {
	if start.Sec <= 0 || start.Usec < 0 || start.Usec >= 1_000_000 {
		return ""
	}
	return StartToken(fmt.Sprintf("%d.%06d", start.Sec, start.Usec))
}

// darwinRow builds the birth and the start token from the same kernel record,
// so both describe the same process lifetime.
func darwinRow(info *unix.KinfoProc) Identity {
	birth := info.Proc.P_starttime
	return Identity{PID: int(info.Proc.P_pid), ParentPID: int(info.Eproc.Ppid), Birth: time.Unix(birth.Sec, int64(birth.Usec)*1000), StartToken: darwinStartToken(birth)}
}

func nativeTable(ctx context.Context) ([]Identity, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	rows := make([]Identity, 0, len(procs))
	for i := range procs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := darwinRow(&procs[i])
		if row.PID <= 0 {
			continue
		}
		_, args, _ := procArgs(row.PID) // A failed sample carries nil argv.
		after, err := kernelRecord(ctx, row.PID)
		var current Identity
		if err == nil {
			current = darwinRow(after)
		} else if errors.Is(unix.Kill(row.PID, 0), unix.ESRCH) {
			continue
		}
		if checked, keep := checkedTableRow(row, current, args); keep {
			rows = append(rows, checked)
		}
	}
	return rows, nil
}

func nativeBirth(ctx context.Context, pid int) (time.Time, error) {
	info, err := kernelRecord(ctx, pid)
	if err != nil {
		return time.Time{}, fmt.Errorf("kern.proc.pid: %w", err)
	}
	if int(info.Proc.P_pid) != pid || info.Proc.P_starttime.Sec <= 0 {
		return time.Time{}, ErrUnknown
	}
	return darwinRow(info).Birth, nil
}

// nativeStartToken reads only the kernel's process record, so a process whose
// arguments or executable are out of reach still has a token.
func nativeStartToken(ctx context.Context, pid int) (StartToken, error) {
	info, err := kernelRecord(ctx, pid)
	if err != nil {
		return "", fmt.Errorf("kern.proc.pid: %w", err)
	}
	token := darwinStartToken(info.Proc.P_starttime)
	if int(info.Proc.P_pid) != pid || token == "" {
		return "", ErrUnknown
	}
	return token, nil
}

func nativeIdentity(ctx context.Context, pid int) (Identity, error) {
	info, err := kernelRecord(ctx, pid)
	if err != nil {
		return Identity{}, fmt.Errorf("kern.proc.pid: %w", err)
	}
	id := darwinRow(info)
	launchPath, argv, err := procArgs(pid)
	if err != nil {
		return Identity{}, err
	}
	id.Argv = argv
	p := process.Process{Pid: int32(pid)}
	kernelPath, err := p.ExeWithContext(ctx)
	if err != nil {
		kernelPath = ""
	}
	// lsof supplies the mapped vnode, never a stat of a potentially replaced leaf.
	id.Image, err = darwinImageEvidence(pid, kernelPath, launchPath, func() ([]byte, error) {
		return darwinCommand(ctx, "/usr/sbin/lsof", "+w", "-nP", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F0pDfni")
	})
	if err != nil {
		return Identity{}, fmt.Errorf("correlate lsof txt vnode with executable: %w", err)
	}
	secondPath, secondArgs, err := procArgs(pid)
	if err != nil {
		return Identity{}, fmt.Errorf("reread argv: %w", err)
	}
	if launchPath != secondPath || !slices.Equal(argv, secondArgs) {
		return Identity{}, fmt.Errorf("argv changed during inspection: %w", ErrUnknown)
	}
	after, err := kernelRecord(ctx, pid)
	if err != nil {
		return Identity{}, fmt.Errorf("reread kern.proc.pid: %w", err)
	}
	if after.Proc.P_pid != info.Proc.P_pid || after.Proc.P_starttime != info.Proc.P_starttime {
		return Identity{}, fmt.Errorf("birth changed during inspection: %w", ErrUnknown)
	}
	return id, nil
}

func nativeListener(ctx context.Context, port uint16) (Listener, error) {
	out, err := darwinCommand(ctx, "/usr/sbin/lsof", "+w", "-nP", "-iTCP:"+strconv.Itoa(int(port)), "-sTCP:LISTEN", "-F0pfd")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Listener{State: ListenerTimedOut}, err
		}
		e, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			return Listener{State: ListenerUnknown}, err
		}
		return lsofListener(out, e.ExitCode())
	}
	return lsofListener(out, 0)
}
