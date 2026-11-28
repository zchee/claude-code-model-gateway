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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"golang.org/x/sys/unix"
)

// heldDirTBound is how long one call may take before the test unblocks it and reports it as blocked.
const heldDirTBound = 30 * time.Second

// heldDirTGuard runs call and fails the calling test with the panic's text when call panics, so that one panicking
// method fails only the row that called it.
func heldDirTGuard(t *testing.T, what string, call func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	call()
}

// heldDirTBounded runs call in its own goroutine and waits for it. A panic in call is carried back and fails the
// calling test with its text. When call has not returned within heldDirTBound, the test opens every FIFO in dir for
// writing, which releases an open that waits for a writer and is the only obstruction these cases build, reports the
// call as blocked and joins it. A call that stays blocked after that is bounded only by the timeout of the test
// binary, which ends the whole test process: an in-process goroutine cannot be both abandoned and joined.
func heldDirTBounded(t *testing.T, what, dir string, call func()) {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		call()
	}()
	var panicked any
	select {
	case panicked = <-done:
	case <-time.After(heldDirTBound):
		// A result that was ready when the bound passed is the call's result, not a block.
		select {
		case panicked = <-done:
		default:
			t.Errorf("%s did not return within %v: it blocked", what, heldDirTBound)
			heldDirTReleaseFIFOs(t, dir)
			panicked = <-done
		}
	}
	if panicked != nil {
		t.Fatalf("%s panicked: %v", what, panicked)
	}
}

// heldDirTReleaseFIFOs opens and closes every FIFO directly in dir for writing without blocking. An open for reading
// that waits for a writer returns once a writer has opened the FIFO.
func heldDirTReleaseFIFOs(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Errorf("list %s to release FIFOs: %v", dir, err)
		return
	}
	for _, e := range entries {
		if e.Type()&fs.ModeNamedPipe == 0 {
			continue
		}
		fd, err := unix.Open(filepath.Join(dir, e.Name()), unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Errorf("release FIFO %s: %v", e.Name(), err)
			continue
		}
		_ = unix.Close(fd)
	}
}

// heldDirTTree describes every entry below root, without following a symbolic link, by its relative path: "dir"
// and the permission bits, "file", the permission bits and the content, "link" and the target, "fifo", "socket" or
// "other". The root itself is not described.
func heldDirTTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			tree[rel] = fmt.Sprintf("dir %04o", mode.Perm())
		case mode.IsRegular():
			if mode.Perm()&0o400 == 0 {
				tree[rel] = fmt.Sprintf("file %04o size %d", mode.Perm(), info.Size())
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			tree[rel] = fmt.Sprintf("file %04o %s", mode.Perm(), data)
		case mode&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			tree[rel] = "link " + target
		case mode&fs.ModeNamedPipe != 0:
			tree[rel] = "fifo"
		case mode&fs.ModeSocket != 0:
			tree[rel] = "socket"
		default:
			tree[rel] = "other"
		}
		return nil
	})
	if err != nil {
		t.Fatalf("describe the tree below %s: %v", root, err)
	}
	return tree
}

// heldDirTMkdir makes the directory path and gives it exactly the permission bits perm.
func heldDirTMkdir(t *testing.T, path string, perm fs.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// heldDirTSymlink makes a symbolic link at path whose target is target.
func heldDirTSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// heldDirTFIFO makes a FIFO at path.
func heldDirTFIFO(t *testing.T, path string) {
	t.Helper()
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// heldDirTSocket binds a Unix socket at name in dir and closes it, which leaves the socket entry in place. The bind
// uses a name relative to dir, which becomes the working directory of the test, because an absolute path below a
// test's temporary directory can be longer than a socket address holds.
func heldDirTSocket(t *testing.T, dir, name string) {
	t.Helper()
	t.Chdir(dir)
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: name}); err != nil {
		t.Fatalf("bind a Unix socket at %s in %s: %v", name, dir, err)
	}
}

// heldDirTUmask sets the creation mask to mask for the rest of the test. No test of this package runs in parallel, so
// the process-wide mask is not changed under another test.
func heldDirTUmask(t *testing.T, mask int) {
	t.Helper()
	previous := unix.Umask(mask)
	t.Cleanup(func() { unix.Umask(previous) })
}

