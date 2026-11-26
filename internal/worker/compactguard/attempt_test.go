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

package compactguard

import (
	"math"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

func TestClassify(t *testing.T) {
	tests := map[string]struct {
		attempt        sse.Attempt
		status         int
		override, want string
	}{
		"success: visible terminal survives abort":       {sse.Attempt{Terminal: true, HasVisibleText: true}, 200, "upstream_aborted", "completed"},
		"success: no visible summary":                    {sse.Attempt{Terminal: true}, 200, "", "empty_summary"},
		"error: HTTP status precedes deliverability":     {sse.Attempt{Terminal: true, HasVisibleText: true}, 503, "", "upstream_error"},
		"error: unfinished abort precedes unknown event": {sse.Attempt{SawError: true, ErrorCode: "unknown_error"}, 200, "upstream_aborted", "aborted"},
		"error: unknown event":                           {sse.Attempt{SawError: true, ErrorCode: "unknown_error"}, 200, "", "unknown_error"},
		"error: incomplete generation":                   {sse.Attempt{}, 200, "upstream_error", "incomplete"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := classify(&tt.attempt, tt.status, tt.override)
			if diff := gocmp.Diff(tt.want, got.Outcome); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestUsageSnapshotMergeAndDiagnostics(t *testing.T) {
	previous := outcome.Compaction{InputTokens: new(17.0), CacheReadTokens: new(3.0)}
	tests := map[string]struct {
		snapshot map[string]any
		want     outcome.Compaction
	}{
		"success: zero replaces prior count but absent values do not": {map[string]any{"input_tokens": 0.0, "cache_read_tokens": nil, "output_tokens": 12.5, "cache_creation_tokens": 4.0, "private": "omitted"}, outcome.Compaction{InputTokens: new(0.0), CacheReadTokens: new(3.0), OutputTokens: new(12.5), CacheCreationTokens: new(4.0)}},
		"error: invalid counts add no diagnostic value":               {map[string]any{"input_tokens": math.NaN(), "output_tokens": math.Inf(-1), "cache_read_tokens": -1.0, "cache_creation_tokens": math.Inf(1)}, previous},
		"error: strings and booleans are not numeric counts":          {map[string]any{"output_tokens": "6", "cache_creation_tokens": true}, previous},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, mergeUsage(previous, tt.snapshot)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
