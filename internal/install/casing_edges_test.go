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

package install

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestComparisonKeySigmaLookahead(t *testing.T) {
	marks := strings.Repeat("́", 31)
	prefix := "/" + strings.Repeat("a", 127)
	tests := map[string]struct{ input, want string }{
		// JavaScript looks through all combining marks and uses σ, not ς.
		"success: bounded final sigma lookahead": {"ΟΣ" + marks + "A", "ος" + marks + "a"},
		// JavaScript retains the word context and uses final sigma (ς).
		"success: sigma at chunk boundary": {prefix + "Σ", prefix + "σ"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, normalPath(tt.input)); diff != "" {
				t.Errorf("normalPath (-want +got):\n%s", diff)
			}
		})
	}
}
