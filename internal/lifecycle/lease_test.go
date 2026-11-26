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
	"math"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

func TestAdmittedCallbackHoldsLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lock := lockFor(t.TempDir())
		lock.Timeout = new(time.Duration(0))
		entered, finish := make(chan context.Context, 1), make(chan struct{})
		outerDone, nestedDone := make(chan error, 1), make(chan error, 1)
		go func() {
			outerDone <- lock.With(t.Context(), func(ctx context.Context) error {
				started := make(chan struct{})
				go func() {
					nestedDone <- lock.With(ctx, func(nested context.Context) error {
						entered <- nested
						close(started)
						<-finish
						return nil
					})
				}()
				<-started
				return nil
			})
		}()
		ctx := <-entered
		synctest.Wait() // The outer callback has returned and only the nested callback is active.
		other := lock
		other.NewToken = func() string { return previous }
		if release, err := other.Acquire(t.Context()); err == nil {
			release()
			t.Error("second acquirer entered before the admitted nested callback finished")
		}
		if err := lock.With(ctx, func(context.Context) error { t.Error("admitted during release"); return nil }); err == nil {
			t.Error("expired nested context returned success")
		}
		close(finish)
		noError(t, <-nestedDone)
		noError(t, <-outerDone)
		release := acquire(t, other)
		release()
	})
}

func TestLeaseScope(t *testing.T) {
	tests := map[string]struct{ change func(*lifecycle.Lock) }{
		"error: different directory": {func(l *lifecycle.Lock) { l.StateDir = filepath.Join(l.StateDir, "other") }},
		"error: different owner":     {func(l *lifecycle.Lock) { l.PID++ }},
		"error: different host":      {func(l *lifecycle.Lock) { l.Host += "-other" }},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			lock := lockFor(t.TempDir())
			lock.Timeout = new(time.Duration(0))
			other := lock
			tc.change(&other)
			var saved context.Context
			noError(t, lock.With(t.Context(), func(ctx context.Context) error {
				saved = context.WithoutCancel(ctx)
				if err := other.With(saved, func(context.Context) error { t.Error("foreign context entered"); return nil }); err == nil {
					t.Error("foreign context returned success")
				}
				return nil
			}))
			if err := lock.With(saved, func(context.Context) error { t.Error("expired context reacquired an empty lock"); return nil }); err == nil {
				t.Error("expired context returned success")
			}
		})
	}
}

func TestUnavailableOwnerEvidence(t *testing.T) {
	tests := map[string]struct {
		pid       int
		processes lifecycle.ProcessSource
	}{
		"success: no process source": {41, nil},
		"success: outside pid range": {math.MaxInt32 + 1, lookup(func(context.Context, int) (lifecycle.ProcessState, string) {
			t.Error("probed invalid pid")
			return lifecycle.Absent, ""
		})},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			owner := lifecycle.Owner{PID: tc.pid, Host: "host", StartedAt: new("birth")}
			equal(t, false, lifecycle.Stale(t.Context(), &owner, "host", tc.processes))
		})
	}
}

func TestRetargetedJournalAppend(t *testing.T) {
	f := filesFor(t.TempDir())
	logs, moved := filepath.Join(f.StateDir, "logs"), filepath.Join(f.StateDir, "moved")
	f.Now = func() time.Time {
		noError(t, os.Rename(logs, moved))
		noError(t, os.Mkdir(logs, 0o700))
		return time.Unix(0, 0)
	}
	f.Record("tick", map[string]any{"child": math.Inf(1)})
	mustBeAbsent(t, filepath.Join(logs, "lifecycle.jsonl"))
	equal(t, `{"at":"1970-01-01T00:00:00.000Z","event":"tick","child":null}`+"\n", bytesAt(t, filepath.Join(moved, "lifecycle.jsonl")))
}

func TestUnrequestedFailureGeneration(t *testing.T) {
	f := filesFor(t.TempDir())
	noError(t, atomicfile.WriteFile(filepath.Join(f.StateDir, "shim-supervisor-failure.txt"), []byte(`{"supervisorPid":41,"generation":{},"message":"failure"}`), 0o600))
	msg, ok := f.StartupFailure(41, "")
	equal(t, true, ok)
	equal(t, "failure", msg)
	_, ok = f.StartupFailure(41, "specific")
	equal(t, false, ok)
}
