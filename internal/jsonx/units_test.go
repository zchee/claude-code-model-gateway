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

package jsonx_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// unitsOf encodes s as UTF-16 and replaces the markers <hi> and <lo> with the unpaired surrogates
// U+D800 and U+DC00, which no Go string can hold.
func unitsOf(s string) []uint16 {
	var out []uint16
	for s != "" {
		switch {
		case strings.HasPrefix(s, "<hi>"):
			out, s = append(out, 0xd800), s[len("<hi>"):]
		case strings.HasPrefix(s, "<lo>"):
			out, s = append(out, 0xdc00), s[len("<lo>"):]
		default:
			r, width := utf8.DecodeRuneInString(s)
			out, s = utf16.AppendRune(out, r), s[width:]
		}
	}
	return out
}

func TestDecodeWireUnits(t *testing.T) {
	tests := map[string]struct {
		input   string
		want    string
		members int
		wantErr bool
	}{
		"success: lone high surrogate in a string stays a unit":      {input: `{"k":"<hi>"}`, want: `{"k":"\ud800"}`, members: 1},
		"success: lone low surrogate between characters":             {input: `["a<lo>b"]`, want: `["a\udc00b"]`},
		"success: reversed pair stays two units":                     {input: `["<lo><hi>"]`, want: `["\udc00\ud800"]`},
		"success: escaped lone surrogate stays a unit":               {input: `{"k":"\ud800"}`, want: `{"k":"\ud800"}`, members: 1},
		"success: pair is one character":                             {input: "[\"\U0001F600\"]", want: "[\"\U0001F600\"]"},
		"success: lone key differs from the replacement character":   {input: "{\"<hi>\":1,\"\ufffd\":2}", want: "{\"\\ud800\":1,\"\ufffd\":2}", members: 2},
		"success: escaped backslash before a lone unit":              {input: `["\\<hi>"]`, want: `["\\\ud800"]`},
		"success: duplicate names collapse as in DecodeWire":         {input: `{"<hi>":1,"\ud800":2}`, want: `{"\ud800":2}`, members: 1},
		"success: whitespace and numbers as in DecodeWire":           {input: " \t[1e400, -0, 0.1]\r\n", want: `[null,0,0.1]`},
		"error: lone unit outside a string":                          {input: `[1]<hi>`, wantErr: true},
		"error: lone unit before a document":                         {input: `<lo>[1]`, wantErr: true},
		"error: lone unit right after the escape backslash":          {input: `["\<hi>"]`, wantErr: true},
		"error: pair right after the escape backslash":               {input: "[\"\\\U0001F600\"]", wantErr: true},
		"error: lone unit among unicode escape digits":               {input: `["\u12<hi>"]`, wantErr: true},
		"error: lone unit as an object name outside quotes":          {input: `{<hi>:1}`, wantErr: true},
		"success: raw high half then escaped low half form one pair": {input: `["<hi>\udc00"]`, want: "[\"\U00010000\"]"},
		"success: escaped high half then raw low half form one pair": {input: `["\ud800<lo>"]`, want: "[\"\U00010000\"]"},
		"success: line and paragraph separators inside literals":     {input: "{\"\u2028\":\"a\u2029b\"}", want: "{\"\u2028\":\"a\u2029b\"}", members: 1},
		"error: line separator outside a literal":                    {input: "[1,\u20282]", wantErr: true},
		"error: trailing value":                                      {input: `[1] [2]`, wantErr: true},
		"error: empty input":                                         {input: ``, wantErr: true},
		"error: byte order mark is not JSON white space":             {input: "\ufeff[1]", wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := jsonx.DecodeWireUnits(unitsOf(tt.input))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DecodeWireUnits(%q) accepted the input", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeWireUnits(%q): %v", tt.input, err)
			}
			wire, err := jsonx.EncodeWire(got)
			if err != nil {
				t.Fatalf("EncodeWire: %v", err)
			}
			if diff := gocmp.Diff(tt.want, string(wire)); diff != "" {
				t.Errorf("DecodeWireUnits(%q) re-encoded (-want +got):\n%s", tt.input, diff)
			}
			if got.Kind == '{' {
				if diff := gocmp.Diff(tt.members, len(got.Members)); diff != "" {
					t.Errorf("member count (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestDecodeWireUnitsMatchesDecodeWire checks that a document without unpaired surrogates decodes
// to the same tree from its UTF-16 units as from its UTF-8 bytes.
func TestDecodeWireUnitsMatchesDecodeWire(t *testing.T) {
	tests := map[string]struct{ input string }{
		"success: nested members with index names": {`{"b":{"2":[true,false,null],"10":"x"},"a":1}`},
		"success: escapes and astral characters":   {"[\"\\n\\t\\\"\\\\\\u0000\",\"\U0001F600\u00e9\"]"},
		"success: large integers":                  {`[9007199254740993,1e-7,123456789012345678901234567890]`},
		"success: scalar document":                 {`"only"`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want, err := jsonx.DecodeWire([]byte(tt.input))
			if err != nil {
				t.Fatalf("DecodeWire: %v", err)
			}
			got, err := jsonx.DecodeWireUnits(utf16.Encode([]rune(tt.input)))
			if err != nil {
				t.Fatalf("DecodeWireUnits: %v", err)
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("trees differ (-DecodeWire +DecodeWireUnits):\n%s", diff)
			}
		})
	}
}

func TestDecodeWireUnitsDepth(t *testing.T) {
	tests := map[string]struct {
		depth   int
		wantErr bool
	}{
		"success: 10000 nested containers": {depth: 10000},
		"error: 10001 nested containers":   {depth: 10001, wantErr: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			doc := strings.Repeat("[", tt.depth) + strings.Repeat("]", tt.depth)
			_, err := jsonx.DecodeWireUnits(utf16.Encode([]rune(doc)))
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("depth %d: %v", tt.depth, err)
				}
				return
			}
			if _, ok := errors.AsType[*jsonx.WireDepthError](err); !ok {
				t.Fatalf("depth %d: error %v, want *jsonx.WireDepthError", tt.depth, err)
			}
		})
	}
}

// TestDecodeWireUnitsErrorNamesNoContent checks that a syntax error repeats no part of the input:
// the tokenizer's own error quotes member names through its JSON pointer.
func TestDecodeWireUnitsErrorNamesNoContent(t *testing.T) {
	tests := map[string]struct{ input string }{
		"error: malformed array under a member":   {`{"private-label":[?]}`},
		"error: missing value after a member":     {`{"private-label":}`},
		"error: lone unit inside a nested member": {`{"private-label":{"x":1}<hi>}`},
		"error: bad escape in a member value":     {`{"private-label":"\q"}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := jsonx.DecodeWireUnits(unitsOf(tt.input))
			if err == nil {
				t.Fatalf("DecodeWireUnits(%q) accepted the input", tt.input)
			}
			if strings.Contains(err.Error(), "private-label") {
				t.Errorf("error %q names input content", err)
			}
		})
	}
}