// heldDirTInode returns the inode of the entry at path, without following a symbolic link there.
func heldDirTInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// heldDirTHolds checks, through Stat of c, that c holds the directory at path and that its permission bits are perm.
func heldDirTHolds(t *testing.T, c *Dir, path string, perm fs.FileMode) {
	t.Helper()
	var info fs.FileInfo
	var err error
	heldDirTGuard(t, "Stat of the returned Dir", func() { info, err = c.Stat() })
	if err != nil || info == nil {
		t.Fatalf("Stat of the returned Dir = %v, %v; want file information and no error", info, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Stat of the returned Dir: Sys() is %T, want *syscall.Stat_t", info.Sys())
	}
	if got, want := st.Ino, heldDirTInode(t, path); got != want {
		t.Errorf("the returned Dir holds inode %d, want %d, the directory at %s", got, want, path)
	}
	if diff := gocmp.Diff(perm, info.Mode().Perm()); diff != "" {
		t.Errorf("permission bits of the returned Dir (-want +got):\n%s", diff)
	}
}

// heldDirTClose closes c when it is not nil and reports a failure to close it.
func heldDirTClose(t *testing.T, c *Dir) {
	t.Helper()
	if c == nil {
		return
	}
	if err := c.Close(); err != nil {
		t.Errorf("close the returned Dir: %v", err)
	}
}

// heldDirTDenied fails the test unless the permission bits of the entry at path really deny this process reading it.
// A process that file modes do not bind would make a case that relies on them pass or fail for another reason.
func heldDirTDenied(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Fatalf("the venue must not run as root: this case needs a file mode that denies reading %s", path)
	}
	f, err := os.Open(path)
	if err == nil {
		_ = f.Close()
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("the file mode did not deny reading %s (error %v): this process is not bound by file modes", path, err)
	}
}

// heldDirTErr describes how err fails the expectation named by want, or returns "" when it meets it. The
// expectations are: "" (no error), "destination" (a *DestinationError), "nonregular" (a *NonRegularError), "closed"
// (fs.ErrClosed), "notexist" (fs.ErrNotExist),
// "notexist-exist-permission" (an error that matches none of fs.ErrNotExist, fs.ErrExist and fs.ErrPermission),
// "notexist-permission" (an error that matches neither fs.ErrNotExist nor fs.ErrPermission), "permission"
// (fs.ErrPermission and neither refusal type) and "toolong" (unix.ENAMETOOLONG and neither refusal type).
func heldDirTErr(want string, err error) string {
	_, destination := errors.AsType[*DestinationError](err)
	_, nonregular := errors.AsType[*NonRegularError](err)
	refusal := destination || nonregular
	ok := false
	switch want {
	case "":
		ok = err == nil
	case "destination":
		ok = destination
	case "nonregular":
		ok = nonregular
	case "closed":
		ok = errors.Is(err, fs.ErrClosed)
	case "notexist":
		ok = errors.Is(err, fs.ErrNotExist)
	case "notexist-exist-permission":
		ok = err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrPermission)
	case "notexist-permission":
		ok = err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission)
	case "permission":
		ok = errors.Is(err, fs.ErrPermission) && !refusal
	case "toolong":
		ok = errors.Is(err, unix.ENAMETOOLONG) && !refusal
	default:
		return fmt.Sprintf("unknown expectation %q", want)
	}
	if ok {
		return ""
	}
	return fmt.Sprintf("error %v (%T); want %s", err, err, heldDirTDescribeWant(want))
}

// heldDirTDescribeWant spells out an expectation of heldDirTErr for a failure message.
func heldDirTDescribeWant(want string) string {
	switch want {
	case "":
		return "no error"
	case "notexist-exist-permission":
		return "an error matching none of fs.ErrNotExist, fs.ErrExist and fs.ErrPermission"
	case "notexist-permission":
		return "an error matching neither fs.ErrNotExist nor fs.ErrPermission"
	default:
		return "an error of the kind " + want
	}
}

// heldDirTClosedDirs returns a Dir that was opened and closed, and the zero Dir.
func heldDirTClosedDirs(t *testing.T, dir string) map[string]*Dir {
	t.Helper()
	closed, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	return map[string]*Dir{"closed": closed, "zero": new(Dir)}
}

