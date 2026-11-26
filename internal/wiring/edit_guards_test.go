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
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

func TestMigrationStaleSourceInterleavings(t *testing.T) {
	t.Run("success: source changed to personal URL before copy aborts with no local edit", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.shared, map[string]any{base: direct, discovery: "1"})
		personal := `{"env":{"ANTHROPIC_BASE_URL":"https://personal.example","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			if file == f.local {
				put(t, f.shared, personal)
			}
			return atomicfile.ReplaceSettings(file, edit)
		}

		migrated, err := f.MigrateLegacy()
		check(t, err)
		equal(t, false, migrated)
		absent(t, f.local)
		equal(t, personal, text(t, f.shared))
		res, err := f.EffectiveBaseURL()
		check(t, err)
		if res.Winner == nil || res.Winner.Source != "project-shared" || res.Winner.Value != "https://personal.example" {
			t.Fatalf("expected personal shared winner, got %+v", res)
		}
	})

	t.Run("success: source swapped to alias of local before copy aborts cleanly", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.local, map[string]any{"LOCAL_KEEP": "1"})
		localBefore := text(t, f.local)
		putEnv(t, f.shared, map[string]any{base: direct, discovery: "1"})
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			if file == f.local {
				check(t, os.Remove(f.shared))
				check(t, os.Symlink(f.local, f.shared))
			}
			return atomicfile.ReplaceSettings(file, edit)
		}

		migrated, err := f.MigrateLegacy()
		check(t, err)
		equal(t, false, migrated)
		equal(t, localBefore, text(t, f.local))
	})

	t.Run("success: source swapped to alias of user before copy aborts cleanly", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.user, map[string]any{base: compat, "USER_KEEP": "1"})
		userBefore := text(t, f.user)
		putEnv(t, f.shared, map[string]any{base: direct, discovery: "1"})
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			if file == f.local {
				check(t, os.Remove(f.shared))
				check(t, os.Symlink(f.user, f.shared))
			}
			return atomicfile.ReplaceSettings(file, edit)
		}

		migrated, err := f.MigrateLegacy()
		check(t, err)
		equal(t, false, migrated)
		absent(t, f.local)
		equal(t, userBefore, text(t, f.user))
	})

	t.Run("success: owned pin updated to different owned value before source deletion is not dropped", func(t *testing.T) {
		f := sandbox(t)
		f.Pins = wiring.Pins{
			Current: []wiring.Entry{{Key: opus, Value: "claude-opus-5-5[1m]"}},
			Owned:   map[string][]string{opus: {"claude-opus-5[1m]", "claude-opus-5-5[1m]"}},
		}
		putEnv(t, f.shared, map[string]any{base: direct, opus: "claude-opus-5[1m]"})
		updatedShared := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","ANTHROPIC_DEFAULT_OPUS_MODEL":"claude-opus-5-5[1m]"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			if file == f.shared {
				put(t, f.shared, updatedShared)
			}
			return atomicfile.ReplaceSettings(file, edit)
		}

		_, err := f.MigrateLegacy()
		check(t, err)
		// Local only ever received claude-opus-5[1m], so claude-opus-5-5[1m] in shared must not be deleted.
		equal(t, "claude-opus-5-5[1m]", envAt(t, f.shared)[opus])
	})

	t.Run("success: source atomically replaced with new inode before deletion aborts deletion", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.shared, map[string]any{base: direct, discovery: "1"})
		replacedShared := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1","NEW_KEY":"added"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			if file == f.shared {
				tmp := filepath.Join(filepath.Dir(f.shared), "replaced.tmp")
				put(t, tmp, replacedShared)
				check(t, os.Rename(tmp, f.shared))
			}
			return atomicfile.ReplaceSettings(file, edit)
		}

		migrated, err := f.MigrateLegacy()
		check(t, err)
		equal(t, false, migrated)
		equal(t, replacedShared, text(t, f.shared))
	})

	t.Run("success: CleanLegacyEnv stops without editing user fallback when migration declines", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.local, map[string]any{base: nil, "LOCAL_KEEP": "yes"})
		putEnv(t, f.shared, map[string]any{base: direct, discovery: "1"})
		putEnv(t, f.user, map[string]any{base: compat, "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "950000"})
		localBefore := text(t, f.local)
		sharedBefore := text(t, f.shared)
		userBefore := text(t, f.user)

		check(t, f.CleanLegacyEnv())
		equal(t, localBefore, text(t, f.local))
		equal(t, sharedBefore, text(t, f.shared))
		equal(t, userBefore, text(t, f.user))
	})
}

func TestProjectRemovalProtectsUserSettings(t *testing.T) {
	tests := map[string]struct {
		scope    string
		linkKind string
		linkDst  string
	}{
		"error: local symlinked to user file":          {scope: "project", linkKind: "symlink", linkDst: "local"},
		"error: local hard-linked to user file":        {scope: "project", linkKind: "hardlink", linkDst: "local"},
		"error: project-shared symlinked to user file": {scope: "project-shared", linkKind: "symlink", linkDst: "shared"},
		"error: project-shared hard-linked to user":    {scope: "project-shared", linkKind: "hardlink", linkDst: "shared"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.user, map[string]any{base: direct, discovery: "1", "USER_KEEP": "yes"})
			userBefore := text(t, f.user)
			target := f.local
			if tt.linkDst == "shared" {
				target = f.shared
			}
			check(t, os.MkdirAll(filepath.Dir(target), 0o700))
			switch tt.linkKind {
			case "symlink":
				check(t, os.Symlink(f.user, target))
			case "hardlink":
				check(t, os.Link(f.user, target))
			}

			err := f.WriteEnv(tt.scope, "default", true)
			if err == nil {
				t.Fatalf("expected error when %s aliases user settings", tt.linkDst)
			}
			equal(t, userBefore, text(t, f.user))
			equal(t, userBefore, text(t, target))
		})
	}

	t.Run("success: project .claude directory symlinked to user .claude leaves user file untouched", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.user, map[string]any{base: direct, discovery: "1", "USER_KEEP": "yes"})
		userBefore := text(t, f.user)
		check(t, os.Symlink(filepath.Dir(f.user), filepath.Dir(f.local)))

		check(t, f.WriteEnv("project", "default", true))
		equal(t, userBefore, text(t, f.user))
	})

	t.Run("success: project file symlinked to another regular file edits through the link", func(t *testing.T) {
		f := sandbox(t)
		other := filepath.Join(t.TempDir(), "custom-settings.json")
		putEnv(t, other, map[string]any{base: direct, discovery: "1", "KEEP": "yes"})
		check(t, os.MkdirAll(filepath.Dir(f.local), 0o700))
		check(t, os.Symlink(other, f.local))

		check(t, f.WriteEnv("project", "default", true))
		equal(t, map[string]any{"KEEP": "yes"}, envAt(t, other))
		info, err := os.Lstat(f.local)
		check(t, err)
		equal(t, true, info.Mode()&os.ModeSymlink != 0)
	})
}

func TestProjectRemovalHardAndSymlinkedProjectFiles(t *testing.T) {
	tests := map[string]struct{ linkKind string }{
		"success: hard-linked local and shared project files are both cleaned": {"hardlink"},
		"success: symlinked local to shared cleans target cleanly":             {"symlink"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.shared, map[string]any{base: direct, discovery: "1", "TEAM_KEEP": "yes"})
			switch tt.linkKind {
			case "hardlink":
				check(t, os.Link(f.shared, f.local))
			case "symlink":
				check(t, os.Symlink(f.shared, f.local))
			}

			check(t, f.WriteEnv("project", "default", true))
			equal(t, map[string]any{"TEAM_KEEP": "yes"}, envAt(t, f.local))
			equal(t, map[string]any{"TEAM_KEEP": "yes"}, envAt(t, f.shared))
			res, err := f.EffectiveBaseURL()
			check(t, err)
			equal(t, (*wiring.Definition)(nil), res.Winner)
		})
	}
}

func TestFileWideDuplicateOwnedKeysAllOperations(t *testing.T) {
	dupHookCmd := func(bin string) string {
		return `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"personal-hook","command":` + encoded(t, bin) + `,"args":["readiness-gate"]}]}]}}`
	}
	dupHookType := func(bin string) string {
		return `{"hooks":{"SessionStart":[{"hooks":[{"type":"other","type":"command","command":` + encoded(t, bin) + `,"args":["readiness-gate"]}]}]}}`
	}
	dupHookArgs := func(bin string) string {
		return `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":` + encoded(t, bin) + `,"args":["personal"],"args":["readiness-gate"]}]}]}}`
	}
	dupGroupHooks := func(bin string) string {
		return `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"personal"}],"hooks":[{"type":"command","command":` + encoded(t, bin) + `,"args":["readiness-gate"]}]}]}}`
	}

	tests := map[string]struct {
		needle string
		setup  func(*testing.T, fixture) (string, string)
		run    func(*testing.T, fixture) error
	}{
		"error: SyncWindow refuses duplicate ANTHROPIC_BASE_URL": {
			needle: base,
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := `{"env":{"ANTHROPIC_BASE_URL":"personal","ANTHROPIC_BASE_URL":"` + direct + `"},"autoCompactWindow":400000}`
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(_ *testing.T, f fixture) error {
				_, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
				return err
			},
		},
		"error: SyncPins refuses duplicate autoCompactWindow": {
			needle: "autoCompactWindow",
			setup: func(t *testing.T, f fixture) (string, string) {
				f.Pins = wiring.Pins{
					Current: []wiring.Entry{{Key: opus, Value: "claude-opus-5-5[1m]"}},
					Owned:   map[string][]string{opus: {"claude-opus-5[1m]", "claude-opus-5-5[1m]"}},
				}
				raw := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","ANTHROPIC_DEFAULT_OPUS_MODEL":"claude-opus-5[1m]"},"autoCompactWindow":300000,"autoCompactWindow":400000}`
				put(t, f.local, raw)
				put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
				return f.local, raw
			},
			run: func(_ *testing.T, f fixture) error {
				_, err := f.SyncPins()
				return err
			},
		},
		"error: SyncReadinessHooks refuses duplicate command inside hook entry": {
			needle: "command",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := dupHookCmd(f.bin)
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(t *testing.T, f fixture) error {
				_, err := f.SyncReadinessHooks([]byte(text(t, f.local)), false)
				return err
			},
		},
		"error: SyncReadinessHooks refuses duplicate type inside hook entry": {
			needle: "type",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := dupHookType(f.bin)
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(t *testing.T, f fixture) error {
				_, err := f.SyncReadinessHooks([]byte(text(t, f.local)), false)
				return err
			},
		},
		"error: SyncReadinessHooks refuses duplicate args inside hook entry": {
			needle: "args",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := dupHookArgs(f.bin)
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(t *testing.T, f fixture) error {
				_, err := f.SyncReadinessHooks([]byte(text(t, f.local)), false)
				return err
			},
		},
		"error: SyncReadinessHooks refuses duplicate hooks inside hook group": {
			needle: "hooks",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := dupGroupHooks(f.bin)
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(t *testing.T, f fixture) error {
				_, err := f.SyncReadinessHooks([]byte(text(t, f.local)), false)
				return err
			},
		},
		"error: explicit write refuses duplicate command inside hook entry": {
			needle: "command",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := dupHookCmd(f.bin)
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(_ *testing.T, f fixture) error {
				return f.WriteEnv("project", "default", false)
			},
		},
		"error: explicit removal refuses duplicate command inside hook entry": {
			needle: "command",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := dupHookCmd(f.bin)
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(_ *testing.T, f fixture) error {
				return f.WriteEnv("project", "default", true)
			},
		},
		"error: migration refuses duplicate autoCompactWindow in shared": {
			needle: "autoCompactWindow",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"},"autoCompactWindow":300000,"autoCompactWindow":400000}`
				put(t, f.shared, raw)
				return f.shared, raw
			},
			run: func(_ *testing.T, f fixture) error {
				_, err := f.MigrateLegacy()
				return err
			},
		},
		"error: CleanLegacyEnv refuses duplicate command inside hook entry": {
			needle: "command",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"950000"},"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"personal","command":` + encoded(t, f.bin) + `,"args":["readiness-gate"]}]}]}}`
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(_ *testing.T, f fixture) error {
				return f.CleanLegacyEnv()
			},
		},
		"error: Repair refuses duplicate autoCompactWindow": {
			needle: "autoCompactWindow",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := `{"env":{"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"},"autoCompactWindow":300000,"autoCompactWindow":400000}`
				put(t, f.local, raw)
				return f.local, raw
			},
			run: func(_ *testing.T, f fixture) error {
				return f.Repair(nil)
			},
		},
		"error: Reconcile refuses duplicate command inside hook entry": {
			needle: "command",
			setup: func(t *testing.T, f fixture) (string, string) {
				raw := `{"env":{"ANTHROPIC_BASE_URL":"` + compat + `","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"},"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"personal","command":` + encoded(t, f.bin) + `,"args":["readiness-gate"]}]}]}}`
				put(t, f.local, raw)
				put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
				return f.local, raw
			},
			run: func(_ *testing.T, f fixture) error {
				_, _, err := f.Reconcile(direct, true)
				return err
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			file, before := tt.setup(t, f)
			err := tt.run(t, f)
			if err == nil || !strings.Contains(err.Error(), tt.needle) {
				t.Fatalf("expected error naming %q, got %v", tt.needle, err)
			}
			equal(t, before, text(t, file))
		})
	}

	t.Run("success: duplicate unrelated keys outside owned paths remain allowed", func(t *testing.T) {
		f := sandbox(t)
		raw := `{"other":1,"other":2,"env":{"USER_KEY":"a","USER_KEY":"b"},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"a","command":"b"}]}]}}`
		put(t, f.local, raw)
		check(t, f.WriteEnv("project", "default", false))
		res, err := f.EffectiveBaseURL()
		check(t, err)
		if res.Winner == nil || res.Winner.Source != "project-local" || res.Winner.Value != direct {
			t.Fatalf("expected wired local winner, got %+v", res)
		}
	})
}

