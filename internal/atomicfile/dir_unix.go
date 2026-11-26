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

package atomicfile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// OpenDir opens an existing directory, resolving path as written. Subsequent
// operations use this directory, not another lookup of path.
func OpenDir(path string) (*Dir, error) {
	fd, err := openDirectoryAt(unix.AT_FDCWD, path, false, 0)
	if err != nil {
		return nil, err
	}
	return &Dir{root: directory{os.NewFile(uintptr(fd), path)}}, nil
}

// OpenDirCreate opens path, creating missing directories with the permission
// bits of perm subject to the creation mask. Existing directories, including
// those made by concurrent callers, keep their modes. A non-directory or
// dangling symlink stops the walk; directories already created are not removed.
//
// The walk starts at the root or, for relative paths, the current directory.
// An empty path is invalid. Trailing slashes are accepted, and . and .. are
// resolved in place. Linux and Darwin need search, not read, permission on
// ancestors; the final directory is opened for reading as in [OpenDir].
// A component that resolves to a directory on its first lookup may be a
// symlink. After a missing component, only a real directory is accepted at
// that name. The walk is not confined to any root. Each lookup starts from
// the last directory opened. The returned Dir holds the directory actually
// opened, not necessarily the inode this call created; no subsequent rename
// or symlink change redirects its operations. The caller must close it.
func OpenDirCreate(path string, perm fs.FileMode) (*Dir, error) {
	return openDirCreate(path, perm, nil)
}

func openDirCreate(path string, perm fs.FileMode, afterOpen func(string)) (*Dir, error) {
	f, err := openDirectoryFile(path, perm, afterOpen)
	if err != nil {
		return nil, err
	}
	return &Dir{root: directory{f}}, nil
}

// OpenDirectoryFile opens or creates path with the same traversal and permission
// rules as [OpenDirCreate], and returns the final directory as a readable file.
// The caller owns the file and must close it. Use this for directory-relative
// system calls not offered by [Dir]; otherwise prefer [OpenDirCreate].
func OpenDirectoryFile(path string, perm fs.FileMode) (*os.File, error) {
	return openDirectoryFile(path, perm, nil)
}

func openDirectoryFile(path string, perm fs.FileMode, afterOpen func(string)) (*os.File, error) {
	start := "."
	if path == "" {
		start = "" // Preserve the kernel's empty-path error.
	} else if strings.HasPrefix(path, "/") {
		start = "/"
	}
	remaining := strings.Trim(path, "/")
	flags := traversalFlags
	if remaining == "" { // The starting directory is also the final directory.
		flags = directoryFlags
	}
	fd, err := openDirectoryAtFlags(unix.AT_FDCWD, start, false, 0, flags)
	if err != nil {
		return nil, err
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()
	for remaining != "" {
		component, rest, _ := strings.Cut(remaining, "/")
		remaining = strings.TrimLeft(rest, "/")
		if remaining == "" {
			flags = directoryFlags
		}
		next, err := openDirectoryAtFlags(fd, component, true, perm, flags)
		if err != nil {
			return nil, err
		}
		_ = unix.Close(fd)
		fd = next
		if afterOpen != nil {
			afterOpen(component)
		}
	}
	f := os.NewFile(uintptr(fd), path)
	fd = -1 // Ownership transferred to f.
	return f, nil
}

// ReadFile reads a regular file named by one component. It does not follow
// symlinks; symlinks and special files yield a *NonRegularError.
func (d *Dir) ReadFile(name string) ([]byte, error) {
	return d.readFile(name, nil)
}

func (d *Dir) readFile(name string, beforeOpen func()) ([]byte, error) {
	if d.root.handle == nil {
		return nil, fs.ErrClosed
	}
	st, err := d.root.lstatRetry(name)
	if err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, &NonRegularError{d.root.path(name)}
	}
	// The entry may have changed since lstat. Never follow a new link or
	// wait for a FIFO writer, and check the opened file before reading it.
	if beforeOpen != nil {
		beforeOpen()
	}
	f, err := d.root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, &NonRegularError{d.root.path(name)}
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &NonRegularError{d.root.path(name)}
	}
	return io.ReadAll(f)
}

// WriteFile replaces a single named entry using the same semantics as WriteFile.
func (d *Dir) WriteFile(name string, data []byte, mode fs.FileMode) error {
	if d.root.handle == nil {
		return fs.ErrClosed
	}
	if !validWriteBase(name) {
		return &DestinationError{name}
	}
	return newWriter().writeFileAt(&d.root, name, data, mode)
}

// CreateExclusive publishes a single named entry with the creation and cleanup
// guarantees of [CreateExclusive], including its [StageCleanupError].
func (d *Dir) CreateExclusive(name string, data []byte, mode fs.FileMode) error {
	if d.root.handle == nil {
		return fs.ErrClosed
	}
	if !validWriteBase(name) {
		return &DestinationError{name}
	}
	return newWriter().createExclusiveAt(&d.root, name, data, mode)
}
