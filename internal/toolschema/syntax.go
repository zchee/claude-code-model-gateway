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

import (
	"math"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// A group collects names that could participate in a single match. Duplicate
// names in different alternatives are legal, but not in consecutive terms.
type patternGroup struct {
	assertion            bool
	branch, alternatives map[string]bool
}

func mergeNames(a, b map[string]bool, disjoint bool) (map[string]bool, bool) {
	if len(b) > len(a) {
		a, b = b, a
	}
	for name := range b {
		if disjoint && a[name] {
			return nil, false
		}
		a[name] = true
	}
	return a, true
}

// V8 allocates two 16-bit capture registers per group, including the whole match.
const maxCaptures = (1 << 15) - 1

type syntaxParser struct {
	rest             []uint16
	captures, maxRef int
	namedRefs        []string
}

// validUnicodePattern checks ECMA-262 Pattern with UnicodeMode=true and
// UnicodeSetsMode=false. It never compiles or matches a translated expression.
func validUnicodePattern(pattern []uint16) bool {
	p := syntaxParser{rest: pattern}
	groups := []patternGroup{{}}
	atom := false
	for len(p.rest) > 0 {
		g := &groups[len(groups)-1]
		c := p.rest[0]
		switch c {
		case '|':
			p.rest = p.rest[1:]
			g.alternatives, _ = mergeNames(g.alternatives, g.branch, false)
			g.branch = nil
			atom = false
		case '(':
			p.rest = p.rest[1:]
			name, capture, assertion, ok := p.groupPrefix()
			if !ok {
				return false
			}
			if capture {
				p.captures++
				if p.captures > maxCaptures {
					return false
				}
			}
			if name != "" {
				if g.branch[name] {
					return false
				}
				if g.branch == nil {
					g.branch = make(map[string]bool)
				}
				g.branch[name] = true
			}
			groups = append(groups, patternGroup{assertion: assertion})
			atom = false
		case ')':
			if len(groups) == 1 {
				return false
			}
			p.rest = p.rest[1:]
			atom = !g.assertion
			names, _ := mergeNames(g.alternatives, g.branch, false)
			groups = groups[:len(groups)-1]
			parent := &groups[len(groups)-1]
			var ok bool
			parent.branch, ok = mergeNames(parent.branch, names, true)
			if !ok {
				return false
			}
		case '^', '$':
			p.rest = p.rest[1:]
			atom = false
		case '*', '+', '?', '{':
			if !atom || !p.quantifier() {
				return false
			}
			p.take('?')
			atom = false
		case ']', '}':
			return false
		case '[':
			p.rest = p.rest[1:]
			if !p.class() {
				return false
			}
			atom = true
		case '\\':
			p.rest = p.rest[1:]
			if p.take('b') || p.take('B') {
				atom = false
				continue
			}
			if _, ok := p.escape(false); !ok {
				return false
			}
			atom = true
		default:
			p.literal()
			atom = true
		}
	}
	if len(groups) != 1 || p.maxRef > p.captures {
		return false
	}
	g := groups[0]
	for _, name := range p.namedRefs {
		if !g.branch[name] && !g.alternatives[name] {
			return false
		}
	}
	return true
}

func (p *syntaxParser) take(c uint16) bool {
	if len(p.rest) == 0 || p.rest[0] != c {
		return false
	}
	p.rest = p.rest[1:]
	return true
}

func (p *syntaxParser) groupPrefix() (name string, capture, assertion, ok bool) {
	if !p.take('?') {
		return "", true, false, true
	}
	if p.take('=') || p.take('!') {
		return "", false, true, true
	}
	if p.take('<') {
		if p.take('=') || p.take('!') {
			return "", false, true, true
		}
		name, ok = p.groupName()
		return name, true, false, ok
	}
	if p.take(':') {
		return "", false, false, true
	}
	// Only i, m and s are local modifiers. Across both lists a flag occurs once.
	flags := 0
	minus := false
	for len(p.rest) > 0 {
		if !minus && p.take('-') {
			minus = true
			continue
		}
		c := p.rest[0]
		if !inASCII(c, "ims") {
			break
		}
		bit := 1 << (c - 'i')
		if flags&bit != 0 {
			return "", false, false, false
		}
		flags |= bit
		p.rest = p.rest[1:]
	}
	return "", false, false, flags != 0 && p.take(':')
}

func (p *syntaxParser) groupName() (string, bool) {
	var name strings.Builder
	for len(p.rest) > 0 {
		var r rune
		if p.take('\\') {
			if !p.take('u') {
				return "", false
			}
			var ok bool
			r, ok = p.unicodeEscape()
			if !ok {
				return "", false
			}
		} else {
			r = p.literal()
		}
		if r == '>' {
			return name.String(), name.Len() > 0
		}
		if !identifierChar(r, name.Len() == 0) {
			return "", false
		}
		name.WriteRune(r)
	}
	return "", false
}

func (p *syntaxParser) literal() rune {
	r := rune(p.rest[0])
	p.rest = p.rest[1:]
	if len(p.rest) > 0 && utf16.IsSurrogate(r) {
		if pair := utf16.DecodeRune(r, rune(p.rest[0])); pair != utf8.RuneError {
			p.rest = p.rest[1:]
			return pair
		}
	}
	return r
}

func digit(c uint16) bool { return c >= '0' && c <= '9' }

// V8 saturates interval bounds at RegExpTree::kInfinity (INT_MAX).
func (p *syntaxParser) decimal() (int, bool) {
	if len(p.rest) == 0 || !digit(p.rest[0]) {
		return 0, false
	}
	n := 0
	for len(p.rest) > 0 && digit(p.rest[0]) {
		d := int(p.rest[0] - '0')
		if n > (math.MaxInt32-d)/10 {
			n = math.MaxInt32
		} else {
			n = n*10 + d
		}
		p.rest = p.rest[1:]
	}
	return n, true
}

func (p *syntaxParser) quantifier() bool {
	if !p.take('{') {
		p.rest = p.rest[1:]
		return true
	}
	lo, ok := p.decimal()
	if !ok {
		return false
	}
	if p.take(',') {
		if hi, ok := p.decimal(); ok && hi < lo {
			return false
		}
	}
	return p.take('}')
}

// A negative value stands for a set, which cannot be a range endpoint.
func (p *syntaxParser) class() bool {
	p.take('^')
	for len(p.rest) > 0 {
		if p.take(']') {
			return true
		}
		left, ok := p.classAtom()
		if !ok {
			return false
		}
		if len(p.rest) > 1 && p.rest[0] == '-' && p.rest[1] != ']' {
			p.rest = p.rest[1:]
			right, ok := p.classAtom()
			if !ok || left < 0 || right < left {
				return false
			}
		}
	}
	return false
}

func (p *syntaxParser) classAtom() (rune, bool) {
	if p.take('\\') {
		return p.escape(true)
	}
	return p.literal(), true
}

func (p *syntaxParser) escape(inClass bool) (rune, bool) {
	if len(p.rest) == 0 {
		return 0, false
	}
	c := p.rest[0]
	p.rest = p.rest[1:]
	switch {
	case inASCII(c, "dDsSwW"):
		return -1, true
	case c == 'p' || c == 'P':
		if !p.take('{') {
			return 0, false
		}
		var b strings.Builder
		for len(p.rest) > 0 && p.rest[0] != '}' {
			c := p.rest[0]
			if !asciiLetter(c) && !digit(c) && !inASCII(c, "_=") {
				return 0, false
			}
			b.WriteByte(byte(c))
			p.rest = p.rest[1:]
		}
		return -1, p.take('}') && validProperty(b.String())
	case inASCII(c, syntaxCharacters) || inClass && c == '-':
		return rune(c), true
	case c == 'b' && inClass:
		return '\b', true
	case c == 'f':
		return '\f', true
	case c == 'n':
		return '\n', true
	case c == 'r':
		return '\r', true
	case c == 't':
		return '\t', true
	case c == 'v':
		return '\v', true
	case c == '0':
		return 0, len(p.rest) == 0 || !digit(p.rest[0])
	case c == 'c':
		if len(p.rest) == 0 || !asciiLetter(p.rest[0]) {
			return 0, false
		}
		r := rune(p.rest[0] % 32)
		p.rest = p.rest[1:]
		return r, true
	case c == 'x':
		return p.hex(2)
	case c == 'u':
		return p.unicodeEscape()
	case !inClass && digit(c):
		n := int(c - '0')
		for len(p.rest) > 0 && digit(p.rest[0]) {
			if n > maxCaptures {
				return 0, false
			}
			n = n*10 + int(p.rest[0]-'0')
			p.rest = p.rest[1:]
		}
		p.maxRef = max(p.maxRef, n)
		return -1, true
	case !inClass && c == 'k':
		if !p.take('<') {
			return 0, false
		}
		name, ok := p.groupName()
		if ok {
			p.namedRefs = append(p.namedRefs, name)
		}
		return -1, ok
	}
	return 0, false
}

func (p *syntaxParser) hex(n int) (rune, bool) {
	if len(p.rest) < n {
		return 0, false
	}
	var r rune
	for _, c := range p.rest[:n] {
		d := hexValue(c)
		if d < 0 {
			return 0, false
		}
		r = r*16 + rune(d)
	}
	p.rest = p.rest[n:]
	return r, true
}

func (p *syntaxParser) unicodeEscape() (rune, bool) {
	if p.take('{') {
		if len(p.rest) == 0 || hexValue(p.rest[0]) < 0 {
			return 0, false
		}
		var r rune
		for len(p.rest) > 0 {
			d := hexValue(p.rest[0])
			if d < 0 {
				break
			}
			r = r*16 + rune(d)
			if r > utf8.MaxRune {
				return 0, false
			}
			p.rest = p.rest[1:]
		}
		return r, p.take('}')
	}
	r, ok := p.hex(4)
	if !ok {
		return 0, false
	}
	if r >= 0xd800 && r <= 0xdbff && startsASCII(p.rest, `\u`) {
		trial := syntaxParser{rest: p.rest[2:]}
		if low, ok := trial.hex(4); ok {
			if pair := utf16.DecodeRune(r, low); pair != utf8.RuneError {
				p.rest = trial.rest
				return pair, true
			}
		}
	}
	return r, true
}
