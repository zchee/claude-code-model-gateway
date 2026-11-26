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

func TestAutomaticMigrationPreservesEffectiveBaseURL(t *testing.T) {
	tests := map[string]struct {
		localBase  any
		userBase   string
		hasUser    bool
		sharedBase string
	}{
		"success: null local base with user legacy fallback":       {localBase: nil, userBase: compat, hasUser: true, sharedBase: direct},
		"success: numeric local base with user legacy fallback":    {localBase: 42, userBase: compat, hasUser: true, sharedBase: direct},
		"success: boolean local base with user foreign fallback":   {localBase: false, userBase: "https://example.invalid", hasUser: true, sharedBase: direct},
		"success: object local base with no user fallback":         {localBase: map[string]any{"x": 1}, hasUser: false, sharedBase: direct},
		"success: null local base with absent user stays anchored": {localBase: nil, hasUser: false, sharedBase: direct},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.local, map[string]any{base: tt.localBase, "LOCAL_KEEP": "yes"})
			putEnv(t, f.shared, map[string]any{base: tt.sharedBase, discovery: "1", "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "950000"})
			if tt.hasUser {
				putEnv(t, f.user, map[string]any{base: tt.userBase})
			}
			localBefore := text(t, f.local)
			sharedBefore := text(t, f.shared)
			var userBefore string
			if tt.hasUser {
				userBefore = text(t, f.user)
			}

			beforeEff, err := f.EffectiveBaseURL()
			check(t, err)
			equal(t, "project-shared", beforeEff.Winner.Source)
			equal(t, tt.sharedBase, beforeEff.Winner.Value)

			migrated, err := f.MigrateLegacy()
			check(t, err)
			equal(t, false, migrated)
			equal(t, localBefore, text(t, f.local))
			equal(t, sharedBefore, text(t, f.shared))

			modeResult, err := f.SyncMode(nil, "")
			check(t, err)
			equal(t, "default", modeResult.Mode)
			equal(t, false, modeResult.Changed)
			equal(t, localBefore, text(t, f.local))
			equal(t, sharedBefore, text(t, f.shared))

			check(t, f.CleanLegacyEnv())
			equal(t, localBefore, text(t, f.local))
			equal(t, sharedBefore, text(t, f.shared))
			if tt.hasUser {
				equal(t, userBefore, text(t, f.user))
			}

			afterEff, err := f.EffectiveBaseURL()
			check(t, err)
			equal(t, beforeEff.Winner, afterEff.Winner)
		})
	}
}

func TestMigrationSkipsSharedLocalAliases(t *testing.T) {
	tests := map[string]struct{ linkKind string }{
		"success: symlink local to shared":  {"symlink"},
		"success: hardlink local to shared": {"hardlink"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.shared, gatewaySettings(direct))
			before := text(t, f.shared)
			switch tt.linkKind {
			case "symlink":
				check(t, os.Symlink(f.shared, f.local))
			case "hardlink":
				check(t, os.Link(f.shared, f.local))
			}

			migrated, err := f.MigrateLegacy()
			check(t, err)
			equal(t, false, migrated)
			equal(t, before, text(t, f.shared))
			equal(t, before, text(t, f.local))

			res, err := f.SyncMode(nil, "")
			check(t, err)
			equal(t, "default", res.Mode)
			equal(t, false, res.Changed)
			equal(t, before, text(t, f.shared))

			check(t, f.CleanLegacyEnv())
			equal(t, before, text(t, f.shared))
		})
	}
}

