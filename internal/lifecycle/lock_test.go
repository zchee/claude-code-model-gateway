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

package lifecycle_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) { venue.Require(); os.Exit(m.Run()) }

type lookup func(context.Context, int) (lifecycle.ProcessState, string)

func (f lookup) Lookup(ctx context.Context, pid int) (lifecycle.ProcessState, string) {
	return f(ctx, pid)
}

const (
	token    = "00000000-0000-4000-8000-000000000000"
	previous = "00000000-0000-4000-8000-000000000001"
)

func lockFor(state string) lifecycle.Lock {
	return lifecycle.Lock{StateDir: state, PID: 41, Host: "host", Processes: lookup(func(context.Context, int) (lifecycle.ProcessState, string) {
		return lifecycle.Alive, "Mon Jan  2 15:04:05 2006"
	}), NewToken: func() string { return token }}
}

func equal[T any](t testing.TB, want, got T) {
	t.Helper()
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func noError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func bytesAt(t testing.TB, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	noError(t, err)
	return string(data)
}

func mustBeAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Lstat(%q): %v, want nonexistent", path, err)
	}
}

func acquire(t testing.TB, lock lifecycle.Lock) func() {
	t.Helper()
	release, err := lock.Acquire(t.Context())
	noError(t, err)
	if release == nil {
		t.Fatal("missing release")
	}
	return release
}

func busyMessage(dir string) string {
	return fmt.Sprintf("gateway lifecycle is busy or ownership is unknown (%s); retry recover or run doctor", dir)
}

func TestLiveOwner(t *testing.T) {
	lock := lockFor(t.TempDir())
	dir := filepath.Join(lock.StateDir, "lifecycle.lock")
	older := lock
	older.NewToken = func() string { return previous }
	oldRelease := acquire(t, older)
	oldRelease()
	release := acquire(t, lock)
	const want = `{"token":"00000000-0000-4000-8000-000000000000","pid":41,"startedAt":"Mon Jan  2 15:04:05 2006","host":"host"}`
	path := filepath.Join(dir, token)
	equal(t, want, bytesAt(t, path))
	older.Timeout = new(time.Duration(0))
	_, err := older.Acquire(t.Context())
	if err == nil {
		t.Fatal("accepted a live owner")
	}
	equal(t, busyMessage(dir), err.Error())
	oldRelease()
	equal(t, want, bytesAt(t, path))
	release()
	mustBeAbsent(t, dir)
}

func TestReusedPID(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lock := lockFor(t.TempDir())
		dir := filepath.Join(lock.StateDir, "lifecycle.lock")
		old := lock
		old.NewToken = func() string { return previous }
		old.Processes = lookup(func(context.Context, int) (lifecycle.ProcessState, string) {
			return lifecycle.Alive, "Sun Jan  1 23:59:59 2006"
		})
		oldRelease := acquire(t, old)
		release := acquire(t, lock)
		mustBeAbsent(t, filepath.Join(dir, previous))
		const want = `{"token":"00000000-0000-4000-8000-000000000000","pid":41,"startedAt":"Mon Jan  2 15:04:05 2006","host":"host"}`
		equal(t, want, bytesAt(t, filepath.Join(dir, token)))
		oldRelease()
		equal(t, want, bytesAt(t, filepath.Join(dir, token)))
		release()
	})
}

