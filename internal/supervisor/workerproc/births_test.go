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

package workerproc_test

import (
	"context"
	"crypto/rand"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/procinfo"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor/workerproc"
)

// storeTBound is how long one store call may take before the test unblocks it and reports it as blocked.
const storeTBound = 30 * time.Second

// storeTLimitFixed is the length of the encoding of the "limit" birth without its state root and with an empty third
// argument; the third argument is sized so that the whole encoding has exactly 65,536 bytes.
const storeTLimitFixed = 452

// storeTDir is the birth directory of the state root every store case uses, relative to the case's base directory.
const storeTDir = "state/worker-births"

// storeTOddParent is a directory name holding a quote and a backslash, which a record writes escaped.
const storeTOddParent = `odd "quoted" \ name`

// storeTNames are the record names of the fixture births: the process number, a hyphen, the lowercase hexadecimal
// SHA-256 of the start token, and ".json".
var storeTNames = map[string]string{
	"first":       "4242-39156f38138f921a79d777b878276a84330043580a82c7408d9fe9a55b5d3037.json",
	"replacement": "4242-fb15d5b740a8ebd089db3cb9ab9c96a79f9fb0e6aae68ea219716946897aca66.json",
	"other":       "5151-f1718b8924d7e05bb0411172cbb9ec0faacdde9707ab8458b7aaa3dcd474da30.json",
	"limit":       "4243-426cf8f61ff11212d5349a698e8efb2f1a11a23f5f8aef6696845b2b3e31d297.json",
	"over":        "4243-426cf8f61ff11212d5349a698e8efb2f1a11a23f5f8aef6696845b2b3e31d297.json",
}

// storeTSendError is the error a failing send returns.
type storeTSendError struct{}

func (storeTSendError) Error() string { return "send failed on purpose" }

// storeTPanic is the value a panicking send panics with.
const storeTPanic = "send panicked on purpose"

// storeTRecovered is the error a test call returns in place of the panic of send it recovered.
type storeTRecovered struct{}

func (storeTRecovered) Error() string { return "the panic of send reached the caller of Signal" }

// storeTQuoted returns root as it stands between the quotes of a JSON string, so that a state root holding a quote, a
// backslash or a control character is written into a literal record as an encoder writes it. It encodes the one
// dynamic string of a record; every other byte of the expected records stays literal.
func storeTQuoted(t *testing.T, root string) string {
	t.Helper()
	encoded, err := jsonv2.Marshal(root)
	if err != nil {
		t.Fatalf("fixture: encode the state root %q: %v", root, err)
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(encoded), `"`), `"`)
}

// storeTPad returns the length of the third argument of the "limit" birth (or, plus one, of the "over" birth) for the
// state root root, counting the root as it is written between its quotes.
func storeTPad(t *testing.T, key, root string) int {
	t.Helper()
	n := 65536 - storeTLimitFixed - len(storeTQuoted(t, root))
	if key == "over" {
		n++
	}
	return n
}

// storeTBirth returns the fixture birth key of the state root root.
func storeTBirth(t *testing.T, key, root string) workerproc.Birth {
	t.Helper()
	b := workerproc.Birth{
		SchemaVersion: 1,
		PID:           4242,
		Argv:          []string{birthTPath, "serve-worker"},
		StartedAt:     "Sat Nov 28 12:00:00 2026",
		StartToken:    "boot-1:12345",
		StateRoot:     root,
		Generation:    "g-0001",
		Version:       "1.2.3",
		Image:         procinfo.Image{Path: birthTPath, Device: 64769, Inode: 1311},
		Parent: workerproc.BirthParent{
			PID:        4100,
			Argv:       []string{birthTPath, "serve-shim"},
			StartedAt:  "Sat Nov 28 11:59:30 2026",
			StartToken: "boot-1:12000",
		},
	}
	switch key {
	case "first":
	case "replacement":
		b.StartedAt = "Sat Nov 28 12:05:00 2026"
		b.StartToken = "boot-2:12345"
		b.Generation = "g-0002"
	case "other":
		b.PID = 5151
		b.StartedAt = "Sat Nov 28 12:01:00 2026"
		b.StartToken = "boot-1:22222"
	case "limit", "over":
		b.PID = 4243
		b.StartToken = "boot-1:33333"
		b.Argv = []string{birthTPath, "serve-worker", strings.Repeat("a", storeTPad(t, key, root))}
	default:
		t.Fatalf("fixture: unknown birth %q", key)
	}
	return b
}

// storeTEncoding returns the exact bytes of the record of the fixture birth key of the state root root.
func storeTEncoding(t *testing.T, key, root string) string {
	t.Helper()
	const parent = `"parent":{"pid":4100,"argv":["/opt/model-gateway/bin/model-gateway","serve-shim"],"startedAt":"Sat Nov 28 11:59:30 2026","startToken":"boot-1:12000"}}`
	const image = `"version":"1.2.3","image":{"path":"/opt/model-gateway/bin/model-gateway","device":64769,"inode":1311},`
	quoted := storeTQuoted(t, root)
	switch key {
	case "first":
		return `{"schemaVersion":1,"pid":4242,"argv":["/opt/model-gateway/bin/model-gateway","serve-worker"],"startedAt":"Sat Nov 28 12:00:00 2026","startToken":"boot-1:12345","stateRoot":"` + quoted + `","generation":"g-0001",` + image + parent
	case "replacement":
		return `{"schemaVersion":1,"pid":4242,"argv":["/opt/model-gateway/bin/model-gateway","serve-worker"],"startedAt":"Sat Nov 28 12:05:00 2026","startToken":"boot-2:12345","stateRoot":"` + quoted + `","generation":"g-0002",` + image + parent
	case "other":
		return `{"schemaVersion":1,"pid":5151,"argv":["/opt/model-gateway/bin/model-gateway","serve-worker"],"startedAt":"Sat Nov 28 12:01:00 2026","startToken":"boot-1:22222","stateRoot":"` + quoted + `","generation":"g-0001",` + image + parent
	case "limit", "over":
		return `{"schemaVersion":1,"pid":4243,"argv":["/opt/model-gateway/bin/model-gateway","serve-worker","` + strings.Repeat("a", storeTPad(t, key, root)) + `"],"startedAt":"Sat Nov 28 12:00:00 2026","startToken":"boot-1:33333","stateRoot":"` + quoted + `","generation":"g-0001",` + image + parent
	}
	t.Fatalf("fixture: unknown birth %q", key)
	return ""
}

// storeTCheckFixtures fails the test unless every fixture birth of root encodes to its literal record, and the
// "limit" and "over" records have 65,536 and 65,537 bytes. It checks the test's own fixtures, not the store.
func storeTCheckFixtures(t *testing.T, root string) {
	t.Helper()
	for _, key := range []string{"first", "replacement", "other", "limit", "over"} {
		encoded, err := jsonv2.Marshal(storeTBirth(t, key, root))
		if err != nil {
			t.Fatalf("fixture: encode the %s birth: %v", key, err)
		}
		if string(encoded) != storeTEncoding(t, key, root) {
			t.Fatalf("fixture: the %s birth encodes to\n%.600s\nnot its literal record\n%.600s", key, encoded, storeTEncoding(t, key, root))
		}
	}
	if n := len(storeTEncoding(t, "limit", root)); n != 65536 {
		t.Fatalf("fixture: the limit record has %d bytes, want 65,536", n)
	}
	if n := len(storeTEncoding(t, "over", root)); n != 65537 {
		t.Fatalf("fixture: the over record has %d bytes, want 65,537", n)
	}
}

// storeTTree describes every entry below base, without following a symbolic link, by its path relative to base:
// "dir" and the permission bits, "file", the permission bits and the content (or the size, for a file this process
// may not read), "link" and the target, "fifo", "socket" or "other".
func storeTTree(t *testing.T, base string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == base {
			return nil
		}
		rel, err := filepath.Rel(base, path)
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
			if mode.Perm()&0o500 != 0o500 {
				return fs.SkipDir
			}
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
		t.Fatalf("describe the tree below %s: %v", base, err)
	}
	return tree
}

// storeTResolve turns a description of storeTCase.delta into the form storeTTree gives: "dir" is a private
// directory, "lock" an empty private file, "birth:<key>" a private file holding exactly that fixture's record and
// "changed:<key>" one holding that record and a line end; any other description is kept as it is.
func storeTResolve(t *testing.T, description, root string) string {
	t.Helper()
	switch description {
	case "dir":
		return "dir 0700"
	case "lock":
		return "file 0600 "
	}
	if key, ok := strings.CutPrefix(description, "birth:"); ok {
		return "file 0600 " + storeTEncoding(t, key, root)
	}
	if key, ok := strings.CutPrefix(description, "changed:"); ok {
		return "file 0600 " + storeTEncoding(t, key, root) + "\n"
	}
	return description
}