func TestReplaceInterleavingsReevaluateOwnership(t *testing.T) {
	t.Run("success: window changed to personal value before replacement", func(t *testing.T) {
		f := sandbox(t)
		put(t, f.local, `{"env":{"ANTHROPIC_BASE_URL":"`+direct+`"},"autoCompactWindow":400000}`)
		replaced := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `"},"autoCompactWindow":333333}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			return atomicfile.ReplaceSettings(file, func([]byte) ([]byte, error) {
				// Simulate another writer saving a personal window right before this attempt sees the file.
				put(t, file, replaced)
				return edit([]byte(replaced))
			})
		}
		changes, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
		check(t, err)
		equal(t, 0, len(changes.Changed))
		if len(changes.Skipped) != 1 || changes.Skipped[0].File != f.local || !strings.Contains(changes.Skipped[0].Reason, "333333") {
			t.Fatalf("expected skip for personal window 333333, got %+v", changes.Skipped)
		}
		// File stays untouched, with no hooks added.
		equal(t, replaced, text(t, f.local))
	})

	t.Run("success: legacy URL corrected to default before confirmed reconcile", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.local, map[string]any{base: compat, discovery: "1"})
		put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
		corrected := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			put(t, file, corrected)
			return atomicfile.ReplaceSettings(file, edit)
		}
		conflicts, reconciled, err := f.Reconcile(direct, true)
		check(t, err)
		equal(t, 1, len(conflicts))
		equal(t, 0, len(reconciled))
		equal(t, corrected, text(t, f.local))
	})

	t.Run("success: base URL added after orphan scan before repair", func(t *testing.T) {
		f := sandbox(t)
		f.Pins.Current = shippedEntries()
		f.Pins.Owned = map[string][]string{opus: {"claude-opus-5[1m]"}}
		putEnv(t, f.local, map[string]any{discovery: "1", opus: "claude-opus-5[1m]"})
		wired := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1","ANTHROPIC_DEFAULT_OPUS_MODEL":"claude-opus-5[1m]"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			put(t, file, wired)
			return atomicfile.ReplaceSettings(file, edit)
		}
		var reports []string
		check(t, f.Repair(func(s string) { reports = append(reports, s) }))
		equal(t, 0, len(reports))
		equal(t, wired, text(t, f.local))
	})

	t.Run("success: migration keeps shared value that became personal before source deletion", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.shared, map[string]any{base: direct, discovery: "1", "ENABLE_TOOL_SEARCH": "true"})
		editedShared := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1","ENABLE_TOOL_SEARCH":"false"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			if file == f.shared {
				put(t, file, editedShared)
			}
			return atomicfile.ReplaceSettings(file, edit)
		}
		migrated, err := f.MigrateLegacy()
		check(t, err)
		equal(t, true, migrated)
		equal(t, map[string]any{"ENABLE_TOOL_SEARCH": "false"}, envAt(t, f.shared))
	})

	t.Run("success: migration aborts source deletion when shared base became personal", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.shared, map[string]any{base: direct, discovery: "1"})
		editedShared := `{"env":{"ANTHROPIC_BASE_URL":"https://personal.example","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			if file == f.shared {
				put(t, file, editedShared)
			}
			return atomicfile.ReplaceSettings(file, edit)
		}
		migrated, err := f.MigrateLegacy()
		check(t, err)
		equal(t, false, migrated)
		equal(t, editedShared, text(t, f.shared))
	})

	t.Run("success: pin became personal before pin sync replacement", func(t *testing.T) {
		f := sandbox(t)
		f.Pins = wiring.Pins{
			Current: []wiring.Entry{{Key: opus, Value: "claude-opus-5-5[1m]"}},
			Owned:   map[string][]string{opus: {"claude-opus-5[1m]", "claude-opus-5-5[1m]"}},
		}
		putEnv(t, f.local, map[string]any{base: direct, opus: "claude-opus-5[1m]"})
		put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
		personal := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","ANTHROPIC_DEFAULT_OPUS_MODEL":"my-custom-opus"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			put(t, file, personal)
			return atomicfile.ReplaceSettings(file, edit)
		}
		changes, err := f.SyncPins()
		check(t, err)
		equal(t, 0, len(changes.Changed))
		if len(changes.Skipped) != 1 || changes.Skipped[0].Key != opus || changes.Skipped[0].Value != "my-custom-opus" {
			t.Fatalf("expected skipped personal pin, got %+v", changes.Skipped)
		}
		equal(t, personal, text(t, f.local))
	})

	t.Run("success: base changed to foreign before legacy env cleanup", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.local, map[string]any{base: direct, "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "950000"})
		foreign := `{"env":{"ANTHROPIC_BASE_URL":"https://personal.example","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"950000"}}`
		f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
			put(t, file, foreign)
			return atomicfile.ReplaceSettings(file, edit)
		}
		check(t, f.CleanLegacyEnv())
		equal(t, foreign, text(t, f.local))
	})
}

