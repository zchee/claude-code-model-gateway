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
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestMalformedIdentifiersLeaveValidControlsUsable(t *testing.T) {
	m := model()
	root := sentry.SessionKey(true, "s", nil)
	type identity struct {
		session string
		target  sentry.Target
	}
	replacementModel := m
	replacementModel.Key = "gpt-�"
	defaultModel := m
	defaultModel.Key = "default"
	controls := []identity{
		{sentry.SessionKey(true, "�", nil), m},
		{sentry.SessionKey(true, "s", new("�")), m},
		{root, m},
		{root, replacementModel},
		{root, defaultModel},
	}
	var s sentry.State
	for i, c := range controls {
		observe(&s, c.session, c.target, float64(110+i), false)
	}
	badDirect := m
	badDirect.Key = "gpt-\xff"
	badResolved, err := sentry.ResolveTarget(policy.Resolver{}, badDirect.Key, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	bad := []identity{
		{sentry.SessionKey(true, "\xff", nil), m},
		{sentry.SessionKey(true, "s", new("\xff")), m},
		{"\xff", m},
		{root, badDirect},
		{root, badResolved},
	}
	for _, c := range bad {
		observe(&s, c.session, c.target, 81, false)
		s.ObserveJSON(c.session, c.target, []byte(first), []byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":82}}`))
		check(t, &s, c.session, c.target, first, sentry.Crossing{})
		if got := s.Overflow(c.session, c.target, true); got != 0 {
			t.Errorf("disabled overflow returned %v", got)
		}
	}
	for i, c := range controls {
		check(t, &s, c.session, c.target, first, sentry.Crossing{Usage: float64(110 + i), Trigger: 100})
	}
}
