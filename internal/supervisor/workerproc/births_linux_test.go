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

//go:build linux

package workerproc_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/supervisor/workerproc"
)

// storeTFileID is the device and inode of a file.
type storeTFileID struct {
	Dev uint64
	Ino uint64
}

// storeTFixtureIDs adds to ids the device and inode of base and of every entry below it, without following a symbolic
// link, each with its path.
func storeTFixtureIDs(t *testing.T, base string, ids map[storeTFileID]string) {
	t.Helper()
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		ids[storeTFileID{Dev: uint64(st.Dev), Ino: st.Ino}] = path
		return nil
	})
	if err != nil {
		t.Fatalf("fixture: list the files below %s: %v", base, err)
	}
}

// storeTOpenFile is one open descriptor of this process.
type storeTOpenFile struct {
	id storeTFileID
	// link is what /proc/self/fd says the descriptor refers to.
	link string
}

// storeTOpenFiles returns every open descriptor of this process by number, as /proc/self/fd lists them. The
// descriptor of the listing itself is closed by the time it is examined and is left out.
func storeTOpenFiles(t *testing.T) map[int]storeTOpenFile {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("list the open descriptors: %v", err)
	}
	open := map[int]storeTOpenFile{}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			continue
		}
		link, _ := os.Readlink("/proc/self/fd/" + e.Name())
		open[fd] = storeTOpenFile{id: storeTFileID{Dev: uint64(st.Dev), Ino: st.Ino}, link: link}
	}
	return open
}

// TestBirthStoreDescriptors checks that every store method, on each way it returns, has closed every descriptor it
// opened: no descriptor that was not open before the call refers, by device and inode, to a file or directory that
// exists below the case's directory after the call, and none refers, by the link /proc/self/fd gives, to a file below
// it that was removed meanwhile, such as a retired record or a staged sibling. Identities are taken after the call
// only, so that an inode freed by the call and reused for another file is not mistaken for the removed one.
func TestBirthStoreDescriptors(t *testing.T) {
	tests := map[string]struct {
		setup func(t *testing.T, base string) string
		// hold has another open file description hold the lock during the call; its own descriptor was open before.
		hold bool
		// call runs one store method and returns its boolean result (for Read: whether it listed all nine entries)
		// and its error.
		call func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error)
		want bool
		err  string
	}{
		"success: Record that publishes": {
			setup: storeTEmptyRoot,
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				return s.Record(ctx, storeTBirth(t, "first", root))
			},
			want: true,
		},
		"error: Record that finds an entry at its name": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				return s.Record(ctx, storeTBirth(t, "first", root))
			},
			err: "exist",
		},
		"error: Record refused by the privacy rule": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o750) },
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				return s.Record(ctx, storeTBirth(t, "other", root))
			},
			err: "permission",
		},
		"success: Read of entries of every kind": {
			setup: func(t *testing.T, base string) string {
				root := storeTNeighbours(t, base)
				dir := filepath.Join(root, "worker-births")
				storeTWrite(t, filepath.Join(dir, storeTNames["first"]), storeTEncoding(t, "first", root))
				storeTWrite(t, filepath.Join(dir, "8000-oversize.json"), storeTEncoding(t, "over", root))
				storeTMkdir(t, filepath.Join(dir, "m-directory"), 0o700)
				storeTFIFO(t, filepath.Join(dir, "n-fifo"))
				storeTSymlink(t, storeTNames["first"], filepath.Join(dir, "l-link"))
				return root
			},
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				entries, err := s.Read(ctx)
				return len(entries) == 9, err
			},
			want: true,
		},
		"success: Retire that removes the record": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				return s.Retire(ctx, storeTBirth(t, "first", root))
			},
			want: true,
		},
		"success: Retire that keeps a changed record": {
			setup: func(t *testing.T, base string) string {
				root := storeTState(t, base, 0o700)
				storeTWrite(t, filepath.Join(root, "worker-births", storeTNames["first"]), storeTEncoding(t, "first", root)+"\n")
				return root
			},
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				return s.Retire(ctx, storeTBirth(t, "first", root))
			},
		},
		"success: Signal that sends": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				return s.Signal(ctx, storeTBirth(t, "first", root), func() error { return nil })
			},
			want: true,
		},
		"error: Signal whose send fails": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				return s.Signal(ctx, storeTBirth(t, "first", root), func() error { return storeTSendError{} })
			},
			want: true,
			err:  "send",
		},
		"error: Signal whose send panics": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (attempted bool, err error) {
				// The panic of send must reach this caller; any other panic is passed on, so that the row fails
				// with its text.
				defer func() {
					if r := recover(); r != nil {
						if r != storeTPanic {
							panic(r)
						}
						attempted, err = false, storeTRecovered{}
					}
				}()
				return s.Signal(ctx, storeTBirth(t, "first", root), func() error { panic(storeTPanic) })
			},
			err: "recovered",
		},
		"success: Signal that finds the lock held": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			hold:  true,
			call: func(t *testing.T, ctx context.Context, s workerproc.Births, root string) (bool, error) {
				return s.Signal(ctx, storeTBirth(t, "first", root), func() error { return nil })
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			storeTUmask(t, 0o022)
			base := t.TempDir()
			root := tt.setup(t, base)
			storeTCheckFixtures(t, root)
			release := func() {}
			if tt.hold {
				release = storeTHold(t, filepath.Join(root, "worker-births"), unix.LOCK_EX)
			}
			// The kernel reports the link of a removed file under the resolved path of its directory.
			resolved, err := filepath.EvalSymlinks(base)
			if err != nil {
				t.Fatal(err)
			}
			before := storeTOpenFiles(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			unblock := func() {
				cancel()
				release()
				storeTReleaseFIFOs(t, filepath.Join(root, "worker-births"))
			}
			s := workerproc.Births{Root: root}
			var ok bool
			storeTBounded(t, "the store call", unblock, func() { ok, err = tt.call(t, ctx, s, root) })
			after := storeTOpenFiles(t)
			ids := map[storeTFileID]string{}
			storeTFixtureIDs(t, base, ids)
			// The call's own result comes first, then its descriptors.
			if problem := storeTErr(tt.err, err); problem != "" {
				t.Errorf("the store call: %s", problem)
			}
			if ok != tt.want {
				t.Errorf("the store call gave %t, want %t (error %v)", ok, tt.want, err)
			}
			for fd, f := range after {
				if was, open := before[fd]; open && was.id == f.id {
					continue
				}
				path, fixture := ids[f.id]
				removed := strings.HasPrefix(f.link, resolved+"/") && strings.HasSuffix(f.link, " (deleted)")
				switch {
				case fixture:
					t.Errorf("descriptor %d is still open on %s after the call returned", fd, path)
				case removed:
					t.Errorf("descriptor %d is still open on the removed file %s after the call returned", fd, f.link)
				}
			}
			release()
		})
	}
}
