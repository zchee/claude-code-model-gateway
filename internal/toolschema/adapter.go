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
	return jsonx.WireValue{}, nil, errors.New("tool schema adaptation not implemented")
}

func containsPotentialProperty(pattern []uint16) bool { return false }

func acceptedEscapeKinds() []string { return nil }

func validUnicodePattern(pattern []uint16) bool { return false }
