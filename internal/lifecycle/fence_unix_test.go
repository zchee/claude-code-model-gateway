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

package lifecycle_test

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"

	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

const fenceName = "worker-start.lock"

// noPanic runs call and turns a panic into a test failure, so that one unimplemented function cannot abort the binary.
func noPanic[T any](t testing.TB, what string, call func() T) (result T) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("%s panicked: %v", what, p)
		}
	}()
	return call()
}

type fenceResult struct {
	release func() error
	err     error
}

func newFence(t testing.TB, state string) *lifecycle.StartFence {
	t.Helper()
	type made struct {
		fence *lifecycle.StartFence
		err   error
	}
	m := noPanic(t, "NewStartFence", func() made {
		f, err := lifecycle.NewStartFence(state)
		return made{f, err}
	})
	if m.err != nil || m.fence == nil {
		t.Fatalf("NewStartFence(%q) = %v, %v; want a fence", state, m.fence, m.err)
	}
	return m.fence
}

// checkLease fails unless exactly one of the release closure and the error is present.
func checkLease(t testing.TB, r fenceResult) {
	t.Helper()
	if (r.err == nil) != (r.release != nil) {
		t.Fatalf("Acquire returned a release closure (%t) with error %v; want exactly one of them", r.release != nil, r.err)
	}
}

// quietly runs a fallback cleanup. It runs only after a test stopped before its own release or close, so its error and
// any panic are dropped: the test has already reported why it stopped.
func quietly(cleanup func() error) {
	defer func() { _ = recover() }()
	_ = cleanup()
}

// guardLease registers a fallback that releases a lease the test owns if the test ends before releasing it, so that a
// failed assertion never leaves a fence locked for a later test. The returned closure is the one the test calls: every
// call disarms the fallback and then calls release, so a lease the test released gets no further call at cleanup.
func guardLease(t testing.TB, release func() error) func() error {
	armed := true
	t.Cleanup(func() {
		if armed {
			armed = false
			quietly(release)
		}
	})
	return func() error {
		armed = false
		return release()
	}
}

// ownFD registers a fallback that closes fd if the test ends before closing it, and returns the close the test calls.
// That call disarms the fallback first, so a number the system may hand out again is never closed a second time.
func ownFD(t testing.TB, fd int) (closeFD func() error) {
	open := true
	t.Cleanup(func() {
		if open {
			open = false
			_ = unix.Close(fd)
		}
	})
	return func() error {
		open = false
		return unix.Close(fd)
	}
}

func tryFence(t testing.TB, ctx context.Context, f *lifecycle.StartFence) fenceResult {
	t.Helper()
	r := noPanic(t, "StartFence.Acquire", func() fenceResult {
		release, err := f.Acquire(ctx)
		return fenceResult{release, err}
	})
	// Guarded the moment it is received, so that a lease the checks reject is still released.
	if r.release != nil {
		r.release = guardLease(t, r.release)
	}
	checkLease(t, r)
	return r
}

func holdFence(t testing.TB, f *lifecycle.StartFence) func() error {
	t.Helper()
	r := tryFence(t, t.Context(), f)
	if r.err != nil {
		t.Fatalf("Acquire on a free fence: %v", r.err)
	}
	return r.release
}

// unfence calls a fence's release closure and fails the test, not the binary, if it panics.
func unfence(t testing.TB, release func() error) error {
	t.Helper()
	return noPanic(t, "fence release", release)
}

// acquireAsync runs Acquire on its own goroutine and hands its one result to the test over an unbuffered channel, so
// a result is either received by the test or still with the goroutine, never parked in a buffer nobody reads. The
// test's cleanup abandons the handoff: a result that was not handed over by then, whenever it arrives, is kept by the
// goroutine, which releases a lease in it itself, so no lock outlives the test that took it. The cleanup then waits,
// bounded by the watchdog, for the goroutine to end. A panic is reported as a test error while the test is still
// running, and is handed over as a result without a lease.
func acquireAsync(t testing.TB, ctx context.Context, f *lifecycle.StartFence) <-chan fenceResult {
	results := make(chan fenceResult)
	abandoned, finished := make(chan struct{}), make(chan struct{})
	// ended is set at the end of the cleanup; after it, the test has completed and must not be reported to.
	var mu sync.Mutex
	ended := false
	go func() {
		defer close(finished)
		r := func() (r fenceResult) {
			defer func() {
				if p := recover(); p != nil {
					mu.Lock()
					if !ended {
						t.Errorf("StartFence.Acquire panicked: %v", p)
					}
					mu.Unlock()
					r = fenceResult{err: errors.New("Acquire panicked")}
				}
			}()
			release, err := f.Acquire(ctx)
			return fenceResult{release, err}
		}()
		select {
		case results <- r:
		case <-abandoned:
			if r.release != nil {
				quietly(r.release)
			}
		}
	}()
	t.Cleanup(func() {
		close(abandoned)
		select {
		case <-finished:
		case <-time.After(watchdogLimit):
		}
		mu.Lock()
		ended = true
		mu.Unlock()
	})
	return results
}

func receive(t testing.TB, results <-chan fenceResult) fenceResult {
	t.Helper()
	r := <-results
	// Guarded the moment it is received, so that a lease the checks reject is still released.
	if r.release != nil {
		r.release = guardLease(t, r.release)
	}
	if t.Failed() {
		t.FailNow()
	}
	checkLease(t, r)
	return r
}

