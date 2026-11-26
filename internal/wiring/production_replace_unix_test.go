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

package wiring

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
)

func internalSandbox(t *testing.T) (Store, string, string, string) {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	env := map[string]string{"CODEX_GATEWAY_PORT": "49123"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	store := Store{
		Paths:   paths.New(lookup, func() (string, error) { return home, nil }, func() (string, error) { return project, nil }),
		Project: project,
		Lookup:  lookup,
	}
	local := filepath.Join(project, ".claude", "settings.local.json")
	shared := filepath.Join(project, ".claude", "settings.json")
	return store, home, local, shared
}

func writeFileMust(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProductionAdapterExistenceFollowsOpenedSnapshot(t *testing.T) {
	tests := map[string]struct {
		initial        bool
		interfere      func(*testing.T, string)
		wantAttempt1NL bool
		wantFinalNL    bool
	}{
		"success: file appears after open but before callback": {
			initial:        false,
			interfere:      func(t *testing.T, file string) { writeFileMust(t, file, "{}") },
			wantAttempt1NL: true,
			wantFinalNL:    false,
		},
		"success: file disappears after open but before callback": {
			initial: true,
			interfere: func(t *testing.T, file string) {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			},
			wantAttempt1NL: false,
			wantFinalNL:    true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store, _, local, _ := internalSandbox(t)
			if tt.initial {
				writeFileMust(t, local, "{}")
			}
			var attemptNewlines []bool
			store.replaceSettingsInfo = func(file string, edit func([]byte, fs.FileInfo) ([]byte, error)) error {
				return atomicfile.ReplaceSettingsInfo(file, func(old []byte, info fs.FileInfo) ([]byte, error) {
					if file == local && len(attemptNewlines) == 0 {
						// Mutate the pathname AFTER atomicfile opened the snapshot and BEFORE wiring's
						// production adapter runs: probing the pathname would disagree with info.
						tt.interfere(t, file)
					}
					next, err := edit(old, info)
					if err == nil && file == local {
						attemptNewlines = append(attemptNewlines, bytes.HasSuffix(next, []byte("\n")))
					}
					return next, err
				})
			}
			// Keep Replace nil to exercise the exact production adapter.
			if err := store.WriteEnv("project", "default", false); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff([]bool{tt.wantAttempt1NL, tt.wantFinalNL}, attemptNewlines); diff != "" {
				t.Fatalf("attempt trailing-newline decisions (-want +got):\n%s", diff)
			}
			got, err := os.ReadFile(local)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.wantFinalNL, bytes.HasSuffix(got, []byte("\n"))); diff != "" {
				t.Fatalf("final file trailing newline (-want +got):\n%s", diff)
			}
		})
	}
}

func TestProductionAdapterMigrationStaleSourceAndGrowthBound(t *testing.T) {
	t.Run("success: production adapter aborts migration when source changes before local replacement", func(t *testing.T) {
		store, _, local, shared := internalSandbox(t)
		const directURL = "http://127.0.0.1:49123"
		writeFileMust(t, shared, `{"env":{"ANTHROPIC_BASE_URL":"`+directURL+`","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"}}`)
		const personal = `{"env":{"ANTHROPIC_BASE_URL":"https://personal.example","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"}}`
		store.replaceSettingsInfo = func(file string, edit func([]byte, fs.FileInfo) ([]byte, error)) error {
			if file == local {
				writeFileMust(t, shared, personal)
			}
			return atomicfile.ReplaceSettingsInfo(file, edit)
		}
		migrated, err := store.MigrateLegacy()
		if err != nil {
			t.Fatal(err)
		}
		if diff := gocmp.Diff(false, migrated); diff != "" {
			t.Fatalf("migrated (-want +got):\n%s", diff)
		}
		if _, err := os.Lstat(local); !os.IsNotExist(err) {
			t.Fatalf("expected local absent, got %v", err)
		}
		gotShared, err := os.ReadFile(shared)
		if err != nil {
			t.Fatal(err)
		}
		if diff := gocmp.Diff(personal, string(gotShared)); diff != "" {
			t.Fatalf("shared bytes (-want +got):\n%s", diff)
		}
	})

	t.Run("error: file growing past bound between preliminary read and replacement is refused", func(t *testing.T) {
		store, _, local, _ := internalSandbox(t)
		writeFileMust(t, local, `{"env":{"KEEP":"1"}}`)
		var grownSize int64
		store.replaceSettingsInfo = func(file string, edit func([]byte, fs.FileInfo) ([]byte, error)) error {
			if file == local {
				const targetSize = maxSnapshotBytes + 64
				f, err := os.Create(file)
				if err != nil {
					t.Fatal(err)
				}
				const body = `{"env":{"KEEP":"grown"}}`
				if _, err := f.WriteString(body); err != nil {
					t.Fatal(err)
				}
				pad := bytes.Repeat([]byte(" "), 64<<10)
				written := int64(len(body))
				for written < targetSize {
					n := min(int64(len(pad)), targetSize-written)
					if _, err := f.Write(pad[:n]); err != nil {
						t.Fatal(err)
					}
					written += n
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				grownSize = written
			}
			return atomicfile.ReplaceSettingsInfo(file, edit)
		}
		err := store.WriteEnv("project", "default", false)
		if err == nil {
			t.Fatal("expected size-bound error when file grew before replacement, got nil")
		}
		if !strings.Contains(err.Error(), local) || !strings.Contains(err.Error(), "16777216") {
			t.Fatalf("error %q should name file %q and size bound", err.Error(), local)
		}
		info, statErr := os.Stat(local)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if diff := gocmp.Diff(grownSize, info.Size()); diff != "" {
			t.Fatalf("grown file size changed (-want +got):\n%s", diff)
		}
	})
}
