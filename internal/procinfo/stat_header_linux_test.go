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
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// statHeaderBirth restates how the kernel's start ticks become a wall-clock
// birth: btime seconds plus start ticks at the clock rate the kernel hands this
// process in its auxiliary vector. It is read here, apart from the probe.
func statHeaderBirth(t *testing.T, btime, start int64) time.Time {
	t.Helper()
	aux, err := unix.Auxv()
	if err != nil {
		t.Fatalf("auxiliary vector: %v", err)
	}
	var ticks int64
	for _, entry := range aux {
		if entry[0] == 17 { // AT_CLKTCK
			ticks = int64(entry[1])
		}
	}
	if ticks <= 0 {
		t.Fatalf("auxiliary vector has no clock rate: %v", aux)
	}
	return time.Unix(btime, 0).Add(time.Duration(start) * time.Second / time.Duration(ticks))
}

// statHeaderImage restates the executable evidence of a fake process whose exe
// link points at target, from the file system rather than from the probe.
func statHeaderImage(t *testing.T, target string) ImageEvidence {
	t.Helper()
	path, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatalf("resolve %s: %v", target, err)
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return ImageEvidence{Path: path, Device: uint64(st.Dev), Inode: st.Ino, DirectFromExecutable: true}
}

func TestLinuxStatRecordHeader(t *testing.T) {
	// A stat record speaks about a process only when it opens with that
	// process's decimal number, one space and an opening parenthesis. Each
	// record below keeps the valid tail of statLine and changes only the
	// header, so every field after the last closing parenthesis still parses.
	self := os.Getpid()
	neighbour := self + 1
	pid := strconv.Itoa(self)
	tail := "worker" + statLine(self, "worker", 1, "4242")[len(pid)+len(" (worker"):]
	tests := map[string]struct{ stat string }{
		"error: record names the next process number":             {stat: strconv.Itoa(neighbour) + " (" + tail},
		"error: record names the previous process number":         {stat: strconv.Itoa(self-1) + " (" + tail},
		"error: record names a number that has the pid as prefix": {stat: pid + "0 (" + tail},
		"error: record names a number that has the pid as suffix": {stat: "1" + pid + " (" + tail},
		"error: number has a leading zero":                        {stat: "0" + pid + " (" + tail},
		"error: number has two leading zeros":                     {stat: "00" + pid + " (" + tail},
		"error: record has no process number":                     {stat: "(" + tail},
		"error: number is followed by the command after a space":  {stat: pid + " " + tail},
		"error: number is followed directly by the command":       {stat: pid + tail},
		"error: number is followed directly by the parenthesis":   {stat: pid + "(" + tail},
		"error: number is followed by two spaces":                 {stat: pid + "  (" + tail},
		"error: record starts with a space":                       {stat: " " + pid + " (" + tail},
		"error: record starts with a tab":                         {stat: "\t" + pid + " (" + tail},
		"error: record starts with a plus sign":                   {stat: "+" + pid + " (" + tail},
		"error: record starts with a minus sign":                  {stat: "-" + pid + " (" + tail},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, root := fakeProc(t, self, "worker", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			writeSample(t, root, neighbour, "neighbour", procSample{btime: 1000, bootID: bootA, ppid: 1, start: "5151"})
			p := Probe{}
			// These prove the fixture is valid before its header is changed, so
			// each failure below comes from the header alone.
			if _, err := p.Identity(ctx, self); err != nil {
				t.Fatalf("identity of the valid record: %v", err)
			}
			if _, err := p.Birth(ctx, self); err != nil {
				t.Fatalf("birth of the valid record: %v", err)
			}
			if _, err := readStartToken(t, ctx, p, self); err != nil {
				t.Fatalf("start token of the valid record: %v", err)
			}
			putFile(t, filepath.Join(root, pid, "stat"), tt.stat)

			// This test process is alive, so a malformed record is never proof of absence.
			id, err := p.Identity(ctx, self)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				t.Errorf("Identity(%d) error %v; want an error other than %v", self, err, os.ErrNotExist)
			}
			if diff := gocmp.Diff(Identity{}, id); diff != "" {
				t.Errorf("Identity(%d) returned a sample (-want +got): %s", self, diff)
			}
			token, err := readStartToken(t, ctx, p, self)
			if err == nil || token != "" || errors.Is(err, os.ErrNotExist) {
				t.Errorf("StartToken(%d) = %q, %v; want an empty token and an error other than %v", self, token, err, os.ErrNotExist)
			}
			birth, err := p.Birth(ctx, self)
			if err == nil || !birth.IsZero() || errors.Is(err, os.ErrNotExist) {
				t.Errorf("Birth(%d) = %s, %v; want the zero time and an error other than %v", self, birth, err, os.ErrNotExist)
			}
			table, err := p.Table(ctx)
			if err != nil {
				t.Fatalf("one malformed record failed the whole table: %v", err)
			}
			if row, ok := table[self]; ok {
				t.Errorf("table keeps the malformed record as row %d: %+v", self, row)
			}
			row, ok := table[neighbour]
			if !ok {
				t.Fatalf("table %v lost the valid neighbour %d", table, neighbour)
			}
			checkLinuxToken(t, row.StartToken, bootA, "5151")
			row.StartToken = ""
			if diff := gocmp.Diff(Identity{PID: neighbour, ParentPID: 1, Argv: []string{"worker"}, Birth: statHeaderBirth(t, 1000, 5151)}, row); diff != "" {
				t.Errorf("neighbour row (-want +got): %s", diff)
			}
		})
	}
}