// watchdogLimit bounds a wait that a correct implementation ends on its own. When it expires the test is aborted; it
// never decides an assertion, and no context the code under test receives carries it.
const watchdogLimit = 30 * time.Second

// leaseWithin acquires f under t.Context(), which has no deadline, so a lease the test then holds cannot end while
// it is held. The watchdog only aborts a test whose Acquire never returns; a lease that arrives after that is
// released by acquireAsync's goroutine, which the aborted test no longer receives from.
func leaseWithin(t testing.TB, f *lifecycle.StartFence, what string) func() error {
	t.Helper()
	results := acquireAsync(t, t.Context(), f)
	select {
	case <-time.After(watchdogLimit):
		t.Fatalf("watchdog: Acquire of %s did not return within %v; the test is aborted", what, watchdogLimit)
		return nil
	case r := <-results:
		// Guarded the moment it is received, so that a lease the checks reject is still released.
		if r.release != nil {
			r.release = guardLease(t, r.release)
		}
		if t.Failed() {
			t.FailNow()
		}
		checkLease(t, r)
		if r.err != nil {
			t.Fatalf("Acquire of %s: %v", what, r.err)
		}
		return r.release
	}
}

// openReference opens an unlocked, close-on-exec descriptor on the file at path, without following a link, and keeps
// it open until the test ends. While it is open the file cannot be freed, so no file created later can take its device
// and inode number: comparing the path with this descriptor shows the same file, where a number saved earlier would not.
func openReference(t testing.TB, path string) int {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open a reference to %s: %v", path, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

// sameFileAs reports whether path names, without following a link, the very file that the open descriptor fd refers to.
func sameFileAs(t testing.TB, path string, fd int) bool {
	t.Helper()
	var held, at unix.Stat_t
	if err := unix.Fstat(fd, &held); err != nil {
		t.Fatalf("fstat descriptor %d: %v", fd, err)
	}
	if err := unix.Lstat(path, &at); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false
		}
		t.Fatalf("lstat %s: %v", path, err)
	}
	return at.Dev == held.Dev && at.Ino == held.Ino
}

// fileMode returns the complete mode of the entry at path as lstat reports it: the file type and every permission
// and special bit. FileMode.Perm would drop the setuid, setgid and sticky bits.
func fileMode(t testing.TB, path string) uint32 {
	t.Helper()
	var st unix.Stat_t
	noError(t, unix.Lstat(path, &st))
	return uint32(st.Mode)
}

// occupy tries to make descriptor number fd refer to a sentinel file the test creates, without disturbing any open
// descriptor: F_DUPFD_CLOEXEC returns the lowest free number at or above its argument, so it lands on fd only when
// fd is free, and it never replaces what holds fd. When fd is held already, occupy closes what it opened and reports
// that the sentinel was not placed; the caller decides what that means for its fixture. The returned close, and the
// fallback at cleanup, close fd only while it still refers to the sentinel, so a number taken over is never closed.
func occupy(t testing.TB, fd int, sentinel string) (closeSentinel func(), placed bool) {
	t.Helper()
	noError(t, os.WriteFile(sentinel, []byte("sentinel"), 0o600))
	// The mode is set explicitly: the creation mask could have removed the read bit the open below needs.
	noError(t, os.Chmod(sentinel, 0o600))
	src, err := unix.Open(sentinel, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	noError(t, err)
	if src != fd {
		dup, err := unix.FcntlInt(uintptr(src), unix.F_DUPFD_CLOEXEC, fd)
		_ = unix.Close(src)
		noError(t, err)
		if dup != fd {
			_ = unix.Close(dup)
			return nil, false
		}
	}
	open := true
	closeSentinel = func() {
		if !open {
			return
		}
		open = false
		var held, want unix.Stat_t
		if unix.Fstat(fd, &held) == nil && unix.Lstat(sentinel, &want) == nil && held.Dev == want.Dev && held.Ino == want.Ino {
			_ = unix.Close(fd)
		}
	}
	t.Cleanup(closeSentinel)
	return closeSentinel, true
}

// descriptorOpen reports whether fd is an open descriptor of this process.
func descriptorOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// fenceHeld reports whether another open file description holds the advisory lock on path. It tries a
// nonblocking exclusive flock on a fresh description and never creates the file.
func fenceHeld(t testing.TB, path string) bool {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return false
	}
	if err != nil {
		t.Fatalf("open %s for a lock probe: %v", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	switch {
	case err == nil:
		noError(t, unix.Flock(fd, unix.LOCK_UN))
		return false
	case errors.Is(err, unix.EWOULDBLOCK):
		return true
	default:
		t.Fatalf("flock probe of %s: %v", path, err)
		return false
	}
}

// fenceDescriptors maps each descriptor of this process that refers to the file at path to its close-on-exec flag.
func fenceDescriptors(t testing.TB, path string) map[int]bool {
	t.Helper()
	var want unix.Stat_t
	if err := unix.Lstat(path, &want); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return map[int]bool{}
		}
		t.Fatalf("lstat %s: %v", path, err)
	}
	entries, err := os.ReadDir("/dev/fd")
	noError(t, err)
	found := map[int]bool{}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		var got unix.Stat_t
		// The directory listing's own descriptor is closed by now and fails here.
		if unix.Fstat(fd, &got) != nil || got.Dev != want.Dev || got.Ino != want.Ino {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		noError(t, err)
		found[fd] = flags&unix.FD_CLOEXEC != 0
	}
	return found
}

