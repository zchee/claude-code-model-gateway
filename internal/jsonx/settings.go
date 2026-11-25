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

// Package jsonx edits gateway-owned settings without rewriting unrelated JSON.
package jsonx

import (
	"encoding/json/jsontext"
	"errors"
)

var errNotImplemented = errors.New("authored settings editing not implemented")

// Edit addresses an object member or array element by JSON Pointer. A nil value
// removes it. A final "-" token on an array appends Value instead.
type Edit struct {
	Path  jsontext.Pointer
	Value jsontext.Value
}

// EditSettings applies edits to a settings document, preserving untouched bytes.
// An ambiguous owned key is reported by name rather than edited.
func EditSettings(input []byte, edits ...Edit) ([]byte, error) {
	return nil, errNotImplemented
}
