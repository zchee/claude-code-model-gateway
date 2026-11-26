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
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestRecoveryPolicy(t *testing.T) {
	const id = "model-gateway@my-marketplace"
	const loopback = "http://127.0.0.1:49123"
	const compat = "http://api.anthropic.com"
	tests := map[string]struct {
		env      map[string]string
		settings []readiness.Settings
		want     string
	}{
		"success: unwired before disabled":           {settings: []readiness.Settings{{EnabledPlugins: map[string]any{id: false}}}, want: "unwired"},
		"success: empty environment overrides file":  {map[string]string{"ANTHROPIC_BASE_URL": ""}, []readiness.Settings{{BaseURL: new(compat)}}, "unwired"},
		"success: local URL wins":                    {settings: []readiness.Settings{{BaseURL: new("https://example.invalid")}, {BaseURL: new(compat)}}, want: "unwired"},
		"success: env route and lower file disables": {map[string]string{"ANTHROPIC_BASE_URL": loopback, "CODEX_GATEWAY_PORT": "49123"}, []readiness.Settings{{EnabledPlugins: map[string]any{id: "false"}}, {EnabledPlugins: map[string]any{id: false}}}, "disabled"},
		"success: explicit enable wins":              {settings: []readiness.Settings{{EnabledPlugins: map[string]any{id: true}}, {BaseURL: new(compat), EnabledPlugins: map[string]any{id: false}}}, want: "legacy"},
		"success: unrelated plugin ignored":          {settings: []readiness.Settings{{BaseURL: new(compat), EnabledPlugins: map[string]any{"model-gateway@eigenwise-toolshed": false}}}, want: "legacy"},
		"success: default port":                      {settings: []readiness.Settings{{BaseURL: new("http://127.0.0.1:18764")}}, want: "recover"},
		"success: configured port excludes default":  {env: map[string]string{"CODEX_GATEWAY_PORT": "49123"}, settings: []readiness.Settings{{BaseURL: new("http://127.0.0.1:18764")}}, want: "unwired"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			lookup := func(key string) (string, bool) { v, ok := tt.env[key]; return v, ok }
			if diff := gocmp.Diff(tt.want, readiness.Policy(lookup, tt.settings, id)); diff != "" {
				t.Fatalf("policy (-want +got): %s", diff)
			}
		})
	}
}