// besides checks that each of own, descriptors the test opened itself, is in a census of path and close-on-exec, and
// returns the census without them.
func besides(t testing.TB, path string, census map[int]bool, own ...int) map[int]bool {
	t.Helper()
	rest := maps.Clone(census)
	for _, fd := range own {
		cloexec, ok := rest[fd]
		if !ok {
			t.Fatalf("the test's own descriptor %d does not refer to %s (%v); the file at the path was replaced", fd, path, census)
		}
		if !cloexec {
			t.Fatalf("the test's own descriptor %d for %s is not close-on-exec", fd, path)
		}
		delete(rest, fd)
	}
	return rest
}

// onlyCloseOnExec fails unless exactly want descriptors besides own (see besides) refer to path and all of them are
// close-on-exec.
func onlyCloseOnExec(t testing.TB, path string, want int, own ...int) {
	t.Helper()
	got := besides(t, path, fenceDescriptors(t, path), own...)
	if len(got) != want {
		t.Fatalf("%d descriptors refer to %s (%v), want %d", len(got), path, got, want)
	}
	for fd, cloexec := range got {
		if !cloexec {
			t.Fatalf("descriptor %d for %s is inherited by child processes; the kernel would not release the fence when its holder dies", fd, path)
		}
	}
}

func entryNames(t testing.TB, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	noError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestFenceProbes checks the lock and descriptor probes the fence tests rely on, against descriptors this test opens itself.
func TestFenceProbes(t *testing.T) {
	path := filepath.Join(t.TempDir(), fenceName)
	noError(t, os.WriteFile(path, nil, 0o600))
	// Read and write permission set explicitly, whatever the creation mask removed: both opens below need them.
	noError(t, os.Chmod(path, 0o600))
	equal(t, false, fenceHeld(t, path))
	equal(t, map[int]bool{}, fenceDescriptors(t, path))

	held, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	noError(t, err)
	closeHeld := ownFD(t, held)
	noError(t, unix.Flock(held, unix.LOCK_EX|unix.LOCK_NB))
	equal(t, true, fenceHeld(t, path))
	equal(t, map[int]bool{held: true}, fenceDescriptors(t, path))

	inherited, err := unix.Open(path, unix.O_RDONLY, 0)
	noError(t, err)
	closeInherited := ownFD(t, inherited)
	equal(t, map[int]bool{held: true, inherited: false}, fenceDescriptors(t, path))
	// The census leaves out a descriptor the test names as its own, and keeps the rest.
	equal(t, map[int]bool{inherited: false}, besides(t, path, fenceDescriptors(t, path), held))
	noError(t, closeInherited())

	noError(t, unix.Flock(held, unix.LOCK_UN))
	equal(t, false, fenceHeld(t, path))
	noError(t, closeHeld())
	equal(t, map[int]bool{}, fenceDescriptors(t, path))
	equal(t, false, fenceHeld(t, filepath.Join(filepath.Dir(path), "absent")))
}

// TestFenceFixtureHelpers checks the file identity, mode, descriptor placement and fallback cleanup helpers the fence
// tests rely on, against files and descriptors this test creates itself.
func TestFenceFixtureHelpers(t *testing.T) {
	tests := map[string]func(t *testing.T){
		"success: a reference tells a recreated file from the original": func(t *testing.T) {
			path := filepath.Join(t.TempDir(), fenceName)
			noError(t, os.WriteFile(path, nil, 0o600))
			// The read bit the reference needs is set explicitly, whatever the creation mask removed.
			noError(t, os.Chmod(path, 0o600))
			reference := openReference(t, path)
			equal(t, true, sameFileAs(t, path, reference))
			onlyCloseOnExec(t, path, 0, reference)
			noError(t, unix.Unlink(path))
			equal(t, false, sameFileAs(t, path, reference))
			// The reference keeps the removed file allocated, so the new file cannot take its number.
			noError(t, os.WriteFile(path, nil, 0o600))
			equal(t, false, sameFileAs(t, path, reference))
			equal(t, map[int]bool{}, fenceDescriptors(t, path))
		},
		"success: the complete mode shows a setuid bit that the permission bits hide": func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mode")
			noError(t, os.WriteFile(path, nil, 0o600))
			// The creation mask may have removed permission bits; the mode under test is the one set here.
			noError(t, os.Chmod(path, 0o600))
			equal(t, uint32(unix.S_IFREG|0o600), fileMode(t, path))
			noError(t, os.Chmod(path, 0o600|fs.ModeSetuid))
			info, err := os.Lstat(path)
			noError(t, err)
			equal(t, fs.FileMode(0o600), info.Mode().Perm())
			equal(t, uint32(unix.S_IFREG|unix.S_ISUID|0o600), fileMode(t, path))
		},
		"success: a sentinel takes a freed descriptor number": func(t *testing.T) {
			dir := t.TempDir()
			scratch, err := unix.Open(filepath.Join(dir, "scratch"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
			noError(t, err)
			noError(t, ownFD(t, scratch)())
			equal(t, false, descriptorOpen(scratch))
			sentinel := filepath.Join(dir, "sentinel")
			closeSentinel, placed := occupy(t, scratch, sentinel)
			equal(t, true, placed)
			equal(t, true, descriptorOpen(scratch))
			equal(t, true, sameFileAs(t, sentinel, scratch))
			flags, err := unix.FcntlInt(uintptr(scratch), unix.F_GETFD, 0)
			noError(t, err)
			equal(t, unix.FD_CLOEXEC, flags&unix.FD_CLOEXEC)
			closeSentinel()
			equal(t, false, descriptorOpen(scratch))
		},
		"success: a sentinel is not placed on a held number, and the holder is left alone": func(t *testing.T) {
			dir := t.TempDir()
			heldPath := filepath.Join(dir, "held")
			held, err := unix.Open(heldPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
			noError(t, err)
			closeHeld := ownFD(t, held)
			sentinel := filepath.Join(dir, "sentinel")
			closeSentinel, placed := occupy(t, held, sentinel)
			equal(t, false, placed)
			if closeSentinel != nil {
				t.Fatal("occupy returned a close for a sentinel it did not place")
			}
			equal(t, true, descriptorOpen(held))
			equal(t, true, sameFileAs(t, heldPath, held))
			// Nothing occupy opened stays open.
			equal(t, map[int]bool{}, fenceDescriptors(t, sentinel))
			noError(t, closeHeld())
		},
		"success: a fallback runs once, and only when the test did not release": func(t *testing.T) {
			var armed, disarmed int
			t.Run("armed", func(t *testing.T) { guardLease(t, func() error { armed++; return nil }) })
			t.Run("disarmed", func(t *testing.T) {
				release := guardLease(t, func() error { disarmed++; return nil })
				noError(t, release())
				noError(t, release())
			})
			equal(t, [2]int{1, 2}, [2]int{armed, disarmed})
		},
		"success: a closed descriptor's fallback leaves its reused number open": func(t *testing.T) {
			dir := t.TempDir()
			var first, reused int
			t.Run("owner", func(t *testing.T) {
				var err error
				first, err = unix.Open(filepath.Join(dir, "first"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
				noError(t, err)
				noError(t, ownFD(t, first)())
				// Not owned by this subtest: its fallback must not close what now holds the number.
				reused, err = unix.Open(filepath.Join(dir, "second"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0o600)
				noError(t, err)
				if reused != first {
					// A lower number was free as well; move the new descriptor onto the freed one.
					placed, err := unix.FcntlInt(uintptr(reused), unix.F_DUPFD_CLOEXEC, first)
					_ = unix.Close(reused)
					noError(t, err)
					reused = placed
				}
			})
			closeReused := ownFD(t, reused)
			equal(t, first, reused)
			equal(t, true, descriptorOpen(reused))
			equal(t, true, sameFileAs(t, filepath.Join(dir, "second"), reused))
			noError(t, closeReused())
		},
	}
	for name, run := range tests {
		t.Run(name, run)
	}
}

func TestNewStartFence(t *testing.T) {
	tests := map[string]struct {
		root  func(dir string) string
		valid bool
	}{
		"success: clean absolute root": {root: func(dir string) string { return dir }, valid: true},
		"error: empty root":            {root: func(string) string { return "" }},
		"error: relative root":         {root: func(string) string { return "state" }},
		"error: dot root":              {root: func(string) string { return "." }},
		"error: trailing separator":    {root: func(dir string) string { return dir + "/" }},
		"error: dot segment":           {root: func(dir string) string { return dir + "/." }},
		"error: parent segment":        {root: func(dir string) string { return dir + "/state/.." }},
		"error: doubled separator":     {root: func(dir string) string { return filepath.Dir(dir) + "//" + filepath.Base(dir) }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			noError(t, os.Mkdir(filepath.Join(dir, "state"), 0o700))
			root := tt.root(dir)
			type made struct {
				fence *lifecycle.StartFence
				err   error
			}
			m := noPanic(t, "NewStartFence", func() made {
				f, err := lifecycle.NewStartFence(root)
				return made{f, err}
			})
			if tt.valid {
				if m.err != nil || m.fence == nil {
					t.Fatalf("NewStartFence(%q) = %v, %v; want a fence", root, m.fence, m.err)
				}
			} else if m.err == nil || m.fence != nil {
				t.Fatalf("NewStartFence(%q) = %v, %v; want an error and no fence", root, m.fence, m.err)
			}
			// Construction validates only; it neither creates nor locks the fence file.
			mustBeAbsent(t, filepath.Join(dir, fenceName))
			mustBeAbsent(t, filepath.Join(dir, "state", fenceName))
		})
	}
}

// TestStartFenceExcludesHolders shows that two holders of one state root exclude each other, as a nonblocking flock
// on a fresh open file description observes it. It does not show how a waiting Acquire is built (whether it waits
// on its own open file description, or opens one per attempt); reading the implementation decides that.
func TestStartFenceExcludesHolders(t *testing.T) {
	tests := map[string]struct{ separate bool }{
		"success: one fence value acquired twice": {},
		"success: two fence values for one root":  {separate: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state := t.TempDir()
				path := filepath.Join(state, fenceName)
				first := newFence(t, state)
				second := first
				if tt.separate {
					second = newFence(t, state)
				}
				held := holdFence(t, first)
				if !fenceHeld(t, path) {
					t.Fatal("Acquire returned without an advisory lock on worker-start.lock")
				}
				onlyCloseOnExec(t, path, 1)

				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				timed := tryFence(t, ctx, second)
				cancel()
				if !errors.Is(timed.err, context.DeadlineExceeded) {
					t.Fatalf("Acquire while another holder held the fence: %v, want context.DeadlineExceeded", timed.err)
				}

				waiter := acquireAsync(t, t.Context(), second)
				synctest.Wait() // The waiter is blocked, not merely slow.
				select {
				case r := <-waiter:
					if r.release != nil {
						guardLease(t, r.release)
					}
					t.Fatalf("second holder entered while the first held the fence (error %v)", r.err)
				default:
				}
				if !fenceHeld(t, path) {
					t.Fatal("the first holder lost the fence while another acquisition waited")
				}
				noError(t, unfence(t, held))
				next := receive(t, waiter)
				if next.err != nil {
					t.Fatalf("waiting Acquire after release: %v", next.err)
				}
				if !fenceHeld(t, path) {
					t.Fatal("the second holder returned without the advisory lock")
				}
				onlyCloseOnExec(t, path, 1)
				noError(t, unfence(t, next.release))
				if fenceHeld(t, path) {
					t.Fatal("the fence stayed locked after its last holder released it")
				}
				onlyCloseOnExec(t, path, 0)
			})
		})
	}
}

func TestStartFenceCancellation(t *testing.T) {
	tests := map[string]struct {
		contended bool
		deadline  bool
		want      error
	}{
		"error: cancelled before the call":   {want: context.Canceled},
		"error: cancelled while waiting":     {contended: true, want: context.Canceled},
		"error: deadline expires while held": {contended: true, deadline: true, want: context.DeadlineExceeded},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state := t.TempDir()
				path := filepath.Join(state, fenceName)
				fence := newFence(t, state)
				var holder func() error
				if tt.contended {
					holder = holdFence(t, newFence(t, state))
				}
				var ctx context.Context
				var cancel context.CancelFunc
				if tt.deadline {
					ctx, cancel = context.WithTimeout(t.Context(), 30*time.Second)
				} else {
					ctx, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				var got fenceResult
				if tt.contended {
					waiter := acquireAsync(t, ctx, fence)
					synctest.Wait()
					select {
					case r := <-waiter:
						if r.release != nil {
							guardLease(t, r.release)
						}
						t.Fatalf("Acquire returned while another holder held the fence (error %v)", r.err)
					default:
					}
					if !tt.deadline {
						cancel()
					}
					got = receive(t, waiter)
				} else {
					cancel()
					got = tryFence(t, ctx, fence)
				}
				if !errors.Is(got.err, tt.want) {
					t.Fatalf("Acquire error = %v, want %v", got.err, tt.want)
				}
				if tt.contended {
					// The abandoned attempt neither disturbed the holder nor kept a descriptor of its own.
					if !fenceHeld(t, path) {
						t.Fatal("a cancelled acquisition released another holder's fence")
					}
					onlyCloseOnExec(t, path, 1)
					noError(t, unfence(t, holder))
				}
				if fenceHeld(t, path) {
					t.Fatal("a cancelled acquisition left the fence locked")
				}
				onlyCloseOnExec(t, path, 0)
				// Nothing is held: an immediate acquisition succeeds without waiting.
				next := tryFence(t, t.Context(), fence)
				if next.err != nil {
					t.Fatalf("Acquire after a cancelled attempt: %v", next.err)
				}
				noError(t, unfence(t, next.release))
			})
		})
	}
}

