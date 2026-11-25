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
	"os"
	"strings"
	"testing"
	"testing/fstest"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/rccompat"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

const (
	start    = "# >>> model-gateway RC compatibility >>>"
	end      = "# <<< model-gateway RC compatibility <<<"
	entry    = "127.0.0.1 api.anthropic.com"
	block    = start + "\n" + entry + "\n" + end + "\n"
	original = "127.0.0.1 localhost\n192.168.1.20 internal.example\n"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestParseEntry(t *testing.T) {
	tests := map[string]struct{ line, ip string }{
		"success: IPv4":               {entry, "127.0.0.1"},
		"success: tab":                {"127.0.0.1\tapi.anthropic.com", "127.0.0.1"},
		"success: trailing comment":   {"127.0.0.1   api.anthropic.com   # model-gateway RC compatibility", "127.0.0.1"},
		"success: surrounding spaces": {"  127.0.0.1  api.anthropic.com  ", "127.0.0.1"},
		"success: uppercase":          {"127.0.0.1 API.ANTHROPIC.COM", "127.0.0.1"},
		"success: alias":              {"127.0.0.1 other.example.com api.anthropic.com", "127.0.0.1"},
		"success: IPv6":               {"::1 api.anthropic.com", "::1"},
		"success: trailing dot":       {entry + ".", "127.0.0.1"},
		"absent: comment":             {"# " + entry, ""},
		"absent: non-loopback":        {"192.168.1.50 api.anthropic.com", ""},
		"absent: prefixed hostname":   {"127.0.0.1 notapi.anthropic.com", ""},
		"absent: suffixed hostname":   {entry + ".evil.example", ""},
		"absent: different host":      {"127.0.0.1 someotherhost.com", ""},
		"absent: empty":               {},
		"absent: whitespace":          {"   ", ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var want *rccompat.Entry
			eol := "\n"
			if tc.ip != "" {
				want = &rccompat.Entry{IP: tc.ip, Line: strings.TrimSpace(tc.line)}
				eol = "\r\n"
			}
			if diff := gocmp.Diff(want, rccompat.ParseEntry(tc.line+eol)); diff != "" {
				t.Errorf("entry (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFirstEntry(t *testing.T) {
	text := "# managed by model-gateway\r\n255.255.255.255 broadcast.example\r\n127.0.0.1 localhost\r\n" + entry + "\r\n::1 api.anthropic.com"
	if diff := gocmp.Diff(&rccompat.Entry{IP: "127.0.0.1", Line: entry}, rccompat.ParseEntry(text)); diff != "" {
		t.Errorf("first entry (-want +got):\n%s", diff)
	}
}

func TestBlockState(t *testing.T) {
	tests := map[string]struct{ text, want string }{
		"absent":     {"127.0.0.1 localhost\n", "absent"},
		"start only": {start + "\n", "partial"},
		"end only":   {end + "\n", "partial"},
		"reversed":   {end + "\n" + start, "partial"},
		"valid":      {block, "valid"},
		"invalid":    {start + "\n127.0.0.1 localhost\n" + end + "\n", "invalid"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, rccompat.BlockState(tc.text)); diff != "" {
				t.Errorf("block state (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTransformBlock(t *testing.T) {
	const crlfBlock = start + "\r\n" + entry + "\r\n" + end + "\r\n"
	const alias = "  ::1 other.example API.ANTHROPIC.COM. # retained\t"
	tests := map[string]struct {
		transform   func(string) (string, bool, error)
		input, want string
		changed     bool
		problem     string
	}{
		"add":                                {rccompat.AddBlock, original, original + block, true, ""},
		"already added":                      {rccompat.AddBlock, original + block, original + block, false, ""},
		"remove":                             {rccompat.RemoveBlock, original + block, original, true, ""},
		"already removed":                    {rccompat.RemoveBlock, original, original, false, ""},
		"add partial":                        {transform: rccompat.AddBlock, input: start + "\n", problem: "partial"},
		"remove partial":                     {transform: rccompat.RemoveBlock, input: end + "\n", problem: "partial"},
		"add reversed":                       {transform: rccompat.AddBlock, input: end + "\n" + start, problem: "partial"},
		"remove reversed":                    {transform: rccompat.RemoveBlock, input: end + "\n" + start, problem: "partial"},
		"add invalid":                        {transform: rccompat.AddBlock, input: start + "\n" + end, problem: "invalid"},
		"remove invalid":                     {transform: rccompat.RemoveBlock, input: start + "\n" + end, problem: "invalid"},
		"append to unterminated line":        {rccompat.AddBlock, "# first\r\n# second", "# first\r\n# second\r\n" + crlfBlock, true, ""},
		"adopt first matching line verbatim": {rccompat.AddBlock, "# before\r\n" + alias + "\r\n" + entry, "# before\r\n" + start + "\r\n" + alias + "\r\n" + end + "\r\n" + entry, true, ""},
		"collapse surplus newlines":          {rccompat.RemoveBlock, "# before\r\n\r\n\r\n" + crlfBlock + "# after", "# before\r\n\r\n# after", true, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, changed, err := tc.transform(tc.input)
			if tc.problem != "" {
				want := "plugin-marked hosts block is " + tc.problem + "; run remote-control doctor and repair it manually"
				if err == nil || err.Error() != want {
					t.Errorf("error = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("hosts text (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tc.changed, changed); diff != "" {
				t.Errorf("changed (-want +got):\n%s", diff)
			}
		})
	}
}

func TestConflicts(t *testing.T) {
	const first = "203.0.113.4 api.anthropic.com"
	const second = "192.0.2.8 another.example API.ANTHROPIC.COM. # note"
	tests := map[string]struct {
		text string
		want []string
	}{
		"non-loopback only":       {first + "\n" + entry + "\n", []string{first}},
		"original lines in order": {"  " + second + "  \r\n# " + first + "\n" + first + " # " + start + "\n" + first, []string{second, first}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tc.want, rccompat.Conflicts(tc.text)); diff != "" {
				t.Errorf("conflicts (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDetectOverride(t *testing.T) {
	const path = "fixture/hosts"
	tests := map[string]struct {
		files fstest.MapFS
		want  *rccompat.Entry
	}{
		"missing":       {},
		"entry present": {fstest.MapFS{path: {Data: []byte("127.0.0.1 localhost\n" + entry + "\n")}}, &rccompat.Entry{IP: "127.0.0.1", Line: entry}},
		"entry removed": {files: fstest.MapFS{path: {Data: []byte("127.0.0.1 localhost\n")}}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var reads []string
			got := rccompat.Detect(path, func(name string) ([]byte, error) {
				reads = append(reads, name)
				return tc.files.ReadFile(name)
			})
			if diff := gocmp.Diff([]string{path}, reads); diff != "" {
				t.Errorf("paths read (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tc.want, got); diff != "" {
				t.Errorf("entry (-want +got):\n%s", diff)
			}
		})
	}
}
