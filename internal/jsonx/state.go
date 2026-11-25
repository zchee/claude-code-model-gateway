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

package jsonx

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"io/fs"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

// EncodeState serializes a typed state value with two-space indentation and one
// final newline. For insertion-ordered objects use structs or WireValue; Go maps
// have no insertion order and use a deterministic key order instead.
func EncodeState(value any) ([]byte, error) {
	tree, ok := value.(WireValue)
	if !ok {
		floats := json.JoinMarshalers(
			json.MarshalFunc(func(n float64) ([]byte, error) { return appendNumber(nil, n), nil }),
			json.MarshalFunc(func(n float32) ([]byte, error) { return appendNumber(nil, float64(n)), nil }),
		)
		data, err := json.Marshal(value, json.WithMarshalers(floats), json.Deterministic(true), jsontext.PreserveRawStrings(true), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
		if err != nil {
			return nil, err
		}
		tree, err = DecodeWire(data)
		if err != nil {
			return nil, err
		}
	}
	data, err := EncodeWire(tree)
	if err != nil {
		return nil, err
	}
	pretty := jsontext.Value(data)
	if err := pretty.Indent(jsontext.WithIndent("  ")); err != nil {
		return nil, err
	}
	return append(pretty, '\n'), nil
}

// WriteState atomically replaces path with the encoded state using mode on creation.
func WriteState(path string, value any, mode fs.FileMode) error {
	data, err := EncodeState(value)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, mode)
}
