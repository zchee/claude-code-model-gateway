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
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

func tryFence(t testing.TB, ctx context.Context, f *lifecycle.StartFence) fenceResult {
	t.Helper()
	r := noPanic(t, "StartFence.Acquire", func() fenceResult {
		release, err := f.Acquire(ctx)
		return fenceResult{release, err}
	})
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

// acquireAsync runs Acquire on its own goroutine. A panic is reported as a test error and as a result without a lease.
func acquireAsync(t testing.TB, ctx context.Context, f *lifecycle.StartFence) <-chan fenceResult {
	results := make(chan fenceResult, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("StartFence.Acquire panicked: %v", p)
				results <- fenceResult{err: errors.New("Acquire panicked")}
			}
		}()
		release, err := f.Acquire(ctx)
		results <- fenceResult{release, err}
	}()
	return results
}

func receive(t testing.TB, results <-chan fenceResult) fenceResult {
	t.Helper()
	r := <-results
	if t.Failed() {
		t.FailNow()
	}
	checkLease(t, r)
	return r
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

// onlyCloseOnExec fails unless exactly want descriptors refer to path and all of them are close-on-exec.
func onlyCloseOnExec(t testing.TB, path string, want int) {
	t.Helper()
	got := fenceDescriptors(t, path)
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
	equal(t, false, fenceHeld(t, path))
	equal(t, map[int]bool{}, fenceDescriptors(t, path))

	held, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	noError(t, err)
	noError(t, unix.Flock(held, unix.LOCK_EX|unix.LOCK_NB))
	equal(t, true, fenceHeld(t, path))
	equal(t, map[int]bool{held: true}, fenceDescriptors(t, path))

	inherited, err := unix.Open(path, unix.O_RDONLY, 0)
	noError(t, err)
	equal(t, map[int]bool{held: true, inherited: false}, fenceDescriptors(t, path))
	noError(t, unix.Close(inherited))

	noError(t, unix.Flock(held, unix.LOCK_UN))
	equal(t, false, fenceHeld(t, path))
	noError(t, unix.Close(held))
	equal(t, map[int]bool{}, fenceDescriptors(t, path))
	equal(t, false, fenceHeld(t, filepath.Join(filepath.Dir(path), "absent")))
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
			}
			fence := newFence(t, state)
			var original fs.FileInfo
			for attempt := range 3 {
				held := holdFence(t, fence)
				info, err := os.Lstat(path)
				noError(t, err)
				if !info.Mode().IsRegular() {
					t.Fatalf("fence path has mode %v, want a regular file", info.Mode())
				}
				if attempt == 0 {
					original = info
					if tt.existing == nil {
						// The requested creation mode under the venue's umask of 022.
						equal(t, fs.FileMode(0o600), info.Mode().Perm())
					}
				} else if !os.SameFile(original, info) {
					t.Fatalf("acquisition %d uses a different file than the first; the fence file was replaced", attempt+1)
				}
				equal(t, []string{fenceName}, entryNames(t, state))
				noError(t, unfence(t, held))
				// The file stays after release; its presence says nothing about whether it is locked.
				info, err = os.Lstat(path)
				noError(t, err)
				if !os.SameFile(original, info) {
					t.Fatal("release removed or replaced the fence file")
				}
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
			noError(t, os.Symlink(filepath.Join(f.outside, "regular"), f.path))
		}},
		"error: relative symbolic link to a sibling": {target: "sibling", setup: func(t *testing.T, f fixture) {
			noError(t, os.WriteFile(filepath.Join(f.state, "sibling"), []byte("target"), 0o600))
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
			// The watchdog only bounds a fence that waits on the lifecycle lock.
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			takeFence := func() func() error {
				r := tryFence(t, ctx, fence)
				if r.err != nil {
					t.Fatalf("Acquire of the start fence: %v", r.err)
				}
				return r.release
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
