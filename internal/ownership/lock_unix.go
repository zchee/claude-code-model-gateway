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

package ownership

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

// recordLockRetry is the pause between two attempts on a record lock another holder has. Every holder keeps it only
// for a few file operations, so a short pause keeps a waiting operation prompt.
const recordLockRetry = 5 * time.Millisecond

// lockRecords takes the exclusive advisory lock of the record lock file in dir, the held state root whose path is
// root, and returns the function that gives it up. The file is opened relative to dir, created with mode 0600 when it
// is missing, never truncated and never removed; a symbolic link at the leaf is not followed and anything but a
// regular file is refused, judged by fstat of the opened descriptor.
//
// Each call opens its own open file description, so two holders exclude each other within one process as well as
// across processes. The descriptor is close-on-exec, so no executed child keeps the lock. The lock is only ever
// tried without blocking: when another holder has it and wait is false, the call fails at once with
// errRecordsBusy; when wait is true, it pauses on a timer and on ctx between attempts, and ctx is checked before
// every attempt and again once the lock is held, so an ended context never gets a lock. A wait that ctx ends is an
// error matching errLockWait and ctx's error. Neither errRecordsBusy nor errLockWait comes after anything was read or
// changed under the lock.
func lockRecords(ctx context.Context, dir *atomicfile.Dir, root string, wait bool) (release func() error, err error) {
	if ctx.Err() != nil {
		return nil, lockWaitEnded(ctx)
	}
	path := filepath.Join(root, recordLockName)
	file, err := openLockFile(dir, path)
	if err != nil {
		return nil, err
	}
	// file stays referenced by the closures below until the release closes it, so its descriptor stays valid.
	fd := int(file.Fd())
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if ctx.Err() != nil {
			return nil, closeLock(lockWaitEnded(ctx), file, path)
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, closeLock(&fs.PathError{Op: "lock", Path: path, Err: err}, file, path)
		}
		if !wait {
			return nil, closeLock(errRecordsBusy, file, path)
		}
		if timer == nil {
			timer = time.NewTimer(recordLockRetry)
		} else {
			timer.Reset(recordLockRetry)
		}
		select {
		case <-ctx.Done():
			return nil, closeLock(lockWaitEnded(ctx), file, path)
		case <-timer.C:
		}
	}
	release = sync.OnceValue(func() error {
		// Unlocking first gives the lock up even while a forked child that has not yet executed shares the open file
		// description; closing alone would keep it until that child execs.
		var cause error
		for {
			err := unix.Flock(fd, unix.LOCK_UN)
			if !errors.Is(err, unix.EINTR) {
				if err != nil {
					cause = &fs.PathError{Op: "unlock", Path: path, Err: err}
				}
				break
			}
		}
		return closeLock(cause, file, path)
	})
	if ctx.Err() != nil {
		return nil, errors.Join(lockWaitEnded(ctx), release())
	}
	return release, nil
}

func openLockFile(dir *atomicfile.Dir, path string) (*os.File, error) {
	// O_NONBLOCK keeps the open from waiting on a named pipe at the leaf; the fstat below refuses it. The directory adds
	// O_NOFOLLOW and O_CLOEXEC.
	var file *os.File
	var err error
	for {
		file, err = dir.OpenFile(recordLockName, os.O_RDWR|os.O_CREATE|unix.O_NONBLOCK|unix.O_NOCTTY, 0o600)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	for {
		err = unix.Fstat(int(file.Fd()), &st)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, closeLock(&fs.PathError{Op: "fstat", Path: path, Err: err}, file, path)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, closeLock(fmt.Errorf("record lock %s is not a regular file", path), file, path)
	}
	return file, nil
}

// closeLock closes file once and returns cause joined with the close's error. The close is never retried: after a
// failed close the descriptor number may already belong to another open file.
func closeLock(cause error, file *os.File, path string) error {
	if err := file.Close(); err != nil {
		return errors.Join(cause, &fs.PathError{Op: "close", Path: path, Err: err})
	}
	return cause
}
