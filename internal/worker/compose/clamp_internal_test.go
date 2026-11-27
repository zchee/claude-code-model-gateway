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

package compose

import (
	"math"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestClampIntBoundary decides where the conversion of count and byte settings saturates: at the
// platform's largest int, never at the 32-bit maximum. Telling the two apart through a setting would
// take more than 2 GiB of decoded body or 2^31 upstream attempts, so the conversion is tested here
// directly. Expectations are int64, so the table also compiles for a 32-bit int, where the platform
// maximum is the 32-bit one.
func TestClampIntBoundary(t *testing.T) {
	maxInt := int64(math.MaxInt)
	tests := map[string]struct {
		in   float64
		want int64
	}{
		"success: zero stays zero": {
			in:   0,
			want: 0,
		},
		"success: the 32-bit signed maximum keeps its value": {
			in:   math.MaxInt32,
			want: math.MaxInt32,
		},
		"success: one above the 32-bit signed maximum is not held at that maximum": {
			in:   math.MaxInt32 + 1,
			want: min(math.MaxInt32+1, maxInt),
		},
		"success: the 32-bit unsigned maximum keeps its value": {
			in:   math.MaxUint32,
			want: min(math.MaxUint32, maxInt),
		},
		"success: the largest exact JavaScript integer keeps its value": {
			in:   1<<53 - 1,
			want: min(1<<53-1, maxInt),
		},
		"success: the float nearest the largest int saturates instead of wrapping": {
			in:   float64(math.MaxInt),
			want: maxInt,
		},
		"success: positive infinity saturates": {
			in:   math.Inf(1),
			want: maxInt,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, int64(clampInt(tt.in))); diff != "" {
				t.Errorf("clampInt(%v) (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}
