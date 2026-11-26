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

package proxyreq_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// pendingTurn is a classified request whose Prepare call may run on any goroutine.
type pendingTurn struct {
	r        *http.Request
	decision route.Decision
	w        *httptest.ResponseRecorder
	original []byte
}

// newPendingTurn classifies raw against an empty snapshot on the test goroutine.
func newPendingTurn(t testing.TB, path, raw string, headers http.Header) pendingTurn {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
	r.Header = headers.Clone()
	d := route.Snapshot{}.Classify(r.Method, r.RequestURI, []byte(raw))
	if d.Branch != "proxy" {
		t.Fatalf("fixture classified %q as %+v", raw, d)
	}
	return pendingTurn{r: r, decision: d, w: httptest.NewRecorder(), original: []byte(raw)}
}

func (p pendingTurn) prepare(h *proxyreq.Handler) (proxyreq.Result, error) {
	return h.Prepare(p.w, p.r, route.Snapshot{}, p.decision, p.original)
}

// turnOutcome is the part of a prepared dispatch turn that decides the following turns.
type turnOutcome struct {
	Status                     int
	Via, Model, Effort, Ticket string
}

func outcomeOf(res proxyreq.Result, err error) turnOutcome {
	if err != nil {
		return turnOutcome{Via: "error: " + err.Error()}
	}
	ticket := "<none>"
	if res.Ticket != nil {
		ticket = *res.Ticket
	}
	return turnOutcome{Status: res.LocalStatus, Via: res.Route.Via, Model: res.Route.EffectiveModel, Effort: res.Route.Effort, Ticket: ticket}
}

func autoBody(content string) string {
	return fmt.Sprintf(`{"model":"claude-codex-auto","messages":[{"role":"user","content":%q}]}`, content)
}

// TestConcurrentBriefingAndFollowUp runs a child agent's explicit briefing and its unmarked follow-up at the same
// time while the parent agent already has a route. Run one after the other, the two turns can end in only two ways:
// the briefing first, so the follow-up reuses the child's new route, or the follow-up first, so it inherits the
// parent's route and the briefing then replaces the child's binding. Either way the child is left bound to its
// explicit route. A follow-up that inherits the parent's route and leaves that route as the child's binding is
// a state neither order produces: every later child turn would then use the parent's model, effort and ticket.
func TestConcurrentBriefingAndFollowUp(t *testing.T) {
	const (
		rounds   = 32
		sessions = 64 // Two keys per session stay far below the default limit of 500 live entries.
		path     = "/v1/messages"
	)
	parentBriefing := autoBody("[sidequest-route model=gpt-5.6-terra effort=low ticket=SQ-PARENT]")
	childBriefing := autoBody("[sidequest-route model=gpt-5.6-sol effort=high ticket=SQ-CHILD]")
	followUp := autoBody("follow up")
	explicit := turnOutcome{Via: "dispatch", Model: "gpt-5.6-sol", Effort: "high", Ticket: "SQ-CHILD"}
	cached := turnOutcome{Via: "dispatch-cached", Model: "gpt-5.6-sol", Effort: "high", Ticket: "SQ-CHILD"}
	inherited := turnOutcome{Via: "dispatch-inherited", Model: "gpt-5.6-terra", Effort: "low", Ticket: "<none>"}
	parentBound := turnOutcome{Via: "dispatch", Model: "gpt-5.6-terra", Effort: "low", Ticket: "SQ-PARENT"}

	counts := map[string]int{}
	var violations []string
	for round := range rounds {
		h, err := proxyreq.New(proxyreq.Options{StartupDir: t.TempDir(), DispatchPath: "routes.json", Now: func() time.Time { return time.UnixMilli(1234) }})
		if err != nil {
			t.Fatal(err)
		}
		type pair struct {
			session                  string
			briefing, followUp       pendingTurn
			briefingOut, followUpOut turnOutcome
		}
		pairs := make([]pair, sessions)
		for i := range pairs {
			session := fmt.Sprintf("s%d-%d", round, i)
			parent := http.Header{"X-Claude-Code-Session-Id": {session}, "X-Claude-Code-Agent-Id": {"parent"}}
			if got := outcomeOf(newPendingTurn(t, path, parentBriefing, parent).prepare(h)); got != parentBound {
				t.Fatalf("session %s: parent briefing = %+v, want %+v", session, got, parentBound)
			}
			child := http.Header{"X-Claude-Code-Session-Id": {session}, "X-Claude-Code-Agent-Id": {"child"}, "X-Claude-Code-Parent-Agent-Id": {"parent"}}
			pairs[i] = pair{session: session, briefing: newPendingTurn(t, path, childBriefing, child), followUp: newPendingTurn(t, path, followUp, child)}
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range pairs {
			p := &pairs[i]
			wg.Go(func() {
				<-start
				p.briefingOut = outcomeOf(p.briefing.prepare(h))
			})
			wg.Go(func() {
				<-start
				p.followUpOut = outcomeOf(p.followUp.prepare(h))
			})
		}
		close(start)
		wg.Wait()

		for _, p := range pairs {
			child := p.followUp.r.Header
			final := outcomeOf(newPendingTurn(t, path, followUp, child).prepare(h))
			switch p.followUpOut {
			case cached:
				counts["briefing first"]++
			case inherited:
				counts["follow-up first"]++
			default:
				counts["other follow-up"]++
			}
			if p.briefingOut != explicit || (p.followUpOut != cached && p.followUpOut != inherited) || final != cached {
				violations = append(violations, fmt.Sprintf("session %s: briefing %+v, concurrent follow-up %+v, next follow-up %+v", p.session, p.briefingOut, p.followUpOut, final))
			}
		}
	}
	t.Logf("concurrent follow-up outcomes over %d pairs: %v", rounds*sessions, counts)
	if len(violations) > 0 {
		t.Errorf("%d of %d concurrent briefing and follow-up pairs ended in a state no serial order produces; want briefing %+v, follow-up %+v or %+v, next follow-up %+v. First cases:\n%s",
			len(violations), rounds*sessions, explicit, cached, inherited, cached, strings.Join(violations[:min(len(violations), 5)], "\n"))
	}
}
