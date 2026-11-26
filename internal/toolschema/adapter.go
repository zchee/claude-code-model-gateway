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

// Package toolschema adapts tool input schemas for Codex.
package toolschema

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Edit records the RFC 6901 pointer rooted at /tools and its replacement pattern.
// Both fields contain UTF-16 code units, like jsonx.WireValue.Text.
type Edit struct {
	Pointer     []uint16 `json:"pointer"`
	Replacement []uint16 `json:"replacement"`
}

// CompatibilityError identifies a refusal. Pointer and ToolName are the original
// UTF-16 code units to use when constructing the wire error; ReasonCode is ASCII.
type CompatibilityError struct {
	ReasonCode string   `json:"reasonCode"`
	Pointer    []uint16 `json:"pointer"`
	ToolName   []uint16 `json:"toolName"`
}

// Error describes the refusal for diagnostics, escaping unpaired surrogates.
func (e *CompatibilityError) Error() string {
	return "Codex schema compatibility refused " + diagnosticText(e.ToolName) + " at " + diagnosticText(e.Pointer) + " (" + e.ReasonCode + ")"
}

func diagnosticText(text []uint16) string {
	var out []byte
	for i := 0; i < len(text); i++ {
		r := rune(text[i])
		if utf16.IsSurrogate(r) {
			if i+1 < len(text) {
				if pair := utf16.DecodeRune(r, rune(text[i+1])); pair != utf8.RuneError {
					out = utf8.AppendRune(out, pair)
					i++
					continue
				}
			}
			out = fmt.Appendf(out, `\u%04x`, r)
			continue
		}
		out = utf8.AppendRune(out, r)
	}
	return string(out)
}

// Adapt rewrites eligible tool schemas without mutating tools. Its input must use
// the object member order established by jsonx.DecodeWire. A no-op returns the
// original value. Otherwise it preserves unaffected tool identities and clones the
// entire input schema of each changed tool. Edits follow traversal order. A refusal
// returns no output or edits and identifies the first incompatible schema.
func Adapt(tools jsonx.WireValue) (jsonx.WireValue, []Edit, error) {
	if tools.Kind != '[' {
		return tools, nil, nil
	}
	type change struct {
		tool, schema int
		patterns     map[*jsonx.WireValue][]uint16
	}
	var changes []change
	var edits []Edit
	for i, tool := range tools.Elements {
		if tool.Kind != '{' {
			continue
		}
		idx := fieldIndex(tool, "input_schema")
		if idx < 0 {
			continue
		}
		v := &tool.Members[idx].Value
		if !affected(*v, make(map[containerID]bool)) {
			continue
		}
		c := classification{toolName: utf16.Encode([]rune("<unnamed tool>")), patterns: make(map[*jsonx.WireValue][]uint16), visiting: make(map[containerID]bool)}
		if n := fieldIndex(tool, "name"); n >= 0 && tool.Members[n].Value.Kind == '"' {
			c.toolName = tool.Members[n].Value.Text
		}
		c.path = utf16.Encode([]rune("/tools/" + strconv.Itoa(i) + "/input_schema"))
		if v.Kind != '{' {
			return jsonx.WireValue{}, nil, c.refuse("invalid-schema")
		}
		if err := c.visit(*v); err != nil {
			return jsonx.WireValue{}, nil, err
		}
		if len(c.edits) != 0 {
			edits = append(edits, c.edits...)
			changes = append(changes, change{i, idx, c.patterns})
		}
	}
	if len(changes) == 0 {
		return tools, nil, nil
	}
	result := tools
	result.Elements = slices.Clone(tools.Elements)
	for _, ch := range changes {
		tool := &result.Elements[ch.tool]
		tool.Members = slices.Clone(tool.Members)
		v := &tools.Elements[ch.tool].Members[ch.schema].Value
		tool.Members[ch.schema].Value = cloneSchema(v, ch.patterns, make(map[containerID]jsonx.WireValue))
	}
	return result, edits, nil
}

func fieldIndex(v jsonx.WireValue, key string) int {
	for i, m := range v.Members {
		if len(m.Name) == len(key) && startsASCII(m.Name, key) {
			return i
		}
	}
	return -1
}

func pointer(path, key []uint16) []uint16 {
	p := append(path, '/')
	for _, u := range key {
		switch u {
		case '~':
			p = append(p, '~', '0')
		case '/':
			p = append(p, '~', '1')
		default:
			p = append(p, u)
		}
	}
	return p
}

// Backing storage identifies shared or cyclic objects in caller-constructed
// graphs. Include the length to distinguish different views of the same slice.
type containerID struct {
	member  *jsonx.Member
	element *jsonx.WireValue
	length  int
}

func identity(v jsonx.WireValue) containerID {
	if v.Kind == '{' && cap(v.Members) > 0 {
		return containerID{member: &v.Members[:1][0], length: len(v.Members)}
	}
	if v.Kind == '[' && cap(v.Elements) > 0 {
		return containerID{element: &v.Elements[:1][0], length: len(v.Elements)}
	}
	return containerID{}
}

func dataKeyword(s string) bool {
	switch s {
	case "const", "default", "enum", "examples", "description":
		return true
	}
	return false
}

func affected(v jsonx.WireValue, seen map[containerID]bool) bool {
	id := identity(v)
	if id == (containerID{}) || seen[id] {
		return false
	}
	seen[id] = true
	for _, m := range v.Members {
		name := string(utf16.Decode(m.Name))
		switch name {
		case "pattern":
			if m.Value.Kind == '"' && containsPotentialProperty(m.Value.Text) {
				return true
			}
		case "patternProperties":
			if m.Value.Kind == '{' {
				for _, p := range m.Value.Members {
					if containsPotentialProperty(p.Name) {
						return true
					}
				}
			}
		}
		if !dataKeyword(name) && affected(m.Value, seen) {
			return true
		}
	}
	for _, child := range v.Elements {
		if affected(child, seen) {
			return true
		}
	}
	return false
}

