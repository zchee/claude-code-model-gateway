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

package toolschema

import "slices"

const (
	punctuation       = "escaped-punctuation"
	shorthand         = "character-class-shorthand"
	boundary          = "word-boundary-or-backspace"
	whitespace        = "control-whitespace"
	hexUnit           = "hexadecimal-code-unit"
	codePoint         = "unicode-code-point"
	control           = "control-character"
	nul               = "null-character"
	property          = "unicode-property"
	malformedProperty = "malformed-unicode-property"
	invalidRegex      = "invalid-regex"
	syntaxCharacters  = `^$\.*+?()[]{}|/`
)

func acceptedEscapeKinds() []string {
	return []string{punctuation, shorthand, boundary, whitespace, hexUnit, codePoint, control, nul, property}
}

func inASCII(c uint16, chars string) bool {
	for i := range len(chars) {
		if uint16(chars[i]) == c {
			return true
		}
	}
	return false
}

func startsASCII(text []uint16, prefix string) bool {
	if len(text) < len(prefix) {
		return false
	}
	for i := range len(prefix) {
		if text[i] != uint16(prefix[i]) {
			return false
		}
	}
	return true
}

func hexValue(c uint16) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return -1
}

func asciiLetter(c uint16) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// scanEscape recognizes compatibility tokens, not ECMAScript grammar: in
// particular, the source's case-insensitive control-letter scan folds ſ and K.
func scanEscape(text []uint16) (kind string, width int) {
	if len(text) == 1 {
		return invalidRegex, 1
	}
	c := text[1]
	switch {
	case c == 'p' || c == 'P':
		if len(text) < 3 || text[2] != '{' {
			return malformedProperty, 2
		}
		i := slices.Index(text[3:], '}')
		if i <= 0 {
			return malformedProperty, len(text)
		}
		return property, i + 4
	case c >= '1' && c <= '9' || c == 'k':
		return "backreference-or-unsupported-escape", 2
	case c == '0':
		return nul, 2
	case c == 'x' || c == 'u':
		n, kind := 2, hexUnit
		if c == 'u' {
			n, kind = 4, codePoint
			if len(text) > 2 && text[2] == '{' {
				i := 3
				for i < len(text) && i < 9 && hexValue(text[i]) >= 0 {
					i++
				}
				if i > 3 && i < len(text) && text[i] == '}' {
					return kind, i + 1
				}
				return invalidRegex, 2
			}
		}
		if len(text) >= n+2 {
			for _, d := range text[2 : n+2] {
				if hexValue(d) < 0 {
					return invalidRegex, 2
				}
			}
			return kind, n + 2
		}
		return invalidRegex, 2
	case c == 'c':
		if len(text) > 2 && (asciiLetter(text[2]) || text[2] == 'ſ' || text[2] == 'K') {
			return control, 3
		}
		return invalidRegex, 2
	case inASCII(c, "dDsSwW"):
		return shorthand, 2
	case inASCII(c, "bB"):
		return boundary, 2
	case inASCII(c, "fnrtv"):
		return whitespace, 2
	case inASCII(c, syntaxCharacters+"-"):
		return punctuation, 2
	default:
		return "unsupported-regex-escape", 2
	}
}

func containsPotentialProperty(pattern []uint16) bool {
	for i := 0; i < len(pattern); {
		if pattern[i] != '\\' {
			i++
			continue
		}
		kind, n := scanEscape(pattern[i:])
		if kind == property || kind == malformedProperty {
			return true
		}
		i += n
	}
	return false
}

type span struct{ start, end int }

// scanPattern returns the property spans to remove, in source order. A class's
// own errors take precedence over the negative lookaround that contains it.
func scanPattern(pattern []uint16) ([]span, string) {
	var removals []span
	var groups []bool
	negative := 0
	accepted := acceptedEscapeKinds()
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '\\':
			kind, n := scanEscape(pattern[i:])
			if kind == property {
				return nil, "property-outside-negated-character-class"
			}
			if !slices.Contains(accepted, kind) {
				return nil, kind
			}
			i += n
		case '[':
			i++
			negated := i < len(pattern) && pattern[i] == '^'
			if negated {
				i++
			}
			start, first := i, len(removals)
			removed := 0
			hasRange := false
			for i < len(pattern) && pattern[i] != ']' {
				if pattern[i] == '\\' {
					kind, n := scanEscape(pattern[i:])
					if !slices.Contains(accepted, kind) {
						return nil, kind
					}
					if kind == property {
						removals = append(removals, span{i, i + n})
						removed += n
					}
					i += n
					continue
				}
				hasRange = hasRange || pattern[i] == '-' || startsASCII(pattern[i:], "&&")
				i++
			}
			if i == len(pattern) {
				return nil, invalidRegex
			}
			if len(removals) > first {
				if !negated {
					return nil, "positive-character-class"
				}
				if !hasRange {
					for j := start; j+1 < i; j++ {
						if startsASCII(pattern[j:], "--") {
							hasRange = true
							break
						}
					}
				}
				if hasRange {
					return nil, "range-or-set-character-class"
				}
				if i-start == removed {
					return nil, "empty-negated-character-class"
				}
				if negative != 0 {
					return nil, "negative-regex-context"
				}
			}
			i++
		case '(':
			prefix := pattern[i:]
			neg := startsASCII(prefix, "(?!") || startsASCII(prefix, "(?<!")
			switch {
			case startsASCII(prefix, "(?<=") || startsASCII(prefix, "(?<!"):
				i += 4
			case neg || startsASCII(prefix, "(?:") || startsASCII(prefix, "(?="):
				i += 3
			default:
				return nil, "capture-or-unsupported-group"
			}
			groups = append(groups, neg)
			if neg {
				negative++
			}
		case ')':
			if n := len(groups); n != 0 {
				if groups[n-1] {
					negative--
				}
				groups = groups[:n-1]
			}
			i++
		default:
			i++
		}
	}
	return removals, ""
}
