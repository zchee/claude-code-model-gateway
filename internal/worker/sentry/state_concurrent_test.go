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
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestLateCallbackUsesAdmittedWindow(t *testing.T) {
	var s sentry.State
	old := model()
	observe(&s, "old", old, 101, false)
	check(t, &s, "old", old, first, sentry.Crossing{Usage: 101, Trigger: 100})
	current := sentry.Target{Key: old.Key, Policy: &policy.Sentry{BackendWindow: 210, CompactTrigger: 200, Source: "derived"}, Headroom: 10}
	check(t, &s, "learner", current, first, sentry.Crossing{})
	observe(&s, "learner", current, 150, false)
	s.Overflow("learner", current, true)
	observe(&s, "old", old, 130, true)
	observe(&s, "old", current, 150, false)
	check(t, &s, "old", current, first, sentry.Crossing{})
	observe(&s, "new", current, 150, false)
	check(t, &s, "new", current, first, sentry.Crossing{Usage: 150, Trigger: 140})

	// A fresh snapshot of the same window must not discard the learned ceiling.
	equal := current
	equal.Policy = new(*current.Policy)
	observe(&s, "equal", equal, 145, false)
	check(t, &s, "equal", equal, first, sentry.Crossing{Usage: 145, Trigger: 140})
}

func TestLateOverflowUsesCurrentHeadroom(t *testing.T) {
	var s sentry.State
	old := model()
	check(t, &s, "old", old, first, sentry.Crossing{})
	observe(&s, "old", old, 155, false)
	current := sentry.Target{Key: old.Key, Policy: &policy.Sentry{BackendWindow: 220, CompactTrigger: 200, Source: "derived"}, Headroom: 20}
	check(t, &s, "new", current, first, sentry.Crossing{})
	s.Overflow("old", old, true)
	observe(&s, "new", current, 140, false)
	check(t, &s, "new", current, first, sentry.Crossing{Usage: 140, Trigger: 135})
}

func TestConcurrentTransitions(t *testing.T) {
	const n = 32
	concurrent := func(f func(int)) {
		var ready, done sync.WaitGroup
		start := make(chan struct{})
		ready.Add(n)
		for i := range n {
			done.Go(func() { ready.Done(); <-start; f(i) })
		}
		ready.Wait()
		close(start)
		done.Wait()
	}
	t.Run("success: one refusal per armed latch", func(t *testing.T) {
		var s sentry.State
		m := model()
		observe(&s, "s", m, 101, false)
		var refusals atomic.Int32
		concurrent(func(int) {
			if _, refused := s.Check("s", m, []byte(first)); refused {
				refusals.Add(1)
			}
		})
		if got := refusals.Load(); got != 1 {
			t.Errorf("%d concurrent checks yielded %d refusals, want 1", n, got)
		}
	})
	t.Run("success: competing overflows retain minimum ceiling", func(t *testing.T) {
		var s sentry.State
		m := model()
		for i := range n {
			observe(&s, strconv.Itoa(i), m, float64(60+i), false)
		}
		concurrent(func(i int) { s.Overflow(strconv.Itoa(i), m, true) })
		observe(&s, "reader", m, 51, false)
		check(t, &s, "reader", m, first, sentry.Crossing{Usage: 51, Trigger: 50})
	})
}
