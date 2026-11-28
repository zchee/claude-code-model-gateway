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

// TestAppendFileOpenRetry pins how the append path treats the answers of its creating open. A creating open without
// the exclusive flag that is told the entry does not exist, while the held directory is still linked, is repeated a
// bounded number of times: another opener may be creating the same entry at that moment, and on macOS the open system
// call itself can then answer "does not exist". Any other answer ends the append at once, and a bound that is used up
// returns the last "does not exist" unchanged: the very error value of the last open. The test asserts outcomes
// only: how many answers in a row the bound survives is the implementation's, so the rows that succeed stop at five
// failures and the row that never succeeds only requires more than one open and at most maxAppendOpens. One row stages
// a real symbolic link at the name after the first injected "does not exist", so that the repeat meets it through the
// directory's own open: the append must refuse it without following it into the file it names.
func TestAppendFileOpenRetry(t *testing.T) {
	const wantFlags = unix.O_WRONLY | unix.O_CREAT | unix.O_APPEND | unix.O_NONBLOCK | unix.O_NOCTTY
	// maxAppendOpens is a generous cap that proves the repetition is bounded without pinning its bound.
	const maxAppendOpens = 64
	tests := map[string]struct {
		// failures are the answers of the first opens, in order; the opens after them reach the directory.
		failures []error
		// missingForever answers every open with "does not exist", so that only the bound ends the append.
		missingForever bool
		// linkOnRepeat stages a symbolic link at the name, to a sentinel file, after the injected failures and before
		// the open that reaches the directory.
		linkOnRepeat bool
		// opens is the exact number of opens, zero when only the range of the bound is required.
		opens, writes int
		wantErr       error
	}{
		"success: one missing-entry answer, then the entry": {
			failures: []error{unix.ENOENT}, opens: 2, writes: 1,
		},
		"success: two missing-entry answers in a row, then the entry": {
			failures: []error{unix.ENOENT, unix.ENOENT}, opens: 3, writes: 1,
		},
		"success: five missing-entry answers in a row, then the entry": {
			failures: []error{unix.ENOENT, unix.ENOENT, unix.ENOENT, unix.ENOENT, unix.ENOENT}, opens: 6, writes: 1,
		},
		"error: an entry that stays missing returns the missing-entry error once the bound is used up": {
			missingForever: true, wantErr: unix.ENOENT,
		},
		"error: permission denied after a missing-entry answer is returned at once": {
			failures: []error{unix.ENOENT, unix.EACCES}, opens: 2, wantErr: unix.EACCES,
		},
		"error: a symbolic link met on a repeat is still refused as not regular": {
			failures: []error{unix.ENOENT, unix.ELOOP}, opens: 2, wantErr: &NonRegularError{},
		},
		"error: a real symbolic link that appears before the repeat is refused and its target is left untouched": {
			failures: []error{unix.ENOENT}, linkOnRepeat: true, opens: 2, wantErr: &NonRegularError{},
		},
		"error: permission denied on the first open is not repeated": {
			failures: []error{unix.EACCES}, opens: 1, wantErr: unix.EACCES,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			d := openTestDir(t, dir)
			path := filepath.Join(dir, "journal")
			// sentinel is a writable regular file outside the directory: an append that followed the link would land
			// in it.
			sentinel := filepath.Join(t.TempDir(), "sentinel")
			if tt.linkOnRepeat {
				writeFixture(t, sentinel, oldState)
			}
			opens, writes := 0, 0
			// lastInjected is the error value the latest injected failure returned, a fresh one for every open.
			var lastInjected error
			open := func(name string, flags int, mode fs.FileMode) (*os.File, error) {
				opens++
				if opens > maxAppendOpens || tt.opens > 0 && opens > tt.opens || name != "journal" || flags != wantFlags || mode != 0o600 {
					t.Fatalf("unexpected open %d: %q, flags=%#x, mode=%o", opens, name, flags, mode)
				}
				if tt.missingForever {
					lastInjected = &os.PathError{Op: "open", Path: name, Err: unix.ENOENT}
					return nil, lastInjected
				}
				if opens <= len(tt.failures) {
					lastInjected = &os.PathError{Op: "open", Path: name, Err: tt.failures[opens-1]}
					return nil, lastInjected
				}
				if tt.linkOnRepeat {
					if err := os.Symlink(sentinel, path); err != nil {
						t.Fatalf("test infrastructure: stage the symbolic link before the repeat: %v", err)
					}
				}
				return d.root.OpenFile(name, flags, mode)
			}
			err := d.root.openAndAppend("journal", []byte(newState), 0o600, open, func(fd int, data []byte) (int, error) {
				writes++
				return unix.Write(fd, data)
			})
			matches := errors.Is(err, tt.wantErr)
			if _, wantNonRegular := errors.AsType[*NonRegularError](tt.wantErr); wantNonRegular {
				refusal, ok := errors.AsType[*NonRegularError](err)
				matches = ok && refusal.Path == path
			}
			if writes != tt.writes || !matches {
				t.Fatalf("got (%d opens, %d writes, %v), want (%d writes, %v)", opens, writes, err, tt.writes, tt.wantErr)
			}
			switch {
			case tt.opens > 0 && opens != tt.opens:
				t.Fatalf("got %d opens, want %d: every answer but a missing entry ends the append at once, and the open that finds the entry is the last", opens, tt.opens)
			case tt.opens == 0 && (opens < 2 || opens > maxAppendOpens):
				t.Fatalf("got %d opens for an entry that stays missing, want at least 2 and at most %d", opens, maxAppendOpens)
			case tt.missingForever && err != lastInjected: //nolint:errorlint // The error must be the last open's own value, not a wrapper of it.
				// Unchanged means the very value the last open returned: not the first one, not a new wrapper of it.
				t.Fatalf("got error %#v once the bound was used up, want the last open's own error value %#v", err, lastInjected)
			}
			switch {
			case tt.wantErr == nil:
				checkContents(t, path, newState)
			case tt.linkOnRepeat:
				if target, err := os.Readlink(path); err != nil || target != sentinel {
					t.Fatalf("the staged link after the refusal: %q, %v; want the same link to %q", target, err, sentinel)
				}
				checkContents(t, sentinel, oldState)
			default:
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("expected no journal after open failure: %v", err)
				}
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
