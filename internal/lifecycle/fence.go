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

import "context"

// StartFence serializes worker process starts with intentional stop and resume in one state root.
type StartFence struct{}

// unlockFence is the unlock call of a fence release, given the descriptor that holds the lock. It is a variable so
// that a test can make the unlock fail and observe that the release still closes the descriptor exactly once.
var unlockFence = func(fd int) error {
	panic("not implemented")
}

// NewStartFence validates a clean absolute state root without acquiring a lock.
func NewStartFence(stateDir string) (*StartFence, error) {
	panic("not implemented")
}

// Acquire takes the root's advisory fence or returns a context or filesystem error without a lease.
func (f *StartFence) Acquire(ctx context.Context) (release func() error, err error) {
	panic("not implemented")
}
