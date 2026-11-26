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
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

// The surrounding members are deliberately not in the spelling a JSON encoder would choose.
const (
	authoredPrefix = "{\r\n\t" + `"before" : { "text":"\u0041", "n":1e+01 },` + "\r\n\t"
	authoredSuffix = ",\r\n\t" + `"after" : [ "\u0062", 3E-02 ]` + "\r\n}"
	envPrefix      = authoredPrefix + `"disableAllHooks" : true, "env" : { ` + `"LEFT":"\u006c", `
	envSuffix      = `, "RIGHT":4e+03 }` + authoredSuffix
	userPair       = `"USER_SETTING" : "keep-me"`
)

func TestAuthoredWriterBytes(t *testing.T) {
	tests := map[string]struct {
		before, want string
		run          func(*testing.T, fixture)
	}{
		"success: write environment": {`"ANTHROPIC_BASE_URL" : "` + compat + `", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1", "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK":"1", "ENABLE_TOOL_SEARCH":"true", "CLAUDE_CODE_MAX_OUTPUT_TOKENS":"64000", ` + userPair, `"ANTHROPIC_BASE_URL" : "` + direct + `", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1", "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK":"1", "ENABLE_TOOL_SEARCH":"true", "CLAUDE_CODE_MAX_OUTPUT_TOKENS":"64000", ` + userPair, func(t *testing.T, f fixture) { check(t, f.WriteEnv("project", "default", false)) }},
		"success: sync pins": {`"ANTHROPIC_BASE_URL" : "` + direct + `", "ANTHROPIC_DEFAULT_OPUS_MODEL":"old", ` + userPair, `"ANTHROPIC_BASE_URL" : "` + direct + `", "ANTHROPIC_DEFAULT_OPUS_MODEL":"new", ` + userPair, func(t *testing.T, f fixture) {
			f.Pins = wiring.Pins{Current: []wiring.Entry{{Key: opus, Value: "new"}}, Owned: map[string][]string{opus: {"old", "new"}}}
			put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
			r, err := f.SyncPins()
			check(t, err)
			equal(t, []string{f.local}, r.Changed)
		}},
		"success: repair orphan": {`"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":1, "ENABLE_TOOL_SEARCH":true, "CLAUDE_CODE_AUTO_COMPACT_WINDOW":950000, ` + userPair, userPair, func(t *testing.T, f fixture) { check(t, f.Repair(func(string) {})) }},
		"success: reconcile": {`"ANTHROPIC_BASE_URL":"` + compat + `", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1", ` + userPair, userPair, func(t *testing.T, f fixture) {
			put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
			_, _, err := f.Reconcile(direct, true)
			check(t, err)
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.local, envPrefix+tt.before+envSuffix)
			tt.run(t, f)
			equal(t, envPrefix+tt.want+envSuffix, text(t, f.local))
		})
	}
}

func TestLegacyMigrationBytes(t *testing.T) {
	f := sandbox(t)
	const owned = `"ANTHROPIC_BASE_URL" : "` + direct + `", "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY" : "1", `
	put(t, f.shared, envPrefix+owned+userPair+envSuffix)
	put(t, f.local, envPrefix+`"ANTHROPIC_BASE_URL":"`+compat+`", `+userPair+envSuffix)
	ok, err := f.MigrateLegacy()
	check(t, err)
	equal(t, true, ok)
	equal(t, envPrefix+userPair+envSuffix, text(t, f.shared))
	equal(t, envPrefix+`"ANTHROPIC_BASE_URL":"`+compat+`", `+userPair+`, "RIGHT":4e+03,`+"\r\n    "+`"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"`+"\r\n  "+` }`+authoredSuffix, text(t, f.local))
}

func TestWindowWriteContract(t *testing.T) {
	tests := map[string]struct{ wired, fail bool }{"success: wired installs readiness": {true, false}, "success: unwired removes readiness": {false, false}, "error: replacement failed": {true, true}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			var doc, wantWired string
			if tt.wired {
				var installed string
				doc, installed = firstHookSettings(t, f)
				const hooks = ",\r\n  \"hooks\":"
				wantWired = strings.Replace(installed, hooks, ",\r\n  \"autoCompactWindow\": 500000"+hooks, 1)
			} else {
				doc = authoredPrefix + `"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":` + encoded(t, f.bin) + `,"args":["readiness-gate"]}]}]}, "autoCompactWindow":400000` + authoredSuffix
			}
			put(t, f.local, doc)
			stop := errors.New("replacement refused")
			if tt.fail {
				f.Replace = func(string, func([]byte) ([]byte, error)) error { return stop }
			}
			r, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
			if tt.fail {
				if !errors.Is(err, stop) {
					t.Fatalf("expected settings replacement error, got %v", err)
				}
				equal(t, 0, len(r.Changed))
				equal(t, doc, text(t, f.local))
				return
			}
			check(t, err)
			equal(t, []string{f.local}, r.Changed)
			got := text(t, f.local)
			if tt.wired {
				equal(t, wantWired, got)
				equal(t, true, f.ReadinessHookPresent([]byte(got)))
				equal(t, float64(500000), object(t, got)["autoCompactWindow"])
			} else {
				equal(t, authoredPrefix+`"autoCompactWindow":500000`+authoredSuffix, got)
			}
		})
	}
}

func TestPinSyncReport(t *testing.T) {
	f := sandbox(t)
	f.Pins.Current = shippedEntries()
	f.Pins.Owned = map[string][]string{}
	for _, p := range shippedEntries() {
		f.Pins.Owned[p.Key] = []string{p.Value}
	}
	putEnv(t, f.local, map[string]any{base: direct, opus: 17, sonnet: "chosen", option: "elsewhere"})
	missing := filepath.Join(f.home, "gone")
	put(t, f.registry, encoded(t, map[string]any{"projects": []string{missing, f.Project}}))
	before := text(t, f.local)
	r, err := f.SyncPins()
	check(t, err)
	equal(t, 0, len(r.Changed))
	equal(t, []wiring.Pruned{{Project: missing, Reason: "directory is missing"}}, r.Pruned)
	equal(t, []wiring.Skipped{{File: f.local, Key: opus, Value: float64(17), Reason: "pin value is not gateway-owned"}}, r.Skipped)
	equal(t, before, text(t, f.local))
}

func TestWindowRemovalBytes(t *testing.T) {
	f := sandbox(t)
	before := authoredPrefix + `"env" : { "A":"A" }, "autoCompactWindow" : 500000` + authoredSuffix
	put(t, f.local, before)
	r, err := f.SyncWindow([]string{f.local}, new(500000), nil)
	check(t, err)
	equal(t, []string{f.local}, r.Changed)
	equal(t, strings.Replace(before, `"autoCompactWindow" : 500000,`+"\r\n\t", "", 1), text(t, f.local))
}
