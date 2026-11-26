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
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInstallAfterStateDirectoryMoves(t *testing.T) {
	base := t.TempDir()
	state, moved := filepath.Join(base, "state"), filepath.Join(base, "moved")
	lock := lockFor(state)
	// The deadline is sampled after the parent is open but before installation. Moving
	// it here catches installation or release by path, not creation by path followed
	// by holding the descriptor. Acquire uses a single OpenDirectoryFile call;
	// atomicfile's TestOpenDirCreateMovedAncestor checks the walk between components.
	lock.Now = func() time.Time {
		noError(t, os.Rename(state, moved))
		noError(t, os.Mkdir(state, 0o700))
		return time.Unix(0, 0)
	}
	release := acquire(t, lock)
	t.Cleanup(release)
	path := filepath.Join(moved, "lifecycle.lock", token)
	equal(t, `{"token":"00000000-0000-4000-8000-000000000000","pid":41,"startedAt":"Mon Jan  2 15:04:05 2006","host":"host"}`, bytesAt(t, path))
	entries, err := os.ReadDir(state)
	noError(t, err)
	equal(t, 0, len(entries))
	release()
	mustBeAbsent(t, filepath.Join(moved, "lifecycle.lock"))
	entries, err = os.ReadDir(state)
	noError(t, err)
	equal(t, 0, len(entries))
}
