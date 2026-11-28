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

package ownership

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
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
)

// The outcome forms keep these exact signatures; Publish keeps its one result.
var (
	_ func(Store, string, procinfo.Record) error                                    = Store.Publish
	_ func(Store, context.Context, string, procinfo.Record) (bool, error)           = Store.PublishContext
	_ func(Store, context.Context, string, procinfo.Record) (bool, error)           = Store.RemoveIfContext
	_ func(Store, context.Context, string) (procinfo.Record, error)                 = Store.ReadContext
	_ func(Store, context.Context, string, procinfo.Record) (wrote bool, err error) = Store.Reconcile
)

const (
	// storeOutTName is the record most rows work on; storeOutTOther is another valid record name, used with
	// storeOutTName stored beside it as a decoy; storeOutTTarget is the record a symbolic link points at.
	storeOutTName   = "worker"
	storeOutTOther  = "proxy"
	storeOutTTarget = "target"
	// storeOutTLimit is how long a call that should finish may take before the row gives up on it.
	storeOutTLimit = 30 * time.Second
	// storeOutTGrace is how long a call may take to return once its context has ended.
	storeOutTGrace = 5 * time.Second
	// storeOutTDeadline is the deadline of a call whose lock another holder keeps, on the bubble's clock.
	storeOutTDeadline = 200 * time.Millisecond
)

// storeOutTReleaseError is the error the fault hook adds at a release; every value is equal, so errors.Is finds it.
type storeOutTReleaseError struct{}

func (storeOutTReleaseError) Error() string {
	return "an injected failure of the record lock's release"
}

// storeOutTMirrorError is the error the fault hook gives a write of a decimal mirror.
type storeOutTMirrorError struct{}

func (storeOutTMirrorError) Error() string {
	return "an injected failure of the decimal mirror's write"
}

// storeOutTStepName names a step of the store in a failure message.
func storeOutTStepName(step storeStep) string {
	switch step {
	case storeLookup:
		return "lookup"
	case storeWrite:
		return "write"
	case storeRelease:
		return "release"
	default:
		return "step " + strconv.Itoa(int(step))
	}
}

// storeOutTAt is a step of the store at one entry of the state root.
type storeOutTAt struct {
	step storeStep
	name string
}

func (a storeOutTAt) String() string { return storeOutTStepName(a.step) + " " + a.name }

// storeOutTFault is an error the fault hook returns at a step.
type storeOutTFault struct {
	at  storeOutTAt
	err error
}

// storeOutTFixture is one row's state root, the holds the row took on its lock, and what the row observed of the
// store's steps through the fault hook.
type storeOutTFixture struct {
	t *testing.T
	// parent is a test-owned directory; root, its subdirectory "state", is the state root.
	parent, root string
	// ctx and cancel are the context of the row's call, made inside the row's synctest bubble.
	ctx    context.Context
	cancel context.CancelFunc
	// lockSeed is the record lock file as the fixture created it, when it did.
	lockSeed fs.FileInfo
	// faults are returned at their steps; cancelAt, when set, ends the call's context at that step; observe, when set,
	// sees every step first.
	faults   []storeOutTFault
	cancelAt *storeOutTAt
	observe  func(at storeOutTAt)
	// cancelRan reports that the row's cancellation inside the operation really ran.
	cancelRan atomic.Bool

	mu    sync.Mutex
	steps []storeOutTAt
	holds []func()
}

// storeOutTNewFixture returns a fixture whose state root is an empty, clean, absolute directory with symbolic links
// resolved, inside a test-owned parent.
func storeOutTNewFixture(t *testing.T) *storeOutTFixture {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return &storeOutTFixture{t: t, parent: parent, root: root}
}

// hook is the store's fault hook: it records the step, then lets the row observe it, cancel at it or fail it.
func (fx *storeOutTFixture) hook(step storeStep, name string) error {
	at := storeOutTAt{step, name}
	fx.mu.Lock()
	fx.steps = append(fx.steps, at)
	fx.mu.Unlock()
	if fx.observe != nil {
		fx.observe(at)
	}
	if fx.cancelAt != nil && *fx.cancelAt == at {
		fx.cancelNow()
	}
	for _, f := range fx.faults {
		if f.at == at {
			return f.err
		}
	}
	return nil
}

// cancelNow ends the call's context from inside the operation and records that it ran.
func (fx *storeOutTFixture) cancelNow() {
	fx.cancelRan.Store(true)
	fx.cancel()
}

// injected returns the errors the row's fault hook returns, which the call's error must hold where the row says so.
func (fx *storeOutTFixture) injected() []error {
	errs := make([]error, 0, len(fx.faults))
	for _, f := range fx.faults {
		errs = append(errs, f.err)
	}
	return errs
}

// store returns a store of root that reports its steps to the fixture.
func (fx *storeOutTFixture) store(root string) Store { return Store{Root: root, fault: fx.hook} }

// seen returns the steps the store reported so far.
func (fx *storeOutTFixture) seen() []storeOutTAt {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	return slices.Clone(fx.steps)
}

// seedLock creates the retained record lock file of the state root, as an earlier operation would have, and keeps
// its identity so that the row can require the same file afterwards.
func (fx *storeOutTFixture) seedLock() {
	fx.t.Helper()
	path := filepath.Join(fx.root, recordLockName)
	if _, err := os.Lstat(path); err == nil {
		return
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fx.t.Fatal(err)
	}
	info, err := f.Stat()
	if err := errors.Join(err, f.Close()); err != nil {
		fx.t.Fatal(err)
	}
	fx.lockSeed = info
}

// hold takes the record lock of the state root on a descriptor of its own, as another process holding it would, and
// returns the function that gives it up; giving up every hold and cleanup give it up as well.
func (fx *storeOutTFixture) hold() (release func()) {
	t := fx.t
	t.Helper()
	fx.seedLock()
	path := filepath.Join(fx.root, recordLockName)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("take the record lock of %s: %v", fx.root, err)
	}
	release = sync.OnceFunc(func() {
		if err := errors.Join(unix.Flock(int(f.Fd()), unix.LOCK_UN), f.Close()); err != nil {
			t.Errorf("give up the record lock of %s: %v", fx.root, err)
		}
	})
	t.Cleanup(release)
	fx.mu.Lock()
	fx.holds = append(fx.holds, release)
	fx.mu.Unlock()
	return release
}

// releaseHolds gives up every hold the row took.
func (fx *storeOutTFixture) releaseHolds() {
	fx.mu.Lock()
	holds := slices.Clone(fx.holds)
	fx.mu.Unlock()
	for _, release := range holds {
		release()
	}
}

// storeOutTRecord returns the valid record of root that the rows publish, reconcile, read or remove.
func storeOutTRecord(root string) procinfo.Record {
	return procinfo.Record{
		PID:        43101,
		Command:    "/opt/model-gateway/bin/model-gateway serve-worker",
		StartedAt:  procinfo.LegacyStart(time.Date(2026, time.November, 28, 9, 30, 0, 0, time.Local)),
		Role:       "worker",
		StateRoot:  root,
		Generation: "generation-7",
		Port:       43117,
		Version:    "1.2.3",
		Image:      procinfo.Image{Path: "/opt/model-gateway/bin/model-gateway", Device: 64769, Inode: 4242},
		StartToken: "6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b/43101",
	}
}

// storeOutTPrior returns a valid record of root of an earlier generation and another process.
func storeOutTPrior(root string) procinfo.Record {
	r := storeOutTRecord(root)
	r.PID = 43099
	r.Generation = "generation-6"
	r.StartToken = "6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b/43099"
	return r
}

// storeOutTDecoy returns the valid record of root that a row stores under storeOutTName beside the record it works on.
func storeOutTDecoy(root string) procinfo.Record {
	r := storeOutTRecord(root)
	r.PID = 43055
	r.Generation = "generation-2"
	r.StartToken = "6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b/43055"
	return r
}

// storeOutTSeed writes r as the record name of root with the existing record writer, independently of the store.
func storeOutTSeed(t *testing.T, root, name string, r procinfo.Record) {
	t.Helper()
	if err := procinfo.WriteRecord(root, name, r); err != nil {
		t.Fatalf("seed record %s: %v", name, err)
	}
}

// storeOutTWrite writes data as the file of root, replacing whatever regular file was there.
func storeOutTWrite(t *testing.T, root, file, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, file), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeOutTEncode returns r's JSON encoding, which may name another state root than the one it is written to.
func storeOutTEncode(t *testing.T, r procinfo.Record) string {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// storeOutTMirrorDir replaces the decimal mirror of the record name with a directory.
func storeOutTMirrorDir(t *testing.T, root, name string) {
	t.Helper()
	mirror := filepath.Join(root, name+".pid")
	if err := os.Remove(mirror); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
}

// storeOutTLinkRecord makes the record file of storeOutTName a symbolic link to the record file of storeOutTTarget.
func storeOutTLinkRecord(t *testing.T, root string) {
	t.Helper()
	if err := os.Symlink(storeOutTTarget+".pid.json", filepath.Join(root, storeOutTName+".pid.json")); err != nil {
		t.Fatal(err)
	}
}

// storeOutTDenied is the error the operating system gives a process that a file's mode refuses.
func storeOutTDenied(root, file string) error {
	return &fs.PathError{Op: "open", Path: filepath.Join(root, file), Err: unix.EACCES}
}

// storeOutTEIO is an ordinary input and output failure of the file.
func storeOutTEIO(root, file string) error {
	return &fs.PathError{Op: "read", Path: filepath.Join(root, file), Err: unix.EIO}
}

// storeOutTRefuses sets the permission bits of path to mode until restore runs (at the latest in cleanup) and reports
// whether that mode really refuses this process the open that try attempts. try returns, when the open succeeded, the
// function that undoes it, and the open's error. Only an open that succeeds, as it does for the superuser, makes it
// report false: the row then injects the same refusal through the store's fault hook. Any other failure of the open,
// and any failure to undo it, fails the row.
func storeOutTRefuses(t *testing.T, path string, mode fs.FileMode, try func() (undo func() error, open error)) (refused bool, restore func()) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	restore = sync.OnceFunc(func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Errorf("restore the mode of %s: %v", path, err)
		}
	})
	t.Cleanup(restore)
	undo, open := try()
	switch {
	case open == nil:
		if err := undo(); err != nil {
			restore()
			t.Fatalf("undo the probe of mode %v on %s: %v", mode, path, err)
		}
		t.Logf("uid %d: mode %v on %s does not refuse this process; the row injects the same refusal", os.Geteuid(), mode, path)
		return false, restore
	case errors.Is(open, fs.ErrPermission):
		t.Logf("uid %d: mode %v on %s refuses this process (%v); the operating system decides the row", os.Geteuid(), mode, path, open)
		return true, restore
	default:
		restore()
		t.Fatalf("probe the access that mode %v on %s refuses: %v", mode, path, open)
		return false, restore
	}
}

