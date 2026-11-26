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

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
	"uuid"

	"golang.org/x/sys/unix"
)

const lockName = "lifecycle.lock"

func openAt(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readOwner(dir *os.File) *Owner {
	names, err := dir.Readdirnames(2)
	if err != nil || len(names) != 1 {
		return nil
	}
	file, err := openAt(dir, names[0], unix.O_RDONLY|unix.O_NOCTTY, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	data, err := io.ReadAll(file)
	var owner Owner
	if err != nil || !decodeRecord(data, &owner) || owner.Token != names[0] {
		return nil
	}
	return &owner
}

func removeOwned(parent, dir *os.File, name, token string) {
	if unix.Unlinkat(int(dir.Fd()), token, 0) == nil {
		// Never remove a replacement owner's file, even if this release is delayed.
		_ = unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
	}
}

func (l Lock) reap(ctx context.Context, parent *os.File) {
	dir, err := openAt(parent, lockName, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return
	}
	defer func() { _ = dir.Close() }()
	if owner := readOwner(dir); Stale(ctx, owner, l.Host, l.Processes) {
		removeOwned(parent, dir, lockName, owner.Token)
	}
}

func (l Lock) acquire(ctx context.Context, birth string) (release func(), err error) {
	if !validStateDir(l.StateDir) {
		return nil, fs.ErrInvalid
	}
	var token string
	if l.NewToken != nil {
		token = l.NewToken()
	} else {
		token = uuid.NewV4().String()
	}
	if !filepath.IsLocal(token) || token == "." || token != filepath.Base(token) {
		return nil, fs.ErrInvalid
	}
	data, err := compactRecord(Owner{Token: token, PID: l.PID, StartedAt: &birth, Host: l.Host})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(l.StateDir, 0o777); err != nil {
		return nil, err
	}
	parent, err := os.Open(l.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if release == nil {
			_ = parent.Close()
		}
	}()
	candidate := lockName + ".candidate-" + uuid.NewV4().String()
	fd := int(parent.Fd())
	if err := unix.Mkdirat(fd, candidate, 0o700); err != nil {
		return nil, err
	}
	dir, err := openAt(parent, candidate, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		_ = unix.Unlinkat(fd, candidate, unix.AT_REMOVEDIR)
		return nil, err
	}
	defer func() {
		if release == nil {
			_ = unix.Unlinkat(int(dir.Fd()), token, 0)
			_ = unix.Unlinkat(fd, candidate, unix.AT_REMOVEDIR)
			_ = dir.Close()
		}
	}()
	// This file is private until the directory rename publishes its complete contents.
	file, err := openAt(dir, token, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = file.Write(data)
	if err = errors.Join(err, file.Close()); err != nil {
		return nil, err
	}
	timeout := 15 * time.Second
	if l.Timeout != nil {
		timeout = *l.Timeout
	}
	now := clock(l.Now)
	deadline := now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// os.Rename and os.Root.Rename refuse even an empty directory on Unix.
		err := unix.Renameat(fd, candidate, fd, lockName)
		if err == nil {
			return sync.OnceFunc(func() {
				removeOwned(parent, dir, lockName, token)
				_ = dir.Close()
				_ = parent.Close()
			}), nil
		}
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, fs.ErrPermission) {
			return nil, err
		}
		l.reap(ctx, parent)
		if !now().Before(deadline) {
			return nil, fmt.Errorf("gateway lifecycle is busy or ownership is unknown (%s); retry recover or run doctor", filepath.Join(l.StateDir, lockName))
		}
		const interval = 50 * time.Millisecond
		if l.Wait != nil {
			if err := l.Wait(ctx, interval); err != nil {
				return nil, err
			}
		} else {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(interval):
			}
		}
	}
}
