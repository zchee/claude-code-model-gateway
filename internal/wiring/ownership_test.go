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
	"path/filepath"
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

func TestCleanLegacyEnvironment(t *testing.T) {
	const key = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
	tests := map[string]struct {
		url   string
		value any
		keep  bool
	}{
		"success: numeric retired value": {direct, 950000, false}, "success: authored cap": {compat, "940000", true}, "success: foreign endpoint": {"https://example.invalid", "950000", true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			before := envPrefix + `"ANTHROPIC_BASE_URL":` + encoded(t, tt.url) + `, "CLAUDE_CODE_AUTO_COMPACT_WINDOW":` + encoded(t, tt.value) + `, ` + userPair + envSuffix
			for _, file := range []string{f.user, f.local} {
				put(t, file, before)
			}
			check(t, f.CleanLegacyEnv())
			want := before
			if !tt.keep {
				want = strings.Replace(before, `"CLAUDE_CODE_AUTO_COMPACT_WINDOW":950000, `, "", 1)
			}
			for _, file := range []string{f.user, f.local} {
				equal(t, want, text(t, file))
			}
		})
	}
	t.Run("success: migrate first", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.shared, map[string]any{base: direct, key: "950000"})
		check(t, f.CleanLegacyEnv())
		equal(t, map[string]any{base: direct}, envAt(t, f.local))
		equal(t, map[string]any(nil), envAt(t, f.shared))
	})
	t.Run("success: retire scope selection", func(t *testing.T) {
		f := sandbox(t)
		file := filepath.Join(filepath.Dir(f.registry), "wiring.json")
		put(t, file, `{"mode":"local"}`)
		equal(t, true, f.RetireModeConfig())
		absent(t, file)
		equal(t, false, f.RetireModeConfig())
	})
}

func TestExplicitAndAutomaticSettingsChanges(t *testing.T) {
	t.Run("success: unlink leaves foreign URL and cap", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.local, map[string]any{base: "https://example.invalid", "CLAUDE_CODE_AUTO_COMPACT_WINDOW": 325000, discovery: 1})
		check(t, f.WriteEnv("project", "default", true))
		equal(t, map[string]any{base: "https://example.invalid", "CLAUDE_CODE_AUTO_COMPACT_WINDOW": float64(325000)}, envAt(t, f.local))
	})
	t.Run("success: explicit write overwrites selected pins", func(t *testing.T) {
		f := sandbox(t)
		f.Pins.Current = shippedEntries()
		putEnv(t, f.local, map[string]any{opus: "personal-opus"})
		check(t, f.WriteEnv("project", "default", false))
		equal(t, "claude-opus-5-5[1m]", envAt(t, f.local)[opus])
		equal(t, direct, envAt(t, f.local)[base])
	})
	t.Run("success: automatic sync does not install missing aliases or picker", func(t *testing.T) {
		f := sandbox(t)
		f.Pins.Current = shippedEntries()
		putEnv(t, f.local, map[string]any{base: direct})
		put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
		before := text(t, f.local)
		r, err := f.SyncPins()
		check(t, err)
		equal(t, 0, len(r.Changed))
		equal(t, before, text(t, f.local))
	})
	t.Run("success: user write honors migrated project mode", func(t *testing.T) {
		f := sandbox(t)
		putEnv(t, f.shared, gatewaySettings(compat))
		check(t, f.WriteEnv("user", "default", false))
		equal(t, compat, envAt(t, f.user)[base])
		equal(t, compat, envAt(t, f.local)[base])
		equal(t, map[string]any(nil), envAt(t, f.shared))
	})
}

func TestInvalidSettingsWrites(t *testing.T) {
	tests := map[string]struct{ raw, needle string }{"error: malformed settings": {`{"env":`, ""}, "error: ambiguous owned key": {`{"env":{"ANTHROPIC_BASE_URL":"one","ANTHROPIC_BASE_URL":"two"}}`, base}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.local, tt.raw)
			err := f.WriteEnv("project", "default", false)
			if err == nil || !strings.Contains(err.Error(), tt.needle) {
				t.Fatalf("expected settings error containing %q, got %v", tt.needle, err)
			}
			equal(t, tt.raw, text(t, f.local))
		})
	}
}

func TestRepairFileDiagnostics(t *testing.T) {
	tests := map[string]struct {
		raw       string
		malformed bool
	}{"error: unreadable settings": {"{", true}, "success: unwired with no owned values": {`{"env":{"ENABLE_TOOL_SEARCH":"false","MY_VALUE":"keep"}}`, false}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.shared, tt.raw)
			var reports []string
			err := f.Repair(func(s string) { reports = append(reports, s) })
			if tt.malformed {
				if err == nil || !strings.Contains(err.Error(), f.shared) {
					t.Fatalf("expected read error naming %q, got %v", f.shared, err)
				}
			} else {
				check(t, err)
			}
			equal(t, 0, len(reports))
			equal(t, tt.raw, text(t, f.shared))
			absent(t, f.registry)
		})
	}
}

func TestPartialCompatHealth(t *testing.T) {
	tests := map[string]struct{ health wiring.Health }{
		"success: missing hosts entry": {wiring.Health{Port80Bound: true}}, "success: listener failure": {wiring.Health{HostsDetected: true, HostsLine: "127.0.0.1 api.anthropic.com", Reason: "address already in use"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.local, map[string]any{base: compat})
			want := wiring.ModeResult{Mode: "default", Compat: tt.health, Changed: true}
			observation := tt.health
			r, err := f.SyncMode(&observation, "")
			check(t, err)
			equal(t, want, r)
			equal(t, direct, envAt(t, f.local)[base])
		})
	}
}
