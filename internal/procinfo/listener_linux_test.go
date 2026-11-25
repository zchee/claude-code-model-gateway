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
	rand "math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/shirou/gopsutil/v4/common"
	"golang.org/x/sys/unix"
)

// restrictedProc exposes real network tables but only the specified process descriptors.
func restrictedProc(t *testing.T, visiblePIDs ...int) context.Context {
	t.Helper()
	proc := t.TempDir()
	if err := os.Symlink("/proc/self/net", filepath.Join(proc, "net")); err != nil {
		t.Fatal(err)
	}
	for _, pid := range visiblePIDs {
		name := strconv.Itoa(pid)
		if err := os.Mkdir(filepath.Join(proc, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("/proc", name, "fd"), filepath.Join(proc, name, "fd")); err != nil {
			t.Fatal(err)
		}
	}
	return context.WithValue(t.Context(), common.EnvKey, common.EnvMap{common.HostProcEnvKey: proc})
}

func TestLinuxListenerWithoutLsof(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var sockErr error
		if err := raw.Control(func(fd uintptr) { sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1) }); err != nil {
			return err
		}
		return sockErr
	}}
	p := Probe{}
	host := netip.AddrFrom4([4]byte{127, byte(1 + rand.IntN(254)), byte(rand.Uint32()), byte(1 + rand.IntN(254))}).String()
	// A plain bind cannot join another process's existing reuse-port group.
	reservation := listenerOn(t, net.ListenConfig{}, host, 0)
	port := uint16(reservation.Addr().(*net.TCPAddr).Port)
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	first := listenerOn(t, lc, host, int(port))
	t.Cleanup(func() { _ = first.Close() })
	t.Run("identifiable", func(t *testing.T) {
		if got, want := p.Listener(t.Context(), port), (Listener{ListenerIdentified, os.Getpid()}); got != want {
			t.Errorf("listener without lsof = %+v, want %+v", got, want)
		}
	})
	t.Run("inaccessible", func(t *testing.T) {
		if got := p.Listener(restrictedProc(t), port); got != (Listener{State: ListenerUnresolved}) {
			t.Errorf("visible listening socket without visible descriptors = %+v, want unresolved and no pid", got)
		}
	})
	t.Run("shared_descriptor", func(t *testing.T) {
		fd, err := first.File()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fd.Close() })
		child := startChild(t, "/bin/cat", fd)
		if got := p.Listener(t.Context(), port); got != (Listener{State: ListenerAmbiguous}) {
			t.Errorf("one socket held by pids %d and %d = %+v, want ambiguous without pid", os.Getpid(), child, got)
		}
	})
	tests := map[string]struct{ restrict bool }{"ambiguous": {}, "identified_plus_unresolved": {restrict: true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Two sockets have exactly the same local and remote endpoints, but distinct inodes.
			second := listenerOn(t, lc, host, int(port))
			t.Cleanup(func() { _ = second.Close() })
			fd, err := second.File()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fd.Close() })
			child := startChild(t, "/bin/cat", fd)
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fd.Close(); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if tt.restrict {
				ctx = restrictedProc(t, os.Getpid())
			}
			if got := p.Listener(ctx, port); got != (Listener{State: ListenerAmbiguous}) {
				t.Errorf("two sockets held by pids %d and %d (restricted=%t) = %+v, want ambiguous without pid", os.Getpid(), child, tt.restrict, got)
			}
		})
	}
}
