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

package outcome_test

import (
	"os"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestOutcomePresence(t *testing.T) {
	var result outcome.Result
	if result.Contacted || result.Cancelled || result.Status != 0 || result.StatusOverride != "" || result.Audit != nil || result.Route != (outcome.Route{}) {
		t.Errorf("unexpected terminal facts in zero result: %+v", result)
	}
	var overflow outcome.Overflow
	if overflow.TokensFrom != "" || overflow.RequestBytes != 0 {
		t.Errorf("zero overflow has a token source or request size: %+v", overflow)
	}
	var compact outcome.Compaction
	var audit outcome.Audit
	if compact.UpstreamStatus != nil || audit.MarkersLength != nil || audit.Model != nil {
		t.Errorf("zero values supplied optional metadata: compaction=%+v audit=%+v", compact, audit)
	}
	tests := map[string]struct {
		compact  outcome.Compaction
		overflow outcome.Overflow
		want     [6]*float64
	}{
		"success: unavailable counts are absent": {},
		"success: measured zero counts are present": {
			compact:  outcome.Compaction{InputTokens: new(0.0), OutputTokens: new(0.0), CacheReadTokens: new(0.0), CacheCreationTokens: new(0.0)},
			overflow: outcome.Overflow{Tokens: new(0.0), LimitTokens: new(0.0)},
			want:     [6]*float64{new(0.0), new(0.0), new(0.0), new(0.0), new(0.0), new(0.0)},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			route := outcome.Route{Compaction: &tt.compact, Overflow: &tt.overflow}
			got := [6]*float64{route.Compaction.InputTokens, route.Compaction.OutputTokens, route.Compaction.CacheReadTokens, route.Compaction.CacheCreationTokens, route.Overflow.Tokens, route.Overflow.LimitTokens}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("route token counts (-want +got):\n%s", diff)
			}
		})
	}
}
