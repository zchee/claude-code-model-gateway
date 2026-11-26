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

package config

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestJSTrim(t *testing.T) {
	const bom = string(rune(0xfeff))
	const nel = string(rune(0x85))
	tests := map[string]struct{ input, want string }{
		"success: byte order mark":               {input: bom + "word" + bom, want: "word"},
		"success: next line is retained":         {input: nel + "word" + nel, want: nel + "word" + nel},
		"success: line and paragraph separators": {input: string(rune(0x2028)) + "word" + string(rune(0x2029)), want: "word"},
		"success: empty":                         {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, JSTrim(tt.input)); diff != "" {
				t.Error(diff)
			}
		})
	}
}
