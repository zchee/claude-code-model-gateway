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
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/pins"
)

func TestVersionFieldLimits(t *testing.T) {
	digits := strings.Repeat("9", 100_000)
	tests := map[string]struct{ id string }{
		"error: malformed family": {"claude-" + string([]byte{0xff}) + "-1"},
		"error: oversized major":  {"claude-opus-" + digits},
		"error: oversized minor":  {"claude-opus-1-" + digits},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { _, ok := pins.ParseVersion(tt.id); equal(t, false, ok) })
	}
}

func TestCompareSigmaLookahead(t *testing.T) {
	// JavaScript sees the following B across all the combining marks and returns +1.
	// The port's bounded casing context produces final sigma and unrelated families.
	marks := strings.Repeat(string(rune(0x0301)), 31)
	_, ok := pins.Compare("claude-AΣ"+marks+"B-6", "claude-aσ"+marks+"b-5")
	equal(t, false, ok)
}

func TestFollowCatalogOwnsDetection(t *testing.T) {
	in := pins.Inputs{Cache: pins.Cache{Detected: map[string]string{"opus": "claude-opus-6[1m]"}, DetectedFor: map[string]string{"opus": "v1"}}}
	result, _ := in.FollowCatalog()
	result.Detected["opus"] = "other"
	result.DetectedFor["opus"] = "v2"
	equal(t, "claude-opus-6[1m]", in.Cache.Detected["opus"])
	equal(t, "v1", in.Cache.DetectedFor["opus"])
}
