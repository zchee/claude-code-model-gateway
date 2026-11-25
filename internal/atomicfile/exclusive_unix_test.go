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
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"
)

func TestCreateExclusive(t *testing.T) {
	tests := map[string]struct {
		data string
		mode fs.FileMode
	}{
		"success: private binary file":  {"\x00\xffhello\n", 0o600},
		"success: exact shared mode":    {"shared", 0o666},
		"success: empty read-only file": {"", 0o400},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			if err := CreateExclusive(path, []byte(tt.data), tt.mode); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.mode, info.Mode()); diff != "" {
				t.Fatal(diff)
			}
			checkContents(t, path, tt.data)
			checkOnlyTarget(t, path)
		})
	}
}

func exclusiveStat(t *testing.T, path string) unix.Stat_t {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCreateExclusiveExisting(t *testing.T) {
	tests := map[string]struct {
		kind, content string
	}{
		"error: nonempty file":             {"file", "already here"},
		"error: empty file":                {"file", ""},
		"error: directory":                 {"directory", ""},
		"error: dangling symlink":          {"dangling", ""},
		"error: symlink to a regular file": {"link", "linked data"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			target := filepath.Join(t.TempDir(), "target")
			var targetBefore unix.Stat_t
			switch tt.kind {
			case "file":
				writeFixture(t, path, tt.content)
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "link", "dangling":
				if tt.kind == "link" {
					writeFixture(t, target, tt.content)
					targetBefore = exclusiveStat(t, target)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			before := exclusiveStat(t, path)
			if err := CreateExclusive(path, []byte("new data"), 0o640); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("want fs.ErrExist, got %v", err)
			}
			if got := exclusiveStat(t, path); got != before {
				t.Fatalf("metadata changed: %+v -> %+v", before, got)
			}
			switch tt.kind {
			case "file":
				checkContents(t, path, tt.content)
			case "directory":
				if entries, err := os.ReadDir(path); err != nil || len(entries) != 0 {
					t.Fatalf("directory contents: %v, %v", entries, err)
				}
			case "link", "dangling":
				if link, err := os.Readlink(path); err != nil || link != target {
					t.Fatalf("symlink target = %q, %v; want %q", link, err, target)
				}
				if tt.kind == "link" {
					if got := exclusiveStat(t, target); got != targetBefore {
						t.Fatalf("symlink target metadata changed: %+v -> %+v", targetBefore, got)
					}
					checkContents(t, target, tt.content)
				} else if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("dangling link no longer dangling: %v", err)
				}
			}
			checkOnlyTarget(t, path)
		})
	}
}

func TestCreateExclusiveDestination(t *testing.T) {
	dir := t.TempDir()
	tests := map[string]struct {
		path    string
		missing bool
	}{
		"error: empty":              {"", false},
		"error: trailing separator": {dir + "/", false},
		"error: final dot":          {dir + "/.", false},
		"error: final dot-dot":      {dir + "/..", false},
		"error: missing parent":     {dir + "/missing/file", true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := CreateExclusive(tt.path, []byte("data"), 0o600)
			if tt.missing {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("want missing parent error, got %v", err)
				}
			} else if dest, ok := errors.AsType[*DestinationError](err); !ok || dest.Path != tt.path {
				t.Fatalf("want DestinationError for %q, got %v", tt.path, err)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("unexpected entries: %v, %v", entries, err)
			}
		})
	}
}

func TestCreateExclusiveMovedParent(t *testing.T) {
	dir := t.TempDir()
	parent, moved := filepath.Join(dir, "parent"), filepath.Join(dir, "moved")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root, base, err := openParent(nil, filepath.Join(parent, "file"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := createExclusiveAt(root, base, []byte(newState), 0o600); err != nil {
		t.Fatal(err)
	}
	checkContents(t, filepath.Join(moved, base), newState)
	checkOnlyTarget(t, filepath.Join(moved, base))
	if entries, err := os.ReadDir(parent); err != nil || len(entries) != 0 {
		t.Fatalf("replacement parent contains %v, %v", entries, err)
	}
}

func TestCreateExclusiveRace(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type result struct {
		data string
		err  error
	}
	for range 32 {
		path := filepath.Join(t.TempDir(), "file")
		start, results := make(chan struct{}), make(chan result, 2)
		for _, data := range []string{"first", "second"} {
			go func() {
				<-start
				results <- result{data, CreateExclusive(path, []byte(data), 0o600)}
			}()
		}
		close(start)
		var winners []string
		for range 2 {
			select {
			case r := <-results:
				if r.err == nil {
					winners = append(winners, r.data)
				} else if !errors.Is(r.err, fs.ErrExist) {
					t.Fatal(r.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		if len(winners) != 1 {
			t.Fatalf("want one successful writer, got %v", winners)
		}
		checkContents(t, path, winners[0])
		checkOnlyTarget(t, path)
	}
}

func TestCreateExclusiveVisibleContents(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	data := bytes.Repeat([]byte("abcd0123"), 1<<20)
	for range 4 {
		path := filepath.Join(t.TempDir(), "file")
		start, done := make(chan struct{}), make(chan error, 1)
		go func() {
			<-start
			done <- CreateExclusive(path, data, 0o600)
		}()
		// Start polling before the writer. Check size on first sight: reading
		// to EOF alone could catch up with an in-place writer and hide a prefix.
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("expected absent destination, got %v", err)
		}
		close(start)
		for {
			info, err := os.Lstat(path)
			if err == nil {
				if info.Size() != int64(len(data)) {
					t.Fatalf("saw %d of %d bytes", info.Size(), len(data))
				}
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("first visible content differs: read %d bytes, err=%v", len(got), err)
				}
				break
			}
			if !errors.Is(err, fs.ErrNotExist) || ctx.Err() != nil {
				t.Fatalf("reader: %v, context: %v", err, ctx.Err())
			}
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
