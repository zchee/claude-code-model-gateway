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

package workerproc

import (
	"slices"
	"strings"
)

// versionNumbers is the numeric triple of a valid version text: its major, minor and patch numbers.
type versionNumbers [3]uint32

// parseVersion returns the numeric triple of the version text s and whether s is valid.
//
// s is valid when it is, with nothing before or after it: at most one lowercase "v"; three decimal numbers of one to
// nine ASCII digits each, separated by single dots; and optionally a suffix of one "-" or "+" followed by one or more
// ASCII letters, digits, dots, hyphens and plus signs. The "v" and the suffix do not change the triple, and leading
// zeros do not change a number. parseVersion is the only reader of version texts in this package.
func parseVersion(s string) (versionNumbers, bool) {
	const maxDigits = 9
	var v versionNumbers
	s = strings.TrimPrefix(s, "v")
	for i := range v {
		if i > 0 {
			var ok bool
			if s, ok = strings.CutPrefix(s, "."); !ok {
				return versionNumbers{}, false
			}
		}
		digits := 0
		for digits < len(s) && isASCIIDigit(s[digits]) {
			if digits == maxDigits {
				return versionNumbers{}, false
			}
			v[i] = v[i]*10 + uint32(s[digits]-'0')
			digits++
		}
		if digits == 0 {
			return versionNumbers{}, false
		}
		s = s[digits:]
	}
	if s == "" {
		return v, true
	}
	if s[0] != '-' && s[0] != '+' {
		return versionNumbers{}, false
	}
	suffix := s[1:]
	if suffix == "" || strings.ContainsFunc(suffix, func(r rune) bool { return !isSuffixChar(r) }) {
		return versionNumbers{}, false
	}
	return v, true
}

// olderThan reports whether v is lexicographically smaller than w, number by number.
func (v versionNumbers) olderThan(w versionNumbers) bool {
	return slices.Compare(v[:], w[:]) < 0
}

// isASCIIDigit reports whether c is one of the ASCII digits 0 to 9.
func isASCIIDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

// isSuffixChar reports whether r may appear in a version suffix after its delimiter.
func isSuffixChar(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '.' || r == '-' || r == '+'
}
