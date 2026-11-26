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
	"unicode/utf16"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestRawMergeUsagePassthrough(t *testing.T) {
	const (
		spacedPrev   = "  { \n\t \"input_tokens\" : 3 ,  \"output_tokens\" : 2 \r\n}  "
		unsortedPrev = `{"z":1,"0":2,"z":3}`
		invalidPrev  = "  {not valid json\t\n"
	)
	tests := map[string]struct {
		previous string
		event    string
		want     string
	}{
		"success: content delta preserves unusual whitespace verbatim": {
			previous: spacedPrev,
			event:    `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
			want:     spacedPrev,
		},
		"success: content delta preserves invalid previous spelling verbatim": {
			previous: invalidPrev,
			event:    `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
			want:     invalidPrev,
		},
		"success: ping preserves unnormalized previous bytes verbatim": {
			previous: unsortedPrev,
			event:    `{"type":"ping"}`,
			want:     unsortedPrev,
		},
		"success: message delta without usage preserves unusual whitespace verbatim": {
			previous: spacedPrev,
			event:    `{"type":"message_delta"}`,
			want:     spacedPrev,
		},
		"success: message delta with scalar usage preserves invalid previous spelling verbatim": {
			previous: invalidPrev,
			event:    `{"type":"message_delta","usage":13}`,
			want:     invalidPrev,
		},
		"success: message delta with null usage preserves unusual whitespace verbatim": {
			previous: spacedPrev,
			event:    `{"type":"message_delta","usage":null}`,
			want:     spacedPrev,
		},
		"success: message start without nested usage ignores outer usage and preserves invalid spelling": {
			previous: invalidPrev,
			event:    `{"type":"message_start","usage":{"input_tokens":5},"message":{}}`,
			want:     invalidPrev,
		},
		"success: malformed event preserves invalid previous spelling verbatim": {
			previous: invalidPrev,
			event:    `{"type":"message_delta"`,
			want:     invalidPrev,
		},
		"success: null event preserves unusual whitespace verbatim": {
			previous: spacedPrev,
			event:    `null`,
			want:     spacedPrev,
		},
		"success: usable delta normalizes top level onto spaced previous": {
			previous: spacedPrev,
			event:    `{"type":"message_delta","usage":{"output_tokens":9,"0":1}}`,
			want:     `{"0":1,"input_tokens":3,"output_tokens":9}`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			prev := jsontext.Value(tt.previous)
			event := jsontext.Value(tt.event)
			got := sentry.MergeUsage(prev, event)
			if diff := gocmp.Diff([]byte(tt.want), []byte(got)); diff != "" {
				t.Errorf("MergeUsage bytes (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWireMergeUsagePassthrough(t *testing.T) {
	newUnnormalized := func() jsonx.WireValue {
		return jsonx.WireValue{
			Kind: '{',
			Members: []jsonx.Member{
				{Name: utf16.Encode([]rune("z")), Value: jsonx.WireValue{Kind: '0', Number: 1}},
				{Name: utf16.Encode([]rune("0")), Value: jsonx.WireValue{Kind: '0', Number: 2}},
				{Name: utf16.Encode([]rune("z")), Value: jsonx.WireValue{Kind: '0', Number: 3}},
			},
		}
	}
	tests := map[string]struct {
		event string
	}{
		"success: content delta returns unnormalized previous untouched": {`{"type":"content_block_delta","delta":{"text":"hi"}}`},
		"success: missing usage returns unnormalized previous untouched": {`{"type":"message_delta"}`},
		"success: scalar usage returns unnormalized previous untouched":  {`{"type":"message_delta","usage":13}`},
		"success: message start without nested usage returns previous":   {`{"type":"message_start","usage":{"input_tokens":5},"message":{}}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			prev := newUnnormalized()
			want := newUnnormalized()
			ev, err := jsonx.DecodeWire([]byte(tt.event))
			if err != nil {
				t.Fatalf("DecodeWire(%q): %v", tt.event, err)
			}
			got := sentry.MergeWireUsage(prev, ev)
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("MergeWireUsage modified previous (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(want, prev); diff != "" {
				t.Errorf("MergeWireUsage mutated caller previous (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRawObserveWithoutUsableUsageNoOp(t *testing.T) {
	tests := map[string]struct {
		event string
	}{
		"success: content delta leaves state and latch unchanged":         {`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`},
		"success: spaced content delta leaves state and latch unchanged":  {"  { \n\t \"type\" : \"content_block_delta\" , \"delta\" : { \"text\" : \"hi\" } \r\n}  "},
		"success: message start leaves state and latch unchanged":         {`{"type":"message_start","message":{"usage":{"input_tokens":200}}}`},
		"success: message delta without usage leaves state unchanged":     {`{"type":"message_delta"}`},
		"success: message delta with scalar usage leaves state unchanged": {`{"type":"message_delta","usage":200}`},
		"success: message delta with null usage leaves state unchanged":   {`{"type":"message_delta","usage":null}`},
		"success: message delta with output only leaves state unchanged":  {`{"type":"message_delta","usage":{"output_tokens":999}}`},
		"success: invalid spelling raw event leaves state unchanged":      {"  {not valid json\t\n"},
		"success: empty raw event leaves state unchanged":                 {""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentry.State
			target := model()
			observe(&s, "session", target, 109, false)
			// Latch the refusal so a spurious observation with ordinaryCompleted=true would rearm it.
			check(t, &s, "session", target, first, sentry.Crossing{Usage: 109, Trigger: 100})

			s.Observe("session", target, jsontext.Value(tt.event), true)

			check(t, &s, "session", target, first, sentry.Crossing{})
			if got := s.Overflow("session", target, false); got != 109 {
				t.Errorf("observed usage after no-op event = %v, want 109", got)
			}
		})
	}
}