// storeOutTDenyRoot makes root read-only after creating the retained record lock file, so that an operation gets its
// lock and fails at its write. It reports whether the mode refuses this process the creation of a file in root.
func storeOutTDenyRoot(fx *storeOutTFixture) (refused bool, restore func()) {
	fx.t.Helper()
	fx.seedLock()
	probe := filepath.Join(fx.root, "storeOutT-probe")
	return storeOutTRefuses(fx.t, fx.root, 0o500, func() (func() error, error) {
		f, err := os.OpenFile(probe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		return func() error { return errors.Join(f.Close(), os.Remove(probe)) }, nil
	})
}

// storeOutTDenyRecord makes the record file of name in root unreadable. It reports whether the mode refuses this
// process the open for reading.
func storeOutTDenyRecord(t *testing.T, root, name string) (refused bool, restore func()) {
	t.Helper()
	path := filepath.Join(root, name+".pid.json")
	return storeOutTRefuses(t, path, 0, func() (func() error, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return f.Close, nil
	})
}

// storeOutTLockHeld reports whether some holder has the record lock of root, by trying it once without waiting on a
// descriptor of its own.
func storeOutTLockHeld(t *testing.T, root string) bool {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(root, recordLockName), os.O_RDWR, 0)
	if err != nil {
		t.Errorf("open the record lock of %s: %v", root, err)
		return false
	}
	defer func() { _ = f.Close() }()
	switch err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); {
	case err == nil:
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return false
	case errors.Is(err, unix.EWOULDBLOCK):
		return true
	default:
		t.Errorf("try the record lock of %s: %v", root, err)
		return false
	}
}

// storeOutTLockState is what a row requires of the retained record lock file after the call.
type storeOutTLockState int

const (
	// storeOutTNoLock: the call was refused before the lock, so no lock file exists in the state root or its parent.
	storeOutTNoLock storeOutTLockState = iota
	// storeOutTAcquired: the call took the lock, so the lock file exists, is a regular file, is the file the fixture
	// created when it created one, and a fresh descriptor can take it.
	storeOutTAcquired
	// storeOutTNotAcquired: the call's wait ended. Where the fixture created no lock file, the call created none
	// either; where it did, that file is as after an acquisition.
	storeOutTNotAcquired
)

// storeOutTCheckLock fails the row unless the record lock file of the fixture is as state says.
func storeOutTCheckLock(fx *storeOutTFixture, what string, state storeOutTLockState) {
	t := fx.t
	t.Helper()
	path := filepath.Join(fx.root, recordLockName)
	info, err := os.Lstat(path)
	if state == storeOutTNoLock {
		for _, p := range []string{path, filepath.Join(fx.parent, recordLockName)} {
			if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s: the record lock file %s exists after a refusal before the lock (lstat: %v)", what, p, err)
			}
		}
		return
	}
	if state == storeOutTNotAcquired && fx.lockSeed == nil {
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: the call created the record lock file although its context had ended before the call (lstat: %v)", what, err)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: the retained record lock file: %v", what, err)
		return
	}
	if !info.Mode().IsRegular() {
		t.Errorf("%s: the retained record lock is %v; want a regular file", what, info.Mode().Type())
		return
	}
	if fx.lockSeed != nil && !os.SameFile(fx.lockSeed, info) {
		t.Errorf("%s: the retained record lock file was replaced by another file", what)
	}
	if storeOutTLockHeld(t, fx.root) {
		t.Errorf("%s: the record lock is still held after the call returned", what)
	}
}

// storeOutTWait is how a row's call meets the record lock.
type storeOutTWait int

const (
	// storeOutTFree runs the call with a live context and nobody else holding the lock.
	storeOutTFree storeOutTWait = iota
	// storeOutTCanceled runs the call with a context canceled before the call.
	storeOutTCanceled
	// storeOutTExpired runs the call with a context whose deadline passed before the call.
	storeOutTExpired
	// storeOutTHeldCanceled holds the lock, waits until the call waits for it, and then cancels the call's context.
	storeOutTHeldCanceled
	// storeOutTHeldDeadline holds the lock, waits until the call waits for it, and then lets the deadline of the
	// call's context pass.
	storeOutTHeldDeadline
	// storeOutTHeldReleased holds the lock, waits until the call waits for it, and then gives the lock up.
	storeOutTHeldReleased
)

// storeOutTOutcome is what one call of the unit gave.
type storeOutTOutcome[T any] struct {
	value    T
	panicked any
}

// storeOutTRun calls call on its own goroutine inside a synctest bubble, with a real context and a lock as wait
// describes, and returns its result. ok is false, and the row failed, when call panicked or did not return in time.
//
// A row whose lock another holder keeps waits, with synctest.Wait, until the call is durably blocked, checks that it
// has not returned, and only then cancels its context, lets its deadline pass on the bubble's clock, or gives the lock
// up. This relies on the store taking its lock through locked and lockRecords, whose retry pauses on a timer between
// two non-blocking attempts: that pause, not a file operation, is where the call blocks while another holder keeps
// the lock. A store that blocked anywhere else would not be told apart by this fixture.
//
// A call that does not return in time has its context ended and every hold given up, and storeOutTRun waits for it
// before it returns, so no call outlives its row. The timers are the bubble's, so they only run out while every
// goroutine of the bubble is blocked; they protect against a hang and are never evidence about the store, and a
// result that is ready when one runs out is used.
func storeOutTRun[T any](fx *storeOutTFixture, what string, wait storeOutTWait, call func(ctx context.Context) T) (value T, ok bool) {
	outer := fx.t
	outer.Helper()
	synctest.Test(outer, func(t *testing.T) {
		fx.t = t
		defer func() { fx.t = outer }()
		value, ok = storeOutTRunInBubble(fx, what, wait, call)
	})
	return value, ok
}

// storeOutTRunInBubble is storeOutTRun inside the row's bubble.
func storeOutTRunInBubble[T any](fx *storeOutTFixture, what string, wait storeOutTWait, call func(ctx context.Context) T) (value T, ok bool) {
	t := fx.t
	held := wait == storeOutTHeldCanceled || wait == storeOutTHeldDeadline || wait == storeOutTHeldReleased
	var release func()
	if held {
		release = fx.hold()
	}
	switch wait {
	case storeOutTHeldDeadline:
		fx.ctx, fx.cancel = context.WithTimeout(t.Context(), storeOutTDeadline)
	case storeOutTExpired:
		fx.ctx, fx.cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	default:
		fx.ctx, fx.cancel = context.WithCancel(t.Context())
	}
	defer fx.cancel()
	if wait == storeOutTCanceled {
		fx.cancel()
	}
	done := make(chan storeOutTOutcome[T], 1)
	go func() {
		var o storeOutTOutcome[T]
		defer func() {
			o.panicked = recover()
			done <- o
		}()
		o.value = call(fx.ctx)
	}()
	watchdog := time.NewTimer(storeOutTLimit)
	defer watchdog.Stop()
	abandon := func(reason string) (T, bool) {
		t.Errorf("%s %s; its context is ended, every hold given up, and the row waits for it to return", what, reason)
		fx.cancel()
		fx.releaseHolds()
		<-done
		var zero T
		return zero, false
	}
	var o storeOutTOutcome[T]
	returned := false
	if held {
		synctest.Wait()
		select {
		case o = <-done:
			returned = true
			if o.panicked == nil {
				t.Errorf("%s returned while another holder kept the record lock, without waiting for it", what)
			}
		default:
			switch wait {
			case storeOutTHeldCanceled:
				fx.cancel()
			case storeOutTHeldDeadline:
				// Sleeping lets the bubble's clock pass the deadline while the call waits for the lock.
				time.Sleep(storeOutTDeadline)
			default:
				release()
			}
		}
	}
	if !returned {
		var grace <-chan time.Time
		if fx.ctx.Err() != nil {
			timer := time.NewTimer(storeOutTGrace)
			defer timer.Stop()
			grace = timer.C
		}
		select {
		case o = <-done:
		case <-grace:
			select {
			case o = <-done:
			default:
				return abandon(fmt.Sprintf("did not return within %v of its context's end", storeOutTGrace))
			}
		case <-watchdog.C:
			select {
			case o = <-done:
			default:
				return abandon(fmt.Sprintf("did not return within %v", storeOutTLimit))
			}
		}
	}
	fx.releaseHolds()
	if o.panicked != nil {
		t.Errorf("%s panicked: %v", what, o.panicked)
		return o.value, false
	}
	return o.value, true
}

// storeOutTCheckSteps fails the row unless the store's reported steps fit how the call met the lock: a refusal, a
// context ended before the call and a wait that ended report none, and a cancellation the row placed inside the
// operation really ran.
func storeOutTCheckSteps(fx *storeOutTFixture, what string, wait storeOutTWait, refusal, cancels bool) {
	t := fx.t
	t.Helper()
	steps := fx.seen()
	switch {
	case refusal || wait == storeOutTCanceled || wait == storeOutTExpired:
		if len(steps) != 0 {
			t.Errorf("%s made the steps %v; want none, before the lock and without reading or writing anything", what, steps)
		}
	case wait == storeOutTHeldCanceled || wait == storeOutTHeldDeadline:
		if len(steps) != 0 {
			t.Errorf("%s made the steps %v while another holder kept the lock; want no lookup, write or release", what, steps)
		}
	}
	if cancels && !fx.cancelRan.Load() {
		t.Errorf("%s never reached the step at which the row ends its context: steps %v", what, steps)
	}
}

