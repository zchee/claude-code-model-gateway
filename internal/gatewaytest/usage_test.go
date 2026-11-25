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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

const fixtureUsageKey = "CODEX_GATEWAY_USAGE_ENDPOINT"

func TestUsageEnvironment(t *testing.T) {
	tests := map[string]struct{ key string }{
		"success: generic OTLP endpoint removed": {"OTEL_EXPORTER_OTLP_ENDPOINT"},
		"success: logs endpoint removed":         {"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"},
		"success: traces endpoint removed":       {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"},
		"success: lower-case endpoint removed":   {"otel_exporter_otlp_endpoint"},
		"success: mixed-case setting removed":    {"Otel_Exporter_OTLP_Logs_Timeout"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			parent := map[string]string{tt.key: "inherited", fixtureUsageKey: "inherited"}
			env := gatewaytest.NewEnv(t.TempDir(), parent, parent, nil)
			if diff := gocmp.Diff("0", env[fixtureUsageKey]); diff != "" {
				t.Errorf("usage endpoint (-want +got):\n%s", diff)
			}
			if _, ok := env[tt.key]; ok {
				t.Errorf("inherited setting %q survived", tt.key)
			}
		})
	}
}
