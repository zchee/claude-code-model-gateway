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

package procinfo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/shirou/gopsutil/v4/common"
	"golang.org/x/sys/unix"
)

const (
	bootA = "5b0b2a8e-7a43-4b51-9a8c-2f0d6c1e9b3d"
	bootB = "c41d0e7f-08a2-4f6b-b1c3-9e5a7d2f4c60"
)

// procSample is the kernel state a fake proc root presents for one process.
type procSample struct {
	btime  int64
	bootID string
	ppid   int
	start  string
}

// statLine builds a /proc/<pid>/stat line in which every numeric field after the
// command differs, so a value taken from a neighbouring column changes the result.
func statLine(pid int, comm string, ppid int, start string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d (%s) S %d", pid, comm, ppid)
	for field := 5; field <= 52; field++ {
		value := strconv.Itoa(field*1000 + field)
		if field == 22 {
			value = start
		}
		b.WriteString(" " + value)
	}
	b.WriteString("\n")
	return b.String()
}

// writeSample presents s for pid under root, the way the kernel lays out procfs.
func writeSample(t *testing.T, root string, pid int, comm string, s procSample) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	random := filepath.Join(root, "sys", "kernel", "random")
	for _, d := range []string{dir, random} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	putFile(t, filepath.Join(root, "stat"), fmt.Sprintf("cpu 1 2 3 4\nbtime %d\nprocesses 9\n", s.btime))
	putFile(t, filepath.Join(random, "boot_id"), s.bootID+"\n")
	putFile(t, filepath.Join(dir, "stat"), statLine(pid, comm, s.ppid, s.start))
	putFile(t, filepath.Join(dir, "cmdline"), "worker\x00")
	exe := filepath.Join(dir, "exe")
	if _, err := os.Lstat(exe); errors.Is(err, os.ErrNotExist) {
		if err := os.Symlink("/bin/cat", exe); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeProc returns a context whose probes read the fake proc root instead of /proc.
func fakeProc(t *testing.T, pid int, comm string, s procSample) (context.Context, string) {
	t.Helper()
	root := t.TempDir()
	writeSample(t, root, pid, comm, s)
	return context.WithValue(t.Context(), common.EnvKey, common.EnvMap{common.HostProcEnvKey: root}), root
}

// checkLinuxToken requires the boot identifier, one separator byte that cannot
// occur in either part, and the decimal start ticks, in that order.
func checkLinuxToken(t *testing.T, token StartToken, bootID, start string) {
	t.Helper()
	rest, bootFirst := strings.CutPrefix(string(token), bootID)
	sep, startLast := strings.CutSuffix(rest, start)
	if !bootFirst || !startLast || len(sep) != 1 || !strings.Contains("!#$%&*+,./:;<=>?@^_|~", sep) {
		t.Errorf("token %q; want %q, one punctuation separator other than '-', then %q", token, bootID, start)
	}
}

func TestLinuxStartTokenFormat(t *testing.T) {
	tests := map[string]struct{ comm string }{
		"success: plain command name":                              {comm: "worker"},
		"success: command name with spaces and parentheses":        {comm: "a) (b c) d"},
		"success: command name ending in a parenthesis":            {comm: "x)"},
		"success: command name that imitates the following fields": {comm: "a) R 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pid := os.Getpid()
			ctx, _ := fakeProc(t, pid, tt.comm, procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			p := Probe{}
			if _, err := p.Birth(ctx, pid); err != nil {
				t.Fatalf("fixture stat line is not readable as a birth: %v", err)
			}
			first, err := readStartToken(t, ctx, p, pid)
			if err != nil {
				t.Fatalf("first read: %v", err)
			}
			second, err := readStartToken(t, ctx, p, pid)
			if err != nil {
				t.Fatalf("second read: %v", err)
			}
			if first != second {
				t.Errorf("two reads of one process: %q then %q", first, second)
			}
			checkLinuxToken(t, first, bootA, "4242")
		})
	}
}

func TestLinuxStartTokenReuse(t *testing.T) {
	base := procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"}
	tests := map[string]struct {
		later procSample
		same  bool
	}{
		"success: later start under the same process number is another process": {later: procSample{btime: 1000, bootID: bootA, ppid: 1, start: "9999"}},
		"success: same start ticks after a reboot is another process":           {later: procSample{btime: 1000, bootID: bootB, ppid: 1, start: "4242"}},
		"success: wall clock set back keeps the token":                          {later: procSample{btime: 400, bootID: bootA, ppid: 1, start: "4242"}, same: true},
		"success: wall clock set forward keeps the token":                       {later: procSample{btime: 5000, bootID: bootA, ppid: 1, start: "4242"}, same: true},
		"success: reparented process keeps the token":                           {later: procSample{btime: 1000, bootID: bootA, ppid: 2, start: "4242"}, same: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pid := os.Getpid()
			ctx, root := fakeProc(t, pid, "worker", base)
			p := Probe{}
			firstBirth, err := p.Birth(ctx, pid)
			if err != nil {
				t.Fatalf("first birth: %v", err)
			}
			first, err := readStartToken(t, ctx, p, pid)
			if err != nil {
				t.Fatalf("first token: %v", err)
			}
			writeSample(t, root, pid, "worker", tt.later)
			laterBirth, err := p.Birth(ctx, pid)
			if err != nil {
				t.Fatalf("later birth: %v", err)
			}
			// The birth moves with the boot wall clock; this proves the fixture changed what a clock step changes.
			if shift := time.Duration(tt.later.btime-base.btime) * time.Second; tt.later.start == base.start && laterBirth.Sub(firstBirth) != shift {
				t.Fatalf("birth moved by %s; want %s", laterBirth.Sub(firstBirth), shift)
			}
			later, err := readStartToken(t, ctx, p, pid)
			if err != nil {
				t.Fatalf("later token: %v", err)
			}
			if first == "" || later == "" || (first == later) != tt.same {
				t.Errorf("tokens %q then %q; want same=%t", first, later, tt.same)
			}
			checkLinuxToken(t, later, tt.later.bootID, tt.later.start)
		})
	}
}

