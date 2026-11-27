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

//go:build process && (linux || darwin)

package workerlink_test

import (
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/workerlink"
)

// TestFixtureHeldDescriptorSeenByChild checks heldInheritedListener through a child process: after the file's Close
// the held descriptor is still open and inheritable, so a child started then finds the listener's port.
func TestFixtureHeldDescriptorSeenByChild(t *testing.T) {
	holdLowDescriptors(t)
	h := holdInheritedListener(t)
	if err := h.file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if found := leakedInto(childSockets(t), int(h.port), ""); len(found) == 0 {
		t.Errorf("a child started after the file's Close did not inherit the held descriptor at port %d; the fixture cannot show a leak", h.port)
	}
}

// TestImportListenerRefusalNotInherited checks a refused hand-off through a child process: the import refuses the
// inherited listener and closes its file, the test keeps the descriptor open past that Close (see
// heldInheritedListener), and a child started then must not inherit it. A zero expected port is refused like a wrong
// one, and neither may leave the descriptor inheritable.
func TestImportListenerRefusalNotInherited(t *testing.T) {
	tests := map[string]struct {
		expected func(port uint16) uint16
	}{
		"error: expected port is zero":    {expected: func(uint16) uint16 { return 0 }},
		"error: listener on another port": {expected: otherPort},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			holdLowDescriptors(t)
			h := holdInheritedListener(t)
			l, err := workerlink.ImportListener(h.file, tt.expected(h.port))
			if l != nil {
				_ = l.Close()
				t.Errorf("ImportListener returned listener %v; a refused hand-off must return none", l.Addr())
			}
			if err == nil {
				t.Fatal("ImportListener accepted the descriptor")
			}
			checkConsumed(t, h.file)
			if found := leakedInto(childSockets(t), int(h.port), ""); len(found) != 0 {
				t.Errorf("a child started after the refused hand-off inherited its descriptor: %v", found)
			}
		})
	}
}
