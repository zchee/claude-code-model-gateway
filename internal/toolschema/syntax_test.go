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
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf16"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestIdentifierTableVersion(t *testing.T) {
	// Recompute identifierChar's ID_Start and ID_Continue differences against
	// the pinned Node UCD before accepting a new Go Unicode version.
	if diff := gocmp.Diff("17.0.0", unicode.Version); diff != "" {
		t.Fatalf("identifier delta needs updating (-expected Unicode version +got):\n%s", diff)
	}
}

func TestEmptyToolName(t *testing.T) {
	tools := toolsWithPattern(t, `\p{L}`)
	member(t, tools.Elements[0], "name").Text = []uint16{}
	_, _, err := Adapt(tools)
	refusal, ok := errors.AsType[*CompatibilityError](err)
	if !ok {
		t.Fatalf("want compatibility refusal; got %v", err)
	}
	if diff := gocmp.Diff("Codex schema compatibility refused  at /tools/0/input_schema/pattern (property-outside-negated-character-class)", refusal.Error()); diff != "" {
		t.Error(diff)
	}
}

func TestWhitespacePropertyRewrite(t *testing.T) {
	out, _, err := Adapt(toolsWithPattern(t, `[^\p{WSpace}x]`))
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff("[^x]", string(utf16.Decode(member(t, *schemaOf(t, out), "pattern").Text))); diff != "" {
		t.Error(diff)
	}
}

func TestUnicodeGrammar(t *testing.T) {
	const u = `\u`
	tests := map[string]struct {
		valid    bool
		patterns []string
	}{
		"success: maximum capture count":                {true, []string{strings.Repeat("()", 32767)}},
		"error: capture register exhaustion":            {false, []string{strings.Repeat("()", 32768)}},
		"success: raw and escaped identifier":           {true, []string{`(?<π>x)\k<` + u + `03c0>`, `(?<` + u + `03c0>x)\k<π>`}},
		"error: nonjoiner cannot start an identifier":   {false, []string{"(?<" + string(rune(0x200c)) + ">x)", `(?<` + u + `200c>x)`}},
		"success: class backspace equals eight":         {true, []string{`[\b-\x08]`, `[\x08-\b]`}},
		"error: range ends before backspace":            {false, []string{`[\b-\x07]`, `[\x09-\b]`}},
		"error: loose whitespace property spelling":     {false, []string{`\p{wspace}`, `\p{W_Space}`}},
		"success: alternatives and atoms":               {true, []string{"", `|a||`, `(?:.|()|/|\/)`, "[\n]\r", `[[]`, `[]`, `[^]`}},
		"success: assertions":                           {true, []string{`^\b(?=a)(?!b)(?<=c)(?<!d)\B$`, `(?=a|)`}},
		"success: quantifiers":                          {true, []string{`a?b+?c*?d??`, `a{0002}b{2,}c{0,0}?`, `a{2147483648,2147483647}`}},
		"success: scoped modifiers":                     {true, []string{`(?ims:x)`, `(?s-im:x)`, `(?-s:x)`, `(?i-:x)`}},
		"success: literal escapes":                      {true, []string{`\f\n\r\t\v\0\cZ\ca\xAb` + u + `ABcd`, `\^\$\\\.\*\+\?\(\)\[\]\{\}\|\/`, `\u{0000000041}`, `\u{10FFFF}`}},
		"success: class ranges":                         {true, []string{`[\b-\t\-\x30-9]`, `[-a-]`, `[` + u + `D83D` + u + `DE00-😀]`, `[\uD800-\uDFFF]`, `[a-b-\d]`, `\w\W\s\S\d\D`}},
		"success: exact property aliases":               {true, []string{`\p{General_Category=Cased_Letter}`, `\p{gc=cntrl}`, `\P{digit}`, `\p{sc=Qaac}`, `\p{scx=Qaai}`, `\P{Script=Unknown}`, `\p{CWKCF}`, `\p{space}`, `\p{sc=Garay}`}},
		"success: numeric and named forward references": {true, []string{`\2(a)(\1)`, `\k<n>(?<` + u + `006e>.)`, `(?<$π1_` + u + `200D>x)\k<$π1_` + u + `200D>`, `(?<` + u + `D801` + u + `DC00>x)`, `(?<a` + u + `003Ex)`}},
		"success: disjoint duplicate names":             {true, []string{`(?:(?<x>a)|(?<x>b))\k<x>`, `(?<x>a)|(?:(?<y>b)|(?<x>c))`}},
		"error: delimiters":                             {false, []string{`(`, `)`, `]`, `}`, `[a`, `\`, `(?q:a)`, `(?>a)`}},
		"error: quantifiers":                            {false, []string{`?`, `a{`, `a{,2}`, `a{1, 2}`, `a{1`, `a{2,1}`, `a*??`, `^+`, `(?<=x)?`, `\b{2}`}},
		"error: scoped modifiers":                       {false, []string{`(?u:x)`, `(?i-i:x)`, `(?i--s:x)`, `(?-:x)`, `(?i=x)`}},
		"error: escape syntax":                          {false, []string{`\a`, `\_`, `\-`, `\x0`, `\u{}`, `\u{110000}`, `\u0hh0`, `\08`, `\cſ`, `\c0`, `[\B]`, `[\9]`}},
		"error: invalid ranges":                         {false, []string{`[z-a]`, `[\d-a]`, `[a-\p{L}]`, `[😀-￿]`, `[𐀀-\uDFFF]`}},
		"error: property names or values":               {false, []string{`\p`, `\p{`, `\p{Lu`, `\p{}`, `\p{LC=Lu}`, `\p{sc=}`, `\p{sc= Latn}`, `\p{sc=LAtn}`, `\p{Alphabetic=Yes}`, `\p{RGI_Emoji}`, `\p{Other_ID_Start}`, `\p{sc=Hrkt}`, `\p{sc=Sidetic}`, `\p{Script=Katakana_Or_Hiragana}`}},
		"error: missing references or invalid names":    {false, []string{`\2(a)`, `\k<x>`, `\k`, `(?<a>x)\k<A>`, `(?<1>a)`, `(?<>x)`, `(?<\x61>a)`, `(?<\u{88F}>x)`, `(?<\uD800>x)`}},
		"error: participating duplicates":               {false, []string{`(?<a>)(?<a>)`, `(?<a>(?<a>))`, `(?:(?<a>)|b)(?:(?<a>)|c)`, `(?<x>)|(?<y>)(?<y>)`}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			for _, pattern := range tc.patterns {
				if diff := gocmp.Diff(tc.valid, validUnicodePattern(utf16.Encode([]rune(pattern)))); diff != "" {
					t.Errorf("%q: Unicode verdict (-want +got):\n%s", pattern, diff)
				}
			}
		})
	}
}
