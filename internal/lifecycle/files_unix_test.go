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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

const (
	markerName  = "intentionally-stopped.json"
	failureName = "shim-supervisor-failure.txt"
)

type inspection struct {
	present bool
	err     error
}

// markerPresent calls StopMarkerPresent on its own goroutine. The watchdog bounds a hang (a blocking
// open of a named pipe, for example); a panic is a test failure, not an aborted binary.
func markerPresent(t testing.TB, f lifecycle.Files) inspection {
	t.Helper()
	results := make(chan inspection, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("StopMarkerPresent panicked: %v", p)
				results <- inspection{err: errors.New("StopMarkerPresent panicked")}
			}
		}()
		present, err := f.StopMarkerPresent()
		results <- inspection{present, err}
	}()
	select {
	case r := <-results:
		if t.Failed() {
			t.FailNow()
		}
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("StopMarkerPresent did not return")
		return inspection{}
	}
}

func clearFailure(t testing.TB, f lifecycle.Files) error {
	t.Helper()
	return noPanic(t, "ClearStartupFailure", f.ClearStartupFailure)
}

// rootFixture is a state root with an unrelated directory beside it; a case's setup returns the StateDir to use.
type rootFixture struct{ state, outside string }

func newRootFixture(t *testing.T) rootFixture {
	t.Helper()
	base := t.TempDir()
	f := rootFixture{state: filepath.Join(base, "state"), outside: filepath.Join(base, "outside")}
	noError(t, os.Mkdir(f.state, 0o700))
	noError(t, os.Mkdir(f.outside, 0o700))
	// A relative or unclean StateDir must not resolve against the test's working directory either.
	t.Chdir(base)
	return f
}

// lockOut sets the state root's mode for the call under test; the returned function and the test's
// cleanup both restore it.
func lockOut(t *testing.T, dir string, mode *fs.FileMode) func() {
	t.Helper()
	if mode == nil {
		return func() {}
	}
	noError(t, os.Chmod(dir, *mode))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return func() { noError(t, os.Chmod(dir, 0o700)) }
}

// modeDenies fails the test unless the file mode a case set really denies the access the case relies on. A process
// that file modes do not bind (the superuser, or one with an override capability) would make such a case pass or
// fail for a reason other than the behaviour under test. try attempts the access and returns its error.
func modeDenies(t *testing.T, access string, try func() error) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Fatalf("the venue must not run as root: this case needs a file mode that denies %s", access)
	}
	if err := try(); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("the file mode did not deny %s (error %v): this process is not bound by file modes", access, err)
	}
}

func TestStopMarkerPresent(t *testing.T) {
	tests := map[string]struct {
		setup func(t *testing.T, f rootFixture) (stateDir string)
		// lock, when set, is the state root's mode during the call.
		lock *fs.FileMode
		// denied, when set, confirms that the case's file modes deny the access it relies on.
		denied  func(t *testing.T, f rootFixture)
		present bool
		wantErr bool
		// stopped is the unchanged answer of the existing boolean helper for the same tree.
		stopped bool
	}{
		"success: absent marker": {
			setup: func(t *testing.T, f rootFixture) string { return f.state },
		},
		"success: published marker": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, filesFor(f.state).Stop())
				return f.state
			},
			present: true, stopped: true,
		},
		"success: marker body is not parsed": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, atomicfile.WriteFile(filepath.Join(f.state, markerName), []byte("not JSON"), 0o600))
				return f.state
			},
			present: true, stopped: true,
		},
		"success: empty marker": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, atomicfile.WriteFile(filepath.Join(f.state, markerName), nil, 0o600))
				return f.state
			},
			present: true, stopped: true,
		},
		"success: marker without read permission": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, filesFor(f.state).Stop())
				noError(t, os.Chmod(filepath.Join(f.state, markerName), 0))
				return f.state
			},
			denied: func(t *testing.T, f rootFixture) {
				modeDenies(t, "reading the marker", func() error {
					file, err := os.Open(filepath.Join(f.state, markerName))
					if err == nil {
						_ = file.Close()
					}
					return err
				})
			},
			present: true, stopped: true,
		},
		"error: directory at the marker": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, os.Mkdir(filepath.Join(f.state, markerName), 0o700))
				return f.state
			},
			wantErr: true, stopped: true,
		},
		"error: symbolic link to a regular marker": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, filesFor(f.outside).Stop())
				noError(t, os.Symlink(filepath.Join(f.outside, markerName), filepath.Join(f.state, markerName)))
				return f.state
			},
			wantErr: true, stopped: true,
		},
		"error: dangling symbolic link": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, os.Symlink(filepath.Join(f.outside, "absent"), filepath.Join(f.state, markerName)))
				return f.state
			},
			wantErr: true,
		},
		"error: named pipe at the marker": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, unix.Mkfifo(filepath.Join(f.state, markerName), 0o600))
				return f.state
			},
			wantErr: true, stopped: true,
		},
		"error: state root not searchable": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, filesFor(f.state).Stop())
				return f.state
			},
			lock: new(fs.FileMode(0)),
			denied: func(t *testing.T, f rootFixture) {
				modeDenies(t, "looking up the marker in the state root", func() error {
					_, err := os.Lstat(filepath.Join(f.state, markerName))
					return err
				})
			},
			wantErr: true,
		},
		"error: state root is a regular file": {
			setup: func(t *testing.T, f rootFixture) string {
				root := filepath.Join(f.outside, "file")
				noError(t, os.WriteFile(root, []byte("not a directory"), 0o600))
				return root
			},
			wantErr: true,
		},
		"error: relative state root": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, filesFor(f.state).Stop())
				return "state"
			},
			wantErr: true,
		},
		"error: unclean state root": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, filesFor(f.state).Stop())
				return f.state + "/."
			},
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newRootFixture(t)
			files := filesFor(tt.setup(t, fixture))
			before := treeSnapshot(t, fixture)
			restore := lockOut(t, fixture.state, tt.lock)
			if tt.denied != nil {
				tt.denied(t, fixture)
			}
			equal(t, tt.stopped, files.Stopped())
			got := markerPresent(t, files)
			restore()
			if tt.wantErr {
				// The boolean beside an error is not specified; a caller never starts a worker on an error.
				if got.err == nil {
					t.Fatalf("StopMarkerPresent() = %t, nil; want an inspection error", got.present)
				}
			} else {
				if got.err != nil {
					t.Fatalf("StopMarkerPresent(): %v", got.err)
				}
				equal(t, tt.present, got.present)
			}
			// Inspection never creates, consumes or rewrites anything.
			equal(t, before, treeSnapshot(t, fixture))
		})
	}
}

