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

package gatewaytest_test

import (
	"maps"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const fixtureUsageKey = "CODEX_GATEWAY_USAGE_ENDPOINT"

func TestUsageEnvironment(t *testing.T) {
	tests := map[string]struct {
		parent, normal, isolated map[string]string
	}{
		"success: remove inherited generic endpoint": {parent: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "inherited"}},
		"success: remove different ordinary override": {
			parent: map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "inherited"},
			normal: map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "override"},
		},
		"success: remove mixed-case isolated override": {isolated: map[string]string{"Otel_Exporter_OTLP_Logs_Timeout": "override"}},
		"success: remove inherited value copied into isolated override": {
			parent:   map[string]string{"otel_exporter_otlp_traces_endpoint": "inherited"},
			isolated: map[string]string{"otel_exporter_otlp_traces_endpoint": "inherited"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			parent := map[string]string{fixtureUsageKey: "inherited"}
			maps.Copy(parent, tt.parent)
			home := t.TempDir()
			want := gatewaytest.NewEnv(home, nil, nil, nil)
			env := gatewaytest.NewEnv(home, parent, tt.normal, tt.isolated)
			if diff := gocmp.Diff(want, env); diff != "" {
				t.Errorf("fixture environment (-want +got):\n%s", diff)
			}
			if env[fixtureUsageKey] != "0" {
				t.Errorf("usage endpoint=%q, want 0", env[fixtureUsageKey])
			}
		})
	}
}
