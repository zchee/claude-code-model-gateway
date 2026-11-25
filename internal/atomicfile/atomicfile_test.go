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
	"io/fs"
	"os"
	"path/filepath"
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

func interceptRename(t *testing.T, rename func(string, string) error) {
	t.Helper()
	original := renameFile
	renameFile = rename
	t.Cleanup(func() { renameFile = original })
}

func TestRetryTargetLock(t *testing.T) {
	path := stateFile(t)
	attempts := 0
	interceptRename(t, func(from, to string) error {
		if to == path {
			attempts++
			if attempts == 1 {
				return syscall.EBUSY
			}
		}
		return os.Rename(from, to)
	})
	err := WriteFile(path, []byte(newState), 0o600)
	if err != nil || attempts != 2 {
		t.Errorf("WriteFile after one busy rename: attempts=%d, err=%v; want two attempts and success", attempts, err)
	}
	checkContents(t, path, newState)
}

func TestFailedReplace(t *testing.T) {
	path := stateFile(t)
	attempts := 0
	interceptRename(t, func(from, to string) error {
		if to != path || from == to || filepath.Dir(from) != filepath.Dir(path) {
			t.Fatalf("unexpected rename: %q -> %q", from, to)
		}
		checkContents(t, from, newState)
		attempts++
		return syscall.EPERM
	})
	err := WriteFile(path, []byte(newState), 0o600)
	t.Run("PermissionError", func(t *testing.T) {
		if !errors.Is(err, syscall.EPERM) {
			t.Errorf("WriteFile error = %v, want rename permission error", err)
		}
	})
	t.Run("NoTemporarySibling", func(t *testing.T) {
		if attempts == 0 {
			t.Errorf("WriteFile never reached rename: %v", err)
		}
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
	})
}

func TestSweepTemporarySiblings(t *testing.T) {
	path := stateFile(t)
	stale, live := path+".4242.abandoned.tmp", path+".4243.in-flight.tmp"
	writeFixture(t, stale, "stale")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, live, "live")
	err := WriteFile(path, []byte(newState), 0o600)
	tests := map[string]struct {
		path   string
		exists bool
	}{
		"RemovesStale": {stale, false},
		"RetainsLive":  {live, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if err != nil {
				t.Errorf("WriteFile: %v", err)
			}
			_, statErr := os.Stat(tt.path)
			if tt.exists && statErr != nil || !tt.exists && !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("stat %q = %v; want exists=%v", tt.path, statErr, tt.exists)
			}
		})
	}
}
