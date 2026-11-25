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
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestDestinationSpelling(t *testing.T) {
	tests := map[string]struct{ write func(string) error }{"state writer": {func(path string) error { return WriteFile(path, []byte(newState), 0o600) }}, "settings writer": {func(path string) error {
		return ReplaceSettings(path, func([]byte) ([]byte, error) { return []byte(newState), nil })
	}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, p := range []string{"", dir + "/"} {
				err := tt.write(p)
				if _, ok := errors.AsType[*DestinationError](err); !ok {
					t.Fatalf("%q: got %v, want destination error", p, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("trailing slash changed directory: %v, %v", entries, err)
			}
			physical := filepath.Join(dir, "actual")
			child := filepath.Join(physical, "child")
			if err := os.MkdirAll(child, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link")
			if err := os.Symlink(child, link); err != nil {
				t.Fatal(err)
			}
			if err := tt.write(link + "/../state.json"); err != nil {
				t.Fatal(err)
			}
			checkContents(t, filepath.Join(physical, "state.json"), newState)
			if _, err := os.Stat(filepath.Join(dir, "state.json")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("lexical parent touched: %v", err)
			}
		})
	}
}

func TestFileOnlyUnlink(t *testing.T) {
	path := stateFile(t)
	root, _, err := openParent(nil, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	const name = "state.json.1.abcdef012345.tmp"
	if err := root.Mkdir(name, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(name); err == nil {
		t.Fatal("file-only unlink removed a directory")
	}
	info, err := root.Lstat(name)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory not preserved: %v", err)
	}
}

func TestStageMetadataReadback(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stage")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	owner := ownerOf(info)
	tests := map[string]struct {
		mode  fs.FileMode
		owner ownership
	}{"error: setgid not retained": {info.Mode() | fs.ModeSetgid, owner}, "error: group not retained": {info.Mode(), ownership{owner.uid, owner.gid + 1}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := verifyMetadata(f, f.Name(), tt.mode, tt.owner)
			if _, ok := errors.AsType[*OwnershipError](err); !ok {
				t.Fatalf("expected metadata refusal, got %v", err)
			}
		})
	}
}

func TestSweepRetainsSymlink(t *testing.T) {
	path := stateFile(t)
	dest := filepath.Join(filepath.Dir(path), "authored")
	writeFixture(t, dest, "authored")
	link := path + ".4242.abc012def345.tmp"
	if err := os.Symlink(dest, link); err != nil {
		t.Fatal(err)
	}
	w := newWriter()
	w.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := w.writeFile(path, []byte(newState), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(link); err != nil || got != dest {
		t.Fatalf("swept or changed link: %q %v", got, err)
	}
	checkContents(t, dest, "authored")
}

func TestSettingsParentIdentity(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "config")
	moved := filepath.Join(dir, "archived")
	path := filepath.Join(parent, "settings.json")
	w := newWriter()
	err := w.replaceSettings(path, func([]byte) ([]byte, error) { return []byte(newState), nil }, func(f preparedFile, _ string) {
		if !generatedName(filepath.Base(f.Name()), filepath.Base(path)) {
			t.Fatalf("unexpected stage name %s", f.Name())
		}
		if err := os.Rename(parent, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("accepted a different directory")
	}
	for _, p := range []string{parent, moved} {
		contents, err := os.ReadDir(p)
		if err != nil || len(contents) != 0 {
			t.Fatalf("unexpected contents of %s: %v %v", p, contents, err)
		}
	}
}

func TestExistingSettingsParentChangesDuringRead(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "config")
	newDir := filepath.Join(dir, "new-config")
	oldDir := filepath.Join(dir, "old-config")
	const file = "settings.json"
	for _, d := range []string{parent, newDir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(d, file), oldState)
	}
	err := newWriter().replaceSettings(filepath.Join(parent, file), func([]byte) ([]byte, error) { return []byte(newState), nil }, func(preparedFile, string) {
		if err := os.Rename(parent, oldDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(newDir, parent); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Fatal("published into detached parent")
	}
	for _, d := range []string{parent, oldDir} {
		path := filepath.Join(d, file)
		checkContents(t, path, oldState)
		checkOnlyTarget(t, path)
	}
}

func TestOwnershipRefusal(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	gid := 1
	for gid == os.Getegid() || slices.Contains(groups, gid) {
		gid++
	}
	tests := map[string]struct{ otherUser bool }{"error: another owner": {true}, "error: cannot keep group": {false}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "stage")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			owner := ownerOf(info)
			want := owner
			if tt.otherUser {
				want.uid++
			} else {
				want.gid = uint32(gid)
				if os.Geteuid() == 0 {
					if err := f.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = preserveOwner(f, f.Name(), owner, want)
			if _, ok := errors.AsType[*OwnershipError](err); !ok {
				t.Fatalf("want ownership refusal, got %v", err)
			}
			after, err := os.Stat(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			if ownerOf(after) != owner {
				t.Fatal("ownership changed despite refusal")
			}
		})
	}
}

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