func TestLinuxStartTokenUnreadable(t *testing.T) {
	self := os.Getpid()
	denied := func(path string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			t.Helper()
			path := filepath.Join(root, path)
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
			// A privileged account would read the file anyway; then this case tests nothing.
			if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
				t.Fatalf("fixture %s is readable after chmod 0: %v", path, err)
			}
		}
	}
	replace := func(path, content string) func(*testing.T, string) {
		return func(t *testing.T, root string) { t.Helper(); putFile(t, filepath.Join(root, path), content) }
	}
	ownStat := filepath.Join(strconv.Itoa(self), "stat")
	bootID := filepath.Join("sys", "kernel", "random", "boot_id")
	full := statLine(self, "worker", 1, "4242")
	tests := map[string]struct {
		pid   int
		setup func(*testing.T, string)
	}{
		"error: stat is not readable":             {pid: self, setup: denied(ownStat)},
		"error: boot identifier is not readable":  {pid: self, setup: denied(bootID)},
		"error: boot identifier is missing":       {pid: self, setup: func(t *testing.T, root string) { t.Helper(); removePath(t, filepath.Join(root, bootID)) }},
		"error: boot identifier is empty":         {pid: self, setup: replace(bootID, "\n")},
		"error: process directory is missing":     {pid: self, setup: func(t *testing.T, root string) { t.Helper(); removePath(t, filepath.Join(root, strconv.Itoa(self))) }},
		"error: stat has no command terminator":   {pid: self, setup: replace(ownStat, strings.Replace(full, ") ", " ", 1))},
		"error: stat ends before the start field": {pid: self, setup: replace(ownStat, full[:strings.Index(full, " 4242")])},
		"error: start field is not a number":      {pid: self, setup: replace(ownStat, statLine(self, "worker", 1, "x"))},
		"error: start field is negative":          {pid: self, setup: replace(ownStat, statLine(self, "worker", 1, "-5"))},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, root := fakeProc(t, self, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			tt.setup(t, root)
			token, err := readStartToken(t, ctx, Probe{}, tt.pid)
			if err == nil || token != "" {
				t.Errorf("StartToken(%d) = %q, %v; want an empty token and an error", tt.pid, token, err)
			}
		})
	}
}

