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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"os"
	"slices"
	"testing"
	"unicode/utf16"

	_ "embed"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

//go:embed testdata/node-inputs.json
var nodeInputs []byte

//go:embed testdata/node-golden.json
var nodeGolden []byte

type corpusInput struct {
	Name      string         `json:"name"`
	Schema    jsontext.Value `json:"schema"`
	Tools     jsontext.Value `json:"tools"`
	Instances jsontext.Value `json:"instances"`
	SelfCycle bool           `json:"selfCycle"`
}

type corpusResult struct {
	Wire      *string             `json:"wire"`
	Same      *bool               `json:"same"`
	SameTools []bool              `json:"sameTools"`
	SelfCycle *bool               `json:"selfCycle"`
	Edits     []Edit              `json:"edits"`
	Error     *CompatibilityError `json:"error"`
	Before    jsontext.Value      `json:"before"`
	After     jsontext.Value      `json:"after"`
}

func TestNodeCorpus(t *testing.T) {
	var inputs []corpusInput
	var golden struct {
		Cases map[string]corpusResult `json:"cases"`
	}
	if err := json.Unmarshal(nodeInputs, &inputs, jsontext.AllowInvalidUTF8(true)); err != nil {
		t.Fatal(err)
	}
	// Decode journal and refusal strings before any conversion to UTF-8; JSON
	// permits isolated surrogate escapes that a Go string decoder would replace.
	units := json.UnmarshalFunc(func(raw []byte, text *[]uint16) error {
		v, err := jsonx.DecodeWire(raw)
		if err != nil {
			return err
		}
		if v.Kind != '"' {
			return errors.New("expected a JSON string for UTF-16 field")
		}
		*text = v.Text
		return nil
	})
	if err := json.Unmarshal(nodeGolden, &golden, jsontext.AllowInvalidUTF8(true), json.WithUnmarshalers(units)); err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]corpusInput, len(inputs))
	for _, in := range inputs {
		if _, dup := byName[in.Name]; dup || in.Name == "" {
			t.Fatalf("duplicate or empty input name %q", in.Name)
		}
		if _, ok := golden.Cases[in.Name]; !ok {
			t.Fatalf("missing oracle result for %q", in.Name)
		}
		byName[in.Name] = in
	}
	if len(byName) != len(golden.Cases) || len(byName) == 0 {
		t.Fatalf("input names: %d, oracle results: %d", len(byName), len(golden.Cases))
	}

	// A source assertion inside a loop covers every member, not just one representative.
	tests := map[string]struct{ names []string }{
		"positive-applicators":     {[]string{"positive-root", "positive-properties", "positive-pattern-properties", "positive-additional-properties", "positive-items", "positive-prefix-items", "positive-all-of", "positive-any-of", "positive-dependent-schemas", "positive-property-names", "positive-unevaluated-properties", "positive-unevaluated-items"}},
		"unsupported-compositions": {[]string{"applicator-not", "applicator-if", "applicator-one-of", "applicator-contains", "applicator-defs", "applicator-definitions", "schema-ref", "applicator-content-schema", "applicator-unknown", "schema-dialect-older", "schema-anchor", "schema-vocabulary"}},
		"counterexamples":          {[]string{"counterexample-conditional-properties", "counterexample-all-of-properties", "counterexample-ref-conditional", "counterexample-pattern-key", "counterexample-conditional-items"}},
		"lexical-positive":         {[]string{"lexical-escaped-parens", "lexical-lookahead", "lexical-lookbehind", "lexical-nested-noncapture"}},
		"lexical-negative":         {[]string{"lexical-negative-lookahead-paren", "lexical-negative-lookahead-bracket", "lexical-negative-lookahead-class", "lexical-negative-lookbehind", "lexical-negative-nested"}},
		"lexical-even":             {[]string{"lexical-even-noncapture", "lexical-even-negative"}},
		"regex-refusals":           {[]string{"regex-outside-class", "regex-positive-class", "regex-empty-class", "regex-range", "regex-negative-context", "regex-capture", "regex-backreference", "regex-unsupported-escape", "regex-incomplete-unicode", "regex-property-no-braces"}},
	}
	run := func(t *testing.T, name string, requireValidPattern bool) {
		in, ok := byName[name]
		if !ok {
			t.Fatalf("missing or repeated case %q", name)
		}
		delete(byName, name)
		t.Run(name, func(t *testing.T) { checkCase(t, in, golden.Cases[name], requireValidPattern) })
	}
	// Partition before invoking any filtered subtest: -run may omit an entire group.
	grouped := make(map[string]bool)
	for group, tc := range tests {
		for _, name := range tc.names {
			grouped[name] = true
		}
		t.Run(group, func(t *testing.T) {
			for _, name := range tc.names {
				run(t, name, group == "lexical-negative")
			}
		})
	}
	for _, in := range inputs {
		if !grouped[in.Name] {
			run(t, in.Name, false)
		}
	}
}

