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

package procinfo

import (
	"errors"
	"testing"
)

// validateRecord calls Record.Validate and turns a panic into a test failure, so that an unimplemented method cannot
// abort the package's other tests.
func validateRecord(t *testing.T, r Record, root string) (err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Record.Validate panicked: %v", p)
		}
	}()
	return r.Validate(root)
}

// TestRecordValidate decides that the exported validation gives the answers the record reader and writer give
// (TestReadRecord and TestRecordValidation): the fixture record is valid, and each start text they refuse is refused
// with ErrStartedAt.
func TestRecordValidate(t *testing.T) {
	tests := map[string]struct {
		// start, when set, replaces the fixture's start text.
		start string
		want  error
	}{
		"success: the record the reader returns is valid":    {},
		"error: a start text whose weekday disagrees":        {start: "Sun Feb  3 22:15:09 2001", want: ErrStartedAt},
		"error: a start text whose day is not padded":        {start: "Sat Feb 3 22:15:09 2001", want: ErrStartedAt},
		"error: a start text that is not a timestamp at all": {start: "unknown", want: ErrStartedAt},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			r, _ := recordFixture(root)
			if tt.start != "" {
				r.StartedAt = tt.start
			}
			err := validateRecord(t, r, root)
			if !errors.Is(err, tt.want) || (err == nil) != (tt.want == nil) {
				t.Errorf("Validate(%q) = %v; want %v", root, err, tt.want)
			}
		})
	}
}
