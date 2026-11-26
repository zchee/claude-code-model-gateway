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

package policy_test

import (
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

func TestCheckedWindowNote(t *testing.T) {
	const id = "grok-composer-2.5-fast"
	r := listedResolver(proxymodels.Route{ID: id, Provider: "grok"})
	equal(t, int64(200000), r.AdvertisedWindow(id))
	tests := map[string]struct {
		model *policy.Model
		want  string
	}{
		"error: direct maximum for listed unmeasured Grok": {r.Resolve(id), "cannot format a direct compaction maximum without a synthetic sentry"},
		"error: invalid backend window":                    {&policy.Model{BackendID: "broken", Sentry: "synthetic-413", BackendWindow: 40000}, "model-gateway: invalid sentry backend window for broken"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			note, err := policy.CheckedWindowNote("grok", 0, 242000, tt.model, nil)
			if err == nil {
				t.Fatalf("CheckedWindowNote returned %q, want error %q", note, tt.want)
			}
			equal(t, tt.want, err.Error())
			equal(t, "", note)
			equal(t, "", policy.WindowNote("grok", 0, 242000, tt.model, nil))
		})
	}
}

func TestCompactTriggerValidatedLimits(t *testing.T) {
	tests := map[string]struct{ cap, direct, want int64 }{
		"success: largest direct maximum":                {0, 1<<53 - 1, 1<<53 - 1},
		"success: direct maximum clipped at largest cap": {1000000, 1<<53 - 1, 1000000},
		"success: reserve below largest cap":             {1000000, 0, 915000},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { equal(t, tt.want, policy.CompactTrigger(tt.cap, tt.direct)) })
	}
}

func TestAliasReplacementWithoutCaptureGroup(t *testing.T) {
	equal(t, "claude-gpt-$1[1m]", policy.Resolver{}.ClientID("gpt-$1"))
}
