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

package ownership_test

import (
	"reflect"
	"slices"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/ownership"
)

// TestHandleNotCopyable decides that a Handle holds an unexported sync.Mutex by value, so that go vet's copylocks
// check reports a copy of a Handle value: a nonzero Handle returned by OpenHandle must not be copied. A lock behind
// a pointer, or a locker type of its own, does not make vet report the copy and does not count.
func TestHandleNotCopyable(t *testing.T) {
	tests := map[string]struct {
		typ  reflect.Type
		want int
	}{
		"success: a handle value holds one unexported mutex by value": {typ: reflect.TypeFor[ownership.Handle](), want: 1},
	}
	mutex := reflect.TypeFor[sync.Mutex]()
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var fields []string
			mutexes := 0
			for f := range tt.typ.Fields() {
				fields = append(fields, f.Name+" "+f.Type.String())
				if f.Type == mutex && !f.IsExported() {
					mutexes++
				}
			}
			if mutexes != tt.want {
				t.Errorf("%v holds %d unexported sync.Mutex value fields (fields %v); want %d", tt.typ, mutexes, fields, tt.want)
			}
		})
	}
}

// TestHandleMethods decides that a handle offers no signal but the terminate: its only methods are Terminate and
// Close.
func TestHandleMethods(t *testing.T) {
	tests := map[string]struct {
		typ  reflect.Type
		want []string
	}{
		"success: the pointer offers only terminate and close": {typ: reflect.TypeFor[*ownership.Handle](), want: []string{"Close", "Terminate"}},
		"success: the value offers no method":                  {typ: reflect.TypeFor[ownership.Handle](), want: nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var got []string
			for m := range tt.typ.Methods() {
				got = append(got, m.Name)
			}
			slices.Sort(got)
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("methods of %v (-want +got):\n%s", tt.typ, diff)
			}
		})
	}
}
