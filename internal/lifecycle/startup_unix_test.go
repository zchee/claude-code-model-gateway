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

package lifecycle_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

// entryState is what a snapshot keeps of one entry: its type and permission bits, its inode number (directories
// included), the content of a readable regular file and a link's target. Comparing two snapshots shows a change to
// these recorded values: an entry removed or added, or one whose bytes, mode, inode number or target differ. It does
// not show every replacement (an unlinked entry's inode number can be reused), a write that left the same bytes in
// place, or anything done and undone between the two snapshots.
type entryState struct {
	Mode    string
	Content string
	Target  string
	Inode   uint64
}

// recursiveSnapshot records every entry below base, by path relative to base.
func recursiveSnapshot(t *testing.T, base string) map[string]entryState {
	t.Helper()
	snapshot := map[string]entryState{}
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == base {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		state := entryState{Mode: info.Mode().String(), Inode: info.Sys().(*syscall.Stat_t).Ino}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o400 != 0 {
			state.Content = bytesAt(t, path)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if state.Target, err = os.Readlink(path); err != nil {
				return err
			}
		}
		snapshot[rel] = state
		return nil
	})
	noError(t, err)
	return snapshot
}

// populateStateRoot fills the fixture's state root with the other evidence a gateway keeps there, files whose names
// resemble the record's, and a record of another state root beside it.
func populateStateRoot(t *testing.T, f rootFixture) {
	t.Helper()
	files := filesFor(f.state)
	noError(t, files.Stop())
	files.Record("tick", nil)
	// The retained start fence file.
	release := holdFence(t, newFence(t, f.state))
	noError(t, release())
	write := func(path, content string) {
		t.Helper()
		noError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		noError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	write(filepath.Join(f.state, "guardian.pid"), "4242")
	write(filepath.Join(f.state, "lifecycle.lock", token), `{"token":"`+token+`"}`)
	// The record's name one level down, a staged sibling of the record, and two near names.
	write(filepath.Join(f.state, "logs", failureName), "a record in the logs directory")
	write(filepath.Join(f.state, failureName+"."+strconv.Itoa(os.Getpid())+".0123456789ab.tmp"), "a staged record")
	write(filepath.Join(f.state, failureName+"~"), "a backup record")
	write(filepath.Join(f.state, "shim-supervisor-failure.json"), "a record with another extension")
	// Another state root's record.
	noError(t, filesFor(f.outside).WriteStartupFailure(7, new("other-root"), "another root's failure"))
	// A link to the record's name, which the clear must leave in place even when it dangles afterwards.
	noError(t, os.Symlink(failureName, filepath.Join(f.state, "record-link")))
}

func TestClearStartupFailureTouchesOnlyTheRecord(t *testing.T) {
	tests := map[string]struct {
		// record puts something at the record's name before the snapshot is taken.
		record  func(t *testing.T, f rootFixture)
		wantErr bool
		// removed reports that the record's own entry is gone afterwards.
		removed bool
	}{
		"success: a written record": {
			record: func(t *testing.T, f rootFixture) {
				noError(t, filesFor(f.state).WriteStartupFailure(41, new("startup"), "bind failed"))
			},
			removed: true,
		},
		// The leaf is unlinked, not truncated or rewritten: a second name of the same file keeps its content.
		"success: a record with a second hard link": {
			record: func(t *testing.T, f rootFixture) {
				noError(t, filesFor(f.state).WriteStartupFailure(41, new("startup"), "bind failed"))
				noError(t, os.Link(filepath.Join(f.state, failureName), filepath.Join(f.state, "record-hard-link")))
			},
			removed: true,
		},
		"success: no record": {
			record: func(*testing.T, rootFixture) {},
		},
		"error: a directory at the record": {
			record: func(t *testing.T, f rootFixture) {
				noError(t, os.Mkdir(filepath.Join(f.state, failureName), 0o700))
				noError(t, os.WriteFile(filepath.Join(f.state, failureName, "entry"), []byte("entry"), 0o600))
			},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newRootFixture(t)
			populateStateRoot(t, fixture)
			tt.record(t, fixture)
			base := filepath.Dir(fixture.state)
			want := recursiveSnapshot(t, base)
			if tt.removed {
				record := filepath.Join(filepath.Base(fixture.state), failureName)
				if _, ok := want[record]; !ok {
					t.Fatalf("fixture has no entry %s to remove", record)
				}
				delete(want, record)
			}

			err := clearFailure(t, filesFor(fixture.state))
			if tt.wantErr && err == nil {
				t.Fatal("ClearStartupFailure() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ClearStartupFailure(): %v", err)
			}
			// In the final state, every entry but the record's own has the name, mode, inode number, content and link
			// target it had before, below the state root and beside it.
			equal(t, want, recursiveSnapshot(t, base))
		})
	}
}