func TestDuplicateOwnedKeysRefusedByteIdentical(t *testing.T) {
	tests := map[string]struct {
		raw    string
		needle string
		run    func(*testing.T, fixture) error
	}{
		"error: removal coalescing duplicate base URL": {
			raw:    `{"env":{"ANTHROPIC_BASE_URL":"personal","ANTHROPIC_BASE_URL":"` + compat + `"}}`,
			needle: base,
			run:    func(_ *testing.T, f fixture) error { return f.WriteEnv("project", "default", true) },
		},
		"error: escaped duplicate base URL on removal": {
			raw:    `{"env":{"ANTHROPIC_BASE_URL":"personal","ANTHROPIC_BASE_\u0055RL":"` + compat + `"}}`,
			needle: base,
			run:    func(_ *testing.T, f fixture) error { return f.WriteEnv("project", "default", true) },
		},
		"error: write equal-value shortcut hides duplicate base URL": {
			raw:    `{"env":{"ANTHROPIC_BASE_URL":"personal","ANTHROPIC_BASE_URL":"` + direct + `","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"0"}}`,
			needle: base,
			run:    func(_ *testing.T, f fixture) error { return f.WriteEnv("project", "default", false) },
		},
		"error: duplicate env object on empty env removal": {
			raw:    `{"env":{"KEEP":"1"},"env":{}}`,
			needle: "env",
			run:    func(_ *testing.T, f fixture) error { return f.WriteEnv("project", "default", true) },
		},
		"error: duplicate autoCompactWindow on window sync": {
			raw:    `{"autoCompactWindow":325000,"autoCompactWindow":400000}`,
			needle: "autoCompactWindow",
			run: func(_ *testing.T, f fixture) error {
				_, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
				return err
			},
		},
		"error: duplicate autoCompactWindow on equal target value": {
			raw:    `{"autoCompactWindow":325000,"autoCompactWindow":500000}`,
			needle: "autoCompactWindow",
			run: func(_ *testing.T, f fixture) error {
				_, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
				return err
			},
		},
		"error: duplicate SessionStart hook event on write": {
			raw:    `{"hooks":{"SessionStart":[],"SessionStart":[]}}`,
			needle: "SessionStart",
			run:    func(_ *testing.T, f fixture) error { return f.WriteEnv("project", "default", false) },
		},
		"error: duplicate legacy compact window on CleanLegacyEnv": {
			raw:    `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"325000","CLAUDE_CODE_AUTO_COMPACT_WINDOW":"950000"}}`,
			needle: "CLAUDE_CODE_AUTO_COMPACT_WINDOW",
			run:    func(_ *testing.T, f fixture) error { return f.CleanLegacyEnv() },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.local, tt.raw)
			err := tt.run(t, f)
			if err == nil || !strings.Contains(err.Error(), tt.needle) {
				t.Fatalf("expected error naming %q, got %v", tt.needle, err)
			}
			equal(t, tt.raw, text(t, f.local))
		})
	}
}

