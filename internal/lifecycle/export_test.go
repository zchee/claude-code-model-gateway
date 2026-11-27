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

import "testing"

// ReplaceFenceUnlock installs unlock as the unlock call of every fence release until the test ends, and returns
// the call it replaced. Tests that use it must not run in parallel with other fence tests.
func ReplaceFenceUnlock(t testing.TB, unlock func(fd int) error) (previous func(fd int) error) {
	t.Helper()
	previous = unlockFence
	unlockFence = unlock
	t.Cleanup(func() { unlockFence = previous })
	return previous
}
