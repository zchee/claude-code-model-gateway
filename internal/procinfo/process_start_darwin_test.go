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
	"fmt"
	"strconv"
	"strings"
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

// checkDarwinToken requires decimal seconds, one separator byte, and six-digit
// microseconds of the kernel's recorded start, in that order.
func checkDarwinToken(t *testing.T, token StartToken, sec int64, usec int32) {
	t.Helper()
	rest, secFirst := strings.CutPrefix(string(token), strconv.FormatInt(sec, 10))
	sep, usecLast := strings.CutSuffix(rest, fmt.Sprintf("%06d", usec))
	if !secFirst || !usecLast || len(sep) != 1 || !strings.Contains("!#$%&*+,-./:;<=>?@^_|~", sep) {
		t.Errorf("token %q; want %d, one punctuation separator, then %06d", token, sec, usec)
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
	base := kinfo(702, 1, 981238509, 250000)
	tests := map[string]struct {
		later *unix.KinfoProc
		same  bool
	}{
		"success: later microsecond under the same process number is another process": {later: kinfo(702, 1, 981238509, 250001)},
		"success: later second under the same process number is another process":      {later: kinfo(702, 1, 981238510, 250000)},
		// The kernel records the start at fork and a clock step does not rewrite it,
		// so a sample after a step carries the same recorded start.
		"success: recorded start read again keeps the token": {later: kinfo(702, 1, 981238509, 250000), same: true},
		"success: reparented process keeps the token":        {later: kinfo(702, 2, 981238509, 250000), same: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			first, later := darwinRow(base).StartToken, darwinRow(tt.later).StartToken
			if first == "" || later == "" || (first == later) != tt.same {
				t.Errorf("tokens %q then %q; want same=%t", first, later, tt.same)
			}
		})
	}
}