func TestBOMPrefixedSettingsEndToEnd(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	tests := map[string]struct {
		raw string
	}{
		"success: BOM with CRLF and trailing whitespace": {
			raw: bom + "{\r\n  \"env\": {\r\n    \"ANTHROPIC_BASE_URL\": \"" + compat + "\",\r\n    \"USER_KEEP\": \"ok\"\r\n  }\r\n}  \t\r\n",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.local, tt.raw)

			// Visible to resolution and readiness checks.
			res, err := f.EffectiveBaseURL()
			check(t, err)
			if res.Winner == nil || res.Winner.Source != "project-local" || res.Winner.Value != compat {
				t.Fatalf("expected BOM-prefixed local legacy winner, got %+v", res)
			}
			wired, err := f.IsWired()
			check(t, err)
			equal(t, true, wired)
			mode, err := f.WiredMode()
			check(t, err)
			equal(t, &wiring.Wiring{Scope: "project", Mode: "compat", Source: "project-local", File: f.local}, mode)

			// Explicit write keeps leading BOM, CRLF and trailing whitespace.
			check(t, f.WriteEnv("project", "default", false))
			written := text(t, f.local)
			if !strings.HasPrefix(written, bom) {
				t.Fatalf("leading BOM lost after write: %q", written[:min(16, len(written))])
			}
			equal(t, ending(tt.raw), ending(written))
			equal(t, strings.Count(written, "\r\n"), strings.Count(written, "\n"))
			res, err = f.EffectiveBaseURL()
			check(t, err)
			equal(t, direct, res.Winner.Value)

			// Explicit removal keeps leading BOM, CRLF, trailing whitespace and USER_KEEP.
			check(t, f.WriteEnv("project", "default", true))
			removed := text(t, f.local)
			if !strings.HasPrefix(removed, bom) {
				t.Fatalf("leading BOM lost after removal: %q", removed[:min(16, len(removed))])
			}
			equal(t, ending(tt.raw), ending(removed))
			equal(t, strings.Count(removed, "\r\n"), strings.Count(removed, "\n"))
			res, err = f.EffectiveBaseURL()
			check(t, err)
			equal(t, (*wiring.Definition)(nil), res.Winner)
			env := object(t, strings.TrimPrefix(removed, bom))["env"].(map[string]any)
			equal(t, map[string]any{"USER_KEEP": "ok"}, env)
		})
	}

	t.Run("success: BOM empty object preserves BOM and no trailing newline", func(t *testing.T) {
		f := sandbox(t)
		f.Pins.Current = shippedEntries()
		put(t, f.local, bom+"{}")
		check(t, f.WriteEnv("project", "default", false))
		got := text(t, f.local)
		equal(t, bom+strings.TrimSuffix(explicitSettings(t, f), "\n"), got)
	})

	t.Run("error: double BOM is still rejected", func(t *testing.T) {
		f := sandbox(t)
		raw := bom + bom + `{"env":{"ANTHROPIC_BASE_URL":"` + compat + `"}}`
		put(t, f.local, raw)
		res, err := f.EffectiveBaseURL()
		check(t, err)
		equal(t, (*wiring.Definition)(nil), res.Winner)
		if err := f.WriteEnv("project", "default", false); err == nil {
			t.Fatal("double BOM accepted")
		}
		equal(t, raw, text(t, f.local))
	})
}

func TestProjectRemovalClearsBothProjectFilesOnly(t *testing.T) {
	tests := map[string]struct {
		localEnv  map[string]any
		sharedEnv map[string]any
		wantLocal map[string]any
		wantShare map[string]any
		noLocal   bool
	}{
		"success: shared-only legacy URL cleared without creating local": {
			noLocal:   true,
			sharedEnv: map[string]any{base: compat, discovery: "1", "TEAM_KEY": "keep"},
			wantShare: map[string]any{"TEAM_KEY": "keep"},
		},
		"success: shared-only default URL cleared and empty env removed": {
			noLocal:   true,
			sharedEnv: map[string]any{base: direct, discovery: "1"},
			wantShare: nil,
		},
		"success: both shared legacy and local default cleared while personal values survive": {
			localEnv:  map[string]any{base: direct, opus: "my-local-opus", discovery: "1"},
			sharedEnv: map[string]any{base: compat, sonnet: "my-shared-sonnet", discovery: "1"},
			wantLocal: map[string]any{opus: "my-local-opus"},
			wantShare: map[string]any{sonnet: "my-shared-sonnet"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.Pins = wiring.Pins{
				Current: shippedEntries()[:2],
				Owned:   map[string][]string{opus: {"claude-opus-5-5[1m]"}, sonnet: {"claude-sonnet-5-5[1m]"}},
			}
			if !tt.noLocal {
				putEnv(t, f.local, tt.localEnv)
			}
			putEnv(t, f.shared, tt.sharedEnv)

			// User scope and another registered project must remain untouched.
			putEnv(t, f.user, map[string]any{base: compat, "USER_KEEP": "yes"})
			userBefore := text(t, f.user)
			otherProject := t.TempDir()
			otherLocal := filepath.Join(otherProject, ".claude", "settings.local.json")
			putEnv(t, otherLocal, map[string]any{base: compat})
			otherBefore := text(t, otherLocal)
			put(t, f.registry, encoded(t, map[string]any{"projects": []string{otherProject}}))

			check(t, f.WriteEnv("project", "default", true))

			if tt.noLocal {
				absent(t, f.local)
			} else {
				equal(t, tt.wantLocal, envAt(t, f.local))
			}
			equal(t, tt.wantShare, envAt(t, f.shared))
			equal(t, userBefore, text(t, f.user))
			equal(t, otherBefore, text(t, otherLocal))

			// Effective base URL falls through to user scope; neither project file defines a gateway URL.
			res, err := f.EffectiveBaseURL()
			check(t, err)
			equal(t, "user", res.Winner.Source)
			equal(t, 0, len(res.Shadowed))
		})
	}

	t.Run("success: project shared aliasing user file leaves user file untouched", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.user, map[string]any{base: compat, discovery: "1"})
		userBefore := text(t, f.user)
		check(t, os.MkdirAll(filepath.Dir(f.shared), 0o700))
		check(t, os.Symlink(f.user, f.shared))

		check(t, f.WriteEnv("project", "default", true))
		equal(t, userBefore, text(t, f.user))
	})
}

