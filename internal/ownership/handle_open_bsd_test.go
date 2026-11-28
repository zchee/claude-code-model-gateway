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

//go:build unix && !linux

package ownership

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// TestHandleNumericBackend decides that on macOS and the BSDs the handle is the number itself: it opens nothing and
// delivers by kill on the number. It names no lifetime, so a process that later holds the number is not protected
// from the terminate, and no row here claims it is. Nothing is delivered for real: kill is the model's.
func TestHandleNumericBackend(t *testing.T) {
	tests := map[string]struct {
		killErr    error
		results    []string
		deliveries []string
	}{
		"success: the handle delivers by number only": {
			results:    []string{"open: nil", "terminate: nil", "close: nil", "terminate: os.ErrClosed"},
			deliveries: []string{"kill(1073741824, terminated)"},
		},
		"error: a number nobody holds is a process that is gone": {
			killErr:    unix.ESRCH,
			results:    []string{"open: nil", "terminate: os.ErrProcessDone", "close: nil", "terminate: os.ErrClosed"},
			deliveries: []string{"kill(1073741824, terminated)"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := handleTInstallModel(t, &handleTModel{killErr: tt.killErr})
			h, err := handleTOpenIn(t, handleTVirtualPID)
			got := []string{"open: " + handleTKind(err)}
			if h == nil {
				t.Fatalf("OpenHandle(%d) returned no handle; results %v", handleTVirtualPID, got)
			}
			got = append(got,
				"terminate: "+handleTKind(handleTCall(t, "Handle.Terminate", h.Terminate)),
				"close: "+handleTKind(handleTCall(t, "Handle.Close", h.Close)),
				"terminate: "+handleTKind(handleTCall(t, "Handle.Terminate", h.Terminate)))
			if diff := gocmp.Diff(tt.results, got); diff != "" {
				t.Errorf("results (-want +got):\n%s", diff)
			}
			opens, deliveries := m.calls()
			if diff := gocmp.Diff([]string(nil), opens); diff != "" {
				t.Errorf("open calls (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.deliveries, deliveries); diff != "" {
				t.Errorf("delivery calls (-want +got):\n%s", diff)
			}
		})
	}
}
