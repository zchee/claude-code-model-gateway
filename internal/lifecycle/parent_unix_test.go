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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/lifecycle"
)

func TestRetargetedStateDirectory(t *testing.T) {
	base := t.TempDir()
	state, moved := filepath.Join(base, "state"), filepath.Join(base, "moved")
	ownerDir := filepath.Join(state, "lifecycle.lock")
	noError(t, os.MkdirAll(ownerDir, 0o700))
	const old = `{"token":"old","pid":19,"startedAt":"birth","host":"host"}`
	noError(t, atomicfile.WriteFile(filepath.Join(ownerDir, "old"), []byte(old), 0o600))
	lock := lockFor(state)
	lock.Processes = lookup(func(_ context.Context, pid int) (lifecycle.ProcessState, string) {
		if pid == lock.PID {
			return lifecycle.Alive, "own birth"
		}
		equal(t, 19, pid)
		noError(t, os.Rename(state, moved))
		noError(t, os.MkdirAll(ownerDir, 0o700))
		noError(t, atomicfile.WriteFile(filepath.Join(ownerDir, "old"), []byte("preserve"), 0o600))
		return lifecycle.Absent, ""
	})
	lock.Wait = func(context.Context, time.Duration) error { return nil }
	release := acquire(t, lock)
	noError(t, atomicfile.WriteFile(filepath.Join(ownerDir, token), []byte("new owner"), 0o600))
	release()
	mustBeAbsent(t, filepath.Join(moved, "lifecycle.lock"))
	equal(t, "preserve", bytesAt(t, filepath.Join(ownerDir, "old")))
	equal(t, "new owner", bytesAt(t, filepath.Join(ownerDir, token)))
}