func TestStartFenceRelease(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, fenceName)
	first, second := newFence(t, state), newFence(t, state)

	stale := holdFence(t, first)
	onlyCloseOnExec(t, path, 1)
	noError(t, unfence(t, stale))
	if fenceHeld(t, path) {
		t.Fatal("release left the fence locked")
	}
	onlyCloseOnExec(t, path, 0)
	if err := unfence(t, stale); err != nil {
		t.Fatalf("second call of the same release closure: %v, want nil", err)
	}

	// A stale closure from an earlier acquisition never releases or reacquires a later holder's fence.
	current := holdFence(t, second)
	if err := unfence(t, stale); err != nil {
		t.Fatalf("stale release closure while another holder held the fence: %v, want nil", err)
	}
	if !fenceHeld(t, path) {
		t.Fatal("a stale release closure released another holder's fence")
	}
	onlyCloseOnExec(t, path, 1)
	noError(t, unfence(t, current))
	onlyCloseOnExec(t, path, 0)

	// A fence value is reusable after its own release.
	again := holdFence(t, first)
	if !fenceHeld(t, path) {
		t.Fatal("reacquisition returned without the advisory lock")
	}
	noError(t, unfence(t, again))
	if fenceHeld(t, path) {
		t.Fatal("release after reacquisition left the fence locked")
	}
}