// storeOutTCheckErr fails the row unless err matches every error of want and none of not and, when check is set,
// is an error that check accepts; an empty want without check asks for nil.
func storeOutTCheckErr(fx *storeOutTFixture, what string, err error, want, not []error, check func(fx *storeOutTFixture, err error) string) {
	t := fx.t
	t.Helper()
	switch {
	case check != nil && err == nil:
		t.Errorf("%s error = nil; want an error", what)
	case check != nil:
		if msg := check(fx, err); msg != "" {
			t.Errorf("%s error = %v; %s", what, err, msg)
		}
	case len(want) == 0 && err != nil:
		t.Errorf("%s error = %v; want nil", what, err)
	}
	for _, w := range want {
		if !errors.Is(err, w) {
			t.Errorf("%s error = %v; want an error matching %v", what, err, w)
		}
	}
	for _, n := range not {
		if errors.Is(err, n) {
			t.Errorf("%s error = %v; it must not match %v", what, err, n)
		}
	}
}

// storeOutTWant returns the errors a row's error must hold: want and, when injected is set, every error the row's
// fault hook returns, as the same objects, so that an error that only resembles one, such as its bare errno, is not
// taken for it.
func storeOutTWant(fx *storeOutTFixture, want []error, injected bool) []error {
	if !injected {
		return want
	}
	return append(slices.Clone(want), fx.injected()...)
}

// storeOutTSyntax is an error check: err holds a JSON syntax error.
func storeOutTSyntax(_ *storeOutTFixture, err error) string {
	if _, ok := errors.AsType[*jsontext.SyntacticError](err); !ok {
		return "want an error holding a JSON syntax error"
	}
	return ""
}

// storeOutTPathAt returns an error check: err holds a path error of the file of the state root.
func storeOutTPathAt(file string) func(fx *storeOutTFixture, err error) string {
	return func(fx *storeOutTFixture, err error) string {
		pe, ok := errors.AsType[*fs.PathError](err)
		if !ok {
			return "want an error holding a path error of " + file
		}
		if want := filepath.Join(fx.root, file); pe.Path != want {
			return fmt.Sprintf("its path error names %s; want %s", pe.Path, want)
		}
		return ""
	}
}

// storeOutTEntry is one directory entry as it is on disk.
type storeOutTEntry struct {
	present bool
	kind    fs.FileMode
	// data is a regular file's content or a symbolic link's target.
	data string
	info fs.FileInfo
	// file is an open descriptor of a regular file captured before the call, to read what that file holds after it.
	file *os.File
}

// storeOutTState is every entry of the state root and of its parent, by its path relative to the parent, except the
// record lock file.
type storeOutTState map[string]storeOutTEntry

func storeOutTEntryOf(t *testing.T, path string, open bool) storeOutTEntry {
	t.Helper()
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return storeOutTEntry{}
	}
	if err != nil {
		t.Fatal(err)
	}
	e := storeOutTEntry{present: true, kind: info.Mode().Type(), info: info}
	switch {
	case info.Mode().IsRegular():
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		e.data = string(data)
		if open {
			if e.file, err = os.Open(path); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.file.Close() })
		}
	case info.Mode()&fs.ModeSymlink != 0:
		if e.data, err = os.Readlink(path); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// storeOutTCapture returns the entries of the fixture; with open, it keeps a descriptor of every regular file.
func storeOutTCapture(fx *storeOutTFixture, open bool) storeOutTState {
	t := fx.t
	t.Helper()
	state := storeOutTState{}
	for _, dir := range []string{fx.parent, fx.root} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if dir == fx.root && entry.Name() == recordLockName {
				continue
			}
			rel, err := filepath.Rel(fx.parent, filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			state[rel] = storeOutTEntryOf(t, filepath.Join(dir, entry.Name()), open)
		}
	}
	return state
}

// storeOutTSame fails the row unless the entry is the same file with the same content: a file replaced by another,
// even with equal bytes, is not the same.
func storeOutTSame(t *testing.T, what string, before, after storeOutTEntry) {
	t.Helper()
	if before.present != after.present || before.kind != after.kind || before.data != after.data {
		t.Errorf("%s changed: before present %v kind %v content %q; after present %v kind %v content %q", what, before.present, before.kind, before.data, after.present, after.kind, after.data)
		return
	}
	if before.present && !os.SameFile(before.info, after.info) {
		t.Errorf("%s was replaced by another file with the same content %q", what, after.data)
	}
}

