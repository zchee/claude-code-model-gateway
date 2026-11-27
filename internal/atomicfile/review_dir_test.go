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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

// dirRemove calls Dir.Remove and turns a panic into a test failure, so that an unimplemented method cannot abort the
// package's other tests.
func dirRemove(t *testing.T, d *Dir, name string) (err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Dir.Remove panicked: %v", p)
		}
	}()
	return d.Remove(name)
}

// dirOpenFile calls Dir.OpenFile, turns a panic into a test failure, and closes a returned file when the test ends.
func dirOpenFile(t *testing.T, d *Dir, name string, flag int, perm os.FileMode) (f *os.File, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Dir.OpenFile panicked: %v", p)
		}
		if f != nil {
			t.Cleanup(func() { _ = f.Close() })
		}
	}()
	return d.OpenFile(name, flag, perm)
}

// checkMissing fails unless path names no entry at all, not even a symlink.
func checkMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: Lstat = %v; want the entry gone", path, err)
	}
}

// TestDirRemove decides that Remove unlinks one entry of the held directory: a regular file, or a symlink as itself
// without touching its target, and never a directory.
func TestDirRemove(t *testing.T) {
	tests := map[string]struct {
		// prepare creates the entry in dir and returns the check of what must hold afterwards.
		prepare func(t *testing.T, dir string) (check func(t *testing.T))
		name    string
		wantErr error
		// refused asks for some error, of whatever kind, and the entry kept.
		refused bool
	}{
		"success: a regular file is removed": {
			prepare: func(t *testing.T, dir string) func(*testing.T) {
				writeFixture(t, filepath.Join(dir, "file"), oldState)
				return func(t *testing.T) { checkMissing(t, filepath.Join(dir, "file")) }
			},
			name: "file",
		},
		"error: a missing entry does not exist": {
			prepare: func(t *testing.T, dir string) func(*testing.T) { return nil },
			name:    "absent", wantErr: fs.ErrNotExist,
		},
		"success: a symlink to a regular file is removed as an entry and its target is kept": {
			prepare: func(t *testing.T, dir string) func(*testing.T) {
				target := filepath.Join(t.TempDir(), "target")
				writeFixture(t, target, oldState)
				if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
				return func(t *testing.T) {
					checkMissing(t, filepath.Join(dir, "link"))
					checkContents(t, target, oldState)
				}
			},
			name: "link",
		},
		"success: a symlink to a directory is removed as an entry and the directory is kept": {
			prepare: func(t *testing.T, dir string) func(*testing.T) {
				target := t.TempDir()
				writeFixture(t, filepath.Join(target, "inside"), oldState)
				if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
					t.Fatal(err)
				}
				return func(t *testing.T) {
					checkMissing(t, filepath.Join(dir, "link"))
					checkContents(t, filepath.Join(target, "inside"), oldState)
				}
			},
			name: "link",
		},
		"error: an empty subdirectory is not removed": {
			prepare: func(t *testing.T, dir string) func(*testing.T) {
				sub := filepath.Join(dir, "sub")
				if err := os.Mkdir(sub, 0o700); err != nil {
					t.Fatal(err)
				}
				return func(t *testing.T) {
					if info, err := os.Lstat(sub); err != nil || !info.IsDir() {
						t.Errorf("the subdirectory after Remove: %v, %v; want it kept", info, err)
					}
				}
			},
			name: "sub", refused: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			check := tt.prepare(t, dir)
			d := openTestDir(t, dir)
			err := dirRemove(t, d, tt.name)
			switch {
			case tt.refused:
				if err == nil {
					t.Errorf("Remove(%q) succeeded; want a refusal", tt.name)
				}
			case !errors.Is(err, tt.wantErr) || (err == nil) != (tt.wantErr == nil):
				t.Errorf("Remove(%q) = %v; want %v", tt.name, err, tt.wantErr)
			}
			if check != nil {
				check(t)
			}
		})
	}
}

