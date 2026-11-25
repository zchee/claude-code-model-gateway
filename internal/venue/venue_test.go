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

package venue

import (
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestCheck(t *testing.T) {
	home := "/account"
	if !filepath.IsAbs(home) {
		home = `C:\account`
	}
	calls := []string{"home", filepath.Join(home, ".model-gateway-test-venue"), filepath.Join(home, ".claude", "model-gateway"), "18764", "18765"}
	tests := map[string]struct {
		homeErr, attestErr, stateErr, portErr error
		relative, statePresent                bool
		mode                                  fs.FileMode
		busy                                  string
		reason                                string
		steps                                 int
	}{
		"success: attested and idle":      {steps: 5},
		"error: home lookup":              {homeErr: fs.ErrPermission, reason: "cannot establish account home", steps: 1},
		"error: relative home":            {relative: true, reason: "cannot establish account home", steps: 1},
		"error: missing attestation":      {attestErr: fs.ErrNotExist, reason: "attestation missing", steps: 2},
		"error: inaccessible attestation": {attestErr: fs.ErrPermission, reason: "cannot inspect attestation", steps: 2},
		"error: symlink attestation":      {mode: fs.ModeSymlink, reason: "attestation is not a regular file", steps: 2},
		"error: directory attestation":    {mode: fs.ModeDir, reason: "attestation is not a regular file", steps: 2},
		"error: gateway state":            {statePresent: true, reason: "gateway state present", steps: 3},
		"error: inaccessible state":       {stateErr: fs.ErrPermission, reason: "cannot inspect gateway state", steps: 3},
		"error: first port busy":          {busy: "18764", reason: "TCP port 18764 has a listener", steps: 4},
		"error: second port busy":         {busy: "18765", reason: "TCP port 18765 has a listener", steps: 5},
		"error: port table unavailable":   {portErr: fs.ErrPermission, reason: "cannot inspect TCP port 18764", steps: 4},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var gotCalls []string
			lookup := func() (string, error) {
				gotCalls = append(gotCalls, "home")
				if tc.relative {
					return "relative", nil
				}
				return home, tc.homeErr
			}
			lstat := func(path string) (fs.FileInfo, error) {
				gotCalls = append(gotCalls, path)
				if path == calls[1] {
					if tc.attestErr != nil {
						return nil, tc.attestErr
					}
					return fs.Lstat(fstest.MapFS{"marker": {Mode: tc.mode}}, "marker")
				}
				if tc.stateErr != nil || tc.statePresent {
					return nil, tc.stateErr
				}
				return nil, fs.ErrNotExist
			}
			listen := func(port string) (bool, error) {
				gotCalls = append(gotCalls, port)
				return port == tc.busy, tc.portErr
			}
			if diff := gocmp.Diff(tc.reason, check(lookup, lstat, listen)); diff != "" {
				t.Errorf("reason (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(calls[:tc.steps], gotCalls); diff != "" {
				t.Errorf("probe order (-want +got):\n%s", diff)
			}
		})
	}
}