func TestExplicitWriteRemovesRetiredMaxContextTokens(t *testing.T) {
	const key = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	tests := map[string]struct {
		value any
		keep  bool
	}{
		"success: retired string value removed":    {"920000", false},
		"success: retired numeric value removed":   {920000, false},
		"success: personal string value preserved": {"800000", true},
		"success: personal numeric value retained": {800000, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.local, map[string]any{key: tt.value, "USER_KEEP": "1"})
			check(t, f.WriteEnv("project", "default", false))
			env := envAt(t, f.local)
			got, present := env[key]
			equal(t, tt.keep, present)
			if tt.keep {
				switch v := tt.value.(type) {
				case int:
					equal(t, float64(v), got)
				default:
					equal(t, v, got)
				}
			}
			equal(t, "1", env["USER_KEEP"])
		})
	}

	t.Run("success: automatic cleanup does not delete CLAUDE_CODE_MAX_CONTEXT_TOKENS", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.local, map[string]any{base: direct, key: "920000"})
		before := text(t, f.local)
		check(t, f.CleanLegacyEnv())
		equal(t, before, text(t, f.local))
	})
}

func TestReconcileRejectsRetiredCompatTarget(t *testing.T) {
	tests := map[string]struct{ confirm bool }{
		"error: preview rejects compat target": {false},
		"error: confirm rejects compat target": {true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.local, map[string]any{base: direct, discovery: "1"})
			localBefore := text(t, f.local)
			// Include a missing project in the registry so any scan/prune would rewrite the registry.
			missing := filepath.Join(f.home, "missing-project")
			regBefore := encoded(t, map[string]any{"projects": []string{f.Project, missing}})
			put(t, f.registry, regBefore)

			conflicts, reconciled, err := f.Reconcile(compat, tt.confirm)
			if err == nil {
				t.Fatalf("expected error for retired compat target, got conflicts=%+v reconciled=%+v", conflicts, reconciled)
			}
			equal(t, 0, len(conflicts))
			equal(t, 0, len(reconciled))
			equal(t, localBefore, text(t, f.local))
			equal(t, regBefore, text(t, f.registry))
		})
	}
}

