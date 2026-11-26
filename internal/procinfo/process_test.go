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
	"slices"
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
	stdin, in, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	cmd.Stdin = stdin
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = cmd.Wait() })
	// Start can return before exec has published argv. An echo proves cat is running.
	deadline := time.Now().Add(5 * time.Second)
	for _, pipe := range []any{in, out} {
		f, ok := pipe.(*os.File)
		if !ok {
			t.Fatal("child readiness: pipe cannot set a deadline")
		}
		if err := f.SetDeadline(deadline); err != nil {
			t.Fatalf("child readiness: %v", err)
		}
	}
	const marker = '?'
	if n, err := in.Write([]byte{marker}); err != nil || n != 1 {
		t.Fatalf("child readiness: wrote %d bytes: %v", n, err)
	}
	var echo [1]byte
	if n, err := out.Read(echo[:]); err != nil || n != 1 || echo[0] != marker {
		t.Fatalf("child readiness: read %q (%d bytes): %v", echo[:n], n, err)
	}
	return cmd.Process.Pid
}

func listenerOn(t *testing.T, lc net.ListenConfig, host string, port int) *net.TCPListener {
	t.Helper()
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
	l := listenerOn(t, net.ListenConfig{}, "127.0.0.1", 0)
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
}

func TestNativeTable(t *testing.T) {
	pid := startChild(t, "/bin/cat")
	parent := os.Getppid()
	probe := Probe{}
	table, err := probe.Table(t.Context())
	if err != nil {
		t.Fatalf("enumerate native processes: %v", err)
	}
	if row, ok := table[parent]; !ok || row.PID != parent {
		t.Errorf("parent pid %d outside the test's descendants is not in the same snapshot", parent)
	}
	row, ok := table[pid]
	if !ok || row.PID != pid || row.ParentPID != os.Getpid() || row.Birth.IsZero() || !slices.Equal(row.Argv, []string{"/bin/cat", "-"}) {
		t.Errorf("own child pid %d missing or incomplete in the same snapshot: %+v", pid, row)
	}
	id, err := probe.Identity(t.Context(), pid)
	if err != nil {
		t.Fatalf("inspect table child %d: %v", pid, err)
	}
	if id.PID != pid || !row.Birth.Equal(id.Birth) {
		t.Errorf("table birth for pid %d = %v, identity for pid %d reports %v", pid, row.Birth, id.PID, id.Birth)
	}
}

func TestNativeBirthInLocalZone(t *testing.T) {
	const zone = "Asia/Tokyo"
	if os.Getenv("PROCINFO_TEST_BIRTH_ZONE") != zone {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestNativeBirthInLocalZone$")
		cmd.Env = append(cmd.Environ(), "TZ="+zone, "LC_ALL=C", "PROCINFO_TEST_BIRTH_ZONE="+zone)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("native non-UTC check: %v\n%s", err, out)
		}
		return
	}
	if _, offset := time.Now().Zone(); offset != 9*3600 {
		t.Fatalf("child has local offset %d, want Tokyo", offset)
	}
	pid := os.Getppid()
	cmd := exec.CommandContext(t.Context(), "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want, err := time.ParseInLocation(time.ANSIC, strings.TrimSpace(string(out)), time.Local)
	if err != nil {
		t.Fatalf("native ps birth %q: %v", out, err)
	}
	// ps reports whole seconds. A timestamp manufactured at inspection must not match it.
	if want.After(time.Now()) {
		t.Fatalf("ps reported a future parent birth: %s", want)
	}
	time.Sleep(time.Until(want.Add(time.Second)))
	id, err := (Probe{}).Identity(t.Context(), pid)
	if err != nil {
		t.Fatalf("native birth in %s: %v", zone, err)
	}
	if !id.Birth.Truncate(time.Second).Equal(want) {
		t.Errorf("native birth instant = %s, ps reports %s", id.Birth.UTC(), want.UTC())
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
