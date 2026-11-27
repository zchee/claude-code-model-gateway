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
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func localListing(id string, window int64) policy.Resolver {
	routes := proxymodels.NewTable([]proxymodels.Route{{ID: id, Provider: "local"}})
	return policy.NewResolver(policy.ResolverInputs{Routes: routes, LocalWindow: window})
}

func TestRelistedModelCeiling(t *testing.T) {
	tests := map[string]struct {
		window  int64
		trigger float64
	}{
		"success: same window restores learned trigger":      {60000, 35000},
		"success: different window discards learned trigger": {90000, 60000},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentry.State
			m := sentry.Target{Key: "local-model"}
			s.Publish(1, localListing(m.Key, 60000), nil, 0)
			observe(&s, "learner", m, 55000, false)
			s.Overflow("learner", m, true)
			s.Publish(2, policy.Resolver{}, nil, 0)
			observe(&s, "learner", m, 30000, false)
			if got := s.Overflow("learner", m, true); got != 55000 {
				t.Fatalf("dormant usage = %g, want 55000", got)
			}
			s.Publish(3, localListing(m.Key, tt.window), nil, 0)
			observe(&s, "reader", m, 37000, false)
			if tt.trigger < 37000 {
				check(t, &s, "reader", m, first, sentry.Crossing{Usage: 37000, Trigger: tt.trigger})
			} else {
				check(t, &s, "reader", m, first, sentry.Crossing{})
				observe(&s, "reader", m, 60001, false)
				check(t, &s, "reader", m, first, sentry.Crossing{Usage: 60001, Trigger: tt.trigger})
			}
		})
	}
}

func TestReplacedBackendDoesNotFollowPickerAlias(t *testing.T) {
	var s sentry.State
	old := sentry.Target{Key: "fable-live"}
	s.Publish(1, localListing(old.Key, 60000), nil, 0)
	observe(&s, "old", old, 55000, false)
	s.Overflow("old", old, true)
	replacement := sentry.Target{Key: "claude-fable-live"}
	s.Publish(2, localListing(replacement.Key, 60000), nil, 0)
	observe(&s, "late", old, 37000, false)
	check(t, &s, "late", old, first, sentry.Crossing{})
	if got := s.Overflow("late", old, true); got != 0 {
		t.Fatalf("removed backend recorded usage %g", got)
	}
	observe(&s, "reader", replacement, 37000, false)
	check(t, &s, "reader", replacement, first, sentry.Crossing{})
	observe(&s, "reader", replacement, 40001, false)
	check(t, &s, "reader", replacement, first, sentry.Crossing{Usage: 40001, Trigger: 40000})
}

// TestWindowChangedAndRestoredLosesCeiling checks that every publication replaces the policy of each
// tracked model at once: a learned ceiling is dropped as soon as a publication changes the model's
// window, and a later publication that restores the window does not bring it back, even when no
// request touched the model in between. The source replaces a model's policy only when a request
// reads it, so with no request between the two publications it keeps the learned ceiling.
func TestWindowChangedAndRestoredLosesCeiling(t *testing.T) {
	tests := map[string]struct {
		// between is the window published after the ceiling was learned, before 60000 is
		// published again.
		between int64
		want    sentry.Crossing
	}{
		"success: unchanged window keeps the learned trigger": {
			between: 60000, want: sentry.Crossing{Usage: 37000, Trigger: 35000},
		},
		"success: window changed and restored with no request between drops the learned trigger": {
			between: 90000,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var s sentry.State
			m := sentry.Target{Key: "local-model"}
			s.Publish(1, localListing(m.Key, 60000), nil, 0)
			observe(&s, "learner", m, 55000, false)
			if got := s.Overflow("learner", m, true); got != 55000 {
				t.Fatalf("learning overflow usage = %g, want 55000", got)
			}
			s.Publish(2, localListing(m.Key, tt.between), nil, 0)
			s.Publish(3, localListing(m.Key, 60000), nil, 0)
			observe(&s, "reader", m, 37000, false)
			check(t, &s, "reader", m, first, tt.want)
			if tt.want == (sentry.Crossing{}) {
				// The derived trigger of a 60000 window without a learned ceiling.
				observe(&s, "reader", m, 40001, false)
				check(t, &s, "reader", m, first, sentry.Crossing{Usage: 40001, Trigger: 40000})
			}
		})
	}
}