// storeOutTOldBytes returns what the file captured before the call holds now, read through its old descriptor.
func storeOutTOldBytes(t *testing.T, e storeOutTEntry) string {
	t.Helper()
	data, err := io.ReadAll(io.NewSectionReader(e.file, 0, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// storeOutTReplaced fails the row unless the record file now holds r and, when a file was there before, is another
// file renamed over it while the old file kept its bytes: an in-place write does not commit atomically.
func storeOutTReplaced(t *testing.T, what string, before, now storeOutTEntry, r procinfo.Record) {
	t.Helper()
	if !now.present || now.kind != 0 {
		t.Errorf("%s present %v kind %v; want a regular file holding the record", what, now.present, now.kind)
		return
	}
	var got procinfo.Record
	if err := json.Unmarshal([]byte(now.data), &got); err != nil {
		t.Errorf("%s %q: %v", what, now.data, err)
		return
	}
	if diff := gocmp.Diff(r, got); diff != "" {
		t.Errorf("%s (-want +got):\n%s", what, diff)
	}
	if !before.present {
		return
	}
	if os.SameFile(before.info, now.info) {
		t.Errorf("%s was rewritten in place; want a new file renamed over the old one", what)
		return
	}
	if before.file != nil {
		if old := storeOutTOldBytes(t, before); old != before.data {
			t.Errorf("%s: the replaced file was changed in place before the replacement: it holds %q, it held %q", what, old, before.data)
		}
	}
}

// storeOutTMirrors fails the row unless the mirror holds r's decimal process number, and the file that was there
// before is either untouched because it already held it, or replaced by another file while it kept its bytes.
func storeOutTMirrors(t *testing.T, what string, before, now storeOutTEntry, r procinfo.Record) {
	t.Helper()
	want := strconv.Itoa(r.PID)
	if !now.present || now.kind != 0 {
		t.Errorf("%s present %v kind %v; want a regular file", what, now.present, now.kind)
		return
	}
	if diff := gocmp.Diff(want, now.data); diff != "" {
		t.Errorf("%s (-want +got):\n%s", what, diff)
	}
	if !before.present || before.kind != 0 {
		return
	}
	if os.SameFile(before.info, now.info) {
		if before.data != want {
			t.Errorf("%s was rewritten in place from %q; want a new file renamed over the old one", what, before.data)
		}
		return
	}
	if before.file != nil {
		if old := storeOutTOldBytes(t, before); old != before.data {
			t.Errorf("%s: the replaced file was changed in place before the replacement: it holds %q, it held %q", what, old, before.data)
		}
	}
}

// storeOutTAfter is what a row's record files must be after the call.
type storeOutTAfter int

const (
	// storeOutTKept: the record and its mirror are exactly the files they were, or still absent.
	storeOutTKept storeOutTAfter = iota
	// storeOutTWritten: the record holds the row's record and the mirror its process number.
	storeOutTWritten
	// storeOutTRecordOnly: the record holds the row's record; the mirror entry is exactly what it was.
	storeOutTRecordOnly
	// storeOutTMirrorFixed: the record is exactly the file it was; the mirror holds its process number.
	storeOutTMirrorFixed
	// storeOutTRemoved: neither the record nor its mirror exists.
	storeOutTRemoved
	// storeOutTRecordGone: the record does not exist; the mirror entry is exactly what it was.
	storeOutTRecordGone
	// storeOutTCustom: the row checks the record and its mirror itself.
	storeOutTCustom
)

// storeOutTCheckAfter fails the row unless the record files of name are as after says, r being the row's record, and
// every other entry of the state root and its parent is exactly what it was.
func storeOutTCheckAfter(t *testing.T, after storeOutTAfter, before, now storeOutTState, name string, r procinfo.Record) {
	t.Helper()
	record, mirror := filepath.Join("state", name+".pid.json"), filepath.Join("state", name+".pid")
	keys := slices.Sorted(maps.Keys(before))
	for k := range now {
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		if k != record && k != mirror {
			storeOutTSame(t, k, before[k], now[k])
		}
	}
	switch after {
	case storeOutTKept:
		storeOutTSame(t, record, before[record], now[record])
		storeOutTSame(t, mirror, before[mirror], now[mirror])
	case storeOutTWritten:
		storeOutTReplaced(t, record, before[record], now[record], r)
		storeOutTMirrors(t, mirror, before[mirror], now[mirror], r)
	case storeOutTRecordOnly:
		storeOutTReplaced(t, record, before[record], now[record], r)
		storeOutTSame(t, mirror, before[mirror], now[mirror])
	case storeOutTMirrorFixed:
		storeOutTSame(t, record, before[record], now[record])
		storeOutTMirrors(t, mirror, before[mirror], now[mirror], r)
	case storeOutTRemoved:
		if now[record].present || now[mirror].present {
			t.Errorf("record present %v, mirror present %v; want neither", now[record].present, now[mirror].present)
		}
	case storeOutTRecordGone:
		if now[record].present {
			t.Errorf("the record is still present: %q", now[record].data)
		}
		storeOutTSame(t, mirror, before[mirror], now[mirror])
	}
}

// storeOutTBool is a call's boolean result and error.
type storeOutTBool struct {
	ok  bool
	err error
}

// storeOutTPublishCase is one row of TestStorePublishOutcome and of TestStoreReconcile.
type storeOutTPublishCase struct {
	// setup prepares the state root before the call.
	setup func(fx *storeOutTFixture)
	// record changes the row's valid record before the call.
	record func(fx *storeOutTFixture, r *procinfo.Record)
	// storeRoot, when set, gives the store's root instead of the fixture's state root.
	storeRoot func(t *testing.T, fx *storeOutTFixture) string
	// name, when set, is the record name instead of storeOutTName.
	name *string
	wait storeOutTWait
	// denyRoot makes the state root read-only after the record lock file was created; denyRecord makes the record file
	// unreadable. Where the mode does not refuse this process, the fault hook gives the same refusal instead.
	denyRoot   bool
	denyRecord bool
	// faults are the errors the fault hook returns; cancelAt ends the call's context at that step.
	faults   func(fx *storeOutTFixture, name string) []storeOutTFault
	cancelAt func(name string) storeOutTAt
	// legacy calls Publish, whose one result has no publication flag, instead of PublishContext.
	legacy bool
	// refusal says the call is refused before the lock.
	refusal bool
	// wantOK is published or wrote.
	wantOK  bool
	wantErr []error
	notErr  []error
	// wantInjected asks for an error that holds every error the row's fault hook returns, as the same objects.
	wantInjected bool
	// check, when set, is a further check of the error; it returns what is wrong, or "".
	check func(fx *storeOutTFixture, err error) string
	after storeOutTAfter
}

// storeOutTRefusals are the refusals before the lock that PublishContext and Reconcile share with Publish.
func storeOutTRefusals() map[string]storeOutTPublishCase {
	seedPrior := func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTPrior(fx.root)) }
	return map[string]storeOutTPublishCase{
		"error: a record name that leaves the state root is refused before the lock and the record it names is kept": {
			setup: func(fx *storeOutTFixture) {
				seedPrior(fx)
				storeOutTSeed(fx.t, fx.parent, storeOutTName, storeOutTPrior(fx.parent))
			},
			name: new("../" + storeOutTName), refusal: true, wantErr: []error{fs.ErrInvalid},
		},
		"error: an empty record name is refused before the lock": {
			setup: seedPrior, name: new(""), refusal: true, wantErr: []error{fs.ErrInvalid},
		},
		"error: a relative spelling of the state root is refused before the lock": {
			setup: seedPrior,
			storeRoot: func(t *testing.T, fx *storeOutTFixture) string {
				wd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				rel, err := filepath.Rel(wd, fx.root)
				if err != nil {
					t.Fatal(err)
				}
				return rel
			},
			refusal: true, wantErr: []error{procinfo.ErrStateRoot},
		},
		"error: an unclean spelling of the state root is refused before the lock": {
			setup:     seedPrior,
			storeRoot: func(_ *testing.T, fx *storeOutTFixture) string { return fx.root + "/../state" },
			refusal:   true, wantErr: []error{procinfo.ErrStateRoot},
		},
		"error: a record without a start token is refused before the lock and the prior record is kept": {
			setup:   seedPrior,
			record:  func(_ *storeOutTFixture, r *procinfo.Record) { r.StartToken = "" },
			refusal: true, wantErr: []error{errNoStartToken},
		},
		"error: a record of another state root is refused before the lock": {
			setup:   seedPrior,
			record:  func(fx *storeOutTFixture, r *procinfo.Record) { r.StateRoot = fx.parent },
			refusal: true, wantErr: []error{fs.ErrInvalid},
		},
		"error: a record with a start time that names no whole second is refused before the lock": {
			setup:   seedPrior,
			record:  func(_ *storeOutTFixture, r *procinfo.Record) { r.StartedAt = "Sat Nov 28 09:30:00.5 2026" },
			refusal: true, wantErr: []error{procinfo.ErrStartedAt},
		},
		"error: a record that cannot be encoded is refused before the lock": {
			setup: seedPrior,
			record: func(_ *storeOutTFixture, r *procinfo.Record) {
				r.Command = "/opt/model-gateway/bin/model-gateway serve-worker \xff"
			},
			refusal: true, check: storeOutTSyntax,
		},
	}
}

// storeOutTRunPublishCase runs one row through call, which is PublishContext or Reconcile on the store it is given.
func storeOutTRunPublishCase(t *testing.T, what string, tt storeOutTPublishCase, call func(s Store, ctx context.Context, name string, r procinfo.Record) (bool, error)) {
	t.Helper()
	fx := storeOutTNewFixture(t)
	if tt.setup != nil {
		tt.setup(fx)
	}
	r := storeOutTRecord(fx.root)
	if tt.record != nil {
		tt.record(fx, &r)
	}
	name := storeOutTName
	if tt.name != nil {
		name = *tt.name
	}
	storeRoot := fx.root
	if tt.storeRoot != nil {
		storeRoot = tt.storeRoot(t, fx)
	}
	file := name + ".pid.json"
	if tt.faults != nil {
		fx.faults = tt.faults(fx, name)
	}
	if tt.cancelAt != nil {
		at := tt.cancelAt(name)
		fx.cancelAt = &at
	}
	before := storeOutTCapture(fx, true)
	restore := func() {}
	if tt.denyRoot {
		var refused bool
		refused, restore = storeOutTDenyRoot(fx)
		if !refused {
			fx.faults = append(fx.faults, storeOutTFault{storeOutTAt{storeWrite, file}, storeOutTDenied(fx.root, file)})
		}
	}
	if tt.denyRecord {
		var refused bool
		refused, restore = storeOutTDenyRecord(t, fx.root, name)
		if !refused {
			fx.faults = append(fx.faults, storeOutTFault{storeOutTAt{storeLookup, file}, storeOutTDenied(fx.root, file)})
		}
	}
	got, ok := storeOutTRun(fx, what, tt.wait, func(ctx context.Context) storeOutTBool {
		published, err := call(fx.store(storeRoot), ctx, name, r)
		return storeOutTBool{published, err}
	})
	restore()
	if !ok {
		return
	}
	if !tt.legacy && got.ok != tt.wantOK {
		t.Errorf("%s = %v, %v; want %v", what, got.ok, got.err, tt.wantOK)
	}
	storeOutTCheckErr(fx, what, got.err, storeOutTWant(fx, tt.wantErr, tt.wantInjected), tt.notErr, tt.check)
	storeOutTCheckSteps(fx, what, tt.wait, tt.refusal, tt.cancelAt != nil)
	storeOutTCheckLock(fx, what, storeOutTLockStateOf(tt.wait, tt.refusal))
	storeOutTCheckAfter(t, tt.after, before, storeOutTCapture(fx, false), name, r)
}

// storeOutTLockStateOf is what a row requires of the record lock file after a call that met the lock as wait says.
func storeOutTLockStateOf(wait storeOutTWait, refusal bool) storeOutTLockState {
	switch {
	case refusal:
		return storeOutTNoLock
	case wait == storeOutTFree || wait == storeOutTHeldReleased:
		return storeOutTAcquired
	default:
		return storeOutTNotAcquired
	}
}

// storeOutTFail returns the fault that fails the step at file of the record name with err.
func storeOutTFail(step storeStep, file string, err error) storeOutTFault {
	return storeOutTFault{storeOutTAt{step, file}, err}
}

// storeOutTReleaseFails is a fault list that fails the release of the record lock.
func storeOutTReleaseFails(*storeOutTFixture, string) []storeOutTFault {
	return []storeOutTFault{storeOutTFail(storeRelease, recordLockName, storeOutTReleaseError{})}
}

// storeOutTMirrorWriteFails is a fault list that fails the write of the record's decimal mirror.
func storeOutTMirrorWriteFails(_ *storeOutTFixture, name string) []storeOutTFault {
	return []storeOutTFault{storeOutTFail(storeWrite, name+".pid", storeOutTMirrorError{})}
}

// storeOutTRecordWriteFails is a fault list that fails the write of the authoritative record with an I/O error.
func storeOutTRecordWriteFails(fx *storeOutTFixture, name string) []storeOutTFault {
	return []storeOutTFault{storeOutTFail(storeWrite, name+".pid.json", storeOutTEIO(fx.root, name+".pid.json"))}
}

// storeOutTRecordLookupFails is a fault list that fails the lookup of the authoritative record with an I/O error.
func storeOutTRecordLookupFails(fx *storeOutTFixture, name string) []storeOutTFault {
	return []storeOutTFault{storeOutTFail(storeLookup, name+".pid.json", storeOutTEIO(fx.root, name+".pid.json"))}
}

// storeOutTAtRecord returns the step at the authoritative record of the name.
func storeOutTAtRecord(step storeStep) func(name string) storeOutTAt {
	return func(name string) storeOutTAt { return storeOutTAt{step, name + ".pid.json"} }
}

func TestStorePublishOutcome(t *testing.T) {
	seedPrior := func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTPrior(fx.root)) }
	tests := map[string]storeOutTPublishCase{
		"success: a first record is published with its mirror": {
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record replaces the prior record of another generation and its mirror": {
			setup: seedPrior, wantOK: true, after: storeOutTWritten,
		},
		"success: a stored record equal to the record is replaced all the same": {
			setup:  func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTRecord(fx.root)) },
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record of another name is published and the record stored beside it is untouched": {
			setup:  func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTDecoy(fx.root)) },
			name:   new(storeOutTOther),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a lock another holder gives up while the call waits for it is taken and the record published": {
			setup: seedPrior, wait: storeOutTHeldReleased, wantOK: true, after: storeOutTWritten,
		},
		"success: a context that ends once the lock is held does not interrupt the writes": {
			setup: seedPrior, cancelAt: storeOutTAtRecord(storeWrite), wantOK: true, after: storeOutTWritten,
		},
		"error: a context ended before the call publishes nothing": {
			setup: seedPrior, wait: storeOutTCanceled, wantErr: []error{context.Canceled}, after: storeOutTKept,
		},
		"error: a deadline passed before the call publishes nothing": {
			setup: seedPrior, wait: storeOutTExpired, wantErr: []error{context.DeadlineExceeded}, after: storeOutTKept,
		},
		"error: a context canceled while another holder keeps the lock publishes nothing": {
			setup: seedPrior, wait: storeOutTHeldCanceled, wantErr: []error{context.Canceled}, after: storeOutTKept,
		},
		"error: a deadline that passes while another holder keeps the lock publishes nothing": {
			setup: seedPrior, wait: storeOutTHeldDeadline, wantErr: []error{context.DeadlineExceeded}, after: storeOutTKept,
		},
		"error: a directory at the record's name is refused before the commitment": {
			setup: func(fx *storeOutTFixture) {
				if err := os.Mkdir(filepath.Join(fx.root, storeOutTName+".pid.json"), 0o700); err != nil {
					fx.t.Fatal(err)
				}
			},
			wantErr: []error{procinfo.ErrRecordEntry}, notErr: []error{procinfo.ErrMirrorPublish}, after: storeOutTKept,
		},
		"error: a symbolic link at the record's name is refused and its target is untouched": {
			setup: func(fx *storeOutTFixture) {
				storeOutTSeed(fx.t, fx.root, storeOutTTarget, storeOutTPrior(fx.root))
				storeOutTLinkRecord(fx.t, fx.root)
			},
			wantErr: []error{procinfo.ErrRecordEntry}, notErr: []error{procinfo.ErrMirrorPublish}, after: storeOutTKept,
		},
		"error: a read-only state root fails the record's write before the commitment": {
			denyRoot: true,
			wantErr:  []error{fs.ErrPermission}, wantInjected: true, notErr: []error{procinfo.ErrMirrorPublish}, after: storeOutTKept,
		},
		"error: a failed write of the record keeps the prior record and is no publication": {
			setup: seedPrior, faults: storeOutTRecordWriteFails,
			wantInjected: true, notErr: []error{procinfo.ErrMirrorPublish}, after: storeOutTKept,
		},
		"error: a directory at the mirror's name fails after the commitment and the record stays published": {
			setup: func(fx *storeOutTFixture) {
				seedPrior(fx)
				storeOutTMirrorDir(fx.t, fx.root, storeOutTName)
			},
			wantOK: true, wantErr: []error{procinfo.ErrMirrorPublish, procinfo.ErrRecordEntry}, after: storeOutTRecordOnly,
		},
		"error: a failed write of the mirror after the commitment is returned with its cause and the record stays published": {
			setup: seedPrior, faults: storeOutTMirrorWriteFails,
			wantOK: true, wantErr: []error{procinfo.ErrMirrorPublish, storeOutTMirrorError{}}, after: storeOutTRecordOnly,
		},
		"error: a release failure after the commitment keeps published true": {
			setup: seedPrior, faults: storeOutTReleaseFails,
			wantOK: true, wantErr: []error{storeOutTReleaseError{}}, notErr: []error{procinfo.ErrMirrorPublish}, after: storeOutTWritten,
		},
		"error: a mirror failure and a release failure after the commitment are both returned": {
			setup: func(fx *storeOutTFixture) {
				seedPrior(fx)
				storeOutTMirrorDir(fx.t, fx.root, storeOutTName)
			},
			faults: storeOutTReleaseFails,
			wantOK: true, wantErr: []error{procinfo.ErrMirrorPublish, procinfo.ErrRecordEntry, storeOutTReleaseError{}}, after: storeOutTRecordOnly,
		},
		// Publish keeps its one result and the outcomes of PublishContext.
		"success: Publish publishes the record and its mirror": {
			setup: seedPrior, legacy: true, after: storeOutTWritten,
		},
		// The landed Publish does not yet go through PublishContext, so it does not report its release to the fault
		// hook: this row fails on the compile-only declarations by its assertion, not by a panic, and passes once
		// Publish delegates to PublishContext.
		"error: Publish returns a release failure after the commitment and the record stays published": {
			setup: seedPrior, legacy: true, faults: storeOutTReleaseFails,
			wantErr: []error{storeOutTReleaseError{}}, after: storeOutTWritten,
		},
		"error: Publish returns a mirror failure after the commitment and the record stays published": {
			setup: func(fx *storeOutTFixture) {
				seedPrior(fx)
				storeOutTMirrorDir(fx.t, fx.root, storeOutTName)
			},
			legacy: true, wantErr: []error{procinfo.ErrMirrorPublish, procinfo.ErrRecordEntry}, after: storeOutTRecordOnly,
		},
		"error: Publish refuses a record without a start token before the lock": {
			setup: seedPrior, legacy: true,
			record:  func(_ *storeOutTFixture, r *procinfo.Record) { r.StartToken = "" },
			refusal: true, wantErr: []error{errNoStartToken}, after: storeOutTKept,
		},
	}
	maps.Copy(tests, storeOutTRefusals())
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			what := "PublishContext"
			if tt.legacy {
				what = "Publish"
			}
			storeOutTRunPublishCase(t, what, tt, func(s Store, ctx context.Context, name string, r procinfo.Record) (bool, error) {
				if tt.legacy {
					return false, s.Publish(name, r)
				}
				return s.PublishContext(ctx, name, r)
			})
		})
	}
}

