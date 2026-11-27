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

//go:build unix

package serveshim

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
)

// imageOf identifies the executable file the supervisor launches or runs from, by path, device and inode.
func imageOf(path string) (procinfo.Image, error) {
	if !filepath.IsAbs(path) {
		return procinfo.Image{}, errors.New("executable path is not absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return procinfo.Image{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return procinfo.Image{}, errors.New("executable has no device and inode")
	}
	return procinfo.Image{Path: path, Device: uint64(st.Dev), Inode: st.Ino}, nil //nolint:gosec,unconvert // Stat_t.Dev is signed on some platforms.
}

// openFixtureChannel takes the inherited fixture channel named by NODE_CHANNEL_FD, after the same checks the worker
// applies to its inherited descriptors: not a standard stream, a pipe or socket, and readable. A descriptor that fails
// them is never touched: under the test guard that is a startup error, and outside it the setting is ignored. The
// channel is made close-on-exec, so the worker never inherits it, and non-blocking, so closing it ends a pending read.
func openFixtureChannel(env environment) (*os.File, error) {
	raw, ok := env.lookup("NODE_CHANNEL_FD")
	if !ok {
		return nil, nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd <= 2 || strconv.Itoa(fd) != raw {
		if env.guarded() {
			return nil, fmt.Errorf("model-gateway: NODE_CHANNEL_FD names an unusable descriptor %q", raw)
		}
		return nil, nil
	}
	if err := checkReadable(fd); err != nil {
		if env.guarded() {
			return nil, fmt.Errorf("model-gateway: NODE_CHANNEL_FD descriptor %d is unusable: %w", fd, err)
		}
		return nil, nil
	}
	// Marked with forks locked out, so that no worker started meanwhile inherits it.
	syscall.ForkLock.RLock()
	unix.CloseOnExec(fd)
	syscall.ForkLock.RUnlock()
	if err := unix.SetNonblock(fd, true); err != nil {
		return nil, fmt.Errorf("model-gateway: could not prepare the fixture channel descriptor %d: %w", fd, err)
	}
	return os.NewFile(uintptr(fd), "fixture channel"), nil
}

// checkReadable accepts an open pipe or socket that this process can read from.
func checkReadable(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if kind := st.Mode & unix.S_IFMT; kind != unix.S_IFIFO && kind != unix.S_IFSOCK {
		return errors.New("not a pipe or socket")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if mode := flags & unix.O_ACCMODE; mode != unix.O_RDONLY && mode != unix.O_RDWR {
		return errors.New("open in the wrong direction")
	}
	return nil
}

// openLog opens the worker's log for appending without blocking and without following a symbolic link, creating it as
// a regular file when it is missing. Anything but a regular file is refused before a worker could write to it: a FIFO
// would block an ordinary open, or take the worker's output away, and a directory cannot take it at all.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o666)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("open %s: not a regular file", path)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func signalName(sig syscall.Signal) string {
	if name := unix.SignalName(sig); name != "" {
		return name
	}
	return strconv.Itoa(int(sig))
}
