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

package modeldata_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// mdatTFIFOOpen returns an open call that checks the flags Read asks for and then opens, through the
// previous open call, the writerless FIFO dir/pipe instead of the path Read named. The row fails when
// the flags lack O_NONBLOCK or ask for other than read-only access; O_NONBLOCK is set when forwarding,
// so even such a Read returns at once. The forwarded handle is checked by mdatTCloseCheck.
func mdatTFIFOOpen(t *testing.T, dir string, previous mdatTOpen) mdatTOpen {
	return func(name string, flag int, perm os.FileMode) (*os.File, error) {
		if flag&unix.O_NONBLOCK == 0 {
			t.Errorf("Read opened %q without O_NONBLOCK (flags %#x)", name, flag)
		}
		if flag&unix.O_ACCMODE != unix.O_RDONLY {
			t.Errorf("Read opened %q for other than read-only access (flags %#x)", name, flag)
		}
		f, err := previous(filepath.Join(dir, "pipe"), flag|unix.O_NONBLOCK, perm)
		if err != nil {
			return nil, err
		}
		mdatTCloseCheck(t, f)
		return f, nil
	}
}

// mdatTSymlink returns a setup that links the catalog path to target after prepare has created
// whatever target should be.
func mdatTSymlink(target string, prepare func(t *testing.T, dir string)) func(*testing.T, string) string {
	return func(t *testing.T, dir string) string {
		if prepare != nil {
			prepare(t, dir)
		}
		path := filepath.Join(dir, "anthropic-models.json")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		return path
	}
}

// mdatTPlatformRows holds the rows that need symbolic links, FIFOs or device files.
func mdatTPlatformRows() map[string]struct {
	setup func(t *testing.T, dir string) (path string)
	open  func(t *testing.T, dir string, previous mdatTOpen) mdatTOpen
	want  mdatTResult
} {
	return map[string]struct {
		setup func(t *testing.T, dir string) (path string)
		open  func(t *testing.T, dir string, previous mdatTOpen) mdatTOpen
		want  mdatTResult
	}{
		// Wrong: refuses symbolic links (Lstat or O_NOFOLLOW) instead of reading the linked catalog.
		"success: symlink to a valid catalog is followed": {
			setup: mdatTSymlink("target.json", func(t *testing.T, dir string) { mdatTWrite(t, dir, "target.json", mdatTValid) }),
			want:  mdatTResult{Object: mdatTValid, UpdatedAt: 7},
		},
		// Wrong: inspects the link itself and reports a dangling link as an error, not as a missing file.
		"success: dangling symlink is a missing catalog": {
			setup: mdatTSymlink("absent.json", nil),
			want:  mdatTResult{Object: mdatTEmpty},
		},
		// Wrong: checks the type of the link rather than of the file the open reached.
		"error: symlink to a directory is not a regular file": {
			setup: mdatTSymlink("target", func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "target"), 0o700); err != nil {
					t.Fatal(err)
				}
			}),
			want: mdatTFailure,
		},
		// Wrong: refuses only directories and FIFOs, and reads a character device as an empty catalog.
		"error: symlink to a character device is not a regular file": {
			setup: mdatTSymlink("/dev/null", nil),
			want:  mdatTFailure,
		},
		// Wrong: treats every open failure as a missing catalog.
		"error: symlink loop is an I/O error": {
			setup: mdatTSymlink("anthropic-models.json", nil),
			want:  mdatTFailure,
		},
		// Wrong: treats ENOTDIR like ENOENT; only a missing file is an empty catalog.
		"error: path through a regular file is an I/O error": {
			setup: func(t *testing.T, dir string) string {
				return filepath.Join(mdatTWrite(t, dir, "state", "not a directory"), "anthropic-models.json")
			},
			want: mdatTFailure,
		},
		// Read is given a directory path, and only the open call reaches the writerless FIFO, so a Read
		// that opens the path itself or rejects it before the open call fails the call count at once.
		// Wrong: asks for a blocking open, which would wait for a writer; asks for write access; reads
		// a FIFO as a malformed catalog; leaves the handle open; or bypasses the open call.
		"error: FIFO without a writer is not a regular file": {
			setup: func(t *testing.T, dir string) string {
				if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "anthropic-models.json")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			},
			open: mdatTFIFOOpen,
			want: mdatTFailure,
		},
		// Wrong: stats the path and then reads the handle without checking it, so a regular file at the
		// path lets it read a FIFO handle, which with no writer reads as an empty, malformed catalog.
		"error: FIFO handle is refused": {
			setup: func(t *testing.T, dir string) string {
				if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
					t.Fatal(err)
				}
				return mdatTWrite(t, dir, "anthropic-models.json", mdatTValid)
			},
			open: mdatTHandle(func(_ *testing.T, dir string) (*os.File, error) {
				return os.OpenFile(filepath.Join(dir, "pipe"), os.O_RDONLY|unix.O_NONBLOCK, 0)
			}),
			want: mdatTFailure,
		},
	}
}
