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
	"slices"
	"testing"
	"unicode/utf16"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

func TestCompatibilityDiagnostic(t *testing.T) {
	err := &CompatibilityError{
		ReasonCode: "invalid-regex",
		ToolName:   []uint16{'x', 0xdfff, 0xd83d, 0xde00},
		Pointer:    []uint16{'/', 0xd800, '/', 0xfffd},
	}
	if diff := gocmp.Diff(`Codex schema compatibility refused x\udfff😀 at /\ud800/� (invalid-regex)`, err.Error()); diff != "" {
		t.Errorf("diagnostic (-want +got):\n%s", diff)
	}
}

func TestPotentialProperty(t *testing.T) {
	tests := map[string]struct {
		pattern string
		want    bool
	}{
		"literal": {`^\\p{Cc}$`, false}, "property": {`\p{Cc}`, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := containsPotentialProperty(utf16.Encode([]rune(tc.pattern))); got != tc.want {
				t.Errorf("%q: got %t, want %t", tc.pattern, got, tc.want)
			}
		})
	}
}

func TestAcceptedEscapeKinds(t *testing.T) {
	want := []string{"character-class-shorthand", "control-character", "control-whitespace", "escaped-punctuation", "hexadecimal-code-unit", "null-character", "unicode-code-point", "unicode-property", "word-boundary-or-backspace"}
	got := slices.Clone(acceptedEscapeKinds())
	slices.Sort(got)
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("escape kinds (-want +got):\n%s", diff)
	}
}

func toolsWithPattern(t *testing.T, pattern string) jsonx.WireValue {
	t.Helper()
	tools := decode(t, []byte(`[{"name":"search","description":"search by name","input_schema":{"type":"string","pattern":"","examples":["example"]}}]`))
	member(t, *schemaOf(t, tools), "pattern").Text = utf16.Encode([]rune(pattern))
	return tools
}

func TestUnicodeSyntax(t *testing.T) {
	const affected = `[^\p{Cc}x]`
	// Each neighboring case names the ECMA-262 RegExp Pattern production or
	// static semantics that sets its verdict. All pass the adapter's lexical scan.
	tests := map[string]struct {
		pattern string
		valid   bool
	}{
		"success: qualified script":             {`[^\p{Script=Greek}x]`, true},
		"error: bare script":                    {`[^\p{Greek}x]`, false},
		"error: unknown property":               {`[^\p{Not_A_Property}x]`, false},
		"success: only the original is checked": {`[^\0\p{Cc}1]`, true},
		"error: out of range code point":        {`[^\p{Cc}\u{110000}]`, false},
		"error: identity escape outside class":  {`\-` + affected, false},
		// RegExpUnicodeEscapeSequence: braced CodePoint has maximum value 0x10FFFF.
		"success: largest code point": {`\u{10FFFF}` + affected, true},
		// ClassEscape[+UnicodeMode] permits escaped hyphen; IdentityEscape outside does not.
		"success: escaped hyphen inside class": {`[\-]` + affected, true},
		// CharacterEscape: a null escape must not be followed by a DecimalDigit.
		"error: decimal digit after null": {`\01` + affected, false},
		// QuantifierPrefix early error: the lower decimal bound must not exceed the upper.
		"error: inverted bounds": {affected + `{3,2}`, false},
		"success: equal bounds":  {affected + `{2,2}?`, true},
		// Term: a quantifier follows an Atom, not an Assertion or another quantifier.
		"error: quantifier on assertion": {`(?=x)+` + affected, false},
		"error: repeated quantifier":     {affected + `**`, false},
		// Atom: an opened noncapturing group requires a closing parenthesis.
		"error: unfinished group": {`(?:` + affected, false},
		// ClassRanges early errors prohibit a CharacterClassEscape as a range endpoint in u mode.
		"error: shorthand range endpoint": {`[a-\d]` + affected, false},
		// UnicodePropertyValueExpression requires an exact property name or alias (no loose matching).
		"success: abbreviated script name": {`[^\p{sc=Grek}x]`, true},
		"error: property name case":        {`[^\p{script=Greek}x]`, false},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := validUnicodePattern(utf16.Encode([]rune(tc.pattern))); got != tc.valid {
				t.Errorf("Unicode grammar accepted %q: %t, want %t", tc.pattern, got, tc.valid)
			}
			_, edits, err := Adapt(toolsWithPattern(t, tc.pattern))
			if tc.valid {
				if err != nil || len(edits) != 1 {
					t.Errorf("want one edit, got %+v, %v", edits, err)
				}
				return
			}
			refusal, ok := errors.AsType[*CompatibilityError](err)
			if !ok || refusal == nil || refusal.ReasonCode != "invalid-regex" {
				t.Errorf("want invalid-regex, got %v", err)
			}
		})
	}
}

func FuzzAdaptPattern(f *testing.F) {
	for _, pattern := range []string{"", `[^\p{Cc}x]`, `[\P{ASCII}]`, `(?:[^\\p{Cc}a])`, `[^\p{sc=Grek}😀]`} {
		f.Add(pattern)
	}
	f.Fuzz(func(t *testing.T, pattern string) {
		input := toolsWithPattern(t, pattern)
		before := wire(t, input)
		out, edits, err := Adapt(input)
		if got := wire(t, input); got != before {
			t.Fatalf("mutated input: %s != %s", got, before)
		}
		path := utf16.Encode([]rune("/tools/0/input_schema/pattern"))
		if err != nil {
			if refusal, ok := errors.AsType[*CompatibilityError](err); !ok || refusal == nil || !slices.Equal(refusal.Pointer, path) || out.Kind != 0 || len(edits) != 0 {
				t.Errorf("unexpected refusal: %v, edits=%+v, kind=%s", err, edits, out.Kind)
			}
			return
		}
		switch len(edits) {
		case 0:
			if !identical(input, out) || wire(t, out) != before {
				t.Error("no edits but tools changed")
			}
		case 1:
			want := decode(t, []byte(before))
			target := member(t, *schemaOf(t, want), "pattern")
			if identical(input, out) || !slices.Equal(edits[0].Pointer, path) || slices.Equal(edits[0].Replacement, target.Text) {
				t.Fatalf("edit does not change the original pattern: %+v", edits)
			}
			target.Text = edits[0].Replacement
			if diff := gocmp.Diff(wire(t, want), wire(t, out)); diff != "" {
				t.Errorf("applying the reported edit did not reproduce the whole output (-want +got):\n%s", diff)
			}
		default:
			t.Fatalf("single pattern produced %d edits", len(edits))
		}
	})
}