// storeTMkdir makes the directory path and gives it exactly the permission bits perm.
func storeTMkdir(t *testing.T, path string, perm fs.FileMode) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// storeTWrite writes data to a new private file at path.
func storeTWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeTSymlink makes a symbolic link at path whose target is target.
func storeTSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// storeTFIFO makes a FIFO at path.
func storeTFIFO(t *testing.T, path string) {
	t.Helper()
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeTSocket binds a Unix socket at name in dir and closes it, which leaves the socket entry in place. The bind
// uses a name relative to dir, which becomes the working directory of the test, because a record name below a test's
// temporary directory is longer than a socket address holds.
func storeTSocket(t *testing.T, dir, name string) {
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

// storeTUnreadable writes data to a file at path that grants no permission, and fails the test unless that really
// denies this process reading it: a process that file modes do not bind would make a case relying on it pass or fail
// for another reason.
func storeTUnreadable(t *testing.T, path, data string) {
	t.Helper()
	storeTWrite(t, path, data)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
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

// storeTUmask sets the creation mask to mask for the rest of the test. No test of this package runs in parallel, so
// the process-wide mask is not changed under another test.
func storeTUmask(t *testing.T, mask int) {
	t.Helper()
	previous := unix.Umask(mask)
	t.Cleanup(func() { unix.Umask(previous) })
}

// storeTReleaseFIFOs opens and closes every FIFO directly in each of dirs, and each of dirs that is itself a FIFO,
// for writing without blocking. An open for reading that waits for a writer returns once a writer has opened the FIFO.
func storeTReleaseFIFOs(t *testing.T, dirs ...string) {
	t.Helper()
	release := func(path string) {
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(fd)
		}
	}
	for _, dir := range dirs {
		if info, err := os.Lstat(dir); err == nil && info.Mode()&fs.ModeNamedPipe != 0 {
			release(dir)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type()&fs.ModeNamedPipe != 0 {
				release(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// storeTBounded runs call in its own goroutine and waits for it. A panic in call is carried back and fails the
// calling test with its text. When call has not returned within storeTBound, the test reports it as blocked and runs
// unblock, which ends the call's context and gives up every lock and FIFO the test holds, and then joins it. A call
// that ignores its ended context and every released obstruction is bounded only by the timeout of the test binary,
// which ends the whole test process: an in-process goroutine cannot be both abandoned and joined.
func storeTBounded(t *testing.T, what string, unblock, call func()) {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		call()
	}()
	var panicked any
	timer := time.NewTimer(storeTBound)
	defer timer.Stop()
	select {
	case panicked = <-done:
	case <-timer.C:
		// A result that was ready when the bound passed is the call's result, not a block.
		select {
		case panicked = <-done:
		default:
			t.Errorf("%s did not return within %v: it blocked", what, storeTBound)
			unblock()
			panicked = <-done
		}
	}
	if panicked != nil {
		t.Fatalf("%s panicked: %v", what, panicked)
	}
}

// storeTHold takes the lock of the lock file in dir through its own open file description, as another holder would,
// creating the file when it is missing, and returns the function that gives it up. how is unix.LOCK_EX for an
// exclusive holder or unix.LOCK_SH for a shared one: a method that holds the lock exclusively must not get it past
// either. The test gives it up at its end at the latest.
func storeTHold(t *testing.T, dir string, how int) func() {
	t.Helper()
	fd, err := unix.Open(filepath.Join(dir, "births.lock"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(fd, how|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("fixture: take the lock of %s: %v", dir, err)
	}
	release := sync.OnceFunc(func() {
		_ = unix.Flock(fd, unix.LOCK_UN)
		_ = unix.Close(fd)
	})
	t.Cleanup(release)
	return release
}

// storeTLockFree fails the test when the lock file of the birth directory of root is a regular file whose lock
// another open file description cannot take at once: every method releases the lock on every return. It reports
// whether the lock was free, so that a case stops instead of waiting behind a lock that was never released.
func storeTLockFree(t *testing.T, what, root string) bool {
	t.Helper()
	if !filepath.IsAbs(root) || strings.ContainsRune(root, 0) {
		return true
	}
	path := filepath.Join(root, "worker-births", "births.lock")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Errorf("after %s: open the lock file: %v", what, err)
		return true
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Errorf("after %s: the lock is still held (%v); want it released on every return", what, err)
		return false
	}
	_ = unix.Flock(fd, unix.LOCK_UN)
	return true
}

// storeTErr describes how err fails the expectation named by want, or returns "" when it meets it. The expectations
// are "" (no error), "invalid" (fs.ErrInvalid, and neither ErrBirth nor an ended context), "canceled"
// (context.Canceled, and neither fs.ErrInvalid nor ErrBirth), "deadline" (context.DeadlineExceeded), "birth"
// (ErrBirth, and not fs.ErrInvalid), "notexist" (fs.ErrNotExist), "exist" (fs.ErrExist), "permission"
// (fs.ErrPermission, and not fs.ErrInvalid), "dangling" (an error matching none of fs.ErrNotExist, fs.ErrExist and
// fs.ErrPermission), "notdir" (an error matching neither fs.ErrNotExist nor fs.ErrPermission), "fail" (any error),
// "send" (the error of a failing send) and "recovered" (storeTRecovered{}, a recovered panic of send).
func storeTErr(want string, err error) string {
	invalid, birth := errors.Is(err, fs.ErrInvalid), errors.Is(err, workerproc.ErrBirth)
	ended := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	ok := false
	switch want {
	case "":
		ok = err == nil
	case "invalid":
		ok = invalid && !birth && !ended
	case "canceled":
		ok = errors.Is(err, context.Canceled) && !invalid && !birth
	case "deadline":
		ok = errors.Is(err, context.DeadlineExceeded)
	case "birth":
		ok = birth && !invalid
	case "notexist":
		ok = errors.Is(err, fs.ErrNotExist)
	case "exist":
		ok = errors.Is(err, fs.ErrExist)
	case "permission":
		ok = errors.Is(err, fs.ErrPermission) && !invalid
	case "dangling":
		ok = err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrPermission)
	case "notdir":
		ok = err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission)
	case "fail":
		ok = err != nil
	case "send":
		ok = errors.Is(err, storeTSendError{})
	case "recovered":
		ok = errors.Is(err, storeTRecovered{})
	default:
		return fmt.Sprintf("unknown expectation %q", want)
	}
	if ok {
		return ""
	}
	if want == "" {
		return fmt.Sprintf("error %v (%T); want none", err, err)
	}
	return fmt.Sprintf("error %v (%T); want an error of the kind %q", err, err, want)
}

// storeTEntry is what a case expects of one entry Read returns: its name, the kind of its Err ("usable" for a nil
// Err, "invalid" for fs.ErrInvalid, "birth" for ErrBirth, "permission" for a read failure that fs.ErrPermission
// matches) and, for a usable entry, the fixture birth it holds.
type storeTEntry struct {
	Name  string
	Kind  string
	Birth string
	// Edit, when set, changes the fixture birth the same way the case changed the birth it recorded.
	Edit func(b *workerproc.Birth)
}

// storeTKind names the kind of the Err of an entry Read returned.
func storeTKind(err error) string {
	invalid, birth, permission := errors.Is(err, fs.ErrInvalid), errors.Is(err, workerproc.ErrBirth), errors.Is(err, fs.ErrPermission)
	switch {
	case err == nil:
		return "usable"
	case invalid && !birth && !permission:
		return "invalid"
	case birth && !invalid && !permission:
		return "birth"
	case permission && !invalid && !birth:
		return "permission"
	}
	return fmt.Sprintf("other: %v (%T)", err, err)
}

// storeTStep is one call of a store method, or one change the test makes between calls.
type storeTStep struct {
	// op is a method, "record", "retire", "signal" or "read", a change: "write" writes data, resolved as
	// storeTResolve resolves a birth, to the file at path; "link" replaces the entry at path with a symbolic link to
	// target, or a check: "absent" requires that nothing is at path, without following a link there; "regular"
	// requires a regular file at path and an entry of its directory whose name is exactly path's last element;
	// "locked" requires a private regular file at path, the lock file the call before it must have made. path is
	// relative to the case's base directory.
	op string
	// birth is the fixture birth the method takes, "first" when empty; edit, when set, changes it first.
	birth string
	edit  func(b *workerproc.Birth)
	// nilSend passes a nil send to Signal; sendErr makes send return storeTSendError{}.
	nilSend, sendErr bool
	// ok is the expected published, removed or attempted result, and err the expected error, as storeTErr reads it.
	ok  bool
	err string
	// sends is how many times Signal must call send.
	sends int
	// entries is what Read must return when err is "".
	entries []storeTEntry
	// absent requires Read to return a nil slice, as it does when the root, the birth directory or the directory a
	// link at worker-births names is missing.
	absent bool
	// path, data and target describe a change.
	path, data, target string
}

// storeTCase is one store case: the tree, the calls, and how the tree differs afterwards.
type storeTCase struct {
	// setup builds the tree below base and returns the Root the store is given.
	setup func(t *testing.T, base string) string
	// ended runs every method with a context that has already ended.
	ended bool
	steps []storeTStep
	// delta is how the tree below base differs after the steps from before them: a path relative to base and its
	// description as storeTResolve reads it, "-" for a removed entry, or "lock?" for a lock file that a method may
	// have created.
	delta map[string]string
}

// storeTEvery returns one call of each method, in the order Read, Retire, Signal, Record, each expecting the error
// err and a false result, with the birth changed by edit and, when nilSend is set, a nil send.
func storeTEvery(err string, edit func(b *workerproc.Birth), nilSend bool) []storeTStep {
	return []storeTStep{
		{op: "read", err: err},
		{op: "retire", edit: edit, err: err},
		{op: "signal", edit: edit, nilSend: nilSend, err: err},
		{op: "record", edit: edit, err: err},
	}
}

// storeTRunAll runs every case of cases as its own subtest.
func storeTRunAll(t *testing.T, cases map[string]storeTCase) {
	t.Helper()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { storeTRun(t, tc) })
	}
}

// storeTRun runs one store case.
func storeTRun(t *testing.T, tc storeTCase) {
	t.Helper()
	storeTUmask(t, 0o022)
	base := t.TempDir()
	root := tc.setup(t, base)
	storeTCheckFixtures(t, root)
	before := storeTTree(t, base)
	ctx := t.Context()
	if tc.ended {
		ended, cancel := context.WithCancel(ctx)
		cancel()
		ctx = ended
	}
	s := workerproc.Births{Root: root}
	for i, step := range tc.steps {
		storeTDo(t, ctx, s, base, root, fmt.Sprintf("step %d (%s)", i+1, step.op), step)
	}
	got := storeTTree(t, base)
	want := maps.Clone(before)
	for path, description := range tc.delta {
		switch description {
		case "-":
			delete(want, path)
		case "lock?":
			if _, ok := got[path]; ok {
				want[path] = "file 0600 "
			}
		default:
			want[path] = storeTResolve(t, description, root)
		}
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("tree after the calls (-want +got):\n%s", diff)
	}
}

// storeTDo runs one step of a store case.
func storeTDo(t *testing.T, ctx context.Context, s workerproc.Births, base, root, what string, step storeTStep) {
	t.Helper()
	switch step.op {
	case "write":
		data := step.data
		if key, ok := strings.CutPrefix(data, "changed:"); ok {
			data = storeTEncoding(t, key, root) + "\n"
		}
		if err := os.WriteFile(filepath.Join(base, step.path), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	case "link":
		if err := os.Remove(filepath.Join(base, step.path)); err != nil {
			t.Fatal(err)
		}
		storeTSymlink(t, step.target, filepath.Join(base, step.path))
		return
	case "absent":
		if _, err := os.Lstat(filepath.Join(base, step.path)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %s exists (error %v); want nothing there: no earlier call may make it", what, step.path, err)
		}
		return
	case "regular":
		path := filepath.Join(base, step.path)
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
			t.Errorf("%s: %s is %v (error %v); want a regular file at exactly that name", what, step.path, info, err)
		}
		// A file system that does not tell names apart by case resolves an alias above; the listing gives the name as
		// it is stored.
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatalf("%s: list %s: %v", what, filepath.Dir(step.path), err)
		}
		if !slices.ContainsFunc(entries, func(e fs.DirEntry) bool { return e.Name() == filepath.Base(path) }) {
			t.Errorf("%s: no entry of %s is named exactly %s", what, filepath.Dir(step.path), filepath.Base(path))
		}
		return
	case "locked":
		info, err := os.Lstat(filepath.Join(base, step.path))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: the lock file %s is %v (error %v); want a private regular file made by the call before it", what, step.path, info, err)
		}
		return
	}
	key := step.birth
	if key == "" {
		key = "first"
	}
	b := storeTBirth(t, key, root)
	if step.edit != nil {
		step.edit(&b)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	unblock := func() {
		cancel()
		storeTReleaseFIFOs(t, filepath.Join(base, storeTDir), filepath.Join(base, "state"))
	}
	var ok bool
	var err error
	switch step.op {
	case "record":
		storeTBounded(t, what, unblock, func() { ok, err = s.Record(ctx, b) })
	case "retire":
		storeTBounded(t, what, unblock, func() { ok, err = s.Retire(ctx, b) })
	case "signal":
		// An atomic count, so that two calls of send from goroutines of a faulty Signal are both counted.
		var calls atomic.Int32
		var send func() error
		if !step.nilSend {
			send = func() error {
				calls.Add(1)
				if step.sendErr {
					return storeTSendError{}
				}
				return nil
			}
		}
		storeTBounded(t, what, unblock, func() { ok, err = s.Signal(ctx, b, send) })
		if got := int(calls.Load()); got != step.sends {
			t.Errorf("%s: Signal called send %d times, want %d", what, got, step.sends)
		}
	case "read":
		var entries []workerproc.Stored
		storeTBounded(t, what, unblock, func() { entries, err = s.Read(ctx) })
		if problem := storeTErr(step.err, err); problem != "" {
			t.Errorf("%s: Read: %s", what, problem)
		}
		if step.err != "" {
			if entries != nil {
				t.Errorf("%s: Read failed but returned %d entries, want nil", what, len(entries))
			}
		} else {
			storeTCheckEntries(t, what, root, step.entries, entries)
			if step.absent && entries != nil {
				t.Errorf("%s: Read returned %#v, want a nil slice", what, entries)
			}
		}
		if !storeTLockFree(t, what, root) {
			t.FailNow()
		}
		return
	default:
		t.Fatalf("fixture: unknown step %q", step.op)
	}
	if problem := storeTErr(step.err, err); problem != "" {
		t.Errorf("%s: %s", what, problem)
	}
	if ok != step.ok {
		t.Errorf("%s: result %t, want %t (error %v)", what, ok, step.ok, err)
	}
	if !storeTLockFree(t, what, root) {
		t.FailNow()
	}
}

// storeTCheckEntries compares the entries Read returned with the expected ones, in order.
func storeTCheckEntries(t *testing.T, what, root string, want []storeTEntry, got []workerproc.Stored) {
	t.Helper()
	type entry struct{ Name, Kind string }
	project := func(e storeTEntry) entry { return entry{Name: e.Name, Kind: e.Kind} }
	wantEntries := make([]entry, 0, len(want))
	for _, e := range want {
		wantEntries = append(wantEntries, project(e))
	}
	gotEntries := make([]entry, 0, len(got))
	for _, s := range got {
		gotEntries = append(gotEntries, entry{Name: s.Name, Kind: storeTKind(s.Err)})
	}
	if diff := gocmp.Diff(wantEntries, gotEntries); diff != "" {
		t.Errorf("%s: Read entries (-want +got):\n%s", what, diff)
		return
	}
	for i, e := range want {
		if e.Kind != "usable" {
			continue
		}
		want := storeTBirth(t, e.Birth, root)
		if e.Edit != nil {
			e.Edit(&want)
		}
		if diff := gocmp.Diff(want, got[i].Birth); diff != "" {
			t.Errorf("%s: the birth Read decoded from %s (-want +got):\n%s", what, e.Name, diff)
		}
	}
}

// storeTState makes base/state holding a birth directory with the permission bits perm and the exact record of the
// "first" birth, and returns base/state.
func storeTState(t *testing.T, base string, perm fs.FileMode) string {
	t.Helper()
	root := filepath.Join(base, "state")
	storeTMkdir(t, root, 0o700)
	storeTMkdir(t, filepath.Join(root, "worker-births"), 0o700)
	storeTWrite(t, filepath.Join(root, "worker-births", storeTNames["first"]), storeTEncoding(t, "first", root))
	if err := os.Chmod(filepath.Join(root, "worker-births"), perm); err != nil {
		t.Fatal(err)
	}
	return root
}

// storeTEmptyRoot makes base/state without a birth directory and returns it.
func storeTEmptyRoot(t *testing.T, base string) string {
	t.Helper()
	root := filepath.Join(base, "state")
	storeTMkdir(t, root, 0o700)
	return root
}

// storeTRecordPath returns the path of the record of the fixture birth key, relative to the case's base directory.
func storeTRecordPath(key string) string { return storeTDir + "/" + storeTNames[key] }

// storeTSchema2 makes a birth of schema 2.
func storeTSchema2(b *workerproc.Birth) { b.SchemaVersion = 2 }

// storeTPrecedenceCases are the cases of the root, the context, the directory and its privacy, run through every
// method.
func storeTPrecedenceCases() map[string]storeTCase {
	return map[string]storeTCase{
		"error: a relative root": {
			setup: func(t *testing.T, base string) string {
				storeTState(t, base, 0o700)
				t.Chdir(base)
				return "state"
			},
			steps: storeTEvery("invalid", nil, false),
		},
		"error: a root with a trailing slash": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) + "/" },
			steps: storeTEvery("invalid", nil, false),
		},
		"error: a root with a dot-dot component": {
			setup: func(t *testing.T, base string) string {
				storeTState(t, base, 0o700)
				storeTMkdir(t, filepath.Join(base, "x"), 0o700)
				return base + "/x/../state"
			},
			steps: storeTEvery("invalid", nil, false),
		},
		"error: a root with a dot component": {
			setup: func(t *testing.T, base string) string {
				storeTState(t, base, 0o700)
				return base + "/./state"
			},
			steps: storeTEvery("invalid", nil, false),
		},
		"error: an empty root": {
			setup: func(t *testing.T, base string) string {
				t.Chdir(storeTState(t, base, 0o700))
				return ""
			},
			steps: storeTEvery("invalid", nil, false),
		},
		"error: a root holding a NUL byte": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) + "\x00" },
			steps: storeTEvery("invalid", nil, false),
		},
		"error: an invalid root comes before an ended context, an invalid birth and a nil send": {
			setup: func(t *testing.T, base string) string {
				storeTState(t, base, 0o700)
				t.Chdir(base)
				return "state"
			},
			ended: true,
			steps: storeTEvery("invalid", storeTSchema2, true),
		},
		"error: an ended context": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			ended: true,
			steps: storeTEvery("canceled", nil, false),
		},
		"error: an ended context comes before an invalid birth, a nil send and a missing root": {
			setup: func(t *testing.T, base string) string { return filepath.Join(base, "state") },
			ended: true,
			steps: storeTEvery("canceled", storeTSchema2, true),
		},
		"error: a missing root": {
			setup: func(t *testing.T, base string) string { return filepath.Join(base, "state") },
			steps: []storeTStep{
				{op: "read", absent: true},
				{op: "retire"},
				{op: "signal"},
				{op: "record", err: "notexist"},
			},
		},
		"error: a missing root below a missing directory": {
			setup: func(t *testing.T, base string) string { return filepath.Join(base, "absent", "state") },
			steps: []storeTStep{
				{op: "read", absent: true},
				{op: "retire"},
				{op: "signal"},
				{op: "record", err: "notexist"},
			},
		},
		"success: a missing birth directory is made by Record only": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "read", absent: true},
				{op: "absent", path: storeTDir},
				{op: "retire"},
				{op: "absent", path: storeTDir},
				{op: "signal"},
				{op: "absent", path: storeTDir},
				{op: "record", ok: true},
			},
			delta: map[string]string{storeTDir: "dir", storeTDir + "/births.lock": "lock", storeTRecordPath("first"): "birth:first"},
		},
		"error: a dangling symbolic link at the birth directory": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTSymlink(t, "elsewhere", filepath.Join(root, "worker-births"))
				return root
			},
			steps: []storeTStep{
				{op: "read", absent: true},
				{op: "retire"},
				{op: "signal"},
				{op: "record", err: "dangling"},
			},
		},
		"error: a regular file at the birth directory": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTWrite(t, filepath.Join(root, "worker-births"), "not a directory\n")
				return root
			},
			steps: storeTEvery("notdir", nil, false),
		},
		"error: a FIFO at the birth directory": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTFIFO(t, filepath.Join(root, "worker-births"))
				return root
			},
			steps: storeTEvery("notdir", nil, false),
		},
		"error: a symbolic link to a regular file at the birth directory": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTWrite(t, filepath.Join(root, "notes"), "not a directory\n")
				storeTSymlink(t, "notes", filepath.Join(root, "worker-births"))
				return root
			},
			steps: storeTEvery("notdir", nil, false),
		},
		"error: a birth directory readable by its group": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o750) },
			steps: storeTEvery("permission", nil, false),
		},
		"error: a birth directory searchable by others": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o701) },
			steps: storeTEvery("permission", nil, false),
		},
		"error: a birth directory with only the group read bit": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o740) },
			steps: storeTEvery("permission", nil, false),
		},
		"error: a birth directory with only the group write bit": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o720) },
			steps: storeTEvery("permission", nil, false),
		},
		"error: a birth directory with only the group search bit": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o710) },
			steps: storeTEvery("permission", nil, false),
		},
		"error: a birth directory with only the other read bit": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o704) },
			steps: storeTEvery("permission", nil, false),
		},
		"error: a birth directory with only the other write bit": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o702) },
			steps: storeTEvery("permission", nil, false),
		},
		"error: a private birth directory this process may not list": {
			setup: func(t *testing.T, base string) string {
				root := storeTState(t, base, 0o300)
				dir := filepath.Join(root, "worker-births")
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Errorf("give the birth directory its permission back: %v", err)
					}
				})
				if os.Geteuid() == 0 {
					t.Fatalf("the venue must not run as root: this case needs a file mode that denies listing %s", dir)
				}
				if _, err := os.ReadDir(dir); !errors.Is(err, fs.ErrPermission) {
					t.Fatalf("the file mode did not deny listing %s (error %v): this process is not bound by file modes", dir, err)
				}
				return root
			},
			steps: []storeTStep{{op: "read", err: "permission"}},
		},
		"error: a symbolic link to a directory readable by its group": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTMkdir(t, filepath.Join(root, "shared"), 0o700)
				storeTWrite(t, filepath.Join(root, "shared", storeTNames["first"]), storeTEncoding(t, "first", root))
				if err := os.Chmod(filepath.Join(root, "shared"), 0o750); err != nil {
					t.Fatal(err)
				}
				storeTSymlink(t, "shared", filepath.Join(root, "worker-births"))
				return root
			},
			steps: storeTEvery("permission", nil, false),
		},
		"success: a symbolic link to a private directory is followed by every method": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTMkdir(t, filepath.Join(root, "private"), 0o700)
				storeTWrite(t, filepath.Join(root, "private", storeTNames["first"]), storeTEncoding(t, "first", root))
				storeTSymlink(t, "private", filepath.Join(root, "worker-births"))
				return root
			},
			steps: []storeTStep{
				{op: "read", entries: []storeTEntry{{Name: storeTNames["first"], Kind: "usable", Birth: "first"}}},
				{op: "signal", ok: true, sends: 1},
				{op: "retire", ok: true},
				{op: "record", ok: true},
			},
			delta: map[string]string{"state/private/births.lock": "lock"},
		},
		"success: a symbolic link at the birth directory is resolved again by every method": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTMkdir(t, filepath.Join(root, "private-a"), 0o700)
				storeTWrite(t, filepath.Join(root, "private-a", storeTNames["first"]), storeTEncoding(t, "first", root))
				storeTMkdir(t, filepath.Join(root, "private-b"), 0o700)
				storeTWrite(t, filepath.Join(root, "private-b", storeTNames["other"]), storeTEncoding(t, "other", root))
				storeTSymlink(t, "private-a", filepath.Join(root, "worker-births"))
				return root
			},
			steps: []storeTStep{
				{op: "read", entries: []storeTEntry{{Name: storeTNames["first"], Kind: "usable", Birth: "first"}}},
				{op: "link", path: storeTDir, target: "private-b"},
				{op: "read", entries: []storeTEntry{{Name: storeTNames["other"], Kind: "usable", Birth: "other"}}},
				{op: "retire", birth: "other", ok: true},
			},
			delta: map[string]string{
				storeTDir: "link private-b",
				"state/private-b/" + storeTNames["other"]: "-",
				"state/private-a/births.lock":             "lock",
				"state/private-b/births.lock":             "lock",
			},
		},
	}
}

