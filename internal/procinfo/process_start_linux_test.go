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
	"cmp"
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

// bootIDPath is where a proc root presents the kernel's boot identifier.
var bootIDPath = filepath.Join("sys", "kernel", "random", "boot_id")

// procSample is the kernel state a fake proc root presents for one process.
// An empty argv or exe presents the default program, worker run from /bin/cat.
type procSample struct {
	btime  int64
	bootID string
	ppid   int
	start  string
	argv   []string
	exe    string
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
		b.WriteByte(' ')
		b.WriteString(value)
	}
	b.WriteString("\n")
	return b.String()
}

// writeProcDir presents s for pid in dir, the way the kernel lays out one
// process directory.
func writeProcDir(t *testing.T, dir string, pid int, comm string, s procSample) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(dir, "stat"), statLine(pid, comm, s.ppid, s.start))
	argv := s.argv
	if len(argv) == 0 {
		argv = []string{"worker"}
	}
	putFile(t, filepath.Join(dir, "cmdline"), strings.Join(argv, "\x00")+"\x00")
	exe, target := filepath.Join(dir, "exe"), cmp.Or(s.exe, "/bin/cat")
	if got, err := os.Readlink(exe); err == nil && got == target {
		return
	}
	if err := os.Remove(exe); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, exe); err != nil {
		t.Fatal(err)
	}
}

// writeSample presents s for pid under root, the way the kernel lays out procfs.
func writeSample(t *testing.T, root string, pid int, comm string, s procSample) {
	t.Helper()
	random := filepath.Dir(filepath.Join(root, bootIDPath))
	if err := os.MkdirAll(random, 0o700); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(root, "stat"), fmt.Sprintf("cpu 1 2 3 4\nbtime %d\nprocesses 9\n", s.btime))
	putFile(t, filepath.Join(root, bootIDPath), s.bootID+"\n")
	writeProcDir(t, filepath.Join(root, strconv.Itoa(pid)), pid, comm, s)
}

// fakeProc returns a context whose probes read the fake proc root instead of /proc.
func fakeProc(t *testing.T, pid int, comm string, s procSample) (context.Context, string) {
	t.Helper()
	root := t.TempDir()
	writeSample(t, root, pid, comm, s)
	return context.WithValue(t.Context(), common.EnvKey, common.EnvMap{common.HostProcEnvKey: root}), root
}

// denyRead removes every permission from path and proves that this account can
// no longer read it; a privileged account would, and the case would test nothing.
func denyRead(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("fixture %s is readable after chmod 0: %v", path, err)
	}
}

