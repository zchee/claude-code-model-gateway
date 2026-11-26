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

//go:build darwin || linux

package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func TestOpenDirCreateSearchOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("the venue must not run as root to test search-only directory permissions")
	}
	ancestor := filepath.Join(t.TempDir(), "ancestor")
	if err := os.MkdirAll(filepath.Join(ancestor, "writable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(ancestor, 0o700); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.ReadDir(ancestor); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("expected unreadable ancestor, got %v", err)
	}
	t.Chdir(ancestor)
	tests := map[string]struct{ path string }{
		"success: search-only intermediate":       {filepath.Join(ancestor, "writable", "absolute")},
		"success: search-only starting directory": {"writable/relative"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d, err := OpenDirCreate(tt.path, 0o700)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = d.Close() }()
			if err := d.WriteFile("state", []byte(newState), 0o600); err != nil {
				t.Fatal(err)
			}
			entries, err := d.root.handle.Readdirnames(-1)
			if err != nil {
				t.Fatalf("final descriptor is not readable: %v", err)
			}
			if diff := gocmp.Diff([]string{"state"}, entries); diff != "" {
				t.Errorf("directory contents (-want +got):\n%s", diff)
			}
			checkContents(t, filepath.Join(tt.path, "state"), newState)
		})
	}
}

func TestPostMkdirRefusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(root, "entry")); err != nil {
		t.Fatal(err)
	}
	d := openTestDir(t, root)
	// Model the entry visible at reopen, independently of which mkdir result led there.
	tests := map[string]struct {
		flags    int
		mkdirErr error
	}{
		"error: created intermediate":                 {traversalFlags, nil},
		"error: concurrently created intermediate":    {traversalFlags, unix.EEXIST},
		"error: created final directory":              {directoryFlags, nil},
		"error: concurrently created final directory": {directoryFlags, unix.EEXIST},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fd, err := openAfterMkdir(int(d.root.handle.Fd()), "entry", filepath.Join(root, "entry"), tt.flags, tt.mkdirErr)
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			if fd >= 0 || err == nil {
				t.Fatalf("protected reopen returned fd=%d err=%v", fd, err)
			}
		})
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("symlink target changed: %v, %v", entries, err)
	}
}