type classification struct {
	path     []uint16 // Shared traversal buffer; edits and refusals retain a copy.
	toolName []uint16
	edits    []Edit
	patterns map[*jsonx.WireValue][]uint16
	visiting map[containerID]bool
}

func (c *classification) refuse(reason string) error {
	return &CompatibilityError{Pointer: slices.Clone(c.path), ReasonCode: reason, ToolName: slices.Clone(c.toolName)}
}

func (c *classification) visit(v jsonx.WireValue) error {
	switch v.Kind {
	case 't', 'f':
		return nil
	case '{':
	default:
		return c.refuse("invalid-schema-container")
	}
	id := identity(v)
	if c.visiting[id] {
		return errors.New("cyclic schema applicator")
	}
	c.visiting[id] = true
	defer delete(c.visiting, id)
	base := len(c.path)
	for i := range v.Members {
		m := &v.Members[i]
		k := string(utf16.Decode(m.Name))
		child := &m.Value
		c.path = pointer(c.path[:base], m.Name)
		switch k {
		case "$schema":
			const dialect = "https://json-schema.org/draft/2020-12/schema"
			s := string(utf16.Decode(child.Text))
			if child.Kind != '"' || s != dialect && s != dialect+"#" {
				return c.refuse("unsupported-dialect")
			}
		case "$vocabulary":
			return c.refuse("custom-vocabulary")
		case "$id", "$ref", "$dynamicRef", "$recursiveRef", "$anchor", "$dynamicAnchor", "$recursiveAnchor":
			return c.refuse("reference-or-anchor-keyword")
		case "$defs", "definitions", "not", "if", "then", "else", "oneOf", "contains", "contentSchema":
			return c.refuse("forbidden-schema-applicator")
		case "pattern":
			if child.Kind != '"' || !containsPotentialProperty(child.Text) {
				continue
			}
			spans, why := scanPattern(child.Text)
			if why != "" {
				return c.refuse(why)
			}
			if !validUnicodePattern(child.Text) {
				return c.refuse(invalidRegex)
			}
			var replacement []uint16
			cursor := 0
			for _, s := range spans {
				replacement = append(replacement, child.Text[cursor:s.start]...)
				cursor = s.end
			}
			replacement = append(replacement, child.Text[cursor:]...)
			c.patterns[child] = replacement
			c.edits = append(c.edits, Edit{Pointer: slices.Clone(c.path), Replacement: replacement})
		case "patternProperties", "properties", "dependentSchemas":
			if child.Kind != '{' {
				if affected(*child, make(map[containerID]bool)) {
					return c.refuse("invalid-schema-container")
				}
				continue
			}
			prefix := len(c.path)
			for _, entry := range child.Members {
				c.path = c.path[:prefix]
				if k == "patternProperties" && containsPotentialProperty(entry.Name) {
					return c.refuse("affected-pattern-properties-key")
				}
				c.path = pointer(c.path, entry.Name)
				if err := c.visit(entry.Value); err != nil {
					return err
				}
			}
		case "additionalProperties", "items", "propertyNames", "unevaluatedProperties", "unevaluatedItems":
			if err := c.visit(*child); err != nil {
				return err
			}
		case "prefixItems", "allOf", "anyOf":
			if child.Kind != '[' {
				if affected(*child, make(map[containerID]bool)) {
					return c.refuse("invalid-schema-container")
				}
				continue
			}
			prefix := len(c.path)
			for j, entry := range child.Elements {
				c.path = append(c.path[:prefix], '/')
				for _, d := range strconv.Itoa(j) {
					c.path = append(c.path, uint16(d))
				}
				if err := c.visit(entry); err != nil {
					return err
				}
			}
		case "$comment", "title", "deprecated", "readOnly", "writeOnly", "format", "contentEncoding", "contentMediaType", "multipleOf", "maximum", "exclusiveMaximum", "minimum", "exclusiveMinimum", "maxLength", "minLength", "maxItems", "minItems", "uniqueItems", "maxContains", "minContains", "maxProperties", "minProperties", "required", "dependentRequired", "type":
		default:
			if !dataKeyword(k) && affected(*child, make(map[containerID]bool)) {
				return c.refuse("unknown-schema-container")
			}
		}
	}
	return nil
}

func cloneSchema(v *jsonx.WireValue, patterns map[*jsonx.WireValue][]uint16, seen map[containerID]jsonx.WireValue) jsonx.WireValue {
	id := identity(*v)
	if old, ok := seen[id]; ok {
		return old
	}
	out := *v
	switch v.Kind {
	case '"':
		if s, ok := patterns[v]; ok {
			out.Text = slices.Clone(s)
		} else {
			out.Text = slices.Clone(v.Text)
		}
	case '{':
		out.Members = make([]jsonx.Member, len(v.Members), max(1, len(v.Members)))
		if id != (containerID{}) {
			seen[id] = out
		}
		for i := range v.Members {
			m := &v.Members[i]
			out.Members[i] = jsonx.Member{Name: slices.Clone(m.Name), Value: cloneSchema(&m.Value, patterns, seen)}
		}
	case '[':
		out.Elements = make([]jsonx.WireValue, len(v.Elements), max(1, len(v.Elements)))
		if id != (containerID{}) {
			seen[id] = out
		}
		for i := range v.Elements {
			out.Elements[i] = cloneSchema(&v.Elements[i], patterns, seen)
		}
	}
	return out
}
