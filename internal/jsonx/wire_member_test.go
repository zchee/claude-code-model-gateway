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

package jsonx_test

import (
	"errors"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// objectWithMemberAtDepth wraps an inner object in arrays until that inner object sits at depth open containers
// in total and places member where a member name is required.
func objectWithMemberAtDepth(depth int, member string) []byte {
	return []byte(strings.Repeat("[", depth-1) + "{" + member + ":1}" + strings.Repeat("]", depth-1))
}

// TestDecodeWireMalformedMemberName checks that a non-string token in an object member-name position is rejected
// before reading a value there, so it never triggers a container-depth error unless the enclosing object itself
// already crossed the nesting limit.
func TestDecodeWireMalformedMemberName(t *testing.T) {
	const limit = 10000
	tests := map[string]struct {
		input         []byte
		wantDepth     bool
		wantMemberErr bool
	}{
		"error: array opener as member name at the nesting limit":           {objectWithMemberAtDepth(limit, "[0]"), false, true},
		"error: object opener as member name at the nesting limit":          {objectWithMemberAtDepth(limit, `{"k":0}`), false, true},
		"error: number as member name at the nesting limit":                 {objectWithMemberAtDepth(limit, "0"), false, true},
		"error: literal as member name at the nesting limit":                {objectWithMemberAtDepth(limit, "true"), false, true},
		"error: array opener as second member name at the nesting limit":    {[]byte(strings.Repeat("[", limit-1) + `{"k":0,[0]:1}` + strings.Repeat("]", limit-1)), false, true},
		"error: truncated array opener as member name at the nesting limit": {[]byte(strings.Repeat("[", limit-1) + `{[`), false, true},
		"error: over-limit array in a top-level member name":                {[]byte(`{` + strings.Repeat("[", limit+1) + strings.Repeat("]", limit+1) + `:1}`), false, true},
		"error: array opener as member name past the nesting limit":         {objectWithMemberAtDepth(limit+1, "[0]"), true, false},
		"error: object opener as member name past the nesting limit":        {objectWithMemberAtDepth(limit+1, `{"k":0}`), true, false},
		"error: number as member name past the nesting limit":               {objectWithMemberAtDepth(limit+1, "0"), true, false},
		"error: literal as member name past the nesting limit":              {objectWithMemberAtDepth(limit+1, "true"), true, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := jsonx.DecodeWire(tt.input)
			if err == nil {
				t.Fatal("accepted object with non-string member name")
			}
			_, gotDepth := errors.AsType[*jsonx.WireDepthError](err)
			if diff := gocmp.Diff(tt.wantDepth, gotDepth); diff != "" {
				t.Errorf("WireDepthError (-want +got) for err=%v:\n%s", err, diff)
			}
			if tt.wantMemberErr {
				if diff := gocmp.Diff("expected string for object member name", err.Error()); diff != "" {
					t.Errorf("error message (-want +got):\n%s", diff)
				}
			}
		})
	}
}
