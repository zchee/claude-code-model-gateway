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
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

func procArgs(pid int) (string, []string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", nil, err
	}
	return decodeProcArgs(raw)
}

func darwinRow(info *unix.KinfoProc) Identity {
	birth := info.Proc.P_starttime
	return Identity{PID: int(info.Proc.P_pid), ParentPID: int(info.Eproc.Ppid), Birth: time.Unix(birth.Sec, int64(birth.Usec)*1000)}
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
		after, err := unix.SysctlKinfoProc("kern.proc.pid", row.PID)
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

func nativeIdentity(ctx context.Context, pid int) (Identity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Identity{}, err
	}
	id := darwinRow(info)
	launchPath, argv, err := procArgs(pid)
	if err != nil {
		return Identity{}, err
	}
	id.Argv = argv
	p := process.Process{Pid: int32(pid)}
	path, err := p.ExeWithContext(ctx)
	if err != nil || path == "" {
		path = launchPath
	}
	if !filepath.IsAbs(path) {
		return Identity{}, ErrUnknown
	}
	// lsof's txt records contain device and inode from the mapped vnode, not
	// from stat(path), which could already identify a replacement executable.
	out, err := darwinCommand(ctx, "/usr/sbin/lsof", "+w", "-nP", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F0pDfni")
	if err != nil {
		return Identity{}, err
	}
	id.Image, err = mappedImage(out, pid, path)
	if err != nil {
		return Identity{}, err
	}
	secondPath, secondArgs, err := procArgs(pid)
	if err != nil || launchPath != secondPath || !slices.Equal(argv, secondArgs) {
		return Identity{}, ErrUnknown
	}
	after, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || after.Proc.P_pid != info.Proc.P_pid || after.Proc.P_starttime != info.Proc.P_starttime {
		return Identity{}, ErrUnknown
	}
	return id, nil
}

func mappedImage(out []byte, pid int, path string) (Image, error) {
	if !bytes.HasSuffix(out, []byte{0, '\n'}) {
		return Image{}, ErrUnknown
	}
	var match Image
	var current Image
	var seenPID, text, device bool
	finish := func() error {
		if !text || current.Path != path {
			return nil
		}
		if !device || current.Inode == 0 || match.Inode != 0 && current != match {
			return ErrUnknown
		}
		match = current
		return nil
	}
	for field := range bytes.SplitSeq(out, []byte{0}) {
		field = bytes.TrimPrefix(field, []byte{'\n'})
		if len(field) == 0 {
			continue
		}
		value := string(field[1:])
		switch field[0] {
		case 'p':
			if seenPID || value != strconv.Itoa(pid) {
				return Image{}, ErrUnknown
			}
			seenPID = true
		case 'f':
			if err := finish(); err != nil {
				return Image{}, err
			}
			current = Image{}
			text = value == "txt"
			device = false
		case 'D':
			n, err := strconv.ParseUint(value, 0, 64)
			if err != nil {
				return Image{}, err
			}
			current.Device = n
			device = true
		case 'i':
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return Image{}, err
			}
			current.Inode = n
		case 'n':
			current.Path = value
		default:
			return Image{}, ErrUnknown
		}
	}
	if err := finish(); err != nil {
		return Image{}, err
	}
	if !seenPID || match.Inode == 0 {
		return Image{}, ErrUnknown
	}
	return match, nil
}

func nativeSockets(ctx context.Context, port uint16, _ func()) ([]socketEvidence, error) {
	// lsof reports descriptors visible to this user, not every kernel socket.
	// Cross-check the listening rows before calling a port vacant or uniquely owned.
	out, err := darwinCommand(ctx, "/usr/sbin/netstat", "-an", "-p", "tcp")
	if err != nil {
		return nil, err
	}
	count, err := countTCPListeners(out, port)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	out, err = darwinCommand(ctx, "/usr/sbin/lsof", "+w", "-nP", "-iTCP:"+strconv.Itoa(int(port)), "-sTCP:LISTEN", "-F0pfd")
	if err != nil {
		if e, ok := errors.AsType[*exec.ExitError](err); !ok || e.ExitCode() != 1 || len(out) != 0 {
			return nil, err
		}
	}
	return listenerFiles(out, port, count)
}
