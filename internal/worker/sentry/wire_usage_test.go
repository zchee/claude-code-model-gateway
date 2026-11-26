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

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestWireUsageParity(t *testing.T) {
	const previous = `{"input_tokens":3,"output_tokens":2}`
	tests := map[string]struct {
		event, merged string
		observed      float64
		completed     bool
		crosses       bool
	}{
		"success: message start uses nested usage only for merging": {`{"type":"message_start","usage":{"input_tokens":5},"message":{"usage":{"input_tokens":123}}}`, `{"input_tokens":123,"output_tokens":2}`, 109, true, false},
		"success: partial delta replaces without rearming":          {`{"type":"message_delta","usage":{"input_tokens":121}}`, `{"input_tokens":121,"output_tokens":2}`, 121, false, false},
		"success: completed delta includes both cache counts":       {`{"type":"message_delta","usage":{"input_tokens":80,"cache_read_input_tokens":15,"cache_creation_input_tokens":12,"output_tokens":9}}`, `{"input_tokens":80,"output_tokens":9,"cache_read_input_tokens":15,"cache_creation_input_tokens":12}`, 107, true, true},
		"success: missing usage preserves previous values":          {`{"type":"message_delta"}`, previous, 109, true, false},
		"success: scalar usage preserves previous values":           {`{"type":"message_delta","usage":13}`, previous, 109, true, false},
		"success: array usage spreads indexed properties":           {`{"type":"message_delta","usage":[7,8]}`, `{"0":7,"1":8,"input_tokens":3,"output_tokens":2}`, 109, true, false},
		"success: number above exact integer range rounds":          {`{"type":"message_delta","usage":{"input_tokens":9007199254740993}}`, `{"input_tokens":9007199254740992,"output_tokens":2}`, 9007199254740992, true, true},
		"success: duplicate fields retain last value":               {`{"type":"message_delta","usage":{"input_tokens":6},"usage":{"input_tokens":5,"output_tokens":4,"input_tokens":119}}`, `{"input_tokens":119,"output_tokens":4}`, 119, true, true},
		"success: null event is a no-op":                            {`null`, previous, 109, true, false},
		"success: absent event is a no-op":                          {"", previous, 109, true, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			decode := func(input string) jsonx.WireValue {
				t.Helper()
				if input == "" {
					return jsonx.WireValue{}
				}
				v, err := jsonx.DecodeWire([]byte(input))
				if err != nil {
					t.Fatalf("DecodeWire(%q): %v", input, err)
				}
				return v
			}
			event := decode(tt.event)
			raw := jsontext.Value(tt.event)
			rawMerged := sentry.MergeUsage(jsontext.Value(previous), raw)
			if diff := gocmp.Diff(tt.merged, string(rawMerged)); diff != "" {
				t.Errorf("raw merged usage (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(decode(string(rawMerged)), sentry.MergeWireUsage(decode(previous), event)); diff != "" {
				t.Errorf("merged usage (-raw +wire):\n%s", diff)
			}
			if tt.event == "" {
				if diff := gocmp.Diff(jsonx.WireValue{}, sentry.MergeWireUsage(jsonx.WireValue{}, event)); diff != "" {
					t.Errorf("absent usage (-want +got):\n%s", diff)
				}
			}
			var rawState, wireState sentry.State
			target := model()
			for _, s := range []*sentry.State{&rawState, &wireState} {
				observe(s, "session", target, 109, false)
				s.Overflow("session", target, false)
			}
			rawState.Observe("session", target, raw, tt.completed)
			wireState.ObserveWire("session", target, event, tt.completed)
			var want sentry.Crossing
			if tt.crosses {
				want = sentry.Crossing{Usage: tt.observed, Trigger: 100}
			}
			for label, state := range map[string]*sentry.State{"raw": &rawState, "wire": &wireState} {
				got, crossed := state.Check("session", target, []byte(first))
				if diff := gocmp.Diff(want, got); diff != "" || tt.crosses != crossed {
					t.Errorf("%s crossing=%t, want %t (-want +got):\n%s", label, crossed, tt.crosses, diff)
				}
				if got := state.Overflow("session", target, false); got != tt.observed {
					t.Errorf("%s observed usage = %v, want %v", label, got, tt.observed)
				}
			}
		})
	}
}