func TestLinuxStatRecordHeaderCommandName(t *testing.T) {
	// The header is the process number, one space and an opening parenthesis
	// at the start of the record; what the command name holds after that must
	// neither break the sample nor be read as the header.
	self := os.Getpid()
	pid := strconv.Itoa(self)
	tests := map[string]struct{ comm string }{
		"success: plain command name":                                 {comm: "worker"},
		"success: command name with spaces":                           {comm: "a b c"},
		"success: command name with parentheses":                      {comm: "(a)(b)"},
		"success: command name with a closing parenthesis and space":  {comm: "a) S 7 (b"},
		"success: command name with a newline":                        {comm: "a\nb"},
		"success: command name with a newline and another header":     {comm: "x\n" + strconv.Itoa(self+1) + " (y"},
		"success: command name that repeats this record's own header": {comm: ") " + pid + " (z"},
		"success: command name with every feature at once":            {comm: "a b) (c\nd) e"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, _ := fakeProc(t, self, tt.comm, procSample{btime: 1000, bootID: bootA, ppid: 1, start: "4242"})
			p := Probe{}
			birth := statHeaderBirth(t, 1000, 4242)

			id, err := p.Identity(ctx, self)
			if err != nil {
				t.Fatalf("Identity(%d): %v", self, err)
			}
			checkLinuxToken(t, id.StartToken, bootA, "4242")
			id.StartToken = ""
			if diff := gocmp.Diff(Identity{PID: self, ParentPID: 1, Argv: []string{"worker"}, Birth: birth, Image: statHeaderImage(t, "/bin/cat")}, id); diff != "" {
				t.Errorf("Identity(%d) (-want +got): %s", self, diff)
			}

			got, err := p.Birth(ctx, self)
			if err != nil || !got.Equal(birth) {
				t.Errorf("Birth(%d) = %s, %v; want %s", self, got, err, birth)
			}

			token, err := readStartToken(t, ctx, p, self)
			if err != nil {
				t.Errorf("StartToken(%d): %v", self, err)
			}
			checkLinuxToken(t, token, bootA, "4242")

			table, err := p.Table(ctx)
			if err != nil {
				t.Fatalf("Table: %v", err)
			}
			row, ok := table[self]
			if !ok {
				t.Fatalf("table %v has no row %d", table, self)
			}
			checkLinuxToken(t, row.StartToken, bootA, "4242")
			row.StartToken = ""
			if diff := gocmp.Diff(Identity{PID: self, ParentPID: 1, Argv: []string{"worker"}, Birth: birth}, row); diff != "" {
				t.Errorf("row %d (-want +got): %s", self, diff)
			}
		})
	}
}
