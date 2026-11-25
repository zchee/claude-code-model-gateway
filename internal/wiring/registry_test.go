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

package wiring_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

func TestProjectRegistry(t *testing.T) {
	tests := map[string]struct{ input string }{
		"success: mixed registrations": {"mixed"},
		"success: unreadable JSON":     {"{"},
		"success: invalid list":        {`{"projects":true}`},
		"success: absent":              {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			original := tt.input
			var want []wiring.Registration
			var pruned []wiring.Pruned
			if tt.input == "mixed" {
				putEnv(t, f.local, map[string]any{base: direct})
				alias := filepath.Join(t.TempDir(), "alias")
				check(t, os.Symlink(f.Project, alias))
				missing := filepath.Join(t.TempDir(), "gone")
				foreign := t.TempDir()
				putEnv(t, filepath.Join(foreign, ".claude", "settings.local.json"), map[string]any{base: "https://example.invalid"})
				malformed := t.TempDir()
				put(t, filepath.Join(malformed, ".claude", "settings.local.json"), "{")
				original = encoded(t, map[string]any{"projects": []any{f.Project, alias, missing, 17, "relative", foreign, malformed}})
				want = []wiring.Registration{{Project: f.Project, File: f.local, Value: direct}}
				pruned = []wiring.Pruned{{Project: missing, Reason: "directory is missing"}}
			}
			if original != "" {
				put(t, f.registry, original)
			}
			for _, readOnly := range []bool{true, false} {
				got, dropped, err := f.Projects(readOnly)
				check(t, err)
				equal(t, len(want), len(got))
				equal(t, len(pruned), len(dropped))
				if len(want) > 0 {
					equal(t, want, got)
					equal(t, pruned, dropped)
				}
				if original == "" {
					absent(t, f.registry)
				} else if readOnly {
					equal(t, original, text(t, f.registry))
				} else {
					projects := []string{}
					if len(want) > 0 {
						projects = append(projects, f.Project)
					}
					golden := "{\n  \"projects\": []\n}\n"
					if len(projects) > 0 {
						golden = "{\n  \"projects\": [\n    " + encoded(t, projects[0]) + "\n  ]\n}\n"
					}
					equal(t, golden, text(t, f.registry))
				}
			}
		})
	}
}

func TestRegisteredPinDisagreements(t *testing.T) {
	f := sandbox(t)
	f.Pins.Current = shippedEntries()
	putEnv(t, f.local, map[string]any{base: direct, opus: "my-opus", fable: nil, description: "old"})
	missing := filepath.Join(f.home, "missing")
	registry := encoded(t, map[string]any{"projects": []string{f.Project, missing}})
	put(t, f.registry, registry)
	settings := text(t, f.local)
	got, pruned, err := f.PinDisagreements()
	check(t, err)
	equal(t, []wiring.Pruned{{Project: missing, Reason: "directory is missing"}}, pruned)
	reg := wiring.Registration{Project: f.Project, File: f.local, Value: direct}
	equal(t, []wiring.Disagreement{
		{Registration: reg, Key: opus, Expected: "claude-opus-5-5[1m]", Present: true, Stale: "my-opus"},
		{Registration: reg, Key: sonnet, Expected: "claude-sonnet-5-5[1m]"},
		{Registration: reg, Key: fable, Expected: "claude-fable-5-1[1m]", Present: true},
		{Registration: reg, Key: option, Expected: "claude-fable-5-1[1m]"},
		{Registration: reg, Key: optionName, Expected: "Fable"},
		{Registration: reg, Key: description, Expected: "Fable 5.1 · current Fable, through the gateway", Present: true, Stale: "old"},
	}, got)
	equal(t, registry, text(t, f.registry))
	equal(t, settings, text(t, f.local))
}

func TestReconcileRegisteredProjects(t *testing.T) {
	tests := map[string]struct {
		target  string
		confirm bool
	}{
		"error: unknown target":      {"https://example.invalid", true},
		"success: preview only":      {direct, false},
		"success: confirmed removal": {direct, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.Pins.Current = []wiring.Entry{{Key: opus, Value: "owned"}, {Key: sonnet, Value: "owned-sonnet"}}
			f.Pins.Owned = map[string][]string{opus: {"owned"}, sonnet: {"owned-sonnet"}}
			putEnv(t, f.local, map[string]any{base: compat, opus: "owned", sonnet: "my-model", discovery: "1", "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "32000"})
			aligned := t.TempDir()
			alignedFile := filepath.Join(aligned, ".claude", "settings.local.json")
			putEnv(t, alignedFile, map[string]any{base: direct, "USER": true})
			alignedBytes := text(t, alignedFile)
			put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project, aligned}}))
			before := text(t, f.local)
			reg := text(t, f.registry)
			conflicts, removed, err := f.Reconcile(tt.target, tt.confirm)
			if tt.target != direct {
				if err == nil {
					t.Fatal("unknown target accepted")
				}
				equal(t, before, text(t, f.local))
				equal(t, reg, text(t, f.registry))
				return
			}
			check(t, err)
			want := []wiring.Registration{{Project: f.Project, File: f.local, Value: compat}}
			equal(t, want, conflicts)
			if !tt.confirm {
				equal(t, 0, len(removed))
				equal(t, before, text(t, f.local))
				equal(t, reg, text(t, f.registry))
				return
			}
			equal(t, want, removed)
			equal(t, map[string]any{sonnet: "my-model", "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "32000"}, envAt(t, f.local))
			equal(t, alignedBytes, text(t, alignedFile))
			equal(t, "{\n  \"projects\": [\n    "+encoded(t, aligned)+"\n  ]\n}\n", text(t, f.registry))
		})
	}
}

func TestRepairRegistersWiredProject(t *testing.T) {
	f := sandbox(t)
	putEnv(t, f.local, map[string]any{base: direct})
	var lines []string
	report := func(s string) { lines = append(lines, s) }
	check(t, f.Repair(report))
	equal(t, []string{"model-gateway: registered wired project " + f.Project + " for pin sync"}, lines)
	equal(t, object(t, encoded(t, map[string]any{"projects": []string{f.Project}})), object(t, text(t, f.registry)))
	lines = nil
	check(t, f.Repair(report))
	equal(t, 0, len(lines))
}
