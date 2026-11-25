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

package procinfo

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

// writeRecord adapts the publication fault tests' path-based callback to the
// same writer as WriteRecord; production only passes directory-relative names.
func writeRecord(root, name string, r Record, replace func(string, []byte) error) error {
	return (recordIO{}).write(root, name, r, func(_ *atomicfile.Dir, base string, data []byte) error {
		return replace(filepath.Join(root, base), data)
	})
}

func TestRecordKeepsOpenedDirectory(t *testing.T) {
	tests := map[string]struct{ repair bool }{"success: publish mirror after root is replaced": {}, "success: repair mirror after root is replaced": {repair: true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "state")
			moved := filepath.Join(parent, "opened")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			want, wire := recordFixture(root)
			records := recordIO{beforeMirror: func() {
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			}}
			if tt.repair {
				putFile(t, filepath.Join(root, "guardian.pid.json"), wire)
				putFile(t, filepath.Join(root, "guardian.pid"), "701")
				got, err := records.read(root, "guardian")
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Fatal(diff)
				}
			} else if err := records.write(root, "guardian", want, replaceRecordEntry); err != nil {
				t.Fatal(err)
			}
			var saved Record
			if err := json.Unmarshal([]byte(readFile(t, filepath.Join(moved, "guardian.pid.json"))), &saved); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(want, saved); diff != "" {
				t.Fatal(diff)
			}
			if got := readFile(t, filepath.Join(moved, "guardian.pid")); got != strconv.Itoa(want.PID) {
				t.Fatalf("opened directory mirror = %q, want %d", got, want.PID)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("replacement directory was modified: %v", entries)
			}
		})
	}
}