// TestStartFencePerStateRoot shows that the fence belongs to one state root: holding one root's fence never delays
// an acquisition for another root, and each root's file is locked by its own holder only.
func TestStartFencePerStateRoot(t *testing.T) {
	tests := map[string]struct {
		// other returns the second state root, given the first.
		other func(t *testing.T, first string) string
	}{
		"success: sibling state roots": {other: func(t *testing.T, _ string) string { return t.TempDir() }},
		"success: state root nested inside another": {other: func(t *testing.T, first string) string {
			nested := filepath.Join(first, "nested")
			noError(t, os.Mkdir(nested, 0o700))
			return nested
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			firstRoot := t.TempDir()
			secondRoot := tt.other(t, firstRoot)
			firstPath, secondPath := filepath.Join(firstRoot, fenceName), filepath.Join(secondRoot, fenceName)
			first, second := newFence(t, firstRoot), newFence(t, secondRoot)
			// Real time, not a synctest bubble: an acquisition blocked on a process-wide mutex would hang a bubble
			// instead of failing. The evidence is the order of events: each root's lease is acquired while the other
			// root's fence is still held, as the probes show before and after. Every lease is acquired under
			// t.Context(), which no deadline ends; the watchdog only aborts an acquisition that never returns.
			promptly := func(f *lifecycle.StartFence, which string) func() error {
				t.Helper()
				return leaseWithin(t, f, "the "+which+" root's fence while the other root's fence is held")
			}
			descriptors := func(held bool) int {
				if held {
					return 1
				}
				return 0
			}
			// held checks both roots' files: whether each is locked, and that only its own holder's one
			// close-on-exec descriptor refers to it.
			held := func(wantFirst, wantSecond bool) {
				t.Helper()
				equal(t, [2]bool{wantFirst, wantSecond}, [2]bool{fenceHeld(t, firstPath), fenceHeld(t, secondPath)})
				onlyCloseOnExec(t, firstPath, descriptors(wantFirst))
				onlyCloseOnExec(t, secondPath, descriptors(wantSecond))
			}

			releaseFirst := holdFence(t, first)
			held(true, false)
			releaseSecond := promptly(second, "second")
			held(true, true)
			// Releasing one root's fence leaves the other's held, and the released one is free again at once.
			noError(t, unfence(t, releaseFirst))
			held(false, true)
			releaseFirst = promptly(first, "first")
			held(true, true)
			noError(t, unfence(t, releaseSecond))
			held(true, false)
			noError(t, unfence(t, releaseFirst))
			held(false, false)
		})
	}
}