// treeSnapshot records the entries and readable regular-file contents of the state root and its sibling.
func treeSnapshot(t *testing.T, f rootFixture) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	for _, dir := range []string{f.state, f.outside} {
		entries, err := os.ReadDir(dir)
		noError(t, err)
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			info, err := os.Lstat(path)
			noError(t, err)
			value := info.Mode().String()
			if info.Mode().IsRegular() && info.Mode().Perm()&0o400 != 0 {
				value += " " + bytesAt(t, path)
			}
			if target, err := os.Readlink(path); err == nil {
				value += " -> " + target
			}
			snapshot[path] = value
		}
	}
	return snapshot
}

func TestClearStartupFailure(t *testing.T) {
	const record = `{"supervisorPid":41,"generation":"startup","message":"bind failed"}`
	writeRecord := func(t *testing.T, state string) {
		t.Helper()
		noError(t, filesFor(state).WriteStartupFailure(41, new("startup"), "bind failed"))
		equal(t, record, bytesAt(t, filepath.Join(state, failureName)))
	}
	tests := map[string]struct {
		setup func(t *testing.T, f rootFixture) (stateDir string)
		// lock, when set, is the state root's mode during the call.
		lock *fs.FileMode
		// denied, when set, confirms that the case's file modes deny the access it relies on.
		denied  func(t *testing.T, f rootFixture)
		wantErr bool
		check   func(t *testing.T, f rootFixture)
	}{
		"success: removes a written record": {
			setup: func(t *testing.T, f rootFixture) string {
				writeRecord(t, f.state)
				return f.state
			},
			check: func(t *testing.T, f rootFixture) {
				mustBeAbsent(t, filepath.Join(f.state, failureName))
				_, ok := filesFor(f.state).StartupFailure(41, "")
				equal(t, false, ok)
			},
		},
		"success: missing record": {
			setup: func(t *testing.T, f rootFixture) string { return f.state },
			check: func(t *testing.T, f rootFixture) { mustBeAbsent(t, filepath.Join(f.state, failureName)) },
		},
		"success: malformed record": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, atomicfile.WriteFile(filepath.Join(f.state, failureName), []byte(`{"supervisorPid":`), 0o600))
				return f.state
			},
			check: func(t *testing.T, f rootFixture) { mustBeAbsent(t, filepath.Join(f.state, failureName)) },
		},
		"success: read-only record in a writable root": {
			setup: func(t *testing.T, f rootFixture) string {
				writeRecord(t, f.state)
				noError(t, os.Chmod(filepath.Join(f.state, failureName), 0o400))
				return f.state
			},
			check: func(t *testing.T, f rootFixture) { mustBeAbsent(t, filepath.Join(f.state, failureName)) },
		},
		"success: symbolic link is removed and its target kept": {
			setup: func(t *testing.T, f rootFixture) string {
				writeRecord(t, f.outside)
				noError(t, os.Symlink(filepath.Join(f.outside, failureName), filepath.Join(f.state, failureName)))
				return f.state
			},
			check: func(t *testing.T, f rootFixture) {
				mustBeAbsent(t, filepath.Join(f.state, failureName))
				equal(t, record, bytesAt(t, filepath.Join(f.outside, failureName)))
			},
		},
		"success: dangling symbolic link is removed": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, os.Symlink(filepath.Join(f.outside, "absent"), filepath.Join(f.state, failureName)))
				return f.state
			},
			check: func(t *testing.T, f rootFixture) {
				mustBeAbsent(t, filepath.Join(f.state, failureName))
				mustBeAbsent(t, filepath.Join(f.outside, "absent"))
			},
		},
		"error: empty directory at the record": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, os.Mkdir(filepath.Join(f.state, failureName), 0o700))
				return f.state
			},
			wantErr: true,
			check: func(t *testing.T, f rootFixture) {
				info, err := os.Lstat(filepath.Join(f.state, failureName))
				noError(t, err)
				if !info.IsDir() {
					t.Fatalf("record path has mode %v after a failed clear, want the directory kept", info.Mode())
				}
			},
		},
		"error: directory with an entry at the record": {
			setup: func(t *testing.T, f rootFixture) string {
				noError(t, os.Mkdir(filepath.Join(f.state, failureName), 0o700))
				noError(t, os.WriteFile(filepath.Join(f.state, failureName, "entry"), []byte("entry"), 0o600))
				return f.state
			},
			wantErr: true,
			check: func(t *testing.T, f rootFixture) {
				equal(t, "entry", bytesAt(t, filepath.Join(f.state, failureName, "entry")))
			},
		},
		"error: state root not writable": {
			setup: func(t *testing.T, f rootFixture) string {
				writeRecord(t, f.state)
				return f.state
			},
			lock: new(fs.FileMode(0o500)),
			denied: func(t *testing.T, f rootFixture) {
				modeDenies(t, "changing the entries of the state root", func() error {
					probe := filepath.Join(f.state, "mode-probe")
					file, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
					if err == nil {
						_ = file.Close()
						_ = os.Remove(probe)
					}
					return err
				})
			},
			wantErr: true,
			check: func(t *testing.T, f rootFixture) {
				msg, ok := filesFor(f.state).StartupFailure(41, "startup")
				equal(t, true, ok)
				equal(t, "bind failed", msg)
			},
		},
		// A file where the state root should be is not a confirmed absence of the record.
		"error: state root is a regular file": {
			setup: func(t *testing.T, f rootFixture) string {
				root := filepath.Join(f.outside, "file")
				noError(t, os.WriteFile(root, []byte("not a directory"), 0o600))
				return root
			},
			wantErr: true,
			check: func(t *testing.T, f rootFixture) {
				root := filepath.Join(f.outside, "file")
				info, err := os.Lstat(root)
				noError(t, err)
				if !info.Mode().IsRegular() {
					t.Fatalf("state root has mode %v after a failed clear, want the regular file kept", info.Mode())
				}
				equal(t, "not a directory", bytesAt(t, root))
				equal(t, []string{"file"}, entryNames(t, f.outside))
			},
		},
		"error: relative state root": {
			setup: func(t *testing.T, f rootFixture) string {
				writeRecord(t, f.state)
				return "state"
			},
			wantErr: true,
			check:   func(t *testing.T, f rootFixture) { equal(t, record, bytesAt(t, filepath.Join(f.state, failureName))) },
		},
		"error: unclean state root": {
			setup: func(t *testing.T, f rootFixture) string {
				writeRecord(t, f.state)
				return f.state + "/."
			},
			wantErr: true,
			check:   func(t *testing.T, f rootFixture) { equal(t, record, bytesAt(t, filepath.Join(f.state, failureName))) },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newRootFixture(t)
			files := filesFor(tt.setup(t, fixture))
			// Neighbouring lifecycle evidence that clearing must leave alone.
			neighbour := filesFor(fixture.state)
			noError(t, neighbour.Stop())
			neighbour.Record("tick", nil)
			marker := bytesAt(t, filepath.Join(neighbour.StateDir, markerName))
			journal := bytesAt(t, filepath.Join(neighbour.StateDir, "logs", "lifecycle.jsonl"))
			restore := lockOut(t, fixture.state, tt.lock)
			if tt.denied != nil {
				tt.denied(t, fixture)
			}
			err := clearFailure(t, files)
			restore()
			if tt.wantErr {
				if err == nil {
					t.Fatal("ClearStartupFailure() = nil, want an error")
				}
				if tt.lock != nil && !errors.Is(err, fs.ErrPermission) {
					t.Fatalf("ClearStartupFailure() = %v, want a permission error", err)
				}
			} else if err != nil {
				t.Fatalf("ClearStartupFailure(): %v", err)
			}
			tt.check(t, fixture)
			equal(t, marker, bytesAt(t, filepath.Join(neighbour.StateDir, markerName)))
			equal(t, journal, bytesAt(t, filepath.Join(neighbour.StateDir, "logs", "lifecycle.jsonl")))

			if !tt.wantErr {
				// Clearing again finds nothing and stays silent.
				noError(t, clearFailure(t, files))
				// The existing writer and reader work unchanged after a clear.
				writeRecord(t, fixture.state)
				msg, ok := filesFor(fixture.state).StartupFailure(41, "startup")
				equal(t, true, ok)
				equal(t, "bind failed", msg)
			}
		})
	}
}