func TestErrorTextNeverContainsAuthoredContent(t *testing.T) {
	t.Run("error: malformed JSON with recognisable property name leaks no property name or value", func(t *testing.T) {
		const propSentinel = "SECRET_PROP_SENTINEL_9941"
		const valSentinel = "SECRET_VAL_SENTINEL_8821"
		f := sandbox(t)
		raw := `{"` + propSentinel + `":` + valSentinel + `}`
		put(t, f.local, raw)

		checkRedacted := func(t *testing.T, context, msg string) {
			t.Helper()
			if strings.Contains(msg, propSentinel) || strings.Contains(msg, valSentinel) {
				t.Fatalf("%s leaked authored content: %q", context, msg)
			}
		}

		err := f.WriteEnv("project", "default", false)
		if err == nil {
			t.Fatal("expected WriteEnv error on malformed JSON")
		}
		if !strings.Contains(err.Error(), f.local) {
			t.Fatalf("expected filename %q in error, got %q", f.local, err.Error())
		}
		checkRedacted(t, "WriteEnv", err.Error())

		err = f.Repair(nil)
		if err == nil {
			t.Fatal("expected Repair error on malformed JSON")
		}
		checkRedacted(t, "Repair", err.Error())

		r, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
		check(t, err)
		if len(r.Skipped) != 1 {
			t.Fatalf("expected 1 skipped file, got %+v", r.Skipped)
		}
		checkRedacted(t, "SyncWindow", r.Skipped[0].Reason)

		_, err = f.SyncReadinessHooks([]byte(raw), true)
		if err == nil {
			t.Fatal("expected SyncReadinessHooks error on malformed JSON")
		}
		checkRedacted(t, "SyncReadinessHooks", err.Error())
	})

	t.Run("error: SyncWindow nonnumeric window leaks no authored value", func(t *testing.T) {
		const strSentinel = "SECRET_WINDOW_SENTINEL_7731"
		const arrSentinel = "SECRET_ARRAY_SENTINEL_6621"
		f := sandbox(t)
		strFile := filepath.Join(f.Project, "str.json")
		arrFile := filepath.Join(f.Project, "arr.json")
		put(t, strFile, `{"autoCompactWindow":"`+strSentinel+`"}`)
		put(t, arrFile, `{"autoCompactWindow":["`+arrSentinel+`","x"]}`)

		r, err := f.SyncWindow([]string{strFile, arrFile}, new(400000), new(500000))
		check(t, err)
		if len(r.Skipped) != 2 {
			t.Fatalf("expected 2 skipped files, got %+v", r.Skipped)
		}
		for _, skip := range r.Skipped {
			if !strings.Contains(skip.Reason, "not gateway-owned") {
				t.Fatalf("unexpected skip reason: %q", skip.Reason)
			}
			if strings.Contains(skip.Reason, strSentinel) || strings.Contains(skip.Reason, arrSentinel) {
				t.Fatalf("SyncWindow skip reason leaked authored value: %q", skip.Reason)
			}
		}
	})
}