// TestStartFenceReleaseReportsUnlockFailure makes the unlock call of one release fail. That release reports the
// failure and still closes its descriptor, which gives the lock up. Calling the same closure again does nothing: it
// neither unlocks nor closes again. That is shown against the failed holder's descriptor number itself, which before
// the repeated call is held by a descriptor the test owns: the later holder's fence descriptor, when it took the
// number, or otherwise a sentinel the test places there. A descriptor of any other owner is never the witness, since
// its owner may close or reuse it. When such a descriptor holds the number, the fixture is set up again, a bounded
// number of times, with the sentinel placed before the later holder acquires.
func TestStartFenceReleaseReportsUnlockFailure(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, fenceName)
	errUnlock := errors.New("unlock refused by the test")
	var calls []int
	failNext := false
	var unlock func(fd int) error
	unlock = lifecycle.ReplaceFenceUnlock(t, func(fd int) error {
		calls = append(calls, fd)
		// The release unlocks the descriptor that holds the fence, before it closes that descriptor.
		if _, ok := fenceDescriptors(t, path)[fd]; !ok {
			t.Errorf("unlock was called with descriptor %d, which does not refer to the fence file", fd)
		}
		if !fenceHeld(t, path) {
			t.Error("unlock was called after the fence had already been given up")
		}
		if failNext {
			failNext = false
			return errUnlock
		}
		return unlock(fd)
	})

	// setUp runs the case once and reports false, having released everything it acquired, when the failed holder's
	// number is held by a descriptor the test does not own. sentinelFirst places the sentinel right after the failed
	// release, while the number is free, so that nothing the later holder opens can take it.
	setUp := func(sentinelFirst bool) (witnessed bool) {
		calls, failNext = nil, true
		failing := holdFence(t, newFence(t, state))
		onlyCloseOnExec(t, path, 1)
		var former int
		for fd := range fenceDescriptors(t, path) {
			former = fd
		}
		// The cause must stay matchable: a caller that logs the failure needs the operating system's error, so the
		// returned error wraps the unlock call's error instead of only describing it.
		if err := unfence(t, failing); !errors.Is(err, errUnlock) {
			t.Fatalf("release whose unlock failed = %v, want an error that wraps the unlock failure", err)
		}
		equal(t, []int{former}, calls)
		// The descriptor was closed although the unlock failed, and closing the only description gave the lock up.
		onlyCloseOnExec(t, path, 0)
		if fenceHeld(t, path) {
			t.Fatal("the fence stayed locked after a release whose unlock failed")
		}

		sentinel := filepath.Join(t.TempDir(), "sentinel")
		var closeSentinel func()
		placed := false
		if sentinelFirst {
			closeSentinel, placed = occupy(t, former, sentinel)
		}
		successor := holdFence(t, newFence(t, state))
		descriptors := fenceDescriptors(t, path)
		equal(t, 1, len(descriptors))
		_, successorTook := descriptors[former]
		if !placed && !successorTook {
			closeSentinel, placed = occupy(t, former, sentinel)
		}
		if !placed && !successorTook {
			noError(t, unfence(t, successor))
			return false
		}
		if placed {
			defer closeSentinel()
		}
		var witness unix.Stat_t
		noError(t, unix.Fstat(former, &witness))

		// What a repeated call returns after a failed release is not fixed here; what it does is.
		_ = unfence(t, failing)
		equal(t, []int{former}, calls)
		if !descriptorOpen(former) {
			t.Fatalf("a repeated call of a failed release closure closed descriptor %d, which the test held again", former)
		}
		var after unix.Stat_t
		noError(t, unix.Fstat(former, &after))
		if after.Dev != witness.Dev || after.Ino != witness.Ino {
			t.Fatalf("descriptor %d refers to another file after a repeated call of a failed release closure", former)
		}
		equal(t, descriptors, fenceDescriptors(t, path))
		if !fenceHeld(t, path) {
			t.Fatal("a repeated call of a failed release closure released a later holder's fence")
		}
		noError(t, unfence(t, successor))
		equal(t, 2, len(calls))
		onlyCloseOnExec(t, path, 0)
		if fenceHeld(t, path) {
			t.Fatal("the later holder's release left the fence locked")
		}
		return true
	}
	const setUps = 4
	for attempt := range setUps {
		if setUp(attempt > 0) {
			return
		}
	}
	t.Fatalf("fixture not established: in each of %d set-ups the failed holder's descriptor number was held by a "+
		"descriptor this test does not own, so neither the later holder's fence descriptor nor a sentinel could witness it", setUps)
}