func TestDirNames(t *testing.T) {
	tests := map[string]struct {
		// setup builds the entries below root and returns the directory to open.
		setup func(t *testing.T, root string) string
		// change runs after the directory was opened and before the first listing.
		change func(t *testing.T, root string)
		want   []string
		// between runs after the first listing; the second listing must then give wantSecond.
		between    func(t *testing.T, root string)
		wantSecond []string
	}{
		"success: every entry once, a symbolic link by its own name": {
			setup: func(t *testing.T, root string) string {
				held := filepath.Join(root, "held")
				heldDirTMkdir(t, held, 0o700)
				writeFixture(t, filepath.Join(held, "a.json"), oldState)
				heldDirTMkdir(t, filepath.Join(held, "b"), 0o700)
				writeFixture(t, filepath.Join(held, "b", "inner"), oldState)
				heldDirTSymlink(t, "b", filepath.Join(held, "c-link"))
				heldDirTSymlink(t, "absent", filepath.Join(held, "d-dangling"))
				heldDirTFIFO(t, filepath.Join(held, "e-fifo"))
				return held
			},
			want: []string{"a.json", "b", "c-link", "d-dangling", "e-fifo"},
		},
		"success: an empty directory": {
			setup: func(t *testing.T, root string) string {
				held := filepath.Join(root, "held")
				heldDirTMkdir(t, held, 0o700)
				return held
			},
			want: []string{},
		},
		"success: a renamed directory with another directory at its former path": {
			setup: func(t *testing.T, root string) string {
				held := filepath.Join(root, "held")
				heldDirTMkdir(t, held, 0o700)
				writeFixture(t, filepath.Join(held, "x"), oldState)
				writeFixture(t, filepath.Join(held, "y"), oldState)
				return held
			},
			change: func(t *testing.T, root string) {
				if err := os.Rename(filepath.Join(root, "held"), filepath.Join(root, "moved")); err != nil {
					t.Fatal(err)
				}
				heldDirTMkdir(t, filepath.Join(root, "held"), 0o700)
				writeFixture(t, filepath.Join(root, "held", "decoy"), oldState)
			},
			want: []string{"x", "y"},
		},
		"success: an entry added after the first listing is in the second": {
			setup: func(t *testing.T, root string) string {
				held := filepath.Join(root, "held")
				heldDirTMkdir(t, held, 0o700)
				writeFixture(t, filepath.Join(held, "first"), oldState)
				return held
			},
			want: []string{"first"},
			between: func(t *testing.T, root string) {
				writeFixture(t, filepath.Join(root, "held", "second"), oldState)
			},
			wantSecond: []string{"first", "second"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			d := openTestDir(t, tt.setup(t, root))
			if tt.change != nil {
				tt.change(t, root)
			}
			for _, call := range []string{"first", "second"} {
				want := tt.want
				if call == "second" && tt.between != nil {
					tt.between(t, root)
					want = tt.wantSecond
				}
				var got []string
				var err error
				heldDirTGuard(t, "Names", func() { got, err = d.Names() })
				if err != nil {
					t.Fatalf("%s Names: %v", call, err)
				}
				slices.Sort(got)
				if diff := gocmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("%s Names (-want +got):\n%s", call, diff)
				}
			}
		})
	}
	for name, d := range heldDirTClosedDirs(t, t.TempDir()) {
		t.Run("error: a "+name+" Dir", func(t *testing.T) {
			var got []string
			var err error
			heldDirTGuard(t, "Names", func() { got, err = d.Names() })
			if got != nil {
				t.Errorf("Names = %q, want nil", got)
			}
			if problem := heldDirTErr("closed", err); problem != "" {
				t.Errorf("Names: %s", problem)
			}
		})
	}
}

func TestDirStat(t *testing.T) {
	tests := map[string]struct {
		// replace renames the held directory away and puts another directory at its former path.
		replace bool
		// chmod, when not zero, is given to the held directory after a first Stat; a second Stat must show it.
		chmod fs.FileMode
		// perm is the permission bits the last Stat must show.
		perm fs.FileMode
	}{
		"success: the held directory":                                            {perm: 0o751},
		"success: a renamed directory with another directory at its former path": {replace: true, perm: 0o751},
		"success: a mode changed after the open is seen by the next Stat":        {chmod: 0o710, perm: 0o710},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			held := filepath.Join(root, "held")
			heldDirTMkdir(t, held, 0o751)
			inode := heldDirTInode(t, held)
			d := openTestDir(t, held)
			if tt.replace {
				if err := os.Rename(held, filepath.Join(root, "moved")); err != nil {
					t.Fatal(err)
				}
				heldDirTMkdir(t, held, 0o700)
			}
			var info fs.FileInfo
			var err error
			heldDirTGuard(t, "Stat", func() { info, err = d.Stat() })
			if err != nil || info == nil {
				t.Fatalf("Stat = %v, %v; want file information and no error", info, err)
			}
			if tt.chmod != 0 {
				if diff := gocmp.Diff(fs.FileMode(0o751), info.Mode().Perm()); diff != "" {
					t.Errorf("first Stat().Mode().Perm() (-want +got):\n%s", diff)
				}
				if err := os.Chmod(held, tt.chmod); err != nil {
					t.Fatal(err)
				}
				heldDirTGuard(t, "Stat", func() { info, err = d.Stat() })
				if err != nil || info == nil {
					t.Fatalf("Stat after the mode change = %v, %v; want file information and no error", info, err)
				}
			}
			if !info.IsDir() {
				t.Errorf("Stat().IsDir() = false, want true (mode %v)", info.Mode())
			}
			if diff := gocmp.Diff(tt.perm, info.Mode().Perm()); diff != "" {
				t.Errorf("Stat().Mode().Perm() (-want +got):\n%s", diff)
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				t.Fatalf("Stat().Sys() is %T, want *syscall.Stat_t", info.Sys())
			}
			type stat struct {
				Inode uint64
				UID   uint32
				Type  uint32
				Perm  uint32
			}
			got := stat{Inode: st.Ino, UID: st.Uid, Type: uint32(st.Mode) & syscall.S_IFMT, Perm: uint32(st.Mode) & 0o7777}
			want := stat{Inode: inode, UID: uint32(os.Geteuid()), Type: syscall.S_IFDIR, Perm: uint32(tt.perm)}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("Stat().Sys() (-want +got):\n%s", diff)
			}
		})
	}
	for name, d := range heldDirTClosedDirs(t, t.TempDir()) {
		t.Run("error: a "+name+" Dir", func(t *testing.T) {
			var info fs.FileInfo
			var err error
			heldDirTGuard(t, "Stat", func() { info, err = d.Stat() })
			if info != nil {
				t.Errorf("Stat = %v, want nil", info)
			}
			if problem := heldDirTErr("closed", err); problem != "" {
				t.Errorf("Stat: %s", problem)
			}
		})
	}
}

