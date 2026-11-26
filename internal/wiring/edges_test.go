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

func TestUnsafeProcessOverrideSpellings(t *testing.T) {
	tests := map[string]struct {
		value  string
		unsafe bool
	}{
		"success: no slashes after the scheme":   {"https:api.anthropic.com", true},
		"success: credentials and a path":        {"https://user:secret@api.anthropic.com/v1/messages", true},
		"success: percent-encoded host":          {"https://%61pi.anthropic.com", true},
		"success: surrounding controls":          {"\thttps://api.anthropic.com/\n", true},
		"success: default port with zeros":       {"https://api.anthropic.com:0443", true},
		"success: backslash separators":          {`https:\\api.anthropic.com\v1`, true},
		"success: port zero is not the default":  {"https://api.anthropic.com:0", false},
		"success: trailing dot is another host":  {"https://api.anthropic.com.", false},
		"success: malformed port":                {"https://api.anthropic.com:443x", false},
		"success: IPv6 literal":                  {"https://[::1]:443", false},
		"success: other scheme with HTTPS words": {"httpsx://api.anthropic.com", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			f.env[base] = tt.value
			r, err := f.EffectiveBaseURL()
			check(t, err)
			equal(t, tt.unsafe, r.UnsafeProcessOverride() != nil)
		})
	}
}

func TestReadinessHookShapes(t *testing.T) {
	tests := map[string]struct {
		input, want string
		enabled     bool
		fails       bool
	}{
		"error: event is an object":            {input: `{"hooks":{"SessionStart":{}}}`, enabled: true, fails: true},
		"error: null group":                    {input: `{"hooks":{"UserPromptSubmit":[null]}}`, fails: true},
		"error: settings are not an object":    {input: `5`, enabled: true, fails: true},
		"success: string hooks spread indexes": {input: `{"hooks":"ab"}`, want: `{"hooks":{"0":"a","1":"b"}}`},
		"success: falsy event removed":         {input: `{"hooks":{"SessionStart":null,"Stop":[]}}`, want: `{"hooks":{"Stop":[]}}`},
		"success: number hooks removed":        {input: `{"hooks":7,"model":"x"}`, want: `{"model":"x"}`},
		"success: groups without hook lists":   {input: `{"hooks":{"SessionStart":[5,"x",[],{"hooks":null}]}}`, want: `{"hooks":{"SessionStart":[5,"x",[],{"hooks":null}]}}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			got, err := f.SyncReadinessHooks([]byte(tt.input), tt.enabled)
			if tt.fails {
				if err == nil {
					t.Fatalf("expected an error, got %s", got)
				}
				return
			}
			check(t, err)
			equal(t, object(t, tt.want), object(t, string(got)))
		})
	}
}

func TestReadinessHookPresentShapes(t *testing.T) {
	f := sandbox(t)
	owned := `{"hooks":[{"type":"command","command":` + encoded(t, f.bin) + `,"args":["readiness-gate"]}]}`
	tests := map[string]struct {
		input string
		want  bool
	}{
		"success: both events":           {`{"hooks":{"SessionStart":[` + owned + `],"UserPromptSubmit":[{},` + owned + `]}}`, true},
		"success: event is a string":     {`{"hooks":{"SessionStart":"x","UserPromptSubmit":[` + owned + `]}}`, false},
		"success: null group stops scan": {`{"hooks":{"SessionStart":[null,` + owned + `],"UserPromptSubmit":[` + owned + `]}}`, false},
		"success: not JSON":              {`{`, false},
		"success: hooks is an array":     {`{"hooks":[` + owned + `]}`, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			equal(t, tt.want, f.ReadinessHookPresent([]byte(tt.input)))
		})
	}
}

func TestIllFormedBaseURLKeepsPrecedence(t *testing.T) {
	f := sandbox(t)
	// The escape stays in the file: an unpaired surrogate the decoder must neither drop nor replace.
	put(t, f.local, "{\"env\":{\"ANTHROPIC_BASE_URL\":\"\\ud800\"}}")
	putEnv(t, f.user, map[string]any{base: direct})
	r, err := f.EffectiveBaseURL()
	check(t, err)
	equal(t, "project-local", r.Winner.Source)
	equal(t, 1, len(r.Shadowed))
	wired, err := f.IsWired()
	check(t, err)
	equal(t, false, wired)
	mode, err := f.WiredMode()
	check(t, err)
	equal(t, &wiring.Wiring{Scope: "user", Mode: "default", Source: "user", File: f.user}, mode)
}

func TestRegistryEntrySpelling(t *testing.T) {
	f := sandbox(t)
	putEnv(t, f.local, map[string]any{base: direct})
	spelled := f.Project + "/"
	put(t, f.registry, "{\"projects\":["+encoded(t, spelled)+",\"/\\ud800\"]}")
	got, pruned, err := f.Projects(false)
	check(t, err)
	equal(t, 0, len(pruned))
	equal(t, []wiring.Registration{{Project: spelled, File: f.local, Value: direct}}, got)
	equal(t, "{\n  \"projects\": [\n    "+encoded(t, spelled)+"\n  ]\n}\n", text(t, f.registry))
}

func TestNonObjectEnvWrites(t *testing.T) {
	tests := map[string]struct {
		input string
		fails bool
	}{
		"error: string env":         {`{"env":"x"}`, true},
		"error: array env":          {`{"env":["x"]}`, true},
		"success: null env":         {`{"env":null}`, false},
		"success: false env":        {`{"env":false}`, false},
		"success: missing env":      {`{"model":"x"}`, false},
		"success: empty object env": {`{"env":{}}`, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			put(t, f.local, tt.input)
			err := f.WriteEnv("project", "default", false)
			if tt.fails {
				if err == nil {
					t.Fatal("expected the write to fail")
				}
				equal(t, tt.input, text(t, f.local))
				return
			}
			check(t, err)
			equal(t, direct, envAt(t, f.local)[base])
			check(t, f.WriteEnv("project", "default", true))
			_, present := object(t, text(t, f.local))["env"]
			equal(t, false, present)
		})
	}
}

func TestRetireModeConfigKeepsDirectory(t *testing.T) {
	f := sandbox(t)
	dir := filepath.Join(filepath.Dir(f.registry), "wiring.json")
	check(t, os.MkdirAll(dir, 0o700))
	equal(t, false, f.RetireModeConfig())
	info, err := os.Stat(dir)
	check(t, err)
	equal(t, true, info.IsDir())
}
