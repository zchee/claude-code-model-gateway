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

package modeldata_test

import (
	"math"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog/modeldata"
)

// mdatTCall runs f and fails only the calling row when it panics.
func mdatTCall[T any](t *testing.T, f func() T) (got T) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	return f()
}

// mdatTEqual compares with NaN equal to NaN, since a NaN field round-trips through an ID.
func mdatTEqual[T any](t *testing.T, want, got T) {
	t.Helper()
	if diff := gocmp.Diff(want, got, cmpopts.EquateNaNs()); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

// mdatTParsed holds both results of ParseVersion.
type mdatTParsed struct {
	Version modeldata.Version
	OK      bool
}

// mdatTCompared holds both results of Compare.
type mdatTCompared struct {
	Sign float64
	OK   bool
}

// TestParseVersion decides the shared canonical pin parser the pin and catalog packages both use:
// a lowercased family, a canonical decimal major and minor, an omitted minor as zero, and only the
// one [1m] window suffix.
func TestParseVersion(t *testing.T) {
	digits := strings.Repeat("9", 100_000)
	tests := map[string]struct {
		id   string
		want *modeldata.Version
	}{
		// Wrong: keeps the family's spelling instead of lowercasing it.
		"success: bare major": {"claude-OpUs-0", &modeldata.Version{Family: "opus"}},
		// Wrong: refuses a dotted family or keeps the window suffix in the minor.
		"success: explicit minor": {"claude-my.family-10-3[1m]", &modeldata.Version{Family: "my.family", Major: 10, Minor: 3}},
		// Wrong: refuses an ID without a minor instead of treating the minor as zero.
		"success: omitted minor is zero": {"claude-opus-5[1m]", &modeldata.Version{Family: "opus", Major: 5}},
		// Wrong: requires a nonempty family.
		"success: empty family": {"claude--5", &modeldata.Version{Major: 5}},
		// Wrong: refuses the NaN spelling that JavaScript's parseInt and String round-trip.
		"success: NaN round trips": {"claude-fable-NaN-NaN", &modeldata.Version{Family: "fable", Major: math.NaN(), Minor: math.NaN()}},
		// Wrong: limits the major to a machine integer.
		"success: large canonical number": {"claude-opus-100000000000000000000", &modeldata.Version{Family: "opus", Major: 1e20}},
		// Wrong: accepts a number that does not survive conversion to a double.
		"error: rounded integer": {"claude-opus-9007199254740993", nil},
		// Wrong: accepts a number JavaScript would print in exponential notation.
		"error: exponential rendering": {"claude-opus-1000000000000000000000", nil},
		// Wrong: matches the prefix case-insensitively.
		"error: wrong case prefix": {"Claude-opus-5", nil},
		// Wrong: matches the window suffix case-insensitively.
		"error: upper suffix": {"claude-opus-5[1M]", nil},
		// Wrong: strips the window suffix more than once.
		"error: doubled suffix": {"claude-opus-5[1m][1m]", nil},
		// Wrong: accepts a leading zero in the major.
		"error: leading zero major": {"claude-opus-05", nil},
		// Wrong: accepts a leading zero in the minor.
		"error: leading zero minor": {"claude-opus-5-01", nil},
		// Wrong: accepts a sign.
		"error: signed": {"claude-opus-+5", nil},
		// Wrong: accepts a negative major.
		"error: negative": {"claude-opus--5", nil},
		// Wrong: accepts an exponent.
		"error: exponent": {"claude-opus-1e2", nil},
		// Wrong: accepts a decimal point in a number field.
		"error: decimal": {"claude-opus-5.1", nil},
		// Wrong: trims space inside a number field.
		"error: spaces": {"claude-opus- 5", nil},
		// Wrong: treats an empty minor as zero.
		"error: empty minor": {"claude-opus-5-", nil},
		// Wrong: accepts a numeric prefix as the number.
		"error: numeric prefix": {"claude-opus-5a", nil},
		// Wrong: accepts a dated snapshot ID as a canonical pin.
		"error: dated": {"claude-opus-4-8-20251101", nil},
		// Wrong: accepts a prerelease field after the minor.
		"error: prerelease": {"claude-opus-5-0-beta", nil},
		// Wrong: strips any bracketed suffix, not only the one-million window.
		"error: unknown suffix": {"claude-opus-5[2m]", nil},
		// Wrong: accepts a non-numeric minor.
		"error: alpha minor": {"claude-opus-5-alpha", nil},
		// Wrong: accepts an ID without a major.
		"error: absent major": {"claude-opus", nil},
		// Wrong: accepts another prefix.
		"error: wrong prefix": {"other-opus-5", nil},
		// Wrong: parses bytes that are not UTF-8.
		"error: invalid UTF-8": {"claude-op\xffus-5", nil},
		// Wrong: lowercases a family that is not UTF-8.
		"error: malformed family": {"claude-" + string([]byte{0xff}) + "-1", nil},
		// Wrong: converts a major too long to round-trip instead of refusing it.
		"error: oversized major": {"claude-opus-" + digits, nil},
		// Wrong: converts a minor too long to round-trip instead of refusing it.
		"error: oversized minor": {"claude-opus-1-" + digits, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := mdatTCall(t, func() mdatTParsed {
				v, ok := modeldata.ParseVersion(tt.id)
				return mdatTParsed{v, ok}
			})
			mdatTEqual(t, tt.want != nil, got.OK)
			if tt.want != nil {
				mdatTEqual(t, *tt.want, got.Version)
			}
		})
	}
}

