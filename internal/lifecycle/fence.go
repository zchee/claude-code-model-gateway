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

package lifecycle

import "io/fs"

// fenceName is the start fence's lock file in the state root. It is created once and never truncated, renamed or
// removed, so every holder locks the same file; its presence says nothing about whether it is locked.
const fenceName = "worker-start.lock"

// StartFence serializes worker process starts with intentional stop and resume in one state root.
type StartFence struct{ stateDir string }

// unlockFence is the unlock call of a fence release, given the descriptor that holds the lock. It is a variable so
// that a test can make the unlock fail and observe that the release still closes the descriptor exactly once.
var unlockFence = func(fd int) error {
	return unlockFenceDescriptor(fd)
}

// NewStartFence validates a clean absolute state root without acquiring a lock.
func NewStartFence(stateDir string) (*StartFence, error) {
	if !validStateDir(stateDir) {
		return nil, &fs.PathError{Op: "start fence", Path: stateDir, Err: fs.ErrInvalid}
	}
	return &StartFence{stateDir: stateDir}, nil
}