// heldDirTChildFixture builds the entries the OpenChildDir cases work on in root.
func heldDirTChildFixture(t *testing.T, root string) {
	t.Helper()
	heldDirTMkdir(t, filepath.Join(root, "child"), 0o750)
	heldDirTMkdir(t, filepath.Join(root, "open"), 0o777)
	heldDirTMkdir(t, filepath.Join(root, "target"), 0o750)
	heldDirTSymlink(t, "target", filepath.Join(root, "link"))
	heldDirTSymlink(t, "absent", filepath.Join(root, "dangling"))
	writeFixture(t, filepath.Join(root, "file"), oldState)
	heldDirTSymlink(t, "file", filepath.Join(root, "filelink"))
	heldDirTFIFO(t, filepath.Join(root, "fifo"))
}

// heldDirTChildTree is the description heldDirTTree gives of the entries heldDirTChildFixture builds.
func heldDirTChildTree() map[string]string {
	return map[string]string{
		"child":    "dir 0750",
		"open":     "dir 0777",
		"target":   "dir 0750",
		"link":     "link target",
		"dangling": "link absent",
		"file":     "file 0600 " + oldState,
		"filelink": "link file",
		"fifo":     "fifo",
	}
}

func TestDirOpenChildDir(t *testing.T) {
	tests := map[string]struct {
		name   string
		create bool
		perm   fs.FileMode
		// held is the entry the returned Dir must hold, relative to the opened directory; "" when no Dir is returned.
		held string
		// mode is the permission bits Stat of the returned Dir must show.
		mode fs.FileMode
		// err names the expected error, as heldDirTErr reads it.
		err string
		// added is what the call adds to the tree, as heldDirTTree describes it.
		added map[string]string
	}{
		"error: an empty name":                          {name: "", create: true, perm: 0o700, err: "destination"},
		"error: a dot":                                  {name: ".", create: true, perm: 0o700, err: "destination"},
		"error: a dot-dot":                              {name: "..", create: true, perm: 0o700, err: "destination"},
		"error: a dot-dot without create":               {name: "..", err: "destination"},
		"error: a name holding a slash":                 {name: "child/sub", create: true, perm: 0o700, err: "destination"},
		"error: a name holding a slash to a new entry":  {name: "new/sub", create: true, perm: 0o700, err: "destination"},
		"error: a name holding a NUL byte":              {name: "new\x00", create: true, perm: 0o700, err: "destination"},
		"success: an existing directory":                {name: "child", held: "child", mode: 0o750},
		"success: an existing directory keeps its mode": {name: "child", create: true, perm: 0o700, held: "child", mode: 0o750},
		"success: a directory open to everyone is returned as it is": {
			name: "open", held: "open", mode: 0o777,
		},
		"success: a directory open to everyone keeps its mode with create": {
			name: "open", create: true, perm: 0o700, held: "open", mode: 0o777,
		},
		"success: a symbolic link to a directory is followed": {name: "link", held: "target", mode: 0o750},
		"success: a symbolic link to a directory is followed with create": {
			name: "link", create: true, perm: 0o700, held: "target", mode: 0o750,
		},
		"error: a missing name without create": {name: "new", err: "notexist"},
		"success: a missing name is made with the permission bits of perm": {
			name: "new", create: true, perm: 0o751, held: "new", mode: 0o751, added: map[string]string{"new": "dir 0751"},
		},
		"success: the creation mask applies to perm": {
			name: "new", create: true, perm: 0o777, held: "new", mode: 0o755, added: map[string]string{"new": "dir 0755"},
		},
		"success: a private directory is made": {
			name: "new", create: true, perm: 0o700, held: "new", mode: 0o700, added: map[string]string{"new": "dir 0700"},
		},
		"error: a dangling symbolic link without create": {name: "dangling", err: "notexist"},
		"error: a dangling symbolic link with create": {
			name: "dangling", create: true, perm: 0o700, err: "notexist-exist-permission",
		},
		"error: a regular file":                                {name: "file", err: "notexist-permission"},
		"error: a regular file with create":                    {name: "file", create: true, perm: 0o700, err: "notexist-permission"},
		"error: a symbolic link to a regular file":             {name: "filelink", err: "notexist-permission"},
		"error: a symbolic link to a regular file with create": {name: "filelink", create: true, perm: 0o700, err: "notexist-permission"},
		"error: a FIFO":                                        {name: "fifo", err: "notexist-permission"},
		"error: a FIFO with create":                            {name: "fifo", create: true, perm: 0o700, err: "notexist-permission"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			heldDirTUmask(t, 0o022)
			root := t.TempDir()
			heldDirTChildFixture(t, root)
			d := openTestDir(t, root)
			var c *Dir
			var err error
			heldDirTBounded(t, "OpenChildDir", root, func() { c, err = d.OpenChildDir(tt.name, tt.create, tt.perm) })
			defer heldDirTClose(t, c)
			if problem := heldDirTErr(tt.err, err); problem != "" {
				t.Errorf("OpenChildDir(%q, %t, %v): %s", tt.name, tt.create, tt.perm, problem)
			}
			if (c != nil) != (tt.held != "") {
				t.Fatalf("OpenChildDir(%q, %t, %v) returned Dir %v; want a Dir: %t", tt.name, tt.create, tt.perm, c, tt.held != "")
			}
			want := heldDirTChildTree()
			maps.Copy(want, tt.added)
			if diff := gocmp.Diff(want, heldDirTTree(t, root)); diff != "" {
				t.Errorf("tree after OpenChildDir (-want +got):\n%s", diff)
			}
			if c != nil {
				heldDirTHolds(t, c, filepath.Join(root, tt.held), tt.mode)
			}
		})
	}
	heldDirTChildMoved(t)
	heldDirTChildIndependent(t)
	heldDirTChildClosed(t)
}

