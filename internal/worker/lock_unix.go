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

package worker

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// flockSocketLock is the flock call lockReclaim makes on the lock file's descriptor. It is a variable so that a test
// can interrupt the attempt or fail it, which a nonblocking lock of a local file does not do on its own.
var flockSocketLock = unix.Flock

// lockReclaim takes an exclusive advisory lock on the regular file path+".lock", creating it, and returns its release
// with locked set. The lock file is never followed through a symbolic link and is left in place, also by the release,
// so that every starter locks the same file; a file left by a holder that died is locked again as it is, since the
// kernel drops a lock with the last descriptor of its holder. The attempt does not wait: a lock held through another
// descriptor, by a starter that is binding or by a listener that is not yet closed, gives an error whose first system
// error number is EADDRINUSE, the code a bind onto a live socket gives, and leaves the socket path untouched. Any other
// failure to take the lock is returned naming the lock file. A Linux abstract address has no file that could be
// reclaimed, so it gets no lock file and lockReclaim reports locked false with no error.
func lockReclaim(path string) (unlock func(), locked bool, err error) {
	// On the systems this program supports that have an abstract namespace, Linux and Android, Go binds a name that
	// starts with '@' or a NUL byte there. Elsewhere this code handles such a name as a path.
	if (runtime.GOOS == "linux" || runtime.GOOS == "android") && path != "" && (path[0] == '@' || path[0] == 0) {
		return nil, false, nil
	}
	name := path + ".lock"
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("model-gateway: could not open the socket lock %s: %w", name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("model-gateway: could not inspect the socket lock %s: %w", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("model-gateway: the socket lock %s is not a regular file", name)
	}
	for {
		err = flockSocketLock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	// flock reports a lock held through another open file description as EWOULDBLOCK when it may not wait.
	if errors.Is(err, unix.EWOULDBLOCK) {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("model-gateway: the socket %s is in use: its lock %s is held: %w", path, name, syscall.EADDRINUSE)
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("model-gateway: could not take the socket lock %s: %w", name, err)
	}
	// The explicit unlock releases the lock even while a child forked but not yet executed still shares the descriptor's
	// open file description; closing the descriptor alone would release it only once that child has executed.
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}, true, nil
}
