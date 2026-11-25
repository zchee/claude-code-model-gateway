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

func openTestDir(t *testing.T, path string) *Dir {
	t.Helper()
	d, err := OpenDir(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d
}

func TestOpenDir(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "actual", "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(child, link); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "file")
	writeFixture(t, regular, oldState)
	tests := map[string]struct {
		path string
		err  error
	}{
		"success: kernel resolves symlink before dot-dot": {link + "/..", nil},
		"error: regular file":                             {regular, unix.ENOTDIR},
		"error: missing directory":                        {filepath.Join(root, "absent"), fs.ErrNotExist},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d, err := OpenDir(tt.path)
			if d != nil {
				defer func() { _ = d.Close() }()
			}
			if !errors.Is(err, tt.err) || (d == nil) != (tt.err != nil) {
				t.Fatalf("OpenDir(%q) = %v, %v; want error %v", tt.path, d, err, tt.err)
			}
			if d != nil {
				if err := d.CreateExclusive("file", []byte(newState), 0o600); err != nil {
					t.Fatal(err)
				}
				checkContents(t, filepath.Join(root, "actual", "file"), newState)
				checkContents(t, regular, oldState)
			}
		})
	}
}

func TestDirMovedParent(t *testing.T) {
	root := t.TempDir()
	before, after := filepath.Join(root, "before"), filepath.Join(root, "after")
	if err := os.Mkdir(before, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(before, "existing"), oldState)
	d := openTestDir(t, before)
	if err := os.Rename(before, after); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(before, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := d.ReadFile("existing")
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(oldState, string(data)); diff != "" {
		t.Fatal(diff)
	}
	original := exclusiveStat(t, filepath.Join(after, "existing"))
	if err := d.WriteFile("existing", []byte(newState), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := exclusiveStat(t, filepath.Join(after, "existing")); got.Ino == original.Ino {
		t.Fatal("WriteFile did not replace the inode")
	}
	if err := d.CreateExclusive("new", []byte("created"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateExclusive("new", []byte("duplicate"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: %v", err)
	}
	checkContents(t, filepath.Join(after, "existing"), newState)
	checkContents(t, filepath.Join(after, "new"), "created")
	if entries, err := os.ReadDir(before); err != nil || len(entries) != 0 {
		t.Fatalf("replacement directory: %v, %v", entries, err)
	}
	if entries, err := os.ReadDir(after); err != nil || len(entries) != 2 {
		t.Fatalf("opened directory: %v, %v", entries, err)
	}
}

func TestDirReadFileRefusals(t *testing.T) {
	dir := t.TempDir()
	d := openTestDir(t, dir)
	writeFixture(t, filepath.Join(dir, "target"), oldState)
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"symlink": "target", "dangling": "missing"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	tests := map[string]struct {
		base    string
		missing bool
	}{
		"error: symlink":          {"symlink", false},
		"error: dangling symlink": {"dangling", false},
		"error: directory":        {"directory", false},
		"error: missing file":     {"absent", true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := d.ReadFile(tt.base)
			if got != nil {
				t.Fatalf("refusal returned %q", got)
			}
			if tt.missing {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("want fs.ErrNotExist, got %v", err)
				}
			} else if refusal, ok := errors.AsType[*NonRegularError](err); !ok || refusal.Path != filepath.Join(dir, tt.base) {
				t.Fatalf("want nonregular refusal for %s, got %v", tt.base, err)
			}
		})
	}
}

func TestDirNameAndClose(t *testing.T) {
	root := t.TempDir()
	d := openTestDir(t, root)
	tests := map[string]struct{ call func(string) error }{
		"read":   {func(name string) error { _, err := d.ReadFile(name); return err }},
		"write":  {func(name string) error { return d.WriteFile(name, nil, 0o600) }},
		"create": {func(name string) error { return d.CreateExclusive(name, nil, 0o600) }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, invalid := range []string{"", ".", "..", "nested/file", "file/", "/absolute"} {
				err := tt.call(invalid)
				if dest, ok := errors.AsType[*DestinationError](err); !ok || dest.Path != invalid {
					t.Fatalf("%q: want DestinationError; got %v", invalid, err)
				}
			}
		})
	}
	for range 2 {
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for name, tt := range tests {
		if err := tt.call("file"); !errors.Is(err, fs.ErrClosed) {
			t.Errorf("%s after Close: %v", name, err)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("directory changed despite refusals: %v, %v", entries, err)
	}
}

func TestDirCompetingCreates(t *testing.T) {
	dir := t.TempDir()
	handles := [...]*Dir{openTestDir(t, dir), openTestDir(t, dir)}
	contents := [...]string{"first", "second"}
	type result struct {
		i   int
		err error
	}
	start, results := make(chan struct{}), make(chan result, len(handles))
	for i, d := range handles {
		go func() {
			<-start
			results <- result{i, d.CreateExclusive("file", []byte(contents[i]), 0o600)}
		}()
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	close(start)
	wins := 0
	for range handles {
		select {
		case r := <-results:
			if r.err == nil {
				wins++
				checkContents(t, filepath.Join(dir, "file"), contents[r.i])
			} else if !errors.Is(r.err, fs.ErrExist) {
				t.Fatal(r.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if wins != 1 {
		t.Fatalf("got %d successful creates, want one", wins)
	}
	checkOnlyTarget(t, filepath.Join(dir, "file"))
}
