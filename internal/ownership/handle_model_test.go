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

package ownership

import (
	"errors"
	"os"
	"testing"
)

// handleTNoRealPath installs an empty model for a row whose handle is built on a fake reference, and fails the row if
// anything reached the open or delivery system calls.
func handleTNoRealPath(t *testing.T) {
	t.Helper()
	m := handleTInstallModel(t, &handleTModel{})
	t.Cleanup(func() {
		if opens, deliveries := m.calls(); opens != nil || deliveries != nil {
			t.Errorf("the handle reached the system calls: opens %v, deliveries %v", opens, deliveries)
		}
	})
}

// handleTKind says which of the handle's sentinels err matches, so that a row compares one literal per call.
func handleTKind(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, os.ErrProcessDone):
		return "os.ErrProcessDone"
	case errors.Is(err, os.ErrClosed):
		return "os.ErrClosed"
	}
	return "unexpected: " + err.Error()
}
