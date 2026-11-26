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

// Package sentry tracks context usage and recognizes recoverable upstream overflows.
package sentry

import (
	"cmp"
	"encoding/json/jsontext"
	"math"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

const defaultHeadroom = 40000

// SessionKey returns the encoded session and optional agent, or empty if gating is disabled or the session is absent.
func SessionKey(enabled bool, session string, agent *string) string {
	if !enabled || session == "" {
		return ""
	}
	id := jsonx.WireValue{Kind: 'n'}
	if agent != nil {
		id = wireString(*agent)
	}
	// A pair of strings or null is always encodable.
	key, _ := jsonx.EncodeWire(jsonx.WireValue{Kind: '[', Elements: []jsonx.WireValue{wireString(session), id}})
	return string(key)
}

// Target identifies the shared model ceiling and the refresh generation's effective policy.
type Target struct {
	Key      string
	Policy   *policy.Sentry
	Headroom int64
}

// ResolveTarget canonicalizes the model key and derives its trigger from the request's resolver and settings.
func ResolveTarget(resolver policy.Resolver, id string, env float64, cap, compactAt int64) (Target, error) {
	model := resolver.Resolve(id)
	p, err := policy.EffectiveSentry(model, env, cap, compactAt)
	if err != nil {
		return Target{}, err
	}
	t := Target{Key: resolver.WindowModelID(id), Policy: p}
	if p != nil {
		t.Headroom = cmp.Or(model.CompactHeadroom, defaultHeadroom)
	}
	return t, nil
}

// RequestPhase classifies a client request as first_turn, continuation, compaction or UNVERIFIED.
func RequestPhase(body []byte) string {
	v, err := jsonx.DecodeWire(body)
	if err != nil {
		return "UNVERIFIED"
	}
	if member(v, "stream").Kind == 't' {
		const prompt = "You are a helpful AI assistant tasked with summarizing conversations."
		system := member(v, "system")
		if strings.Contains(text(system), prompt) {
			return "compaction"
		}
		// The source joins blocks with newlines; the marker cannot span that separator.
		for _, block := range system.Elements {
			if strings.Contains(text(member(block, "text")), prompt) {
				return "compaction"
			}
		}
	}
	messages := member(v, "messages")
	if messages.Kind != '[' {
		return "UNVERIFIED"
	}
	for _, message := range messages.Elements {
		if text(member(message, "role")) == "assistant" {
			return "continuation"
		}
	}
	return "first_turn"
}

type sessionModel struct{ session, model string }

type latch struct {
	usage float64
	fired bool
}

type modelState struct {
	policy.Sentry
	ceiling float64
}

// State holds session latches and learned model ceilings. Its zero value is safe for concurrent use.
// Like the original worker, it retains entries until the worker exits, with no expiry or cap.
// On 64-bit targets each session/model pair uses 48 bytes for its two string headers,
// usage count and latch, plus the retained string data and map overhead. Sessions share one ceiling per model.
type State struct {
	mu       sync.Mutex
	sessions map[sessionModel]latch
	models   map[string]*modelState
}

// model returns the policy snapshot under the caller's lock. Only a backend window change invalidates it.
func (s *State) model(t Target) *modelState {
	m := s.models[t.Key]
	if m == nil || m.BackendWindow != t.Policy.BackendWindow {
		if s.models == nil {
			s.models = make(map[string]*modelState)
		}
		m = &modelState{Sentry: *t.Policy}
		s.models[t.Key] = m
	}
	return m
}

func (s *State) store(k sessionModel, v latch) {
	if s.sessions == nil {
		s.sessions = make(map[sessionModel]latch)
	}
	s.sessions[k] = v
}

// Crossing describes the previous turn's measured usage and the trigger it exceeded.
type Crossing struct{ Usage, Trigger float64 }

// Check tests the original request for a crossing and latches a refusal until rearmed.
func (s *State) Check(session string, target Target, request []byte) (Crossing, bool) {
	if target.Policy == nil || RequestPhase(request) == "compaction" {
		return Crossing{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trigger := s.model(target).CompactTrigger
	key := sessionModel{session, target.Key}
	v := s.sessions[key]
	if session == "" || v.fired || v.usage <= trigger {
		return Crossing{}, false
	}
	v.fired = true
	s.store(key, v)
	return Crossing{Usage: v.usage, Trigger: trigger}, true
}

// Observe records a message_delta usage observation; ordinaryCompleted marks a delivered non-compaction turn.
func (s *State) Observe(session string, target Target, event jsontext.Value, ordinaryCompleted bool) {
	if session == "" || target.Policy == nil {
		return
	}
	v, err := jsonx.DecodeWire(event)
	if err == nil && text(member(v, "type")) == "message_delta" {
		s.observe(session, target, member(v, "usage"), ordinaryCompleted)
	}
}

func (s *State) observe(session string, target Target, usage jsonx.WireValue, completed bool) {
	var total float64
	for _, name := range [...]string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		v := member(usage, name)
		if v.Kind == '0' && !math.IsInf(v.Number, 0) {
			total += v.Number
		}
	}
	if total <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionModel{session, target.Key}
	v := s.sessions[key]
	v.usage = total
	if v.fired {
		trigger := s.model(target).CompactTrigger
		low := trigger - min(defaultHeadroom, trigger*0.25)
		v.fired = !(total < low || completed && total > trigger)
	}
	s.store(key, v)
}

// ObserveJSON records a proxy JSON message with the completion rule for its stop reason and original request.
func (s *State) ObserveJSON(session string, target Target, request, response []byte) {
	if session == "" || target.Policy == nil {
		return
	}
	v, err := jsonx.DecodeWire(response)
	if err != nil || text(member(v, "type")) != "message" || truthy(member(v, "error")) {
		return
	}
	var complete bool
	switch text(member(v, "stop_reason")) {
	case "end_turn", "stop_sequence", "tool_use":
		complete = RequestPhase(request) != "compaction"
	}
	s.observe(session, target, member(v, "usage"), complete)
}

// Overflow latches a genuine refusal and returns the previous usage, optionally learning a derived ceiling.
func (s *State) Overflow(session string, target Target, learn bool) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionModel{session, target.Key}
	v := s.sessions[key]
	if session != "" {
		v.fired = true
		s.store(key, v)
	}
	if target.Policy != nil {
		m := s.model(target)
		headroom := float64(target.Headroom)
		if learn && m.Source == "derived" && v.usage > headroom {
			if m.ceiling == 0 || v.usage < m.ceiling {
				m.ceiling = v.usage
			}
			m.CompactTrigger = max(1, m.ceiling-headroom)
		}
	}
	return v.usage
}

func wireString(s string) jsonx.WireValue {
	return jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(s))}
}

func text(v jsonx.WireValue) string { return string(utf16.Decode(v.Text)) }

func member(v jsonx.WireValue, name string) jsonx.WireValue {
	for _, m := range v.Members {
		if string(utf16.Decode(m.Name)) == name {
			return m.Value
		}
	}
	return jsonx.WireValue{}
}

func truthy(v jsonx.WireValue) bool {
	switch v.Kind {
	case 't', '{', '[':
		return true
	case '0':
		return v.Number != 0
	case '"':
		return len(v.Text) != 0
	default:
		return false
	}
}
