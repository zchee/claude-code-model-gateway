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
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadinessHookForms(t *testing.T) {
	tests := map[string]struct{ legacy bool }{
		"success: replace legacy hook": {true},
		"success: refresh native hook": {false},
	}
	t.Run("success: migrate registered hooks", migrateRegisteredHooks)
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			node := fmt.Sprintf(`node "%s" readiness-gate`, f.legacy)
			owned := map[string]any{"type": "command", "command": f.bin, "args": []string{"readiness-gate"}, "onFailure": "block"}
			if tt.legacy {
				owned["command"] = node
				delete(owned, "args")
			}
			other := map[string]any{"type": "command", "command": f.bin, "args": []string{"readiness-gate", "user-option"}}
			otherLegacy := map[string]any{"type": "command", "command": node + " --custom"}
			group := func(hooks ...any) map[string]any { return map[string]any{"matcher": "user", "hooks": hooks} }
			untouched := map[string]any{"noHookArray": true}
			permissions := map[string]any{"allow": []string{"Read"}}
			stop := []any{map[string]any{}}
			input := map[string]any{"hooks": map[string]any{"SessionStart": []any{group(other, owned, otherLegacy), untouched, group(owned)}, "UserPromptSubmit": []any{group(owned)}, "Stop": stop}, "permissions": permissions}
			before := encoded(t, input)
			equal(t, true, f.ReadinessHookPresent([]byte(before)))
			got, err := f.SyncReadinessHooks([]byte(before), true)
			check(t, err)
			hook := map[string]any{"type": "command", "command": f.bin, "args": []string{"readiness-gate"}, "timeout": 35}
			expected := map[string]any{"hooks": map[string]any{"SessionStart": []any{group(other, otherLegacy), untouched, map[string]any{"matcher": "startup|resume", "hooks": []any{hook}}}, "UserPromptSubmit": []any{map[string]any{"hooks": []any{hook}}}, "Stop": stop}, "permissions": permissions}
			equal(t, object(t, encoded(t, expected)), object(t, string(got)))
			equal(t, true, f.ReadinessHookPresent(got))
			repeat, err := f.SyncReadinessHooks(got, true)
			check(t, err)
			equal(t, string(got), string(repeat))
			off, err := f.SyncReadinessHooks(repeat, false)
			check(t, err)
			expected["hooks"] = map[string]any{"SessionStart": []any{group(other, otherLegacy), untouched}, "Stop": stop}
			equal(t, object(t, encoded(t, expected)), object(t, string(off)))
			equal(t, false, f.ReadinessHookPresent(off))
		})
	}
}

func TestUnwirePreservesUserHookBytes(t *testing.T) {
	f := sandbox(t)
	user := `{ "type" : "command", "command" : "user hook", "metadata": [ 1e+01, "A" ] }`
	prefix := "{\r\n\t\"hooks\" : { \"SessionStart\" : [ { \"matcher\" : \"custom\", \"hooks\": [ " + user
	suffix := " ] } ] },\r\n\t\"permissions\": { \"allow\" : [ \"Read\" ] }\r\n}"
	owned := encoded(t, map[string]any{"type": "command", "command": f.bin, "args": []string{"readiness-gate"}})
	got, err := f.SyncReadinessHooks([]byte(prefix+",\r\n\t\t"+owned+suffix), false)
	check(t, err)
	equal(t, prefix+suffix, string(got))
}

func TestDisabledReadinessHooks(t *testing.T) {
	f := sandbox(t)
	original := "{\r\n\t\"disableAllHooks\" : true,\r\n\t\"hooks\" : { \"Stop\": [ {} ] }\r\n}"
	got, err := f.SyncReadinessHooks([]byte(original), true)
	check(t, err)
	equal(t, original, string(got))
	equal(t, false, f.ReadinessHookPresent(got))
}

func migrateRegisteredHooks(t *testing.T) {
	f := sandbox(t)
	fields := `"env" : { "ANTHROPIC_BASE_URL" : "` + direct + `", "CUSTOM" : "retained" },` + "\r\n\t" + `"permissions" : { "allow": [ "Read" ] }`
	original := "{\r\n\t" + fields + "\r\n}"
	put(t, f.local, original)
	put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
	unregistered := filepath.Join(t.TempDir(), ".claude", "settings.local.json")
	put(t, unregistered, original)
	check(t, f.MigrateReadinessHooks())
	migrated := text(t, f.local)
	if !strings.Contains(migrated, fields) {
		t.Fatalf("unrelated settings bytes changed: %q", migrated)
	}
	equal(t, true, f.ReadinessHookPresent([]byte(migrated)))
	want := map[string]any{"type": "command", "command": f.bin, "args": []string{"readiness-gate"}, "timeout": 35}
	var shape struct {
		Hooks struct {
			UserPromptSubmit []struct {
				Hooks []map[string]any `json:"hooks"`
			}
		} `json:"hooks"`
	}
	decode(t, migrated, &shape)
	if len(shape.Hooks.UserPromptSubmit) != 1 {
		t.Fatalf("prompt groups: %+v", shape.Hooks.UserPromptSubmit)
	}
	equal(t, []map[string]any{object(t, encoded(t, want))}, shape.Hooks.UserPromptSubmit[0].Hooks)
	absent(t, f.user)
	equal(t, original, text(t, unregistered))
}
