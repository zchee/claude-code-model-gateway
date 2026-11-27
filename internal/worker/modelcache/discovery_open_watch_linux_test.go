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
	"errors"
	"slices"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// discoveryOpenWatch records which of a set of existing files were opened, through inotify IN_OPEN
// watches. The kernel queues the event during the open itself, also for an open that never reads, so
// a drain right after the opener returned sees it.
type discoveryOpenWatch struct {
	fd    int
	names map[int32]string
}

// watchDiscoveryInputOpens starts watching paths, which must exist, for opens. Linux can observe
// opens, so it always reports the observation as available.
func watchDiscoveryInputOpens(t *testing.T, paths ...string) (*discoveryOpenWatch, bool) {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatalf("inotify_init1: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	w := &discoveryOpenWatch{fd: fd, names: make(map[int32]string, len(paths))}
	for _, p := range paths {
		wd, err := unix.InotifyAddWatch(fd, p, unix.IN_OPEN)
		if err != nil {
			t.Fatalf("watch %s for opens: %v", p, err)
		}
		w.names[int32(wd)] = p
	}
	return w, true
}

// opened drains the queued events without waiting and returns the watched paths opened since the
// last drain, sorted and each once.
func (w *discoveryOpenWatch) opened(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
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
			if event.Mask&unix.IN_OPEN != 0 {
				if name, ok := w.names[event.Wd]; ok {
					seen[name] = true
				}
			}
			off += unix.SizeofInotifyEvent + int(event.Len)
		}
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