func removePath(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxStartTokenFromIdentitySample(t *testing.T) {
	// A generous budget keeps the probe deadline out of the handshake below.
	p := New(func(key string) (string, bool) { return "30000", key == "CODEX_GATEWAY_PROBE_TIMEOUT_MS" })
	tests := map[string]struct {
		sample func(context.Context, int) (Identity, error)
	}{
		"success: identity": {sample: p.Identity},
		"success: process table row": {sample: func(ctx context.Context, pid int) (Identity, error) {
			table, err := p.Table(ctx)
			if err != nil {
				return Identity{}, err
			}
			row, ok := table[pid]
			if !ok {
				return Identity{}, fmt.Errorf("pid %d missing from table %v", pid, table)
			}
			return row, nil
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pid := os.Getpid()
			ctx, root := fakeProc(t, pid, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			wantBirth, err := p.Birth(ctx, pid)
			if err != nil {
				t.Fatal(err)
			}
			// The first open of stat receives the original line through a FIFO; any
			// later open finds a regular file whose start ticks name another process.
			stat := filepath.Join(root, strconv.Itoa(pid), "stat")
			removePath(t, stat)
			if err := unix.Mkfifo(stat, 0o600); err != nil {
				t.Fatal(err)
			}
			putFile(t, stat+".new", statLine(pid, "worker", 1, "9999"))
			type result struct {
				id  Identity
				err error
			}
			sampled := make(chan result, 1)
			go func() { id, err := tt.sample(ctx, pid); sampled <- result{id, err} }()
			wait, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var fd int
			for {
				fd, err = unix.Open(stat, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				if err == nil {
					break
				}
				if !errors.Is(err, unix.ENXIO) {
					t.Fatal(err)
				}
				select {
				case got := <-sampled:
					t.Fatalf("sample finished without opening stat: %+v", got)
				case <-wait.Done():
					t.Fatal("sample did not open stat")
				case <-time.After(time.Millisecond):
				}
			}
			first := os.NewFile(uintptr(fd), stat)
			defer func() { _ = first.Close() }()
			if err := os.Rename(stat+".new", stat); err != nil {
				t.Fatal(err)
			}
			if _, err := first.WriteString(statLine(pid, "worker", 1, "4242")); err != nil {
				t.Fatal(err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			var got result
			select {
			case got = <-sampled:
			case <-wait.Done():
				t.Fatal("sample blocked after the first stat read")
			}
			if got.err != nil {
				t.Fatalf("sample: %v", got.err)
			}
			if !got.id.Birth.Equal(wantBirth) {
				t.Fatalf("birth %s; want %s from the first stat line", got.id.Birth, wantBirth)
			}
			checkLinuxToken(t, got.id.StartToken, bootA, "4242")
		})
	}
}

func TestLinuxSampleWithoutBootID(t *testing.T) {
	// The token decides authority to signal, not whether a process exists, so a
	// missing boot identifier must not cost the caller the rest of the sample: the
	// identity is returned as before with an empty token, which never matches.
	p := Probe{}
	tableRow := func(ctx context.Context, pid int) (Identity, error) {
		table, err := p.Table(ctx)
		if err != nil {
			return Identity{}, err
		}
		row, ok := table[pid]
		if !ok {
			return Identity{}, fmt.Errorf("pid %d missing from table %v", pid, table)
		}
		return row, nil
	}
	bootID := filepath.Join("sys", "kernel", "random", "boot_id")
	missing := func(t *testing.T, root string) { t.Helper(); removePath(t, filepath.Join(root, bootID)) }
	empty := func(t *testing.T, root string) { t.Helper(); putFile(t, filepath.Join(root, bootID), "\n") }
	tests := map[string]struct {
		setup  func(*testing.T, string)
		sample func(context.Context, int) (Identity, error)
	}{
		"success: identity without a boot identifier has an empty token":      {setup: missing, sample: p.Identity},
		"success: identity with an empty boot identifier has an empty token":  {setup: empty, sample: p.Identity},
		"success: table row without a boot identifier has an empty token":     {setup: missing, sample: tableRow},
		"success: table row with an empty boot identifier has an empty token": {setup: empty, sample: tableRow},
		"error: start token without a boot identifier is unreadable":          {setup: missing},
		"error: start token with an empty boot identifier is unreadable":      {setup: empty},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pid := os.Getpid()
			ctx, root := fakeProc(t, pid, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			if tt.sample == nil {
				tt.setup(t, root)
				token, err := readStartToken(t, ctx, p, pid)
				if err == nil || token != "" {
					t.Errorf("StartToken(%d) = %q, %v; want an empty token and an error", pid, token, err)
				}
				return
			}
			want, err := tt.sample(ctx, pid)
			if err != nil {
				t.Fatalf("sample with a boot identifier: %v", err)
			}
			want.StartToken = ""
			tt.setup(t, root)
			got, err := tt.sample(ctx, pid)
			if err != nil {
				t.Fatalf("sample without a usable boot identifier failed: %v", err)
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("sample (-want +got): %s", diff)
			}
		})
	}
}
