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

//go:build darwin

package procinfo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// kinfo is the part of a kern.proc.pid sample that a process row is built from.
func kinfo(pid, ppid int32, sec int64, usec int32) *unix.KinfoProc {
	var info unix.KinfoProc
	info.Proc.P_pid = pid
	info.Eproc.Ppid = ppid
	info.Proc.P_starttime = unix.Timeval{Sec: sec, Usec: usec}
	return &info
}

// named sets the command name the kernel keeps for the process, which an exec
// replaces.
func named(info *unix.KinfoProc, comm string) *unix.KinfoProc {
	clear(info.Proc.P_comm[:])
	copy(info.Proc.P_comm[:len(info.Proc.P_comm)-1], comm)
	return info
}

// checkDarwinToken requires decimal seconds, one separator byte, and six-digit
// microseconds of the kernel's recorded start, in that order.
func checkDarwinToken(t *testing.T, token StartToken, sec int64, usec int32) {
	t.Helper()
	rest, secFirst := strings.CutPrefix(string(token), strconv.FormatInt(sec, 10))
	sep, usecLast := strings.CutSuffix(rest, fmt.Sprintf("%06d", usec))
	if !secFirst || !usecLast || !oneASCIIPunctuation(sep) {
		t.Errorf("token %q; want %d, one ASCII punctuation byte, then %06d", token, sec, usec)
	}
}

func TestDarwinStartTokenFormat(t *testing.T) {
	tests := map[string]struct {
		sec  int64
		usec int32
	}{
		"success: microseconds below ten keep six digits": {sec: 981238509, usec: 7},
		"success: whole second":                           {sec: 981238509, usec: 0},
		"success: last microsecond of a second":           {sec: 981238509, usec: 999999},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			info := kinfo(702, 1, tt.sec, tt.usec)
			row := darwinRow(info)
			if want := time.Unix(tt.sec, int64(tt.usec)*1000); !row.Birth.Equal(want) {
				t.Fatalf("birth %s; want %s from the same sample", row.Birth, want)
			}
			if again := darwinRow(info); again.StartToken != row.StartToken {
				t.Errorf("two rows from one sample: %q then %q", row.StartToken, again.StartToken)
			}
			checkDarwinToken(t, row.StartToken, tt.sec, tt.usec)
		})
	}
}

func TestDarwinStartTokenReuse(t *testing.T) {
	base := named(kinfo(702, 1, 981238509, 250000), "worker")
	tests := map[string]struct {
		later *unix.KinfoProc
		same  bool
		exec  bool
	}{
		"success: later microsecond under the same process number is another process": {later: named(kinfo(702, 1, 981238509, 250001), "worker")},
		"success: later second under the same process number is another process":      {later: named(kinfo(702, 1, 981238510, 250000), "worker")},
		// The kernel records the start at fork and a clock step does not rewrite it,
		// so a sample after a step carries the same recorded start.
		"success: recorded start read again keeps the token": {later: named(kinfo(702, 1, 981238509, 250000), "worker"), same: true},
		"success: reparented process keeps the token":        {later: named(kinfo(702, 2, 981238509, 250000), "worker"), same: true},
		// An exec keeps the process number and the recorded start and replaces the command name.
		"success: exec of another program keeps the token": {later: named(kinfo(702, 1, 981238509, 250000), "sh"), same: true, exec: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// An exec replaces the command name only; this proves the fixture changed it and nothing the token is made of.
			if tt.exec && (tt.later.Proc.P_comm == base.Proc.P_comm || tt.later.Proc.P_pid != base.Proc.P_pid || tt.later.Proc.P_starttime != base.Proc.P_starttime) {
				t.Fatalf("exec fixture %q at pid %d start %+v; want another name than %q under the same pid and start", tt.later.Proc.P_comm, tt.later.Proc.P_pid, tt.later.Proc.P_starttime, base.Proc.P_comm)
			}
			first, later := darwinRow(base).StartToken, darwinRow(tt.later).StartToken
			if first == "" || later == "" || (first == later) != tt.same {
				t.Errorf("tokens %q then %q; want same=%t", first, later, tt.same)
			}
		})
	}
}

func TestDarwinStartTokenAbsence(t *testing.T) {
	// The kernel answers kern.proc.pid for a vanished process with an empty
	// record; SysctlKinfoProc reports an empty or short record as EIO.
	// Only the existence probe can then tell a process that is gone from one
	// whose record is out of reach. The number is this live test process's, so
	// a read that bypasses the replaced record finds a real one.
	pid := os.Getpid()
	tests := map[string]struct {
		read      error
		existence error
		absent    bool
	}{
		"error: an empty or short record of a process that does not exist is confirmed absent": {read: unix.EIO, existence: unix.ESRCH, absent: true},
		"error: a failed read for a process that does not exist is confirmed absent":           {read: unix.EINVAL, existence: unix.ESRCH, absent: true},
		"error: an empty or short record of a live process is not absence":                     {read: unix.EIO, existence: nil},
		"error: a failed read for a live process is not absence":                               {read: unix.EINVAL, existence: nil},
		"error: an empty or short record of a live process of another user is not absence":     {read: unix.EIO, existence: unix.EPERM},
		"error: a failed read for a live process of another user is not absence":               {read: unix.EINVAL, existence: unix.EPERM},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var reads atomic.Int32
			ctx := context.WithValue(t.Context(), kernelRecordKey{}, func(got int) (*unix.KinfoProc, error) {
				reads.Add(1)
				if got != pid {
					t.Errorf("kernel record read for pid %d; want %d", got, pid)
				}
				return nil, tt.read
			})
			p := Probe{existence: func(_ context.Context, got int) error {
				if got != pid {
					t.Errorf("existence probe asked about pid %d; want %d", got, pid)
				}
				return tt.existence
			}}
			// Birth reads the same record; this proves the replaced read is the one the adapter uses.
			if _, err := p.Birth(ctx, pid); reads.Load() == 0 || err == nil || errors.Is(err, os.ErrNotExist) != tt.absent {
				t.Fatalf("Birth(%d) with the replaced record: %v after %d reads; want confirmed absence = %t", pid, err, reads.Load(), tt.absent)
			}
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