func decode(t *testing.T, raw []byte) jsonx.WireValue {
	t.Helper()
	v, err := jsonx.DecodeWire(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func wire(t *testing.T, v jsonx.WireValue) string {
	t.Helper()
	b, err := jsonx.EncodeWire(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

func member(t *testing.T, v jsonx.WireValue, name string) *jsonx.WireValue {
	t.Helper()
	for i := range v.Members {
		if slices.Equal(v.Members[i].Name, utf16.Encode([]rune(name))) {
			return &v.Members[i].Value
		}
	}
	t.Fatalf("missing member %q", name)
	return nil
}

func schemaOf(t *testing.T, tools jsonx.WireValue) *jsonx.WireValue {
	t.Helper()
	if tools.Kind != '[' || len(tools.Elements) != 1 {
		t.Fatal("expected a single tool")
	}
	return member(t, tools.Elements[0], "input_schema")
}

// Give empty containers observable identity too: pointers to distinct zero-length slices can be equal.
func markEmpty(v *jsonx.WireValue) {
	switch v.Kind {
	case '{':
		if len(v.Members) == 0 {
			v.Members = make([]jsonx.Member, 0, 1)
		}
		for i := range v.Members {
			markEmpty(&v.Members[i].Value)
		}
	case '[':
		if len(v.Elements) == 0 {
			v.Elements = make([]jsonx.WireValue, 0, 1)
		}
		for i := range v.Elements {
			markEmpty(&v.Elements[i])
		}
	}
}

func sameBacking[T any](a, b []T) bool { return cap(a) > 0 && cap(b) > 0 && &a[:1][0] == &b[:1][0] }

func identical(a, b jsonx.WireValue) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case '{':
		return sameBacking(a.Members, b.Members)
	case '[':
		return sameBacking(a.Elements, b.Elements)
	default:
		return gocmp.Equal(a, b)
	}
}

func fixtureTools(t *testing.T, in corpusInput) jsonx.WireValue {
	t.Helper()
	if (in.Schema == nil) == (in.Tools == nil) {
		t.Fatal("expected exactly one of schema and tools")
	}
	raw := in.Tools
	if in.Schema != nil {
		raw = append([]byte(`[{"name":"artifact_tool","input_schema":`), in.Schema...)
		raw = append(raw, '}', ']')
	}
	tools := decode(t, raw)
	markEmpty(&tools)
	if in.SelfCycle {
		schema := schemaOf(t, tools)
		schema.Members = append(schema.Members, jsonx.Member{Name: utf16.Encode([]rune("self"))})
		schema.Members[len(schema.Members)-1].Value = *schema
	}
	return tools
}

func checkCase(t *testing.T, in corpusInput, want corpusResult, requireValidPattern bool) {
	t.Helper()
	tools := fixtureTools(t, in)
	snapshot := fixtureTools(t, in)
	if requireValidPattern && !validUnicodePattern(member(t, *schemaOf(t, tools), "pattern").Text) {
		t.Error("the original negative lookaround must compile in Unicode mode independently of its adapter refusal")
	}
	checkEvidence(t, in, want)
	got, edits, err := Adapt(tools)
	if diff := gocmp.Diff(snapshot, tools); diff != "" {
		t.Fatalf("input mutated (-before +after):\n%s", diff)
	}
	if in.SelfCycle && !identical(*schemaOf(t, tools), *member(t, *schemaOf(t, tools), "self")) {
		t.Fatal("original self link changed")
	}
	if want.Error != nil {
		refusal, ok := errors.AsType[*CompatibilityError](err)
		if !ok || refusal == nil {
			t.Fatalf("want compatibility refusal %+v, got %v", *want.Error, err)
		}
		if diff := gocmp.Diff(*want.Error, *refusal); diff != "" {
			t.Errorf("refusal (-want +got):\n%s", diff)
		}
		if got.Kind != 0 || len(edits) != 0 {
			t.Error("refusal returned partial output or edits")
		}
		return
	}
	if err != nil {
		t.Fatalf("adapt: %v", err)
	}
	if diff := gocmp.Diff(want.Edits, edits, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("edits (-want +got):\n%s", diff)
	}
	observed, original := got, tools
	if in.Schema != nil {
		observed, original = *schemaOf(t, got), *schemaOf(t, tools)
	}
	if want.Same == nil || identical(observed, original) != *want.Same {
		t.Errorf("unexpected identity: same=%t", identical(observed, original))
	}
	if want.SameTools != nil {
		if got.Kind != '[' || len(got.Elements) != len(want.SameTools) || len(tools.Elements) != len(want.SameTools) {
			t.Fatal("unexpected tool array shape")
		}
		for i, same := range want.SameTools {
			if identical(got.Elements[i], tools.Elements[i]) != same {
				t.Errorf("tool %d: want same=%t", i, same)
			}
		}
	}
	if in.SelfCycle {
		if want.Wire != nil || want.SelfCycle == nil || identical(observed, *member(t, observed, "self")) != *want.SelfCycle {
			t.Fatal("unexpected self link or wire for cyclic result")
		}
		return
	}
	if want.Wire == nil {
		t.Fatal("missing golden wire")
	}
	if diff := gocmp.Diff(*want.Wire, wire(t, observed)); diff != "" {
		t.Fatalf("wire (-want +got):\n%s", diff)
	}
	if in.Name == "sibling-regex" {
		repeated := member(t, *member(t, observed, "properties"), "repeated")
		before := member(t, *member(t, original, "properties"), "repeated")
		if identical(*repeated, *before) {
			t.Fatal("unchanged sibling inside the cloned schema shares object storage")
		}
		pattern := member(t, *repeated, "pattern")
		pattern.Text[0] = '!'
		if diff := gocmp.Diff(snapshot, tools); diff != "" {
			t.Fatalf("writing to cloned sibling mutated input (-before +after):\n%s", diff)
		}
	}
}

// The validator ran in the oracle venue. Bind its answers to the source instances;
// checkCase compares the actual adapter result with the very bytes it validated.
func checkEvidence(t *testing.T, in corpusInput, want corpusResult) {
	t.Helper()
	tests := map[string]struct{ instances, before, after string }{
		"artifact-name":                         {`["safe_name","bad/name","\u0000","__internal__",""]`, `[true,false,false,false,false]`, `[true,false,true,false,false]`},
		"unevaluated-object":                    {`[{"filename":"safe_name"},{"filename":"safe_name","extra":"nope"}]`, `[true,false]`, `[true,false]`},
		"unevaluated-array":                     {`[["safe_name"],["safe_name","extra"]]`, `[true,false]`, `[true,false]`},
		"counterexample-conditional-properties": {`[{"fallback":"plain"}]`, `[true]`, ""},
		"counterexample-all-of-properties":      {`[{"x-value":"plain"}]`, `[true]`, `[true]`},
		"counterexample-ref-conditional":        {`[{"fallback":"plain"}]`, `[true]`, ""},
		"counterexample-pattern-key":            {`[{"x-\u0000":"plain"}]`, `[true]`, ""},
		"counterexample-conditional-items":      {`[["plain"]]`, `[true]`, ""},
		"lexical-negative-lookahead-paren":      {`[")\u0000"]`, `[true]`, ""},
	}
	tc, ok := tests[in.Name]
	if !ok {
		return
	}
	for _, field := range []struct {
		name, expected string
		raw            jsontext.Value
	}{{"instances", tc.instances, in.Instances}, {"before", tc.before, want.Before}, {"after", tc.after, want.After}} {
		var actual string
		if field.raw != nil {
			actual = wire(t, decode(t, field.raw))
		}
		if diff := gocmp.Diff(field.expected, actual); diff != "" {
			t.Errorf("%s (-want +got):\n%s", field.name, diff)
		}
	}
}
