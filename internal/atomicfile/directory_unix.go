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
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type directory struct {
	*os.Root
	handle *os.File
}

func (d *directory) Close() error { return errors.Join(d.Root.Close(), d.handle.Close()) }

// Remove only unlinks files. Unlike os.Root.Remove, it never retries as rmdir.
func (d *directory) Remove(base string) error {
	err := unix.Unlinkat(int(d.handle.Fd()), base, 0)
	runtime.KeepAlive(d.handle)
	return err
}

func (d *directory) path(base string) string {
	return strings.TrimSuffix(d.handle.Name(), "/") + "/" + base
}

func splitDestination(path string) (string, string, error) {
	parent, base := filepath.Split(path)
	if base == "" || base == "." || base == ".." {
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
func openDirectoryAt(at int, path string, create bool) (int, error) {
	fd, err := unix.Openat(at, path, directoryFlags, 0)
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
	if parent == "" {
		parent = "."
	}
	ancestor, err := openDirectoryAt(at, parent, true)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(ancestor) }()
	if leaf != "." && leaf != ".." {
		if err := unix.Mkdirat(ancestor, leaf, 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
			return -1, &os.PathError{Op: "mkdir", Path: path, Err: err}
		}
	}
	fd, err = unix.Openat(ancestor, leaf, directoryFlags|unix.O_NOFOLLOW, 0)
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
	fd, err := openDirectoryAt(at, dir, create)
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, "", err
	}
	f := os.NewFile(uintptr(fd), display)
	// Go has no os.Root constructor from an fd. Open the directory descriptor, not the caller's path a second time.
	root, err := os.OpenRoot("/dev/fd/" + strconv.Itoa(fd))
	if err != nil {
		_ = f.Close()
		return nil, "", err
	}
	return &directory{root, f}, base, nil
}

func openSettingsParent(path string, create bool) (*directory, string, error) {
	d, base, err := openParent(nil, path, create)
	if err != nil {
		return nil, "", err
	}
	for hops := 0; ; hops++ {
		info, err := d.Lstat(base)
		if errors.Is(err, fs.ErrNotExist) && hops == 0 {
			return d, base, nil
		}
		if err == nil && info.Mode()&os.ModeSymlink == 0 {
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
