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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDirRewriteFile(t *testing.T) {
	tests := map[string]struct{ replacement string }{
		"success: shorter": {"new"},
		"success: longer":  {"substantially longer than the old contents"},
		"success: empty":   {""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "journal")
			writeFixture(t, path, "old contents")
			d := openTestDir(t, dir)
			appender, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = appender.Close() }()
			before, err := appender.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := d.RewriteFile("journal", []byte(tt.replacement)); err != nil {
				t.Fatal(err)
			}
			if _, err := appender.WriteString("\nrecord"); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || before.Mode() != after.Mode() || ownerOf(before) != ownerOf(after) {
				t.Errorf("file identity or metadata changed: before %v, after %v", before, after)
			}
			checkContents(t, path, tt.replacement+"\nrecord")
		})
	}
}

func TestDirRewriteFileMovedParent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("state", 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, "state/journal", oldState)
	d := openTestDir(t, "state")
	if err := os.Rename("state", "moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("state", 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, "state/journal", "untouched")
	if err := d.RewriteFile("journal", []byte(newState)); err != nil {
		t.Fatal(err)
	}
	checkContents(t, "moved/journal", newState)
	checkContents(t, "state/journal", "untouched")
}

func TestDirRewriteFileRefusals(t *testing.T) {
	dir := t.TempDir()
	d := openTestDir(t, dir)
	writeFixture(t, filepath.Join(dir, "regular"), oldState)
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"link": "regular", "dangling": "missing"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	tests := map[string]struct {
		d    *Dir
		name string
	}{
		"error: symlink":          {d, "link"},
		"error: dangling symlink": {d, "dangling"},
		"error: directory":        {d, "directory"},
		"error: device":           {openTestDir(t, "/dev"), "null"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := tt.d.rewriteFile(tt.name, []byte(newState), func(int, []byte) (int, error) {
				t.Fatal("attempted to write a nonregular entry")
				return 0, nil
			})
			if refusal, ok := errors.AsType[*NonRegularError](err); !ok || refusal.Path != tt.d.root.path(tt.name) {
				t.Fatalf("want NonRegularError for %q, got %v", tt.name, err)
			}
		})
	}
	checkContents(t, filepath.Join(dir, "regular"), oldState)
	if err := d.RewriteFile("missing", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file created: %v", err)
	}
	for _, invalid := range []string{"", ".", "..", "sub/leaf", "nul\x00", "state.1.0123456789ab.tmp", "state.1.0123456789AB.TmP"} {
		err := d.RewriteFile(invalid, nil)
		if dest, ok := errors.AsType[*DestinationError](err); !ok || dest.Path != invalid {
			t.Errorf("want DestinationError for %q, got %v", invalid, err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for name, handle := range map[string]*Dir{"closed": d, "zero": {}} {
		if err := handle.RewriteFile("regular", nil); !errors.Is(err, fs.ErrClosed) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDirRewriteShortWrites(t *testing.T) {
	const data = "1234567"
	tests := map[string]struct {
		stop int
		err  error
	}{
		"success: multiple short writes":    {len(data), nil},
		"error: immediately after truncate": {0, unix.ENOSPC},
		"error: after partial write":        {4, unix.ENOSPC},
		"error: write made no progress":     {2, io.ErrShortWrite},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "journal")
			writeFixture(t, path, "old contents")
			d := openTestDir(t, dir)
			written := 0
			err := d.rewriteFile("journal", []byte(data), func(fd int, p []byte) (int, error) {
				if written == tt.stop {
					if errors.Is(tt.err, io.ErrShortWrite) {
						return 0, nil
					}
					return -1, tt.err
				}
				n, err := unix.Write(fd, p[:min(len(p), 2, tt.stop-written)])
				written += n
				return n, err
			})
			if !errors.Is(err, tt.err) || err != nil && !strings.Contains(err.Error(), fmt.Sprintf("wrote %d of %d bytes", tt.stop, len(data))) {
				t.Fatalf("rewrite error: got %v, want %v after %d bytes", err, tt.err, tt.stop)
			}
			checkContents(t, path, data[:tt.stop])
		})
	}
}