func TestDiagnosticValuesKeepWireMemberOrderAndAvoidRescan(t *testing.T) {
	t.Run("success: ordinary object keeps wire member order in PinDisagreements and SyncPins", func(t *testing.T) {
		f := sandbox(t)
		f.Pins = wiring.Pins{
			Current: []wiring.Entry{
				{Key: opus, Value: "claude-opus-5-5[1m]"},
				{Key: sonnet, Value: "claude-sonnet-5-5[1m]"},
			},
			Owned: map[string][]string{
				opus:   {"claude-opus-5-5[1m]"},
				sonnet: {"claude-sonnet-5-5[1m]"},
			},
		}
		raw := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","ANTHROPIC_DEFAULT_OPUS_MODEL":{"z":1,"m":2,"a":3},"ANTHROPIC_DEFAULT_SONNET_MODEL":[{"b":1,"a":2}]}}`
		put(t, f.local, raw)
		put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))

		disagreements, _, err := f.PinDisagreements()
		check(t, err)
		if len(disagreements) != 2 {
			t.Fatalf("disagreements: %+v", disagreements)
		}
		opusBytes, err := json.Marshal(disagreements[0].Stale, jsontext.PreserveRawStrings(true))
		check(t, err)
		equal(t, `{"z":1,"m":2,"a":3}`, string(opusBytes))
		sonnetBytes, err := json.Marshal(disagreements[1].Stale, jsontext.PreserveRawStrings(true))
		check(t, err)
		equal(t, `[{"b":1,"a":2}]`, string(sonnetBytes))

		stateBytes, err := jsonx.EncodeState(disagreements)
		check(t, err)
		zIdx := bytes.Index(stateBytes, []byte(`"z": 1`))
		mIdx := bytes.Index(stateBytes, []byte(`"m": 2`))
		aIdx := bytes.Index(stateBytes, []byte(`"a": 3`))
		if zIdx < 0 || zIdx >= mIdx || mIdx >= aIdx {
			t.Fatalf("EncodeState lost wire member order:\n%s", stateBytes)
		}

		changes, err := f.SyncPins()
		check(t, err)
		if len(changes.Skipped) != 1 {
			t.Fatalf("skipped: %+v", changes.Skipped)
		}
		skippedBytes, err := json.Marshal(changes.Skipped[0].Value, jsontext.PreserveRawStrings(true))
		check(t, err)
		equal(t, `{"z":1,"m":2,"a":3}`, string(skippedBytes))
	})

	t.Run("success: deeply nested stale pin value completes without quadratic rescan", func(t *testing.T) {
		f := sandbox(t)
		f.Pins = wiring.Pins{
			Current: []wiring.Entry{{Key: opus, Value: "claude-opus-5-5[1m]"}},
			Owned:   map[string][]string{opus: {"claude-opus-5-5[1m]"}},
		}
		const depth = 600
		var b strings.Builder
		b.WriteString(`{"env":{"ANTHROPIC_BASE_URL":"`)
		b.WriteString(direct)
		b.WriteString(`","ANTHROPIC_DEFAULT_OPUS_MODEL":`)
		for range depth {
			b.WriteString(`{"z":`)
		}
		b.WriteString(`1`)
		for range depth {
			b.WriteString(`}`)
		}
		b.WriteString(`}}`)
		put(t, f.local, b.String())
		put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))

		disagreements, _, err := f.PinDisagreements()
		check(t, err)
		if len(disagreements) != 1 {
			t.Fatalf("disagreements: %+v", disagreements)
		}
		if _, ok := disagreements[0].Stale.(jsonx.WireValue); !ok {
			t.Fatalf("expected jsonx.WireValue for nested object, got %T", disagreements[0].Stale)
		}
	})
}