// TestCompare decides the shared version comparison: the sign of the major difference, else of the
// minor difference, only for two parseable IDs of the same lowercased family.
func TestCompare(t *testing.T) {
	const baseline = "claude-opus-5-5[1m]"
	// JavaScript sees the B after all the combining marks and lowercases the capital sigma as non-final;
	// the bounded casing context of the landed Go code gives a final sigma, so the families differ.
	marks := strings.Repeat(string(rune(0x0301)), 31)
	tests := map[string]struct {
		first, second string
		want          mdatTCompared
	}{
		// Wrong: compares only within the shipped families.
		"success: unknown family": {"claude-NewFamily-4-10", "claude-newfamily-4-9", mdatTCompared{1, true}},
		// Wrong: ignores the minor when the majors are equal.
		"success: older minor": {"claude-opus-5-4[1m]", baseline, mdatTCompared{-1, true}},
		// Wrong: reports equal IDs as unrelated.
		"success: equal": {baseline, baseline, mdatTCompared{0, true}},
		// Wrong: lets the minor outweigh the major.
		"success: newer major": {"claude-opus-6-0[1m]", baseline, mdatTCompared{1, true}},
		// Wrong: refuses an omitted minor instead of reading it as zero.
		"success: omitted minor": {"claude-opus-5[1m]", baseline, mdatTCompared{-1, true}},
		// Wrong: compares the fields as text, so 10 sorts before 5.
		"success: numeric not lexical": {"claude-OPUS-5-10", baseline, mdatTCompared{1, true}},
		// Wrong: distinguishes an omitted minor from an explicit zero.
		"success: missing and explicit zero": {"claude-opus-5[1m]", "claude-opus-5-0", mdatTCompared{0, true}},
		// Wrong: returns the difference itself instead of its sign.
		"success: sign not magnitude": {"claude-opus-9-1", "claude-opus-5-5", mdatTCompared{1, true}},
		// Wrong: stops at a NaN major difference instead of comparing the minors.
		"success: NaN major falls through to minor": {"claude-opus-NaN-9", baseline, mdatTCompared{1, true}},
		// Wrong: turns a NaN minor difference into a sign.
		"success: NaN minor yields NaN": {"claude-opus-5-NaN", baseline, mdatTCompared{math.NaN(), true}},
		// Wrong: compares IDs of different families.
		"error: unrelated family": {"claude-sonnet-5[1m]", baseline, mdatTCompared{}},
		// Wrong: accepts an ID without a major on the left.
		"error: absent major": {"claude-opus", baseline, mdatTCompared{}},
		// Wrong: accepts a non-numeric minor on the left.
		"error: alpha minor": {"claude-opus-5-alpha", baseline, mdatTCompared{}},
		// Wrong: accepts another prefix on the left.
		"error: wrong prefix": {"other-opus-5", baseline, mdatTCompared{}},
		// Wrong: validates only the left operand.
		"error: invalid right operand": {baseline, "claude-opus-", mdatTCompared{}},
		// Wrong: lowercases with unbounded lookahead, which makes these two families equal.
		"error: sigma before 31 combining marks": {"claude-AΣ" + marks + "B-6", "claude-aσ" + marks + "b-5", mdatTCompared{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := mdatTCall(t, func() mdatTCompared {
				sign, ok := modeldata.Compare(tt.first, tt.second)
				return mdatTCompared{sign, ok}
			})
			mdatTEqual(t, tt.want.OK, got.OK)
			if tt.want.OK {
				mdatTEqual(t, tt.want.Sign, got.Sign)
			}
		})
	}
}

// TestDisplayName decides the shared display name: the capitalized family and the major, plus the
// minor only when the ID writes it, and the ID unchanged when it does not parse or has no family.
func TestDisplayName(t *testing.T) {
	tests := map[string]struct{ id, want string }{
		// Wrong: drops the minor or keeps the window suffix.
		"success: explicit minor": {"claude-opus-5-5[1m]", "Opus 5.5"},
		// Wrong: drops an explicitly written zero minor.
		"success: explicit zero minor is retained": {"claude-opus-5-0", "Opus 5.0"},
		// Wrong: invents a zero minor the ID does not write.
		"success: omitted minor is not invented": {"claude-opus-5[1m]", "Opus 5"},
		// Wrong: capitalizes the original spelling instead of the lowercased family.
		"success: family is lowercased before capitalizing": {"claude-OPUS-6", "Opus 6"},
		// Wrong: capitalizes every dotted word or the whole family, not only its first letter.
		"success: dotted family": {"claude-my.family-10-3[1m]", "My.family 10.3"},
		// Wrong: maps the first letter to a single upper-case rune.
		"success: uppercase may expand": {"claude-ß-2-0", "SS 2.0"},
		// Wrong: capitalizes a first letter outside the basic multilingual plane.
		"success: first astral letter stays lowercase": {"claude-𐐨-2", "𐐨 2"},
		// Wrong: formats an ID that does not parse.
		"error: malformed negative minor retains spelling": {"claude-opus-5--3", "claude-opus-5--3"},
		// Wrong: formats an ID with an empty family.
		"error: empty family retains spelling": {"claude--5", "claude--5"},
		// Wrong: formats a bare alias.
		"error: alias retains spelling": {"opus", "opus"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			mdatTEqual(t, tt.want, mdatTCall(t, func() string { return modeldata.DisplayName(tt.id) }))
		})
	}
}