// heldDirTChildMoved runs the OpenChildDir cases whose held directory was renamed or removed after it was opened.
func heldDirTChildMoved(t *testing.T) {
	t.Helper()
	tests := map[string]struct {
		// change runs on the tree after the directory root/held was opened.
		change func(t *testing.T, root string)
		name   string
		create bool
		// held is the entry the returned Dir must hold, relative to root; "" when no Dir is expected.
		held string
		// err names the expected error, as heldDirTErr reads it, when held is ""; "" when the result is not specified.
		err string
		// want is the tree below root after the call.
		want map[string]string
		// decoy is an entry relative to root that a Dir the call returns must not hold.
		decoy string
	}{
		"success: a renamed directory gets its new child and its former path gets nothing": {
			change: func(t *testing.T, root string) {
				if err := os.Rename(filepath.Join(root, "held"), filepath.Join(root, "moved")); err != nil {
					t.Fatal(err)
				}
				heldDirTMkdir(t, filepath.Join(root, "held"), 0o700)
			},
			name: "child", create: true, held: "moved/child",
			want: map[string]string{"held": "dir 0700", "moved": "dir 0700", "moved/existing": "dir 0700", "moved/child": "dir 0700"},
		},
		"success: a renamed directory opens its own existing child": {
			change: func(t *testing.T, root string) {
				if err := os.Rename(filepath.Join(root, "held"), filepath.Join(root, "moved")); err != nil {
					t.Fatal(err)
				}
				heldDirTMkdir(t, filepath.Join(root, "held"), 0o700)
				heldDirTMkdir(t, filepath.Join(root, "held", "existing"), 0o700)
			},
			name: "existing", held: "moved/existing",
			want: map[string]string{"held": "dir 0700", "held/existing": "dir 0700", "moved": "dir 0700", "moved/existing": "dir 0700"},
		},
		"success: a renamed directory never makes a missing child at its former path": {
			change: func(t *testing.T, root string) {
				if err := os.Rename(filepath.Join(root, "held"), filepath.Join(root, "moved")); err != nil {
					t.Fatal(err)
				}
				heldDirTMkdir(t, filepath.Join(root, "held"), 0o700)
			},
			name: "child", err: "notexist",
			want: map[string]string{"held": "dir 0700", "moved": "dir 0700", "moved/existing": "dir 0700"},
		},
		"success: a removed directory is not made again": {
			change: func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Join(root, "held")); err != nil {
					t.Fatal(err)
				}
			},
			name: "child", create: true,
			want: map[string]string{},
		},
		"success: a removed directory with another directory at its former path leaves that directory empty": {
			change: func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Join(root, "held")); err != nil {
					t.Fatal(err)
				}
				heldDirTMkdir(t, filepath.Join(root, "held"), 0o700)
			},
			name: "child", create: true,
			want: map[string]string{"held": "dir 0700"},
		},
		"success: a removed directory never opens the child of the directory at its former path": {
			change: func(t *testing.T, root string) {
				if err := os.RemoveAll(filepath.Join(root, "held")); err != nil {
					t.Fatal(err)
				}
				heldDirTMkdir(t, filepath.Join(root, "held"), 0o700)
				heldDirTMkdir(t, filepath.Join(root, "held", "child"), 0o700)
			},
			name: "child", create: true, decoy: "held/child",
			want: map[string]string{"held": "dir 0700", "held/child": "dir 0700"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			heldDirTUmask(t, 0o022)
			root := t.TempDir()
			heldDirTMkdir(t, filepath.Join(root, "held"), 0o700)
			heldDirTMkdir(t, filepath.Join(root, "held", "existing"), 0o700)
			d := openTestDir(t, filepath.Join(root, "held"))
			tt.change(t, root)
			var c *Dir
			var err error
			heldDirTGuard(t, "OpenChildDir", func() { c, err = d.OpenChildDir(tt.name, tt.create, 0o700) })
			defer heldDirTClose(t, c)
			switch {
			case tt.held == "" && tt.err != "":
				if c != nil {
					t.Errorf("OpenChildDir(%q, %t, 0o700) returned a Dir, want none", tt.name, tt.create)
				}
				if problem := heldDirTErr(tt.err, err); problem != "" {
					t.Errorf("OpenChildDir(%q, %t, 0o700): %s", tt.name, tt.create, problem)
				}
			case tt.held == "":
				// Only the absence of any effect at the former path is specified here; the result is recorded.
				t.Logf("OpenChildDir(%q, %t, 0o700) on the held directory: Dir %v, error %v (%T)", tt.name, tt.create, c, err, err)
				if c != nil && tt.decoy != "" {
					var info fs.FileInfo
					var serr error
					heldDirTGuard(t, "Stat of the returned Dir", func() { info, serr = c.Stat() })
					if serr != nil || info == nil {
						t.Fatalf("Stat of the returned Dir = %v, %v; want file information and no error", info, serr)
					}
					st, ok := info.Sys().(*syscall.Stat_t)
					if !ok {
						t.Fatalf("Stat of the returned Dir: Sys() is %T, want *syscall.Stat_t", info.Sys())
					}
					if st.Ino == heldDirTInode(t, filepath.Join(root, tt.decoy)) {
						t.Errorf("OpenChildDir returned the directory at %s, below the former path of the held directory", tt.decoy)
					}
				}
			default:
				if err != nil || c == nil {
					t.Fatalf("OpenChildDir(%q, %t, 0o700) = %v, %v; want a Dir and no error", tt.name, tt.create, c, err)
				}
				heldDirTHolds(t, c, filepath.Join(root, tt.held), 0o700)
			}
			if diff := gocmp.Diff(tt.want, heldDirTTree(t, root)); diff != "" {
				t.Errorf("tree after OpenChildDir (-want +got):\n%s", diff)
			}
		})
	}
}

