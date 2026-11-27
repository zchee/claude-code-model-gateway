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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/common"
	"golang.org/x/sys/unix"
)

func procRoot(ctx context.Context) string {
	if env, ok := ctx.Value(common.EnvKey).(common.EnvMap); ok && env[common.HostProcEnvKey] != "" {
		return env[common.HostProcEnvKey]
	}
	return "/proc"
}

// procClock turns start ticks into a wall-clock birth. bootID is the kernel's
// boot identifier, or empty when it could not be read; with it, a stat sample
// also yields the start token of the same process lifetime.
type procClock struct {
	boot, ticks int64
	bootID      string
}

func readClock(root string) (procClock, error) {
	aux, err := unix.Auxv()
	if err != nil {
		return procClock{}, err
	}
	var ticks int64
	for _, entry := range aux {
		if entry[0] == 17 { // AT_CLKTCK, the clock used by /proc/pid/stat.
			ticks = int64(entry[1])
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "stat"))
	if err != nil {
		return procClock{}, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if s, ok := strings.CutPrefix(line, "btime "); ok {
			boot, err := strconv.ParseInt(s, 10, 64)
			if err == nil && boot > 0 && ticks > 0 {
				return procClock{boot: boot, ticks: ticks}, nil
			}
		}
	}
	return procClock{}, ErrUnknown
}

// readBootID returns the kernel's identifier of the current boot. Start ticks
// count from boot, so only together with it do they name one process lifetime.
// It is refused when empty, or when it holds a space, a control or non-ASCII
// byte or the token separator, so that a token splits in exactly one way.
func readBootID(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if id == "" || strings.ContainsFunc(id, func(r rune) bool { return r <= ' ' || r > '~' || r == '/' }) {
		return "", fmt.Errorf("unusable boot identifier: %w", ErrUnknown)
	}
	return id, nil
}

// linuxStartToken joins the boot identifier and the start ticks of stat. It is
// empty without a boot identifier.
func linuxStartToken(bootID string, start int64) StartToken {
	if bootID == "" {
		return ""
	}
	return StartToken(bootID + "/" + strconv.FormatInt(start, 10))
}

func readAt(dir *os.File, name string) ([]byte, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// statFields returns the parent pid and the start ticks of a stat line. The
// command name may hold spaces and parentheses, so the fields are counted from
// the last ") ".
func statFields(stat []byte) (int, int64, error) {
	_, fields, ok := strings.CutLast(string(stat), ") ")
	cols := strings.Fields(fields)
	if !ok || len(cols) < 20 {
		return 0, 0, ErrUnknown
	}
	ppid, err := strconv.Atoi(cols[1])
	if err != nil || ppid < 0 {
		return 0, 0, ErrUnknown
	}
	start, err := strconv.ParseInt(cols[19], 10, 64)
	if err != nil || start < 0 {
		return 0, 0, ErrUnknown
	}
	return ppid, start, nil
}

// processStat builds the birth and the start token from one stat read, so both
// describe the same process even when its number is reused right after.
func (c procClock) processStat(pid int, stat []byte) (Identity, error) {
	ppid, start, err := statFields(stat)
	if err != nil {
		return Identity{}, err
	}
	return Identity{PID: pid, ParentPID: ppid, Birth: time.Unix(c.boot+start/c.ticks, (start%c.ticks)*int64(time.Second)/c.ticks), StartToken: linuxStartToken(c.bootID, start)}, nil
}

// nativeStartToken reads only the boot identifier and one stat line. It needs
// neither the arguments, the executable nor the boot wall clock, so a process
// whose other evidence is out of reach still has a token.
func nativeStartToken(ctx context.Context, pid int) (StartToken, error) {
	root := procRoot(ctx)
	stat, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	_, start, err := statFields(stat)
	if err != nil {
		return "", err
	}
	bootID, err := readBootID(root)
	if err != nil {
		return "", err
	}
	return linuxStartToken(bootID, start), nil
}

func nativeBirth(ctx context.Context, pid int) (time.Time, error) {
	root := procRoot(ctx)
	clock, err := readClock(root)
	if err != nil {
		return time.Time{}, err
	}
	stat, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}, err
	}
	id, err := clock.processStat(pid, stat)
	return id.Birth, err
}

