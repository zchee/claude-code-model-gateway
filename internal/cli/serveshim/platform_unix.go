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

// openFixtureChannel takes the inherited fixture channel named by NODE_CHANNEL_FD. It is made close-on-exec, so the
// worker never inherits it, and non-blocking, so closing it ends a pending read.
func openFixtureChannel(env environment) *os.File {
	raw, ok := env.lookup("NODE_CHANNEL_FD")
	if !ok {
		return nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd <= 2 || strconv.Itoa(fd) != raw {
		return nil
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return nil
	}
	unix.CloseOnExec(fd)
	if unix.SetNonblock(fd, true) != nil {
		return nil
	}
	return os.NewFile(uintptr(fd), "fixture channel")
}

func signalName(sig syscall.Signal) string {
	if name := unix.SignalName(sig); name != "" {
		return name
	}
	return strconv.Itoa(int(sig))
}
