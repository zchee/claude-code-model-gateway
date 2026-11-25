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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

func gatewaySettings(url string) map[string]any {
	return map[string]any{base: url, discovery: "1", "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1", "ENABLE_TOOL_SEARCH": "true", "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000"}
}

func TestLegacyMigration(t *testing.T) {
	tests := map[string]struct{ identity string }{
		"success: separate files":        {"separate"},
		"success: absent user file":      {"absent"},
		"success: uncertain identity":    {"loop"},
		"success: shared and user alias": {"alias"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			legacy := gatewaySettings(direct)
			if tt.identity == "separate" {
				legacy["SHARED_UNRELATED"] = "keep-shared"
			}
			putEnv(t, f.shared, legacy)
			legacyBytes := text(t, f.shared)
			switch tt.identity {
			case "loop", "alias":
				check(t, os.MkdirAll(filepath.Dir(f.user), 0o700))
				dest := f.user
				if tt.identity == "alias" {
					dest = f.shared
				}
				check(t, os.Symlink(dest, f.user))
				got, err := f.MigrateLegacy()
				check(t, err)
				equal(t, false, got)
				equal(t, legacyBytes, text(t, f.shared))
				absent(t, f.local)
			case "absent":
				got, err := f.MigrateLegacy()
				check(t, err)
				equal(t, true, got)
				absent(t, f.user)
				equal(t, direct, envAt(t, f.local)[base])
			case "separate":
				user := gatewaySettings(direct)
				user["USER_UNRELATED"] = "keep-user"
				putEnv(t, f.user, user)
				before := text(t, f.user)
				putEnv(t, f.local, map[string]any{base: compat, "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "32000", "PROJECT_LOCAL_UNRELATED": "keep-local"})
				got, err := f.MigrateLegacy()
				check(t, err)
				equal(t, true, got)
				equal(t, before, text(t, f.user))
				equal(t, map[string]any{"SHARED_UNRELATED": "keep-shared"}, envAt(t, f.shared))
				env := envAt(t, f.local)
				equal(t, compat, env[base])
				equal(t, "32000", env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"])
				equal(t, "keep-local", env["PROJECT_LOCAL_UNRELATED"])
				equal(t, "1", env[discovery])
			}
		})
	}
}

func TestVerifiedMode(t *testing.T) {
	healthy := wiring.Health{HostsDetected: true, Port80Bound: true, HostsLine: "127.0.0.1 api.anthropic.com"}
	tests := map[string]struct {
		from, to, required string
		health             *wiring.Health
		ignore             bool
	}{
		"success: verified compat":  {direct, compat, "compat", &healthy, false},
		"success: verified default": {compat, direct, "", &wiring.Health{}, false},
		"error: write not observed": {direct, compat, "compat", &healthy, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.shared, gatewaySettings(tt.from))
			if tt.to == direct {
				putEnv(t, f.local, map[string]any{"PROJECT_LOCAL_VALUE": "keep-me"})
			}
			if tt.ignore {
				f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
					return atomicfile.ReplaceSettings(file, func(old []byte) ([]byte, error) {
						next, err := edit(old)
						if err == nil && file == f.local && bytes.Contains(next, []byte(compat)) {
							return old, nil
						}
						return next, err
					})
				}
			}
			got, err := f.SyncMode(tt.health, tt.required)
			if tt.ignore {
				if err == nil || !strings.Contains(err.Error(), "Could not verify gateway settings") {
					t.Fatalf("expected verification failure, got %v", err)
				}
				equal(t, direct, envAt(t, f.local)[base])
			} else {
				check(t, err)
				wantMode := "compat"
				if tt.to == direct {
					wantMode = "default"
				}
				equal(t, wiring.ModeResult{Mode: wantMode, Changed: true, Compat: *tt.health}, got)
				equal(t, tt.to, envAt(t, f.local)[base])
				if tt.to == direct {
					equal(t, "keep-me", envAt(t, f.local)["PROJECT_LOCAL_VALUE"])
				}
			}
			_, present := object(t, text(t, f.shared))["env"]
			equal(t, false, present)
		})
	}
}

func TestUnknownHealth(t *testing.T) {
	f := sandbox(t)
	putEnv(t, f.shared, gatewaySettings(direct))
	before := text(t, f.shared)
	got, err := f.SyncMode(nil, "compat")
	check(t, err)
	equal(t, wiring.ModeResult{Mode: "default"}, got)
	equal(t, before, text(t, f.shared))
	absent(t, f.local)
}

func TestEnvironmentOnlyMode(t *testing.T) {
	tests := map[string]struct{ url, mode string }{
		"success: default": {direct, "default"},
		"success: compat":  {compat, "compat"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.env[base] = tt.url
			health := wiring.Health{HostsDetected: true, Port80Bound: true}
			got, err := f.SyncMode(&health, "compat")
			check(t, err)
			equal(t, wiring.ModeResult{Mode: tt.mode, Compat: health}, got)
			absent(t, f.local)
			absent(t, f.registry)
			absent(t, filepath.Join(filepath.Dir(f.registry), "wiring.json"))
			for _, dir := range []string{f.home, f.Project} {
				entries, err := os.ReadDir(dir)
				check(t, err)
				equal(t, 0, len(entries))
			}
		})
	}
}