func TestUnidentifiableOwner(t *testing.T) {
	tests := map[string]struct{ files map[string]string }{
		"error: missing birth":    {map[string]string{"unknown": `{"token":"unknown","pid":41,"host":"host"}`}},
		"error: nonstring birth":  {map[string]string{"unknown": `{"token":"unknown","pid":41,"host":"host","startedAt":10}`}},
		"error: negative pid":     {map[string]string{"unknown": `{"token":"unknown","pid":-1,"host":"host","startedAt":"Mon Jan  2 15:04:05 2006"}`}},
		"error: fractional pid":   {map[string]string{"unknown": `{"token":"unknown","pid":42.5,"host":"host","startedAt":"Mon Jan  2 15:04:05 2006"}`}},
		"error: corrupt":          {map[string]string{"owner": "bad"}},
		"error: wrong filename":   {map[string]string{"owner": `{"token":"unknown","pid":42,"host":"host","startedAt":"Sun Jan  1 23:59:59 2006"}`}},
		"error: multiple entries": {map[string]string{"first": `{"token":"first","pid":42,"host":"host","startedAt":"Sun Jan  1 23:59:59 2006"}`, "second": ""}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lock := lockFor(t.TempDir())
			dir := filepath.Join(lock.StateDir, "lifecycle.lock")
			noError(t, os.Mkdir(dir, 0o700))
			for file, data := range tc.files {
				noError(t, atomicfile.WriteFile(filepath.Join(dir, file), []byte(data), 0o600))
			}
			lock.Timeout = new(time.Duration(0))
			var inspected []int
			lock.Processes = lookup(func(_ context.Context, pid int) (lifecycle.ProcessState, string) {
				inspected = append(inspected, pid)
				return lifecycle.Alive, "Mon Jan  2 15:04:05 2006"
			})
			_, err := lock.Acquire(t.Context())
			equal(t, []int{lock.PID}, inspected)
			if err == nil {
				t.Fatal("acquired an unidentifiable lock")
			}
			equal(t, busyMessage(dir), err.Error())
			for file, data := range tc.files {
				equal(t, data, bytesAt(t, filepath.Join(dir, file)))
			}
			candidates, err := filepath.Glob(dir + ".candidate-*")
			noError(t, err)
			equal(t, 0, len(candidates))
		})
	}
}

func TestPreserveUncertainProcess(t *testing.T) {
	tests := map[string]struct {
		state lifecycle.ProcessState
		birth string
	}{
		"success: unknown with a different birth":          {lifecycle.Unknown, "Tue Jan  3 01:02:03 2006"},
		"success: inaccessible identity":                   {lifecycle.Unknown, ""},
		"success: existing process without readable birth": {lifecycle.Alive, ""},
		"success: matching legacy owner":                   {lifecycle.Alive, "Mon Jan  2 15:04:05 2006"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lock := lockFor(t.TempDir())
			dir := filepath.Join(lock.StateDir, "lifecycle.lock")
			noError(t, os.Mkdir(dir, 0o700))
			path := filepath.Join(dir, previous)
			const data = `{"token":"00000000-0000-4000-8000-000000000001","pid":42,"startedAt":"Mon Jan  2 15:04:05 2006","host":"host"}`
			noError(t, atomicfile.WriteFile(path, []byte(data), 0o600))
			const timeout = 200 * time.Millisecond
			lock.Timeout = new(timeout)
			start := time.Unix(0, 0)
			now := start
			lock.Now = func() time.Time { return now }
			lock.Wait = func(_ context.Context, d time.Duration) error {
				equal(t, 50*time.Millisecond, d)
				equal(t, data, bytesAt(t, path))
				now = now.Add(d)
				if now.Sub(start) > timeout {
					t.Fatal("continued polling after deadline")
				}
				return nil
			}
			var inspected []int
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			deadline, _ := ctx.Deadline()
			lock.Processes = lookup(func(ctx context.Context, pid int) (lifecycle.ProcessState, string) {
				if d, ok := ctx.Deadline(); !ok || d.After(deadline) {
					t.Fatal("process probe escaped acquisition context deadline")
				}
				inspected = append(inspected, pid)
				if pid == lock.PID {
					return lifecycle.Alive, "Tue Jan  3 01:02:03 2006"
				}
				return tc.state, tc.birth
			})
			_, err := lock.Acquire(ctx)
			if err == nil {
				t.Fatal("displaced an owner without evidence")
			}
			equal(t, busyMessage(dir), err.Error())
			equal(t, timeout, now.Sub(start))
			equal(t, []int{lock.PID, 42, 42, 42, 42, 42}, inspected)
			equal(t, data, bytesAt(t, path))
		})
	}
}

