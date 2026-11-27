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

package readiness_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
)

// TestCatalogVertexNoStatusProcess checks that the live readiness resolver the catalog uses answers Vertex AI as
// config-invalid without starting `claude-code-proxy vertex auth status` when no project is set, and starts it exactly
// once when one is. A project is set by a CCP_VERTEX_PROJECT or GOOGLE_CLOUD_PROJECT value, or by vertex.project in the
// proxy's config.json, that is a string and not blank after ECMAScript trim; the last duplicate member wins and a
// malformed file is no config (lib/commands.js:1670-1686). The status runner is the resolver's only way to start the
// process, so a call to it is a launch.
func TestCatalogVertexNoStatusProcess(t *testing.T) {
	tests := map[string]struct {
		env    map[string]string
		config string
		// wantLaunch is whether the status process is started; its exit code 0 then makes the provider ready.
		wantLaunch bool
	}{
		"error: nothing configured":                     {},
		"error: blank environment projects":             {env: map[string]string{"CCP_VERTEX_PROJECT": "", "GOOGLE_CLOUD_PROJECT": " \t\n"}},
		"error: no-break space project":                 {env: map[string]string{"CCP_VERTEX_PROJECT": "\u00a0"}},
		"error: byte order mark project":                {env: map[string]string{"GOOGLE_CLOUD_PROJECT": "\ufeff\u3000"}},
		"error: config without a vertex section":        {config: `{"opencode":{"apiKey":"k"}}`},
		"error: config with a blank project":            {config: `{"vertex":{"project":"  "}}`},
		"error: config with an empty project":           {config: `{"vertex":{"project":""}}`},
		"error: config with a numeric project":          {config: `{"vertex":{"project":7}}`},
		"error: config with a null project":             {config: `{"vertex":{"project":null}}`},
		"error: config whose vertex is a string":        {config: `{"vertex":"project"}`},
		"error: malformed config":                       {config: `{"vertex":{"project":"p"}`},
		"error: config whose last duplicate is blank":   {config: `{"vertex":{"project":"p"},"vertex":{"project":" "}}`},
		"error: config whose last project is blank":     {config: `{"vertex":{"project":"p","project":"\u00a0"}}`},
		"success: CCP_VERTEX_PROJECT set":               {env: map[string]string{"CCP_VERTEX_PROJECT": "p"}, wantLaunch: true},
		"success: GOOGLE_CLOUD_PROJECT set with spaces": {env: map[string]string{"GOOGLE_CLOUD_PROJECT": " p "}, wantLaunch: true},
		"success: config project":                       {config: `{"vertex":{"project":"p"}}`, wantLaunch: true},
		"success: config whose last duplicate is set":   {config: `{"vertex":{"project":" "},"vertex":{"project":"p"}}`, wantLaunch: true},
		// ECMAScript trim does not remove NEXT LINE, so this project is set.
		"success: next line project": {env: map[string]string{"CCP_VERTEX_PROJECT": "\u0085"}, wantLaunch: true},
		"success: blank environment with a config project": {
			env: map[string]string{"CCP_VERTEX_PROJECT": " "}, config: `{"vertex":{"project":"p"}}`, wantLaunch: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			configDir := t.TempDir()
			if tt.config != "" {
				if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			proxyBinary := filepath.Join(t.TempDir(), "claude-code-proxy")
			type launch struct {
				Binary string
				Args   []string
			}
			var (
				mu       sync.Mutex
				launches []launch
			)
			source := catalog.NewLiveReadinessSource(catalog.LiveReadinessOptions{
				Lookup:      lookupMap(tt.env),
				ConfigDir:   configDir,
				ProxyBinary: proxyBinary,
				CommandPath: filepath.Join(t.TempDir(), "model-gateway"),
				RunStatus: func(_ context.Context, binary string, args []string) (int, error) {
					mu.Lock()
					defer mu.Unlock()
					launches = append(launches, launch{binary, slices.Clone(args)})
					return 0, nil
				},
			})
			got := source.Probe(t.Context(), "vertex")

			var wantLaunches []launch
			wantState := "config-invalid"
			if tt.wantLaunch {
				wantLaunches = []launch{{proxyBinary, []string{"vertex", "auth", "status"}}}
				wantState = "ready"
			}
			mu.Lock()
			defer mu.Unlock()
			if diff := gocmp.Diff(wantLaunches, launches); diff != "" {
				t.Errorf("status process launches (-want +got):\n%s", diff)
			}
			if got.State != wantState || got.Ready != tt.wantLaunch {
				t.Errorf("Probe(vertex) = state %q ready %v, want state %q ready %v; message: %s", got.State, got.Ready, wantState, tt.wantLaunch, got.Message)
			}
		})
	}
}
