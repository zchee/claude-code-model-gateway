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

package grok

import (
	"math"
	"slices"
	"strconv"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// The source reads every upstream value with JavaScript operators (`?.`, `||`, `??`, `String()`,
// `Number()`, template literals). These helpers reproduce those operators over wire trees. A zero
// WireValue (Kind 0) stands for `undefined`: a property that is absent, or read from a value that
// is not an object.

func units(s string) []uint16 { return utf16.Encode([]rune(s)) }

func str(s string) jsonx.WireValue { return jsonx.WireValue{Kind: '"', Text: units(s)} }

func strUnits(u []uint16) jsonx.WireValue { return jsonx.WireValue{Kind: '"', Text: u} }

func num(n float64) jsonx.WireValue { return jsonx.WireValue{Kind: '0', Number: n} }

func boolean(b bool) jsonx.WireValue {
	if b {
		return jsonx.WireValue{Kind: 't'}
	}
	return jsonx.WireValue{Kind: 'f'}
}

func null() jsonx.WireValue { return jsonx.WireValue{Kind: 'n'} }

func array(elements []jsonx.WireValue) jsonx.WireValue {
	return jsonx.WireValue{Kind: '[', Elements: elements}
}

// member pairs a property name with its value for obj.
func member(name string, v jsonx.WireValue) jsonx.Member {
	return jsonx.Member{Name: units(name), Value: v}
}

// obj builds an object literal. A member whose value is undefined is left out, as
// JSON.stringify leaves it out.
func obj(members ...jsonx.Member) jsonx.WireValue {
	out := make([]jsonx.Member, 0, len(members))
	for _, m := range members {
		if m.Value.Kind != 0 {
			out = append(out, m)
		}
	}
	return jsonx.WireValue{Kind: '{', Members: out}
}

func nameIs(name []uint16, s string) bool {
	if len(name) != len(s) {
		return false
	}
	for i := range len(s) {
		if name[i] != uint16(s[i]) {
			return false
		}
	}
	return true
}

// get reads v.name; only objects have the properties the source reads. The decoder has already
// collapsed duplicate names, so the first match is the only one.
func get(v jsonx.WireValue, name string) jsonx.WireValue {
	if v.Kind == '{' {
		for _, m := range v.Members {
			if nameIs(m.Name, name) {
				return m.Value
			}
		}
	}
	return jsonx.WireValue{}
}

// isString reports whether v === s.
func isString(v jsonx.WireValue, s string) bool { return v.Kind == '"' && nameIs(v.Text, s) }

func hasPrefix(v jsonx.WireValue, prefix string) bool {
	return v.Kind == '"' && len(v.Text) >= len(prefix) && nameIs(v.Text[:len(prefix)], prefix)
}

func truthy(v jsonx.WireValue) bool {
	switch v.Kind {
	case 't', '{', '[':
		return true
	case '0':
		return v.Number != 0 && !math.IsNaN(v.Number)
	case '"':
		return len(v.Text) > 0
	default:
		return false
	}
}

// or is a || b.
func or(a, b jsonx.WireValue) jsonx.WireValue {
	if truthy(a) {
		return a
	}
	return b
}

// coalesce is a ?? b.
func coalesce(a, b jsonx.WireValue) jsonx.WireValue {
	if a.Kind == 0 || a.Kind == 'n' {
		return b
	}
	return a
}

// CoercionError reports that JavaScript's String() or Number() would throw on an upstream value.
// ToPrimitive throws for an object whose own toString member is not callable, and a member of a
// parsed object never is; Array.prototype.join applies the same conversion to every element. The
// error carries no part of the value.
type CoercionError struct{}

func (*CoercionError) Error() string {
	return "grok: an upstream object has its own toString member, so String() and Number() reject it"
}

// coercionThrows reports whether String(v) or Number(v) throws.
func coercionThrows(v jsonx.WireValue) bool {
	switch v.Kind {
	case '{':
		return get(v, "toString").Kind != 0
	case '[':
		return slices.ContainsFunc(v.Elements, coercionThrows)
	}
	return false
}

// toString is String(v), as template literals and Array.prototype.join apply it, with the
// source's exception.
func toString(v jsonx.WireValue) ([]uint16, error) {
	if coercionThrows(v) {
		return nil, &CoercionError{}
	}
	return jsString(v), nil
}

// toCount is Number(v) || 0 with the source's exception.
func toCount(v jsonx.WireValue) (float64, error) {
	if coercionThrows(v) {
		return 0, &CoercionError{}
	}
	return count(v), nil
}

// jsString is String(v) for a value on which it does not throw (see toString); an object reads
// as "[object Object]".
func jsString(v jsonx.WireValue) []uint16 {
	if v.Kind == '"' {
		return v.Text // Shared, not copied; callers treat it as read-only.
	}
	return appendString(nil, v)
}

// appendString appends String(v) to dst. Nested arrays write into the one buffer, so the cost is
// linear in the size of v.
func appendString(dst []uint16, v jsonx.WireValue) []uint16 {
	switch v.Kind {
	case '"':
		return append(dst, v.Text...)
	case '0':
		return append(dst, units(numberString(v.Number))...)
	case 't':
		return append(dst, units("true")...)
	case 'f':
		return append(dst, units("false")...)
	case 'n':
		return append(dst, units("null")...)
	case '{':
		return append(dst, units("[object Object]")...)
	case '[':
		for i, el := range v.Elements {
			if i != 0 {
				dst = append(dst, ',')
			}
			if el.Kind != 0 && el.Kind != 'n' {
				dst = appendString(dst, el)
			}
		}
		return dst
	default:
		return append(dst, units("undefined")...)
	}
}

// numberString is Number.prototype.toString(); for finite values it is the JSON.stringify spelling.
func numberString(n float64) string {
	switch {
	case math.IsNaN(n):
		return "NaN"
	case math.IsInf(n, 1):
		return "Infinity"
	case math.IsInf(n, -1):
		return "-Infinity"
	}
	b, err := jsonx.EncodeWire(num(n))
	if err != nil {
		return "NaN"
	}
	return string(b)
}

// jsNumber is Number(v) for a value on which it does not throw (see toCount). Strings, and arrays
// through their String() form, use the repository's ECMAScript StringToNumber.
func jsNumber(v jsonx.WireValue) float64 {
	switch v.Kind {
	case '0':
		return v.Number
	case 't':
		return 1
	case 'f', 'n':
		return 0
	case '"':
		return config.JSNumber(string(utf16.Decode(v.Text)))
	case '[':
		return config.JSNumber(string(utf16.Decode(jsString(v))))
	default:
		return math.NaN()
	}
}

// count is Number(v) || 0 for a value on which Number does not throw.
func count(v jsonx.WireValue) float64 {
	n := jsNumber(v)
	if math.IsNaN(n) || n == 0 {
		return 0
	}
	return n
}

// isSpace reports the WhiteSpace and LineTerminator code points that String.prototype.trim strips.
func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// mapKey is the SameValueZero identity a JavaScript Map uses for a parsed value. Objects and
// arrays are fresh on every parsed event, so they never equal a stored key and have none.
func mapKey(v jsonx.WireValue) (string, bool) {
	switch v.Kind {
	case '"':
		return "s" + unitsKey(v.Text), true
	case '0':
		n := v.Number
		if n == 0 {
			n = 0 // -0 and +0 are the same key.
		}
		return "n" + strconv.FormatFloat(n, 'g', -1, 64), true
	case '{', '[':
		return "", false
	default:
		return string(rune(v.Kind)), true
	}
}

// unitsKey spells UTF-16 units as a Go string without merging distinct lone surrogates.
func unitsKey(u []uint16) string {
	b := make([]byte, 0, 2*len(u))
	for _, c := range u {
		b = append(b, byte(c>>8), byte(c))
	}
	return string(b)
}
