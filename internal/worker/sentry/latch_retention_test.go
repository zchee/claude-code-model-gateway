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
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

// guardedBackend publishes one guarded backend row for every identifier.
type guardedBackend struct{}

func (guardedBackend) ResolveBackend(string) *policy.Model {
	return &policy.Model{Backend: "codex", BackendWindow: 110, CompactHeadroom: 10, Sentry: "synthetic-413"}
}

// TestFirstTurnWithoutUsageStoresNoLatch checks that a guarded session/model pair keeps no latch
// until a crossing, a positive usage observation or an overflow stores one: a first turn's check
// and an observation of zero usage leave nothing retained, where the source allocates the pair's
// latch on its first guarded check.
func TestFirstTurnWithoutUsageStoresNoLatch(t *testing.T) {
	const firstTurn = `{"messages":[{"role":"user","content":"hello"}]}`
	target := Target{Key: "gpt-5.6-sol", Policy: &policy.Sentry{BackendWindow: 110, CompactTrigger: 100, Source: "derived"}, Headroom: 10}
	tests := map[string]struct {
		published bool
		// usage is observed before the check when it is not empty.
		usage       string
		wantLatches int
	}{
		"success: first-turn check without usage on a direct target": {},
		"success: first-turn check without usage under a publication": {
			published: true,
		},
		"success: zero usage observation and check on a direct target": {
			usage: `{"type":"message_delta","usage":{"input_tokens":0,"cache_read_input_tokens":0}}`,
		},
		"success: zero usage observation and check under a publication": {
			published: true, usage: `{"type":"message_delta","usage":{"input_tokens":0}}`,
		},
		"success: positive usage observation stores one latch": {
			usage: `{"type":"message_delta","usage":{"input_tokens":50}}`, wantLatches: 1,
		},
		"success: positive usage observation under a publication stores one latch": {
			published: true, usage: `{"type":"message_delta","usage":{"input_tokens":50}}`, wantLatches: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s State
			if tt.published {
				s.Publish(1, guardedBackend{}, policy.Windows{}, 0)
			}
			if tt.usage != "" {
				s.Observe("session", target, []byte(tt.usage), false)
			}
			if _, refused := s.Check("session", target, []byte(firstTurn)); refused {
				t.Fatal("a first turn below the trigger was refused")
			}
			if s.model(target, true) == nil {
				t.Fatal("test precondition: the target is not guarded")
			}
			if got := len(s.sessions); got != tt.wantLatches {
				t.Errorf("retained latches = %d, want %d: %v", got, tt.wantLatches, s.sessions)
			}
		})
	}
}
