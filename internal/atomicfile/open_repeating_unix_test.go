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
	"testing"

	"golang.org/x/sys/unix"
)

// maxRepeatingOpens is a generous cap on the opens of one call: it proves the repetition is bounded without pinning
// its bound.
const maxRepeatingOpens = 64

// TestOpenRepeatingCreateGates pins the three gates of the repeated creating open, through the open the held directory
// is given: only an open that asks to create, does not ask for exclusivity and is told the entry does not exist is
// repeated, and only while the held directory is still linked. Every other open is made exactly once and its error is
// returned as it is, the very value the open returned. Every open of a call gets the same name, flags and
// permission, and the flags include every flag the caller asked for.
//
// The removed-directory row decides from what the system reports about the held descriptor after the removal: when
// its link count is zero, the directory is known to be gone and the open is made exactly once; when the system still
// reports links, the repeats are allowed and only their bound is required. Which answer a system gives is not
// assumed.
func TestOpenRepeatingCreateGates(t *testing.T) {
	const name = "entry"
	tests := map[string]struct {
		flag int
		// answers are the errors of the first opens, in order; the opens after them reach the directory.
		answers []error
		// removeDir removes the held directory after it was opened, so that every open reaches a directory that is
		// no longer linked and the system answers for itself. The number of opens is then decided by the link count
		// the system reports for the held descriptor.
		removeDir bool
		// opens is the exact number of opens, zero when the link count decides it.
		opens   int
		wantErr error
	}{
		"success: a nonexclusive create told once that the entry is missing finds it on the repeat": {
			flag: os.O_RDWR | os.O_CREATE, answers: []error{unix.ENOENT}, opens: 2,
		},
		"error: an open without the create flag that finds no entry is made once": {
			flag: os.O_RDWR, answers: []error{unix.ENOENT}, opens: 1, wantErr: unix.ENOENT,
		},
		"error: an exclusive create that finds no entry is made once": {
			flag: os.O_RDWR | os.O_CREATE | os.O_EXCL, answers: []error{unix.ENOENT}, opens: 1, wantErr: unix.ENOENT,
		},
		"error: a nonexclusive create answered with another error is made once": {
			flag: os.O_RDWR | os.O_CREATE, answers: []error{unix.EACCES}, opens: 1, wantErr: unix.EACCES,
		},
		"error: a nonexclusive create in a removed held directory returns the missing-entry error, at once when no link is reported": {
			flag: os.O_RDWR | os.O_CREATE, removeDir: true, wantErr: unix.ENOENT,
		},
	}
	for testName, tt := range tests {
		t.Run(testName, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "held")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("test infrastructure: make the held directory: %v", err)
			}
			d := openTestDir(t, dir)
			wantOpens := tt.opens
			if tt.removeDir {
				if err := os.Remove(dir); err != nil {
					t.Fatalf("test infrastructure: remove the held directory after it was opened: %v", err)
				}
				var st unix.Stat_t
				if err := unix.Fstat(int(d.root.handle.Fd()), &st); err != nil {
					t.Fatalf("test infrastructure: read the link count of the held directory after its removal: %v", err)
				}
				t.Logf("the held directory reports %d links after its removal on this system", st.Nlink)
				if st.Nlink == 0 {
					wantOpens = 1
				}
			}
			opens, firstFlag := 0, 0
			// last is the error value the latest open returned, injected or the system's own.
			var last error
			open := func(got string, flag int, perm fs.FileMode) (*os.File, error) {
				opens++
				if opens == 1 {
					firstFlag = flag
				}
				if opens > maxRepeatingOpens || wantOpens > 0 && opens > wantOpens {
					t.Fatalf("open %d: more opens than the gate allows", opens)
				}
				if got != name || perm != 0o600 || flag != firstFlag || flag&tt.flag != tt.flag {
					t.Fatalf("open %d: %q, flags=%#x, perm=%o; want %q with the same flags on every open, including %#x, and perm 0600", opens, got, flag, perm, name, tt.flag)
				}
				if opens <= len(tt.answers) {
					last = &os.PathError{Op: "open", Path: got, Err: tt.answers[opens-1]}
					return nil, last
				}
				f, err := d.root.OpenFile(got, flag, perm)
				last = err
				return f, err
			}
			f, err := d.root.openRepeatingCreate(name, tt.flag, 0o600, open)
			if f != nil {
				defer func() { _ = f.Close() }()
			}
			if tt.removeDir {
				t.Logf("a creating open in a removed held directory was made %d times on this system", opens)
			}
			switch {
			case wantOpens > 0 && opens != wantOpens:
				t.Errorf("got %d opens, want %d", opens, wantOpens)
			case wantOpens == 0 && (opens < 1 || opens > maxRepeatingOpens):
				t.Errorf("got %d opens, want at least 1 and at most %d", opens, maxRepeatingOpens)
			}
			if tt.wantErr == nil {
				if err != nil || f == nil {
					t.Fatalf("openRepeatingCreate = %v, %v; want the entry", f, err)
				}
				var st unix.Stat_t
				if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
					t.Fatalf("the opened entry: mode %#o, %v; want a regular file", st.Mode, err)
				}
				return
			}
			if f != nil {
				t.Errorf("openRepeatingCreate returned a file with the error %v", err)
			}
			if tt.removeDir && !errors.Is(last, unix.ENOENT) {
				t.Fatalf("cannot stage: the system answered a creating open in a removed directory with %v, not with a missing entry", last)
			}
			if !errors.Is(err, tt.wantErr) || err != last { //nolint:errorlint // The error must be the last open's own value, not a wrapper of it.
				t.Fatalf("got error %#v, want the last open's own error value %#v, which matches %v", err, last, tt.wantErr)
			}
		})
	}
}
