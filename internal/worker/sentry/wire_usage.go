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

package sentry

import (
	"slices"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// ObserveWire records a decoded message_delta's usage for the session and target.
// An ordinary completed turn may rearm a fired refusal; an absent event has no effect.
func (s *State) ObserveWire(session string, target Target, event jsonx.WireValue, ordinaryCompleted bool) {
	if target.validFor(session) && text(member(event, "type")) == "message_delta" {
		s.observe(session, target, member(event, "usage"), ordinaryCompleted)
	}
}

// MergeWireUsage overlays usage supplied by a decoded stream event onto previous usage.
// Without a usable usage member it returns the previous value unchanged.
// A merged result has its own top-level member slice; nested values are shared with the inputs.
func MergeWireUsage(previous, event jsonx.WireValue) jsonx.WireValue {
	usage, ok := eventUsage(event)
	if !ok {
		return previous
	}
	// A new slice: normalizing in place must not write into the caller's members.
	return jsonx.WireValue{Kind: '{', Members: jsonx.NormalizeMembers(slices.Concat(spread(previous), spread(usage)))}
}