func TestStoreReconcile(t *testing.T) {
	seedEqual := func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTRecord(fx.root)) }
	// seedDiffering stores the row's record with one field changed, a record that is valid but not the row's.
	seedDiffering := func(change func(r *procinfo.Record)) func(fx *storeOutTFixture) {
		return func(fx *storeOutTFixture) {
			r := storeOutTRecord(fx.root)
			change(&r)
			storeOutTSeed(fx.t, fx.root, storeOutTName, r)
		}
	}
	// seedBytes stores data as the record file, a regular file, with the row's mirror beside it.
	seedBytes := func(data func(fx *storeOutTFixture) string) func(fx *storeOutTFixture) {
		return func(fx *storeOutTFixture) {
			storeOutTWrite(fx.t, fx.root, storeOutTName+".pid.json", data(fx))
			storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "43101")
		}
	}
	corrupt := seedBytes(func(*storeOutTFixture) string {
		return `{"pid":43101,"command":"/opt/model-gateway/bin/model-gateway serve-`
	})
	invalid := seedBytes(func(fx *storeOutTFixture) string {
		r := storeOutTRecord(fx.root)
		r.StateRoot = fx.parent
		return storeOutTEncode(fx.t, r)
	})
	staleMirror := func(fx *storeOutTFixture) {
		seedEqual(fx)
		storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "999")
	}
	tests := map[string]storeOutTPublishCase{
		// The stored record already equals the record: nothing is written.
		"success: a stored record equal to the record writes nothing": {
			setup: seedEqual, after: storeOutTKept,
		},
		"success: an equal record stored with other white space writes nothing": {
			setup: seedBytes(func(fx *storeOutTFixture) string {
				return "\n  " + storeOutTEncode(fx.t, storeOutTRecord(fx.root)) + "\n"
			}),
			after: storeOutTKept,
		},
		"success: an equal record's stale mirror is repaired and the record is not written": {
			setup: staleMirror, after: storeOutTMirrorFixed,
		},
		"success: an equal record's missing mirror is repaired and the record is not written": {
			setup: func(fx *storeOutTFixture) {
				seedEqual(fx)
				if err := os.Remove(filepath.Join(fx.root, storeOutTName+".pid")); err != nil {
					fx.t.Fatal(err)
				}
			},
			after: storeOutTMirrorFixed,
		},
		"error: an equal record whose mirror is a directory is not written and the repair fails": {
			setup: func(fx *storeOutTFixture) {
				seedEqual(fx)
				storeOutTMirrorDir(fx.t, fx.root, storeOutTName)
			},
			wantErr: []error{procinfo.ErrMirrorRepair, procinfo.ErrRecordEntry}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: an equal record whose stale mirror cannot be written returns the write's cause and is not written": {
			setup: staleMirror, faults: storeOutTMirrorWriteFails,
			wantErr: []error{procinfo.ErrMirrorRepair, storeOutTMirrorError{}}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		// A missing record, or a valid one that differs in any field, is replaced.
		"success: a missing record is written with its mirror": {
			wantOK: true, after: storeOutTWritten,
		},
		"success: a mirror without a record is a missing record and is written": {
			setup:  func(fx *storeOutTFixture) { storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "43101") },
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record of another name is written and the record stored beside it is untouched": {
			setup:  func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTDecoy(fx.root)) },
			name:   new(storeOutTOther),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record of another process number is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.PID = 43099 }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record of another generation is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Generation = "generation-6" }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record of another start token is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.StartToken = "6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b/9" }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record without a start token is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.StartToken = "" }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record that differs only in its port is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Port = 0 }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record that differs only in its role is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Role = "supervisor" }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record that differs only in its image's inode is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Image.Inode = 4243 }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record that differs only in its image's path is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Image.Path = "/opt/model-gateway/bin/model-gateway-1.2.3" }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record that differs only in its image's device is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Image.Device = 64770 }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record that differs only in its command is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Command = "/opt/model-gateway/bin/model-gateway serve-shim" }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record that differs only in its version is replaced": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Version = "1.2.4" }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a record that differs only in its start time is replaced": {
			setup: seedDiffering(func(r *procinfo.Record) {
				r.StartedAt = procinfo.LegacyStart(time.Date(2026, time.November, 28, 9, 30, 1, 0, time.Local))
			}),
			wantOK: true, after: storeOutTWritten,
		},
		"error: a failed write of a missing record is a required repair": {
			faults:  storeOutTRecordWriteFails,
			wantErr: []error{ErrRepair}, wantInjected: true, after: storeOutTKept,
		},
		"error: a failed write over a differing record is a required repair and keeps that record": {
			setup:   seedDiffering(func(r *procinfo.Record) { r.Port = 0 }),
			faults:  storeOutTRecordWriteFails,
			wantErr: []error{ErrRepair}, wantInjected: true, after: storeOutTKept,
		},
		// The same refusal of the operating system on both sides of the rule: permission denied while reading the
		// record leaves it alone, permission denied while writing it is a required repair. The first pair uses the
		// file modes where they refuse this process and the fault hook where they do not (the superuser); the second
		// pair always injects the refusal, so that every user decides the distinction.
		"error: permission denied while reading a differing record writes nothing and is no required repair": {
			setup:      seedDiffering(func(r *procinfo.Record) { r.Generation = "generation-6" }),
			denyRecord: true,
			wantErr:    []error{fs.ErrPermission}, wantInjected: true, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: permission denied while writing a deleted record into a read-only state root is a required repair": {
			denyRoot: true,
			wantErr:  []error{ErrRepair, fs.ErrPermission}, wantInjected: true, after: storeOutTKept,
		},
		"error: an injected permission refusal of the record's lookup writes nothing and is no required repair": {
			setup: seedDiffering(func(r *procinfo.Record) { r.Generation = "generation-6" }),
			faults: func(fx *storeOutTFixture, name string) []storeOutTFault {
				return []storeOutTFault{storeOutTFail(storeLookup, name+".pid.json", storeOutTDenied(fx.root, name+".pid.json"))}
			},
			wantErr: []error{fs.ErrPermission}, wantInjected: true, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: an injected permission refusal of the record's write is a required repair": {
			setup: seedDiffering(func(r *procinfo.Record) { r.Generation = "generation-6" }),
			faults: func(fx *storeOutTFixture, name string) []storeOutTFault {
				return []storeOutTFault{storeOutTFail(storeWrite, name+".pid.json", storeOutTDenied(fx.root, name+".pid.json"))}
			},
			wantErr: []error{ErrRepair, fs.ErrPermission}, wantInjected: true, after: storeOutTKept,
		},
		"error: a directory at the mirror's name after the record was written is returned with wrote true and is no required repair": {
			setup: func(fx *storeOutTFixture) {
				seedDiffering(func(r *procinfo.Record) { r.Generation = "generation-6" })(fx)
				storeOutTMirrorDir(fx.t, fx.root, storeOutTName)
			},
			wantOK: true, wantErr: []error{procinfo.ErrRecordEntry}, notErr: []error{ErrRepair}, after: storeOutTRecordOnly,
		},
		"error: a failed write of the mirror after the record was written returns that cause with wrote true and is no required repair": {
			setup:  seedDiffering(func(r *procinfo.Record) { r.Generation = "generation-6" }),
			faults: storeOutTMirrorWriteFails,
			wantOK: true, wantErr: []error{storeOutTMirrorError{}}, notErr: []error{ErrRepair}, after: storeOutTRecordOnly,
		},
		"error: a release failure after the record was written is returned with wrote true and is no required repair": {
			faults: storeOutTReleaseFails,
			wantOK: true, wantErr: []error{storeOutTReleaseError{}}, notErr: []error{ErrRepair}, after: storeOutTWritten,
		},
		// Bytes read from a regular record file that do not make a valid record are repaired from the record.
		"success: corrupt JSON in a regular record file is repaired": {
			setup: corrupt, wantOK: true, after: storeOutTWritten,
		},
		"success: an empty regular record file is repaired": {
			setup:  seedBytes(func(*storeOutTFixture) string { return "" }),
			wantOK: true, after: storeOutTWritten,
		},
		"success: a decodable record of another state root is repaired": {
			setup: invalid, wantOK: true, after: storeOutTWritten,
		},
		"success: a decodable record without a process number is repaired": {
			setup: seedBytes(func(fx *storeOutTFixture) string {
				r := storeOutTRecord(fx.root)
				r.PID = 0
				return storeOutTEncode(fx.t, r)
			}),
			wantOK: true, after: storeOutTWritten,
		},
		"error: corrupt JSON whose repair cannot be written is a required repair and stays as it was": {
			setup: corrupt, faults: storeOutTRecordWriteFails,
			wantErr: []error{ErrRepair}, wantInjected: true, after: storeOutTKept,
		},
		"error: an invalid record whose repair cannot be written is a required repair and stays as it was": {
			setup: invalid, faults: storeOutTRecordWriteFails,
			wantErr: []error{ErrRepair}, wantInjected: true, after: storeOutTKept,
		},
		// Bytes that could not be read, or an entry that is not a regular file, are left alone.
		"error: an ordinary failure to look up the record writes nothing and is no required repair": {
			setup:        seedDiffering(func(r *procinfo.Record) { r.Port = 0 }),
			faults:       storeOutTRecordLookupFails,
			wantInjected: true, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: a failed lookup where no record exists is no missing record, writes nothing and is no required repair": {
			faults:       storeOutTRecordLookupFails,
			wantInjected: true, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: a symbolic link to an equal record is refused and neither it nor its target is written": {
			setup: func(fx *storeOutTFixture) {
				storeOutTSeed(fx.t, fx.root, storeOutTTarget, storeOutTRecord(fx.root))
				storeOutTLinkRecord(fx.t, fx.root)
			},
			wantErr: []error{procinfo.ErrRecordEntry}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: a dangling symbolic link is no missing record and is not written": {
			setup:   func(fx *storeOutTFixture) { storeOutTLinkRecord(fx.t, fx.root) },
			wantErr: []error{procinfo.ErrRecordEntry}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: a directory at the record's name is refused and is no required repair": {
			setup: func(fx *storeOutTFixture) {
				if err := os.Mkdir(filepath.Join(fx.root, storeOutTName+".pid.json"), 0o700); err != nil {
					fx.t.Fatal(err)
				}
			},
			wantErr: []error{procinfo.ErrRecordEntry}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		// The context bounds the lock wait only.
		"error: a context ended before the call writes nothing and is no required repair": {
			wait: storeOutTCanceled, wantErr: []error{context.Canceled}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: a deadline passed before the call writes nothing and is no required repair": {
			setup: corrupt, wait: storeOutTExpired, wantErr: []error{context.DeadlineExceeded}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: a context canceled while another holder keeps the lock writes nothing and is no required repair": {
			setup: corrupt, wait: storeOutTHeldCanceled, wantErr: []error{context.Canceled}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"error: a deadline that passes while another holder keeps the lock writes nothing and is no required repair": {
			setup: corrupt, wait: storeOutTHeldDeadline, wantErr: []error{context.DeadlineExceeded}, notErr: []error{ErrRepair}, after: storeOutTKept,
		},
		"success: a lock another holder gives up while the call waits for it is taken and the record written": {
			wait: storeOutTHeldReleased, wantOK: true, after: storeOutTWritten,
		},
		"success: a context that ends during the lookup does not interrupt the repair": {
			setup: corrupt, cancelAt: storeOutTAtRecord(storeLookup), wantOK: true, after: storeOutTWritten,
		},
	}
	for name, tt := range storeOutTRefusals() {
		tt.notErr = append(tt.notErr, ErrRepair)
		tests[name] = tt
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			storeOutTRunPublishCase(t, "Reconcile", tt, func(s Store, ctx context.Context, name string, r procinfo.Record) (bool, error) {
				return s.Reconcile(ctx, name, r)
			})
		})
	}
}

