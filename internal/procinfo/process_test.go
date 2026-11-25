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

//go:build process && (linux || darwin)

package procinfo

import (
	"errors"
	rand "math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

// startChild keeps the executable alive on a private stdin pipe until cleanup.
func startChild(t *testing.T, path string, files ...*os.File) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), path, "-")
	cmd.ExtraFiles = files
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func listenerOn(t *testing.T, host string, port int) *net.TCPListener {
	t.Helper()
	var lc net.ListenConfig
	start := rand.IntN(40001)
	for i := range 32 {
		candidate := port
		if candidate == 0 {
			candidate = 20000 + (start+i)%40001
		}
		l, err := lc.Listen(t.Context(), "tcp4", net.JoinHostPort(host, strconv.Itoa(candidate)))
		if port == 0 && errors.Is(err, syscall.EADDRINUSE) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		return l.(*net.TCPListener)
	}
	t.Fatal("could not reserve a loopback port")
	return nil
}

func TestProcessAlive(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/cat")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		pid  int
		want bool
	}{
		"running": {os.Getpid(), true},
		"exited":  {pid: cmd.Process.Pid},
		"zero":    {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := (Probe{}).Alive(t.Context(), tt.pid)
			if err != nil || got != tt.want {
				t.Errorf("Alive(%d) = %t, %v; want %t, nil", tt.pid, got, err, tt.want)
			}
		})
	}
}

func TestLocalListener(t *testing.T) {
	l := listenerOn(t, "127.0.0.1", 0)
	t.Cleanup(func() { _ = l.Close() })
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	want := Listener{ListenerIdentified, os.Getpid()}
	if diff := gocmp.Diff(want, (Probe{}).Listener(t.Context(), port)); diff != "" {
		t.Errorf("loopback listener (-want +got): %s", diff)
	}
}

func TestNativeIdentity(t *testing.T) {
	pid := startChild(t, "/bin/cat")
	p := Probe{}
	id, err := p.Identity(t.Context(), pid)
	if err != nil {
		t.Fatalf("inspect child: %v", err)
	}
	if id.PID != pid || id.ParentPID != os.Getpid() {
		t.Errorf("pid/parent = %d/%d, want %d/%d", id.PID, id.ParentPID, pid, os.Getpid())
	}
	if diff := gocmp.Diff([]string{"/bin/cat", "-"}, id.Argv); diff != "" {
		t.Errorf("argv (-want +got): %s", diff)
	}
	cmd := exec.CommandContext(t.Context(), "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = []string{"LC_ALL=C", "TZ=Asia/Tokyo", "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := LegacyStart(id.Birth.In(loc)), strings.TrimSpace(string(out)); got != want {
		t.Errorf("non-UTC native birth = %q, ps reports %q", got, want)
	}
	table, err := p.Table(t.Context())
	if err != nil {
		t.Fatalf("enumerate native processes: %v", err)
	}
	row, ok := table[pid]
	if !ok || row.ParentPID != os.Getpid() || !row.Birth.Equal(id.Birth) {
		t.Errorf("child table evidence inconsistent with identity; present=%t parent=%d birth=%v", ok, row.ParentPID, row.Birth)
	}
}

func TestRunningImage(t *testing.T) {
	tests := map[string]struct{ change func(string) error }{
		"renamed":  {func(path string) error { return os.Rename(path, path+".old") }},
		"unlinked": {os.Remove},
		"replaced": {func(path string) error {
			if err := os.WriteFile(path+".new", []byte("replacement"), 0o700); err != nil {
				return err
			}
			return os.Rename(path+".new", path)
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-cat")
			contents, err := os.ReadFile("/bin/cat")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, contents, 0o700); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			stat := info.Sys().(*syscall.Stat_t)
			pid := startChild(t, path)
			if err := tt.change(path); err != nil {
				t.Fatal(err)
			}
			got, err := (Probe{}).Identity(t.Context(), pid)
			if err != nil {
				t.Fatalf("inspect executing image after %s: %v", name, err)
			}
			if got.Image.Device != uint64(stat.Dev) || got.Image.Inode != stat.Ino {
				t.Errorf("running image device/inode = %d/%d, want original %d/%d", got.Image.Device, got.Image.Inode, stat.Dev, stat.Ino)
			}
		})
	}
}

func TestSpawnWithoutRecord(t *testing.T) {
	root := t.TempDir()
	pid := startChild(t, "/bin/cat")
	record, err := ReadRecord(root, "guardian")
	if !errors.Is(err, os.ErrNotExist) || record != (Record{}) {
		t.Errorf("after spawning pid %d but before publication: record=%+v err=%v; want missing record", pid, record, err)
	}
	for _, suffix := range []string{".pid.json", ".pid"} {
		if _, err := os.Stat(filepath.Join(root, "guardian"+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("read manufactured %s evidence: %v", suffix, err)
		}
	}
}
