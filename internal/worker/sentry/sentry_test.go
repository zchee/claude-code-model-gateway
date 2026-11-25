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
	"fmt"
	"os"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

const (
	first        = `{"model":"claude-gpt-5.6-sol","max_tokens":1,"messages":[{"role":"user","content":"prompt-secret"}]}`
	continuation = `{"model":"claude-gpt-5.6-sol","max_tokens":1,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`
	prompt       = "You are a helpful AI assistant tasked with summarizing conversations."
	compact      = `{"model":"claude-gpt-5.6-sol","max_tokens":1,"stream":true,"system":[{"type":"text","text":"` + prompt + `"}],"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"summarize"}]}`
)

func TestRequestPhase(t *testing.T) {
	tests := map[string]struct{ body, want string }{
		"success: first":                                 {first, "first_turn"},
		"success: continuation":                          {continuation, "continuation"},
		"success: compaction":                            {compact, "compaction"},
		"success: no messages":                           {`{"model":"claude-gpt-5.6-sol"}`, "UNVERIFIED"},
		"success: null message":                          {`{"messages":[null,{"role":"user","content":"x"}]}`, "first_turn"},
		"success: embedded prompt before messages check": {`{"stream":true,"system":"prefix ` + prompt + ` suffix"}`, "compaction"},
		"success: non-stream summary":                    {`{"system":"` + prompt + `","messages":[]}`, "first_turn"},
		"success: stream must be boolean":                {`{"stream":1,"system":"` + prompt + `","messages":[]}`, "first_turn"},
		"success: non-list messages":                     {`{"messages":{}}`, "UNVERIFIED"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, sentry.RequestPhase([]byte(tt.body))); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestSessionKey(t *testing.T) {
	tests := map[string]struct {
		enabled bool
		session string
		agent   *string
		want    string
	}{
		"success: no session":             {true, "", new("a"), ""},
		"success: disabled":               {false, "s", nil, ""},
		"success: no agent":               {true, "s", nil, `["s",null]`},
		"success: blank agent is present": {true, "s", new(""), `["s",""]`},
		"success: unvalidated components": {true, "s /@", new("a|b"), `["s /@","a|b"]`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, sentry.SessionKey(tt.enabled, tt.session, tt.agent)); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestResolveTarget(t *testing.T) {
	tests := map[string]struct {
		id, key string
		cap     int64
		want    *policy.Sentry
	}{
		"success: extended tier shares Codex key": {"claude-gpt-5.6-sol-ultrafast[1m]", "gpt-5.6-sol", 272000, &policy.Sentry{BackendWindow: 920012, CompactTrigger: 187000, Source: "cap"}},
		"success: Grok keeps tier":                {"claude-grok-4.7-fast[1m]", "grok-4.7-fast", 0, &policy.Sentry{BackendWindow: 500000, CompactTrigger: 460000, Source: "derived"}},
		"success: native Claude has no sentry":    {"claude-opus-5-5", "opus-5-5", 0, nil},
		"success: no model uses default key":      {"", "default", 0, nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := sentry.ResolveTarget(policy.Resolver{}, tt.id, 0, tt.cap, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Key != tt.key {
				t.Errorf("key=%q, want %q", got.Key, tt.key)
			}
			if diff := gocmp.Diff(tt.want, got.Policy); diff != "" {
				t.Error(diff)
			}
			if tt.want != nil && got.Headroom != 40000 {
				t.Errorf("headroom=%d, want 40000", got.Headroom)
			}
		})
	}
}

func model() sentry.Target {
	return sentry.Target{Key: "gpt-5.6-sol", Policy: &policy.Sentry{BackendWindow: 110, CompactTrigger: 100, Source: "derived"}, Headroom: 10}
}

func observe(state *sentry.State, key string, m sentry.Target, u float64, completed bool) {
	state.Observe(key, m, jsontext.Value(fmt.Sprintf(`{"type":"message_delta","usage":{"input_tokens":%g}}`, u)), completed)
}

func check(t *testing.T, state *sentry.State, key string, m sentry.Target, request string, want sentry.Crossing) {
	t.Helper()
	got, ok := state.Check(key, m, []byte(request))
	if diff := gocmp.Diff(want, got); diff != "" || ok != (want != (sentry.Crossing{})) {
		t.Errorf("Check(%q, %q), refused=%t (-want +got): %s", key, m.Key, ok, diff)
	}
}

func TestCrossing(t *testing.T) {
	var state sentry.State
	m := model()
	const key = `["s",null]`
	pass := sentry.Crossing{}
	check(t, &state, key, m, first, pass)
	observe(&state, key, m, 100, false)
	check(t, &state, key, m, first, pass)
	observe(&state, key, m, 101, false)
	check(t, &state, key, m, compact, pass)
	check(t, &state, key, m, first, sentry.Crossing{Usage: 101, Trigger: 100})
	check(t, &state, key, m, first, pass)
	// A high observation alone cannot rearm a refusal at either equality boundary.
	for _, u := range []float64{75, 100, 101} {
		observe(&state, key, m, u, false)
		check(t, &state, key, m, first, pass)
	}
	observe(&state, key, m, 100, true)
	observe(&state, key, m, 101, false)
	check(t, &state, key, m, first, pass)
	observe(&state, key, m, 74.75, false)
	check(t, &state, key, m, first, pass)
	observe(&state, key, m, 102, false)
	check(t, &state, key, m, first, sentry.Crossing{Usage: 102, Trigger: 100})
	observe(&state, key, m, 102, true)
	check(t, &state, key, m, first, sentry.Crossing{Usage: 102, Trigger: 100})
}

func TestDeltaObservation(t *testing.T) {
	tests := map[string]struct {
		event string
		want  float64
	}{
		"success: only finite input counts": {`{"type":"message_delta","usage":{"input_tokens":70,"cache_read_input_tokens":22.25,"cache_creation_input_tokens":9,"output_tokens":999}}`, 101.25},
		"success: ignore strings and null":  {`{"type":"message_delta","usage":{"input_tokens":102,"cache_read_input_tokens":"90","cache_creation_input_tokens":null}}`, 102},
		"success: not a delta":              {`{"type":"message_start","usage":{"input_tokens":1}}`, 111},
		"success: ignore output only":       {`{"type":"message_delta","usage":{"output_tokens":999}}`, 111},
		"success: ignore nonpositive sum":   {`{"type":"message_delta","usage":{"input_tokens":-10,"cache_read_input_tokens":5}}`, 111},
		"success: binary64 accumulation":    {`{"type":"message_delta","usage":{"input_tokens":9007199254740992,"cache_read_input_tokens":1,"cache_creation_input_tokens":2}}`, 9007199254740994},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentry.State
			m := model()
			observe(&s, "s", m, 111, false)
			s.Observe("s", m, jsontext.Value(tt.event), false)
			check(t, &s, "s", m, first, sentry.Crossing{Usage: tt.want, Trigger: 100})
		})
	}
}

func TestCompletedJSON(t *testing.T) {
	tests := map[string]struct {
		reason string
		rearm  bool
	}{
		"success: end turn":      {`"end_turn"`, true},
		"success: stop sequence": {`"stop_sequence"`, true},
		"success: tool use":      {`"tool_use"`, true},
		"success: max tokens":    {`"max_tokens"`, false},
		"success: exceeded":      {`"model_context_window_exceeded"`, false},
		"success: pause":         {`"pause_turn"`, false},
		"success: refusal":       {`"refusal"`, false},
		"success: unknown":       {`"other"`, false},
		"success: null":          {`null`, false},
		"success: absent":        {"", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentry.State
			m := model()
			observe(&s, "s", m, 101, false)
			check(t, &s, "s", m, first, sentry.Crossing{Usage: 101, Trigger: 100})
			reason := ""
			if tt.reason != "" {
				reason = `,"stop_reason":` + tt.reason
			}
			response := []byte(`{"type":"message","usage":{"input_tokens":109}` + reason + `}`)
			s.ObserveJSON("s", m, []byte(compact), response)
			check(t, &s, "s", m, first, sentry.Crossing{})
			s.ObserveJSON("s", m, []byte(first), response)
			want := sentry.Crossing{}
			if tt.rearm {
				want = sentry.Crossing{Usage: 109, Trigger: 100}
			}
			check(t, &s, "s", m, first, want)
			if got := s.Overflow("s", m, false); got != 109 {
				t.Errorf("last usage=%v, want 109", got)
			}
		})
	}
}

func TestCompactionUsage(t *testing.T) {
	var s sentry.State
	m := model()
	observe(&s, "s", m, 101, false)
	check(t, &s, "s", m, first, sentry.Crossing{Usage: 101, Trigger: 100})
	s.ObserveJSON("s", m, []byte(compact), []byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":109}}`))
	if got := s.Overflow("s", m, false); got != 109 {
		t.Errorf("compaction usage=%v, want 109", got)
	}
}

func TestFalsyJSONError(t *testing.T) {
	tests := map[string]struct{ value string }{
		"success: null":         {`null`},
		"success: false":        {`false`},
		"success: zero":         {`0`},
		"success: empty string": {`""`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentry.State
			m := model()
			observe(&s, "s", m, 101, false)
			check(t, &s, "s", m, first, sentry.Crossing{Usage: 101, Trigger: 100})
			s.ObserveJSON("s", m, []byte(first), []byte(`{"type":"message","error":`+tt.value+`,"stop_reason":"end_turn","usage":{"input_tokens":109}}`))
			check(t, &s, "s", m, first, sentry.Crossing{Usage: 109, Trigger: 100})
		})
	}
}

func TestIgnoredJSON(t *testing.T) {
	tests := map[string]struct{ response string }{
		"success: malformed":          {`{"type":"message"`},
		"success: error on a message": {`{"type":"message","error":{"type":"api_error"},"stop_reason":"end_turn","usage":{"input_tokens":109}}`},
		"success: not a message":      {`{"type":"error","stop_reason":"end_turn","usage":{"input_tokens":109}}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentry.State
			m := model()
			s.ObserveJSON("s", m, []byte(first), []byte(tt.response))
			if got := s.Overflow("s", m, false); got != 0 {
				t.Errorf("unexpected usage %v", got)
			}
		})
	}
}

func TestKeyIsolation(t *testing.T) {
	var s sentry.State
	m := model()
	root := sentry.SessionKey(true, "s", nil)
	agent := sentry.SessionKey(true, "s", new("a"))
	other := m
	other.Key = "gpt-other"
	observe(&s, root, m, 105, false)
	tests := map[string]struct {
		k string
		m sentry.Target
	}{
		"success: other agent":   {agent, m},
		"success: other session": {sentry.SessionKey(true, "t", nil), m},
		"success: other model":   {root, other},
		"success: no session":    {"", m},
		"success: no policy":     {root, sentry.Target{Key: m.Key}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { check(t, &s, tt.k, tt.m, first, sentry.Crossing{}) })
	}
	observe(&s, "", m, 999, false)
	check(t, &s, "", m, first, sentry.Crossing{})
	observe(&s, root, sentry.Target{Key: m.Key}, 999, false)
	check(t, &s, root, m, first, sentry.Crossing{Usage: 105, Trigger: 100})
	observe(&s, agent, m, 106, false)
	check(t, &s, agent, m, first, sentry.Crossing{Usage: 106, Trigger: 100})
}

func TestLatchWithoutExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var s sentry.State
		m := model()
		observe(&s, "fired", m, 105, false)
		check(t, &s, "fired", m, first, sentry.Crossing{Usage: 105, Trigger: 100})
		observe(&s, "armed", m, 110, false)
		time.Sleep(7 * 24 * time.Hour)
		check(t, &s, "fired", m, first, sentry.Crossing{})
		check(t, &s, "armed", m, first, sentry.Crossing{Usage: 110, Trigger: 100})
	})
}