func TestStoreRemoveIfContext(t *testing.T) {
	tests := map[string]struct {
		// stored changes the stored record; expected changes the record asked for. Both start as the row's record.
		stored   func(r *procinfo.Record)
		expected func(r *procinfo.Record)
		// setup prepares the state root after the record was stored.
		setup    func(fx *storeOutTFixture)
		noRecord bool
		name     *string
		wait     storeOutTWait
		faults   func(fx *storeOutTFixture, name string) []storeOutTFault
		// beforeRemove runs inside the operation after the record matched, before anything is unlinked.
		beforeRemove func(fx *storeOutTFixture)
		// cancels says the row ends the context inside the operation.
		cancels bool
		refusal bool
		wantOK  bool
		wantErr []error
		check   func(fx *storeOutTFixture, err error) string
		after   storeOutTAfter
		// afterCheck checks the record files itself when after is storeOutTCustom.
		afterCheck func(t *testing.T, before, now storeOutTState)
	}{
		"success: the expected record is removed with its mirror": {
			wantOK: true, after: storeOutTRemoved,
		},
		"success: fields other than process number, generation and token do not decide": {
			expected: func(r *procinfo.Record) {
				r.Command, r.Version, r.Port, r.Role = "another command", "9.9.9", 1, "supervisor"
				r.Image = procinfo.Image{Path: "/elsewhere/model-gateway", Device: 1, Inode: 2}
				r.StartedAt = procinfo.LegacyStart(time.Date(2026, time.November, 27, 1, 2, 3, 0, time.Local))
				r.StateRoot = "/elsewhere/state"
			},
			wantOK: true, after: storeOutTRemoved,
		},
		"success: a matching record whose mirror is missing is removed without an error": {
			setup: func(fx *storeOutTFixture) {
				if err := os.Remove(filepath.Join(fx.root, storeOutTName+".pid")); err != nil {
					fx.t.Fatal(err)
				}
			},
			wantOK: true, after: storeOutTRemoved,
		},
		"success: a record of another name is removed and the record stored beside it is untouched": {
			setup:  func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTDecoy(fx.root)) },
			name:   new(storeOutTOther),
			wantOK: true, after: storeOutTRemoved,
		},
		"success: another process number keeps the record": {
			expected: func(r *procinfo.Record) { r.PID = 43099 }, after: storeOutTKept,
		},
		"success: another generation keeps the record": {
			expected: func(r *procinfo.Record) { r.Generation = "generation-6" }, after: storeOutTKept,
		},
		"success: another start token keeps the record": {
			expected: func(r *procinfo.Record) { r.StartToken = "6f1d2c3b-4a59-4e6f-8a7b-9c0d1e2f3a4b/9" }, after: storeOutTKept,
		},
		"success: a missing record is nothing to remove": {
			noRecord: true, after: storeOutTKept,
		},
		"success: a mirror without a record is nothing to remove and is kept": {
			noRecord: true,
			setup:    func(fx *storeOutTFixture) { storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "43101") },
			after:    storeOutTKept,
		},
		"success: a lock another holder gives up while the call waits for it is taken and the record removed": {
			wait: storeOutTHeldReleased, wantOK: true, after: storeOutTRemoved,
		},
		"success: a context that ends once the lock is held does not interrupt the removal": {
			beforeRemove: func(fx *storeOutTFixture) { fx.cancelNow() }, cancels: true,
			wantOK: true, after: storeOutTRemoved,
		},
		"error: a record that cannot be unlinked after it matched is not removed and its mirror is kept": {
			// A directory put in the record's place after the match cannot be unlinked as a file.
			beforeRemove: func(fx *storeOutTFixture) {
				path := filepath.Join(fx.root, storeOutTName+".pid.json")
				if err := os.Remove(path); err != nil {
					fx.t.Error(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					fx.t.Error(err)
				}
			},
			check: storeOutTPathAt(storeOutTName + ".pid.json"),
			after: storeOutTCustom,
			afterCheck: func(t *testing.T, before, now storeOutTState) {
				record, mirror := filepath.Join("state", storeOutTName+".pid.json"), filepath.Join("state", storeOutTName+".pid")
				if !now[record].present || !now[record].info.IsDir() {
					t.Errorf("the directory put in the record's place is gone: %+v", now[record])
				}
				storeOutTSame(t, mirror, before[mirror], now[mirror])
			},
		},
		"error: a mirror that cannot be removed after the record was unlinked keeps removed true": {
			setup:  func(fx *storeOutTFixture) { storeOutTMirrorDir(fx.t, fx.root, storeOutTName) },
			wantOK: true, check: storeOutTPathAt(storeOutTName + ".pid"), after: storeOutTRecordGone,
		},
		"error: a release failure after the record was unlinked keeps removed true": {
			faults: storeOutTReleaseFails,
			wantOK: true, wantErr: []error{storeOutTReleaseError{}}, after: storeOutTRemoved,
		},
		"error: a corrupt record is not removed": {
			setup: func(fx *storeOutTFixture) { storeOutTWrite(fx.t, fx.root, storeOutTName+".pid.json", "{") },
			check: storeOutTSyntax, after: storeOutTKept,
		},
		"error: an invalid record name is refused": {
			name: new("../" + storeOutTName), refusal: true, wantErr: []error{fs.ErrInvalid}, after: storeOutTKept,
		},
		"error: a context ended before the call removes nothing": {
			wait: storeOutTCanceled, wantErr: []error{context.Canceled}, after: storeOutTKept,
		},
		"error: a deadline passed before the call removes nothing": {
			wait: storeOutTExpired, wantErr: []error{context.DeadlineExceeded}, after: storeOutTKept,
		},
		"error: a context canceled while another holder keeps the lock removes nothing": {
			wait: storeOutTHeldCanceled, wantErr: []error{context.Canceled}, after: storeOutTKept,
		},
		"error: a deadline that passes while another holder keeps the lock removes nothing": {
			wait: storeOutTHeldDeadline, wantErr: []error{context.DeadlineExceeded}, after: storeOutTKept,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fx := storeOutTNewFixture(t)
			recordName := storeOutTName
			if tt.name != nil {
				recordName = *tt.name
			}
			current := storeOutTRecord(fx.root)
			if tt.stored != nil {
				tt.stored(&current)
			}
			if !tt.noRecord && filepath.Base(recordName) == recordName {
				storeOutTSeed(t, fx.root, recordName, current)
			}
			if tt.setup != nil {
				tt.setup(fx)
			}
			expected := current
			if tt.expected != nil {
				tt.expected(&expected)
			}
			if tt.faults != nil {
				fx.faults = tt.faults(fx, recordName)
			}
			before := storeOutTCapture(fx, true)
			got, ok := storeOutTRun(fx, "RemoveIfContext", tt.wait, func(ctx context.Context) storeOutTBool {
				s := fx.store(fx.root)
				if tt.beforeRemove != nil {
					s.beforeRemove = func() { tt.beforeRemove(fx) }
				}
				removed, err := s.RemoveIfContext(ctx, recordName, expected)
				return storeOutTBool{removed, err}
			})
			if !ok {
				return
			}
			if got.ok != tt.wantOK {
				t.Errorf("RemoveIfContext = %v, %v; want %v", got.ok, got.err, tt.wantOK)
			}
			storeOutTCheckErr(fx, "RemoveIfContext", got.err, tt.wantErr, nil, tt.check)
			storeOutTCheckSteps(fx, "RemoveIfContext", tt.wait, tt.refusal, tt.cancels)
			storeOutTCheckLock(fx, "RemoveIfContext", storeOutTLockStateOf(tt.wait, tt.refusal))
			now := storeOutTCapture(fx, false)
			storeOutTCheckAfter(t, tt.after, before, now, recordName, current)
			if tt.afterCheck != nil {
				tt.afterCheck(t, before, now)
			}
		})
	}
}

