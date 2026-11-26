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

package reply

import (
	"math"
	"slices"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Every document this package re-serializes goes through the wire tree, so member order, duplicate
// names, out-of-range numbers and unpaired surrogates come out as JSON.parse plus JSON.stringify
// would produce them.

func units(s string) []uint16 { return utf16.Encode([]rune(s)) }

func wireString(s string) jsonx.WireValue { return jsonx.WireValue{Kind: '"', Text: units(s)} }

func wireUnits(u []uint16) jsonx.WireValue { return jsonx.WireValue{Kind: '"', Text: u} }

func wireNumber(n float64) jsonx.WireValue { return jsonx.WireValue{Kind: '0', Number: n} }

var wireNull = jsonx.WireValue{Kind: 'n'}

func field(name string, value jsonx.WireValue) jsonx.Member {
	return jsonx.Member{Name: units(name), Value: value}
}

func object(members ...jsonx.Member) jsonx.WireValue {
	return jsonx.WireValue{Kind: '{', Members: members}
}

// encode serializes a tree built or decoded by this package. Such trees stay within the
// tokenizer's depth limit, so the error is only reported, never expected.
func encode(v jsonx.WireValue) ([]byte, error) { return jsonx.EncodeWire(v) }

// isName compares a member name with an ASCII name without converting either.
func isName(name []uint16, ascii string) bool {
	if len(name) != len(ascii) {
		return false
	}
	for i, unit := range name {
		if unit != uint16(ascii[i]) {
			return false
		}
	}
	return true
}

// lookup returns the member value of an object, or nil when v is no object or lacks the name.
// Decoded objects hold each name once, so the pointer is the only occurrence.
func lookup(v *jsonx.WireValue, name string) *jsonx.WireValue {
	if v.Kind != '{' {
		return nil
	}
	for i := range v.Members {
		if isName(v.Members[i].Name, name) {
			return &v.Members[i].Value
		}
	}
	return nil
}

// member returns the value JavaScript reads as v[name]; a missing value reads as undefined (Kind 0).
func member(v jsonx.WireValue, name string) jsonx.WireValue {
	if m := lookup(&v, name); m != nil {
		return *m
	}
	return jsonx.WireValue{}
}

// assign sets v[name] as a JavaScript assignment does: in place when present, appended otherwise.
func assign(v *jsonx.WireValue, name string, value jsonx.WireValue) {
	if m := lookup(v, name); m != nil {
		*m = value
		return
	}
	v.Members = append(v.Members, field(name, value))
}

func isString(v jsonx.WireValue, s string) bool {
	return v.Kind == '"' && slices.Equal(v.Text, units(s))
}

func truthy(v jsonx.WireValue) bool {
	switch v.Kind {
	case 't', '{', '[':
		return true
	case '0':
		return v.Number != 0 && !math.IsNaN(v.Number)
	case '"':
		return len(v.Text) != 0
	default:
		return false
	}
}

// jsNumber formats n as a JavaScript template literal does.
func jsNumber(n float64) string {
	switch {
	case math.IsNaN(n):
		return "NaN"
	case math.IsInf(n, 1):
		return "Infinity"
	case math.IsInf(n, -1):
		return "-Infinity"
	}
	b, _ := encode(wireNumber(n)) // A finite number always encodes.
	return string(b)
}

// bufferText decodes bytes as Node's Buffer.toString does, replacing ill-formed UTF-8.
func bufferText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out, err := unicode.UTF8.NewDecoder().Bytes(b)
	if err != nil {
		return string([]rune(string(b)))
	}
	return string(out)
}

// wtf8 spells code units as bytes, keeping an unpaired surrogate as its three-byte generalized
// UTF-8 form so that a byte-oriented edit of the surrounding text leaves it intact.
func wtf8(u []uint16) string {
	b := make([]byte, 0, len(u))
	for i := 0; i < len(u); i++ {
		r := rune(u[i])
		if utf16.IsSurrogate(r) {
			if i+1 < len(u) {
				if pair := utf16.DecodeRune(r, rune(u[i+1])); pair != utf8.RuneError {
					b = utf8.AppendRune(b, pair)
					i++
					continue
				}
			}
			b = append(b, byte(0xE0|r>>12), byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
			continue
		}
		b = utf8.AppendRune(b, r)
	}
	return string(b)
}

// fromWTF8 reverses wtf8 for text whose other bytes are well-formed UTF-8.
func fromWTF8(s string) []uint16 {
	u := make([]uint16, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2]&0xC0 == 0x80 {
			u = append(u, 0xD000|uint16(s[i+1]&0x3F)<<6|uint16(s[i+2]&0x3F))
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		u = utf16.AppendRune(u, r)
		i += size
	}
	return u
}

// lowerASCII folds only ASCII letters, as JavaScript's non-Unicode /i flag does for ASCII patterns,
// and keeps every byte offset.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