func (c procClock) inspect(root string, pid int, withImage bool) (Identity, error) {
	// A held proc directory refers to this process, even if its pid is reused.
	dir, err := os.Open(filepath.Join(root, strconv.Itoa(pid)))
	if err != nil {
		return Identity{}, err
	}
	defer func() { _ = dir.Close() }()
	stat, err := readAt(dir, "stat")
	if err != nil {
		return Identity{}, err
	}
	id, err := c.processStat(pid, stat)
	if err != nil {
		return Identity{}, err
	}
	cmd, err := readAt(dir, "cmdline")
	if err != nil {
		return Identity{}, err
	}
	if len(cmd) > 0 {
		id.Argv = strings.Split(string(bytes.TrimSuffix(cmd, []byte{0})), "\x00")
	}
	if !withImage {
		return id, nil
	}
	fd, err := unix.Openat(int(dir.Fd()), "exe", unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return Identity{}, err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Identity{}, err
	}
	path, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return Identity{}, err
	}
	after, err := readAt(dir, "cmdline")
	if err != nil {
		return Identity{}, err
	}
	if !bytes.Equal(cmd, after) {
		return Identity{}, ErrUnknown
	}
	id.Image = ImageEvidence{Path: path, Device: uint64(st.Dev), Inode: st.Ino, DirectFromExecutable: true}
	return id, nil
}

func nativeIdentity(ctx context.Context, pid int) (Identity, error) {
	root := procRoot(ctx)
	clock, err := readClock(root)
	if err != nil {
		return Identity{}, err
	}
	// The token only grants authority to act, and an empty one never matches,
	// so an unreadable boot identifier empties the token instead of failing the
	// sample. The token comes from the stat line inspect reads for the birth.
	clock.bootID, _ = readBootID(root)
	return clock.inspect(root, pid, true)
}

func nativeTable(ctx context.Context) ([]Identity, error) {
	root := procRoot(ctx)
	clock, err := readClock(root)
	if err != nil {
		return nil, err
	}
	// As for Identity, an unreadable boot identifier empties every row's token
	// and keeps the rows.
	clock.bootID, _ = readBootID(root)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var rows []Identity
	for _, ent := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if id, err := clock.inspect(root, pid, false); err == nil {
			rows = append(rows, id)
		}
	}
	return rows, nil
}

func listeningInodes(root string, port uint16) ([]uint64, error) {
	var inodes []uint64
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(root, "net", name))
		if name == "tcp6" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		header, body, ok := strings.Cut(string(data), "\n")
		if !ok || !strings.Contains(header, "local_address") && !strings.Contains(header, "local_addr") {
			return nil, ErrUnknown
		}
		for line := range strings.SplitSeq(body, "\n") {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			if len(f) < 10 {
				return nil, ErrUnknown
			}
			_, hexPort, ok := strings.Cut(f[1], ":")
			n, err := strconv.ParseUint(hexPort, 16, 16)
			if !ok || err != nil {
				return nil, ErrUnknown
			}
			if uint16(n) != port || f[3] != "0A" {
				continue
			}
			inode, err := strconv.ParseUint(f[9], 10, 64)
			if err != nil {
				return nil, ErrUnknown
			}
			inodes = append(inodes, inode)
		}
	}
	slices.Sort(inodes)
	return inodes, nil
}

func nativeListener(ctx context.Context, port uint16) (Listener, error) {
	rows, err := stableSocketHolders(ctx, procRoot(ctx), port, nil, listeningInodes)
	return classifyListener(rows, err, port), err
}

func stableSocketHolders(ctx context.Context, root string, port uint16, changed func(), read func(string, uint16) ([]uint64, error)) ([]socketEvidence, error) {
	before, err := read(root, port)
	if err != nil {
		return nil, err
	}
	for range 2 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := socketHolders(ctx, root, port, before)
		if err != nil {
			return nil, err
		}
		after, err := read(root, port)
		if err != nil {
			return nil, err
		}
		if slices.Equal(before, after) {
			return rows, nil
		}
		if changed != nil {
			changed()
		}
		before = after
	}
	return nil, ErrUnknown
}

func socketHolders(ctx context.Context, root string, port uint16, ids []uint64) ([]socketEvidence, error) {
	// Keep per-inode descriptors: endpoint deduplication would lose reuse-port
	// listeners, and retaining only the first descriptor would lose inherited ones.
	inodes := make(map[string]int, len(ids))
	rows := make([]socketEvidence, len(ids))
	for i, inode := range ids {
		if inode != 0 {
			inodes[fmt.Sprintf("socket:[%d]", inode)] = i
		}
		rows[i] = socketEvidence{port: port, listening: true}
	}
	if len(inodes) == 0 {
		return rows, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdPath := filepath.Join(root, ent.Name(), "fd")
		fds, err := os.ReadDir(fdPath)
		if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, fd := range fds {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			target, err := os.Readlink(filepath.Join(fdPath, fd.Name()))
			if errors.Is(err, os.ErrPermission) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if i, ok := inodes[target]; ok {
				rows[i].owners = append(rows[i].owners, pid)
			}
		}
	}
	return rows, nil
}
