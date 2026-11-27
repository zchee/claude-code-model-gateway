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
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func retryEINTR(call func() (int, error)) (int, error) {
	for {
		n, err := call()
		if !errors.Is(err, unix.EINTR) {
			return n, err
		}
	}
}

func (d *directory) Close() error { return d.handle.Close() }

func validBase(base string) bool {
	return base != "" && base != "." && base != ".." && !strings.Contains(base, "/")
}

func (d *directory) Lstat(base string) (*unix.Stat_t, error) {
	if !validBase(base) {
		return nil, &DestinationError{base}
	}
	var st unix.Stat_t
	err := unix.Fstatat(int(d.handle.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(d)
	if err != nil {
		return nil, &os.PathError{Op: "lstat", Path: d.path(base), Err: err}
	}
	return &st, nil
}

func (d *directory) lstatRetry(base string) (*unix.Stat_t, error) {
	var st *unix.Stat_t
	_, err := retryEINTR(func() (int, error) {
		var err error
		st, err = d.Lstat(base)
		return 0, err
	})
	return st, err
}

func (d *directory) OpenFile(base string, flags int, mode fs.FileMode) (*os.File, error) {
	if !validBase(base) {
		return nil, &DestinationError{base}
	}
	fd, err := retryEINTR(func() (int, error) {
		return unix.Openat(int(d.handle.Fd()), base, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	})
	runtime.KeepAlive(d)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: d.path(base), Err: err}
	}
	return os.NewFile(uintptr(fd), d.path(base)), nil
}

func (d *directory) Readlink(base string) (string, error) {
	if !validBase(base) {
		return "", &DestinationError{base}
	}
	target, err := readlinkTarget(func(buf []byte) (int, error) { return unix.Readlinkat(int(d.handle.Fd()), base, buf) })
	runtime.KeepAlive(d)
	if err != nil {
		return "", &os.PathError{Op: "readlink", Path: d.path(base), Err: err}
	}
	return target, nil
}

// Twice the Linux path limit also covers Darwin paths, without trusting a filesystem to stop returning ERANGE.
const maxReadlinkBuffer = 8192

func readlinkTarget(read func([]byte) (int, error)) (string, error) {
	for capacity := 128; capacity <= maxReadlinkBuffer; capacity *= 2 {
		buf := make([]byte, capacity)
		n, err := retryEINTR(func() (int, error) { return read(buf) })
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return "", err
		}
		if n < len(buf) {
			return string(buf[:n]), nil
		}
	}
	return "", unix.ENAMETOOLONG
}

func (d *directory) Rename(from, to string) error {
	if !validBase(from) {
		return &DestinationError{from}
	}
	if !validBase(to) {
		return &DestinationError{to}
	}
	dirfd := int(d.handle.Fd())
	err := unix.Renameat(dirfd, from, dirfd, to)
	runtime.KeepAlive(d)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: d.path(from), New: d.path(to), Err: err}
	}
	return nil
}

// renameInto renames from in d to to in dest, which is on the same filesystem.
func (d *directory) renameInto(from string, dest *directory, to string) error {
	if !validBase(from) {
		return &DestinationError{from}
	}
	if !validBase(to) {
		return &DestinationError{to}
	}
	err := unix.Renameat(int(d.handle.Fd()), from, int(dest.handle.Fd()), to)
	runtime.KeepAlive(d)
	runtime.KeepAlive(dest)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: d.path(from), New: dest.path(to), Err: err}
	}
	return nil
}

// openStaging opens d's existing [StagingDirName] subdirectory without following a symlink, and
// refuses it unless it is a directory owned by the effective user with no access for others.
func (d *directory) openStaging() (*directory, error) {
	fd, err := retryEINTR(func() (int, error) {
		return unix.Openat(int(d.handle.Fd()), StagingDirName, directoryFlags|unix.O_NOFOLLOW, 0)
	})
	runtime.KeepAlive(d)
	path := d.path(StagingDirName)
	if err != nil {
		return nil, &os.PathError{Op: "open staging directory", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "stat staging directory", Path: path, Err: err}
	}
	if st.Uid != uint32(os.Geteuid()) || st.Mode&0o077 != 0 {
		_ = unix.Close(fd)
		return nil, &os.PathError{Op: "open staging directory", Path: path, Err: fs.ErrPermission}
	}
	return &directory{os.NewFile(uintptr(fd), path)}, nil
}

func (d *directory) entryNames() ([]string, error) {
	// A plain dup shares its directory offset. Reopen dot relative to the pinned fd so each sweep starts at the beginning.
	fd, err := retryEINTR(func() (int, error) { return unix.Openat(int(d.handle.Fd()), ".", directoryFlags, 0) })
	runtime.KeepAlive(d)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), d.handle.Name())
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}

// Remove only unlinks files; it never retries as rmdir.
func (d *directory) Remove(base string) error {
	if !validBase(base) {
		return &DestinationError{base}
	}
	_, err := retryEINTR(func() (int, error) { return 0, unix.Unlinkat(int(d.handle.Fd()), base, 0) })
	runtime.KeepAlive(d.handle)
	return err
}

