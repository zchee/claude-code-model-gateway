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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/shirou/gopsutil/v4/common"
)

func TestLinuxListenerWithoutLsof(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	p := Probe{}
	first := listenerOn(t, "127.0.0.1", 0)
	t.Cleanup(func() { _ = first.Close() })
	port := uint16(first.Addr().(*net.TCPAddr).Port)
	t.Run("identifiable", func(t *testing.T) {
		if got, want := p.Listener(t.Context(), port), (Listener{ListenerIdentified, os.Getpid()}); got != want {
			t.Errorf("listener without lsof = %+v, want %+v", got, want)
		}
	})
	t.Run("inaccessible", func(t *testing.T) {
		// Expose the real socket table but not the owner's file descriptors, as on a restricted procfs mount.
		proc := t.TempDir()
		if err := os.Symlink("/proc/self/net", filepath.Join(proc, "net")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(proc, strconv.Itoa(os.Getpid())), 0o700); err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(t.Context(), common.EnvKey, common.EnvMap{common.HostProcEnvKey: proc})
		got := p.Listener(ctx, port)
		if got.PID != 0 || (got.State != ListenerUnresolved && got.State != ListenerUnknown) {
			t.Errorf("bound listener without accessible owner = %+v, want unresolved or unknown and no pid", got)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		second := listenerOn(t, "127.0.0.2", int(port))
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
		if got := p.Listener(t.Context(), port); got != (Listener{State: ListenerAmbiguous}) {
			t.Errorf("listeners in pids %d and %d = %+v, want ambiguous and no pid", os.Getpid(), child, got)
		}
	})
}
