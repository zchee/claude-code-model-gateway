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

package pins_test

import (
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/pins"
)

func TestNormalizationEdges(t *testing.T) {
	tests := map[string]struct{ input, want string }{
		"success: trim ECMAScript byte order mark":               {string(rune(0xfeff)) + "claude-opus-6" + string(rune(0xfeff)), "claude-opus-6[1m]"},
		"error: next line is not trim whitespace":                {"claude-opus-6\u0085", ""},
		"error: do not remove a marker assembled by replacement": {"claude-opus-6[[1M]1m]", ""},
		"error: case insensitive pattern is ASCII":               {"claude-opus-K", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := pins.NormalizeDetected("opus", tt.input)
			equal(t, tt.want, got)
			equal(t, tt.want != "", ok)
		})
	}
}

func TestPinDisplayName(t *testing.T) {
	tests := map[string]struct{ id, want string }{
		"success: uppercase may expand":                      {"claude-ß-2-0", "SS 2.0"},
		"success: first astral letter stays lowercase":       {"claude-𐐨-2", "𐐨 2"},
		"success: malformed negative minor retains spelling": {"claude-opus-5--3", "claude-opus-5--3"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { equal(t, tt.want, pins.DisplayName(tt.id)) })
	}
}
