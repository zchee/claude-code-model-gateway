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
	"golang.org/x/sys/unix"
)

func TestAppendFile(t *testing.T) {
	path := stateFile(t)
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

func TestAppendFileLongName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, strings.Repeat("y", 240)), nil, 0o600); err != nil {
		t.Fatalf("this test requires a filesystem supporting 240-byte basenames: %v", err)
	}
	path := filepath.Join(dir, strings.Repeat("x", 240))
	if err := AppendFile(path, []byte(oldState), 0o600); err != nil {
		t.Fatal(err)
	}
	checkContents(t, path, oldState)
}

func TestAppendFileOpenRetry(t *testing.T) {
	const wantFlags = unix.O_WRONLY | unix.O_CREAT | unix.O_APPEND | unix.O_NONBLOCK | unix.O_NOCTTY
	tests := map[string]struct {
		failures      []error
		opens, writes int
		wantErr       error
	}{
		"success: transient missing entry":  {[]error{unix.ENOENT}, 2, 1, nil},
		"error: persistently missing entry": {[]error{unix.ENOENT, unix.ENOENT}, 2, 0, unix.ENOENT},
		"error: permission denied":          {[]error{unix.EACCES}, 1, 0, unix.EACCES},
		"error: different error on retry":   {[]error{unix.ENOENT, unix.EACCES}, 2, 0, unix.EACCES},
		"error: symlink on retry":           {[]error{unix.ENOENT, unix.ELOOP}, 2, 0, &NonRegularError{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			d := openTestDir(t, dir)
			opens, writes := 0, 0
			open := func(name string, flags int, mode fs.FileMode) (*os.File, error) {
				opens++
				if opens > tt.opens || name != "journal" || flags != wantFlags || mode != 0o600 {
					t.Fatalf("unexpected open %d: %q, flags=%#x, mode=%o", opens, name, flags, mode)
				}
				if opens <= len(tt.failures) {
					return nil, &os.PathError{Op: "open", Path: name, Err: tt.failures[opens-1]}
				}
				return d.root.OpenFile(name, flags, mode)
			}
			err := d.root.openAndAppend("journal", []byte(newState), 0o600, open, func(fd int, data []byte) (int, error) {
				writes++
				return unix.Write(fd, data)
			})
			path := filepath.Join(dir, "journal")
			matches := errors.Is(err, tt.wantErr)
			if _, wantNonRegular := errors.AsType[*NonRegularError](tt.wantErr); wantNonRegular {
				refusal, ok := errors.AsType[*NonRegularError](err)
				matches = ok && refusal.Path == path
			}
			if opens != tt.opens || writes != tt.writes || !matches {
				t.Fatalf("got (%d opens, %d writes, %v), want (%d, %d, %v)", opens, writes, err, tt.opens, tt.writes, tt.wantErr)
			}
			if tt.wantErr == nil {
				checkContents(t, path, newState)
			} else if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("expected no journal after open failure: %v", err)
			}
		})
	}
}

func TestAppendFileWriteCalls(t *testing.T) {
	const record = "data"
	tests := map[string]struct {
		first, calls      int
		firstErr, wantErr error
		want              string
	}{
		"success: full write":                 {4, 1, nil, nil, record},
		"error: short write":                  {2, 1, nil, io.ErrShortWrite, "da"},
		"success: interrupted before writing": {-1, 2, unix.EINTR, nil, record},
		"error: rejected before writing":      {-1, 1, unix.EFBIG, unix.EFBIG, ""},
		"error: interrupted after writing":    {2, 1, unix.EINTR, unix.EINTR, "da"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			d := openTestDir(t, dir)
			calls := 0
			err := d.root.openAndAppend("journal", []byte(record), 0o600, d.root.OpenFile, func(fd int, data []byte) (int, error) {
				calls++
				if calls > tt.calls || string(data) != record {
					t.Fatalf("unexpected write call %d: %q", calls, data)
				}
				if calls == 1 {
					if tt.first < 0 {
						return tt.first, tt.firstErr
					}
					n, err := unix.Write(fd, data[:tt.first])
					return n, errors.Join(err, tt.firstErr)
				}
				return unix.Write(fd, data)
			})
			if calls != tt.calls || !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %d calls and %v, want %d and %v", calls, err, tt.calls, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("%d of %d bytes", len(tt.want), len(record))) {
				t.Fatalf("missing byte count: %v", err)
			}
			checkContents(t, filepath.Join(dir, "journal"), tt.want)
		})
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
		"error: empty":                             {"", ""},
		"error: trailing separator":                {dir + "/file/", "file/"},
		"error: dot":                               {dir + "/missing/.", "."},
		"error: dot-dot":                           {dir + "/missing/..", ".."},
		"error: reserved sibling":                  {dir + "/missing/log.42.0123456789ab.tmp", "log.42.0123456789ab.tmp"},
		"error: mixed-case nonce":                  {dir + "/missing/log.42.0123456789aB.tmp", "log.42.0123456789aB.tmp"},
		"error: mixed-case extension":              {dir + "/missing/log.42.0123456789ab.TmP", "log.42.0123456789ab.TmP"},
		"error: embedded NUL before parent lookup": {dir + "/missing/log\x00file", "log\x00file"},
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
