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
	"bytes"
	"cmp"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/unicode"
)

// WireValue represents a JavaScript JSON value. Kind selects the active field;
// null and the two booleans are represented by their token kinds alone.
// Text holds UTF-16 units, including any unpaired surrogate.
type WireValue struct {
	Kind     jsontext.Kind
	Text     []uint16
	Number   float64
	Elements []WireValue
	Members  []Member
}

// Member holds an object property with its UTF-16 name.
type Member struct {
	Name  []uint16
	Value WireValue
}

// DecodeWire parses exactly one document with Node's UTF-8 and JSON.parse semantics.
func DecodeWire(input []byte) (WireValue, error) {
	if !utf8.Valid(input) {
		var err error
		input, err = unicode.UTF8.NewDecoder().Bytes(input)
		if err != nil {
			return WireValue{}, err
		}
	}
	d := jsontext.NewDecoder(bytes.NewReader(input), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
	v, err := readWire(d)
	if err != nil {
		return WireValue{}, err
	}
	if _, err = d.ReadToken(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return WireValue{}, err
	}
	return v, nil
}

func readWire(d *jsontext.Decoder) (WireValue, error) {
	v := WireValue{Kind: d.PeekKind()}
	switch v.Kind {
	case '{', '[':
		if _, err := d.ReadToken(); err != nil {
			return WireValue{}, err
		}
		end := jsontext.Kind(']')
		if v.Kind == '{' {
			end = '}'
		}
		for d.PeekKind() != end {
			var name []uint16
			if v.Kind == '{' {
				key, err := d.ReadValue()
				if err != nil {
					return WireValue{}, err
				}
				name = readUnits(key)
			}
			item, err := readWire(d)
			if err != nil {
				return WireValue{}, err
			}
			if v.Kind == '[' {
				v.Elements = append(v.Elements, item)
				continue
			}
			v.Members = append(v.Members, Member{name, item})
		}
		v.Members = orderedMembers(v.Members)
		_, err := d.ReadToken()
		return v, err
	default:
		raw, err := d.ReadValue()
		if err != nil {
			return WireValue{}, err
		}
		switch v.Kind {
		case '"':
			v.Text = readUnits(raw)
		case '0':
			v.Number, err = strconv.ParseFloat(string(raw), 64)
			if errors.Is(err, strconv.ErrRange) {
				err = nil
			}
		}
		return v, err
	}
}

// readUnits consumes a string already validated by jsontext. Decoding directly
// to Go runes would collapse an escaped lone surrogate into U+FFFD.
func readUnits(raw []byte) []uint16 {
	var units []uint16
	for s := raw[1 : len(raw)-1]; len(s) > 0; {
		if s[0] != '\\' {
			r, n := utf8.DecodeRune(s)
			units = utf16.AppendRune(units, r)
			s = s[n:]
			continue
		}
		c := s[1]
		s = s[2:]
		switch c {
		case 'u':
			v, _ := strconv.ParseUint(string(s[:4]), 16, 16)
			units = append(units, uint16(v))
			s = s[4:]
			continue
		case 'b':
			c = '\b'
		case 'f':
			c = '\f'
		case 'n':
			c = '\n'
		case 'r':
			c = '\r'
		case 't':
			c = '\t'
		}
		units = append(units, uint16(c))
	}
	return units
}

func quoteUnits(dst []byte, units []uint16) []byte {
	dst = append(dst, '"')
	const hex = "0123456789abcdef"
	for i := 0; i < len(units); i++ {
		u := rune(units[i])
		switch u {
		case '"', '\\':
			dst = append(dst, '\\', byte(u))
		case '\n':
			dst = append(dst, `\n`...)
		case '\t':
			dst = append(dst, `\t`...)
		case '\r':
			dst = append(dst, `\r`...)
		case '\b':
			dst = append(dst, `\b`...)
		case '\f':
			dst = append(dst, `\f`...)
		default:
			if utf16.IsSurrogate(u) && i+1 < len(units) {
				if r := utf16.DecodeRune(u, rune(units[i+1])); r != utf8.RuneError {
					dst = utf8.AppendRune(dst, r)
					i++
					continue
				}
			}
			if u < 0x20 || utf16.IsSurrogate(u) {
				dst = append(dst, '\\', 'u', hex[(u>>12)&15], hex[(u>>8)&15], hex[(u>>4)&15], hex[u&15])
			} else {
				dst = utf8.AppendRune(dst, u)
			}
		}
	}
	return append(dst, '"')
}

// propertyIndex puts non-indices after the largest JavaScript array index.
func propertyIndex(s []uint16) uint32 {
	if len(s) == 0 || len(s) > 10 || len(s) > 1 && s[0] == '0' {
		return math.MaxUint32
	}
	var n uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return math.MaxUint32
		}
		n = n*10 + uint64(c-'0')
	}
	return uint32(min(n, math.MaxUint32))
}

func appendNumber(dst []byte, n float64) []byte {
	switch {
	case math.IsNaN(n) || math.IsInf(n, 0):
		return append(dst, "null"...)
	case n == 0:
		return append(dst, '0')
	default:
		return jsontext.AppendFloat(dst, n, 64)
	}
}

// orderedMembers normalizes a private slice in place: the first occurrence fixes
// a property's position and the last occurrence supplies its value.
func orderedMembers(members []Member) []Member {
	if len(members) < 2 {
		return members
	}
	positions := make(map[string]int, len(members))
	out := members[:0]
	for _, m := range members {
		key := string(quoteUnits(nil, m.Name))
		if pos, ok := positions[key]; ok {
			out[pos].Value = m.Value
			continue
		}
		positions[key] = len(out)
		out = append(out, m)
	}
	clear(members[len(out):])
	slices.SortStableFunc(out, func(a, b Member) int { return cmp.Compare(propertyIndex(a.Name), propertyIndex(b.Name)) })
	return out
}

// MarshalJSON encodes a wire tree embedded in a typed state record.
func (v WireValue) MarshalJSON() ([]byte, error) { return EncodeWire(v) }

// EncodeWire emits compact JSON.stringify bytes without a trailing newline.
func EncodeWire(value WireValue) ([]byte, error) { return appendWire(nil, value, 0) }

func appendWire(dst []byte, v WireValue, depth int) ([]byte, error) {
	if depth > 10000 {
		return nil, errors.New("JSON nesting exceeds maximum depth")
	}
	switch v.Kind {
	case 'n':
		return append(dst, "null"...), nil
	case 't':
		return append(dst, "true"...), nil
	case 'f':
		return append(dst, "false"...), nil
	case '"':
		return quoteUnits(dst, v.Text), nil
	case '0':
		return appendNumber(dst, v.Number), nil
	case '[':
		dst = append(dst, '[')
		for i, el := range v.Elements {
			if i != 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendWire(dst, el, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	case '{':
		members := orderedMembers(slices.Clone(v.Members))
		dst = append(dst, '{')
		for i, m := range members {
			if i != 0 {
				dst = append(dst, ',')
			}
			dst = quoteUnits(dst, m.Name)
			dst = append(dst, ':')
			var err error
			dst, err = appendWire(dst, m.Value, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	default:
		return nil, fmt.Errorf("unknown JSON kind %q", v.Kind)
	}
}
