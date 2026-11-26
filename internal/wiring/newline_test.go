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
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/atomicfile"
)

// The goldens below are spelled as JSON.stringify(settings, null, 2) prints the same members, which is how the
// source writes every settings file (lib/settings-wiring.js:107-112).

// explicitEnv is the env member an explicit write creates with the shipped pins.
const explicitEnv = `  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:49123",
    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
    "CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK": "1",
    "ENABLE_TOOL_SEARCH": "true",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "64000",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5-5[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5-5[1m]",
    "ANTHROPIC_DEFAULT_FABLE_MODEL": "claude-fable-5-1[1m]",
    "ANTHROPIC_CUSTOM_MODEL_OPTION": "claude-fable-5-1[1m]",
    "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME": "Fable",
    "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION": "Fable 5.1 · current Fable, through the gateway"
  }`

// hooksMember is the readiness hook member of a wired settings file.
func hooksMember(t *testing.T, f fixture) string {
	t.Helper()
	const member = `  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": BIN,
            "args": [
              "readiness-gate"
            ],
            "timeout": 35
          }
        ],
        "matcher": "startup|resume"
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": BIN,
            "args": [
              "readiness-gate"
            ],
            "timeout": 35
          }
        ]
      }
    ]
  }`
	return strings.ReplaceAll(member, "BIN", encoded(t, f.bin))
}

// explicitSettings is the whole file an explicit write creates. The write installs the hooks before it assigns
// the env member, so the hooks come first (lib/commands.js:1189-1190).
func explicitSettings(t *testing.T, f fixture) string {
	t.Helper()
	return "{\n" + hooksMember(t, f) + ",\n" + explicitEnv + "\n}\n"
}

// migratedSettings is the whole local file a migration creates from the owned entries: the env member first,
// then the hooks the settings writer adds (lib/settings-wiring.js:386-390,109).
func migratedSettings(t *testing.T, f fixture, env string) string {
	t.Helper()
	return "{\n  \"env\": {\n" + env + "\n  },\n" + hooksMember(t, f) + "\n}\n"
}

func TestCreatedSettingsBytes(t *testing.T) {
	const discoveryLine = `    "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1"`
	tests := map[string]struct {
		run  func(*testing.T, fixture)
		want func(*testing.T, fixture) map[string]string
	}{
		"success: explicit write, local project scope": {
			run: func(t *testing.T, f fixture) { check(t, f.WriteEnv("project", "default", false)) },
			want: func(t *testing.T, f fixture) map[string]string {
				return map[string]string{f.local: explicitSettings(t, f)}
			},
		},
		"success: explicit write, shared project scope": {
			run: func(t *testing.T, f fixture) { check(t, f.WriteEnv("project-shared", "default", false)) },
			want: func(t *testing.T, f fixture) map[string]string {
				return map[string]string{f.shared: explicitSettings(t, f)}
			},
		},
		"success: explicit write, user scope": {
			run: func(t *testing.T, f fixture) { check(t, f.WriteEnv("user", "default", false)) },
			want: func(t *testing.T, f fixture) map[string]string {
				return map[string]string{f.user: explicitSettings(t, f)}
			},
		},
		"success: automatic migration creates the local file": {
			run: func(t *testing.T, f fixture) {
				put(t, f.shared, `{"env":{"ANTHROPIC_BASE_URL":"`+direct+`","SHARED":"keep","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1","ENABLE_TOOL_SEARCH":"true"}}`)
				migrated, err := f.MigrateLegacy()
				check(t, err)
				equal(t, true, migrated)
			},
			want: func(t *testing.T, f fixture) map[string]string {
				env := `    "ANTHROPIC_BASE_URL": "` + direct + `",` + "\n" + discoveryLine + ",\n" + `    "ENABLE_TOOL_SEARCH": "true"`
				// The shared file existed, so the bytes after its closing brace stay as authored: none.
				return map[string]string{f.local: migratedSettings(t, f, env), f.shared: `{"env":{"SHARED":"keep"}}`}
			},
		},
		"success: explicit write migrates into a created local file": {
			run: func(t *testing.T, f fixture) {
				put(t, f.shared, `{"env":{"ANTHROPIC_BASE_URL":"`+compat+`","CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY":"1"}}`+"\n")
				check(t, f.WriteEnv("user", "default", false))
			},
			want: func(t *testing.T, f fixture) map[string]string {
				env := `    "ANTHROPIC_BASE_URL": "` + direct + `",` + "\n" + discoveryLine
				// The shared file existed, so it keeps the one newline its author ended it with.
				return map[string]string{f.local: migratedSettings(t, f, env), f.user: explicitSettings(t, f), f.shared: "{}\n"}
			},
		},
		"success: context window sync": {
			run: func(t *testing.T, f fixture) {
				r, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
				check(t, err)
				equal(t, []string{f.local}, r.Changed)
			},
			want: func(_ *testing.T, f fixture) map[string]string {
				return map[string]string{f.local: "{\n  \"autoCompactWindow\": 500000\n}\n"}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.Pins.Current = shippedEntries()
			tt.run(t, f)
			for file, want := range tt.want(t, f) {
				equal(t, want, text(t, file))
			}
		})
	}
}

