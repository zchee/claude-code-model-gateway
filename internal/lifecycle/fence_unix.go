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
	"io/fs"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

// fenceRetry is the pause between two attempts on a fence another holder has. The fence is held only around short
// filesystem steps and a process start, so a short pause keeps a waiting start prompt.
const fenceRetry = 20 * time.Millisecond

// Acquire takes the root's advisory fence or returns a context or filesystem error without a lease.
//
// Each call opens its own open file description of the retained lock file and takes a nonblocking exclusive flock
// on it, so two acquisitions exclude each other in one process as well as across processes. The descriptor is
// close-on-exec: a forked child shares it only until it execs, and no executed program retains it. The kernel gives
// the lock up when the last reference to that open file description goes away, so also when its holder dies. While
// another holder has the lock, Acquire waits on a timer and on ctx, never in a blocking system call, and it checks
// ctx before every attempt. A missing state root is created as the lifecycle lock creates it.
func (f *StartFence) Acquire(ctx context.Context) (release func() error, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := f.open()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(f.stateDir, fenceName)
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		// Checked before every attempt, also after a timer wake and after an interrupted call, so that an ended
		// context never takes the lock, not even for an instant.
		if err := ctx.Err(); err != nil {
			return nil, closeFence(err, fd, path)
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, closeFence(&fs.PathError{Op: "lock", Path: path, Err: err}, fd, path)
		}
		if timer == nil {
			timer = time.NewTimer(fenceRetry)
		} else {
			timer.Reset(fenceRetry)
		}
		select {
		case <-ctx.Done():
			return nil, closeFence(ctx.Err(), fd, path)
		case <-timer.C:
		}
	}
	release = sync.OnceValue(func() error { return releaseFence(fd, path) })
	// A context that ended while the lock was being taken gets no lease: the lock is given up before returning.
	if err := ctx.Err(); err != nil {
		if releaseErr := release(); releaseErr != nil {
			return nil, errors.Join(err, releaseErr)
		}
		return nil, err
	}
	return release, nil
}

// open opens a fresh open file description of the fence file, relative to the state root's directory, creating the
// file with mode 0600 if it is missing. A symbolic link at the leaf is never followed, an existing file is never
// truncated, and anything but a regular file is refused, judged by fstat of the opened descriptor.
func (f *StartFence) open() (int, error) {
	dir, err := atomicfile.OpenDirectoryFile(f.stateDir, 0o777)
	if err != nil {
		return -1, err
	}
	defer func() { _ = dir.Close() }()
	path := filepath.Join(f.stateDir, fenceName)
	var fd int
	for {
		// O_NONBLOCK keeps the open from waiting on a named pipe at the leaf; the fstat below refuses it.
		fd, err = unix.Openat(int(dir.Fd()), fenceName, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY, 0o600)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	runtime.KeepAlive(dir)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return -1, closeFence(&fs.PathError{Op: "fstat", Path: path, Err: err}, fd, path)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return -1, closeFence(fmt.Errorf("start fence %s is not a regular file", path), fd, path)
	}
	return fd, nil
}

// releaseFence unlocks the descriptor that holds the fence and then closes it, also when the unlock fails, so that
// the descriptor is closed exactly once. The unlock comes first because a forked child that has not yet called exec
// still shares the open file description, and the lock lasts until the last reference to it goes away; closing
// alone would leave the lock held until that child execs.
func releaseFence(fd int, path string) error {
	var unlockErr error
	if err := unlockFence(fd); err != nil {
		unlockErr = &fs.PathError{Op: "unlock", Path: path, Err: err}
	}
	return closeFence(unlockErr, fd, path)
}

// closeFence closes fd once and returns cause, joined with the close's error when the close fails, so that a close
// failure is reported and errors.Is still finds cause. The close is never retried: after a failed close the
// descriptor number may already have been reused by another open, which a second close would destroy.
func closeFence(cause error, fd int, path string) error {
	if err := unix.Close(fd); err != nil {
		return errors.Join(cause, &fs.PathError{Op: "close", Path: path, Err: err})
	}
	return cause
}

func unlockFenceDescriptor(fd int) error {
	for {
		err := unix.Flock(fd, unix.LOCK_UN)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
