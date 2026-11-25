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
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

func darwinRow(ctx context.Context, info *unix.KinfoProc) (Identity, error) {
	p := process.Process{Pid: info.Proc.P_pid}
	argv, err := p.CmdlineSliceWithContext(ctx)
	if err != nil {
		return Identity{}, err
	}
	birth := info.Proc.P_starttime
	return Identity{PID: int(p.Pid), ParentPID: int(info.Eproc.Ppid), Argv: argv, Birth: time.Unix(birth.Sec, int64(birth.Usec)*1000)}, nil
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
		if row, err := darwinRow(ctx, &procs[i]); err == nil {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func nativeIdentity(ctx context.Context, pid int) (Identity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return Identity{}, err
	}
	id, err := darwinRow(ctx, info)
	if err != nil {
		return Identity{}, err
	}
	p := process.Process{Pid: int32(pid)}
	path, err := p.ExeWithContext(ctx)
	if err != nil || path == "" {
		// proc_pidpath can fail after unlink. The launch path in kern.procargs2
		// precedes argv and is not the user-controlled argv[0].
		args, err := unix.SysctlRaw("kern.procargs2", pid)
		if err != nil {
			return Identity{}, err
		}
		if len(args) < 5 {
			return Identity{}, ErrUnknown
		}
		name, _, ok := bytes.Cut(args[4:], []byte{0})
		if !ok {
			return Identity{}, ErrUnknown
		}
		path = string(name)
	}
	if !filepath.IsAbs(path) {
		return Identity{}, ErrUnknown
	}
	// lsof's txt records contain device and inode from the mapped vnode, not
	// from stat(path), which could already identify a replacement executable.
	out, err := darwinCommand(ctx, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F0pDfni")
	if err != nil {
		return Identity{}, err
	}
	id.Image, err = mappedImage(out, pid, path)
	if err != nil {
		return Identity{}, err
	}
	after, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || after.Proc.P_starttime != info.Proc.P_starttime {
		return Identity{}, ErrUnknown
	}
	return id, nil
}

func mappedImage(out []byte, pid int, path string) (Image, error) {
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

func darwinCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if stderr.Len() != 0 {
		return nil, ErrUnknown
	}
	return out, err
}

func nativeSockets(ctx context.Context, port uint16) ([]socketEvidence, error) {
	// lsof reports descriptors visible to this user, not every kernel socket.
	// Cross-check the listening rows before calling a port vacant or uniquely owned.
	out, err := darwinCommand(ctx, "/usr/sbin/netstat", "-an", "-p", "tcp")
	if err != nil {
		return nil, err
	}
	count := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "tcp") {
			continue
		}
		if len(f) < 6 {
			return nil, ErrUnknown
		}
		if f[5] != "LISTEN" {
			continue
		}
		_, suffix, ok := strings.CutLast(f[3], ".")
		n, err := strconv.ParseUint(suffix, 10, 16)
		if !ok || err != nil {
			return nil, ErrUnknown
		}
		if uint16(n) == port {
			count++
		}
	}
	if count == 0 {
		return nil, nil
	}
	rows := make([]socketEvidence, count)
	for i := range rows {
		rows[i] = socketEvidence{port: port, listening: true}
	}
	out, err = darwinCommand(ctx, "/usr/sbin/lsof", "-nP", "-iTCP:"+strconv.Itoa(int(port)), "-sTCP:LISTEN", "-t")
	if err != nil {
		if e, ok := errors.AsType[*exec.ExitError](err); ok && e.ExitCode() == 1 && len(out) == 0 {
			return rows, nil
		}
		return nil, err
	}
	for field := range strings.FieldsSeq(string(out)) {
		pid, err := strconv.ParseInt(field, 10, 32)
		if err != nil || pid <= 0 {
			return nil, ErrUnknown
		}
		rows[0].owners = append(rows[0].owners, int(pid))
	}
	return rows, nil
}
