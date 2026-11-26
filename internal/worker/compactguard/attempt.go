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

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

// Outcome labels of a compaction attempt sequence.
const (
	outcomeCompleted     = "completed"
	outcomeEmptySummary  = "empty_summary"
	outcomeAborted       = "aborted"
	outcomeIncomplete    = "incomplete"
	outcomeUpstreamError = "upstream_error"
	outcomeUnknownError  = "unknown_error"
)

// classify labels the final attempt. A non-2xx status outranks the stream's own state, a
// deliverable stream outranks an interrupted connection, and an interrupted connection outranks
// an unrecognized upstream error event.
func classify(a *sse.Attempt, status int, override string) outcome.Compaction {
	c := outcome.Compaction{
		TerminalCode:   a.TerminalCode,
		ErrorCode:      a.ErrorCode,
		UpstreamStatus: new(status),
	}
	switch {
	case status < 200 || status >= 300:
		c.Outcome = outcomeUpstreamError
	case a.Deliverable() && a.HasVisibleText:
		c.Outcome = outcomeCompleted
	case a.Deliverable():
		c.Outcome = outcomeEmptySummary
	case override == "upstream_aborted":
		c.Outcome = outcomeAborted
	case a.ErrorCode == outcomeUnknownError:
		c.Outcome = outcomeUnknownError
	default:
		c.Outcome = outcomeIncomplete
	}
	return c
}

// completed reports whether an outcome label delivers a usable summary.
func completed(label string) bool {
	return label == outcomeCompleted || label == outcomeEmptySummary
}

// mergeUsage overlays the four compaction token counts of a usage snapshot onto previous. A
// count replaces the previous one only when the snapshot holds a finite, non-negative number;
// zero is a measurement and replaces, an absent or unusable value keeps what previous had.
func mergeUsage(previous outcome.Compaction, snapshot map[string]any) outcome.Compaction {
	merged := previous
	for _, f := range []struct {
		key string
		dst **float64
	}{
		{"input_tokens", &merged.InputTokens},
		{"output_tokens", &merged.OutputTokens},
		{"cache_read_tokens", &merged.CacheReadTokens},
		{"cache_creation_tokens", &merged.CacheCreationTokens},
	} {
		if n, ok := snapshot[f.key].(float64); ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			*f.dst = new(n)
		}
	}
	return merged
}

// withUsage returns c with the token counts of usage, leaving its labels untouched.
func withUsage(c, usage outcome.Compaction) outcome.Compaction {
	c.InputTokens, c.OutputTokens = usage.InputTokens, usage.OutputTokens
	c.CacheReadTokens, c.CacheCreationTokens = usage.CacheReadTokens, usage.CacheCreationTokens
	return c
}
