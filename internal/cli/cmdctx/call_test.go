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

package cmdctx_test

import (
	"context"
	"io"
	"reflect"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/argscan"
	"github.com/zchee/claude-code-model-gateway/internal/cli/cmdctx"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
)

// cmdTCallType is read through reflection so that a wrong field type fails a row instead of the build.
var cmdTCallType = reflect.TypeFor[cmdctx.Call]()

// TestCallFieldTypes checks the declared type of each field of the command input. It decides which type stands in
// each place; it does not observe when a caller resolves a fact or how a command uses it.
func TestCallFieldTypes(t *testing.T) {
	tests := map[string]struct {
		field string
		want  reflect.Type
	}{
		// Catches a string (for example a resolved home or state root) or a struct of resolved paths in place of the
		// path resolver.
		"success: Paths is the path resolver": {
			field: "Paths",
			want:  reflect.TypeFor[paths.Resolver](),
		},
		// Catches a map or a list of environment entries in place of the lookup function type.
		"success: Lookup is the environment lookup": {
			field: "Lookup",
			want:  reflect.TypeFor[config.LookupEnv](),
		},
		// Catches a slice of environment entries in place of the function that lists them.
		"success: Environ is a function returning the environment list": {
			field: "Environ",
			want:  reflect.TypeFor[func() []string](),
		},
		// Catches a string, or a function without an error result, in place of the executable lookup.
		"success: Executable is a function returning a path and an error": {
			field: "Executable",
			want:  reflect.TypeFor[func() (string, error)](),
		},
		// Catches a string in place of the function that returns the version.
		"success: Version is a function returning the version": {
			field: "Version",
			want:  reflect.TypeFor[func() string](),
		},
		// Catches a string, or a function without an error result, in place of the working-directory lookup.
		"success: Cwd is a function returning a path and an error": {
			field: "Cwd",
			want:  reflect.TypeFor[func() (string, error)](),
		},
		// Catches a byte slice in place of the reader.
		"success: Stdin is a reader": {
			field: "Stdin",
			want:  reflect.TypeFor[io.Reader](),
		},
		// Catches a byte slice, or a read function without a context parameter, in place of the gate's read.
		"success: ReadInput is a read function taking a context": {
			field: "ReadInput",
			want:  reflect.TypeFor[func(context.Context) ([]byte, error)](),
		},
		// Catches a different writer set (for example a struct of separate stdout and stderr writers) in place of
		// the argument scan's streams.
		"success: Streams is the argument scan's stream set": {
			field: "Streams",
			want:  reflect.TypeFor[argscan.Streams](),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			field, ok := cmdTCallType.FieldByName(tt.field)
			if !ok {
				t.Fatalf("cmdctx.Call has no field %s; want one of type %v", tt.field, tt.want)
			}
			if field.Type != tt.want {
				t.Errorf("cmdctx.Call.%s has type %v; want %v", tt.field, field.Type, tt.want)
			}
			// An embedded alias of the same name and type would pass the type check and promote the alias's methods
			// onto Call.
			if field.Anonymous {
				t.Errorf("cmdctx.Call.%s is an embedded field; want a named field", tt.field)
			}
			if field.Tag != "" {
				t.Errorf("cmdctx.Call.%s has tag %q; want none", tt.field, field.Tag)
			}
		})
	}
}

// TestCallFieldSet checks that the input holds exactly the deferred facts, the streams and the gate's read, in the
// declared order, and nothing resolved beside them.
func TestCallFieldSet(t *testing.T) {
	tests := map[string]struct {
		want []string
	}{
		// Catches an added field holding an already resolved fact (for example a Home or StateDir string next to the
		// resolver), a removed fact, an embedded type that brings in more fields, or a reordered declaration.
		"success: exactly the declared fields in order": {
			want: []string{"Paths", "Lookup", "Environ", "Executable", "Version", "Cwd", "Stdin", "ReadInput", "Streams"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var got []string
			for field := range cmdTCallType.Fields() {
				got = append(got, field.Name)
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("cmdctx.Call fields (-want +got):\n%s", diff)
			}
		})
	}
}