// storeTInvalidBirthCases are the cases of a birth that is not valid against the root, refused by every method that
// takes one before anything else happens.
func storeTInvalidBirthCases() map[string]storeTCase {
	edits := map[string]func(b *workerproc.Birth){
		"error: a birth of schema 2":                                 storeTSchema2,
		"error: a birth of schema 0":                                 func(b *workerproc.Birth) { b.SchemaVersion = 0 },
		"error: a birth of process number zero":                      func(b *workerproc.Birth) { b.PID = 0 },
		"error: a birth with an empty start token":                   func(b *workerproc.Birth) { b.StartToken = "" },
		"error: a birth with a one-element command line":             func(b *workerproc.Birth) { b.Argv = []string{birthTPath} },
		"error: a birth whose command line starts with another path": func(b *workerproc.Birth) { b.Argv[0] = "/usr/local/bin/model-gateway" },
		"error: a birth of another role":                             func(b *workerproc.Birth) { b.Argv[1] = "serve-shim" },
		"error: a birth of another state root":                       func(b *workerproc.Birth) { b.StateRoot = "/elsewhere" },
		"error: a birth with an empty generation":                    func(b *workerproc.Birth) { b.Generation = "" },
		"error: a birth with an invalid version":                     func(b *workerproc.Birth) { b.Version = "1.2" },
		"error: a birth with an empty version":                       func(b *workerproc.Birth) { b.Version = "" },
		"error: a birth with a relative image path": func(b *workerproc.Birth) {
			b.Image.Path = "opt/model-gateway/bin/model-gateway"
			b.Argv[0] = b.Image.Path
		},
		"error: a birth with an unclean image path": func(b *workerproc.Birth) {
			b.Image.Path = "/opt/model-gateway/bin/../bin/model-gateway"
			b.Argv[0] = b.Image.Path
		},
		"error: a birth with an image path holding a NUL byte": func(b *workerproc.Birth) {
			b.Image.Path = birthTPath + "\x00"
			b.Argv[0] = b.Image.Path
		},
		"error: a birth with image inode zero":                        func(b *workerproc.Birth) { b.Image.Inode = 0 },
		"error: a birth whose supervisor has process number zero":     func(b *workerproc.Birth) { b.Parent.PID = 0 },
		"error: a birth whose supervisor has the worker's number":     func(b *workerproc.Birth) { b.Parent.PID = 4242 },
		"error: a birth whose supervisor has an empty start token":    func(b *workerproc.Birth) { b.Parent.StartToken = "" },
		"error: a birth whose supervisor has an empty command line":   func(b *workerproc.Birth) { b.Parent.Argv = nil },
		"error: a birth whose supervisor has an empty first argument": func(b *workerproc.Birth) { b.Parent.Argv = []string{""} },
		"error: a birth with an unparsable start text":                func(b *workerproc.Birth) { b.StartedAt = "2026-11-28T12:00:00Z" },
		"error: a birth whose start text names the wrong weekday":     func(b *workerproc.Birth) { b.StartedAt = "Mon Nov 28 12:00:00 2026" },
		"error: a birth whose supervisor start text is unparsable":    func(b *workerproc.Birth) { b.Parent.StartedAt = "yesterday" },
		"error: a birth whose encoding fails":                         func(b *workerproc.Birth) { b.Argv = append(b.Argv, "\xff") },
	}
	// A process number one above the largest exists only where int is wider than 32 bits; the value is converted at
	// run time so that these files also build for a 32-bit target.
	if strconv.IntSize > 32 {
		over := int64(math.MaxInt32) + 1
		edits["error: a birth of a process number above the largest"] = func(b *workerproc.Birth) { b.PID = int(over) }
		edits["error: a birth whose supervisor has a process number above the largest"] = func(b *workerproc.Birth) { b.Parent.PID = int(over) }
	}
	cases := map[string]storeTCase{
		"error: a birth whose encoding has 65,537 bytes": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			steps: []storeTStep{
				{op: "retire", birth: "over", err: "birth"},
				{op: "signal", birth: "over", err: "birth"},
				{op: "record", birth: "over", err: "birth"},
			},
		},
		"error: an invalid birth comes before a nil send": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			steps: []storeTStep{{op: "signal", edit: storeTSchema2, nilSend: true, err: "birth"}},
		},
		"error: an invalid birth comes before a missing root": {
			setup: func(t *testing.T, base string) string { return filepath.Join(base, "state") },
			steps: []storeTStep{
				{op: "retire", edit: storeTSchema2, err: "birth"},
				{op: "signal", edit: storeTSchema2, err: "birth"},
				{op: "record", edit: storeTSchema2, err: "birth"},
			},
		},
		"error: an invalid birth comes before a missing birth directory": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "retire", edit: storeTSchema2, err: "birth"},
				{op: "signal", edit: storeTSchema2, err: "birth"},
				{op: "record", edit: storeTSchema2, err: "birth"},
			},
		},
		"error: an invalid birth comes before a directory readable by its group": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o750) },
			steps: []storeTStep{
				{op: "retire", edit: storeTSchema2, err: "birth"},
				{op: "signal", edit: storeTSchema2, err: "birth"},
				{op: "record", edit: storeTSchema2, err: "birth"},
			},
		},
	}
	for name, edit := range edits {
		cases[name] = storeTCase{
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			steps: []storeTStep{
				{op: "retire", edit: edit, err: "birth"},
				{op: "signal", edit: edit, err: "birth"},
				{op: "record", edit: edit, err: "birth"},
			},
		}
	}
	return cases
}

