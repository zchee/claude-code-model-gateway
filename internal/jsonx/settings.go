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
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Edit addresses an object member or array element by JSON Pointer. A nil value
// removes it. A final "-" token on an array appends Value instead.
type Edit struct {
	Path  jsontext.Pointer
	Value jsontext.Value
}

// EditSettings applies edits to a settings document, preserving untouched bytes.
// An ambiguous owned key is reported by name rather than edited.
func EditSettings(input []byte, edits ...Edit) ([]byte, error) {
	const bom = "\xef\xbb\xbf"
	doc := bytes.TrimPrefix(input, []byte(bom))
	d := settingsDecoder(doc)
	root, err := d.ReadValue()
	if err != nil {
		return nil, err
	}
	if root.Kind() != '{' {
		return nil, errors.New("settings must be a JSON object")
	}
	start := len(input) - len(doc) + int(d.InputOffset()) - len(root)
	end := start + len(root)
	if _, err = d.ReadToken(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after settings object")
	}
	for _, edit := range edits {
		if edit.Path == "" || !edit.Path.IsValid() {
			return nil, fmt.Errorf("invalid settings member pointer %q", edit.Path)
		}
		from, to, replacement, err := locateEdit(input[start:end], slices.Collect(edit.Path.Tokens()), edit.Value)
		if err != nil {
			return nil, fmt.Errorf("settings %s: %w", edit.Path, err)
		}
		if from == to && len(replacement) == 0 {
			continue
		}
		out := make([]byte, 0, len(input)+len(replacement)-(to-from))
		out = append(out, input[:start+from]...)
		out = append(out, replacement...)
		input = append(out, input[start+to:]...)
		end += len(replacement) - (to - from)
	}
	return input, nil
}

func settingsDecoder(data []byte) *jsontext.Decoder {
	return jsontext.NewDecoder(bytes.NewReader(data), jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))
}

type span struct{ start, value, end int }

// locateEdit returns the one byte range to splice; no untouched value is re-encoded.
func locateEdit(root []byte, tokens []string, value jsontext.Value) (int, int, []byte, error) {
	raw := root
	offset := 0
	for depth, name := range tokens {
		object := raw[0] == '{'
		if !object && raw[0] != '[' {
			return 0, 0, nil, errors.New("parent is not a container")
		}
		last := depth == len(tokens)-1
		index := -1
		var err error
		if !object && name != "-" {
			index, err = strconv.Atoi(name)
			if err != nil || index < 0 || strconv.Itoa(index) != name {
				return 0, 0, nil, fmt.Errorf("invalid array index %q", name)
			}
		}
		units := utf16.Encode([]rune(name))
		d := settingsDecoder(raw)
		if _, err = d.ReadToken(); err != nil {
			return 0, 0, nil, err
		}
		var items []span
		found := -1
		for d.PeekKind() != '}' && d.PeekKind() != ']' {
			var item span
			match := len(items) == index
			if object {
				key, err := d.ReadValue()
				if err != nil {
					return 0, 0, nil, err
				}
				item.start = int(d.InputOffset()) - len(key)
				match = slices.Equal(readUnits(key), units)
			}
			elem, err := d.ReadValue()
			if err != nil {
				return 0, 0, nil, err
			}
			item.end = int(d.InputOffset())
			item.value = item.end - len(elem)
			if !object {
				item.start = item.value
			}
			if match {
				if found != -1 {
					return 0, 0, nil, fmt.Errorf("duplicate owned key %q", name)
				}
				found = len(items)
			}
			items = append(items, item)
		}
		if !last {
			if found < 0 {
				return 0, 0, nil, fmt.Errorf("parent member %q does not exist", name)
			}
			p := items[found]
			offset += p.value
			raw = raw[p.value:p.end]
			continue
		}
		if !object && found < 0 && (name != "-" || value == nil) {
			return 0, 0, nil, fmt.Errorf("array index %q does not exist", name)
		}
		if value == nil {
			if found < 0 {
				return 0, 0, nil, nil
			}
			p := items[found]
			from, to := p.start, p.end
			if found+1 < len(items) {
				to = items[found+1].start
			} else if found > 0 {
				from = items[found-1].end
			}
			return offset + from, offset + to, nil, nil
		}
		indent := strings.Repeat("  ", depth+1)
		formatted := value.Clone()
		if err := formatted.Indent(jsontext.WithIndent("  "), jsontext.WithIndentPrefix(indent)); err != nil {
			return 0, 0, nil, err
		}
		if found >= 0 {
			p := items[found]
			return offset + p.value, offset + p.end, formatted, nil
		}
		at := 1
		var added []byte
		if len(items) > 0 {
			at = items[len(items)-1].end
			added = append(added, ',')
		}
		added = append(added, '\n')
		added = append(added, indent...)
		if object {
			added, err = jsontext.AppendQuote(added, name)
			if err != nil {
				return 0, 0, nil, err
			}
			added = append(added, ':', ' ')
		}
		added = append(added, formatted...)
		if !bytes.Contains(raw[at:len(raw)-1], []byte("\n")) {
			added = append(added, '\n')
			added = append(added, indent[:len(indent)-2]...)
		}
		return offset + at, offset + at, added, nil
	}
	return 0, 0, nil, errors.New("missing settings member")
}
