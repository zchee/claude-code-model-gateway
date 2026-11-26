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

package sse

import (
	"bytes"
	"iter"
	"math"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	textunicode "golang.org/x/text/encoding/unicode"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// units converts a Go string to the UTF-16 code units a decoded wire string holds.
func units(s string) []uint16 { return utf16.Encode([]rune(s)) }

// sameASCII reports whether the code units spell the ASCII string s exactly.
func sameASCII(text []uint16, s string) bool {
	if len(text) != len(s) {
		return false
	}
	for i, unit := range text {
		if unit != uint16(s[i]) {
			return false
		}
	}
	return true
}

// isText reports whether v is a wire string equal to the ASCII string s.
func isText(v jsonx.WireValue, s string) bool { return v.Kind == '"' && sameASCII(v.Text, s) }

// memberIndex returns the position of the named member of an object, or -1.
// Decoded objects hold each name once, so the first match is the only one.
func memberIndex(v jsonx.WireValue, name string) int {
	if v.Kind != '{' {
		return -1
	}
	for i, m := range v.Members {
		if sameASCII(m.Name, name) {
			return i
		}
	}
	return -1
}

// member returns the named member of an object, or the zero value when v is not an
// object or has no such member, as a JavaScript property read yields undefined.
func member(v jsonx.WireValue, name string) jsonx.WireValue {
	if i := memberIndex(v, name); i >= 0 {
		return v.Members[i].Value
	}
	return jsonx.WireValue{}
}

// withMember returns a copy of the object v whose existing member at index i holds value.
// The member slice is cloned, so values already handed to an observer never change.
func withMember(v jsonx.WireValue, i int, value jsonx.WireValue) jsonx.WireValue {
	v.Members = slices.Clone(v.Members)
	v.Members[i].Value = value
	return v
}

// finiteNumber reports whether v is a number for which Number.isFinite holds.
func finiteNumber(v jsonx.WireValue) bool {
	return v.Kind == '0' && !math.IsInf(v.Number, 0) && !math.IsNaN(v.Number)
}

// integerNumber reports whether v is a number for which Number.isInteger holds.
func integerNumber(v jsonx.WireValue) bool {
	return finiteNumber(v) && math.Trunc(v.Number) == v.Number
}

// jsWhitespace reports whether r is removed by String.prototype.trim: ECMAScript
// WhiteSpace and LineTerminator code points.
func jsWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrimStart removes leading JavaScript whitespace from s. An ill-formed UTF-8
// byte decodes to U+FFFD, which is not whitespace, so trimming stops there.
func jsTrimStart(s string) string {
	for s != "" {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size <= 1 || !jsWhitespace(r) {
			break
		}
		s = s[size:]
	}
	return s
}

// jsBlank reports whether trimming the UTF-16 text would leave it empty. An unpaired
// surrogate is not whitespace.
func jsBlank(text []uint16) bool {
	for _, unit := range text {
		if !jsWhitespace(rune(unit)) {
			return false
		}
	}
	return true
}

// appendNodeUTF8 appends b decoded as Node decodes UTF-8 text: every maximal ill-formed
// subsequence becomes one U+FFFD.
func appendNodeUTF8(dst, b []byte) []byte {
	if utf8.Valid(b) {
		return append(dst, b...)
	}
	decoded, err := textunicode.UTF8.NewDecoder().Bytes(b)
	if err != nil {
		return append(dst, bytes.ToValidUTF8(b, []byte("�"))...)
	}
	return append(dst, decoded...)
}

// nodeUTF8String decodes s as Node decodes UTF-8 text.
func nodeUTF8String(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return string(appendNodeUTF8(nil, []byte(s)))
}

// incompleteTail returns the length of a trailing UTF-8 sequence that is a valid
// prefix still waiting for continuation bytes, which a streaming decoder holds back.
func incompleteTail(b []byte) int {
	n := len(b)
	for i := 1; i <= 3 && i <= n; i++ {
		c := b[n-i]
		if c < utf8.RuneSelf {
			return 0
		}
		if c&0xc0 == 0x80 {
			continue
		}
		size := 0
		switch {
		case c >= 0xc2 && c <= 0xdf:
			size = 2
		case c >= 0xe0 && c <= 0xef:
			size = 3
		case c >= 0xf0 && c <= 0xf4:
			size = 4
		}
		if size == 0 || i >= size {
			return 0
		}
		if i >= 2 {
			lo, hi := byte(0x80), byte(0xbf)
			switch c {
			case 0xe0:
				lo = 0xa0
			case 0xed:
				hi = 0x9f
			case 0xf0:
				lo = 0x90
			case 0xf4:
				hi = 0x8f
			}
			if second := b[n-i+1]; second < lo || second > hi {
				return 0
			}
		}
		return i
	}
	return 0
}

// findDelimiter finds the earliest blank-line separator /\r?\n\r?\n/ that starts at or
// after from, returning the separator's bounds.
func findDelimiter(b []byte, from int) (start, end int, ok bool) {
	for i := from; i < len(b); i++ {
		next := bytes.IndexAny(b[i:], "\r\n")
		if next < 0 {
			return 0, 0, false
		}
		i += next
		j := i + 1
		if b[i] == '\r' {
			if j >= len(b) || b[j] != '\n' {
				continue
			}
			j++
		}
		switch {
		case j < len(b) && b[j] == '\n':
			return i, j + 1, true
		case j+1 < len(b) && b[j] == '\r' && b[j+1] == '\n':
			return i, j + 2, true
		}
	}
	return 0, 0, false
}

// lines yields each line of an SSE frame body as String.prototype.split(/\r?\n/) produces them.
func lines(body string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for {
			line, rest, found := strings.Cut(body, "\n")
			if !found {
				yield(body)
				return
			}
			if !yield(strings.TrimSuffix(line, "\r")) {
				return
			}
			body = rest
		}
	}
}