func TestStoreReadContext(t *testing.T) {
	staleMirror := func(fx *storeOutTFixture) { storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "999") }
	tests := map[string]struct {
		// stored changes the row's record before it is stored with the existing writer.
		stored func(r *procinfo.Record)
		// setup prepares the state root after the record was stored, or instead of storing it with noRecord.
		setup    func(fx *storeOutTFixture)
		noRecord bool
		name     *string
		wait     storeOutTWait
		faults   func(fx *storeOutTFixture, name string) []storeOutTFault
		cancelAt func(name string) storeOutTAt
		refusal  bool
		// wantRecord asks for the stored record; otherwise the zero record is wanted.
		wantRecord bool
		wantErr    []error
		// wantInjected asks for an error that holds every error the row's fault hook returns, as the same objects.
		wantInjected bool
		check        func(fx *storeOutTFixture, err error) string
		after        storeOutTAfter
	}{
		"success: a record and its mirror are read": {
			wantRecord: true, after: storeOutTKept,
		},
		"success: a historical record without a start token stays readable": {
			stored:     func(r *procinfo.Record) { r.StartToken = "" },
			wantRecord: true, after: storeOutTKept,
		},
		"success: a stale mirror is repaired": {
			setup: staleMirror, wantRecord: true, after: storeOutTMirrorFixed,
		},
		"success: an empty mirror is repaired": {
			setup:      func(fx *storeOutTFixture) { storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "") },
			wantRecord: true, after: storeOutTMirrorFixed,
		},
		"success: a missing mirror is repaired": {
			setup: func(fx *storeOutTFixture) {
				if err := os.Remove(filepath.Join(fx.root, storeOutTName+".pid")); err != nil {
					fx.t.Fatal(err)
				}
			},
			wantRecord: true, after: storeOutTMirrorFixed,
		},
		"success: a record of another name is read and repaired and the record stored beside it is untouched": {
			setup: func(fx *storeOutTFixture) {
				storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTDecoy(fx.root))
				storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "999")
				storeOutTWrite(fx.t, fx.root, storeOutTOther+".pid", "999")
			},
			name:       new(storeOutTOther),
			wantRecord: true, after: storeOutTMirrorFixed,
		},
		"success: a lock another holder gives up while the call waits for it is taken and the mirror repaired": {
			setup: staleMirror, wait: storeOutTHeldReleased, wantRecord: true, after: storeOutTMirrorFixed,
		},
		"success: a context that ends during the lookup does not interrupt the mirror repair": {
			setup: staleMirror, cancelAt: storeOutTAtRecord(storeLookup), wantRecord: true, after: storeOutTMirrorFixed,
		},
		"error: a mirror that is a directory returns the record with the repair's error and its cause": {
			setup:      func(fx *storeOutTFixture) { storeOutTMirrorDir(fx.t, fx.root, storeOutTName) },
			wantRecord: true, wantErr: []error{procinfo.ErrMirrorRepair, procinfo.ErrRecordEntry}, after: storeOutTKept,
		},
		"error: a stale mirror that cannot be written returns the record with the repair's error and the write's cause": {
			setup: staleMirror, faults: storeOutTMirrorWriteFails,
			wantRecord: true, wantErr: []error{procinfo.ErrMirrorRepair, storeOutTMirrorError{}}, after: storeOutTKept,
		},
		"error: a release failure is returned with the record": {
			faults:     storeOutTReleaseFails,
			wantRecord: true, wantErr: []error{storeOutTReleaseError{}}, after: storeOutTKept,
		},
		"error: a missing record does not exist": {
			noRecord: true, wantErr: []error{fs.ErrNotExist}, after: storeOutTKept,
		},
		"error: a decimal mirror alone is not a record": {
			noRecord: true,
			setup:    func(fx *storeOutTFixture) { storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "43101") },
			wantErr:  []error{fs.ErrNotExist}, after: storeOutTKept,
		},
		"error: corrupt JSON is refused and left as it is": {
			setup: func(fx *storeOutTFixture) { storeOutTWrite(fx.t, fx.root, storeOutTName+".pid.json", "{") },
			check: storeOutTSyntax, after: storeOutTKept,
		},
		"error: a decodable record of another state root is refused and left as it is": {
			setup: func(fx *storeOutTFixture) {
				r := storeOutTRecord(fx.root)
				r.StateRoot = fx.parent
				storeOutTWrite(fx.t, fx.root, storeOutTName+".pid.json", storeOutTEncode(fx.t, r))
				storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "999")
			},
			wantErr: []error{fs.ErrInvalid}, after: storeOutTKept,
		},
		"error: a symbolic link to a valid record is refused and its target is untouched": {
			noRecord: true,
			setup: func(fx *storeOutTFixture) {
				storeOutTSeed(fx.t, fx.root, storeOutTTarget, storeOutTRecord(fx.root))
				storeOutTLinkRecord(fx.t, fx.root)
			},
			wantErr: []error{procinfo.ErrRecordEntry}, after: storeOutTKept,
		},
		"error: a failure to look up the record repairs nothing": {
			setup: staleMirror, faults: storeOutTRecordLookupFails,
			wantInjected: true, after: storeOutTKept,
		},
		"error: an invalid record name is refused": {
			name: new("../" + storeOutTName), refusal: true, wantErr: []error{fs.ErrInvalid}, after: storeOutTKept,
		},
		"error: a context ended before the call reads nothing and leaves a stale mirror stale": {
			setup: staleMirror, wait: storeOutTCanceled,
			wantErr: []error{context.Canceled}, after: storeOutTKept,
		},
		"error: a deadline passed before the call reads nothing and leaves a stale mirror stale": {
			setup: staleMirror, wait: storeOutTExpired,
			wantErr: []error{context.DeadlineExceeded}, after: storeOutTKept,
		},
		"error: a context canceled while another holder keeps the lock leaves a stale mirror stale": {
			setup: staleMirror, wait: storeOutTHeldCanceled,
			wantErr: []error{context.Canceled}, after: storeOutTKept,
		},
		"error: a deadline that passes while another holder keeps the lock leaves a stale mirror stale": {
			setup: staleMirror, wait: storeOutTHeldDeadline,
			wantErr: []error{context.DeadlineExceeded}, after: storeOutTKept,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fx := storeOutTNewFixture(t)
			recordName := storeOutTName
			if tt.name != nil {
				recordName = *tt.name
			}
			stored := storeOutTRecord(fx.root)
			if tt.stored != nil {
				tt.stored(&stored)
			}
			if !tt.noRecord && filepath.Base(recordName) == recordName {
				storeOutTSeed(t, fx.root, recordName, stored)
			}
			if tt.setup != nil {
				tt.setup(fx)
			}
			if tt.faults != nil {
				fx.faults = tt.faults(fx, recordName)
			}
			if tt.cancelAt != nil {
				at := tt.cancelAt(recordName)
				fx.cancelAt = &at
			}
			before := storeOutTCapture(fx, true)
			type result struct {
				r   procinfo.Record
				err error
			}
			got, ok := storeOutTRun(fx, "ReadContext", tt.wait, func(ctx context.Context) result {
				r, err := fx.store(fx.root).ReadContext(ctx, recordName)
				return result{r, err}
			})
			if !ok {
				return
			}
			want := procinfo.Record{}
			if tt.wantRecord {
				want = stored
			}
			if diff := gocmp.Diff(want, got.r); diff != "" {
				t.Errorf("ReadContext record (-want +got):\n%s", diff)
			}
			storeOutTCheckErr(fx, "ReadContext", got.err, storeOutTWant(fx, tt.wantErr, tt.wantInjected), nil, tt.check)
			storeOutTCheckSteps(fx, "ReadContext", tt.wait, tt.refusal, tt.cancelAt != nil)
			storeOutTCheckLock(fx, "ReadContext", storeOutTLockStateOf(tt.wait, tt.refusal))
			storeOutTCheckAfter(t, tt.after, before, storeOutTCapture(fx, false), recordName, stored)
		})
	}
}