func TestMissingSettingsStayAbsent(t *testing.T) {
	register := func(t *testing.T, f fixture) {
		t.Helper()
		put(t, f.registry, encoded(t, map[string]any{"projects": []string{f.Project}}))
	}
	tests := map[string]struct {
		run func(*testing.T, fixture)
	}{
		"success: unwire, local project scope":  {func(t *testing.T, f fixture) { check(t, f.WriteEnv("project", "default", true)) }},
		"success: unwire, shared project scope": {func(t *testing.T, f fixture) { check(t, f.WriteEnv("project-shared", "default", true)) }},
		"success: unwire, user scope":           {func(t *testing.T, f fixture) { check(t, f.WriteEnv("user", "default", true)) }},
		"success: readiness hook migration": {func(t *testing.T, f fixture) {
			register(t, f)
			check(t, f.MigrateReadinessHooks())
		}},
		"success: full context window": {func(t *testing.T, f fixture) {
			r, err := f.SyncWindow([]string{f.local, f.shared, f.user}, new(400000), nil)
			check(t, err)
			equal(t, 0, len(r.Changed))
		}},
		"success: pin sync and reconcile": {func(t *testing.T, f fixture) {
			register(t, f)
			_, err := f.SyncPins()
			check(t, err)
			_, _, err = f.Reconcile(direct, true)
			check(t, err)
		}},
		"success: legacy cleanup and repair": {func(t *testing.T, f fixture) {
			check(t, f.CleanLegacyEnv())
			check(t, f.Repair(nil))
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.Pins.Current = shippedEntries()
			tt.run(t, f)
			for _, file := range []string{f.local, f.shared, f.user} {
				absent(t, file)
			}
		})
	}
}

// ending returns the bytes after the closing brace of a settings document.
func ending(doc string) string { return doc[strings.LastIndexByte(doc, '}')+1:] }