func TestStartFenceFileRetained(t *testing.T) {
	tests := map[string]struct {
		existing *string
	}{
		"success: created on first acquisition":     {},
		"success: existing file is reused as it is": {existing: new("left by an earlier holder\n")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			path := filepath.Join(state, fenceName)
			wantData := ""
			if tt.existing != nil {
				wantData = *tt.existing
				noError(t, os.WriteFile(path, []byte(wantData), 0o600))
				// The mode is set explicitly, whatever the creation mask removed: the reference below reads it.
				noError(t, os.Chmod(path, 0o600))
			}
			fence := newFence(t, state)
			// original is an unlocked reference to the fence file: the existing file, or the one the first
			// acquisition creates. Comparing the path with it shows the same file even if a replacement could
			// have reused a removed file's inode number.
			original := -1
			if tt.existing != nil {
				original = openReference(t, path)
			}
			for attempt := range 3 {
				held := func() func() error {
					if attempt == 0 && tt.existing == nil {
						// With an empty creation mask the new file shows exactly the mode Acquire asked for.
						// This test is sequential because the creation mask belongs to the test process.
						previous := unix.Umask(0)
						defer unix.Umask(previous)
					}
					return holdFence(t, fence)
				}()
				info, err := os.Lstat(path)
				noError(t, err)
				if !info.Mode().IsRegular() {
					t.Fatalf("fence path has mode %v, want a regular file", info.Mode())
				}
				if original < 0 {
					original = openReference(t, path)
					// The complete mode: a regular file with permission bits 0600 and no setuid, setgid or
					// sticky bit, which a comparison of the permission bits alone would not see.
					if got := fileMode(t, path); got != unix.S_IFREG|0o600 {
						t.Fatalf("new fence file has mode %#o, want %#o", got, unix.S_IFREG|0o600)
					}
				} else if !sameFileAs(t, path, original) {
					t.Fatalf("acquisition %d uses a different file than the first; the fence file was replaced", attempt+1)
				}
				onlyCloseOnExec(t, path, 1, original)
				equal(t, []string{fenceName}, entryNames(t, state))
				noError(t, unfence(t, held))
				// The file stays after release; its presence says nothing about whether it is locked.
				if !sameFileAs(t, path, original) {
					t.Fatal("release removed or replaced the fence file")
				}
				onlyCloseOnExec(t, path, 0, original)
				equal(t, wantData, bytesAt(t, path))
				equal(t, []string{fenceName}, entryNames(t, state))
				if fenceHeld(t, path) {
					t.Fatal("the retained file is still locked after release")
				}
			}
		})
	}
}

