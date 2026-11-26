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
	"cmp"
	json "encoding/json/v2"
	"strings"
	"testing"
	"unicode/utf16"

	_ "embed"

	gocmp "github.com/google/go-cmp/cmp"
)

//go:embed testdata/node-supplement-inputs.json
var supplementInputs []byte

//go:embed testdata/node-supplement.json
var supplementGolden []byte

type supplementVerdict struct {
	Valid   bool
	Adapter *corpusResult
}

type supplementCase struct {
	pattern string
	engine  supplementVerdict
}

func readSupplement(t *testing.T) map[string]supplementCase {
	t.Helper()
	var inputs []struct {
		Name    string `json:"name"`
		Pattern string `json:"pattern"`
		Repeat  int    `json:"repeat"`
		Adapter bool   `json:"adapter"`
	}
	var golden struct {
		Direct map[string]string       `json:"direct"`
		Cases  map[string]corpusResult `json:"cases"`
	}
	if err := json.Unmarshal(supplementInputs, &inputs); err != nil {
		t.Fatal(err)
	}
	units := json.UnmarshalFunc(func(raw []byte, text *[]uint16) error {
		v := decode(t, raw)
		if v.Kind != '"' {
			t.Fatalf("expected string for UTF-16 field, got %s", raw)
		}
		*text = v.Text
		return nil
	})
	if err := json.Unmarshal(supplementGolden, &golden, json.WithUnmarshalers(units)); err != nil {
		t.Fatal(err)
	}
	if len(inputs) == 0 || len(golden.Cases) == 0 {
		t.Fatal("empty supplemental corpus")
	}
	cases := make(map[string]supplementCase, len(inputs)+len(golden.Cases))
	for _, in := range inputs {
		want, ok := golden.Direct[in.Name]
		if !ok || in.Name == "" || (want != "valid" && want != "SyntaxError") {
			t.Fatalf("missing, repeated or invalid constructor verdict for %q: %q", in.Name, want)
		}
		delete(golden.Direct, in.Name)
		pattern := strings.Repeat(in.Pattern, cmp.Or(in.Repeat, 1))
		cases["constructor/"+in.Name] = supplementCase{pattern, supplementVerdict{Valid: want == "valid"}}
		if in.Adapter {
			result, ok := golden.Cases[in.Name]
			if !ok {
				t.Fatalf("missing adapter result for %q", in.Name)
			}
			delete(golden.Cases, in.Name)
			cases["adapter/"+in.Name] = supplementCase{pattern, supplementVerdict{Adapter: &result}}
		}
	}
	if len(golden.Direct) != 0 || len(golden.Cases) != 0 {
		t.Fatalf("unexpected remaining verdicts: %d constructor, %d adapter", len(golden.Direct), len(golden.Cases))
	}
	return cases
}

func (c supplementCase) checkPort(t *testing.T, want supplementVerdict) {
	t.Helper()
	if want.Adapter == nil {
		got := validUnicodePattern(utf16.Encode([]rune(c.pattern)))
		if diff := gocmp.Diff(want.Valid, got); diff != "" {
			t.Errorf("constructor validity (-want +Go):\n%s", diff)
		}
		return
	}
	schema, err := json.Marshal(map[string]string{"pattern": c.pattern})
	if err != nil {
		t.Fatal(err)
	}
	checkCase(t, corpusInput{Schema: schema}, *want.Adapter, false)
}

type engineQuirk struct {
	reason string
	node   supplementVerdict
	port   supplementVerdict
}

func engineQuirks() map[string]engineQuirk {
	pointer := utf16.Encode([]rune("/tools/0/input_schema/pattern"))
	valid := supplementVerdict{Valid: true}
	invalid := supplementVerdict{Valid: false}
	rewritten := supplementVerdict{Adapter: &corpusResult{
		Same: new(false), Wire: new(`{"pattern":"[^x]"}`),
		Edits: []Edit{{Pointer: pointer, Replacement: utf16.Encode([]rune("[^x]"))}},
	}}
	refused := supplementVerdict{Adapter: &corpusResult{Error: &CompatibilityError{
		ReasonCode: "invalid-regex", Pointer: pointer, ToolName: utf16.Encode([]rune("artifact_tool")),
	}}}
	return map[string]engineQuirk{
		"constructor/property-letter":                   {"U+014C is not the ASCII category L.", valid, invalid},
		"adapter/property-letter":                       {"Do not rewrite a class by narrowing U+014C to L.", rewritten, refused},
		"constructor/property-punctuation":              {"U+0150 is not the ASCII category P.", valid, invalid},
		"adapter/property-punctuation":                  {"Do not rewrite a class by narrowing U+0150 to P.", rewritten, refused},
		"constructor/property-number":                   {"U+014E is not the ASCII category N.", valid, invalid},
		"adapter/property-number":                       {"Do not rewrite a class by narrowing U+014E to N.", rewritten, refused},
		"constructor/property-astral":                   {"U+1004C is not the ASCII category L.", valid, invalid},
		"adapter/property-astral":                       {"Do not rewrite a class by narrowing an astral character to L.", rewritten, refused},
		"constructor/property-uppercase-letter":         {"A non-ASCII initial does not spell the category Lu.", valid, invalid},
		"adapter/property-uppercase-letter":             {"Do not rewrite a class by treating Ōu as Lu.", rewritten, refused},
		"constructor/property-category-value":           {"An explicit general-category value must also be ASCII.", valid, invalid},
		"adapter/property-category-value":               {"Do not rewrite a class by narrowing the value of gc to L.", rewritten, refused},
		"constructor/property-script-value":             {"A non-ASCII initial does not spell the script alias Latn.", valid, invalid},
		"adapter/property-script-value":                 {"Do not rewrite a class by treating Ōatn as Latn.", rewritten, refused},
		"constructor/duplicate-later-branch":            {"The outer capture can participate with the capture in the later branch.", valid, invalid},
		"constructor/duplicate-later-branch-ref":        {"A trailing reference does not separate the outer and later-branch captures.", valid, invalid},
		"constructor/duplicate-successive-branches":     {"The later alternatives of successive groups can both participate.", valid, invalid},
		"constructor/duplicate-successive-branches-ref": {"A trailing reference does not make successive alternatives disjoint.", valid, invalid},
		"constructor/duplicate-nested":                  {"A nested later branch can participate with the outer capture.", valid, invalid},
		"constructor/duplicate-nested-ref":              {"A reference after the nested branches does not separate their captures.", valid, invalid},
	}
}

func TestNodeSupplement(t *testing.T) {
	quirks := engineQuirks()
	for name, c := range readSupplement(t) {
		if _, divergent := quirks[name]; !divergent {
			t.Run(name, func(t *testing.T) { c.checkPort(t, c.engine) })
		}
	}
}

func TestEngineQuirkRefusals(t *testing.T) {
	cases := readSupplement(t)
	for name, q := range engineQuirks() {
		t.Run(name, func(t *testing.T) {
			c, ok := cases[name]
			if !ok {
				t.Fatalf("missing engine observation for %s", name)
			}
			t.Logf("%s If the port and engine now agree, remove %s from the engine quirk table.", q.reason, name)
			if diff := gocmp.Diff(q.node, c.engine); diff != "" {
				t.Errorf("engine observation changed (-recorded +oracle):\n%s", diff)
			}
			c.checkPort(t, q.port)
		})
	}
}
