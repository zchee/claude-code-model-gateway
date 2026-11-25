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

package sentry_test

import (
	"encoding/json/jsontext"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestOverflowLearning(t *testing.T) {
	tests := map[string]struct {
		source               string
		learn                bool
		last, probe, trigger float64
		crosses              bool
	}{
		"success: derived estimate":                   {"derived", true, 80, 71, 70, true},
		"success: exact limit does not teach":         {"derived", false, 80, 71, 100, false},
		"success: never learn a cap":                  {"cap", true, 80, 71, 100, false},
		"success: never learn an explicit setting":    {"compact-at", true, 80, 71, 100, false},
		"success: never learn an environment trigger": {"env", true, 80, 71, 100, false},
		"success: strict headroom boundary":           {"derived", true, 10, 50, 100, false},
		"success: learned trigger at least one":       {"derived", true, 10.25, 1.25, 1, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentry.State
			m := model()
			m.Policy.Source = tt.source
			observe(&s, "first", m, tt.last, false)
			if got := s.Overflow("first", m, tt.learn); got != tt.last {
				t.Errorf("overflow usage=%v, want %v", got, tt.last)
			}
			observe(&s, "first", m, 200, false)
			check(t, &s, "first", m, first, sentry.Crossing{})
			observe(&s, "other", m, tt.probe, false)
			want := sentry.Crossing{}
			if tt.crosses {
				want = sentry.Crossing{Usage: tt.probe, Trigger: tt.trigger}
			}
			check(t, &s, "other", m, first, want)
		})
	}
}

func TestCeilingLifetime(t *testing.T) {
	var s sentry.State
	m := model()
	if got := s.Overflow("empty", m, true); got != 0 {
		t.Errorf("initial usage=%v", got)
	}
	observe(&s, "empty", m, 120, false)
	check(t, &s, "empty", m, first, sentry.Crossing{})
	for _, u := range []float64{80, 60, 90} {
		observe(&s, "learner", m, u, false)
		s.Overflow("learner", m, true)
	}
	observe(&s, "reader", m, 55, false)
	check(t, &s, "reader", m, first, sentry.Crossing{Usage: 55, Trigger: 50})
	other := m
	other.Key = "another"
	observe(&s, "reader", other, 55, false)
	check(t, &s, "reader", other, first, sentry.Crossing{})
	m.Policy = &policy.Sentry{BackendWindow: 210, CompactTrigger: 200, Source: "derived"}
	observe(&s, "later", m, 120, false)
	check(t, &s, "later", m, first, sentry.Crossing{})
	observe(&s, "later", m, 201, false)
	check(t, &s, "later", m, first, sentry.Crossing{Usage: 201, Trigger: 200})
}

func TestLargeWatermark(t *testing.T) {
	var s sentry.State
	m := sentry.Target{Key: "g", Policy: &policy.Sentry{BackendWindow: 240000, CompactTrigger: 200000, Source: "derived"}, Headroom: 40000}
	observe(&s, "s", m, 200001, false)
	check(t, &s, "s", m, first, sentry.Crossing{Usage: 200001, Trigger: 200000})
	observe(&s, "s", m, 160000, false)
	observe(&s, "s", m, 200001, false)
	check(t, &s, "s", m, first, sentry.Crossing{})
	observe(&s, "s", m, 159999, false)
	observe(&s, "s", m, 200001, false)
	check(t, &s, "s", m, first, sentry.Crossing{Usage: 200001, Trigger: 200000})
}

