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

// CreateStagingDir opens path as [OpenDirCreate] does with perm, then creates inside it the
// private [StagingDirName] subdirectory (mode 0o700, subject to the creation mask) that
// [PrivateStaging] writes stage their files in. An existing subdirectory is kept and checked as
// [PrivateStaging] checks it. Creating the subdirectory is itself an event in path for a watcher
// of path, so it belongs before any reader watches, not before a write.
func CreateStagingDir(path string, perm fs.FileMode) error {
	d, err := OpenDirCreateStaging(path, perm)
	if err != nil {
		return err
	}
	return d.Close()
}

// OpenDirCreateStaging is [CreateStagingDir] that returns the opened directory, so that a write
// that has just prepared it goes through the same directory and not another lookup of path. The
// caller must close it.
func OpenDirCreateStaging(path string, perm fs.FileMode) (*Dir, error) {
	d, err := OpenDirCreate(path, perm)
	if err != nil {
		return nil, err
	}
	fd := int(d.root.handle.Fd())
	_, err = retryEINTR(func() (int, error) { return 0, unix.Mkdirat(fd, StagingDirName, 0o700) })
	if err != nil && !errors.Is(err, fs.ErrExist) {
		err = &os.PathError{Op: "mkdir", Path: d.root.path(StagingDirName), Err: err}
		return nil, errors.Join(err, d.Close())
	}
	staging, err := d.root.openStaging()
	if err == nil {
		err = staging.Close()
	}
	if err != nil {
		return nil, errors.Join(err, d.Close())
	}
	return d, nil
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
//
// File.Name returns the original spelling of path for diagnostics; after a rename
// it may no longer name the held directory. Enumerate with File.Readdirnames and
// use descriptor-relative calls for metadata: DirEntry.Info from File.ReadDir
// may call Lstat by path. Keep the file open and alive until the last use of its
// raw descriptor (for example, call runtime.KeepAlive afterward).
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
func (d *Dir) WriteFile(name string, data []byte, mode fs.FileMode, opts ...WriteOption) error {
	if d.root.handle == nil {
		return fs.ErrClosed
	}
	if !validWriteBase(name) {
		return &DestinationError{name}
	}
	return newWriter(opts...).writeFileAt(&d.root, name, data, mode)
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

// Remove unlinks the entry name, one path component, relative to the held directory. A symbolic link is removed
// itself, never followed, and a directory is never removed. A missing entry returns an error matching fs.ErrNotExist.
func (d *Dir) Remove(name string) error {
	if d.root.handle == nil {
		return fs.ErrClosed
	}
	if err := d.root.Remove(name); err != nil {
		if _, ok := errors.AsType[*DestinationError](err); ok {
			return err
		}
		return &os.PathError{Op: "remove", Path: d.root.path(name), Err: err}
	}
	return nil
}

// OpenFile opens the entry name, one path component, relative to the held directory with flag and, when it creates
// the entry, the permission bits of perm. O_NOFOLLOW and O_CLOEXEC are always added, so a symbolic link at name is
// never followed and no executed child inherits the file. The caller owns the returned file and must close it.
func (d *Dir) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if d.root.handle == nil {
		return nil, fs.ErrClosed
	}
	return d.root.OpenFile(name, flag, perm)
}
