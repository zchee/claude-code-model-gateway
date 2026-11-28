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

//go:build process && linux

package ownership_test

import (
	"fmt"
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// handleTTerminateTrace is the guard's trace of an open and one terminate of the held child, by the backend the
// fixture found: a pidfd delivers through the descriptor the guard issued, ENOSYS falls back to the number, and EPERM
// gives no handle. Leading interrupted opens are not traced.
var handleTTerminateTrace = map[string][]string{
	"pidfd":  {"pidfd_open(held, 0)", "pidfd_send_signal(issued, terminated, info=nil, flags=0)"},
	"ENOSYS": {"pidfd_open(held, 0): function not implemented", "kill(held, terminated)"},
	"EPERM":  {"pidfd_open(held, 0): operation not permitted"},
}

// handleTOpenTrace is the guard's trace of an open alone, by the backend the fixture found.
var handleTOpenTrace = map[string][]string{
	"pidfd":  {"pidfd_open(held, 0)"},
	"ENOSYS": {"pidfd_open(held, 0): function not implemented"},
	"EPERM":  {"pidfd_open(held, 0): operation not permitted"},
}

// handleTDelivery is the one delivery a terminate must make on each backend that gives a handle.
var handleTDelivery = map[string]string{
	"pidfd":  "pidfd_send_signal(issued, terminated, info=nil, flags=0)",
	"ENOSYS": "kill(held, terminated)",
}

// handleTDeliveries are the successful deliveries of every backend the guard can forward on this platform.
var handleTDeliveries = []string{"pidfd_send_signal(issued, terminated, info=nil, flags=0)", "kill(held, terminated)"}

// TestHandleJudgeExpired decides the verdict on an expired row on scripted results and traces of a pidfd host, with
// no process: a successful delivery through either backend counts as a delivery for every "no delivery" verdict, and
// only an open handle's successful terminate needs the delivery its backend requires.
func TestHandleJudgeExpired(t *testing.T) {
	const (
		pidfdSend = "pidfd_send_signal(issued, terminated, info=nil, flags=0)"
		numeric   = "kill(held, terminated)"
	)
	closedRow := []string{"nil", "nil", "os.ErrClosed", "nil"}
	terminateRow := []string{"nil", "nil"}
	tests := map[string]struct {
		done  []handleTDone
		want  []string
		trace []string
		// failures are the verdict's failures; none leaves the row undecided.
		failures []string
	}{
		"error: a closed handle that signals by number on a pidfd host is a delivery, not an expiry": {
			done:     []handleTDone{{op: "open"}, {op: "close", open: true}, {op: "terminate", err: fmt.Errorf("terminate: %w", os.ErrClosed)}},
			want:     closedRow,
			trace:    []string{"pidfd_open(held, 0)", numeric},
			failures: []string{"the guard traced 1 deliveries through any backend for 0 terminates on the open handle"},
		},
		"success: a closed handle that delivered nothing leaves the row undecided": {
			done:  []handleTDone{{op: "open"}, {op: "close", open: true}, {op: "terminate", err: fmt.Errorf("terminate: %w", os.ErrClosed)}},
			want:  closedRow,
			trace: []string{"pidfd_open(held, 0)"},
		},
		"error: an open handle's nil terminate with only a numeric delivery on a pidfd host": {
			done:     []handleTDone{{op: "open"}, {op: "terminate", open: true}},
			want:     terminateRow,
			trace:    []string{"pidfd_open(held, 0)", numeric},
			failures: []string{`a terminate on the open handle answered nil but the guard traced no "` + pidfdSend + `" for it`},
		},
		"success: an open handle's nil terminate whose pidfd delivery raced the child's own exit": {
			done:  []handleTDone{{op: "open"}, {op: "terminate", open: true}},
			want:  terminateRow,
			trace: []string{"pidfd_open(held, 0)", pidfdSend},
		},
		"error: an open handle whose terminate answers os.ErrClosed": {
			done:     []handleTDone{{op: "open"}, {op: "terminate", open: true, err: fmt.Errorf("terminate: %w", os.ErrClosed)}},
			want:     terminateRow,
			trace:    []string{"pidfd_open(held, 0)"},
			failures: []string{"a terminate on the open handle answered os.ErrClosed, which an expiry does not explain"},
		},
		"success: an open handle whose terminate finds the process gone": {
			done:  []handleTDone{{op: "open"}, {op: "terminate", open: true, err: fmt.Errorf("terminate: %w", os.ErrProcessDone)}},
			want:  terminateRow,
			trace: []string{"pidfd_open(held, 0)"},
		},
		"success: an open that finds the process gone leaves the row undecided": {
			done:  []handleTDone{{op: "open", err: fmt.Errorf("open: %w", os.ErrProcessDone)}},
			want:  terminateRow,
			trace: []string{"pidfd_open(held, 0)"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := handleTJudgeExpired(tt.done, tt.want, "nil", tt.trace, pidfdSend, handleTDeliveries)
			if diff := gocmp.Diff(tt.failures, got); diff != "" {
				t.Errorf("verdict (-want +got):\n%s", diff)
			}
		})
	}
}