func TestExistingSettingsKeepTrailingBytes(t *testing.T) {
	tests := map[string]struct {
		before string
		// whole, when set, maps the bytes the same write gives a created file to the bytes expected here.
		whole func(created string) string
	}{
		"success: empty object without a final newline": {before: "{}", whole: func(created string) string { return strings.TrimSuffix(created, "\n") }},
		"success: empty object with a final newline":    {before: "{}\n", whole: func(created string) string { return created }},
		"success: empty object with a CRLF ending":      {before: "{}\r\n", whole: func(created string) string { return strings.ReplaceAll(created, "\n", "\r\n") }},
		"success: members without a final newline":      {before: `{"model":"x"}`},
		"success: one final newline":                    {before: "{\n  \"model\": \"x\"\n}\n"},
		"success: two final newlines":                   {before: "{\n  \"model\": \"x\"\n}\n\n"},
		"success: trailing spaces and a tab":            {before: "{ \"model\" : \"x\" } \t "},
		"success: CRLF without a final line ending":     {before: "{\r\n  \"model\": \"x\"\r\n}"},
		"success: CRLF with a final line ending":        {before: "{\r\n  \"model\": \"x\"\r\n}\r\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.Pins.Current = shippedEntries()
			// The unwire step below removes a pin only when its value is one the gateway wrote.
			f.Pins.Owned = map[string][]string{}
			for _, pin := range shippedEntries() {
				f.Pins.Owned[pin.Key] = []string{pin.Value}
			}
			put(t, f.local, tt.before)
			crlf := strings.Contains(tt.before, "\r\n")
			kept := func(t *testing.T) string {
				t.Helper()
				got := text(t, f.local)
				equal(t, ending(tt.before), ending(got))
				// Inserted lines follow the file's own line ending, so no other kind appears.
				if crlf {
					equal(t, strings.Count(got, "\r\n"), strings.Count(got, "\n"))
				} else {
					equal(t, 0, strings.Count(got, "\r"))
				}
				return got
			}

			check(t, f.WriteEnv("project", "default", false))
			wired := kept(t)
			equal(t, direct, envAt(t, f.local)[base])
			if tt.whole != nil {
				equal(t, tt.whole(explicitSettings(t, f)), wired)
			}

			r, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
			check(t, err)
			equal(t, []string{f.local}, r.Changed)
			kept(t)

			check(t, f.WriteEnv("project", "default", true))
			unwired := object(t, kept(t))
			_, hasEnv := unwired["env"]
			equal(t, false, hasEnv)
			equal(t, float64(500000), unwired["autoCompactWindow"])
		})
	}
}

func TestBlankSettingsStayUntouched(t *testing.T) {
	tests := map[string]struct{ before string }{
		"error: empty file":                {""},
		"error: spaces and line ends only": {" \r\n\t\n"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.local, tt.before)
			unreadable := func(t *testing.T, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "Could not read "+f.local) {
					t.Fatalf("expected a read error naming %q, got %v", f.local, err)
				}
				equal(t, tt.before, text(t, f.local))
			}

			r, err := f.SyncWindow([]string{f.local}, new(400000), new(500000))
			check(t, err)
			equal(t, 0, len(r.Changed))
			if len(r.Skipped) != 1 || r.Skipped[0].File != f.local || !strings.Contains(r.Skipped[0].Reason, "Could not read") {
				t.Fatalf("skipped: %+v", r.Skipped)
			}
			equal(t, tt.before, text(t, f.local))

			unreadable(t, f.WriteEnv("project", "default", false))

			// A migration copies into the local file before it edits the shared one, so neither changes.
			shared := `{"env":{"ANTHROPIC_BASE_URL":"` + direct + `"}}`
			put(t, f.shared, shared)
			migrated, err := f.MigrateLegacy()
			equal(t, false, migrated)
			unreadable(t, err)
			equal(t, shared, text(t, f.shared))
		})
	}
}

func TestCreationFollowsTheFileTheWriteReplaces(t *testing.T) {
	tests := map[string]struct {
		before    string
		exists    bool
		interfere func(*testing.T, fixture)
		want      func(created string) string
	}{
		// Another writer's empty object is an existing file: its bytes after the brace, none, are kept.
		"success: file appears while the write is prepared": {
			interfere: func(t *testing.T, f fixture) { put(t, f.local, "{}") },
			want:      func(created string) string { return strings.TrimSuffix(created, "\n") },
		},
		// A file removed under the write leaves nothing authored, so the write creates the file.
		"success: file disappears while the write is prepared": {
			before:    "{}",
			exists:    true,
			interfere: func(t *testing.T, f fixture) { check(t, os.Remove(f.local)) },
			want:      func(created string) string { return created },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.Pins.Current = shippedEntries()
			if tt.exists {
				put(t, f.local, tt.before)
			}
			edits := 0
			f.Replace = func(file string, edit func([]byte) ([]byte, error)) error {
				return atomicfile.ReplaceSettings(file, func(old []byte) ([]byte, error) {
					if file == f.local {
						if edits == 0 {
							tt.interfere(t, f)
						}
						edits++
					}
					return edit(old)
				})
			}
			check(t, f.WriteEnv("project", "default", false))
			// The replacement saw the file change and ran the edit again on the new state.
			equal(t, 2, edits)
			equal(t, tt.want(explicitSettings(t, f)), text(t, f.local))
		})
	}
}