// heldDirTChildIndependent runs the OpenChildDir cases that use the returned Dir after its parent Dir was closed.
func heldDirTChildIndependent(t *testing.T) {
	t.Helper()
	tests := map[string]struct {
		name   string
		create bool
	}{
		"success: an existing child stays usable after its parent Dir is closed": {name: "child"},
		"success: a made child stays usable after its parent Dir is closed":      {name: "new", create: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			heldDirTUmask(t, 0o022)
			root := t.TempDir()
			heldDirTMkdir(t, filepath.Join(root, "child"), 0o700)
			d, err := OpenDir(root)
			if err != nil {
				t.Fatal(err)
			}
			// Close is idempotent, so the explicit Close below and this cleanup do not conflict.
			t.Cleanup(func() { _ = d.Close() })
			var c *Dir
			heldDirTGuard(t, "OpenChildDir", func() { c, err = d.OpenChildDir(tt.name, tt.create, 0o700) })
			defer heldDirTClose(t, c)
			if err != nil || c == nil {
				t.Fatalf("OpenChildDir(%q, %t, 0o700) = %v, %v; want a Dir and no error", tt.name, tt.create, c, err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if err := c.CreateExclusive("probe", []byte(newState), 0o600); err != nil {
				t.Fatalf("CreateExclusive through the returned Dir after its parent was closed: %v", err)
			}
			checkContents(t, filepath.Join(root, tt.name, "probe"), newState)
			var names []string
			heldDirTGuard(t, "Names", func() { names, err = c.Names() })
			if err != nil {
				t.Fatalf("Names of the returned Dir after its parent was closed: %v", err)
			}
			if diff := gocmp.Diff([]string{"probe"}, names); diff != "" {
				t.Errorf("Names of the returned Dir (-want +got):\n%s", diff)
			}
		})
	}
}

// heldDirTChildClosed runs OpenChildDir on a closed Dir and on the zero Dir.
func heldDirTChildClosed(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for name, d := range heldDirTClosedDirs(t, root) {
		t.Run("error: a "+name+" Dir", func(t *testing.T) {
			var c *Dir
			var err error
			heldDirTGuard(t, "OpenChildDir", func() { c, err = d.OpenChildDir("child", true, 0o700) })
			defer heldDirTClose(t, c)
			if c != nil {
				t.Errorf("OpenChildDir returned a Dir, want nil")
			}
			if problem := heldDirTErr("closed", err); problem != "" {
				t.Errorf("OpenChildDir: %s", problem)
			}
			if diff := gocmp.Diff(map[string]string{}, heldDirTTree(t, root)); diff != "" {
				t.Errorf("tree after OpenChildDir on a %s Dir (-want +got):\n%s", name, diff)
			}
		})
	}
}

// heldDirTRegularFixture builds the entries the OpenRegular cases work on in dir.
func heldDirTRegularFixture(t *testing.T, dir string) {
	t.Helper()
	writeFixture(t, filepath.Join(dir, "file"), oldState)
	heldDirTSymlink(t, "file", filepath.Join(dir, "filelink"))
	heldDirTMkdir(t, filepath.Join(dir, "sub"), 0o700)
	heldDirTSymlink(t, "sub", filepath.Join(dir, "dirlink"))
	heldDirTSymlink(t, "absent", filepath.Join(dir, "dangling"))
	heldDirTFIFO(t, filepath.Join(dir, "fifo"))
	heldDirTSocket(t, dir, "socket")
	writeFixture(t, filepath.Join(dir, "locked"), oldState)
	if err := os.Chmod(filepath.Join(dir, "locked"), 0); err != nil {
		t.Fatal(err)
	}
}

// heldDirTLongName returns a name that the file system of the directory dir refuses as too long, which it shows by
// failing a metadata read of that name relative to a descriptor of dir with ENAMETOOLONG. The test needs that error
// from the system, not a limit of its own, to see that OpenRegular returns it as it is.
func heldDirTLongName(t *testing.T, dir string) string {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	for _, n := range []int{256, 1024, 4096} {
		name := strings.Repeat("n", n)
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENAMETOOLONG) {
			return name
		}
	}
	t.Fatalf("fixture: the file system of %s refuses no name of up to 4096 bytes with ENAMETOOLONG", dir)
	return ""
}

