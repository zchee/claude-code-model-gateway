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

package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

const (
	oldState = "{\"old\":true}\n"
	newState = "{\"new\":true}\n"
)

func writeFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stateFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	writeFixture(t, path, oldState)
	return path
}

func checkContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(want, string(data)); diff != "" {
		t.Errorf("%s (-want +got):\n%s", path, diff)
	}
}

func checkStagedSibling(t *testing.T, staged preparedFile, target, wantTarget string) {
	t.Helper()
	directory := func(path string) os.FileInfo {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	parent := directory(wantTarget)
	if filepath.Base(target) != filepath.Base(wantTarget) || !os.SameFile(directory(target), parent) || filepath.Base(staged.Name()) == filepath.Base(target) || !os.SameFile(directory(staged.Name()), parent) {
		t.Fatalf("staged %q for %q; want a sibling of %q", staged.Name(), target, wantTarget)
	}
}

func checkOnlyTarget(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if diff := gocmp.Diff([]string{filepath.Base(path)}, names); diff != "" {
		t.Errorf("remaining files (-want +got):\n%s", diff)
	}
}

func TestWriteFile(t *testing.T) {
	path := stateFile(t)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte(newState), 0o600); err != nil {
		t.Errorf("WriteFile: %v", err)
	}
	checkContents(t, path, newState)
	replaced, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(original, replaced) {
		t.Error("WriteFile left the original inode in place")
	}
	if got := replaced.Mode().Perm(); got != 0o600 {
		t.Errorf("WriteFile mode = %o, want 600", got)
	}
}

func TestRetryTargetLock(t *testing.T) {
	path := stateFile(t)
	attempts := 0
	w := newWriter()
	commit := w.commit
	w.commit = func(file preparedFile, to string) error {
		checkStagedSibling(t, file, to, path)
		checkContents(t, file.Name(), newState)
		attempts++
		if attempts == 1 {
			return syscall.EBUSY
		}
		return commit(file, to)
	}
	err := w.writeFile(path, []byte(newState), 0o600)
	if err != nil || attempts != 2 {
		t.Errorf("WriteFile after one busy rename: attempts=%d, err=%v; want two attempts and success", attempts, err)
	}
	checkContents(t, path, newState)
	checkOnlyTarget(t, path)
}

func TestFailedReplace(t *testing.T) {
	path := stateFile(t)
	attempts := 0
	w := newWriter()
	w.commit = func(file preparedFile, to string) error {
		checkStagedSibling(t, file, to, path)
		checkContents(t, file.Name(), newState)
		attempts++
		return fmt.Errorf("permission denied: %w", syscall.EPERM)
	}
	err := w.writeFile(path, []byte(newState), 0o600)
	t.Run("PermissionError", func(t *testing.T) {
		if !errors.Is(err, syscall.EPERM) || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("WriteFile error = %v, want EPERM with original permission denied diagnostic", err)
		}
	})
	t.Run("NoTemporarySibling", func(t *testing.T) {
		if attempts == 0 {
			t.Errorf("WriteFile never reached rename: %v", err)
		}
		checkContents(t, path, oldState)
		checkOnlyTarget(t, path)
	})
}

func TestSweepTemporarySiblings(t *testing.T) {
	path := stateFile(t)
	instant := time.Unix(1_600_000_000, 0)
	w := newWriter()
	w.now = func() time.Time { return instant }
	tests := map[string]struct {
		suffix string
		age    time.Duration
		exists bool
	}{
		// Only names of the shape a writer generates (pid, twelve hex digits)
		// are swept; any other sibling may be the user's file.
		"RemovesStale":             {".4242.0123456789ab.tmp", 2 * time.Hour, false},
		"RetainsLive":              {".4243.123456789abc.tmp", 0, true},
		"RemovesAtThreshold":       {".4244.23456789abcd.tmp", time.Hour, false},
		"RetainsBelowThreshold":    {".4245.3456789abcde.tmp", time.Hour - time.Second, true},
		"RetainsUnrelatedStale":    {".backup.tmp", 2 * time.Hour, true},
		"RetainsForeignShapeStale": {".4242.abandoned.tmp", 2 * time.Hour, true},
	}
	for _, tt := range tests {
		sibling := path + tt.suffix
		writeFixture(t, sibling, "temporary")
		stamp := w.now().Add(-tt.age)
		if err := os.Chtimes(sibling, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	err := w.writeFile(path, []byte(newState), 0o600)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if err != nil {
				t.Errorf("WriteFile: %v", err)
			}
			sibling := path + tt.suffix
			_, statErr := os.Stat(sibling)
			if tt.exists && statErr != nil || !tt.exists && !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("stat %q = %v; want exists=%v", sibling, statErr, tt.exists)
			}
		})
	}
}