func TestProductionPathCreatedVersusExisting(t *testing.T) {
	tests := map[string]struct {
		setup func(*testing.T, fixture) string
		want  func(*testing.T, fixture) string
	}{
		"success: missing file created with final newline": {
			setup: func(_ *testing.T, f fixture) string { return f.local },
			want:  explicitSettings,
		},
		"success: existing empty object keeps no trailing newline": {
			setup: func(t *testing.T, f fixture) string {
				put(t, f.local, "{}")
				return f.local
			},
			want: func(t *testing.T, f fixture) string { return strings.TrimSuffix(explicitSettings(t, f), "\n") },
		},
		"success: symlink to existing empty object keeps no trailing newline": {
			setup: func(t *testing.T, f fixture) string {
				target := filepath.Join(t.TempDir(), "real.json")
				put(t, target, "{}")
				check(t, os.MkdirAll(filepath.Dir(f.local), 0o700))
				check(t, os.Symlink(target, f.local))
				return target
			},
			want: func(t *testing.T, f fixture) string { return strings.TrimSuffix(explicitSettings(t, f), "\n") },
		},
		"success: existing empty object with final newline keeps final newline": {
			setup: func(t *testing.T, f fixture) string {
				put(t, f.local, "{}\n")
				return f.local
			},
			want: explicitSettings,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.Pins.Current = shippedEntries()
			// Explicitly exercise the production nil-Replace path.
			f.Replace = nil
			target := tt.setup(t, f)
			check(t, f.WriteEnv("project", "default", false))
			equal(t, tt.want(t, f), text(t, target))
		})
	}
}