func (d *directory) removeIfPresent(base string) error {
	err := d.Remove(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (d *directory) path(base string) string {
	return strings.TrimSuffix(d.handle.Name(), "/") + "/" + base
}

// Reserve the longest suffix even when the current pid has fewer than ten digits.
const maxDestinationBase = 255 - len(".2147483647.0123456789ab.tmp")

func validWriteBase(base string) bool {
	return len(base) <= maxDestinationBase && validBase(base) && !stagingName(base)
}

func validAppendBase(base string) bool {
	return validBase(base) && !strings.ContainsRune(base, 0) && !stagingName(strings.ToLower(base))
}

func stagingName(base string) bool {
	stem, ok := strings.CutSuffix(base, ".tmp")
	if !ok {
		return false
	}
	stem, _, _ = strings.CutLast(stem, ".")
	target, _, ok := strings.CutLast(stem, ".")
	return ok && target != "" && generatedName(base, target)
}

func splitDestination(path string) (string, string, error) {
	parent, base := filepath.Split(path)
	if !validBase(base) {
		return "", "", &DestinationError{path}
	}
	if parent == "" {
		parent = "."
	}
	return parent, base, nil
}

const directoryFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK | unix.O_DIRECTORY

// openDirectoryAt leaves the spelling of existing components to the kernel: a symlink
// followed by .. is not equivalent to removing both from the path string.
func openDirectoryAt(at int, path string, create bool, perm fs.FileMode) (int, error) {
	return openDirectoryAtFlags(at, path, create, perm, directoryFlags)
}

func openDirectoryAtFlags(at int, path string, create bool, perm fs.FileMode, flags int) (int, error) {
	fd, err := retryEINTR(func() (int, error) { return unix.Openat(at, path, flags, 0) })
	if err == nil {
		return fd, nil
	}
	if !create || !errors.Is(err, fs.ErrNotExist) {
		return -1, &os.PathError{Op: "open directory", Path: path, Err: err}
	}
	parent, leaf := filepath.Split(strings.TrimRight(path, "/"))
	if leaf == "" {
		return -1, &os.PathError{Op: "open directory", Path: path, Err: err}
	}
	ancestor := at
	if parent != "" || at == unix.AT_FDCWD {
		if parent == "" {
			parent = "."
		}
		ancestor, err = openDirectoryAt(at, parent, true, perm)
		if err != nil {
			return -1, err
		}
		defer func() { _ = unix.Close(ancestor) }()
	}
	var mkdirErr error
	if leaf != "." && leaf != ".." {
		_, mkdirErr = retryEINTR(func() (int, error) { return 0, unix.Mkdirat(ancestor, leaf, uint32(perm.Perm())) })
	}
	return openAfterMkdir(ancestor, leaf, path, flags, mkdirErr)
}

func openAfterMkdir(ancestor int, leaf, path string, flags int, mkdirErr error) (int, error) {
	if mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
		return -1, &os.PathError{Op: "mkdir", Path: path, Err: mkdirErr}
	}
	fd, err := retryEINTR(func() (int, error) { return unix.Openat(ancestor, leaf, flags|unix.O_NOFOLLOW, 0) })
	if err != nil {
		return -1, &os.PathError{Op: "open directory", Path: path, Err: err}
	}
	return fd, nil
}

func openParent(parent *directory, path string, create bool) (*directory, string, error) {
	dir, base, err := splitDestination(path)
	if err != nil {
		return nil, "", err
	}
	at := unix.AT_FDCWD
	display := dir
	if parent != nil {
		at = int(parent.handle.Fd())
		if !filepath.IsAbs(dir) {
			display = parent.path(dir)
		}
	}
	fd, err := openDirectoryAt(at, dir, create, 0o777)
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, "", err
	}
	return &directory{os.NewFile(uintptr(fd), display)}, base, nil
}

func openWriteParent(path string) (*directory, string, error) {
	if _, base := filepath.Split(path); !validWriteBase(base) {
		return nil, "", &DestinationError{path}
	}
	return openParent(nil, path, false)
}

func openSettingsParent(path string, create bool) (*directory, string, error) {
	// A restricted name may be an existing symlink to a valid destination,
	// but a missing parent can never contain such a link. Do not create it.
	_, leaf := filepath.Split(path)
	canStage := validWriteBase(leaf)
	d, base, err := openParent(nil, path, create && canStage)
	if err != nil {
		if !canStage && errors.Is(err, fs.ErrNotExist) {
			return nil, "", &DestinationError{path}
		}
		return nil, "", err
	}
	for hops := 0; ; hops++ {
		info, err := d.Lstat(base)
		if (errors.Is(err, fs.ErrNotExist) && hops == 0) || (err == nil && info.Mode&unix.S_IFMT != unix.S_IFLNK) {
			if !validWriteBase(base) {
				invalid := &DestinationError{d.path(base)}
				if hops == 0 {
					invalid.Path = path
				}
				_ = d.Close()
				return nil, "", invalid
			}
			return d, base, nil
		}
		if err != nil {
			_ = d.Close()
			return nil, "", err
		}
		if hops == 40 {
			_ = d.Close()
			return nil, "", &os.PathError{Op: "resolve settings", Path: path, Err: unix.ELOOP}
		}
		link, err := d.Readlink(base)
		if err != nil {
			_ = d.Close()
			return nil, "", err
		}
		next, name, err := openParent(d, link, false)
		_ = d.Close()
		if err != nil {
			return nil, "", err
		}
		d, base = next, name
	}
}
