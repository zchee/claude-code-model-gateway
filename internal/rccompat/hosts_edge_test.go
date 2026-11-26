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

package rccompat_test

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/rccompat"
)

// TestAddBlockEdges covers the file-wide EOL choice, adoption, and append
// separator rules in lib/remote-control.js:23-34,49-78.
func TestAddBlockEdges(t *testing.T) {
	const crlfBlock = start + "\r\n" + entry + "\r\n" + end + "\r\n"
	const commented = "# " + entry + "\n"
	const eligible = "::1 api.anthropic.com # eligible"
	longRow := "127.0.0.1 " + strings.Repeat("alias.example ", 6_000) + "api.anthropic.com"
	tests := map[string]struct{ input, want string }{
		"success: CRLF after an LF-terminated matching row": {entry + "\n# after\r\n", crlfBlock + "# after\r\n"},
		"success: skip commented matching row before eligible row": {
			commented + eligible + "\n", commented + start + "\n" + eligible + "\n" + end + "\n",
		},
		"success: empty input":                   {"", block},
		"success: only line endings":             {"\n\r\n", "\n\r\n" + crlfBlock},
		"success: unterminated matching row":     {eligible, start + "\n" + eligible + "\n" + end + "\n"},
		"success: unterminated non-matching row": {"# retained", "# retained\n" + block},
		"success: lone CR is part of adopted row": {
			eligible + "\r", start + "\n" + eligible + "\r\n" + end + "\n",
		},
		"success: matching alias in a long row": {longRow + "\n", start + "\n" + longRow + "\n" + end + "\n"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, changed, err := rccompat.AddBlock(tc.input)
			if err != nil || !changed {
				t.Fatalf("AddBlock changed = %t, err = %v; want true, nil", changed, err)
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("hosts text (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRemoveBlockEdges covers substring splicing and whole-file newline
// collapsing in lib/remote-control.js:36-47,80-87.
func TestRemoveBlockEdges(t *testing.T) {
	tests := map[string]struct{ input, want string }{
		"success: collapse distant mixed and LF-only runs": {
			"# first\n\r\n\n# second\n# before\n" + block + "# after\n# later\n\n\n\n# last",
			"# first\r\n\r\n# second\n# before\n# after\n# later\n\n# last",
		},
		"success: keep bytes on inline marker lines outside the block": {
			"# prefix " + start + "\n" + entry + "\n" + end + " # suffix\r\n",
			"# prefix  # suffix\r\n",
		},
		"success: remove only the first of two valid blocks": {block + block, block},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, changed, err := rccompat.RemoveBlock(tc.input)
			if err != nil || !changed {
				t.Fatalf("RemoveBlock changed = %t, err = %v; want true, nil", changed, err)
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("hosts text (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBlockStateMarkerOccurrences covers independent substring searches in
// lib/remote-control.js:36-47.
func TestBlockStateMarkerOccurrences(t *testing.T) {
	tests := map[string]struct{ input, want string }{
		"success: markers within lines":              {"# prefix " + start + "\n" + entry + "\n" + end + " # suffix", "valid"},
		"partial: end precedes a later valid block":  {end + "\n" + block, "partial"},
		"invalid: first end excludes the later row":  {start + "\n" + end + "\n" + entry + "\n" + end, "invalid"},
		"success: first start includes an early row": {start + "\n" + entry + "\n" + start + "\n" + end, "valid"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, rccompat.BlockState(tc.input)); diff != "" {
				t.Errorf("block state (-want +got):\n%s", diff)
			}
		})
	}
}

// TestParseEntryWhitespace covers ECMAScript trimming and field splitting,
// distinct from the LF/CRLF row split in lib/remote-control.js:23-34.
func TestParseEntryWhitespace(t *testing.T) {
	const bom = string(rune(0xFEFF))
	const nel = string(rune(0x85))
	const bomSeparated = "127.0.0.1" + bom + "api.anthropic.com"
	const crSeparated = "127.0.0.1\rapi.anthropic.com"
	tests := map[string]struct {
		input string
		want  *rccompat.Entry
	}{
		"success: BOM trims and separates fields":    {bom + bomSeparated + bom, &rccompat.Entry{IP: "127.0.0.1", Line: bomSeparated}},
		"absent: NEL does not separate fields":       {input: "127.0.0.1" + nel + "api.anthropic.com"},
		"absent: NEL is not trimmed":                 {input: nel + entry},
		"success: lone CR separates fields":          {crSeparated, &rccompat.Entry{IP: "127.0.0.1", Line: crSeparated}},
		"absent: lone CR does not start another row": {input: "192.0.2.1 another.example\r" + entry},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, rccompat.ParseEntry(tc.input)); diff != "" {
				t.Errorf("entry (-want +got):\n%s", diff)
			}
		})
	}
}

// TestConflictsEdgeLines covers literal address comparison and end marker
// exclusion in lib/remote-control.js:8,89-101.
func TestConflictsEdgeLines(t *testing.T) {
	const malformed = "not-an-ip api.anthropic.com"
	tests := map[string]struct {
		input string
		want  []string
	}{
		"success: report malformed address token": {malformed + "\n", []string{malformed}},
		"success: ignore line containing end marker": {
			"192.0.2.8 api.anthropic.com " + end + "\n", []string{},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, rccompat.Conflicts(tc.input)); diff != "" {
				t.Errorf("conflicts (-want +got):\n%s", diff)
			}
		})
	}
}