// storeTBoundaryCases are the cases of a birth whose process numbers are at the ends of the valid range: every method
// accepts it, so it is recorded under its literal record name, read back usable, signalled and retired.
func storeTBoundaryCases() map[string]storeTCase {
	const digest = "-39156f38138f921a79d777b878276a84330043580a82c7408d9fe9a55b5d3037.json"
	tests := map[string]struct {
		edit func(b *workerproc.Birth)
		// name is the record name the birth must get.
		name string
	}{
		"success: a birth of process number 1": {
			edit: func(b *workerproc.Birth) { b.PID = 1 },
			name: "1" + digest,
		},
		"success: a birth of the largest process number": {
			edit: func(b *workerproc.Birth) { b.PID = math.MaxInt32 },
			name: "2147483647" + digest,
		},
		"success: a birth whose supervisor has process number 1": {
			edit: func(b *workerproc.Birth) { b.Parent.PID = 1 },
			name: "4242" + digest,
		},
		"success: a birth whose supervisor has the largest process number": {
			edit: func(b *workerproc.Birth) { b.Parent.PID = math.MaxInt32 },
			name: "4242" + digest,
		},
	}
	cases := map[string]storeTCase{}
	for name, tt := range tests {
		cases[name] = storeTCase{
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "record", edit: tt.edit, ok: true},
				{op: "regular", path: storeTDir + "/" + tt.name},
				{op: "read", entries: []storeTEntry{{Name: tt.name, Kind: "usable", Birth: "first", Edit: tt.edit}}},
				{op: "signal", edit: tt.edit, ok: true, sends: 1},
				{op: "retire", edit: tt.edit, ok: true},
			},
			delta: map[string]string{storeTDir: "dir", storeTDir + "/births.lock": "lock"},
		}
	}
	return cases
}

// storeTNilSendCases are the cases of a nil send, refused before the root and the directory are looked at.
func storeTNilSendCases() map[string]storeTCase {
	return map[string]storeTCase{
		"error: a nil send": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o700) },
			steps: []storeTStep{{op: "signal", nilSend: true, err: "invalid"}},
		},
		"error: a nil send comes before a missing root": {
			setup: func(t *testing.T, base string) string { return filepath.Join(base, "state") },
			steps: []storeTStep{{op: "signal", nilSend: true, err: "invalid"}},
		},
		"error: a nil send comes before a missing birth directory": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{{op: "signal", nilSend: true, err: "invalid"}},
		},
		"error: a nil send comes before a directory readable by its group": {
			setup: func(t *testing.T, base string) string { return storeTState(t, base, 0o750) },
			steps: []storeTStep{{op: "signal", nilSend: true, err: "invalid"}},
		},
	}
}

// storeTNeighbours makes base/state with a private birth directory holding the records of the "replacement" and
// "other" births, a file of another name and a sibling staged for the record of "first", and returns base/state.
func storeTNeighbours(t *testing.T, base string) string {
	t.Helper()
	root := storeTEmptyRoot(t, base)
	dir := filepath.Join(root, "worker-births")
	storeTMkdir(t, dir, 0o700)
	storeTWrite(t, filepath.Join(dir, storeTNames["replacement"]), storeTEncoding(t, "replacement", root))
	storeTWrite(t, filepath.Join(dir, storeTNames["other"]), storeTEncoding(t, "other", root))
	storeTWrite(t, filepath.Join(dir, "notes.txt"), "notes\n")
	storeTWrite(t, filepath.Join(dir, storeTNames["first"]+".4242.0123456789ab.tmp"), storeTEncoding(t, "first", root))
	return root
}

// storeTLeafCases are the cases of the entry at a birth's record name, seen by Signal and then by Retire, beside
// entries neither may touch.
func storeTLeafCases() map[string]storeTCase {
	leaf := func(build func(t *testing.T, root, path string)) func(t *testing.T, base string) string {
		return func(t *testing.T, base string) string {
			root := storeTNeighbours(t, base)
			if build != nil {
				build(t, root, filepath.Join(root, "worker-births", storeTNames["first"]))
			}
			return root
		}
	}
	// A method that reads the bytes of a regular entry holds the lock while it does, so the lock file exists
	// afterwards; whether a method that finds no regular entry there made it is left open.
	untouched := map[string]string{storeTDir + "/births.lock": "lock?"}
	declined := map[string]string{storeTDir + "/births.lock": "lock"}
	removed := map[string]string{storeTDir + "/births.lock": "lock", storeTRecordPath("first"): "-"}
	// sameLength writes the exact record of the "first" birth with old replaced by new, a change of the same length
	// that keeps the process number and the start token.
	sameLength := func(old, new string) func(t *testing.T, root, path string) {
		return func(t *testing.T, root, path string) {
			encoding := storeTEncoding(t, "first", root)
			if len(old) != len(new) || strings.Count(encoding, old) != 1 {
				t.Fatalf("fixture: %q must occur once in the first record and have the length of %q", old, new)
			}
			storeTWrite(t, path, strings.Replace(encoding, old, new, 1))
		}
	}
	// Each refusal of a regular record runs alone, against a birth directory without a lock file, and the lock file
	// is checked at once: a call that read the record must have taken the lock, whichever method it is.
	refusals := map[string]struct {
		build func(t *testing.T, base string) string
		birth string
	}{
		"error: a record holding another birth": {
			build: leaf(func(t *testing.T, root, path string) { storeTWrite(t, path, storeTEncoding(t, "replacement", root)) }),
		},
		"error: a record with one more byte": {
			build: leaf(func(t *testing.T, root, path string) { storeTWrite(t, path, storeTEncoding(t, "first", root)+"\n") }),
		},
		"error: a record one byte shorter": {
			build: leaf(func(t *testing.T, root, path string) {
				encoding := storeTEncoding(t, "first", root)
				storeTWrite(t, path, encoding[:len(encoding)-1])
			}),
		},
		"error: a record of 65,537 bytes that starts with the exact record": {
			build: func(t *testing.T, base string) string {
				root := storeTNeighbours(t, base)
				storeTWrite(t, filepath.Join(root, "worker-births", storeTNames["limit"]), storeTEncoding(t, "limit", root)+"x")
				return root
			},
			birth: "limit",
		},
		"error: a record of the same length with another generation": {
			build: leaf(sameLength(`"generation":"g-0001"`, `"generation":"g-0009"`)),
		},
		"error: a record of the same length with another supervisor argument": {
			build: leaf(sameLength(`"serve-shim"`, `"serve-shin"`)),
		},
	}
	cases := map[string]storeTCase{
		"success: an exact record is signalled once, then retired": {
			setup: leaf(func(t *testing.T, root, path string) { storeTWrite(t, path, storeTEncoding(t, "first", root)) }),
			steps: []storeTStep{
				{op: "signal", ok: true, sends: 1},
				{op: "locked", path: storeTDir + "/births.lock"},
				{op: "retire", ok: true},
				{op: "retire"},
				{op: "signal"},
			},
			delta: removed,
		},
		"success: an exact record is retired by a first call": {
			setup: leaf(func(t *testing.T, root, path string) { storeTWrite(t, path, storeTEncoding(t, "first", root)) }),
			steps: []storeTStep{{op: "retire", ok: true}},
			delta: removed,
		},
		"success: a failing send is still an attempt": {
			setup: leaf(func(t *testing.T, root, path string) { storeTWrite(t, path, storeTEncoding(t, "first", root)) }),
			steps: []storeTStep{
				{op: "signal", sendErr: true, ok: true, err: "send", sends: 1},
				{op: "locked", path: storeTDir + "/births.lock"},
				{op: "retire", ok: true},
			},
			delta: removed,
		},
		"success: a record of exactly 65,536 bytes is signalled, then retired": {
			setup: func(t *testing.T, base string) string {
				root := storeTNeighbours(t, base)
				storeTWrite(t, filepath.Join(root, "worker-births", storeTNames["limit"]), storeTEncoding(t, "limit", root))
				return root
			},
			steps: []storeTStep{
				{op: "signal", birth: "limit", ok: true, sends: 1},
				{op: "locked", path: storeTDir + "/births.lock"},
				{op: "retire", birth: "limit", ok: true},
			},
			delta: map[string]string{storeTDir + "/births.lock": "lock", storeTRecordPath("limit"): "-"},
		},
		"error: a missing record": {
			setup: leaf(nil),
			steps: []storeTStep{{op: "signal"}, {op: "retire"}},
			delta: untouched,
		},
		"error: a symbolic link to a file holding the exact record is neither signalled nor retired": {
			setup: leaf(func(t *testing.T, root, path string) {
				storeTWrite(t, filepath.Join(root, "copy.json"), storeTEncoding(t, "first", root))
				storeTSymlink(t, "../copy.json", path)
			}),
			steps: []storeTStep{{op: "signal"}, {op: "retire"}},
			delta: untouched,
		},
		"error: a symbolic link to a directory is neither signalled nor retired": {
			setup: leaf(func(t *testing.T, root, path string) {
				storeTMkdir(t, filepath.Join(root, "elsewhere"), 0o700)
				storeTSymlink(t, "../elsewhere", path)
			}),
			steps: []storeTStep{{op: "signal"}, {op: "retire"}},
			delta: untouched,
		},
		"error: a dangling symbolic link is neither signalled nor retired": {
			setup: leaf(func(t *testing.T, root, path string) { storeTSymlink(t, "../absent.json", path) }),
			steps: []storeTStep{{op: "signal"}, {op: "retire"}},
			delta: untouched,
		},
		"error: a FIFO is neither signalled nor retired": {
			setup: leaf(func(t *testing.T, root, path string) { storeTFIFO(t, path) }),
			steps: []storeTStep{{op: "signal"}, {op: "retire"}},
			delta: untouched,
		},
		"error: a Unix socket is neither signalled nor retired": {
			setup: leaf(func(t *testing.T, root, path string) { storeTSocket(t, filepath.Dir(path), filepath.Base(path)) }),
			steps: []storeTStep{{op: "signal"}, {op: "retire"}},
			delta: untouched,
		},
		"error: a directory is neither signalled nor retired": {
			setup: leaf(func(t *testing.T, root, path string) { storeTMkdir(t, path, 0o700) }),
			steps: []storeTStep{{op: "signal"}, {op: "retire"}},
			delta: untouched,
		},
		"error: a record this process may not read is neither signalled nor retired": {
			setup: leaf(func(t *testing.T, root, path string) { storeTUnreadable(t, path, storeTEncoding(t, "first", root)) }),
			steps: []storeTStep{{op: "signal", err: "permission"}, {op: "retire", err: "permission"}},
			delta: untouched,
		},
	}
	for name, r := range refusals {
		for _, op := range []string{"signal", "retire"} {
			cases[name+", seen by "+op+" alone"] = storeTCase{
				setup: r.build,
				steps: []storeTStep{{op: op, birth: r.birth}, {op: "locked", path: storeTDir + "/births.lock"}},
				delta: declined,
			}
		}
	}
	return cases
}

