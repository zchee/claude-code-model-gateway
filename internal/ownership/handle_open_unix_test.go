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
	"io/fs"
	"math"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// handleTVirtualPID is a process number no process can hold: Linux assigns at most 2^22 numbers and macOS fewer.
// Rows that open a handle without a real process use it, so that even a handle that wrongly signals by number
// reaches nothing.
const handleTVirtualPID = 1 << 30

// handleTOpenIn opens a handle on pid, fails the row instead of the binary when OpenHandle panics, and closes any
// handle it got when the row ends. A handle returned beside an error fails the row at once, before anything is sent
// through it.
func handleTOpenIn(t *testing.T, pid int) (*Handle, error) {
	t.Helper()
	var h *Handle
	err := handleTCall(t, "OpenHandle", func() error {
		var err error
		h, err = OpenHandle(pid)
		return err
	})
	if h != nil {
		t.Cleanup(func() { _ = handleTGuarded("Handle.Close", h.Close) })
		if err != nil {
			t.Fatalf("OpenHandle(%d) returned a handle beside %v", pid, err)
		}
	}
	return h, err
}

// TestHandleOpenRefusal decides that a process number that is not positive is refused as invalid before the platform
// is asked: nothing is opened and nothing is delivered for it, and no handle comes back. Under the model even a handle
// that dispatched such a number would only produce a recorded call: a real kill of zero or a negative number would
// reach a process group or every process the account may signal.
func TestHandleOpenRefusal(t *testing.T) {
	tests := map[string]struct {
		pid int
	}{
		"error: process number zero opens and delivers nothing":              {pid: 0},
		"error: process number minus one opens and delivers nothing":         {pid: -1},
		"error: the most negative process number opens and delivers nothing": {pid: math.MinInt},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := handleTInstallModel(t, &handleTModel{})
			h, err := handleTOpenIn(t, tt.pid)
			got := []string{handleTResult(err, fs.ErrInvalid)}
			if h != nil {
				got = append(got, "a handle came back")
			}
			if diff := gocmp.Diff([]string{handleTExpect(fs.ErrInvalid)}, got); diff != "" {
				t.Errorf("OpenHandle(%d) (-want +got):\n%s", tt.pid, diff)
			}
			opens, deliveries := m.calls()
			if diff := gocmp.Diff([]string(nil), append(opens, deliveries...)); diff != "" {
				t.Errorf("system calls for OpenHandle(%d) (-want +got):\n%s", tt.pid, diff)
			}
		})
	}
}
