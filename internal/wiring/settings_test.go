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
	json "encoding/json/v2"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

func decode(t *testing.T, raw string, out any) {
	t.Helper()
	check(t, json.Unmarshal([]byte(raw), out))
}

func TestEnvBlock(t *testing.T) {
	f := sandbox(t)
	f.Pins.Current = map[string]string{opus: "claude-opus-5-5[1m]"}
	a, b := f.EnvBlock("default"), f.EnvBlock("compat")
	equal(t, direct, a[base])
	equal(t, compat, b[base])
	delete(a, base)
	delete(b, base)
	equal(t, a, b)
	equal(t, map[string]string{discovery: "1", "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1", "ENABLE_TOOL_SEARCH": "true", "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000", opus: "claude-opus-5-5[1m]"}, a)
}

func TestWriteEnvironment(t *testing.T) {
	f := sandbox(t)
	putEnv(t, f.user, map[string]any{"USER_SETTING": "keep-me"})
	for _, mode := range []string{"default", "compat", "remove"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "compat" {
				doc := object(t, text(t, f.user))
				env := envAt(t, f.user)
				env["ANTHROPIC_UNIX_SOCKET"] = "retired-socket-value"
				doc["env"] = env
				put(t, f.user, encoded(t, doc))
			}
			check(t, f.WriteEnv("user", mode, mode == "remove"))
			env := envAt(t, f.user)
			_, socket := env["ANTHROPIC_UNIX_SOCKET"]
			equal(t, false, socket)
			equal(t, "keep-me", env["USER_SETTING"])
			var want *wiring.Wiring
			if mode == "remove" {
				_, present := env[base]
				equal(t, false, present)
			} else {
				equal(t, map[string]string{"default": direct, "compat": compat}[mode], env[base])
				want = &wiring.Wiring{Scope: "user", Mode: mode, Source: "user", File: f.user}
			}
			actual, err := f.WiredMode()
			check(t, err)
			equal(t, want, actual)
		})
	}
}

func TestRemovePins(t *testing.T) {
	f := sandbox(t)
	f.Pins = wiring.Pins{Current: map[string]string{opus: "claude-opus-5-5[1m]", sonnet: "claude-sonnet-5-5[1m]"}, Owned: map[string][]string{opus: {"claude-opus-5-5[1m]"}, sonnet: {"claude-sonnet-5-5[1m]"}}}
	check(t, f.WriteEnv("user", "default", false))
	env := envAt(t, f.user)
	equal(t, "claude-opus-5-5[1m]", env[opus])
	env[sonnet] = "claude-sonnet-5-my-own-choice"
	putEnv(t, f.user, env)
	check(t, f.WriteEnv("user", "default", true))
	equal(t, map[string]any{sonnet: "claude-sonnet-5-my-own-choice"}, envAt(t, f.user))
}

func TestSyncRegisteredPins(t *testing.T) {
	f := sandbox(t)
	old, newID := "claude-fable-5-1[1m]", "claude-fable-5-3[1m]"
	oldDesc, newDesc := "Fable 5.1 · current Fable, through the gateway", "Fable 5.3 · current Fable, through the gateway"
	f.Pins = wiring.Pins{Current: map[string]string{fable: old, option: old, optionName: "Fable", description: oldDesc}, Owned: map[string][]string{fable: {old, newID}, option: {old, newID}, optionName: {"Fable"}, description: {oldDesc, newDesc}}}
	initial := map[string]string{base: direct, discovery: "1", "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1", "ENABLE_TOOL_SEARCH": "true", "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000", fable: old}
	put(t, f.local, encoded(t, map[string]any{"env": initial}))
	check(t, f.RecordProject())
	_, err := f.SyncPins()
	check(t, err)
	want := maps.Clone(initial)
	maps.Copy(want, f.Pins.Current)
	equal(t, object(t, encoded(t, want)), envAt(t, f.local))
	f.Pins.Current[fable] = newID
	f.Pins.Current[option] = newID
	f.Pins.Current[description] = newDesc
	_, err = f.SyncPins()
	check(t, err)
	env := envAt(t, f.local)
	equal(t, newID, env[option])
	equal(t, newDesc, env[description])
	env[option] = "my-own-model"
	env[fable] = old
	putEnv(t, f.local, env)
	result, err := f.SyncPins()
	check(t, err)
	equal(t, env, envAt(t, f.local))
	if len(result.Skipped) != 1 {
		t.Fatalf("skipped: %+v", result.Skipped)
	}
	equal(t, option, result.Skipped[0].Key)
}