// storeTReadCases are the cases of what Read makes of each kind of entry.
func storeTReadCases() map[string]storeTCase {
	first := storeTNames["first"]
	return map[string]storeTCase{
		"success: every entry but the lock file, in byte order, each judged on its own": {
			setup: func(t *testing.T, base string) string {
				root := storeTNeighbours(t, base)
				dir := filepath.Join(root, "worker-births")
				encoding := storeTEncoding(t, "first", root)
				write := func(name, data string) { storeTWrite(t, filepath.Join(dir, name), data) }
				replace := func(old, new string) string {
					if !strings.Contains(encoding, old) {
						t.Fatalf("fixture: the first record does not hold %q", old)
					}
					return strings.Replace(encoding, old, new, 1)
				}
				write(first, encoding)
				write(storeTNames["limit"], storeTEncoding(t, "limit", root))
				write("6000-wrong-name.json", encoding)
				write("7000-schema-2.json", replace(`"schemaVersion":1,`, `"schemaVersion":2,`))
				write("7001-other-root.json", storeTEncoding(t, "first", "/elsewhere"))
				write("8000-oversize.json", storeTEncoding(t, "over", root))
				write("8001-one-byte-over.json", storeTEncoding(t, "limit", root)+"x")
				write("a-not-json", "not json")
				write("b-array", "[]")
				write("births.lock", "")
				write("c-empty", "")
				write("d-unknown-member", strings.TrimSuffix(encoding, "}")+`,"future":1}`)
				write("e-duplicate-member", replace(`{"schemaVersion":1,`, `{"schemaVersion":1,"schemaVersion":1,`))
				write("f-wrong-type", replace(`"pid":4242,`, `"pid":"4242",`))
				write("g-trailing-newline", encoding+"\n")
				write("h-two-objects", encoding+encoding)
				write("i-reordered", replace(`{"schemaVersion":1,"pid":4242,`, `{"pid":4242,"schemaVersion":1,`))
				write("j-spaced", replace(`{"schemaVersion":1,`, `{"schemaVersion": 1,`))
				write("k-null-argv", replace(`"argv":["/opt/model-gateway/bin/model-gateway","serve-worker"]`, `"argv":null`))
				storeTSymlink(t, first, filepath.Join(dir, "l-link"))
				storeTMkdir(t, filepath.Join(dir, "m-directory"), 0o700)
				storeTFIFO(t, filepath.Join(dir, "n-fifo"))
				storeTSocket(t, dir, "o-socket")
				storeTSymlink(t, "absent", filepath.Join(dir, "p-dangling"))
				storeTUnreadable(t, filepath.Join(dir, "q-unreadable"), encoding)
				return root
			},
			steps: []storeTStep{{op: "read", entries: []storeTEntry{
				{Name: first, Kind: "usable", Birth: "first"},
				{Name: first + ".4242.0123456789ab.tmp", Kind: "invalid"},
				{Name: storeTNames["replacement"], Kind: "usable", Birth: "replacement"},
				{Name: storeTNames["limit"], Kind: "usable", Birth: "limit"},
				{Name: storeTNames["other"], Kind: "usable", Birth: "other"},
				{Name: "6000-wrong-name.json", Kind: "invalid"},
				{Name: "7000-schema-2.json", Kind: "birth"},
				{Name: "7001-other-root.json", Kind: "birth"},
				{Name: "8000-oversize.json", Kind: "invalid"},
				{Name: "8001-one-byte-over.json", Kind: "invalid"},
				{Name: "a-not-json", Kind: "invalid"},
				{Name: "b-array", Kind: "invalid"},
				{Name: "c-empty", Kind: "invalid"},
				{Name: "d-unknown-member", Kind: "invalid"},
				{Name: "e-duplicate-member", Kind: "invalid"},
				{Name: "f-wrong-type", Kind: "invalid"},
				{Name: "g-trailing-newline", Kind: "invalid"},
				{Name: "h-two-objects", Kind: "invalid"},
				{Name: "i-reordered", Kind: "invalid"},
				{Name: "j-spaced", Kind: "invalid"},
				{Name: "k-null-argv", Kind: "invalid"},
				{Name: "l-link", Kind: "invalid"},
				{Name: "m-directory", Kind: "invalid"},
				{Name: "n-fifo", Kind: "invalid"},
				{Name: "notes.txt", Kind: "invalid"},
				{Name: "o-socket", Kind: "invalid"},
				{Name: "p-dangling", Kind: "invalid"},
				{Name: "q-unreadable", Kind: "permission"},
			}}},
		},
		"error: the exact record under its name with the digest in uppercase": {
			// The uppercase spelling is the only entry, so the case holds on a file system that does not tell
			// names apart by case as well.
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTMkdir(t, filepath.Join(root, "worker-births"), 0o700)
				storeTWrite(t, filepath.Join(root, "worker-births", "4242-39156F38138F921A79D777B878276A84330043580A82C7408D9FE9A55B5D3037.json"), storeTEncoding(t, "first", root))
				return root
			},
			steps: []storeTStep{{op: "read", entries: []storeTEntry{
				{Name: "4242-39156F38138F921A79D777B878276A84330043580A82C7408D9FE9A55B5D3037.json", Kind: "invalid"},
			}}},
			delta: map[string]string{storeTDir + "/births.lock": "lock"},
		},
		"success: a birth directory holding only the lock file": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTMkdir(t, filepath.Join(root, "worker-births"), 0o700)
				storeTWrite(t, filepath.Join(root, "worker-births", "births.lock"), "")
				return root
			},
			steps: []storeTStep{{op: "read"}},
		},
		"success: an empty birth directory": {
			setup: func(t *testing.T, base string) string {
				root := storeTEmptyRoot(t, base)
				storeTMkdir(t, filepath.Join(root, "worker-births"), 0o700)
				return root
			},
			steps: []storeTStep{{op: "read"}},
			delta: map[string]string{storeTDir + "/births.lock": "lock?"},
		},
	}
}

// storeTEntryCases are the cases of one entry at the record name of the "first" birth, or of the "limit" birth,
// beside entries that stay usable: each check of Read decides the entry's Err on its own, because the entry has the
// name its record would have.
func storeTEntryCases() map[string]storeTCase {
	at := func(key string, build func(t *testing.T, root, path, encoding string)) func(t *testing.T, base string) string {
		return func(t *testing.T, base string) string {
			root := storeTNeighbours(t, base)
			build(t, root, filepath.Join(root, "worker-births", storeTNames[key]), storeTEncoding(t, "first", root))
			return root
		}
	}
	replaced := func(old, new string) func(t *testing.T, root, path, encoding string) {
		return func(t *testing.T, root, path, encoding string) {
			if !strings.Contains(encoding, old) {
				t.Fatalf("fixture: the first record does not hold %q", old)
			}
			storeTWrite(t, path, strings.Replace(encoding, old, new, 1))
		}
	}
	written := func(data func(root, encoding string) string) func(t *testing.T, root, path, encoding string) {
		return func(t *testing.T, root, path, encoding string) { storeTWrite(t, path, data(root, encoding)) }
	}
	read := func(key, kind string) []storeTStep {
		entries := []storeTEntry{
			{Name: storeTNames["first"] + ".4242.0123456789ab.tmp", Kind: "invalid"},
			{Name: storeTNames["replacement"], Kind: "usable", Birth: "replacement"},
			{Name: storeTNames["other"], Kind: "usable", Birth: "other"},
			{Name: "notes.txt", Kind: "invalid"},
		}
		entry := storeTEntry{Name: storeTNames[key], Kind: kind, Birth: key}
		if key == "first" {
			entries = slices.Insert(entries, 0, entry)
		} else {
			entries = slices.Insert(entries, 2, entry)
		}
		return []storeTStep{{op: "read", entries: entries}}
	}
	// invalid writes the exact record of the "first" birth with each old replaced by the new that follows it, and
	// fails the case unless the result is still a canonical encoding: such a record passes every check of Read up to
	// the validity of the birth, so its one Err comes from that check.
	invalid := func(pairs ...string) func(t *testing.T, root, path, encoding string) {
		return func(t *testing.T, root, path, encoding string) {
			for i := 0; i+1 < len(pairs); i += 2 {
				if strings.Count(encoding, pairs[i]) != 1 {
					t.Fatalf("fixture: the first record does not hold %q exactly once", pairs[i])
				}
				encoding = strings.Replace(encoding, pairs[i], pairs[i+1], 1)
			}
			var b workerproc.Birth
			if err := jsonv2.Unmarshal([]byte(encoding), &b, jsonv2.RejectUnknownMembers(true)); err != nil {
				t.Fatalf("fixture: decode the changed record: %v", err)
			}
			if again, err := jsonv2.Marshal(b); err != nil || string(again) != encoding {
				t.Fatalf("fixture: the changed record is not a canonical encoding (error %v):\n%s", err, encoding)
			}
			storeTWrite(t, path, encoding)
		}
	}
	// Every case reads the records of the neighbouring births, so the lock file exists afterwards.
	lock := map[string]string{storeTDir + "/births.lock": "lock"}
	cases := map[string]storeTCase{
		"success: the exact record at its own name": {
			setup: at("first", written(func(root, encoding string) string { return encoding })),
			steps: read("first", "usable"), delta: lock,
		},
		"success: an exact record of 65,536 bytes at its own name": {
			setup: at("limit", func(t *testing.T, root, path, encoding string) {
				storeTWrite(t, path, storeTEncoding(t, "limit", root))
			}),
			steps: read("limit", "usable"), delta: lock,
		},
		"error: a record with a line end after the exact encoding": {
			setup: at("first", written(func(root, encoding string) string { return encoding + "\n" })),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record whose members are in another order": {
			setup: at("first", replaced(`{"schemaVersion":1,"pid":4242,`, `{"pid":4242,"schemaVersion":1,`)),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record with white space between its tokens": {
			setup: at("first", replaced(`{"schemaVersion":1,`, `{"schemaVersion": 1,`)),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record with an unknown member": {
			setup: at("first", replaced(`"startToken":"boot-1:12000"}}`, `"startToken":"boot-1:12000"},"future":1}`)),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record with a duplicate member": {
			setup: at("first", replaced(`{"schemaVersion":1,`, `{"schemaVersion":1,"schemaVersion":1,`)),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record with a member of the wrong type": {
			setup: at("first", replaced(`"pid":4242,`, `"pid":"4242",`)),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record with a null command line": {
			setup: at("first", replaced(`"argv":["/opt/model-gateway/bin/model-gateway","serve-worker"]`, `"argv":null`)),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record holding the exact encoding twice": {
			setup: at("first", written(func(root, encoding string) string { return encoding + encoding })),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: an empty record": {
			setup: at("first", written(func(root, encoding string) string { return "" })),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record of schema 2": {
			setup: at("first", replaced(`{"schemaVersion":1,`, `{"schemaVersion":2,`)),
			steps: read("first", "birth"), delta: lock,
		},
		"error: a record of another state root": {
			setup: at("first", func(t *testing.T, root, path, encoding string) {
				storeTWrite(t, path, storeTEncoding(t, "first", "/elsewhere"))
			}),
			steps: read("first", "birth"), delta: lock,
		},
		"error: the record of another birth under this birth's name": {
			setup: at("first", func(t *testing.T, root, path, encoding string) {
				storeTWrite(t, path, storeTEncoding(t, "other", root))
			}),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a symbolic link to a file holding the exact record": {
			setup: at("first", func(t *testing.T, root, path, encoding string) {
				storeTWrite(t, filepath.Join(root, "copy.json"), encoding)
				storeTSymlink(t, "../copy.json", path)
			}),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a dangling symbolic link": {
			setup: at("first", func(t *testing.T, root, path, encoding string) { storeTSymlink(t, "../absent.json", path) }),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a directory": {
			setup: at("first", func(t *testing.T, root, path, encoding string) { storeTMkdir(t, path, 0o700) }),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a FIFO": {
			setup: at("first", func(t *testing.T, root, path, encoding string) { storeTFIFO(t, path) }),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a Unix socket": {
			setup: at("first", func(t *testing.T, root, path, encoding string) {
				storeTSocket(t, filepath.Dir(path), filepath.Base(path))
			}),
			steps: read("first", "invalid"), delta: lock,
		},
		"error: a record this process may not read": {
			setup: at("first", func(t *testing.T, root, path, encoding string) { storeTUnreadable(t, path, encoding) }),
			steps: read("first", "permission"), delta: lock,
		},
		"error: a record of 65,537 bytes at its own name": {
			setup: at("limit", func(t *testing.T, root, path, encoding string) {
				storeTWrite(t, path, storeTEncoding(t, "limit", root)+"x")
			}),
			steps: read("limit", "invalid"), delta: lock,
		},
	}
	const workerArgv = `"argv":["/opt/model-gateway/bin/model-gateway","serve-worker"]`
	const parentArgv = `"argv":["/opt/model-gateway/bin/model-gateway","serve-shim"]`
	births := map[string][]string{
		"error: a record of process number zero":                      {`"pid":4242,`, `"pid":0,`},
		"error: a record of a negative process number":                {`"pid":4242,`, `"pid":-1,`},
		"error: a record with an empty start token":                   {`"startToken":"boot-1:12345"`, `"startToken":""`},
		"error: a record with a one-element command line":             {workerArgv, `"argv":["/opt/model-gateway/bin/model-gateway"]`},
		"error: a record whose command line starts with another path": {workerArgv, `"argv":["/usr/local/bin/model-gateway","serve-worker"]`},
		"error: a record of another role":                             {workerArgv, `"argv":["/opt/model-gateway/bin/model-gateway","serve-shim"]`},
		"error: a record with an empty generation":                    {`"generation":"g-0001"`, `"generation":""`},
		"error: a record with an invalid version":                     {`"version":"1.2.3"`, `"version":"1.2"`},
		"error: a record with a relative image path": {
			workerArgv, `"argv":["opt/model-gateway/bin/model-gateway","serve-worker"]`,
			`"image":{"path":"/opt/model-gateway/bin/model-gateway"`, `"image":{"path":"opt/model-gateway/bin/model-gateway"`,
		},
		"error: a record with image inode zero":                        {`"inode":1311}`, `"inode":0}`},
		"error: a record whose supervisor has process number zero":     {`"parent":{"pid":4100,`, `"parent":{"pid":0,`},
		"error: a record whose supervisor has the worker's number":     {`"parent":{"pid":4100,`, `"parent":{"pid":4242,`},
		"error: a record whose supervisor has an empty start token":    {`"startToken":"boot-1:12000"`, `"startToken":""`},
		"error: a record whose supervisor has an empty command line":   {parentArgv, `"argv":[]`},
		"error: a record whose supervisor has an empty first argument": {parentArgv, `"argv":["","serve-shim"]`},
		"error: a record whose start text names the wrong weekday":     {`"startedAt":"Sat Nov 28 12:00:00 2026"`, `"startedAt":"Mon Nov 28 12:00:00 2026"`},
		"error: a record whose supervisor start text is unparsable":    {`"startedAt":"Sat Nov 28 11:59:30 2026"`, `"startedAt":"yesterday"`},
	}
	// A process number one above the largest decodes into an int only where int is wider than 32 bits.
	if strconv.IntSize > 32 {
		births["error: a record of a process number above the largest"] = []string{`"pid":4242,`, `"pid":2147483648,`}
		births["error: a record whose supervisor has a process number above the largest"] = []string{`"parent":{"pid":4100,`, `"parent":{"pid":2147483648,`}
	}
	for name, pairs := range births {
		cases[name] = storeTCase{setup: at("first", invalid(pairs...)), steps: read("first", "birth"), delta: lock}
	}
	return cases
}