func TestUnixSocketRemovalByValueAndWriteByKey(t *testing.T) {
	const personalSocket = "/var/run/user-personal-anthropic.sock"
	const windowsPipe = `\\.\pipe\model-gateway`

	t.Run("success: personal socket survives removal byte-identical across scopes", func(t *testing.T) {
		tests := map[string]struct {
			scope  string
			target func(fixture) string
		}{
			"user scope":           {scope: "user", target: func(f fixture) string { return f.user }},
			"project-local scope":  {scope: "project", target: func(f fixture) string { return f.local }},
			"project-shared scope": {scope: "project-shared", target: func(f fixture) string { return f.shared }},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				f := sandbox(t)
				file := tt.target(f)
				raw := "{\r\n\t\"env\" : { \"ANTHROPIC_UNIX_SOCKET\" : \"" + personalSocket + "\", \"USER_KEEP\" : \"1\" }\r\n}"
				put(t, file, raw)
				check(t, f.WriteEnv(tt.scope, "default", true))
				equal(t, raw, text(t, file))
			})
		}
	})

	t.Run("success: personal socket in shared file survives project removal byte-identical", func(t *testing.T) {
		f := sandbox(t)
		raw := `{"env":{"ANTHROPIC_UNIX_SOCKET":"` + personalSocket + `"}}`
		put(t, f.shared, raw)
		check(t, f.WriteEnv("project", "default", true))
		absent(t, f.local)
		equal(t, raw, text(t, f.shared))
	})

	t.Run("success: personal socket preserved silently when owned base URL is removed", func(t *testing.T) {
		tests := map[string]struct {
			scope  string
			target func(fixture) string
		}{
			"user scope":           {scope: "user", target: func(f fixture) string { return f.user }},
			"project-local scope":  {scope: "project", target: func(f fixture) string { return f.local }},
			"project-shared scope": {scope: "project-shared", target: func(f fixture) string { return f.shared }},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				f := sandbox(t)
				file := tt.target(f)
				putEnv(t, file, map[string]any{base: direct, "ANTHROPIC_UNIX_SOCKET": personalSocket, "KEEP": "yes"})
				check(t, f.WriteEnv(tt.scope, "default", true))
				equal(t, map[string]any{"ANTHROPIC_UNIX_SOCKET": personalSocket, "KEEP": "yes"}, envAt(t, file))
			})
		}
	})

	t.Run("success: each gateway-owned socket form is removed on explicit removal", func(t *testing.T) {
		tests := map[string]struct {
			scope  string
			target func(fixture) string
			socket func(*testing.T, fixture) string
		}{
			"user default state socket": {
				scope:  "user",
				target: func(f fixture) string { return f.user },
				socket: func(_ *testing.T, f fixture) string { return filepath.Join(filepath.Dir(f.registry), "gateway.sock") },
			},
			"project-local default state socket": {
				scope:  "project",
				target: func(f fixture) string { return f.local },
				socket: func(_ *testing.T, f fixture) string { return filepath.Join(filepath.Dir(f.registry), "gateway.sock") },
			},
			"project-shared default state socket via project removal": {
				scope:  "project",
				target: func(f fixture) string { return f.shared },
				socket: func(_ *testing.T, f fixture) string { return filepath.Join(filepath.Dir(f.registry), "gateway.sock") },
			},
			"custom MODEL_GATEWAY_STATE_DIR socket": {
				scope:  "project",
				target: func(f fixture) string { return f.local },
				socket: func(t *testing.T, f fixture) string {
					dir := t.TempDir()
					f.env["MODEL_GATEWAY_STATE_DIR"] = dir
					return filepath.Join(dir, "gateway.sock")
				},
			},
			"CODEX_GATEWAY_SOCKET_PATH override": {
				scope:  "project",
				target: func(f fixture) string { return f.local },
				socket: func(t *testing.T, f fixture) string {
					sock := filepath.Join(t.TempDir(), "custom-gateway.sock")
					f.env["CODEX_GATEWAY_SOCKET_PATH"] = sock
					return sock
				},
			},
			"default state socket while CODEX_GATEWAY_SOCKET_PATH is also set": {
				scope:  "project",
				target: func(f fixture) string { return f.local },
				socket: func(t *testing.T, f fixture) string {
					f.env["CODEX_GATEWAY_SOCKET_PATH"] = filepath.Join(t.TempDir(), "other.sock")
					return filepath.Join(filepath.Dir(f.registry), "gateway.sock")
				},
			},
			"windows named pipe": {
				scope:  "project",
				target: func(f fixture) string { return f.local },
				socket: func(_ *testing.T, _ fixture) string { return windowsPipe },
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				f := sandbox(t)
				sock := tt.socket(t, f)
				file := tt.target(f)
				putEnv(t, file, map[string]any{"ANTHROPIC_UNIX_SOCKET": sock, "KEEP": "yes"})
				check(t, f.WriteEnv(tt.scope, "default", true))
				equal(t, map[string]any{"KEEP": "yes"}, envAt(t, file))
			})
		}
	})

	t.Run("success: explicit write deletes personal socket value by key across scopes", func(t *testing.T) {
		tests := map[string]struct {
			scope  string
			target func(fixture) string
		}{
			"user scope":           {scope: "user", target: func(f fixture) string { return f.user }},
			"project-local scope":  {scope: "project", target: func(f fixture) string { return f.local }},
			"project-shared scope": {scope: "project-shared", target: func(f fixture) string { return f.shared }},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				f := sandbox(t)
				file := tt.target(f)
				putEnv(t, file, map[string]any{"ANTHROPIC_UNIX_SOCKET": personalSocket, "KEEP": "yes"})
				check(t, f.WriteEnv(tt.scope, "default", false))
				env := envAt(t, file)
				_, hasSocket := env["ANTHROPIC_UNIX_SOCKET"]
				equal(t, false, hasSocket)
				equal(t, direct, env[base])
				equal(t, "yes", env["KEEP"])
			})
		}
	})

	t.Run("success: automatic paths do not touch ANTHROPIC_UNIX_SOCKET", func(t *testing.T) {
		tests := map[string]struct {
			run func(*testing.T, fixture, string)
		}{
			"CleanLegacyEnv": {
				run: func(t *testing.T, f fixture, ownedSock string) {
					putEnv(t, f.local, map[string]any{base: direct, "ANTHROPIC_UNIX_SOCKET": ownedSock, "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "950000"})
					check(t, f.CleanLegacyEnv())
					equal(t, map[string]any{base: direct, "ANTHROPIC_UNIX_SOCKET": ownedSock}, envAt(t, f.local))
				},
			},
			"MigrateLegacy": {
				run: func(t *testing.T, f fixture, ownedSock string) {
					putEnv(t, f.shared, map[string]any{base: direct, "ANTHROPIC_UNIX_SOCKET": ownedSock})
					migrated, err := f.MigrateLegacy()
					check(t, err)
					equal(t, true, migrated)
					equal(t, map[string]any{"ANTHROPIC_UNIX_SOCKET": ownedSock}, envAt(t, f.shared))
					equal(t, map[string]any{base: direct}, envAt(t, f.local))
				},
			},
			"Repair": {
				run: func(t *testing.T, f fixture, ownedSock string) {
					putEnv(t, f.local, map[string]any{discovery: "1", "ANTHROPIC_UNIX_SOCKET": ownedSock})
					check(t, f.Repair(nil))
					equal(t, map[string]any{"ANTHROPIC_UNIX_SOCKET": ownedSock}, envAt(t, f.local))
				},
			},
			"Reconcile": {
				run: func(t *testing.T, f fixture, ownedSock string) {
					putEnv(t, f.local, map[string]any{base: compat, discovery: "1", "ANTHROPIC_UNIX_SOCKET": ownedSock})
					put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
					conflicts, reconciled, err := f.Reconcile(direct, true)
					check(t, err)
					equal(t, 1, len(conflicts))
					equal(t, 1, len(reconciled))
					equal(t, map[string]any{"ANTHROPIC_UNIX_SOCKET": ownedSock}, envAt(t, f.local))
				},
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				f := sandbox(t)
				ownedSock := filepath.Join(filepath.Dir(f.registry), "gateway.sock")
				tt.run(t, f, ownedSock)
			})
		}
	})
}

