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

//go:build unix

package ownership

import (
	"errors"
	"io/fs"
	"math"
	"runtime"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestHandleOpenWidePID decides that on Unix a positive process number that the system's signed 32-bit process-number
// type cannot hold is refused as invalid before the platform is asked: no handle comes back, and nothing is opened
// and nothing is delivered for it. Handed on, such a number would be narrowed to its low 32 bits: to another process
// number, to zero (the caller's own process group), or to a negative number (a process group, or with minus one every
// process the account may signal). The largest number the type holds is not refused by this rule: it reaches the
// platform unchanged. Every call goes to the model; nothing reaches a real process.
func TestHandleOpenWidePID(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Logf("int has %d bits here: the four oversized process numbers cannot be represented, and the control at the 32-bit boundary is not exercised on this architecture", strconv.IntSize)
		return
	}
	tests := map[string]struct {
		// pid is held as int64 so that the table compiles where int has 32 bits; it is converted after the guard.
		pid int64
		// reaches says that the number is not refused by this rule and must reach the platform unchanged.
		reaches bool
	}{
		"error: one past the largest 32-bit process number, which narrows to a negative number, opens and delivers nothing": {
			pid: math.MaxInt32 + 1,
		},
		"error: two to the 32nd, which narrows to zero, opens and delivers nothing": {
			pid: 1 << 32,
		},
		"error: two to the 32nd plus one, which narrows to process number one, opens and delivers nothing": {
			pid: 1<<32 + 1,
		},
		"error: the largest int, which narrows to minus one, opens and delivers nothing": {
			pid: math.MaxInt,
		},
		"success: the largest 32-bit process number reaches the platform unchanged": {
			pid:     math.MaxInt32,
			reaches: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			pid := int(tt.pid)
			m := handleTInstallModel(t, &handleTModel{})
			h, err := handleTOpenIn(t, pid)
			if !tt.reaches {
				got := []string{handleTResult(err, fs.ErrInvalid)}
				if h != nil {
					got = append(got, "a handle came back")
				}
				if diff := gocmp.Diff([]string{handleTExpect(fs.ErrInvalid)}, got); diff != "" {
					t.Errorf("OpenHandle(%d) (-want +got):\n%s", pid, diff)
				}
				opens, deliveries := m.calls()
				if diff := gocmp.Diff([]string(nil), append(opens, deliveries...)); diff != "" {
					t.Errorf("system calls for OpenHandle(%d) (-want +got):\n%s", pid, diff)
				}
				return
			}
			// Linux and Android open a pidfd. The answers of pidfd_open can be scripted only from a file of that
			// backend, so there the model answers this row's open with its own error for an unscripted call, which
			// does not match fs.ErrInvalid. The row decides only that the open was asked for the number unchanged and
			// that the number was not refused as invalid; what OpenHandle makes of an open's answer is decided by the
			// scripted open tests. Elsewhere the handle is the number, so the open succeeds and its terminate is a
			// kill of the number.
			want := []string{"kill(2147483647, terminated)"}
			if runtime.GOOS == "linux" || runtime.GOOS == "android" {
				want = []string{"pidfd_open(2147483647, 0)"}
				if errors.Is(err, fs.ErrInvalid) {
					t.Errorf("OpenHandle(%d) = %v, which matches %v: the boundary number was refused", pid, err, fs.ErrInvalid)
				}
			} else if h == nil || err != nil {
				t.Errorf("OpenHandle(%d) = %v, %v; want a handle and a nil error", pid, h, err)
			}
			if h != nil {
				if err := handleTCall(t, "Handle.Terminate", h.Terminate); err != nil {
					t.Errorf("Terminate of the handle on %d: %v", pid, err)
				}
				if err := handleTCall(t, "Handle.Close", h.Close); err != nil {
					t.Errorf("Close of the handle on %d: %v", pid, err)
				}
			}
			opens, deliveries := m.calls()
			if diff := gocmp.Diff(want, append(opens, deliveries...)); diff != "" {
				t.Errorf("system calls for OpenHandle(%d) and its terminate (-want +got):\n%s", pid, diff)
			}
		})
	}
}