func removePath(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

// tableRow samples pid through the process table of p.
func tableRow(p Probe) func(context.Context, int) (Identity, error) {
	return func(ctx context.Context, pid int) (Identity, error) {
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
}

// checkLinuxToken requires the boot identifier, one ASCII punctuation byte, and
// the decimal start ticks, in that order.
func checkLinuxToken(t *testing.T, token StartToken, bootID, start string) {
	t.Helper()
	rest, bootFirst := strings.CutPrefix(string(token), bootID)
	sep, startLast := strings.CutSuffix(rest, start)
	if !bootFirst || !startLast || !oneASCIIPunctuation(sep) {
		t.Errorf("token %q; want %q, one ASCII punctuation byte, then %q", token, bootID, start)
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
	execed := procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242", argv: []string{"/bin/sh", "-c", "exit 0"}, exe: "/bin/sh"}
	tests := map[string]struct {
		later procSample
		comm  string
		same  bool
	}{
		"success: later start under the same process number is another process": {later: procSample{btime: 1000, bootID: bootA, ppid: 1, start: "9999"}},
		"success: same start ticks after a reboot is another process":           {later: procSample{btime: 1000, bootID: bootB, ppid: 1, start: "4242"}},
		"success: wall clock set back keeps the token":                          {later: procSample{btime: 400, bootID: bootA, ppid: 1, start: "4242"}, same: true},
		"success: wall clock set forward keeps the token":                       {later: procSample{btime: 5000, bootID: bootA, ppid: 1, start: "4242"}, same: true},
		"success: reparented process keeps the token":                           {later: procSample{btime: 1000, bootID: bootA, ppid: 2, start: "4242"}, same: true},
		"success: exec of another program keeps the token":                      {later: execed, comm: "sh", same: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pid := os.Getpid()
			ctx, root := fakeProc(t, pid, "worker", base)
			p := Probe{}
			firstID, err := p.Identity(ctx, pid)
			if err != nil {
				t.Fatalf("first identity: %v", err)
			}
			first, err := readStartToken(t, ctx, p, pid)
			if err != nil {
				t.Fatalf("first token: %v", err)
			}
			writeSample(t, root, pid, cmp.Or(tt.comm, "worker"), tt.later)
			laterID, err := p.Identity(ctx, pid)
			if err != nil {
				t.Fatalf("later identity: %v", err)
			}
			// The birth moves with the boot wall clock; this proves the fixture changed what a clock step changes.
			if shift := time.Duration(tt.later.btime-base.btime) * time.Second; tt.later.start == base.start && laterID.Birth.Sub(firstID.Birth) != shift {
				t.Fatalf("birth moved by %s; want %s", laterID.Birth.Sub(firstID.Birth), shift)
			}
			// An exec replaces the arguments and the executable; this proves the fixture changed both.
			if tt.later.exe != "" && (gocmp.Equal(laterID.Argv, firstID.Argv) || laterID.Image.Image == firstID.Image.Image) {
				t.Fatalf("identity after exec %+v; want other arguments and executable than %+v", laterID, firstID)
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

func TestLinuxStartTokenFromItsInputsAlone(t *testing.T) {
	// The token is made of the boot identifier and the start field of stat, so
	// it must not depend on the arguments, the executable or the boot wall clock,
	// all of which the full identity sample needs.
	self := os.Getpid()
	dir := strconv.Itoa(self)
	denied := func(path string) func(*testing.T, string) {
		return func(t *testing.T, root string) { t.Helper(); denyRead(t, filepath.Join(root, path)) }
	}
	remove := func(paths ...string) func(*testing.T, string) {
		return func(t *testing.T, root string) {
			t.Helper()
			for _, path := range paths {
				removePath(t, filepath.Join(root, path))
			}
		}
	}
	cmdline, exe, clock := filepath.Join(dir, "cmdline"), filepath.Join(dir, "exe"), "stat"
	tests := map[string]struct {
		setup   func(*testing.T, string)
		noBirth bool
	}{
		"success: arguments are not readable":                             {setup: denied(cmdline)},
		"success: executable link is missing":                             {setup: remove(exe)},
		"success: boot wall clock is missing":                             {setup: remove(clock), noBirth: true},
		"success: only the boot identifier and the stat line are present": {setup: remove(cmdline, exe, clock), noBirth: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, root := fakeProc(t, self, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			p := Probe{}
			if _, err := p.Identity(ctx, self); err != nil {
				t.Fatalf("identity before the removal: %v", err)
			}
			tt.setup(t, root)
			// These prove the fixture took away what the full sample needs.
			if id, err := p.Identity(ctx, self); err == nil {
				t.Fatalf("identity without its inputs = %+v; want an error", id)
			}
			if _, err := p.Birth(ctx, self); (err != nil) != tt.noBirth {
				t.Fatalf("birth without its inputs: %v; want an error = %t", err, tt.noBirth)
			}
			token, err := readStartToken(t, ctx, p, self)
			if err != nil {
				t.Fatalf("StartToken(%d) = %q, %v; want the token from the boot identifier and stat", self, token, err)
			}
			checkLinuxToken(t, token, bootA, "4242")
		})
	}
}

func TestLinuxStartTokenUnreadable(t *testing.T) {
	self := os.Getpid()
	denied := func(path string) func(*testing.T, string) {
		return func(t *testing.T, root string) { t.Helper(); denyRead(t, filepath.Join(root, path)) }
	}
	replace := func(path, content string) func(*testing.T, string) {
		return func(t *testing.T, root string) { t.Helper(); putFile(t, filepath.Join(root, path), content) }
	}
	remove := func(path string) func(*testing.T, string) {
		return func(t *testing.T, root string) { t.Helper(); removePath(t, filepath.Join(root, path)) }
	}
	ownStat := filepath.Join(strconv.Itoa(self), "stat")
	full := statLine(self, "worker", 1, "4242")
	tests := map[string]struct {
		setup func(*testing.T, string)
	}{
		"error: stat is not readable":             {setup: denied(ownStat)},
		"error: boot identifier is not readable":  {setup: denied(bootIDPath)},
		"error: boot identifier is missing":       {setup: remove(bootIDPath)},
		"error: boot identifier is empty":         {setup: replace(bootIDPath, "\n")},
		"error: process directory is missing":     {setup: remove(strconv.Itoa(self))},
		"error: stat has no command terminator":   {setup: replace(ownStat, strings.Replace(full, ") ", " ", 1))},
		"error: stat ends before the start field": {setup: replace(ownStat, full[:strings.Index(full, " 4242")])},
		"error: start field is not a number":      {setup: replace(ownStat, statLine(self, "worker", 1, "x"))},
		"error: start field is negative":          {setup: replace(ownStat, statLine(self, "worker", 1, "-5"))},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, root := fakeProc(t, self, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			tt.setup(t, root)
			// This test process is alive, so unreadable evidence is never proof of absence.
			token, err := readStartToken(t, ctx, Probe{}, self)
			if err == nil || token != "" || errors.Is(err, os.ErrNotExist) {
				t.Errorf("StartToken(%d) = %q, %v; want an empty token and an error other than %v", self, token, err, os.ErrNotExist)
			}
		})
	}
}

func TestLinuxStartTokenAbsence(t *testing.T) {
	// The fake proc root holds no directory for pid, so only the existence probe
	// can tell a process that is gone from one whose evidence is out of reach.
	self := os.Getpid()
	pid := self + 1
	tests := map[string]struct {
		existence error
		absent    bool
	}{
		"error: a process that does not exist is confirmed absent":   {existence: unix.ESRCH, absent: true},
		"error: a live process without readable evidence is present": {existence: nil},
		"error: a live process of another user is present":           {existence: unix.EPERM},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, _ := fakeProc(t, self, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			p := Probe{existence: func(_ context.Context, got int) error {
				if got != pid {
					t.Errorf("existence probe asked about pid %d; want %d", got, pid)
				}
				return tt.existence
			}}
			token, err := readStartToken(t, ctx, p, pid)
			if err == nil || token != "" {
				t.Fatalf("StartToken(%d) = %q, %v; want an empty token and an error", pid, token, err)
			}
			// Absence is os.ErrNotExist alone; a present process gets any error but that one.
			if notExist := errors.Is(err, os.ErrNotExist); notExist != tt.absent || tt.absent && errors.Is(err, ErrUnknown) {
				t.Errorf("StartToken(%d) error %v; want confirmed absence (%v alone) = %t", pid, err, os.ErrNotExist, tt.absent)
			}
		})
	}
}

func TestLinuxMalformedStartSample(t *testing.T) {
	self := os.Getpid()
	neighbour := self + 1
	full := statLine(self, "worker", 1, "4242")
	tests := map[string]struct{ stat string }{
		"error: start field is not a number":      {stat: statLine(self, "worker", 1, "x")},
		"error: start field is negative":          {stat: statLine(self, "worker", 1, "-5")},
		"error: stat has no command terminator":   {stat: strings.Replace(full, ") ", " ", 1)},
		"error: stat ends before the start field": {stat: full[:strings.Index(full, " 4242")]},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, root := fakeProc(t, self, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			writeSample(t, root, neighbour, "neighbour", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "5151"})
			putFile(t, filepath.Join(root, strconv.Itoa(self), "stat"), tt.stat)
			p := Probe{}
			id, err := p.Identity(ctx, self)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Errorf("Identity(%d) error %v; want an error other than %v", self, err, os.ErrNotExist)
			}
			if diff := gocmp.Diff(Identity{}, id); diff != "" {
				t.Errorf("Identity(%d) returned a sample (-want +got): %s", self, diff)
			}
			table, err := p.Table(ctx)
			if err != nil {
				t.Fatalf("one malformed process failed the whole table: %v", err)
			}
			if row, ok := table[self]; ok {
				t.Errorf("table keeps the malformed process: %+v", row)
			}
			birth, err := p.Birth(ctx, neighbour)
			if err != nil {
				t.Fatalf("neighbour birth: %v", err)
			}
			row, ok := table[neighbour]
			if !ok {
				t.Fatalf("table %v lost the valid neighbour %d", table, neighbour)
			}
			checkLinuxToken(t, row.StartToken, bootA, "5151")
			row.StartToken = ""
			if diff := gocmp.Diff(Identity{PID: neighbour, ParentPID: 1, Argv: []string{"worker"}, Birth: birth}, row); diff != "" {
				t.Errorf("neighbour row (-want +got): %s", diff)
			}
		})
	}
}

func TestLinuxStartTokenFromIdentitySample(t *testing.T) {
	// A generous budget keeps the probe deadline out of the handshake below.
	p := New(func(key string) (string, bool) { return "30000", key == "CODEX_GATEWAY_PROBE_TIMEOUT_MS" })
	tests := map[string]struct {
		sample func(context.Context, int) (Identity, error)
	}{
		"success: identity":          {sample: p.Identity},
		"success: process table row": {sample: tableRow(p)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pid := os.Getpid()
			ctx, root := fakeProc(t, pid, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			want, err := tt.sample(ctx, pid)
			if err != nil {
				t.Fatalf("sample before the swap: %v", err)
			}
			// The replacement is another process under the same number, with other
			// start ticks, arguments and executable. It waits under a name that is
			// not a process number, so the table does not list it.
			dir := filepath.Join(root, strconv.Itoa(pid))
			replacement := filepath.Join(root, "replacement")
			writeProcDir(t, replacement, pid, "other", procSample{ppid: 2, start: "9999", argv: []string{"/bin/sh", "-c", "exit 0"}, exe: "/bin/sh"})
			// The first open of stat receives the original line through a FIFO. A
			// later open through the held directory finds a regular file whose start
			// ticks name another process, and a later open by path finds the replacement.
			stat := filepath.Join(dir, "stat")
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
			if err := os.Rename(dir, filepath.Join(root, "original")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, dir); err != nil {
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
			// The swap was in place: a read by path now finds the replacement.
			if line := readFile(t, stat); !strings.Contains(line, " (other) ") {
				t.Fatalf("stat by path after the swap is %q; want the replacement", line)
			}
			token := got.id.StartToken
			got.id.StartToken, want.StartToken = "", ""
			if diff := gocmp.Diff(want, got.id); diff != "" {
				t.Errorf("sample (-before the swap +during the swap): %s", diff)
			}
			checkLinuxToken(t, token, bootA, "4242")
		})
	}
}

func TestLinuxSampleWithoutBootID(t *testing.T) {
	// The token decides authority to signal, not whether a process exists, so a
	// missing boot identifier must not cost the caller the rest of the sample: the
	// identity is returned as before with an empty token, which never matches.
	p := Probe{}
	missing := func(t *testing.T, root string) { t.Helper(); removePath(t, filepath.Join(root, bootIDPath)) }
	empty := func(t *testing.T, root string) { t.Helper(); putFile(t, filepath.Join(root, bootIDPath), "\n") }
	denied := func(t *testing.T, root string) { t.Helper(); denyRead(t, filepath.Join(root, bootIDPath)) }
	tests := map[string]struct {
		setup  func(*testing.T, string)
		sample func(context.Context, int) (Identity, error)
	}{
		"success: identity without a boot identifier has an empty token":           {setup: missing, sample: p.Identity},
		"success: identity with an empty boot identifier has an empty token":       {setup: empty, sample: p.Identity},
		"success: identity with an unreadable boot identifier has an empty token":  {setup: denied, sample: p.Identity},
		"success: table row without a boot identifier has an empty token":          {setup: missing, sample: tableRow(p)},
		"success: table row with an empty boot identifier has an empty token":      {setup: empty, sample: tableRow(p)},
		"success: table row with an unreadable boot identifier has an empty token": {setup: denied, sample: tableRow(p)},
		"error: start token without a boot identifier is unreadable":               {setup: missing},
		"error: start token with an empty boot identifier is unreadable":           {setup: empty},
		"error: start token with an unreadable boot identifier is unreadable":      {setup: denied},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pid := os.Getpid()
			ctx, root := fakeProc(t, pid, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			if tt.sample == nil {
				tt.setup(t, root)
				// This test process is alive, so an unusable boot identifier is never proof of absence.
				token, err := readStartToken(t, ctx, p, pid)
				if err == nil || token != "" || errors.Is(err, os.ErrNotExist) {
					t.Errorf("StartToken(%d) = %q, %v; want an empty token and an error other than %v", pid, token, err, os.ErrNotExist)
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
