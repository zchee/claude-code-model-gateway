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

// fixedWindow supplies one immutable policy row at a scale where strict boundaries are easy to check.
type fixedWindow int64

func (w fixedWindow) Resolve(id string) *policy.Model {
	if id != "claude-gpt-5.6-sol[1m]" {
		return nil
	}
	return &policy.Model{Backend: "codex", BackendWindow: int64(w), CompactHeadroom: 10, Sentry: "synthetic-413"}
}

func TestPublishedWindowOrder(t *testing.T) {
	var s sentry.State
	old := model()
	current := sentry.Target{Key: old.Key, Policy: &policy.Sentry{BackendWindow: 210, CompactTrigger: 200, Source: "derived"}, Headroom: 10}
	s.Publish(0, fixedWindow(110), policy.Windows{}, 0)
	observe(&s, "old", old, 105, false)
	check(t, &s, "old", old, first, sentry.Crossing{Usage: 105, Trigger: 100})
	s.Publish(2, fixedWindow(210), policy.Windows{}, 0)
	check(t, &s, "learner", current, first, sentry.Crossing{})
	observe(&s, "learner", current, 150, false)
	s.Overflow("learner", current, true)
	check(t, &s, "late admission", old, first, sentry.Crossing{})
	s.Publish(1, fixedWindow(110), policy.Windows{}, 0)
	s.Publish(2, fixedWindow(110), policy.Windows{}, 0)
	s.Publish(3, fixedWindow(210), policy.Windows{}, 0)
	observe(&s, "reader", current, 150, false)
	check(t, &s, "reader", current, first, sentry.Crossing{Usage: 150, Trigger: 140})
	s.Publish(4, fixedWindow(110), policy.Windows{}, 0)
	observe(&s, "decreased", current, 105, false)
	check(t, &s, "decreased", current, first, sentry.Crossing{Usage: 105, Trigger: 100})
}

func TestCallbackReadsPublishedWindowBeforeAdmission(t *testing.T) {
	var s sentry.State
	old := model()
	s.Publish(1, fixedWindow(110), policy.Windows{}, 0)
	observe(&s, "s", old, 105, false)
	check(t, &s, "s", old, first, sentry.Crossing{Usage: 105, Trigger: 100})
	s.Publish(2, fixedWindow(210), policy.Windows{}, 0)
	s.ObserveJSON("s", old, []byte(first), []byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":155}}`))
	observe(&s, "s", old, 201, false)
	check(t, &s, "s", old, first, sentry.Crossing{})
}

func TestPublishedEligibility(t *testing.T) {
	// The backend name deliberately contains both a picker prefix and a suffix.
	const key = "claude-local[1m]"
	routes := proxymodels.NewTable([]proxymodels.Route{{ID: key, Provider: "local"}})
	on := policy.NewResolver(policy.ResolverInputs{Routes: routes, LocalWindow: 60000})
	off := policy.NewResolver(policy.ResolverInputs{Routes: routes, LocalWindow: 30000})
	target := func(r policy.Resolver) sentry.Target {
		m, err := sentry.ResolveTarget(r, "claude-"+key+"[1m]", 0, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	guarded, unguarded := target(on), target(off)
	if guarded.Policy == nil || unguarded.Policy != nil || unguarded.Key != key {
		t.Fatal("invalid local window setup")
	}
	var s sentry.State
	s.Publish(1, on, nil, 0)
	observe(&s, "s", guarded, 39000, false)
	s.Publish(2, off, nil, 0)
	observe(&s, "s", guarded, 10000, true)
	s.ObserveJSON("s", guarded, []byte(first), []byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":99999}}`))
	if got := s.Overflow("s", guarded, true); got != 39000 {
		t.Fatalf("unguarded overflow usage = %v, want 39000", got)
	}
	check(t, &s, "s", guarded, first, sentry.Crossing{})
	s.Publish(3, on, nil, 0)
	observe(&s, "s", unguarded, 41000, false)
	check(t, &s, "s", unguarded, first, sentry.Crossing{}) // The unguarded overflow still latched.
	observe(&s, "probe", unguarded, 39000, false)
	check(t, &s, "probe", unguarded, first, sentry.Crossing{}) // No ceiling learned while unguarded.
	s.Overflow("probe", unguarded, true)
	s.ObserveJSON("reader", unguarded, []byte(first), []byte(`{"type":"message","usage":{"input_tokens":19001}}`))
	check(t, &s, "reader", unguarded, first, sentry.Crossing{Usage: 19001, Trigger: 19000})
}

func TestPublishedBackendSettingsAreCopied(t *testing.T) {
	var s sentry.State
	m := model()
	windows := policy.Windows{"codex": {CompactAt: 70}}
	s.Publish(1, fixedWindow(110), windows, 80)
	windows["codex"] = policy.Setting{CompactAt: 20}
	observe(&s, "s", m, 75, false)
	check(t, &s, "s", m, first, sentry.Crossing{Usage: 75, Trigger: 70})
}