func TestExplicitRemovalDeletesPresentEmptyEnvObject(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want string
	}{
		"success: present empty env with sibling deleted": {raw: `{"env":{},"model":"x"}`, want: `{"model":"x"}`},
		"success: standalone empty env deleted":           {raw: `{"env":{}}`, want: `{}`},
		"success: absent env stays absent byte-identical": {raw: `{"model" : "x"}`, want: `{"model" : "x"}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.local, tt.raw)
			check(t, f.WriteEnv("project", "default", true))
			equal(t, tt.want, text(t, f.local))
		})
	}
}

func TestReportsPreserveLoneSurrogatesLosslessly(t *testing.T) {
	f := sandbox(t)
	f.Pins = wiring.Pins{
		Current: []wiring.Entry{{Key: opus, Value: "claude-opus-5-5[1m]"}, {Key: sonnet, Value: "claude-sonnet-5-5[1m]"}},
		Owned:   map[string][]string{opus: {"claude-opus-5-5[1m]"}, sonnet: {"claude-sonnet-5-5[1m]"}},
	}
	raw := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `","ANTHROPIC_DEFAULT_OPUS_MODEL":"\ud800","ANTHROPIC_DEFAULT_SONNET_MODEL":{"\ud800":1,"\ud801":2}}}`
	put(t, f.local, raw)
	put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))

	disagreements, _, err := f.PinDisagreements()
	check(t, err)
	if len(disagreements) != 2 {
		t.Fatalf("disagreements: %+v", disagreements)
	}
	opusWire, ok := disagreements[0].Stale.(jsonx.WireValue)
	if !ok {
		t.Fatalf("expected jsonx.WireValue for lone surrogate stale pin, got %T (%v)", disagreements[0].Stale, disagreements[0].Stale)
	}
	opusJSON, err := jsonx.EncodeWire(opusWire)
	check(t, err)
	equal(t, `"\ud800"`, string(opusJSON))
	if bytes.Contains(opusJSON, []byte("\ufffd")) {
		t.Fatalf("lone surrogate collapsed to replacement char: %s", opusJSON)
	}
	sonnetWire, ok := disagreements[1].Stale.(jsonx.WireValue)
	if !ok {
		t.Fatalf("expected jsonx.WireValue for lone surrogate object keys, got %T (%v)", disagreements[1].Stale, disagreements[1].Stale)
	}
	sonnetJSON, err := jsonx.EncodeWire(sonnetWire)
	check(t, err)
	equal(t, `{"\ud800":1,"\ud801":2}`, string(sonnetJSON))

	// EncodeState also serializes the full report structs losslessly.
	encodedReport, err := jsonx.EncodeState(disagreements)
	check(t, err)
	if !bytes.Contains(encodedReport, []byte(`"\ud800"`)) || !bytes.Contains(encodedReport, []byte(`"\ud801"`)) {
		t.Fatalf("EncodeState lost lone surrogates:\n%s", encodedReport)
	}
	if bytes.Contains(encodedReport, []byte("\ufffd")) {
		t.Fatalf("EncodeState replaced lone surrogate with U+FFFD:\n%s", encodedReport)
	}

	// SyncPins also preserves the lone surrogate in its Skipped report.
	changes, err := f.SyncPins()
	check(t, err)
	if len(changes.Skipped) != 1 || changes.Skipped[0].Key != opus {
		t.Fatalf("skipped: %+v", changes.Skipped)
	}
	skippedWire, ok := changes.Skipped[0].Value.(jsonx.WireValue)
	if !ok {
		t.Fatalf("expected jsonx.WireValue in Skipped.Value, got %T", changes.Skipped[0].Value)
	}
	skippedJSON, err := jsonx.EncodeWire(skippedWire)
	check(t, err)
	equal(t, `"\ud800"`, string(skippedJSON))
	encodedSkipped, err := json.Marshal(changes.Skipped[0].Value, jsontext.PreserveRawStrings(true), jsontext.AllowInvalidUTF8(true))
	check(t, err)
	equal(t, `"\ud800"`, string(encodedSkipped))
}

func TestHTTPSOverrideASCIIOnlyCaseFolding(t *testing.T) {
	tests := map[string]struct {
		url    string
		unsafe bool
	}{
		"success: uppercase ASCII host":                   {"https://API.ANTHROPIC.COM", true},
		"success: mixed-case ASCII host and port 443":     {"HTTPS://Api.Anthropic.Com:443/", true},
		"success: percent-encoded lowercase ASCII host":   {"https://%61pi.anthropic.com", true},
		"success: percent-encoded uppercase ASCII host":   {"https://%41PI.ANTHROPIC.COM", true},
		"success: Kelvin sign in host does not fold":      {"https://api.anthropi\u212a.com", false},
		"success: dotless i in host does not fold":        {"https://ap\u0131.anthropic.com", false},
		"success: dotted capital I in host does not fold": {"https://ap\u0130.anthropic.com", false},
		"success: percent-encoded non-ASCII host":         {"https://ap%c4%b0.anthropic.com", false},
		"success: long s in scheme does not fold":         {"http\u017f://api.anthropic.com", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.env[base] = tt.url
			r, err := f.EffectiveBaseURL()
			check(t, err)
			equal(t, tt.unsafe, r.UnsafeProcessOverride() != nil)
		})
	}
}