// storeOutTFinalAtRelease fails the row unless, when the store reports its release and before control returns to the
// store, the record files of dir are already in the state after says. It shows that no write or unlink of the record
// is left for after the release is reported. It does not show that every file operation ended before the real unlock:
// a store that unlocked, then wrote or unlinked, and only then reported its release would pass. That order is checked
// by reading the implementation, not by this row.
func storeOutTFinalAtRelease(t *testing.T, dir string, after storeOutTAfter, r procinfo.Record) {
	t.Helper()
	record, mirror := filepath.Join(dir, storeOutTName+".pid.json"), filepath.Join(dir, storeOutTName+".pid")
	wantMirror := func() {
		data, err := os.ReadFile(mirror)
		if err != nil || string(data) != strconv.Itoa(r.PID) {
			t.Errorf("at the release the mirror holds %q (%v); want %d", data, err, r.PID)
		}
	}
	switch after {
	case storeOutTWritten:
		var got procinfo.Record
		data, err := os.ReadFile(record)
		if err == nil {
			err = json.Unmarshal(data, &got)
		}
		if err != nil || got != r {
			t.Errorf("at the release the authoritative record holds %q (%v); want the record", data, err)
		}
		wantMirror()
	case storeOutTMirrorFixed:
		wantMirror()
	case storeOutTRemoved:
		for _, path := range []string{record, mirror} {
			if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("at the release %s is still there (lstat: %v); want it unlinked under the lock", path, err)
			}
		}
	}
}

// storeOutTOp is one of the four outcome forms called on the row's record.
type storeOutTOp int

const (
	storeOutTPublish storeOutTOp = iota
	storeOutTReconcile
	storeOutTRead
	storeOutTRemove
)

// call runs the form on s and returns its boolean result (published, wrote, removed; for ReadContext whether it
// returned r) and its error.
func (op storeOutTOp) call(ctx context.Context, s Store, r procinfo.Record) (bool, error) {
	switch op {
	case storeOutTPublish:
		return s.PublishContext(ctx, storeOutTName, r)
	case storeOutTReconcile:
		return s.Reconcile(ctx, storeOutTName, r)
	case storeOutTRead:
		got, err := s.ReadContext(ctx, storeOutTName)
		return got == r, err
	default:
		return s.RemoveIfContext(ctx, storeOutTName, r)
	}
}

// TestStoreOutcomeExclusion decides that every form takes the store's exclusion once and does every lookup and write,
// the decimal mirror's included, while it holds it and through the one state root it opened: the authoritative record
// already holds the record when the mirror is written, the release gives the lock up, and a state root replaced during
// the call is not followed.
func TestStoreOutcomeExclusion(t *testing.T) {
	seedPrior := func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTPrior(fx.root)) }
	seedEqual := func(fx *storeOutTFixture) { storeOutTSeed(fx.t, fx.root, storeOutTName, storeOutTRecord(fx.root)) }
	staleMirror := func(fx *storeOutTFixture) {
		seedEqual(fx)
		storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "999")
	}
	tests := map[string]struct {
		op    storeOutTOp
		setup func(fx *storeOutTFixture)
		// writes are the entries the form must write, in this order.
		writes []string
		// replaceRoot moves the state root away at the form's first lookup and puts an empty directory in its place.
		replaceRoot bool
		// after is the state of the record files, in the moved root when replaceRoot is set.
		after storeOutTAfter
		// unwritten says the form reports false: Reconcile wrote no authoritative record.
		unwritten bool
	}{
		"success: Reconcile replaces a differing record and its mirror under one exclusion": {
			op: storeOutTReconcile, setup: seedPrior, writes: []string{".pid.json", ".pid"}, after: storeOutTWritten,
		},
		"success: Reconcile repairs a corrupt record and its mirror under one exclusion": {
			op: storeOutTReconcile,
			setup: func(fx *storeOutTFixture) {
				storeOutTWrite(fx.t, fx.root, storeOutTName+".pid.json", "{")
				storeOutTWrite(fx.t, fx.root, storeOutTName+".pid", "999")
			},
			writes: []string{".pid.json", ".pid"}, after: storeOutTWritten,
		},
		"success: Reconcile writes a missing record and its mirror under one exclusion": {
			op: storeOutTReconcile, writes: []string{".pid.json", ".pid"}, after: storeOutTWritten,
		},
		"success: Reconcile repairs an equal record's stale mirror under one exclusion": {
			op: storeOutTReconcile, setup: staleMirror, writes: []string{".pid"}, after: storeOutTMirrorFixed, unwritten: true,
		},
		"success: PublishContext replaces the record and its mirror under one exclusion": {
			op: storeOutTPublish, setup: seedPrior, writes: []string{".pid.json", ".pid"}, after: storeOutTWritten,
		},
		"success: ReadContext repairs a stale mirror under one exclusion": {
			op: storeOutTRead, setup: staleMirror, writes: []string{".pid"}, after: storeOutTMirrorFixed,
		},
		"success: RemoveIfContext compares and removes under one exclusion": {
			op: storeOutTRemove, setup: seedEqual, after: storeOutTRemoved,
		},
		"success: Reconcile keeps working in the state root it opened when the root is replaced during the call": {
			op: storeOutTReconcile, setup: seedPrior, writes: []string{".pid.json", ".pid"}, replaceRoot: true, after: storeOutTWritten,
		},
		"success: PublishContext keeps working in the state root it opened when the root is replaced during the call": {
			op: storeOutTPublish, setup: seedPrior, writes: []string{".pid.json", ".pid"}, replaceRoot: true, after: storeOutTWritten,
		},
		"success: ReadContext repairs the mirror in the state root it opened when the root is replaced during the call": {
			op: storeOutTRead, setup: staleMirror, writes: []string{".pid"}, replaceRoot: true, after: storeOutTMirrorFixed,
		},
		"success: RemoveIfContext removes from the state root it opened when the root is replaced during the call": {
			op: storeOutTRemove, setup: seedEqual, replaceRoot: true, after: storeOutTRemoved,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fx := storeOutTNewFixture(t)
			if tt.setup != nil {
				tt.setup(fx)
			}
			root := fx.root
			moved := filepath.Join(fx.parent, "moved")
			r := storeOutTRecord(root)
			before := storeOutTCapture(fx, true)
			// decoyPath is a directory, an entry that is not a regular file, put at the record's name in the directory that
			// replaces the state root: a store that looked the record up by its path would meet it.
			decoyPath := filepath.Join(root, storeOutTName+".pid.json")
			var replaced atomic.Bool
			var decoy fs.FileInfo
			fx.observe = func(at storeOutTAt) {
				if tt.replaceRoot && at.step == storeLookup && replaced.CompareAndSwap(false, true) {
					if err := os.Rename(root, moved); err != nil {
						t.Errorf("move the state root away: %v", err)
					}
					if err := os.Mkdir(root, 0o700); err != nil {
						t.Errorf("put another directory at the state root: %v", err)
					}
					if err := os.Mkdir(decoyPath, 0o700); err != nil {
						t.Errorf("put a directory at the record's name in the replacing directory: %v", err)
					}
					var err error
					if decoy, err = os.Lstat(decoyPath); err != nil {
						t.Error(err)
					}
				}
				// The directory the call opened: the moved one once the state root was replaced.
				dir := root
				if replaced.Load() {
					dir = moved
				}
				switch at.step {
				case storeLookup, storeWrite:
					if !storeOutTLockHeld(t, dir) {
						t.Errorf("the %v ran without the store's exclusion", at)
					}
				case storeRelease:
					if storeOutTLockHeld(t, dir) {
						t.Error("the record lock is still held at the release")
					}
					storeOutTFinalAtRelease(t, dir, tt.after, r)
				}
				if at.step == storeWrite && at.name == storeOutTName+".pid" {
					var got procinfo.Record
					data, err := os.ReadFile(filepath.Join(dir, storeOutTName+".pid.json"))
					if err == nil {
						err = json.Unmarshal(data, &got)
					}
					if err != nil || got != r {
						t.Errorf("when the mirror is written the authoritative record holds %q (%v); want the record", data, err)
					}
				}
			}
			got, ok := storeOutTRun(fx, "the outcome form", storeOutTFree, func(ctx context.Context) storeOutTBool {
				done, err := tt.op.call(ctx, fx.store(root), r)
				return storeOutTBool{done, err}
			})
			if !ok {
				return
			}
			if got.ok == tt.unwritten || got.err != nil {
				t.Errorf("the outcome form = %v, %v; want %v, nil", got.ok, got.err, !tt.unwritten)
			}
			var writes []string
			releases := 0
			for _, s := range fx.seen() {
				switch s.step {
				case storeWrite:
					writes = append(writes, strings.TrimPrefix(s.name, storeOutTName))
				case storeRelease:
					releases++
				}
			}
			if diff := gocmp.Diff(tt.writes, writes); diff != "" {
				t.Errorf("writes reported through the fault hook (-want +got):\n%s", diff)
			}
			if releases != 1 {
				t.Errorf("the store's exclusion was given up %d times during one call; want once", releases)
			}
			if !tt.replaceRoot {
				storeOutTCheckLock(fx, "the outcome form", storeOutTAcquired)
				storeOutTCheckAfter(t, tt.after, before, storeOutTCapture(fx, false), storeOutTName, r)
				return
			}
			if !replaced.Load() {
				t.Fatal("the form made no lookup, so the state root was never replaced")
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != storeOutTName+".pid.json" {
				t.Errorf("the directory put at the state root's path during the call holds %v; want only the decoy", entries)
			}
			if now, err := os.Lstat(decoyPath); err != nil || decoy == nil || !now.IsDir() || !os.SameFile(decoy, now) {
				t.Errorf("the decoy directory at the record's name in the replacing directory was changed (lstat: %v)", err)
			}
			// Judge the moved root as the state root: its entries keep their names relative to the parent.
			fx.root = moved
			now := storeOutTCapture(fx, false)
			for k, e := range now {
				if rest, ok := strings.CutPrefix(k, "moved/"); ok {
					now[filepath.Join("state", rest)] = e
					delete(now, k)
				}
			}
			delete(now, "state")
			delete(now, "moved")
			beforeFiles := maps.Clone(before)
			delete(beforeFiles, "state")
			storeOutTCheckAfter(t, tt.after, beforeFiles, now, storeOutTName, r)
			if storeOutTLockHeld(t, moved) {
				t.Error("the record lock in the state root the call opened is still held")
			}
		})
	}
}