// TestDirRemoveActsOnTheHeldDirectory decides that Remove unlinks from the directory the Dir holds, not from
// whatever directory the original path names by then.
func TestDirRemoveActsOnTheHeldDirectory(t *testing.T) {
	parent := t.TempDir()
	held, moved := filepath.Join(parent, "state"), filepath.Join(parent, "state.moved")
	if err := os.Mkdir(held, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(held, "entry"), oldState)
	d := openTestDir(t, held)
	if err := os.Rename(held, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(held, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(held, "entry"), newState)
	if err := dirRemove(t, d, "entry"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	checkMissing(t, filepath.Join(moved, "entry"))
	checkContents(t, filepath.Join(held, "entry"), newState)
}

// TestDirOpenFile decides that OpenFile opens one entry of the held directory: what it creates and writes reads back
// through the Dir, a symlink entry is refused whatever its target, and the descriptor is close-on-exec even when the
// caller did not ask for it.
func TestDirOpenFile(t *testing.T) {
	t.Run("success: a created and written file reads back through the Dir", func(t *testing.T) {
		dir := t.TempDir()
		d := openTestDir(t, dir)
		f, err := dirOpenFile(t, d, "created", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		if _, err := io.WriteString(f, newState); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := d.ReadFile("created")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if diff := gocmp.Diff(newState, string(got)); diff != "" {
			t.Errorf("read back (-want +got):\n%s", diff)
		}
		checkContents(t, filepath.Join(dir, "created"), newState)
	})
	tests := map[string]int{
		"error: a symlink to a regular file is refused for reading":            os.O_RDONLY,
		"error: a symlink to a regular file is refused for truncating writes":  os.O_WRONLY | os.O_TRUNC,
		"error: a symlink to a regular file is refused for reading and writes": os.O_RDWR | os.O_APPEND,
	}
	for name, flag := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(t.TempDir(), "target")
			writeFixture(t, target, oldState)
			if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			d := openTestDir(t, dir)
			f, err := dirOpenFile(t, d, "link", flag, 0o600)
			if f != nil || !errors.Is(err, unix.ELOOP) {
				t.Errorf("OpenFile(link, %#x) = %v, %v; want no file and ELOOP, since O_NOFOLLOW is always added", flag, f, err)
			}
			checkContents(t, target, oldState)
		})
	}
	t.Run("success: the descriptor is close-on-exec without the caller asking", func(t *testing.T) {
		dir := t.TempDir()
		writeFixture(t, filepath.Join(dir, "file"), oldState)
		d := openTestDir(t, dir)
		f, err := dirOpenFile(t, d, "file", os.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
		if err != nil {
			t.Fatal(err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Errorf("descriptor flags %#x lack FD_CLOEXEC", flags)
		}
	})
}

// TestDirOpenFileActsOnTheHeldDirectory decides that OpenFile opens and creates in the directory the Dir holds, not
// in whatever directory the original path names by then.
func TestDirOpenFileActsOnTheHeldDirectory(t *testing.T) {
	parent := t.TempDir()
	held, moved := filepath.Join(parent, "state"), filepath.Join(parent, "state.moved")
	if err := os.Mkdir(held, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(held, "existing"), oldState)
	d := openTestDir(t, held)
	if err := os.Rename(held, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(held, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(held, "existing"), newState)
	f, err := dirOpenFile(t, d, "existing", os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile(existing): %v", err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(oldState, string(got)); diff != "" {
		t.Errorf("OpenFile read the entry at the original path instead of the held one (-want +got):\n%s", diff)
	}
	created, err := dirOpenFile(t, d, "created", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(created): %v", err)
	}
	if _, err := io.WriteString(created, "created"); err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	checkContents(t, filepath.Join(moved, "created"), "created")
	checkMissing(t, filepath.Join(held, "created"))
	checkContents(t, filepath.Join(held, "existing"), newState)
}

// TestDirRemoveAndOpenFileNames decides that Remove and OpenFile refuse every name the Dir's other methods refuse,
// with the same DestinationError and without touching anything, and that a closed Dir refuses them with
// fs.ErrClosed.
func TestDirRemoveAndOpenFileNames(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(parent, "sibling")
	writeFixture(t, sibling, oldState)
	if err := os.Mkdir(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(root, "nested", "file"), oldState)
	d := openTestDir(t, root)
	tests := map[string]struct {
		call func(*testing.T, *Dir, string) error
	}{
		"remove": {func(t *testing.T, d *Dir, name string) error { return dirRemove(t, d, name) }},
		"open": {func(t *testing.T, d *Dir, name string) error {
			_, err := dirOpenFile(t, d, name, os.O_RDWR|os.O_TRUNC, 0)
			return err
		}},
		"create": {func(t *testing.T, d *Dir, name string) error {
			_, err := dirOpenFile(t, d, name, os.O_WRONLY|os.O_CREATE, 0o600)
			return err
		}},
	}
	// An absolute name points inside the test's own directory, so that a create that ignores the rule cannot reach
	// anything else.
	absolute := filepath.Join(parent, "absolute")
	invalid := []string{"", ".", "..", "nested/file", "file/", absolute, "../sibling", sibling}
	for name, tt := range tests {
		t.Run("error: "+name+" refuses names that are not one component", func(t *testing.T) {
			for _, base := range invalid {
				err := tt.call(t, d, base)
				if dest, ok := errors.AsType[*DestinationError](err); !ok || dest.Path != base {
					t.Errorf("%q: want a DestinationError naming it; got %v", base, err)
				}
			}
			checkContents(t, sibling, oldState)
			checkMissing(t, absolute)
			checkContents(t, filepath.Join(root, "nested", "file"), oldState)
		})
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Fatalf("the directory changed despite the refusals: %v, %v", entries, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for state, handle := range map[string]*Dir{"closed": d, "zero value": {}} {
		t.Run("error: a "+state+" Dir refuses every call", func(t *testing.T) {
			for name, tt := range tests {
				if err := tt.call(t, handle, "file"); !errors.Is(err, fs.ErrClosed) {
					t.Errorf("%s: want fs.ErrClosed, got %v", name, err)
				}
			}
		})
	}
	checkMissing(t, filepath.Join(root, "file"))
}
