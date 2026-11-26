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
	"errors"
	"math"
	"math/big"
	"strconv"
	"unicode/utf16"

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

// jsString is String(v), which is also what a template literal and Array.prototype.join apply.
func jsString(v jsonx.WireValue) []uint16 {
	switch v.Kind {
	case '"':
		return v.Text
	case '0':
		return units(numberString(v.Number))
	case 't':
		return units("true")
	case 'f':
		return units("false")
	case 'n':
		return units("null")
	case '{':
		return units("[object Object]")
	case '[':
		var out []uint16
		for i, el := range v.Elements {
			if i != 0 {
				out = append(out, ',')
			}
			if el.Kind != 0 && el.Kind != 'n' {
				out = append(out, jsString(el)...)
			}
		}
		return out
	default:
		return units("undefined")
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

// jsNumber is Number(v).
func jsNumber(v jsonx.WireValue) float64 {
	switch v.Kind {
	case '0':
		return v.Number
	case 't':
		return 1
	case 'f', 'n':
		return 0
	case '"':
		return stringToNumber(v.Text)
	case '[':
		return stringToNumber(jsString(v))
	default:
		return math.NaN()
	}
}

// count is Number(v) || 0.
func count(v jsonx.WireValue) float64 {
	n := jsNumber(v)
	if math.IsNaN(n) || n == 0 {
		return 0
	}
	return n
}

// isSpace reports the WhiteSpace and LineTerminator code points that String.prototype.trim and
// Number() strip.
func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func trimUnits(u []uint16) []uint16 {
	for len(u) > 0 && isSpace(rune(u[0])) {
		u = u[1:]
	}
	for len(u) > 0 && isSpace(rune(u[len(u)-1])) {
		u = u[:len(u)-1]
	}
	return u
}

// stringToNumber applies the StringNumericLiteral grammar of ECMAScript: surrounding white space,
// an empty string as 0, signed decimal literals and Infinity, and unsigned 0x, 0o and 0b integers.
// Anything else, including spellings strconv accepts but JavaScript does not (inf, nan, digit
// separators, hexadecimal floats), is NaN.
func stringToNumber(u []uint16) float64 {
	u = trimUnits(u)
	if len(u) == 0 {
		return 0
	}
	b := make([]byte, len(u))
	for i, c := range u {
		if c > 0x7f {
			return math.NaN()
		}
		b[i] = byte(c)
	}
	s := string(b)
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			return integerLiteral(s[2:], base)
		}
	}
	if !decimalLiteral(s) {
		return math.NaN()
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	return n
}

func integerLiteral(digits string, base int) float64 {
	for _, c := range []byte(digits) {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = int(c-'A') + 10
		default:
			return math.NaN()
		}
		if d >= base {
			return math.NaN()
		}
	}
	i, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return math.NaN()
	}
	f, _ := new(big.Float).SetInt(i).Float64()
	return f
}

// decimalLiteral reports whether s is [+-] digits [. digits] [e [+-] digits] with at least one
// mantissa digit.
func decimalLiteral(s string) bool {
	i := 0
	if s[i] == '+' || s[i] == '-' {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits++
		}
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		exponent := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == exponent {
			return false
		}
	}
	return i == len(s)
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
