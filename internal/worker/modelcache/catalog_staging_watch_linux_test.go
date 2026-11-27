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

//go:build linux

package modelcache_test

import (
	"bytes"
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// cacheDirWatch records the entries created, modified or moved into one directory, through a
// non-recursive inotify watch, as a reader that waits for a file to appear watches its directory.
// The kernel queues each event during the system call that causes it, so a drain right after the
// writer returned sees every event of the write.
type cacheDirWatch struct {
	fd int
}

// watchCacheDir starts watching dir, which must exist. Linux can observe the directory, so it always
// reports the observation as available.
func watchCacheDir(t *testing.T, dir string) (*cacheDirWatch, bool) {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatalf("inotify_init1: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	// Opens, reads and closes are left out: a writer may read the file it replaces first, which
	// names the final file without changing what a reader finds.
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_MODIFY|unix.IN_MOVED_TO); err != nil {
		t.Fatalf("watch %s: %v", dir, err)
	}
	return &cacheDirWatch{fd: fd}, true
}

// events drains the queued events without waiting, in the order the kernel queued them.
func (w *cacheDirWatch) events(t *testing.T) []cacheDirEvent {
	t.Helper()
	var out []cacheDirEvent
	buf := make([]byte, 64*(unix.SizeofInotifyEvent+unix.NAME_MAX+1))
	for {
		n, err := unix.Read(w.fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			return out
		}
		if err != nil {
			t.Fatalf("read inotify events: %v", err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			start := off + unix.SizeofInotifyEvent
			name := string(bytes.TrimRight(buf[start:start+int(event.Len)], "\x00"))
			off = start + int(event.Len)
			if event.Mask&unix.IN_Q_OVERFLOW != 0 {
				t.Fatal("the inotify queue overflowed: events of the write were lost")
			}
			op := "modify"
			switch {
			case event.Mask&unix.IN_MOVED_TO != 0:
				op = "rename into the directory"
			case event.Mask&unix.IN_CREATE != 0:
				op = "create"
			}
			out = append(out, cacheDirEvent{Op: op, Name: name, Dir: event.Mask&unix.IN_ISDIR != 0})
		}
	}
}