func TestMissingOwnIdentity(t *testing.T) {
	tests := map[string]struct{ state lifecycle.ProcessState }{"error: unknown existence": {lifecycle.Unknown}, "error: live without birth": {lifecycle.Alive}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			lock := lockFor(state)
			lock.Processes = lookup(func(_ context.Context, pid int) (lifecycle.ProcessState, string) {
				equal(t, lock.PID, pid)
				return tc.state, ""
			})
			_, err := lock.Acquire(t.Context())
			if err == nil {
				t.Fatal("accepted missing own birth")
			}
			equal(t, "could not establish lifecycle lock process identity; run doctor", err.Error())
			mustBeAbsent(t, filepath.Join(state, "lifecycle.lock"))
			mustBeAbsent(t, state)
		})
	}
}

func TestStale(t *testing.T) {
	valid := &lifecycle.Owner{PID: 17, Host: "host", StartedAt: new("Mon Jan  2 15:04:05 2006")}
	tests := map[string]struct {
		owner *lifecycle.Owner
		state lifecycle.ProcessState
		birth string
		want  bool
	}{
		"success: same process":                         {valid, lifecycle.Alive, "Mon Jan  2 15:04:05 2006", false},
		"success: recycled pid":                         {valid, lifecycle.Alive, "Tue Jan  3 01:02:03 2006", true},
		"success: confirmed absent":                     {valid, lifecycle.Absent, "", true},
		"success: unknown process with different birth": {valid, lifecycle.Unknown, "Tue Jan  3 01:02:03 2006", false},
		"success: unknown process":                      {valid, lifecycle.Unknown, "", false},
		"success: unreadable birth":                     {valid, lifecycle.Alive, "", false},
		"success: another host":                         {&lifecycle.Owner{PID: 17, Host: "foreign"}, lifecycle.Absent, "", false},
		"success: foreign identifiable owner":           {&lifecycle.Owner{PID: 17, Host: "foreign", StartedAt: new("Mon Jan  2 15:04:05 2006")}, lifecycle.Absent, "", false},
		"success: invalid pid":                          {&lifecycle.Owner{PID: 0, Host: "host"}, lifecycle.Absent, "", false},
		"success: zero pid with birth":                  {&lifecycle.Owner{PID: 0, Host: "host", StartedAt: new("Mon Jan  2 15:04:05 2006")}, lifecycle.Absent, "", false},
		"success: missing owner":                        {nil, lifecycle.Absent, "", false},
		"success: missing birth":                        {&lifecycle.Owner{PID: 17, Host: "host"}, lifecycle.Absent, "", false},
		"success: empty recorded string":                {&lifecycle.Owner{PID: 17, Host: "host", StartedAt: new("")}, lifecycle.Alive, "Mon Jan  2 15:04:05 2006", true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			calls := 0
			observed := lifecycle.Stale(t.Context(), tc.owner, "host", lookup(func(_ context.Context, pid int) (lifecycle.ProcessState, string) {
				calls++
				equal(t, 17, pid)
				return tc.state, tc.birth
			}))
			equal(t, tc.want, observed)
			if tc.owner == nil || tc.owner.PID <= 0 || tc.owner.Host != "host" || tc.owner.StartedAt == nil {
				equal(t, 0, calls)
			} else {
				equal(t, 1, calls)
			}
		})
	}
}

