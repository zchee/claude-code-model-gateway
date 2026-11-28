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

package ownership

import (
	"errors"
	"fmt"
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// TestHandleOpenSyscall decides what OpenHandle makes of each answer of pidfd_open, and which delivery call the handle
// it gives then makes: ENOSYS gives the numeric handle, which delivers by kill on the number and names no lifetime;
// EINTR is asked again and the descriptor of the next answer is the handle, open from the moment OpenHandle returns
// until Close releases it, and delivering by pidfd_send_signal through it; ESRCH is a process that is gone, with no
// handle and no fallback; any other error is returned with no handle and no fallback. Nothing is delivered for real:
// every call is the model's.
func TestHandleOpenSyscall(t *testing.T) {
	tests := map[string]struct {
		answers []handleTAnswer
		// killErr answers the numeric handle's kill.
		killErr error
		// want is the sentinel every error must match; nil wants none.
		want       error
		notWant    error
		steps      []string
		results    []string
		opens      []string
		deliveries []string
	}{
		"success: ENOSYS gives the numeric handle, which delivers by number only": {
			answers:    []handleTAnswer{{err: unix.ENOSYS}},
			steps:      []string{"terminate", "close"},
			results:    []string{"open: nil", "terminate: nil", "close: nil"},
			opens:      []string{"pidfd_open(1073741824, 0)"},
			deliveries: []string{"kill(1073741824, terminated)"},
		},
		"error: after ENOSYS a number nobody holds is a process that is gone, delivered to once": {
			answers:    []handleTAnswer{{err: unix.ENOSYS}},
			killErr:    unix.ESRCH,
			steps:      []string{"terminate", "close"},
			results:    []string{"open: nil", "terminate: os.ErrProcessDone", "close: nil"},
			opens:      []string{"pidfd_open(1073741824, 0)"},
			deliveries: []string{"kill(1073741824, terminated)"},
		},
		"success: EINTR is asked again and the next descriptor is the handle until Close": {
			answers:    []handleTAnswer{{err: unix.EINTR}, {issue: true}},
			steps:      []string{"descriptor open", "terminate", "descriptor open", "close", "descriptor open"},
			results:    []string{"open: nil", "descriptor open: true", "terminate: nil", "descriptor open: true", "close: nil", "descriptor open: false"},
			opens:      []string{"pidfd_open(1073741824, 0)", "pidfd_open(1073741824, 0)"},
			deliveries: []string{"pidfd_send_signal(descriptor 1, terminated, info=nil, flags=0)"},
		},
		"error: ESRCH is a process that is gone, with no handle and no fallback": {
			answers: []handleTAnswer{{err: unix.ESRCH}},
			want:    os.ErrProcessDone,
			results: []string{"open: " + handleTExpect(os.ErrProcessDone)},
			opens:   []string{"pidfd_open(1073741824, 0)"},
		},
		"error: EPERM is returned with no handle and no fallback": {
			answers: []handleTAnswer{{err: unix.EPERM}},
			want:    unix.EPERM,
			notWant: os.ErrProcessDone,
			results: []string{"open: " + handleTExpect(unix.EPERM)},
			opens:   []string{"pidfd_open(1073741824, 0)"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := handleTInstallModel(t, &handleTModel{answers: tt.answers, killErr: tt.killErr})
			h, err := handleTOpenIn(t, handleTVirtualPID)
			if tt.notWant != nil && errors.Is(err, tt.notWant) {
				t.Errorf("OpenHandle error %v matches %v", err, tt.notWant)
			}
			got := []string{"open: " + handleTResult(err, tt.want)}
			for _, s := range tt.steps {
				if h == nil {
					t.Fatalf("no handle for %q; results so far %v", s, got)
				}
				switch s {
				case "descriptor open":
					got = append(got, fmt.Sprintf("descriptor open: %t", m.descriptorOpen(1)))
				case "terminate":
					got = append(got, s+": "+handleTKind(handleTCall(t, "Handle.Terminate", h.Terminate)))
				case "close":
					got = append(got, s+": "+handleTKind(handleTCall(t, "Handle.Close", h.Close)))
				}
			}
			if diff := gocmp.Diff(tt.results, got); diff != "" {
				t.Errorf("results (-want +got):\n%s", diff)
			}
			opens, deliveries := m.calls()
			if diff := gocmp.Diff(tt.opens, opens); diff != "" {
				t.Errorf("pidfd_open calls (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.deliveries, deliveries); diff != "" {
				t.Errorf("delivery calls (-want +got):\n%s", diff)
			}
		})
	}
}

// TestHandleLifetime decides that Terminate delivers through what OpenHandle bound and nothing else: exactly one
// delivery through the issued descriptor, and none by number, also when the descriptor reports its process gone; and
// nothing at all once the handle is closed. That a pidfd names one lifetime is the kernel's promise and is not tested
// here; what is tested is that the handle never falls back to the number. Nothing is delivered for real.
func TestHandleLifetime(t *testing.T) {
	tests := map[string]struct {
		sendErr    error
		steps      []string
		results    []string
		deliveries []string
	}{
		"success: terminate delivers once, through the descriptor the open bound": {
			steps:      []string{"terminate", "close"},
			results:    []string{"terminate: nil", "close: nil"},
			deliveries: []string{"pidfd_send_signal(descriptor 1, terminated, info=nil, flags=0)"},
		},
		"error: a descriptor whose process is gone reports it gone and is never followed by the number": {
			sendErr:    unix.ESRCH,
			steps:      []string{"terminate", "close"},
			results:    []string{"terminate: os.ErrProcessDone", "close: nil"},
			deliveries: []string{"pidfd_send_signal(descriptor 1, terminated, info=nil, flags=0)"},
		},
		"error: a closed handle delivers nothing": {
			steps:   []string{"close", "terminate", "close"},
			results: []string{"close: nil", "terminate: os.ErrClosed", "close: nil"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := handleTInstallModel(t, &handleTModel{answers: []handleTAnswer{{issue: true}}, sendErr: tt.sendErr, killErr: unix.ESRCH})
			h, err := handleTOpenIn(t, handleTVirtualPID)
			if err != nil || h == nil {
				t.Fatalf("OpenHandle(%d) = %v, %v; want a handle on the issued descriptor", handleTVirtualPID, h, err)
			}
			var got []string
			for _, s := range tt.steps {
				switch s {
				case "terminate":
					got = append(got, s+": "+handleTKind(handleTCall(t, "Handle.Terminate", h.Terminate)))
				case "close":
					got = append(got, s+": "+handleTKind(handleTCall(t, "Handle.Close", h.Close)))
				}
			}
			if diff := gocmp.Diff(tt.results, got); diff != "" {
				t.Errorf("results (-want +got):\n%s", diff)
			}
			opens, deliveries := m.calls()
			if diff := gocmp.Diff([]string{"pidfd_open(1073741824, 0)"}, opens); diff != "" {
				t.Errorf("pidfd_open calls (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.deliveries, deliveries); diff != "" {
				t.Errorf("delivery calls (-want +got):\n%s", diff)
			}
		})
	}
}