func TestClearStartupFailureRefusesInvalidRoot(t *testing.T) {
	tests := map[string]struct {
		// stateDir derives the StateDir under test from the fixture; the working directory is the fixture's base.
		stateDir func(f rootFixture) string
	}{
		"error: empty state root": {
			stateDir: func(rootFixture) string { return "" },
		},
		"error: state root with a trailing separator": {
			stateDir: func(f rootFixture) string { return f.state + string(filepath.Separator) },
		},
		"error: state root through a parent element": {
			stateDir: func(f rootFixture) string {
				return f.outside + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(f.state)
			},
		},
		"error: relative state root with a current element": {
			stateDir: func(f rootFixture) string { return "." + string(filepath.Separator) + filepath.Base(f.state) },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newRootFixture(t)
			base := filepath.Dir(fixture.state)
			// A record in the state root, and one in the working directory, which an unvalidated empty or relative
			// root would resolve against.
			noError(t, filesFor(fixture.state).WriteStartupFailure(41, new("startup"), "bind failed"))
			noError(t, filesFor(base).WriteStartupFailure(42, new("working-directory"), "not this one"))
			before := recursiveSnapshot(t, base)
			stateDir := tt.stateDir(fixture)

			if err := clearFailure(t, filesFor(stateDir)); err == nil {
				t.Fatalf("ClearStartupFailure() with StateDir %q = nil, want an error", stateDir)
			}
			equal(t, before, recursiveSnapshot(t, base))
		})
	}
}

// failureStep is one operation of a sequence on a state root's startup-failure record.
type failureStep func(t *testing.T, files lifecycle.Files)

func writeFailure(pid int, generation *string, message string) failureStep {
	return func(t *testing.T, files lifecycle.Files) {
		t.Helper()
		noError(t, files.WriteStartupFailure(pid, generation, message))
	}
}

func clearRecord() failureStep {
	return func(t *testing.T, files lifecycle.Files) {
		t.Helper()
		noError(t, clearFailure(t, files))
	}
}

// readFailure expects the reader's answer for pid and generation; ok false means no matching record.
func readFailure(pid int, generation, message string, ok bool) failureStep {
	return func(t *testing.T, files lifecycle.Files) {
		t.Helper()
		gotMessage, gotOK := files.StartupFailure(pid, generation)
		if gotMessage != message || gotOK != ok {
			t.Fatalf("StartupFailure(%d, %q) = %q, %v, want %q, %v", pid, generation, gotMessage, gotOK, message, ok)
		}
	}
}

func recordBytes(want string) failureStep {
	return func(t *testing.T, files lifecycle.Files) {
		t.Helper()
		equal(t, want, bytesAt(t, filepath.Join(files.StateDir, failureName)))
	}
}

func recordAbsent() failureStep {
	return func(t *testing.T, files lifecycle.Files) {
		t.Helper()
		mustBeAbsent(t, filepath.Join(files.StateDir, failureName))
	}
}

func TestStartupFailureAroundClear(t *testing.T) {
	tests := map[string]struct {
		steps []failureStep
	}{
		"success: write, clear, then every query finds nothing": {
			steps: []failureStep{
				writeFailure(41, new("gen-a"), "first"),
				readFailure(41, "gen-a", "first", true),
				readFailure(41, "", "first", true),
				clearRecord(),
				recordAbsent(),
				readFailure(41, "gen-a", "", false),
				readFailure(41, "", "", false),
				readFailure(42, "", "", false),
			},
		},
		"success: clear twice after a write": {
			steps: []failureStep{
				writeFailure(41, new("gen-a"), "first"),
				clearRecord(),
				clearRecord(),
				recordAbsent(),
				readFailure(41, "", "", false),
			},
		},
		"success: clear twice in a root that never had a record": {
			steps: []failureStep{
				clearRecord(),
				clearRecord(),
				recordAbsent(),
				readFailure(41, "", "", false),
			},
		},
		"success: a record written after a clear is the only one read": {
			steps: []failureStep{
				writeFailure(41, new("gen-a"), "first"),
				clearRecord(),
				writeFailure(43, new("gen-b"), "third"),
				recordBytes(`{"supervisorPid":43,"generation":"gen-b","message":"third"}`),
				readFailure(43, "gen-b", "third", true),
				readFailure(43, "", "third", true),
				readFailure(43, "gen-a", "", false),
				readFailure(41, "gen-a", "", false),
				readFailure(41, "", "", false),
			},
		},
		"success: a record without a generation written after a clear": {
			steps: []failureStep{
				writeFailure(41, new("gen-a"), "first"),
				clearRecord(),
				writeFailure(41, nil, "second"),
				recordBytes(`{"supervisorPid":41,"message":"second"}`),
				readFailure(41, "", "second", true),
				readFailure(41, "gen-a", "", false),
				clearRecord(),
				recordAbsent(),
				readFailure(41, "", "", false),
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			files := filesFor(newRootFixture(t).state)
			for _, step := range tt.steps {
				step(t, files)
			}
		})
	}
}