func TestPreparedDirectory(t *testing.T) {
	lock := lockFor(t.TempDir())
	dir := filepath.Join(lock.StateDir, "lifecycle.lock")
	noError(t, os.Mkdir(dir, 0o700))
	old := filepath.Join(dir, "old")
	noError(t, atomicfile.WriteFile(old, []byte(`{"token":"old","pid":42,"host":"host","startedAt":"Sun Jan  1 23:59:59 2006"}`), 0o600))
	var prepared fs.FileInfo
	var events []string
	lock.Processes = lookup(func(_ context.Context, pid int) (lifecycle.ProcessState, string) {
		switch pid {
		case 41:
			events = append(events, "self")
			paths, err := filepath.Glob(dir + ".candidate-*")
			noError(t, err)
			equal(t, 0, len(paths))
			return lifecycle.Alive, "Mon Jan  2 15:04:05 2006"
		case 42:
			events = append(events, "contended")
			paths, err := filepath.Glob(dir + ".candidate-*")
			noError(t, err)
			equal(t, 1, len(paths))
			prepared, err = os.Stat(paths[0])
			noError(t, err)
			equal(t, fs.FileMode(0o700), prepared.Mode().Perm())
			path := filepath.Join(paths[0], token)
			equal(t, `{"token":"00000000-0000-4000-8000-000000000000","pid":41,"startedAt":"Mon Jan  2 15:04:05 2006","host":"host"}`, bytesAt(t, path))
			info, err := os.Stat(path)
			noError(t, err)
			equal(t, fs.FileMode(0o600), info.Mode().Perm())
			return lifecycle.Absent, ""
		default:
			t.Fatalf("unexpected pid: %d", pid)
			return lifecycle.Unknown, ""
		}
	})
	lock.Wait = func(_ context.Context, delay time.Duration) error {
		events = append(events, "wait")
		equal(t, 50*time.Millisecond, delay)
		mustBeAbsent(t, dir)
		return nil
	}
	release := acquire(t, lock)
	defer release()
	equal(t, []string{"self", "contended", "wait"}, events)
	info, err := os.Stat(dir)
	noError(t, err)
	if prepared == nil || !os.SameFile(prepared, info) {
		t.Fatal("installed directory is not the prepared directory")
	}
	paths, err := filepath.Glob(dir + ".candidate-*")
	noError(t, err)
	equal(t, 0, len(paths))
}

func TestEmptyLockDirectory(t *testing.T) {
	lock := lockFor(t.TempDir())
	lock.Timeout = new(time.Duration(0))
	dir := filepath.Join(lock.StateDir, "lifecycle.lock")
	noError(t, os.Mkdir(dir, 0o700))
	release, err := lock.Acquire(t.Context())
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Fatal("replaced an existing directory on Windows")
		}
		equal(t, busyMessage(dir), err.Error())
		return
	}
	noError(t, err)
	if release == nil {
		t.Fatal("missing release")
	}
	noError(t, atomicfile.WriteFile(filepath.Join(dir, "another-owner"), []byte("preserve"), 0o600))
	release()
	mustBeAbsent(t, filepath.Join(dir, token))
	equal(t, "preserve", bytesAt(t, filepath.Join(dir, "another-owner")))
}

func TestDefaultTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lock := lockFor(t.TempDir())
		release := acquire(t, lock)
		defer release()
		lock.NewToken = func() string { return previous }
		start := time.Now()
		_, err := lock.Acquire(t.Context())
		if err == nil || !strings.Contains(err.Error(), "lifecycle is busy") {
			t.Fatalf("busy error: %v", err)
		}
		equal(t, 15*time.Second, time.Since(start))
	})
}

func TestReentrantFailure(t *testing.T) {
	lock := lockFor(t.TempDir())
	lock.Timeout = new(time.Duration(0))
	dir := filepath.Join(lock.StateDir, "lifecycle.lock")
	failure := errors.New("operation failed")
	nested := false
	err := lock.With(t.Context(), func(ctx context.Context) error {
		return lock.With(ctx, func(context.Context) error {
			nested = true
			equal(t, `{"token":"00000000-0000-4000-8000-000000000000","pid":41,"startedAt":"Mon Jan  2 15:04:05 2006","host":"host"}`, bytesAt(t, filepath.Join(dir, token)))
			return failure
		})
	})
	if !errors.Is(err, failure) {
		t.Fatalf("operation error: %v, want %v", err, failure)
	}
	equal(t, true, nested)
	mustBeAbsent(t, dir)
}

func TestExpiredOwnership(t *testing.T) {
	lock := lockFor(t.TempDir())
	lock.Timeout = new(time.Duration(0))
	var saved context.Context
	noError(t, lock.With(t.Context(), func(ctx context.Context) error { saved = ctx; return nil }))
	if saved == nil {
		t.Fatal("missing inherited context")
	}
	lock.NewToken = func() string { return previous }
	release := acquire(t, lock)
	defer release()
	err := lock.With(saved, func(context.Context) error { t.Error("expired owner entered"); return nil })
	if err == nil {
		t.Fatal("expired ownership bypassed later owner")
	}
	equal(t, busyMessage(filepath.Join(lock.StateDir, "lifecycle.lock")), err.Error())
}
