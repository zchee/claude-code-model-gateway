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
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestOpenDirCreatePath(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll("target/child", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod("target/child", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target/child", "link"); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ path, want string }{
		"success: existing directory":                  {"target/child", "target/child"},
		"success: absolute path through symlink":       {filepath.Join(root, "link", "absolute"), "target/child/absolute"},
		"success: relative path with trailing slashes": {".//relative/./child//", "relative/child"},
		"success: dot-dot after symlink":               {"link/../sibling", "target/sibling"},
		"success: final symlink":                       {"link", "target/child"},
		"success: current directory":                   {".", "."},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d, err := OpenDirCreate(tt.path, 0o700)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = d.Close() }()
			if err := d.WriteFile("marker", []byte(tt.path), 0o600); err != nil {
				t.Fatal(err)
			}
			checkContents(t, filepath.Join(tt.want, "marker"), tt.path)
		})
	}
	info, err := os.Stat("target/child")
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(fs.FileMode(0o750), info.Mode().Perm()); diff != "" {
		t.Errorf("existing directory mode (-want +got):\n%s", diff)
	}
}

func TestOpenDirCreateRefusals(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFixture(t, "file", oldState)
	if err := os.Symlink("absent", "dangling"); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ path string }{
		"error: empty":                              {""},
		"error: regular file":                       {"file"},
		"error: regular file parent":                {"file/child"},
		"error: dangling symlink":                   {"dangling"},
		"error: dangling symlink parent":            {"dangling/child"},
		"error: failure after creating a directory": {"created/../file/child"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d, err := OpenDirCreate(tt.path, 0o700)
			if d != nil {
				_ = d.Close()
			}
			if err == nil || d != nil {
				t.Fatalf("OpenDirCreate(%q) = %v, %v; want nil and an error", tt.path, d, err)
			}
		})
	}
	checkContents(t, "file", oldState)
	if _, err := os.Lstat("absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dangling symlink target: %v", err)
	}
	if entries, err := os.ReadDir("created"); err != nil || len(entries) != 0 {
		t.Errorf("directory created before failure: %v, %v", entries, err)
	}
}

func TestOpenDirCreateMovedDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	d, err := OpenDirCreate("original", 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := os.Rename("original", "moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("original", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFile("marker", []byte(newState), 0o600); err != nil {
		t.Fatal(err)
	}
	checkContents(t, "moved/marker", newState)
	if entries, err := os.ReadDir("original"); err != nil || len(entries) != 0 {
		t.Fatalf("replacement directory: %v, %v", entries, err)
	}
}

func TestOpenDirCreateMovedAncestor(t *testing.T) {
	t.Chdir(t.TempDir())
	d, err := openDirCreate("parent/child", 0o700, func(component string) {
		if component == "parent" {
			if err := os.Rename("parent", "moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir("parent", 0o700); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if err := d.WriteFile("marker", []byte(newState), 0o600); err != nil {
		t.Fatal(err)
	}
	checkContents(t, "moved/child/marker", newState)
	if entries, err := os.ReadDir("parent"); err != nil || len(entries) != 0 {
		t.Fatalf("replacement ancestor: %v, %v", entries, err)
	}
}

func TestOpenDirCreateConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one", "two", "three")
	names := [...]string{"first", "second"}
	var info [len(names)]fs.FileInfo
	var ready, creators sync.WaitGroup
	ready.Add(len(names))
	for i, name := range names {
		creators.Go(func() {
			ready.Done()
			ready.Wait()
			d, err := OpenDirCreate(path, 0o700)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = d.Close() }()
			info[i], err = d.root.handle.Stat()
			if err != nil {
				t.Error(err)
			}
			if err := d.WriteFile(name, []byte(name), 0o600); err != nil {
				t.Error(err)
			}
		})
	}
	creators.Wait()
	if !os.SameFile(info[0], info[1]) {
		t.Fatal("creators did not open the same device and inode")
	}
	for _, name := range names {
		checkContents(t, filepath.Join(path, name), name)
	}
}
