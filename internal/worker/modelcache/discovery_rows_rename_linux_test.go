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

// discTRenameWatch watches a staging directory for names renamed out of it and a destination
// directory for names renamed into it, on one inotify instance, so the two halves of one rename
// carry the same cookie. watchCacheDir reports neither the cookie nor the staging directory.
type discTRenameWatch struct {
	fd            int
	staging, dest int
}

// discTWatchRenames starts watching staging and dest, which must exist. Linux can observe
// renames, so it always reports the observation as available.
func discTWatchRenames(t *testing.T, staging, dest string) (*discTRenameWatch, bool) {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatalf("inotify_init1: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	w := &discTRenameWatch{fd: fd}
	if w.staging, err = unix.InotifyAddWatch(fd, staging, unix.IN_MOVED_FROM); err != nil {
		t.Fatalf("watch %s: %v", staging, err)
	}
	if w.dest, err = unix.InotifyAddWatch(fd, dest, unix.IN_MOVED_TO); err != nil {
		t.Fatalf("watch %s: %v", dest, err)
	}
	return w, true
}

// moves drains the queued events without waiting and pairs them by cookie, in the order the
// kernel queued each rename's first half.
func (w *discTRenameWatch) moves(t *testing.T) []discTMove {
	t.Helper()
	var order []uint32
	byCookie := map[uint32]*discTMove{}
	buf := make([]byte, 64*(unix.SizeofInotifyEvent+unix.NAME_MAX+1))
	for {
		n, err := unix.Read(w.fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			break
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
				t.Fatal("the inotify queue overflowed: renames were lost")
			}
			move, ok := byCookie[event.Cookie]
			if !ok {
				move = &discTMove{}
				byCookie[event.Cookie] = move
				order = append(order, event.Cookie)
			}
			switch {
			case int(event.Wd) == w.staging && event.Mask&unix.IN_MOVED_FROM != 0:
				move.From = name
			case int(event.Wd) == w.dest && event.Mask&unix.IN_MOVED_TO != 0:
				move.To = name
			}
		}
	}
	out := make([]discTMove, 0, len(order))
	for _, cookie := range order {
		out = append(out, *byCookie[cookie])
	}
	return out
}
