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
	"encoding/json/jsontext"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

// SessionKey returns the encoded session and optional agent, or empty if gating is disabled or the session is absent.
func SessionKey(enabled bool, session string, agent *string) string { return "" }

// Target identifies the shared model ceiling and the refresh generation's effective policy.
type Target struct {
	Key      string
	Policy   *policy.Sentry
	Headroom int64
}

// ResolveTarget canonicalizes the model key and derives its trigger from the request's resolver and settings.
func ResolveTarget(resolver policy.Resolver, id string, env float64, cap, compactAt int64) (Target, error) {
	return Target{}, nil
}

// RequestPhase classifies a client request as first_turn, continuation, compaction or UNVERIFIED.
func RequestPhase(body []byte) string { return "" }

// State holds session latches and learned model ceilings. The zero value is ready for use.
type State struct{}

// Crossing describes the previous turn's measured usage and the trigger it exceeded.
type Crossing struct{ Usage, Trigger float64 }

// Check tests the original request for a crossing and latches a refusal until rearmed.
func (*State) Check(session string, target Target, request []byte) (Crossing, bool) {
	return Crossing{}, false
}

// Observe records a message_delta usage observation; ordinaryCompleted marks a delivered non-compaction turn.
func (*State) Observe(session string, target Target, event jsontext.Value, ordinaryCompleted bool) {}

// ObserveJSON records a proxy JSON message with the completion rule for its stop reason and original request.
func (*State) ObserveJSON(session string, target Target, request, response []byte) {}

// Overflow latches a genuine refusal and returns the previous usage, optionally learning a derived ceiling.
func (*State) Overflow(session string, target Target, learn bool) float64 { return 0 }

// Counts preserves whether each engine count was available, independently of overflow recognition.
type Counts struct{ Tokens, Limit *float64 }

// ParseOverflow recognizes genuine and legacy error responses and reports their upstream token counts.
func ParseOverflow(status int, body []byte) (Counts, bool) { return Counts{}, false }

// MergeUsage applies one stream event to accumulated usage, replacing supplied fields rather than summing them.
func MergeUsage(previous, event jsontext.Value) jsontext.Value { return nil }
