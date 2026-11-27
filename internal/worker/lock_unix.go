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

	"golang.org/x/sys/unix"
)

// flockSocketLock is the flock call lockReclaim makes on the lock file's descriptor. It is a variable so that a test
// can interrupt the attempt or fail it, which a nonblocking lock of a local file does not do on its own.
var flockSocketLock = func(fd, how int) error {
	panic("not implemented")
}

// lockReclaim takes an exclusive advisory lock on the regular file path+".lock", creating it, and returns its release
// with locked set. The lock file is never followed through a symbolic link and is left in place, so that every starter
// locks the same file. A Linux abstract address has no file that could be reclaimed, so it gets no lock file and
// lockReclaim reports locked false with no error. Any failure to take the lock is returned naming the lock file.
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
		err = unix.Flock(fd, unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, false, fmt.Errorf("model-gateway: could not take the socket lock %s: %w", name, err)
	}
	return func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	}, true, nil
}
