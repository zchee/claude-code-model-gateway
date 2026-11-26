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
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// DecodeWireUnits parses exactly one document held as UTF-16 code units, as JSON.parse parses a
// JavaScript string such as a decoded wire string value. An unpaired surrogate inside a string
// literal stays the same code unit in the result. One outside a string literal, or directly after
// the backslash that opens an escape, is a syntax error, as it is in JavaScript. Tokenizing,
// duplicate names and the nesting limit are those of DecodeWire.
func DecodeWireUnits(text []uint16) (WireValue, error) {
	const hex = "0123456789abcdef"
	b := make([]byte, 0, len(text))
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		u := rune(text[i])
		if utf16.IsSurrogate(u) {
			if i+1 < len(text) {
				if r := utf16.DecodeRune(u, rune(text[i+1])); r != utf8.RuneError {
					// A pair is an ordinary character; after a backslash the tokenizer rejects it.
					b = utf8.AppendRune(b, r)
					escaped = false
					i++
					continue
				}
			}
			if !inString || escaped {
				return WireValue{}, fmt.Errorf("unpaired UTF-16 surrogate at unit %d where JSON allows none", i)
			}
			// The escaped spelling is the one the decoder keeps as a lone unit.
			b = append(b, '\\', 'u', hex[(u>>12)&15], hex[(u>>8)&15], hex[(u>>4)&15], hex[u&15])
			continue
		}
		switch {
		case escaped:
			escaped = false
		case inString && u == '\\':
			escaped = true
		case u == '"':
			inString = !inString
		}
		b = utf8.AppendRune(b, u)
	}
	return DecodeWire(b)
}
