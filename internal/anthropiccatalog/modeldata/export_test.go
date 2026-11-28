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

package modeldata

import (
	"os"
	"testing"
)

// MdatTReplaceOpenInput installs, until the test ends, the open call wrap returns when given the one
// Read used before. Tests that use it must not run in parallel with other Read tests.
func MdatTReplaceOpenInput(t testing.TB, wrap func(previous func(name string, flag int, perm os.FileMode) (*os.File, error)) func(name string, flag int, perm os.FileMode) (*os.File, error)) {
	t.Helper()
	previous := openInput
	openInput = wrap(previous)
	t.Cleanup(func() { openInput = previous })
}