// storeTExistingCases are the cases of an entry that already has the name of the record Record would create.
func storeTExistingCases() map[string]storeTCase {
	existing := func(build func(t *testing.T, root, path string)) func(t *testing.T, base string) string {
		return func(t *testing.T, base string) string {
			root := storeTNeighbours(t, base)
			build(t, root, filepath.Join(root, "worker-births", storeTNames["first"]))
			return root
		}
	}
	refused := []storeTStep{{op: "record", err: "exist"}}
	lock := map[string]string{storeTDir + "/births.lock": "lock"}
	return map[string]storeTCase{
		"error: an existing file with other bytes is kept": {
			setup: existing(func(t *testing.T, root, path string) { storeTWrite(t, path, "other bytes\n") }),
			steps: refused,
			delta: lock,
		},
		"error: an existing file with the exact record is kept": {
			setup: existing(func(t *testing.T, root, path string) { storeTWrite(t, path, storeTEncoding(t, "first", root)) }),
			steps: refused,
			delta: lock,
		},
		"error: an existing directory is kept": {
			setup: existing(func(t *testing.T, root, path string) { storeTMkdir(t, path, 0o700) }),
			steps: refused,
			delta: lock,
		},
		"error: an existing symbolic link to a file is kept": {
			setup: existing(func(t *testing.T, root, path string) {
				storeTWrite(t, filepath.Join(root, "copy.json"), "copy\n")
				storeTSymlink(t, "../copy.json", path)
			}),
			steps: refused,
			delta: lock,
		},
		"error: an existing dangling symbolic link is kept and its target is not made": {
			setup: existing(func(t *testing.T, root, path string) { storeTSymlink(t, "../absent.json", path) }),
			steps: refused,
			delta: lock,
		},
		"error: an existing FIFO is kept": {
			setup: existing(func(t *testing.T, root, path string) { storeTFIFO(t, path) }),
			steps: refused,
			delta: lock,
		},
		"error: an existing Unix socket is kept": {
			setup: existing(func(t *testing.T, root, path string) { storeTSocket(t, filepath.Dir(path), filepath.Base(path)) }),
			steps: refused,
			delta: lock,
		},
		"success: an existing lock file is kept as it is": {
			setup: func(t *testing.T, base string) string {
				root := storeTNeighbours(t, base)
				storeTWrite(t, filepath.Join(root, "worker-births", "births.lock"), "kept\n")
				return root
			},
			steps: []storeTStep{
				{op: "record", ok: true},
				{op: "read", entries: []storeTEntry{
					{Name: storeTNames["first"], Kind: "usable", Birth: "first"},
					{Name: storeTNames["first"] + ".4242.0123456789ab.tmp", Kind: "invalid"},
					{Name: storeTNames["replacement"], Kind: "usable", Birth: "replacement"},
					{Name: storeTNames["other"], Kind: "usable", Birth: "other"},
					{Name: "notes.txt", Kind: "invalid"},
				}},
				{op: "signal", ok: true, sends: 1},
				{op: "retire", ok: true},
			},
		},
	}
}

// storeTLockFileCases are the cases of a lock file that is not a regular file: every method fails before it reads or
// changes a record.
func storeTLockFileCases() map[string]storeTCase {
	lockFile := func(build func(t *testing.T, root, path string)) func(t *testing.T, base string) string {
		return func(t *testing.T, base string) string {
			root := storeTState(t, base, 0o700)
			build(t, root, filepath.Join(root, "worker-births", "births.lock"))
			return root
		}
	}
	return map[string]storeTCase{
		"error: a directory at the lock file": {
			setup: lockFile(func(t *testing.T, root, path string) { storeTMkdir(t, path, 0o700) }),
			steps: storeTEvery("fail", nil, false),
		},
		"error: a FIFO at the lock file": {
			setup: lockFile(func(t *testing.T, root, path string) { storeTFIFO(t, path) }),
			steps: storeTEvery("fail", nil, false),
		},
		"error: a symbolic link to a file at the lock file": {
			setup: lockFile(func(t *testing.T, root, path string) {
				storeTWrite(t, filepath.Join(root, "lock-target"), "")
				storeTSymlink(t, "../lock-target", path)
			}),
			steps: storeTEvery("fail", nil, false),
		},
		"error: a dangling symbolic link at the lock file": {
			setup: lockFile(func(t *testing.T, root, path string) { storeTSymlink(t, "../absent-lock", path) }),
			steps: storeTEvery("fail", nil, false),
		},
	}
}

// storeTExitCases are the cases of a supervised worker's birth: recorded once when its pid record is published,
// retired when its exit is observed, and never confused with the birth of a replacement worker.
func storeTExitCases() map[string]storeTCase {
	return map[string]storeTCase{
		"success: the record is exactly the birth's encoding in a private file of a private directory": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{{op: "record", ok: true}},
			delta: map[string]string{storeTDir: "dir", storeTDir + "/births.lock": "lock", storeTRecordPath("first"): "birth:first"},
		},
		"success: a recorded birth is read back and retired at the observed exit": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "record", ok: true},
				{op: "read", entries: []storeTEntry{{Name: storeTNames["first"], Kind: "usable", Birth: "first"}}},
				{op: "retire", ok: true},
				{op: "read"},
			},
			delta: map[string]string{storeTDir: "dir", storeTDir + "/births.lock": "lock"},
		},
		"error: a second record of one birth keeps the first": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "record", ok: true},
				{op: "record", err: "exist"},
			},
			delta: map[string]string{storeTDir: "dir", storeTDir + "/births.lock": "lock", storeTRecordPath("first"): "birth:first"},
		},
		"success: retiring a birth keeps the replacement of its process number and other workers": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "record", ok: true},
				{op: "record", birth: "replacement", ok: true},
				{op: "record", birth: "other", ok: true},
				{op: "retire", ok: true},
				{op: "retire"},
			},
			delta: map[string]string{
				storeTDir:                       "dir",
				storeTDir + "/births.lock":      "lock",
				storeTRecordPath("replacement"): "birth:replacement",
				storeTRecordPath("other"):       "birth:other",
			},
		},
		"success: retiring the replacement keeps the earlier birth of its process number": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "record", ok: true},
				{op: "record", birth: "replacement", ok: true},
				{op: "retire", birth: "replacement", ok: true},
			},
			delta: map[string]string{storeTDir: "dir", storeTDir + "/births.lock": "lock", storeTRecordPath("first"): "birth:first"},
		},
		"error: a record changed after it was written is not retired": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "record", ok: true},
				{op: "write", path: storeTRecordPath("first"), data: "changed:first"},
				{op: "retire"},
			},
			delta: map[string]string{storeTDir: "dir", storeTDir + "/births.lock": "lock", storeTRecordPath("first"): "changed:first"},
		},
		"success: a birth of exactly 65,536 bytes is recorded, read back and retired": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{
				{op: "record", birth: "limit", ok: true},
				{op: "read", entries: []storeTEntry{{Name: storeTNames["limit"], Kind: "usable", Birth: "limit"}}},
				{op: "retire", birth: "limit", ok: true},
			},
			delta: map[string]string{storeTDir: "dir", storeTDir + "/births.lock": "lock"},
		},
		"error: a birth of 65,537 bytes is not recorded": {
			setup: storeTEmptyRoot,
			steps: []storeTStep{{op: "record", birth: "over", err: "birth"}},
		},
		"success: a state root whose name holds a quote and a backslash": {
			setup: func(t *testing.T, base string) string {
				storeTMkdir(t, filepath.Join(base, storeTOddParent), 0o700)
				root := filepath.Join(base, storeTOddParent, "state")
				storeTMkdir(t, root, 0o700)
				return root
			},
			steps: []storeTStep{
				{op: "record", ok: true},
				{op: "read", entries: []storeTEntry{{Name: storeTNames["first"], Kind: "usable", Birth: "first"}}},
				{op: "signal", ok: true, sends: 1},
				{op: "retire", ok: true},
				{op: "record", birth: "limit", ok: true},
				{op: "read", entries: []storeTEntry{{Name: storeTNames["limit"], Kind: "usable", Birth: "limit"}}},
				{op: "retire", birth: "limit", ok: true},
			},
			delta: map[string]string{
				storeTOddParent + "/state/worker-births":             "dir",
				storeTOddParent + "/state/worker-births/births.lock": "lock",
			},
		},
	}
}

func TestBirthStore(t *testing.T) {
	storeTRunAll(t, storeTPrecedenceCases())
	storeTRunAll(t, storeTInvalidBirthCases())
	storeTRunAll(t, storeTBoundaryCases())
	storeTRunAll(t, storeTNilSendCases())
	storeTRunAll(t, storeTLeafCases())
	storeTRunAll(t, storeTReadCases())
	storeTRunAll(t, storeTEntryCases())
	storeTRunAll(t, storeTExistingCases())
	storeTRunAll(t, storeTLockFileCases())
	storeTSignals(t)
	storeTOverlap(t)
	storeTWaits(t)
}

