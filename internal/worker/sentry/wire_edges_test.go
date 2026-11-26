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

	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestUsageWireOrder(t *testing.T) {
	tests := map[string]struct{ previous, event, want string }{
		"success: spread overwrites in place and sorts index keys": {`{"z":1,"3":0,"a":3}`, `{"usage":{"z":2,"1":4,"q":1}}`, `{"1":4,"3":0,"z":2,"a":3,"q":1}`},
		"success: string spreads UTF-16 code units":                {`"😀"`, `{"usage":{}}`, `{"0":"\ud83d","1":"\ude00"}`},
		"success: surrogate names are distinct":                    {`{"\ud800":1,"\ud801":2}`, `{"usage":{"\ud800":3}}`, `{"\ud800":3,"\ud801":2}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := sentry.MergeUsage(jsontext.Value(tt.previous), jsontext.Value(tt.event))
			if diff := gocmp.Diff(tt.want, string(got)); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestUpstreamText(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		ok     bool
		counts sentry.Counts
	}{
		"success: ECMAScript whitespace":                            {413, `{"error":{"type":"request_too_large","message":"18` + string(rune(0xa0)) + `ToKeNs` + string(rune(0xfeff)) + `>` + string(rune(0x2028)) + `13\u000btokens"}}`, true, sentry.Counts{Tokens: new(18.0), Limit: new(13.0)}},
		"success: genuine recognition does not validate counts":     {413, `{"error":{"type":"request_too_large","message":"18 toKens > 13 tokens"}}`, true, sentry.Counts{}},
		"error: successful status does not scan for legacy wording": {200, `context length`, false, sentry.Counts{}},
		"error: non-Unicode case folding":                           {400, `too many toKens`, false, sentry.Counts{}},
		"success: wrapped marker needs no outer type":               {400, `{"error":{"type":"other","message":"exceed_context_size_error: request (18 tokens) exceeds the available context size (13 tokens)"}}`, true, sentry.Counts{Tokens: new(18.0), Limit: new(13.0)}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := sentry.ParseOverflow(tt.status, []byte(tt.body))
			if diff := gocmp.Diff(tt.counts, got); diff != "" || ok != tt.ok {
				t.Errorf("recognized=%t want %t; counts (-want +got): %s", ok, tt.ok, diff)
			}
		})
	}
}

func TestObservationAdditionOrder(t *testing.T) {
	var s sentry.State
	m := model()
	s.Observe("s", m, jsontext.Value(`{"type":"message_delta","usage":{"cache_creation_input_tokens":2,"cache_read_input_tokens":1,"input_tokens":9007199254740992}}`), false)
	check(t, &s, "s", m, first, sentry.Crossing{Usage: 9007199254740994, Trigger: 100})
}

func TestOverflowWithoutPolicyStillLatches(t *testing.T) {
	var s sentry.State
	m := model()
	observe(&s, "s", m, 109, false)
	if got := s.Overflow("s", sentry.Target{Key: m.Key}, true); got != 109 {
		t.Fatalf("previous usage = %v; want 109", got)
	}
	check(t, &s, "s", m, first, sentry.Crossing{})
}
