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

package config

import (
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const decimalPattern = `[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`

var (
	stringDecimal = regexp.MustCompile(`^` + decimalPattern + `$`)
	floatPrefix   = regexp.MustCompile(`^(?:[+-]?Infinity|` + decimalPattern + `)`)
)

func jsSpace(r rune) bool {
	return unicode.Is(unicode.Zs, r) || strings.ContainsRune("\t\n\v\f\r", r) || r == 0x2028 || r == 0x2029 || r == 0xFEFF
}

func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }

// JSTrim trims the whitespace and line terminators removed by ECMAScript's String.prototype.trim.
func JSTrim(s string) string { return jsTrim(s) }

// JSNumber implements ECMAScript Number for environment strings and decoded JSON values.
// A nil value represents JSON null; callers handling a missing value must use NaN instead.
func JSNumber(value any) float64 {
	switch v := value.(type) {
	case nil:
		return 0
	case float64:
		return v
	case int:
		return float64(v)
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		return jsNumber(v)
	case []any:
		switch len(v) {
		case 0:
			return 0
		case 1:
			if _, boolean := v[0].(bool); !boolean {
				// Array stringification loses a number's negative zero sign, but not a string's.
				switch item := v[0].(type) {
				case float64:
					return item + 0
				default:
					return JSNumber(item)
				}
			}
		}
	}
	return math.NaN()
}

func jsNumber(s string) float64 {
	s = jsTrim(s)
	if s == "" {
		return 0
	}
	if len(s) >= 2 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			return integerNumber(s[2:], 16, false, false)
		case 'b', 'B':
			return integerNumber(s[2:], 2, false, false)
		case 'o', 'O':
			return integerNumber(s[2:], 8, false, false)
		}
	}
	if s == "Infinity" || s == "+Infinity" || s == "-Infinity" || stringDecimal.MatchString(s) {
		return decimalNumber(s)
	}
	return math.NaN()
}

func decimalNumber(s string) float64 {
	n, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	return n
}

// JSParseFloat implements ECMAScript parseFloat for strings, consuming the longest decimal or Infinity prefix.
func JSParseFloat(s string) float64 {
	return decimalNumber(floatPrefix.FindString(strings.TrimLeftFunc(s, jsSpace)))
}

// JSParseInt implements ECMAScript parseInt for strings and the radix values used by the gateway.
// Radix zero infers hexadecimal from 0x or decimal otherwise.
func JSParseInt(s string, radix int) float64 {
	s = strings.TrimLeftFunc(s, jsSpace)
	rest, negative := strings.CutPrefix(s, "-")
	if !negative {
		rest, _ = strings.CutPrefix(s, "+")
	}
	if radix == 0 || radix == 16 {
		if len(rest) >= 2 && rest[0] == '0' && (rest[1] == 'x' || rest[1] == 'X') {
			return integerNumber(rest[2:], 16, negative, true)
		}
		if radix == 0 {
			radix = 10
		}
	}
	if radix < 2 || radix > 36 {
		return math.NaN()
	}
	return integerNumber(rest, radix, negative, true)
}

func integerNumber(s string, radix int, negative, prefix bool) float64 {
	end := 0
	for end < len(s) {
		c := s[end]
		var digit int
		switch {
		case '0' <= c && c <= '9':
			digit = int(c - '0')
		case 'a' <= c && c <= 'z':
			digit = int(c-'a') + 10
		case 'A' <= c && c <= 'Z':
			digit = int(c-'A') + 10
		default:
			digit = radix
		}
		if digit >= radix {
			break
		}
		end++
	}
	if end == 0 || !prefix && end != len(s) {
		return math.NaN()
	}
	if n, err := strconv.ParseUint(s[:end], radix, 64); err == nil {
		if negative {
			return -float64(n)
		}
		return float64(n)
	}
	var n big.Int
	n.SetString(s[:end], radix)
	f, _ := n.Float64()
	if negative {
		return -f
	}
	return f
}