func TestRepairOrphan(t *testing.T) {
	f := sandbox(t)
	f.Pins.Owned = map[string][]string{opus: {"claude-opus-5[1m]"}, fable: {"claude-fable-5-1[1m]"}}
	log := filepath.Join(f.home, "claude-called")
	probe := filepath.Join(f.home, "claude")
	put(t, probe, "#!/bin/sh\nprintf called >> \"${0%/*}/claude-called\"\n")
	check(t, os.Chmod(probe, 0o700))
	f.env["CODEX_GATEWAY_CLAUDE_BIN"] = probe
	putEnv(t, f.local, map[string]any{discovery: "1", opus: "claude-opus-5[1m]", fable: "claude-fable-4-2[1m]", "USER_SETTING": "keep-me"})
	var lines []string
	report := func(s string) { lines = append(lines, s) }
	check(t, f.Repair(report))
	equal(t, map[string]any{fable: "claude-fable-4-2[1m]", "USER_SETTING": "keep-me"}, envAt(t, f.local))
	for _, fragment := range []string{"repaired leftover gateway settings", "preserved user pins: ANTHROPIC_DEFAULT_FABLE_MODEL"} {
		if !strings.Contains(strings.Join(lines, "\n"), fragment) {
			t.Fatalf("%q missing from %q", fragment, lines)
		}
	}
	absent(t, log)
	first := text(t, f.local)
	lines = nil
	check(t, f.Repair(report))
	equal(t, first, text(t, f.local))
	equal(t, 0, len(lines))
}

func TestSyncWindow(t *testing.T) {
	root := t.TempDir()
	owned, user, unset, broken := filepath.Join(root, "owned"), filepath.Join(root, "user"), filepath.Join(root, "unset"), filepath.Join(root, "broken")
	original := "{\r\n\t\"env\" : { \"A\" : \"1\" },\r\n\t\"autoCompactWindow\" : 400000\r\n}"
	put(t, owned, original)
	put(t, user, `{"autoCompactWindow":325000}`)
	put(t, broken, "{")
	r := wiring.SyncWindow([]string{owned, user, unset, broken}, new(400000), new(500000))
	equal(t, []string{owned, unset}, r.Changed)
	if len(r.Skipped) != 2 {
		t.Fatalf("skipped: %+v", r.Skipped)
	}
	for i, file := range []string{user, broken} {
		equal(t, file, r.Skipped[i].File)
		if !strings.Contains(r.Skipped[i].Reason, []string{"not gateway-owned", "Could not read"}[i]) {
			t.Fatalf("skip reason %q", r.Skipped[i].Reason)
		}
	}
	equal(t, strings.Replace(original, "400000", "500000", 1), text(t, owned))
	equal(t, float64(500000), object(t, text(t, unset))["autoCompactWindow"])
	equal(t, `{"autoCompactWindow":325000}`, text(t, user))
	equal(t, 0, len(wiring.SyncWindow([]string{owned}, new(500000), new(500000)).Changed))
	r = wiring.SyncWindow([]string{owned, unset}, new(500000), nil)
	equal(t, []string{owned, unset}, r.Changed)
	equal(t, object(t, `{"env":{"A":"1"}}`), object(t, text(t, owned)))
	equal(t, 0, len(wiring.SyncWindow([]string{owned}, nil, nil).Changed))
}

func TestCleanModelCache(t *testing.T) {
	const bare = "claude-gpt-6-astra"
	const full = bare + "[1m]"
	const model = `{"id":"claude-gpt-6-astra","display_name":"GPT-6 Astra (Codex)"}`
	const extras = `,{"id":"claude-grok-4.5[1m]","display_name":"Grok 4.5"},{"id":"anthropic-custom","display_name":"Anthropic Custom"}`
	original := `{"baseUrl":"` + direct + `","fetchedAt":1,"models":[` + model + `]}`
	mixed := strings.Replace(original, "]}", extras+"]}", 1)
	canonical := strings.Replace(original, bare, full, 1)
	tests := map[string]struct {
		input, want, canonicalID string
		changed                  bool
	}{
		"success: upgrade":   {mixed, strings.Replace(mixed, bare, full, 1), full, true},
		"success: canonical": {canonical, canonical, full, false},
		"success: foreign":   {input: strings.Replace(original, direct, "http://other-gateway.example", 1), canonicalID: full},
		"success: missing":   {},
		"success: malformed": {input: "not json"},
		"success: non-array": {input: `{"baseUrl":"` + direct + `","fetchedAt":1,"models":"not-an-array"}`},
		"success: downgrade": {canonical, original, bare, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			file := filepath.Join(f.home, "cache.json")
			mtime := time.Unix(123456, 0)
			if tt.input != "" {
				put(t, file, tt.input)
				check(t, os.Chtimes(file, mtime, mtime))
			}
			got := f.CleanModelCache(file, func(id string) (string, bool) {
				if id == bare || id == full {
					return tt.canonicalID, true
				}
				return id, id == "claude-grok-4.5[1m]"
			})
			equal(t, tt.changed, got)
			switch {
			case tt.input == "":
				absent(t, file)
			case tt.changed:
				equal(t, object(t, tt.want), object(t, text(t, file)))
			default:
				equal(t, tt.input, text(t, file))
				stat, err := os.Stat(file)
				check(t, err)
				equal(t, mtime, stat.ModTime())
			}
		})
	}
}