func TestCleanLegacyEnvNotDeclinedWhenNothingToMigrate(t *testing.T) {
	tests := map[string]struct {
		shared   string
		linkKind string
	}{
		"success: non-gateway shared with local symlinked to user":   {shared: `{"permissions":{"allow":["Read"]}}`, linkKind: "symlink"},
		"success: non-gateway shared with local hard-linked to user": {shared: `{"permissions":{"allow":["Read"]}}`, linkKind: "hardlink"},
		"success: compat shared with local symlinked to user":        {shared: `{"env":{"ANTHROPIC_BASE_URL":"` + compat + `"}}`, linkKind: "symlink"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.shared, tt.shared)
			putEnv(t, f.user, map[string]any{base: direct, "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "950000", "USER_KEEP": "yes"})
			switch tt.linkKind {
			case "symlink":
				check(t, os.Symlink(f.user, f.local))
			case "hardlink":
				check(t, os.Link(f.user, f.local))
			}

			check(t, f.CleanLegacyEnv())
			equal(t, map[string]any{base: direct, "USER_KEEP": "yes"}, envAt(t, f.user))
			equal(t, tt.shared, text(t, f.shared))
		})
	}
}

func TestProjectRemovalSharedAliasingMalformedUserSettings(t *testing.T) {
	tests := map[string]struct {
		linkKind string
	}{
		"success: shared symlinked to malformed user settings returns nil":   {linkKind: "symlink"},
		"success: shared hard-linked to malformed user settings returns nil": {linkKind: "hardlink"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			const malformed = "{"
			put(t, f.user, malformed)
			putEnv(t, f.local, map[string]any{base: direct, discovery: "1", "LOCAL_KEEP": "yes"})
			switch tt.linkKind {
			case "symlink":
				check(t, os.Symlink(f.user, f.shared))
			case "hardlink":
				check(t, os.Link(f.user, f.shared))
			}

			check(t, f.WriteEnv("project", "default", true))
			equal(t, map[string]any{"LOCAL_KEEP": "yes"}, envAt(t, f.local))
			equal(t, malformed, text(t, f.user))
		})
	}

	t.Run("error: unrelated malformed shared settings still returns parse error", func(t *testing.T) {
		f := sandbox(t)
		const malformed = "{"
		put(t, f.shared, malformed)
		putEnv(t, f.local, map[string]any{base: direct, discovery: "1", "LOCAL_KEEP": "yes"})

		err := f.WriteEnv("project", "default", true)
		if err == nil || !strings.Contains(err.Error(), f.shared) {
			t.Fatalf("expected parse error naming %q, got %v", f.shared, err)
		}
		equal(t, malformed, text(t, f.shared))
	})
}

func TestMigrationSourceDeletionRefusesLocalAliasingUser(t *testing.T) {
	tests := map[string]struct {
		linkKind string
	}{
		"success: local swapped to symlink of user before source deletion aborts":  {linkKind: "symlink"},
		"success: local swapped to hardlink of user before source deletion aborts": {linkKind: "hardlink"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.user, map[string]any{base: direct, discovery: "1", "USER_KEEP": "yes"})
			userBefore := text(t, f.user)
			putEnv(t, f.shared, map[string]any{base: direct, discovery: "1", "SHARED_KEEP": "yes"})
			sharedBefore := text(t, f.shared)
			f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
				if file == f.shared {
					check(t, os.Remove(f.local))
					switch tt.linkKind {
					case "symlink":
						check(t, os.Symlink(f.user, f.local))
					case "hardlink":
						check(t, os.Link(f.user, f.local))
					}
				}
				return atomicfile.ReplaceSettings(file, edit)
			}

			migrated, err := f.MigrateLegacy()
			check(t, err)
			equal(t, false, migrated)
			equal(t, sharedBefore, text(t, f.shared))
			equal(t, userBefore, text(t, f.user))
		})
	}
}