func TestDirOpenRegular(t *testing.T) {
	tests := map[string]struct {
		name string
		// dev opens /dev instead of the fixture directory.
		dev bool
		// long replaces name with one the file system refuses as too long.
		long bool
		// decoy, when set, renames the held directory after it was opened and puts another directory at its former
		// path, whose own entry of the same name is a regular file with other bytes ("file"), a symbolic link to such a
		// file ("link") or a directory ("dir"). The held directory's own entry must still be the one opened.
		decoy string
		// err names the expected error, as heldDirTErr reads it; "" for an opened file.
		err string
	}{
		"success: a regular file is opened at its start":                                   {name: "file"},
		"success: a renamed directory gives its own file, not the one at its former path":  {name: "file", decoy: "file"},
		"success: a renamed directory gives its own file, not the link at its former path": {name: "file", decoy: "link"},
		"success: a renamed directory gives its own file, not the directory at its former path": {
			name: "file", decoy: "dir",
		},
		"error: an empty name":                            {name: "", err: "destination"},
		"error: a dot":                                    {name: ".", err: "destination"},
		"error: a dot-dot":                                {name: "..", err: "destination"},
		"error: a name holding a slash":                   {name: "sub/file", err: "destination"},
		"error: a name holding a NUL byte":                {name: "file\x00", err: "destination"},
		"error: a symbolic link to a regular file":        {name: "filelink", err: "nonregular"},
		"error: a symbolic link to a directory":           {name: "dirlink", err: "nonregular"},
		"error: a dangling symbolic link":                 {name: "dangling", err: "nonregular"},
		"error: a FIFO":                                   {name: "fifo", err: "nonregular"},
		"error: a Unix socket":                            {name: "socket", err: "nonregular"},
		"error: a directory":                              {name: "sub", err: "nonregular"},
		"error: a device":                                 {name: "null", dev: true, err: "nonregular"},
		"error: a missing name":                           {name: "absent", err: "notexist"},
		"error: a name longer than a component can be":    {long: true, err: "toolong"},
		"error: a regular file this process may not read": {name: "locked", err: "permission"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "held")
			heldDirTMkdir(t, dir, 0o700)
			heldDirTRegularFixture(t, dir)
			if tt.name == "locked" {
				heldDirTDenied(t, filepath.Join(dir, "locked"))
			}
			name := tt.name
			if tt.long {
				name = heldDirTLongName(t, dir)
			}
			before := heldDirTTree(t, dir)
			opened := dir
			if tt.dev {
				opened = "/dev"
			}
			d := openTestDir(t, opened)
			if tt.decoy != "" {
				moved := dir + "-moved"
				if err := os.Rename(dir, moved); err != nil {
					t.Fatal(err)
				}
				heldDirTMkdir(t, dir, 0o700)
				switch tt.decoy {
				case "file":
					writeFixture(t, filepath.Join(dir, name), newState)
				case "link":
					writeFixture(t, filepath.Join(dir, "other"), newState)
					heldDirTSymlink(t, "other", filepath.Join(dir, name))
				case "dir":
					heldDirTMkdir(t, filepath.Join(dir, name), 0o700)
				default:
					t.Fatalf("fixture: unknown decoy %q", tt.decoy)
				}
				dir = moved
			}
			var f *os.File
			var err error
			heldDirTBounded(t, "OpenRegular", dir, func() { f, err = d.OpenRegular(name) })
			if f != nil {
				defer func() { _ = f.Close() }()
			}
			if problem := heldDirTErr(tt.err, err); problem != "" {
				t.Errorf("OpenRegular(%.40q): %s", name, problem)
			}
			if (f != nil) != (tt.err == "") {
				t.Fatalf("OpenRegular(%.40q) returned file %v; want a file: %t", name, f, tt.err == "")
			}
			if diff := gocmp.Diff(before, heldDirTTree(t, dir)); diff != "" {
				t.Errorf("tree after OpenRegular (-want +got):\n%s", diff)
			}
			if f == nil {
				return
			}
			heldDirTCheckOpened(t, f)
		})
	}
	heldDirTRegularClosed(t)
}

