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

//go:build !unix

package ownership_test

import (
	"math"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// TestHandleUnsupported decides that a platform without Unix signals refuses every positive process number with an
// error matching errors.ErrUnsupported, refuses a number that is not positive as invalid, and returns no handle. This
// platform has no open or delivery system call a handle could reach.
func TestHandleUnsupported(t *testing.T) {
	tests := map[string]struct {
		pid  int
		want string
	}{
		"error: process number one is unsupported":           {pid: 1, want: "errors.ErrUnsupported"},
		"error: a large process number is unsupported":       {pid: math.MaxInt32, want: "errors.ErrUnsupported"},
		"error: the largest process number is unsupported":   {pid: math.MaxInt, want: "errors.ErrUnsupported"},
		"error: process number zero is invalid":              {pid: 0, want: "fs.ErrInvalid"},
		"error: process number minus one is invalid":         {pid: -1, want: "fs.ErrInvalid"},
		"error: the most negative process number is invalid": {pid: math.MinInt, want: "fs.ErrInvalid"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h, err := handleTOpenRaw(t, tt.pid)
			if h != nil {
				t.Errorf("OpenHandle(%d) returned a handle beside %v; closing it: %v", tt.pid, err, guarded("Handle.Close", h.Close))
			}
			if diff := gocmp.Diff(tt.want, handleTMatch(err)); diff != "" {
				t.Errorf("OpenHandle(%d) error %v (-want +got):\n%s", tt.pid, err, diff)
			}
		})
	}
}
