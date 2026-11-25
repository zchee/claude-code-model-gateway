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

package argscan_test

import (
	"bytes"
	"io"
	"os"
	"slices"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/cli/argscan"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestScan(t *testing.T) {
	tests := map[string]struct {
		args      []string
		flag      string
		wantFlag  bool
		wantFirst string
		wantPairs []argscan.Pair
	}{
		// lib/commands.js:199-201,1001-1009,1088-1097: no tokens means nothing to update.
		"success: no arguments": {flag: "--json"},
		// lib/commands.js:199-201,1088-1097: an explicit empty token is both present and unpaired.
		"success: empty first token": {
			args:      []string{""},
			flag:      "",
			wantFlag:  true,
			wantPairs: []argscan.Pair{{}},
		},
		// lib/commands.js:199-201 and lib/remote-control.js:137-145,194: a preceding flag is the action token.
		"success: unknown flag before action is not skipped": {
			args:      []string{"--unknown", "enable", "--confirm"},
			flag:      "--confirm",
			wantFlag:  true,
			wantFirst: "--unknown",
			wantPairs: []argscan.Pair{{Option: "--unknown", Value: "enable", HasValue: true}, {Option: "--confirm"}},
		},
		// lib/commands.js:199-201 and lib/remote-control.js:137-145,194: later tokens do not replace the action.
		"success: unknown flag after action leaves action intact": {
			args:      []string{"enable", "--unknown", "--confirm"},
			flag:      "--confirm",
			wantFlag:  true,
			wantFirst: "enable",
			wantPairs: []argscan.Pair{{Option: "enable", Value: "--unknown", HasValue: true}, {Option: "--confirm"}},
		},
		// lib/commands.js:199-201,2078-2080: the presence check matches only the complete token.
		"success: equals does not enable a presence flag": {
			args:      []string{"--refresh=true"},
			flag:      "--refresh",
			wantFirst: "--refresh=true",
			wantPairs: []argscan.Pair{{Option: "--refresh=true"}},
		},
		// lib/commands.js:1075-1098: equals stays part of an invalid option token, even with an empty suffix.
		"success: equals with empty suffix is not a value": {
			args:      []string{"--opus="},
			flag:      "--opus",
			wantFirst: "--opus=",
			wantPairs: []argscan.Pair{{Option: "--opus="}},
		},
		// lib/commands.js:1001-1011 and lib/context-window.js:20-29: equals is not split into a valid option and value.
		"success: equals with nonempty suffix is not a value": {
			args:      []string{"--codex=272000", "--claude", "full"},
			flag:      "--codex",
			wantFirst: "--codex=272000",
			wantPairs: []argscan.Pair{{Option: "--codex=272000", Value: "--claude", HasValue: true}, {Option: "full"}},
		},
		// lib/commands.js:199-201,1075-1098: keep ordered repeats and distinguish empty from missing.
		"success: repeated value flags with empty and missing values": {
			args:      []string{"--opus", "first", "--opus", "default", "--opus", "", "--opus"},
			flag:      "--opus",
			wantFlag:  true,
			wantFirst: "--opus",
			wantPairs: []argscan.Pair{
				{Option: "--opus", Value: "first", HasValue: true},
				{Option: "--opus", Value: "default", HasValue: true},
				{Option: "--opus", HasValue: true},
				{Option: "--opus"},
			},
		},
		// lib/commands.js:199-201,215-218,1008-1011: -- does not consume or stop scanning; a flag can also be a value.
		"success: no separator and no protection for option-like values": {
			args:      []string{"--", "--grok", "--codex", "--json"},
			flag:      "--json",
			wantFlag:  true,
			wantFirst: "--",
			wantPairs: []argscan.Pair{
				{Option: "--", Value: "--grok", HasValue: true},
				{Option: "--codex", Value: "--json", HasValue: true},
			},
		},
		// lib/commands.js:199-201,2078-2080: repeated presence flags have the same effect as a single flag.
		"success: repeated presence flags": {
			args:      []string{"--refresh", "--refresh"},
			flag:      "--refresh",
			wantFlag:  true,
			wantFirst: "--refresh",
			wantPairs: []argscan.Pair{{Option: "--refresh", Value: "--refresh", HasValue: true}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a := argscan.Scan(tt.args)
			if diff := gocmp.Diff(len(tt.args), a.Len()); diff != "" {
				t.Errorf("token count mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantFlag, a.Has(tt.flag)); diff != "" {
				t.Errorf("presence of %q mismatch (-want +got):\n%s", tt.flag, diff)
			}
			first, found := a.First()
			if diff := gocmp.Diff(tt.wantFirst, first); diff != "" {
				t.Errorf("first token mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(len(tt.args) > 0, found); diff != "" {
				t.Errorf("first token presence mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantPairs, slices.Collect(a.Pairs())); diff != "" {
				t.Errorf("pairs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStreams(t *testing.T) {
	tests := map[string]struct {
		args       []string
		wantStdout string
		wantStderr string
	}{
		// lib/commands.js:215-218,239-241,2097-2100: log lines and ordinary output share stdout by default.
		"success: ordinary output": {wantStdout: "report\n{}\n", wantStderr: "diagnostic\n"},
		// lib/commands.js:199-201,215-218,2097-2100: a literal --json reroutes logs but not the direct payload.
		"success: machine output after double dash": {
			args:       []string{"--", "--json", "--json"},
			wantStdout: "{}\n",
			wantStderr: "report\ndiagnostic\n",
		},
		// lib/commands.js:199-201,215-218: an equals form does not trigger routing.
		"success: equals form leaves logs on stdout": {
			args:       []string{"--json="},
			wantStdout: "report\n{}\n",
			wantStderr: "diagnostic\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			streams := argscan.Scan(tt.args).Streams(&stdout, &stderr)
			_, _ = io.WriteString(streams.Log, "report\n")
			_, _ = io.WriteString(streams.Output, "{}\n")
			_, _ = io.WriteString(streams.Error, "diagnostic\n")
			if diff := gocmp.Diff(tt.wantStdout, stdout.String()); diff != "" {
				t.Errorf("stdout mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantStderr, stderr.String()); diff != "" {
				t.Errorf("stderr mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