// storeTLocked makes a state root holding the exact record of the "first" birth and returns it with its store.
func storeTLocked(t *testing.T) (string, workerproc.Births) {
	t.Helper()
	storeTUmask(t, 0o022)
	root := storeTState(t, t.TempDir(), 0o700)
	storeTCheckFixtures(t, root)
	return root, workerproc.Births{Root: root}
}

// storeTWatch starts a watchdog outside the fake clock of a bubble. A call that blocks in the kernel instead of
// waiting on a timer keeps the fake clock from moving; when the bubble has not finished within storeTBound of wall
// time, the watchdog runs unblock, which acts only on kernel objects the test holds (a lock), never on a value of the
// bubble, so that the call returns and the case reports it. It returns two functions. finish is called by the bubble's
// own callback once it has its result and has made its assertions: a callback that got that far did not block, even
// if its caller has not resumed yet when the bound passes. stop stops the watchdog, joins it and reports whether it
// fired; it acts once however often it is called and returns the same answer each time. The caller calls stop as soon
// as the bubble returns, so that only the bubble is timed, and also defers it, so that it runs when the bubble fails
// its test.
func storeTWatch(t *testing.T, what string, unblock func()) (stop func() bool, finish func()) {
	t.Helper()
	stopped := make(chan struct{})
	// An atomic, not a channel: the callback runs inside the bubble, and the watchdog outside it.
	var finished atomic.Bool
	fired := make(chan bool, 1)
	go func() {
		timer := time.NewTimer(storeTBound)
		defer timer.Stop()
		select {
		case <-stopped:
			fired <- false
		case <-timer.C:
			// A bubble whose callback had finished, or that had returned, when the bound passed did not block.
			select {
			case <-stopped:
				fired <- false
			default:
				if finished.Load() {
					fired <- false
					return
				}
				unblock()
				fired <- true
			}
		}
	}()
	return sync.OnceValue(func() bool {
		close(stopped)
		if <-fired {
			t.Errorf("%s: the bubble did not finish within %v of wall time, so the test gave its lock up; a call blocked in the kernel instead of waiting on a timer, or the venue stalled", what, storeTBound)
			return true
		}
		return false
	}), func() { finished.Store(true) }
}

// storeTSignalResult is what one call of Signal gave.
type storeTSignalResult struct {
	attempted bool
	err       error
	panicked  any
	// elapsed is how far the fake clock of the call's bubble moved while the call ran.
	elapsed time.Duration
}

// storeTSignalOnce calls Signal once inside a bubble of fake time, with a context that the fake clock ends after a
// minute, and returns what it gave. Signal never waits, so a conforming call returns before the fake clock moves. When
// the watchdog fires it first asks for Signal's context to end, then runs unblock, as storeTWatch describes it. The
// context belongs to the bubble, which nothing outside may touch, so the watchdog only sets a flag that a goroutine of
// the bubble turns into the cancellation at its next tick of the fake clock.
func storeTSignalOnce(t *testing.T, what string, s workerproc.Births, b workerproc.Birth, send func() error, unblock func()) storeTSignalResult {
	t.Helper()
	var abandon atomic.Bool
	stop, finish := storeTWatch(t, what, func() {
		abandon.Store(true)
		unblock()
	})
	defer stop()
	var r storeTSignalResult
	synctest.Test(t, func(t *testing.T) {
		defer finish()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		quit := make(chan struct{})
		helped := make(chan struct{})
		go func() {
			defer close(helped)
			for !abandon.Load() {
				select {
				case <-quit:
					return
				case <-time.After(time.Second):
				}
			}
			cancel()
		}()
		defer func() {
			close(quit)
			<-helped
		}()
		done := make(chan storeTSignalResult, 1)
		start := time.Now()
		go func() {
			var o storeTSignalResult
			defer func() {
				o.panicked = recover()
				done <- o
			}()
			o.attempted, o.err = s.Signal(ctx, b, send)
		}()
		r = <-done
		r.elapsed = time.Since(start)
	})
	stop()
	return r
}

// storeTProbe makes one attempt, without waiting, to share the lock of the lock file at path through another open file
// description. It describes what is wrong, or returns "" when the attempt fails because the lock is held exclusively.
// It is the test's own observation from inside send, not something a production send may do: that one neither opens
// nor locks the lock file and never waits.
func storeTProbe(path string) string {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Sprintf("open the lock file: %v", err)
	}
	defer func() { _ = unix.Close(fd) }()
	err = unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB)
	switch {
	case err == nil:
		_ = unix.Flock(fd, unix.LOCK_UN)
		return "another open file description could share it; want it held exclusively"
	case !errors.Is(err, unix.EWOULDBLOCK):
		return fmt.Sprintf("try to share it: %v", err)
	}
	return ""
}

// storeTSignals runs Signal beside another holder of the lock, under the lock Signal takes, and with a send that
// panics. Every call runs under a fake clock that must not move: Signal tries the lock once and never waits.
func storeTSignals(t *testing.T) {
	tests := map[string]struct {
		// hold is the lock another open file description holds while Signal runs: unix.LOCK_EX, unix.LOCK_SH, or 0
		// for none. The holder gives it up afterwards, and a second Signal must then send.
		hold int
		// send is what send does besides counting its calls: "" nothing, "probe" the observation of storeTProbe,
		// "panic" panic with storeTPanic.
		send string
		// want and sends are Signal's attempted result and how many times it called send.
		want  bool
		sends int
	}{
		"success: Signal gives up at once while another holder has the lock exclusively": {hold: unix.LOCK_EX},
		"success: Signal gives up at once while another holder shares the lock":          {hold: unix.LOCK_SH},
		"success: send runs while Signal holds the lock exclusively":                     {send: "probe", want: true, sends: 1},
		"success: a panic of send propagates after the lock is released":                 {send: "panic", sends: 1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root, s := storeTLocked(t)
			dir := filepath.Join(root, "worker-births")
			b := storeTBirth(t, "first", root)
			release := func() {}
			if tt.hold != 0 {
				release = storeTHold(t, dir, tt.hold)
			}
			// The count is atomic and the probe results are kept under a lock, so that a faulty Signal that calls send
			// twice at once is seen as such.
			var calls atomic.Int32
			var probeMu sync.Mutex
			var probes []string
			send := func() error {
				calls.Add(1)
				switch tt.send {
				case "probe":
					problem := storeTProbe(filepath.Join(dir, "births.lock"))
					probeMu.Lock()
					probes = append(probes, problem)
					probeMu.Unlock()
				case "panic":
					panic(storeTPanic)
				}
				return nil
			}
			got := storeTSignalOnce(t, "Signal", s, b, send, release)
			switch {
			case tt.send == "panic" && got.panicked != storeTPanic:
				t.Fatalf("Signal with a panicking send: recovered %v; want the panic of send to propagate", got.panicked)
			case tt.send != "panic" && got.panicked != nil:
				t.Fatalf("Signal panicked: %v", got.panicked)
			}
			if got.attempted != tt.want || got.err != nil || int(calls.Load()) != tt.sends {
				t.Errorf("Signal = %t, %v and %d sends; want %t, no error and %d sends", got.attempted, got.err, calls.Load(), tt.want, tt.sends)
			}
			if got.elapsed != 0 {
				t.Errorf("Signal took %v of fake time; want none: it never waits", got.elapsed)
			}
			probeMu.Lock()
			for _, problem := range probes {
				if problem != "" {
					t.Errorf("the lock while send ran: %s", problem)
				}
			}
			probeMu.Unlock()
			release()
			if !storeTLockFree(t, "Signal", root) {
				t.FailNow()
			}
			if tt.hold != 0 {
				again := storeTSignalOnce(t, "Signal after the release", s, b, send, func() {})
				if again.panicked != nil {
					t.Fatalf("Signal after the release panicked: %v", again.panicked)
				}
				if !again.attempted || again.err != nil || calls.Load() != 1 {
					t.Errorf("Signal after the release = %t, %v and %d sends in all; want true, no error and one send", again.attempted, again.err, calls.Load())
				}
			}
			if tt.send == "panic" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var removed bool
				var err error
				storeTBounded(t, "Retire after the panic", cancel, func() { removed, err = s.Retire(ctx, b) })
				if !removed || err != nil {
					t.Errorf("Retire after a panicking send = %t, %v; want true and no error", removed, err)
				}
			}
		})
	}
}

// storeTStager is the function of internal/atomicfile that draws the random part of a staged sibling's name, the one
// read storeTGate stops.
const storeTStager = "/internal/atomicfile.createSibling"

// storeTGate is a crypto/rand reader that stops the first read made by storeTStager while it is armed until release
// is closed, and passes that read and every other read to inner at once otherwise.
type storeTGate struct {
	inner   io.Reader
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

// Read stops the read when it is the first one storeTStager makes while g is armed, then reads from inner.
func (g *storeTGate) Read(p []byte) (int, error) {
	if g.armed.Load() && storeTCalledBy(storeTStager) && g.armed.CompareAndSwap(true, false) {
		close(g.reached)
		<-g.release
	}
	return g.inner.Read(p)
}

// storeTCalledBy reports whether a function whose full name ends in suffix is on the calling goroutine's stack,
// inlined frames included.
func storeTCalledBy(suffix string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, suffix) {
			return true
		}
		if !more {
			return false
		}
	}
}

// storeTOverlap runs two overlapping calls of one store value in one process: Record stops inside its hold of the
// lock, at the random bytes Dir.CreateExclusive reads through crypto/rand.Reader for the staged sibling's name (the read
// storeTStager makes; any other read of random bytes passes at once), and Signal must find the lock taken. The stop is in the test's reader, not in send, and the rows never run in parallel,
// because crypto/rand.Reader is a process-wide variable.
func storeTOverlap(t *testing.T) {
	tests := map[string]struct {
		// record is the fixture birth Record publishes; signal is the one Signal looks for, already recorded.
		record, signal string
	}{
		"success: Signal gives up while a Record of the same store holds the lock": {record: "other", signal: "first"},
	}
	type result struct {
		ok       bool
		err      error
		panicked any
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root, s := storeTLocked(t)
			dir := filepath.Join(root, "worker-births")
			recorded, signalled := storeTBirth(t, tt.record, root), storeTBirth(t, tt.signal, root)
			saved := rand.Reader
			gate := &storeTGate{inner: saved, reached: make(chan struct{}), release: make(chan struct{})}
			gate.armed.Store(true)
			rand.Reader = gate
			open := sync.OnceFunc(func() { close(gate.release) })
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan result, 1)
			go func() {
				var r result
				defer func() {
					r.panicked = recover()
					done <- r
				}()
				r.ok, r.err = s.Record(ctx, recorded)
			}()
			// Release and join on every return, a failed row included, and only then give the process its reader
			// back.
			// Once the gate is open, Record's return is awaited for storeTBound; after that the test cancels Record and
			// waits for its result, so that it is collected before the process gets its reader back. A Record that
			// ignores its ended context too is bounded only by the timeout of the test binary.
			var rec result
			joined := false
			join := func() {
				if joined {
					return
				}
				timer := time.NewTimer(storeTBound)
				defer timer.Stop()
				select {
				case rec = <-done:
				case <-timer.C:
					// A result that was ready when the bound passed is Record's result, not a block.
					select {
					case rec = <-done:
					default:
						t.Errorf("Record did not return within %v after the gate opened; the test cancels it and waits for it", storeTBound)
						cancel()
						rec = <-done
					}
				}
				joined = true
			}
			defer func() {
				cancel()
				open()
				join()
				rand.Reader = saved
			}()
			const assumption = "this row assumes that Dir.CreateExclusive draws the staged sibling's name through " + storeTStager + " from crypto/rand.Reader while Record holds the lock; the fixture's assumption failed, not the store"
			timer := time.NewTimer(storeTBound)
			defer timer.Stop()
			select {
			case <-gate.reached:
			case rec = <-done:
				joined = true
				if rec.panicked != nil {
					t.Fatalf("Record panicked: %v", rec.panicked)
				}
				t.Fatalf("Record returned (%t, %v) without the gated read: %s", rec.ok, rec.err, assumption)
			case <-timer.C:
				// The gate may have been reached, or Record may have returned, just as the bound passed.
				select {
				case <-gate.reached:
				case rec = <-done:
					joined = true
					if rec.panicked != nil {
						t.Fatalf("Record panicked: %v", rec.panicked)
					}
					t.Fatalf("Record returned (%t, %v) without the gated read: %s", rec.ok, rec.err, assumption)
				default:
					t.Fatalf("Record neither returned nor made the gated read within %v: %s", storeTBound, assumption)
				}
			}
			if problem := storeTProbe(filepath.Join(dir, "births.lock")); problem != "" {
				t.Errorf("the lock while Record staged its record: %s", problem)
			}
			var sends atomic.Int32
			send := func() error {
				sends.Add(1)
				return nil
			}
			// A Signal that waits for the lock Record holds could only return once Record gives it up, so its watchdog ends
			// Signal's context and Record's, and opens the gate, before it joins Signal: a Record that waits for its
			// own context while it holds the lock then returns too.
			signal := func(what string) (bool, error) {
				sctx, scancel := context.WithCancel(t.Context())
				defer scancel()
				var attempted bool
				var err error
				storeTBounded(t, what, func() {
					scancel()
					cancel()
					open()
				}, func() { attempted, err = s.Signal(sctx, signalled, send) })
				return attempted, err
			}
			// The contended Signal runs under a fake clock that must not move: with Record still gated it tries the lock
			// once and gives up, without a back-off on a timer. Its watchdog ends Signal's context, then Record's, and
			// then opens the gate.
			contended := storeTSignalOnce(t, "Signal while Record holds the lock", s, signalled, send, func() {
				cancel()
				open()
			})
			if contended.panicked != nil {
				t.Fatalf("Signal while Record held the lock panicked: %v", contended.panicked)
			}
			if contended.attempted || contended.err != nil || sends.Load() != 0 {
				t.Errorf("Signal while a Record of the same store held the lock = %t, %v and %d sends; want false, nil and no send", contended.attempted, contended.err, sends.Load())
			}
			if contended.elapsed != 0 {
				t.Errorf("Signal while a Record of the same store held the lock took %v of fake time; want none: it never waits", contended.elapsed)
			}
			open()
			join()
			if rec.panicked != nil {
				t.Fatalf("Record panicked: %v", rec.panicked)
			}
			if !rec.ok || rec.err != nil {
				t.Errorf("Record = %t, %v; want true and no error", rec.ok, rec.err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, storeTNames[tt.record])); err != nil || string(got) != storeTEncoding(t, tt.record, root) {
				t.Errorf("the record Record published = %.80q (error %v); want the exact encoding of the %s birth", got, err, tt.record)
			}
			if attempted, err := signal("Signal after Record"); !attempted || err != nil || sends.Load() != 1 {
				t.Errorf("Signal after Record returned = %t, %v and %d sends in all; want true, nil and one send", attempted, err, sends.Load())
			}
			if !storeTLockFree(t, "Signal after Record", root) {
				t.FailNow()
			}
		})
	}
}

