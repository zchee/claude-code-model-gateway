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

package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestSettingsCreateAndPreserve(t *testing.T) {
	tests := map[string]struct {
		existing  bool
		mode      fs.FileMode
		wantInput string
	}{
		"success: missing parent directories":        {wantInput: "{}"},
		"success: existing mode includes sticky bit": {true, fs.ModeSticky | 0o640, oldState},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings", "local.json")
			if tt.existing {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				writeFixture(t, path, oldState)
				if err := os.Chmod(path, tt.mode); err != nil {
					t.Fatal(err)
				}
			}
			if err := ReplaceSettings(path, func(data []byte) ([]byte, error) {
				if diff := gocmp.Diff(tt.wantInput, string(data)); diff != "" {
					t.Fatalf("settings input (-want +got):\n%s", diff)
				}
				return []byte(newState), nil
			}); err != nil {
				t.Fatal(err)
			}
			checkContents(t, path, newState)
			if tt.existing {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if diff := gocmp.Diff(tt.mode, info.Mode()); diff != "" {
					t.Errorf("mode (-want +got):\n%s", diff)
				}
			}
		})
	}
}