func TestParseOverflow(t *testing.T) {
	tests := map[string]struct {
		status        int
		body          string
		recognized    bool
		tokens, limit *float64
	}{
		"success: measured genuine":                     {413, `{"error":{"type":"request_too_large","message":"overflow (250000 TOKENS > 242000 tokens)"}}`, true, new(250000.0), new(242000.0)},
		"success: unmeasured genuine":                   {413, `{"error":{"type":"request_too_large","message":"prompt too long"}}`, true, nil, nil},
		"success: genuine counts use Number":            {413, `{"error":{"type":"request_too_large","message":"9007199254740993 tokens > 0 tokens"}}`, true, new(9007199254740992.0), new(0.0)},
		"error: genuine requires string message":        {413, `{"error":{"type":"request_too_large","message":null}}`, false, nil, nil},
		"error: genuine not legacy":                     {413, `{"error":{"type":"api_error","message":"too many tokens"}}`, false, nil, nil},
		"success: engine counts":                        {400, `{"error":{"type":"exceed_context_size_error","n_prompt_tokens":33001,"n_ctx":32768}}`, true, new(33001.0), new(32768.0)},
		"success: embedded engine":                      {500, `{"error":{"type":"api_error","message":"400: {\"error\":{\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":12,\"n_ctx\":10}}"}}`, true, new(12.0), new(10.0)},
		"success: wording without valid embedded JSON":  {400, `{"error":{"type":"other","message":"exceed_context_size_error { request (19 tokens) exceeds the available context size (10 tokens)"}}`, true, new(19.0), new(10.0)},
		"success: engine type is sufficient":            {400, `{"error":{"type":"exceed_context_size_error","n_prompt_tokens":"123","n_ctx":9007199254740992}}`, true, nil, nil},
		"success: validate engine fields independently": {400, `{"error":{"type":"exceed_context_size_error","n_prompt_tokens":0,"n_ctx":10}}`, true, nil, new(10.0)},
		"success: marker without counts":                {400, `{"error":{"message":"exceed_context_size_error"}}`, true, nil, nil},
		"success: generic phrase":                       {502, "TOO MANY TOKENS", true, nil, nil},
		"error: worded counts without marker":           {400, `{"error":{"message":"request (19 tokens) exceeds the available context size (10 tokens)"}}`, false, nil, nil},
		"error: unrelated upstream failure":             {503, "restart", false, nil, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := sentry.ParseOverflow(tt.status, []byte(tt.body))
			if diff := gocmp.Diff(sentry.Counts{Tokens: tt.tokens, Limit: tt.limit}, got); diff != "" || ok != tt.recognized {
				t.Errorf("recognized=%t, want %t (-want +got): %s", ok, tt.recognized, diff)
			}
		})
	}
}

func TestMergeUsage(t *testing.T) {
	tests := map[string]struct{ previous, event, want string }{
		"success: start reads nested usage":                 {`{}`, `{"type":"message_start","message":{"usage":{"input_tokens":88}},"usage":{"output_tokens":99}}`, `{"input_tokens":88}`},
		"success: delta preserves omitted inputs":           {`{"input_tokens":88,"output_tokens":1}`, `{"type":"message_delta","usage":{"output_tokens":2}}`, `{"input_tokens":88,"output_tokens":2}`},
		"success: later nested value replaces entire field": {`{"nested":{"a":1,"b":2},"persist":3}`, `{"type":"other","usage":{"nested":{"a":4},"count":null}}`, `{"nested":{"a":4},"persist":3,"count":null}`},
		"success: ignore scalar usage":                      {`{"input_tokens":88}`, `{"usage":12}`, `{"input_tokens":88}`},
		"success: ignore missing usage":                     {`{"input_tokens":88}`, `{"type":"message_stop"}`, `{"input_tokens":88}`},
		"success: null previous":                            {`null`, `{"usage":{"output_tokens":7}}`, `{"output_tokens":7}`},
		"success: array usage spreads its indexes":          {`{"keep":1}`, `{"usage":[4,5]}`, `{"keep":1,"0":4,"1":5}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			want := jsontext.Value(tt.want)
			got := sentry.MergeUsage(jsontext.Value(tt.previous), jsontext.Value(tt.event))
			if err := got.Canonicalize(); err != nil {
				t.Fatalf("invalid merged usage %q: %v", got, err)
			}
			if err := want.Canonicalize(); err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(string(want), string(got)); diff != "" {
				t.Error(diff)
			}
		})
	}
}