func TestStartFenceRefusesNonRegular(t *testing.T) {
	type fixture struct {
		state, outside, path string
	}
	tests := map[string]struct {
		setup func(t *testing.T, f fixture)
		// target is the file a link points at; it must stay unlocked, unchanged or absent.
		target string
	}{
		"error: empty directory": {setup: func(t *testing.T, f fixture) { noError(t, os.Mkdir(f.path, 0o700)) }},
		"error: directory with an entry": {setup: func(t *testing.T, f fixture) {
			noError(t, os.Mkdir(f.path, 0o700))
			noError(t, os.WriteFile(filepath.Join(f.path, "entry"), []byte("entry"), 0o600))
		}},
		"error: symbolic link to a regular file": {target: "regular", setup: func(t *testing.T, f fixture) {
			noError(t, os.WriteFile(filepath.Join(f.outside, "regular"), []byte("target"), 0o600))
			// The mode is set explicitly, whatever the creation mask removed: the target is read back below.
			noError(t, os.Chmod(filepath.Join(f.outside, "regular"), 0o600))
			noError(t, os.Symlink(filepath.Join(f.outside, "regular"), f.path))
		}},
		"error: relative symbolic link to a sibling": {target: "sibling", setup: func(t *testing.T, f fixture) {
			noError(t, os.WriteFile(filepath.Join(f.state, "sibling"), []byte("target"), 0o600))
			// The mode is set explicitly, whatever the creation mask removed: the target is read back below.
			noError(t, os.Chmod(filepath.Join(f.state, "sibling"), 0o600))
			noError(t, os.Symlink("sibling", f.path))
		}},
		"error: dangling symbolic link": {target: "absent", setup: func(t *testing.T, f fixture) {
			noError(t, os.Symlink(filepath.Join(f.outside, "absent"), f.path))
		}},
		"error: symbolic link to a directory": {setup: func(t *testing.T, f fixture) {
			noError(t, os.Symlink(f.outside, f.path))
		}},
		"error: named pipe": {setup: func(t *testing.T, f fixture) { noError(t, unix.Mkfifo(f.path, 0o600)) }},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			state, outside := t.TempDir(), t.TempDir()
			f := fixture{state: state, outside: outside, path: filepath.Join(state, fenceName)}
			tt.setup(t, f)
			before, err := os.Lstat(f.path)
			noError(t, err)
			beforeLink, _ := os.Readlink(f.path)
			var targetPath, targetData string
			if tt.target != "" {
				targetPath = filepath.Join(outside, tt.target)
				if tt.target == "sibling" {
					targetPath = filepath.Join(state, tt.target)
				}
				if data, err := os.ReadFile(targetPath); err == nil {
					targetData = string(data)
				}
			}
			beforeNames := entryNames(t, state)

			// The watchdog only bounds a hang (for example a blocking open of the pipe); the result is the refusal.
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			results := acquireAsync(t, ctx, newFence(t, state))
			var got fenceResult
			select {
			case got = <-results:
			case <-time.After(30 * time.Second):
				t.Fatal("Acquire did not return for a non-regular fence path")
			}
			if got.release != nil {
				got.release = guardLease(t, got.release)
			}
			if t.Failed() {
				t.FailNow()
			}
			checkLease(t, got)
			if got.err == nil {
				_ = unfence(t, got.release)
				t.Fatalf("Acquire accepted a %v at the fence path", before.Mode().Type())
			}
			if errors.Is(got.err, context.DeadlineExceeded) || errors.Is(got.err, context.Canceled) {
				t.Fatalf("Acquire waited on a non-regular fence path until its context ended (%v); want an immediate filesystem error", got.err)
			}

			after, err := os.Lstat(f.path)
			noError(t, err)
			equal(t, before.Mode(), after.Mode())
			if !os.SameFile(before, after) {
				t.Fatal("Acquire replaced the entry at the fence path")
			}
			afterLink, _ := os.Readlink(f.path)
			equal(t, beforeLink, afterLink)
			equal(t, beforeNames, entryNames(t, state))
			onlyCloseOnExec(t, f.path, 0)
			if targetPath != "" {
				if targetData == "" {
					mustBeAbsent(t, targetPath)
				} else {
					equal(t, targetData, bytesAt(t, targetPath))
					if fenceHeld(t, targetPath) {
						t.Fatal("Acquire locked the target of a symbolic link")
					}
					onlyCloseOnExec(t, targetPath, 0)
				}
			}
		})
	}
}

func TestStartFenceIndependentOfLifecycleLock(t *testing.T) {
	tests := map[string]struct{ fenceFirst bool }{
		"success: lifecycle lock held first": {},
		"success: start fence held first":    {fenceFirst: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			fencePath, lockPath := filepath.Join(state, fenceName), filepath.Join(state, "lifecycle.lock")
			lock := lockFor(state)
			lock.Timeout = new(time.Duration(0)) // A lock the fence blocked would fail at once instead of waiting.
			fence := newFence(t, state)
			// The lease is acquired under t.Context(), which no deadline ends; the watchdog only aborts a fence
			// acquisition that waits on the lifecycle lock and never returns.
			takeFence := func() func() error {
				return leaseWithin(t, fence, "the start fence beside the lifecycle lock")
			}
			var releaseFence func() error
			var releaseLock func()
			if tt.fenceFirst {
				releaseFence = takeFence()
				releaseLock = acquire(t, lock)
			} else {
				releaseLock = acquire(t, lock)
				releaseFence = takeFence()
			}
			if !fenceHeld(t, fencePath) {
				t.Fatal("the start fence is not locked while both are held")
			}
			info, err := os.Lstat(lockPath)
			noError(t, err)
			if !info.IsDir() {
				t.Fatalf("lifecycle.lock has mode %v, want the lifecycle lock directory", info.Mode())
			}
			equal(t, []string{"lifecycle.lock", fenceName}, slices.Sorted(slices.Values(entryNames(t, state))))

			// Release in acquisition order, so that each is released while the other is still held.
			if tt.fenceFirst {
				noError(t, unfence(t, releaseFence))
				if _, err := os.Lstat(lockPath); err != nil {
					t.Fatalf("releasing the start fence affected the lifecycle lock: %v", err)
				}
				releaseLock()
			} else {
				releaseLock()
				if !fenceHeld(t, fencePath) {
					t.Fatal("releasing the lifecycle lock released the start fence")
				}
				noError(t, unfence(t, releaseFence))
			}
			mustBeAbsent(t, lockPath)
			if fenceHeld(t, fencePath) {
				t.Fatal("the start fence is still locked")
			}
			equal(t, []string{fenceName}, entryNames(t, state))
		})
	}
}
