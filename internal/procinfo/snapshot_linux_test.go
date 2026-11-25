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

//go:build linux && process

package procinfo

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOnlyVisibleHolder(t *testing.T) {
	l := listenerOn(t, net.ListenConfig{}, "127.0.0.1", 0)
	defer func() { _ = l.Close() }()
	fd, err := l.File()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fd.Close() }()
	child := startChild(t, "/bin/cat", fd)
	ctx := restrictedProc(t, os.Getpid())
	view := filepath.Join(procRoot(ctx), strconv.Itoa(child), "fd")
	if err := os.MkdirAll(view, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(view, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(view, 0o700) })
	if got := (Probe{}).Listener(ctx, uint16(l.Addr().(*net.TCPAddr).Port)); got != (Listener{ListenerIdentified, os.Getpid()}) {
		t.Fatalf("sole visible holder: %+v", got)
	}
}

func TestChangingArgv(t *testing.T) {
	tests := map[string]struct {
		second string
		want   error
	}{"success: identical samples": {second: "worker\x00"}, "error: arguments changed": {second: "other\x00", want: ErrUnknown}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "713")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			putFile(t, filepath.Join(dir, "stat"), "713 (test) S 1"+strings.Repeat(" 0", 17)+" 1")
			if err := os.Symlink("/bin/cat", filepath.Join(dir, "exe")); err != nil {
				t.Fatal(err)
			}
			cmd := filepath.Join(dir, "cmdline")
			if err := unix.Mkfifo(cmd, 0o600); err != nil {
				t.Fatal(err)
			}
			putFile(t, cmd+".new", tt.second)
			sampled := make(chan error, 1)
			go func() { _, err := (procClock{boot: 1, ticks: 100}).inspect(root, 713, true); sampled <- err }()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			var fd int
			var err error
			for {
				fd, err = unix.Open(cmd, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				if err == nil {
					break
				}
				if !errors.Is(err, unix.ENXIO) {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatal("reader did not open first sample")
				case <-time.After(time.Millisecond):
				}
			}
			first := os.NewFile(uintptr(fd), cmd)
			defer func() { _ = first.Close() }()
			if err := os.Rename(cmd+".new", cmd); err != nil {
				t.Fatal(err)
			}
			if _, err := first.WriteString("worker\x00"); err != nil {
				t.Fatal(err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-sampled:
				if !errors.Is(err, tt.want) {
					t.Fatalf("inspection: %v, want %v", err, tt.want)
				}
			case <-ctx.Done():
				t.Fatal("inspection blocked reading second sample")
			}
		})
	}
}

func TestSocketSnapshotChange(t *testing.T) {
	tests := map[string]struct {
		stop bool
		want ListenerState
	}{"success: vanished before descriptor scan completed": {want: ListenerAbsent}, "error: canceled before a consistent sample": {stop: true, want: ListenerUnknown}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads, changes := 0, 0
			rows, err := stableSocketHolders(ctx, root, 27000, func() {
				changes++
				if tt.stop {
					cancel()
				}
			}, func(string, uint16) ([]uint64, error) {
				reads++
				if reads == 1 {
					return []uint64{100}, nil
				}
				return nil, nil
			})
			if changes != 1 || classifyListener(rows, err, 27000).State != tt.want || tt.stop && !errors.Is(err, context.Canceled) {
				t.Fatalf("changes=%d reads=%d rows=%v err=%v", changes, reads, rows, err)
			}
		})
	}
}