// storeTWaits runs Record, Retire and Read while another holder has the lock, under a fake clock: each must wait on a
// timer without touching a record, then proceed once the lock is released, judging the records as they are by then,
// or end with its context, whether a deadline passes or the context is canceled.
func storeTWaits(t *testing.T) {
	type outcome struct {
		ok       bool
		err      error
		entries  []workerproc.Stored
		panicked any
	}
	calls := map[string]func(ctx context.Context, s workerproc.Births, b workerproc.Birth) outcome{
		"Record": func(ctx context.Context, s workerproc.Births, b workerproc.Birth) outcome {
			var o outcome
			o.ok, o.err = s.Record(ctx, b)
			return o
		},
		"Retire": func(ctx context.Context, s workerproc.Births, b workerproc.Birth) outcome {
			var o outcome
			o.ok, o.err = s.Retire(ctx, b)
			return o
		},
		"Read": func(ctx context.Context, s workerproc.Births, b workerproc.Birth) outcome {
			var o outcome
			o.entries, o.err = s.Read(ctx)
			o.ok = o.err == nil
			return o
		},
	}
	// replaced puts a new file at the record name of the "first" birth, holding that record and a line end, by
	// renaming a sibling over it, as another holder may while the call waits: a call that opened the record before it
	// held the lock still reads the old file. With add, it also writes the record of the "other" birth.
	replaced := func(add bool) func(t *testing.T, root, dir string) {
		return func(t *testing.T, root, dir string) {
			sibling := filepath.Join(dir, "replacement.tmp")
			storeTWrite(t, sibling, storeTEncoding(t, "first", root)+"\n")
			if err := os.Rename(sibling, filepath.Join(dir, storeTNames["first"])); err != nil {
				t.Fatal(err)
			}
			if add {
				storeTWrite(t, filepath.Join(dir, storeTNames["other"]), storeTEncoding(t, "other", root))
			}
		}
	}
	tests := map[string]struct {
		method string
		// birth is the fixture birth Record and Retire take.
		birth string
		// hold is how the holder holds the lock: unix.LOCK_EX or unix.LOCK_SH.
		hold int
		// end is how the wait ends: "release" (the holder gives the lock up once the call is waiting), "deadline" (the
		// call's context has the shortest positive deadline of the fake clock) or "cancel" (the call's context is
		// canceled once the call is waiting, while the holder keeps the lock).
		end string
		// change runs while the call waits, just before the holder gives the lock up.
		change func(t *testing.T, root, dir string)
		// want is the call's result: published or removed, or, for Read, no error.
		want bool
		err  string
		// entries are what Read must return.
		entries []storeTEntry
		// delta is how the birth directory differs afterwards: a record name and "-" or a description storeTResolve
		// reads.
		delta map[string]string
	}{
		"success: Record waits for an exclusive holder, then records": {
			method: "Record", birth: "other", hold: unix.LOCK_EX, end: "release", want: true,
			delta: map[string]string{storeTNames["other"]: "birth:other"},
		},
		"success: Record waits for a shared holder, then records": {
			method: "Record", birth: "other", hold: unix.LOCK_SH, end: "release", want: true,
			delta: map[string]string{storeTNames["other"]: "birth:other"},
		},
		"success: Record waits for the holder, which removes the entry at its name, then records": {
			method: "Record", birth: "first", hold: unix.LOCK_EX, end: "release", want: true,
			change: func(t *testing.T, root, dir string) {
				if err := os.Remove(filepath.Join(dir, storeTNames["first"])); err != nil {
					t.Fatal(err)
				}
			},
			delta: map[string]string{storeTNames["first"]: "birth:first"},
		},
		"success: Retire waits for an exclusive holder, then retires": {
			method: "Retire", birth: "first", hold: unix.LOCK_EX, end: "release", want: true,
			delta: map[string]string{storeTNames["first"]: "-"},
		},
		"success: Retire waits for a shared holder, then retires": {
			method: "Retire", birth: "first", hold: unix.LOCK_SH, end: "release", want: true,
			delta: map[string]string{storeTNames["first"]: "-"},
		},
		"success: Retire waits for the holder, then keeps the record the holder replaced": {
			method: "Retire", birth: "first", hold: unix.LOCK_EX, end: "release", change: replaced(false),
			delta: map[string]string{storeTNames["first"]: "changed:first"},
		},
		"success: Retire waits for the holder, which makes the missing record, then retires": {
			method: "Retire", birth: "other", hold: unix.LOCK_EX, end: "release", want: true,
			change: func(t *testing.T, root, dir string) {
				storeTWrite(t, filepath.Join(dir, storeTNames["other"]), storeTEncoding(t, "other", root))
			},
		},
		"success: Read waits for an exclusive holder, then reads": {
			method: "Read", hold: unix.LOCK_EX, end: "release", want: true,
			entries: []storeTEntry{{Name: storeTNames["first"], Kind: "usable", Birth: "first"}},
		},
		"success: Read waits for a shared holder, then reads": {
			method: "Read", hold: unix.LOCK_SH, end: "release", want: true,
			entries: []storeTEntry{{Name: storeTNames["first"], Kind: "usable", Birth: "first"}},
		},
		"success: Read waits for the holder, then reads the records the holder left": {
			method: "Read", hold: unix.LOCK_EX, end: "release", change: replaced(true), want: true,
			entries: []storeTEntry{
				{Name: storeTNames["first"], Kind: "invalid"},
				{Name: storeTNames["other"], Kind: "usable", Birth: "other"},
			},
			delta: map[string]string{storeTNames["first"]: "changed:first", storeTNames["other"]: "birth:other"},
		},
		"error: Record ends with its deadline while the holder keeps the lock": {
			method: "Record", birth: "other", hold: unix.LOCK_EX, end: "deadline", err: "deadline",
		},
		"error: Retire ends with its deadline while the holder keeps the lock": {
			method: "Retire", birth: "first", hold: unix.LOCK_EX, end: "deadline", err: "deadline",
		},
		"error: Read ends with its deadline while the holder keeps the lock": {
			method: "Read", hold: unix.LOCK_EX, end: "deadline", err: "deadline",
		},
		"error: Record ends when its context is canceled while the holder keeps the lock": {
			method: "Record", birth: "other", hold: unix.LOCK_EX, end: "cancel", err: "canceled",
		},
		"error: Retire ends when its context is canceled while the holder keeps the lock": {
			method: "Retire", birth: "first", hold: unix.LOCK_EX, end: "cancel", err: "canceled",
		},
		"error: Read ends when its context is canceled while the holder keeps the lock": {
			method: "Read", hold: unix.LOCK_EX, end: "cancel", err: "canceled",
		},
	}
	// blocked names the first row whose lock wait the watchdog had to end. Every later row of this family then fails at
	// once instead of paying the wall-time bound again; it is a failure, never a skip.
	blocked := ""
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if blocked != "" {
				t.Fatalf("not run: the lock wait blocked in the earlier row %q, which the watchdog had to end", blocked)
			}
			root, s := storeTLocked(t)
			dir := filepath.Join(root, "worker-births")
			key := tt.birth
			if key == "" {
				key = "first"
			}
			b := storeTBirth(t, key, root)
			release := storeTHold(t, dir, tt.hold)
			before := storeTTree(t, dir)
			want := maps.Clone(before)
			for k, v := range tt.delta {
				if v == "-" {
					delete(want, k)
				} else {
					want[k] = storeTResolve(t, v, root)
				}
			}
			stop, finish := storeTWatch(t, tt.method, release)
			defer func() {
				if stop() {
					blocked = name
				}
			}()
			synctest.Test(t, func(t *testing.T) {
				defer finish()
				// A release or a cancellation ends the wait, not a deadline, so that no retry interval is assumed.
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tt.end == "deadline" {
					var expire context.CancelFunc
					ctx, expire = context.WithTimeout(ctx, time.Nanosecond)
					defer expire()
				}
				done := make(chan outcome, 1)
				go func() {
					var o outcome
					defer func() {
						o.panicked = recover()
						done <- o
					}()
					o = calls[tt.method](ctx, s, b)
				}()
				var got outcome
				if tt.end != "deadline" {
					synctest.Wait()
					select {
					case got = <-done:
						t.Errorf("%s returned while another holder had the lock (result %t, error %v, panic %v); want it to wait", tt.method, got.ok, got.err, got.panicked)
						return
					default:
					}
					if diff := gocmp.Diff(before, storeTTree(t, dir)); diff != "" {
						t.Errorf("%s changed the birth directory while another holder had the lock (-want +got):\n%s", tt.method, diff)
					}
				}
				switch tt.end {
				case "release":
					if tt.change != nil {
						tt.change(t, root, dir)
					}
					release()
					got = <-done
				case "cancel":
					// No bound of fake time: the call may take any timer before it sees the cancellation. A call that
					// ignores it is released by the watchdog outside the bubble, which gives the holder's lock up.
					cancel()
					got = <-done
					if diff := gocmp.Diff(before, storeTTree(t, dir)); diff != "" {
						t.Errorf("%s changed the birth directory although its context was canceled (-want +got):\n%s", tt.method, diff)
					}
				default:
					got = <-done
				}
				if got.panicked != nil {
					t.Fatalf("%s panicked: %v", tt.method, got.panicked)
				}
				if problem := storeTErr(tt.err, got.err); problem != "" {
					t.Errorf("%s: %s", tt.method, problem)
				}
				if got.ok != tt.want {
					t.Errorf("%s result %t, want %t", tt.method, got.ok, tt.want)
				}
				switch {
				case tt.method != "Read":
				case tt.err != "" && got.entries != nil:
					t.Errorf("Read ended with its context but returned %d entries; want nil", len(got.entries))
				case tt.err == "":
					storeTCheckEntries(t, "Read", root, tt.entries, got.entries)
				}
			})
			stop()
			release()
			if !storeTLockFree(t, tt.method, root) {
				t.FailNow()
			}
			if diff := gocmp.Diff(want, storeTTree(t, dir)); diff != "" {
				t.Errorf("birth directory after %s (-want +got):\n%s", tt.method, diff)
			}
		})
	}
}
