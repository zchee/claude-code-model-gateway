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

func TestReplaceSettingsInfoExistenceAndIdentity(t *testing.T) {
	tests := map[string]struct {
		setup     func(*testing.T, string) fs.FileInfo
		wantExist bool
		wantInput string
		wantOut   string
	}{
		"success: missing file reports nil info": {
			setup:     func(*testing.T, string) fs.FileInfo { return nil },
			wantExist: false,
			wantInput: "{}",
			wantOut:   "{\"created\":true}\n",
		},
		"success: existing empty object reports opened info": {
			setup: func(t *testing.T, path string) fs.FileInfo {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				writeFixture(t, path, "{}")
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info
			},
			wantExist: true,
			wantInput: "{}",
			wantOut:   `{"existing":true}`,
		},
		"success: symlink target reports target info": {
			setup: func(t *testing.T, path string) fs.FileInfo {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "target.json")
				writeFixture(t, target, "{}")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				return info
			},
			wantExist: true,
			wantInput: "{}",
			wantOut:   `{"linked":true}`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings", "settings.local.json")
			wantInfo := tt.setup(t, path)
			err := ReplaceSettingsInfo(path, func(data []byte, info fs.FileInfo) ([]byte, error) {
				if diff := gocmp.Diff(tt.wantInput, string(data)); diff != "" {
					t.Fatalf("input (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.wantExist, info != nil); diff != "" {
					t.Fatalf("existed (-want +got):\n%s", diff)
				}
				if tt.wantExist && !os.SameFile(wantInfo, info) {
					t.Fatalf("callback info does not match opened target file")
				}
				return []byte(tt.wantOut), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			checkContents(t, path, tt.wantOut)
		})
	}
}

func TestReplaceSettingsInfoInterleavings(t *testing.T) {
	tests := map[string]struct {
		initial   bool
		interfere func(*testing.T, string) fs.FileInfo
		wantFirst bool
		wantRetry bool
		wantOut   string
	}{
		"success: file appears before commit": {
			initial: false,
			interfere: func(t *testing.T, path string) fs.FileInfo {
				t.Helper()
				writeFixture(t, path, "{}")
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info
			},
			wantFirst: false,
			wantRetry: true,
			wantOut:   `{"existing":true}`,
		},
		"success: file disappears before commit": {
			initial: true,
			interfere: func(t *testing.T, path string) fs.FileInfo {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			wantFirst: true,
			wantRetry: false,
			wantOut:   "{\"created\":true}\n",
		},
		"success: file replaced by new inode with same empty object bytes": {
			initial: true,
			interfere: func(t *testing.T, path string) fs.FileInfo {
				t.Helper()
				replacement := filepath.Join(filepath.Dir(path), "replacement.json")
				writeFixture(t, replacement, "{}")
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				return info
			},
			wantFirst: true,
			wantRetry: true,
			wantOut:   `{"existing":true}`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.local.json")
			var initialInfo fs.FileInfo
			if tt.initial {
				writeFixture(t, path, "{}")
				var err error
				initialInfo, err = os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			var interferedInfo fs.FileInfo
			var seen []fs.FileInfo
			interfered := false
			err := newWriter().replaceSettingsInfo(path, func(data []byte, info fs.FileInfo) ([]byte, error) {
				if diff := gocmp.Diff("{}", string(data)); diff != "" {
					t.Fatalf("input (-want +got):\n%s", diff)
				}
				seen = append(seen, info)
				if info == nil {
					return []byte("{\"created\":true}\n"), nil
				}
				return []byte(`{"existing":true}`), nil
			}, func(preparedFile, string) {
				if interfered {
					return
				}
				interfered = true
				interferedInfo = tt.interfere(t, path)
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(seen) != 2 {
				t.Fatalf("expected 2 edit attempts, got %d", len(seen))
			}
			if diff := gocmp.Diff(tt.wantFirst, seen[0] != nil); diff != "" {
				t.Fatalf("first attempt existence (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantRetry, seen[1] != nil); diff != "" {
				t.Fatalf("retry attempt existence (-want +got):\n%s", diff)
			}
			if tt.initial && !os.SameFile(initialInfo, seen[0]) {
				t.Fatal("first attempt info did not match initial file")
			}
			if tt.wantRetry && !os.SameFile(interferedInfo, seen[1]) {
				t.Fatal("retry attempt info did not match replacement file")
			}
			if tt.initial && tt.wantRetry && os.SameFile(seen[0], seen[1]) {
				t.Fatal("retry attempt still saw old inode")
			}
			checkContents(t, path, tt.wantOut)
			checkOnlyTarget(t, path)
		})
	}
}