// heldDirTCheckOpened checks that f is the fixture's regular file, opened for reading only, close-on-exec and
// without blocking, at offset 0, and that reading it gives the whole content.
func heldDirTCheckOpened(t *testing.T, f *os.File) {
	t.Helper()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("the opened file has mode %v, want a regular file", info.Mode())
	}
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	type flags struct {
		Access      int
		NonBlocking bool
		CloseOnExec bool
	}
	var got flags
	var flagErr error
	if err := conn.Control(func(fd uintptr) {
		status, err := unix.FcntlInt(fd, unix.F_GETFL, 0)
		if err != nil {
			flagErr = err
			return
		}
		descriptor, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
		if err != nil {
			flagErr = err
			return
		}
		got = flags{Access: status & unix.O_ACCMODE, NonBlocking: status&unix.O_NONBLOCK != 0, CloseOnExec: descriptor&unix.FD_CLOEXEC != 0}
	}); err != nil {
		t.Fatal(err)
	}
	if flagErr != nil {
		t.Fatal(flagErr)
	}
	if diff := gocmp.Diff(flags{Access: unix.O_RDONLY, NonBlocking: true, CloseOnExec: true}, got); diff != "" {
		t.Errorf("flags of the opened file (-want +got):\n%s", diff)
	}
	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 0 {
		t.Errorf("the opened file is at offset %d, want 0: nothing may be read before it is returned", offset)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(oldState, string(data)); diff != "" {
		t.Errorf("content of the opened file (-want +got):\n%s", diff)
	}
}

// heldDirTRegularClosed runs OpenRegular on a closed Dir and on the zero Dir.
func heldDirTRegularClosed(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "file"), oldState)
	for name, d := range heldDirTClosedDirs(t, dir) {
		t.Run("error: a "+name+" Dir", func(t *testing.T) {
			var f *os.File
			var err error
			heldDirTGuard(t, "OpenRegular", func() { f, err = d.OpenRegular("file") })
			if f != nil {
				_ = f.Close()
				t.Errorf("OpenRegular returned a file, want nil")
			}
			if problem := heldDirTErr("closed", err); problem != "" {
				t.Errorf("OpenRegular: %s", problem)
			}
		})
	}
}
