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

package pins_test

import (
	"math"
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/pins"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) { venue.Require(); os.Exit(m.Run()) }

func equal[T any](t *testing.T, want, got T) {
	t.Helper()
	if diff := gocmp.Diff(want, got, cmpopts.EquateNaNs()); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func TestCompare(t *testing.T) {
	const baseline = "claude-opus-5-5[1m]"
	tests := map[string]struct {
		first, second string
		sign          float64
		ok            bool
	}{
		"success: unknown family":      {"claude-NewFamily-4-10", "claude-newfamily-4-9", 1, true},
		"success: older minor":         {"claude-opus-5-4[1m]", baseline, -1, true},
		"success: equal":               {baseline, baseline, 0, true},
		"success: newer major":         {"claude-opus-6-0[1m]", baseline, 1, true},
		"success: omitted minor":       {"claude-opus-5[1m]", baseline, -1, true},
		"error: unrelated family":      {"claude-sonnet-5[1m]", baseline, 0, false},
		"error: absent major":          {"claude-opus", baseline, 0, false},
		"error: alpha minor":           {"claude-opus-5-alpha", baseline, 0, false},
		"error: wrong prefix":          {"other-opus-5", baseline, 0, false},
		"success: numeric not lexical": {"claude-OPUS-5-10", baseline, 1, true},
		"success: missing and explicit zero": {
			"claude-opus-5[1m]",
			"claude-opus-5-0",
			0,
			true,
		},
		"error: invalid right operand":              {baseline, "claude-opus-", 0, false},
		"success: NaN major falls through to minor": {"claude-opus-NaN-9", baseline, 1, true},
		"success: NaN minor yields NaN": {
			"claude-opus-5-NaN",
			baseline,
			math.NaN(),
			true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			sign, ok := pins.Compare(tt.first, tt.second)
			equal(t, tt.ok, ok)
			if ok {
				equal(t, tt.sign, sign)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	tests := map[string]struct {
		id   string
		want *pins.Version
	}{
		"success: bare major": {"claude-OpUs-0", &pins.Version{Family: "opus"}},
		"success: explicit minor": {
			"claude-my.family-10-3[1m]",
			&pins.Version{Family: "my.family", Major: 10, Minor: 3},
		},
		"success: empty family": {"claude--5", &pins.Version{Major: 5}},
		"success: NaN round trips": {
			"claude-fable-NaN-NaN",
			&pins.Version{Family: "fable", Major: math.NaN(), Minor: math.NaN()},
		},
		"success: large canonical number": {
			"claude-opus-100000000000000000000",
			&pins.Version{Family: "opus", Major: 1e20},
		},
		"error: rounded integer":       {"claude-opus-9007199254740993", nil},
		"error: exponential rendering": {"claude-opus-1000000000000000000000", nil},
		"error: wrong case prefix": {
			"Claude-opus-5",
			nil,
		},
		"error: upper suffix":   {"claude-opus-5[1M]", nil},
		"error: doubled suffix": {"claude-opus-5[1m][1m]", nil},
		"error: leading zero major": {
			"claude-opus-05",
			nil,
		},
		"error: leading zero minor": {"claude-opus-5-01", nil},
		"error: signed":             {"claude-opus-+5", nil},
		"error: negative":           {"claude-opus--5", nil},
		"error: exponent":           {"claude-opus-1e2", nil},
		"error: decimal":            {"claude-opus-5.1", nil},
		"error: spaces":             {"claude-opus- 5", nil},
		"error: empty minor":        {"claude-opus-5-", nil},
		"error: numeric prefix":     {"claude-opus-5a", nil},
		"error: dated":              {"claude-opus-4-8-20251101", nil},
		"error: prerelease":         {"claude-opus-5-0-beta", nil},
		"error: unknown suffix":     {"claude-opus-5[2m]", nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := pins.ParseVersion(tt.id)
			equal(t, tt.want != nil, ok)
			if tt.want != nil {
				equal(t, *tt.want, got)
			}
		})
	}
}

func TestNormalizeDetected(t *testing.T) {
	tests := map[string]struct{ value, want string }{
		"success: preserve spelling remove every marker": {
			" \tCLAUDE-extra-[1M]OPUS-next.v_2[1m] ",
			"CLAUDE-extra-OPUS-next.v_2[1m]",
		},
		"success: terminal alias": {"claude-extra-opus", "claude-extra-opus[1m]"},
		"error: partial family": {
			"claude-opus2-6",
			"",
		},
		"error: dotted family boundary": {"claude-dev.opus-5", ""},
		"error: missing prefix":         {"opus-5", ""},
		"error: illegal character":      {"claude-opus-5/1", ""},
		"error: first character":        {"claude-_dev-opus", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := pins.NormalizeDetected("opus", tt.value)
			equal(t, tt.want != "", ok)
			if ok {
				equal(t, tt.want, got)
			}
		})
	}
}

func TestValidOverride(t *testing.T) {
	tests := map[string]struct {
		value string
		want  bool
	}{
		"success: custom spelling":              {"custom:/target@v+1[1m]", true},
		"success: whitespace not in ECMAScript": {"a\u0085b", true},
		"error: empty":                          {"", false},
		"error: leading space":                  {" value", false},
		"error: trailing space":                 {"value ", false},
		"error: internal whitespace":            {"a b", false},
		"error: byte order mark":                {"a" + string(rune(0xfeff)) + "b", false},
	}
	for _, r := range "\x7f\"'`$\\;&|<>(){}" {
		tests["error: forbidden "+string(r)] = struct {
			value string
			want  bool
		}{"a" + string(r) + "b", false}
	}
	for r := range 32 {
		tests["error: control "+string(rune('A'+r))] = struct {
			value string
			want  bool
		}{"a" + string(rune(r)) + "b", false}
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { equal(t, tt.want, pins.ValidOverride(tt.value)) })
	}
}
