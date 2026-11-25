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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestAppendFile(t *testing.T) {
	dir := t.TempDir()
	// No sibling is staged, so the destination may exceed the staging name limit.
	path := filepath.Join(dir, strings.Repeat("x", 240))
	control := filepath.Join(dir, "control")
	if err := os.WriteFile(control, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := AppendFile(path, []byte(oldState), 0o666); err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(exclusiveStat(t, control).Mode, exclusiveStat(t, path).Mode); diff != "" {
		t.Fatalf("creation mode (-want +got):\n%s", diff)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendFile(path, []byte(newState), 0); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
		t.Fatalf("append changed inode or mode: before=%v after=%v err=%v", before, after, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(oldState+newState, string(data)); diff != "" {
		t.Fatalf("old descriptor (-want +got):\n%s", diff)
	}
}

func TestAppendFileConcurrentRecords(t *testing.T) {
	const writers, records = 2, 128
	path := filepath.Join(t.TempDir(), "journal")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	start, done := make(chan struct{}), make(chan error, writers)
	want := map[string]int{}
	for writer := range writers {
		var lines []string
		for record := range records {
			line := fmt.Sprintf("%d:%d:%s\n", writer, record, strings.Repeat("data", 1024))
			lines = append(lines, line)
			want[line]++
		}
		go func() {
			<-start
			for _, line := range lines {
				if err := AppendFile(path, []byte(line), 0o600); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	close(start)
	for range writers {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for line := range strings.Lines(string(data)) {
		got[line]++
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("journal records (-want +got):\n%s", diff)
	}
}

func TestAppendFileRefusals(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "target"), oldState)
	for link, target := range map[string]string{"link": "target", "dangling": "missing"} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	tests := map[string]struct{ path string }{
		"error: regular target symlink": {filepath.Join(dir, "link")},
		"error: dangling symlink":       {filepath.Join(dir, "dangling")},
		"error: directory":              {dir},
		"error: device":                 {os.DevNull},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := AppendFile(tt.path, []byte(newState), 0o600)
			if e, ok := errors.AsType[*NonRegularError](err); !ok || e.Path != tt.path {
				t.Fatalf("want NonRegularError for %q, got %v", tt.path, err)
			}
		})
	}
	checkContents(t, filepath.Join(dir, "target"), oldState)
	if _, err := os.Lstat(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dangling link's target: %v", err)
	}
}

func TestAppendFileNames(t *testing.T) {
	dir := t.TempDir()
	d := openTestDir(t, dir)
	tests := map[string]struct{ path, name string }{
		"error: empty":              {"", ""},
		"error: trailing separator": {dir + "/file/", "file/"},
		"error: dot":                {dir + "/missing/.", "."},
		"error: dot-dot":            {dir + "/missing/..", ".."},
		"error: reserved sibling":   {dir + "/missing/log.42.0123456789ab.tmp", "log.42.0123456789ab.tmp"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if e, ok := errors.AsType[*DestinationError](AppendFile(tt.path, nil, 0o600)); !ok || e.Path != tt.path {
				t.Fatalf("want DestinationError for path %q, got %v", tt.path, e)
			}
			if e, ok := errors.AsType[*DestinationError](d.AppendFile(tt.name, nil, 0o600)); !ok || e.Path != tt.name {
				t.Fatalf("want DestinationError for name %q, got %v", tt.name, e)
			}
		})
	}
	if err := d.AppendFile("nested/file", nil, 0o600); err == nil {
		t.Fatal("accepted multiple components")
	} else if _, ok := errors.AsType[*DestinationError](err); !ok {
		t.Fatal(err)
	}
	if err := AppendFile(dir+"/missing/file", nil, 0o600); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing parent: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("refusal changed directory: %v, %v", entries, err)
	}
}

func TestAppendFileParent(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(child, link); err != nil {
		t.Fatal(err)
	}
	if err := AppendFile(link+"/../journal", []byte(oldState), 0o600); err != nil {
		t.Fatal(err)
	}
	checkContents(t, filepath.Join(parent, "journal"), oldState)
	d := openTestDir(t, parent)
	moved := filepath.Join(dir, "moved")
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.AppendFile("journal", []byte(newState), 0o600); err != nil {
		t.Fatal(err)
	}
	checkContents(t, filepath.Join(moved, "journal"), oldState+newState)
	if _, err := os.Lstat(filepath.Join(parent, "journal")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("replaced parent: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct{ handle *Dir }{"error: closed": {d}, "error: zero": {new(Dir)}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if err := tt.handle.AppendFile("journal", nil, 0o600); !errors.Is(err, fs.ErrClosed) {
				t.Fatalf("want fs.ErrClosed, got %v", err)
			}
		})
	}
}
