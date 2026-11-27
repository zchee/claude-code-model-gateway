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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// stagingSetup prepares the staging subdirectory name inside dir for a private staging write.
type stagingSetup string

const (
	// stagingPrivate is the subdirectory CreateStagingDir makes.
	stagingPrivate stagingSetup = "private"
	// stagingMissing leaves the name absent.
	stagingMissing stagingSetup = "missing"
	// stagingShared is a directory others can open.
	stagingShared stagingSetup = "shared"
	// stagingSymlink is a symbolic link to a private directory elsewhere.
	stagingSymlink stagingSetup = "symlink"
	// stagingFile is a regular file.
	stagingFile stagingSetup = "file"
)

func prepareStaging(t *testing.T, dir string, setup stagingSetup) {
	t.Helper()
	name := filepath.Join(dir, StagingDirName)
	switch setup {
	case stagingPrivate:
		if err := CreateStagingDir(dir, 0o777); err != nil {
			t.Fatalf("CreateStagingDir: %v", err)
		}
	case stagingMissing:
	case stagingShared:
		if err := os.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, 0o755); err != nil {
			t.Fatal(err)
		}
	case stagingSymlink:
		elsewhere := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(elsewhere, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, name); err != nil {
			t.Fatal(err)
		}
	case stagingFile:
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown staging setup %q", setup)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// TestPrivateStagingWrite decides what a PrivateStaging write does with each state of the
// destination directory's staging subdirectory. With the private subdirectory CreateStagingDir
// makes, the new content replaces the destination and nothing is left behind in either
// directory. Without it, or with a name there that is not a private directory of the caller, the
// write fails: there is no second staging path, so the destination keeps its old content and
// nothing is created beside it.
func TestPrivateStagingWrite(t *testing.T) {
	const (
		oldData = `{"old":true}`
		newData = `{"new":true}`
	)
	tests := map[string]struct {
		setup stagingSetup
		// wantErr, when set, is a sentinel the error must match; failing alone requires an error.
		wantErr error
		failing bool
	}{
		"success: a private staging subdirectory publishes the new content": {setup: stagingPrivate},
		"error: a missing staging subdirectory fails the write":             {setup: stagingMissing, failing: true, wantErr: fs.ErrNotExist},
		"error: a staging subdirectory others can open fails the write":     {setup: stagingShared, failing: true, wantErr: fs.ErrPermission},
		"error: a symbolic link at the staging name fails the write":        {setup: stagingSymlink, failing: true},
		"error: a regular file at the staging name fails the write":         {setup: stagingFile, failing: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "catalog.json")
			if err := os.WriteFile(path, []byte(oldData), 0o600); err != nil {
				t.Fatal(err)
			}
			prepareStaging(t, dir, tt.setup)
			before := dirNames(t, dir)

			err := WriteFile(path, []byte(newData), 0o600, SkipSync(), PrivateStaging())

			want := newData
			if tt.failing {
				want = oldData
				switch {
				case err == nil:
					t.Error("WriteFile succeeded, want it to fail without a usable staging subdirectory")
				case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
					t.Errorf("WriteFile error %v, want one matching %v", err, tt.wantErr)
				}
			} else if err != nil {
				t.Errorf("WriteFile: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if d := gocmp.Diff(want, string(got)); d != "" {
				t.Errorf("destination content (-want +got):\n%s", d)
			}
			if d := gocmp.Diff(before, dirNames(t, dir)); d != "" {
				t.Errorf("entries of the destination directory changed (-before +after):\n%s", d)
			}
			if tt.setup == stagingPrivate {
				if d := gocmp.Diff([]string{}, dirNames(t, filepath.Join(dir, StagingDirName))); d != "" {
					t.Errorf("entries left in the staging subdirectory (-want +got):\n%s", d)
				}
			}
		})
	}
}

// TestCreateStagingDir decides what CreateStagingDir makes and keeps: it creates a missing
// directory together with its private staging subdirectory, keeps an existing private one with
// its entries, and refuses a name there that PrivateStaging would refuse.
func TestCreateStagingDir(t *testing.T) {
	tests := map[string]struct {
		// missingParent makes the directory itself absent before the call.
		missingParent bool
		setup         stagingSetup
		// keep is a file placed in an existing private subdirectory before the call.
		keep    bool
		failing bool
		wantErr error
	}{
		"success: a missing directory is created with its private subdirectory": {missingParent: true, setup: stagingMissing},
		"success: a directory without the subdirectory gets a private one":      {setup: stagingMissing},
		"success: an existing private subdirectory is kept with its entries":    {setup: stagingPrivate, keep: true},
		"error: an existing subdirectory others can open is refused":            {setup: stagingShared, failing: true, wantErr: fs.ErrPermission},
		"error: a symbolic link at the subdirectory name is refused":            {setup: stagingSymlink, failing: true},
		"error: a regular file at the subdirectory name is refused":             {setup: stagingFile, failing: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.missingParent {
				dir = filepath.Join(dir, "absent", "cache")
			} else {
				prepareStaging(t, dir, tt.setup)
			}
			staging := filepath.Join(dir, StagingDirName)
			if tt.keep {
				if err := os.WriteFile(filepath.Join(staging, "kept"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			err := CreateStagingDir(dir, 0o777)

			if tt.failing {
				if err == nil {
					t.Fatal("CreateStagingDir succeeded, want it to refuse the existing name")
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("CreateStagingDir error %v, want one matching %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateStagingDir: %v", err)
			}
			info, err := os.Lstat(staging)
			if err != nil {
				t.Fatal(err)
			}
			if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
				t.Errorf("staging subdirectory mode %v, want a directory with no access for others", info.Mode())
			}
			wantEntries := []string{}
			if tt.keep {
				wantEntries = []string{"kept"}
			}
			if d := gocmp.Diff(wantEntries, dirNames(t, staging)); d != "" {
				t.Errorf("entries of the staging subdirectory (-want +got):\n%s", d)
			}
		})
	}
}
