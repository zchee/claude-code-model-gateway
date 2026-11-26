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
	"runtime"
	"strconv"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func TestOpenDirectoryFileMoved(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("ancestor/writable", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod("ancestor", 0o111); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod("ancestor", 0o700); err != nil {
			t.Error(err)
		}
	}()
	if _, err := os.ReadDir("ancestor"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("venue must enforce search-only permissions; ReadDir returned %v", err)
	}
	const path = "ancestor/writable/state"
	f, err := OpenDirectoryFile(path, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkdirat(int(f.Fd()), "child", 0o700); err != nil {
		t.Fatal(err)
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff([]string{"child"}, names); diff != "" {
		t.Fatal(diff)
	}
	if info, err := os.Stat(path + "-moved/child"); err != nil || !info.IsDir() {
		t.Fatalf("original directory's child: %v, %v", info, err)
	}
	if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
		t.Fatalf("replacement directory: %v, %v", entries, err)
	}
}

func TestOpenDirectoryFileErrorsCloseDescriptors(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("parent/readonly", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod("parent/readonly", 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod("parent/readonly", 0o700) }()
	writeFixture(t, "parent/file", oldState)
	if err := os.Symlink("absent", "parent/link"); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ path string }{
		"error: empty starting path":          {""},
		"error: non-directory after open":     {"parent/file"},
		"error: creation denied":              {"parent/readonly/new"},
		"error: reopen after competing entry": {"parent/link"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f, err := OpenDirectoryFile(tt.path, 0o700)
			if f != nil {
				_ = f.Close()
			}
			if f != nil || err == nil {
				t.Fatalf("OpenDirectoryFile(%q) = %v, %v; want nil, error", tt.path, f, err)
			}
		})
	}
	// Check only this walk's directories: other process descriptors may change independently.
	fdDir := "/dev/fd"
	if runtime.GOOS == "linux" {
		fdDir = "/proc/self/fd"
	}
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".", "parent", "parent/readonly"} {
		var want unix.Stat_t
		if err := unix.Stat(path, &want); err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			fd, err := strconv.Atoi(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			var got unix.Stat_t
			if err := unix.Fstat(fd, &got); errors.Is(err, unix.EBADF) {
				continue
			} else if err != nil {
				t.Fatal(err)
			}
			if got.Dev == want.Dev && got.Ino == want.Ino {
				t.Errorf("descriptor %d for %q still open after error", fd, path)
			}
		}
	}
}
