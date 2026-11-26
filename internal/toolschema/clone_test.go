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

package toolschema

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestClonedSchemaGraph(t *testing.T) {
	tests := map[string]struct {
		schema string
		self   bool
	}{
		"success: back edge through data": {`{"pattern":"[^x\\p{L}]","default":null,"examples":null}`, true},
		"success: shared empty object":    {`{"pattern":"[^x\\p{L}]","default":{},"examples":null}`, false},
		"success: shared empty array":     {`{"pattern":"[^x\\p{L}]","default":[],"examples":null}`, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			original := decode(t, []byte(`[{"input_schema":`+tc.schema+`}]`))
			markEmpty(&original)
			schema := schemaOf(t, original)
			first := member(t, *schema, "default")
			if tc.self {
				*first = *schema
			}
			*member(t, *schema, "examples") = *first
			result, edits, err := Adapt(original)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(1, len(edits)); diff != "" {
				t.Fatal(diff)
			}
			cloned := schemaOf(t, result)
			a, b := member(t, *cloned, "default"), member(t, *cloned, "examples")
			if !identical(*a, *b) || identical(*a, *first) || identical(*a, *cloned) != tc.self {
				t.Fatal("clone did not preserve graph identity independently of its input")
			}
		})
	}
}
