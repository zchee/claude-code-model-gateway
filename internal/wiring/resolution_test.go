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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/wiring"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

const (
	base        = "ANTHROPIC_BASE_URL"
	direct      = "http://127.0.0.1:49123"
	compat      = "http://api.anthropic.com"
	discovery   = "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"
	opus        = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	sonnet      = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	fable       = "ANTHROPIC_DEFAULT_FABLE_MODEL"
	option      = "ANTHROPIC_CUSTOM_MODEL_OPTION"
	optionName  = "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"
	description = "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION"
)

type fixture struct {
	wiring.Store
	env                                              map[string]string
	home, local, shared, user, registry, bin, legacy string
}

func sandbox(t *testing.T) fixture {
	t.Helper()
	home, project := t.TempDir(), t.TempDir()
	env := map[string]string{"CODEX_GATEWAY_PORT": "49123"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	gateway := filepath.Join(home, ".claude", "model-gateway")
	return fixture{Paths: paths.New(lookup, func() (string, error) { return home, nil }, func() (string, error) { return project, nil }), Project: project, Lookup: lookup, env: env, home: home, local: filepath.Join(project, ".claude", "settings.local.json"), shared: filepath.Join(project, ".claude", "settings.json"), user: filepath.Join(home, ".claude", "settings.json"), registry: filepath.Join(gateway, "wired-projects.json"), bin: filepath.Join(gateway, "bin", "model-gateway"), legacy: filepath.Join(gateway, "model-gateway.js")}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equal(t *testing.T, want, got any) {
	t.Helper()
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func put(t *testing.T, path, text string) {
	t.Helper()
	check(t, os.MkdirAll(filepath.Dir(path), 0o700))
	check(t, os.WriteFile(path, []byte(text), 0o600))
}

func encoded(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	check(t, err)
	return string(b)
}

func text(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	check(t, err)
	return string(b)
}

func object(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	check(t, json.Unmarshal([]byte(raw), &m))
	return m
}

func envAt(t *testing.T, path string) map[string]any {
	t.Helper()
	v := object(t, text(t, path))["env"]
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("env is %T", v)
	}
	return m
}

func putEnv(t *testing.T, path string, env map[string]any) {
	t.Helper()
	put(t, path, encoded(t, map[string]any{"env": env}))
}

func absent(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected absent %q, got %v", path, err)
	}
}

func TestEffectiveBaseURL(t *testing.T) {
	tests := map[string]struct{ level int }{
		"success: environment":               {0},
		"success: local":                     {1},
		"success: shared":                    {2},
		"success: user":                      {3},
		"success: malformed and missing key": {4},
		"success: empty environment value":   {5},
		"success: no definitions":            {6},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			defs := []wiring.Definition{{Source: "env", Value: "http://env.example"}, {Source: "project-local", File: f.local, Value: "http://local.example"}, {Source: "project-shared", File: f.shared, Value: "http://shared.example"}, {Source: "user", File: f.user, Value: "http://user.example"}}
			want := wiring.Resolution{Shadowed: []wiring.Definition{}}
			switch tt.level {
			case 4:
				put(t, f.local, "{")
				putEnv(t, f.shared, map[string]any{"OTHER_VALUE": "present"})
				putEnv(t, f.user, map[string]any{base: defs[3].Value})
				want.Winner = &defs[3]
			case 5:
				f.env[base] = ""
				putEnv(t, f.local, map[string]any{base: defs[1].Value})
				want.Winner = &wiring.Definition{Source: "env"}
				want.Shadowed = defs[1:2]
			case 6:
			default:
				for _, d := range defs[tt.level:] {
					if d.File == "" {
						f.env[base] = d.Value
					} else {
						putEnv(t, d.File, map[string]any{base: d.Value})
					}
				}
				want.Winner = &defs[tt.level]
				want.Shadowed = defs[tt.level+1:]
			}
			got, err := f.EffectiveBaseURL()
			check(t, err)
			equal(t, want, got)
		})
	}
}

func TestUnsafeProcessOverride(t *testing.T) {
	tests := map[string]struct {
		values               []string
		settingsOnly, unsafe bool
	}{
		"success: HTTPS process overrides": {values: []string{"https://api.anthropic.com", "HTTPS://API.ANTHROPIC.COM:443/", "  HTTPS://API.ANTHROPIC.COM:443/  "}, unsafe: true},
		"success: other endpoints":         {values: []string{compat, "http://127.0.0.1:9", "https://api.anthropic.com:444", "https://not-api.anthropic.com", "not a URL"}},
		"success: settings only HTTPS":     {values: []string{"https://api.anthropic.com"}, settingsOnly: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, v := range tt.values {
				f := sandbox(t)
				putEnv(t, f.user, map[string]any{base: "https://api.anthropic.com"})
				if !tt.settingsOnly {
					f.env[base] = v
				}
				r, err := f.EffectiveBaseURL()
				check(t, err)
				var want *wiring.Resolution
				if tt.unsafe {
					want = &wiring.Resolution{
						Winner:   &wiring.Definition{Source: "env", Value: v},
						Shadowed: []wiring.Definition{{Source: "user", File: f.user, Value: "https://api.anthropic.com"}},
					}
				}
				equal(t, want, r.UnsafeProcessOverride())
			}
		})
	}
}

func TestBypassedWiring(t *testing.T) {
	tests := map[string]struct {
		process, local string
		bypass         bool
	}{
		"success: foreign process shadows wired file": {"https://example.invalid", direct, true},
		"success: wired process shadows wired file":   {compat, direct, false},
		"success: foreign file shadows wired file":    {"", "https://example.invalid", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			putEnv(t, f.local, map[string]any{base: tt.local})
			putEnv(t, f.user, map[string]any{base: compat})
			if tt.process != "" {
				f.env[base] = tt.process
			}
			r, err := f.EffectiveBaseURL()
			check(t, err)
			var want *wiring.Definition
			if tt.bypass {
				want = &wiring.Definition{Source: "project-local", File: f.local, Value: direct}
			}
			equal(t, want, f.BypassedWiring(r))
		})
	}
}

func TestWiredMode(t *testing.T) {
	tests := map[string]struct {
		env, local, user    string
		wired               bool
		scope, mode, source string
	}{
		"success: unwired":                    {},
		"success: environment only":           {env: direct, wired: true, mode: "default", source: "env"},
		"success: file defines writable mode": {env: direct, local: compat, wired: true, scope: "project", mode: "compat", source: "project-local"},
		"success: user legacy compat":         {user: compat, wired: true, scope: "user", mode: "compat", source: "user"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			f := sandbox(t)
			if tt.env != "" {
				f.env[base] = tt.env
			}
			var file string
			switch {
			case tt.local != "":
				file = f.local
				putEnv(t, file, map[string]any{base: tt.local})
			case tt.user != "":
				file = f.user
				putEnv(t, file, map[string]any{base: tt.user})
			}
			is, err := f.IsWired()
			check(t, err)
			equal(t, tt.wired, is)
			var want *wiring.Wiring
			if tt.wired {
				want = &wiring.Wiring{Scope: tt.scope, Mode: tt.mode, Source: tt.source, File: file}
			}
			got, err := f.WiredMode()
			check(t, err)
			equal(t, want, got)
		})
	}
}
