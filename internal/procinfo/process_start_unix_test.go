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

//go:build darwin || linux

package procinfo

import (
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

// oneASCIIPunctuation reports whether sep is exactly one byte of the ASCII
// punctuation class: every printable character that is neither a letter, a
// digit nor a space.
func oneASCIIPunctuation(sep string) bool {
	if len(sep) != 1 {
		return false
	}
	switch c := sep[0]; {
	case '!' <= c && c <= '/', ':' <= c && c <= '@', '[' <= c && c <= '`', '{' <= c && c <= '~':
		return true
	default:
		return false
	}
}

func TestOneASCIIPunctuation(t *testing.T) {
	// eachByte splits s into one-byte strings, also where s is not valid UTF-8.
	eachByte := func(s string) []string {
		seps := make([]string, len(s))
		for i := range len(s) {
			seps[i] = s[i : i+1]
		}
		return seps
	}
	tests := map[string]struct {
		seps []string
		want bool
	}{
		"success: every punctuation byte": {seps: eachByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"), want: true},
		"error: letters":                  {seps: eachByte("azAZ")},
		"error: digits":                   {seps: eachByte("09")},
		"error: space and control bytes":  {seps: eachByte(" \t\n\x00\x7f")},
		"error: bytes beyond ASCII":       {seps: eachByte("\x80\xff")},
		"error: not exactly one byte":     {seps: []string{"", "//", "é"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, sep := range tt.seps {
				if got := oneASCIIPunctuation(sep); got != tt.want {
					t.Errorf("oneASCIIPunctuation(%q) = %t; want %t", sep, got, tt.want)
				}
			}
		})
	}
}

func TestPublishedRecordStartToken(t *testing.T) {
	for name, tt := range recordStartTokenCases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			record, wire := recordWithToken(t, root, tt.token, tt.suffix)
			if err := WriteRecord(root, "guardian", record); err != nil {
				t.Fatalf("publish record: %v", err)
			}
			if diff := gocmp.Diff(wire, readFile(t, filepath.Join(root, "guardian.pid.json"))); diff != "" {
				t.Errorf("published bytes (-want +got): %s", diff)
			}
			got, err := ReadRecord(root, "guardian")
			if err != nil {
				t.Fatalf("read record: %v", err)
			}
			if diff := gocmp.Diff(record, got); diff != "" {
				t.Errorf("read record (-want +got): %s", diff)
			}
		})
	}
}
