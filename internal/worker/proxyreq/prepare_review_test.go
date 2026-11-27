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
	"math"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
)

// TestRequestedEffortUnits pins how a requested effort reaches the prepared body and the route log. The body keeps
// the client's UTF-16 units, so an unpaired surrogate is forwarded escaped as the source forwards it, while the
// route and its log entry are Go strings and carry U+FFFD in its place. Ordinary efforts read the same in both.
// The client's effort is kept only while no dispatch replaces it: a dispatched turn whose marker names an effort
// forwards and logs that effort instead, and one whose marker names none keeps the client's units as a direct
// turn does.
func TestRequestedEffortUnits(t *testing.T) {
	tests := map[string]struct {
		// marker, when set, dispatches the turn on a briefing marker instead of selecting the model directly.
		effort, marker, wantBody, wantEffort string
	}{
		"success: ordinary effort":            {effort: `"high"`, wantBody: `"high"`, wantEffort: "high"},
		"success: paired surrogates":          {effort: `"\ud83d\ude00"`, wantBody: "\"\U0001F600\"", wantEffort: "\U0001F600"},
		"success: unpaired high surrogate":    {effort: `"\ud800"`, wantBody: `"\ud800"`, wantEffort: "�"},
		"success: unpaired low surrogate":     {effort: `"x\udc00y"`, wantBody: `"x\udc00y"`, wantEffort: "x�y"},
		"success: literal replacement stays":  {effort: `"�"`, wantBody: "\"�\"", wantEffort: "�"},
		"success: reversed surrogate pair":    {effort: `"\ude00\ud83d"`, wantBody: `"\ude00\ud83d"`, wantEffort: "��"},
		"success: surrogate before a letter":  {effort: `"\ud83dz"`, wantBody: `"\ud83dz"`, wantEffort: "�z"},
		"success: surrogate at the very end":  {effort: `"low\udbff"`, wantBody: `"low\udbff"`, wantEffort: "low�"},
		"success: escaped ASCII is unescaped": {effort: `"\u0068\u0069\u0067\u0068"`, wantBody: `"high"`, wantEffort: "high"},

		"success: dispatch effort replaces an unpaired surrogate": {effort: `"\ud800"`, marker: "[sidequest-route model=gpt-5.6-sol effort=high]", wantBody: `"high"`, wantEffort: "high"},
		"success: dispatch effort replaces an ordinary effort":    {effort: `"low"`, marker: "[sidequest-route model=gpt-5.6-sol effort=xhigh]", wantBody: `"xhigh"`, wantEffort: "xhigh"},
		"success: dispatch without effort keeps the client units": {effort: `"\ud800"`, marker: "[sidequest-route model=gpt-5.6-sol]", wantBody: `"\ud800"`, wantEffort: "�"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h, err := proxyreq.New(proxyreq.Options{Now: func() time.Time { return time.UnixMilli(1234) }})
			if err != nil {
				t.Fatal(err)
			}
			selected, messages := "claude-gpt-5.6-sol", ""
			wantRoute := outcome.Route{SelectedModel: selected, EffectiveModel: "gpt-5.6-sol", Backend: "codex", Effort: tc.wantEffort, Via: "direct"}
			wantAudit := &outcome.Audit{Backend: "codex", Model: new("gpt-5.6-sol"), Effort: tc.wantEffort}
			if tc.marker != "" {
				selected, messages = "claude-codex-auto", fmt.Sprintf(`,"messages":[{"role":"user","content":%q}]`, tc.marker)
				wantRoute.SelectedModel, wantRoute.Via = selected, "dispatch"
				wantAudit.Via, wantAudit.MarkersLength = "dispatch", new(1)
			}
			raw := `{"model":"` + selected + `"` + messages + `,"output_config":{"effort":` + tc.effort + `}}`
			res, err := newPendingTurn(t, "/v1/messages", raw, nil).prepare(h)
			if err != nil || res.LocalStatus != 0 {
				t.Fatalf("error %v, local status %d", err, res.LocalStatus)
			}
			if diff := gocmp.Diff(`{"model":"gpt-5.6-sol"`+messages+`,"output_config":{"effort":`+tc.wantBody+`}}`, string(res.Body)); diff != "" {
				t.Errorf("prepared body (-want +got): %s", diff)
			}
			if diff := gocmp.Diff(wantRoute, res.Route); diff != "" {
				t.Errorf("route (-want +got): %s", diff)
			}
			if diff := gocmp.Diff(wantAudit, res.Audit); diff != "" {
				t.Errorf("route log entry (-want +got): %s", diff)
			}
		})
	}
}

// TestKeepPlanToolsStartupSnapshot checks that the plan tools switch is read once, when the handler is built. In the
// first three cases the lookup changes after the handler is built and before its first request; in the others one
// request is prepared, the lookup changes, and a second request on the same handler is prepared exactly like the
// first. Neither request sees the changed setting.
func TestKeepPlanToolsStartupSnapshot(t *testing.T) {
	const raw = `{"model":"claude-gpt-5.6-sol","tools":[{"name":"EnterPlanMode"},{"name":"Read"}]}`
	const kept = `{"model":"gpt-5.6-sol","tools":[{"name":"EnterPlanMode"},{"name":"Read"}]}`
	const removed = `{"model":"gpt-5.6-sol","tools":[{"name":"Read"}]}`
	tests := map[string]struct {
		atStart, later *string
		want           string
		// between prepares a request before the lookup changes, so the change falls between two requests.
		between bool
	}{
		"success: switched on after startup stays off": {atStart: nil, later: new("1"), want: removed},
		"success: switched off after startup stays on": {atStart: new("1"), later: nil, want: kept},
		"success: emptied after startup stays on":      {atStart: new("1"), later: new(""), want: kept},

		"success: switched on between requests stays off": {atStart: nil, later: new("1"), want: removed, between: true},
		"success: switched off between requests stays on": {atStart: new("1"), later: nil, want: kept, between: true},
		"success: emptied between requests stays on":      {atStart: new("1"), later: new(""), want: kept, between: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			const key = "CODEX_GATEWAY_KEEP_PLAN_TOOLS"
			env := map[string]string{}
			if tc.atStart != nil {
				env[key] = *tc.atStart
			}
			h, err := proxyreq.New(proxyreq.Options{Now: func() time.Time { return time.UnixMilli(1234) }, Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
			if err != nil {
				t.Fatal(err)
			}
			check := func(when string) {
				t.Helper()
				res, err := newPendingTurn(t, "/v1/messages", raw, nil).prepare(h)
				if err != nil || res.LocalStatus != 0 {
					t.Fatalf("%s: error %v, local status %d", when, err, res.LocalStatus)
				}
				if diff := gocmp.Diff(tc.want, string(res.Body)); diff != "" {
					t.Errorf("%s: prepared body (-want +got): %s", when, diff)
				}
			}
			if tc.between {
				check("request before the change")
			}
			delete(env, key)
			if tc.later != nil {
				env[key] = *tc.later
			}
			check("request after the change")
		})
	}
}

// TestDispatchMarkerScanAllocation checks that looking for a route marker costs nothing in proportion to history
// that cannot hold one. Both requests carry a mebibyte tool result and the same marker and produce the same
// prepared body; one selects the model directly, the other dispatches on the marker. Serializing the message
// history again only to scan it would make the dispatched turn allocate at least the size of that history more.
func TestDispatchMarkerScanAllocation(t *testing.T) {
	const n = 1 << 20
	messages := fmt.Sprintf(`[{"role":"user","content":"[sidequest-route model=gpt-5.6-sol]"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%q}]}]`, strings.Repeat("a", n))
	h, err := proxyreq.New(proxyreq.Options{Now: func() time.Time { return time.UnixMilli(1234) }})
	if err != nil {
		t.Fatal(err)
	}
	allocated := func(model, via string) (uint64, []byte) {
		t.Helper()
		least := uint64(math.MaxUint64)
		var body []byte
		for range 5 {
			turn := newPendingTurn(t, "/v1/messages", fmt.Sprintf(`{"model":%q,"messages":%s}`, model, messages), http.Header{})
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			res, err := turn.prepare(h)
			runtime.ReadMemStats(&after)
			if err != nil || res.LocalStatus != 0 || res.Route.Via != via {
				t.Fatalf("%s: error %v, local status %d, route %+v", model, err, res.LocalStatus, res.Route)
			}
			least, body = min(least, after.TotalAlloc-before.TotalAlloc), res.Body
		}
		return least, body
	}
	direct, directBody := allocated("claude-gpt-5.6-sol", "direct")
	dispatched, dispatchedBody := allocated("claude-codex-auto", "dispatch")
	if diff := gocmp.Diff(string(directBody), string(dispatchedBody)); diff != "" {
		t.Fatalf("the two requests do not prepare the same body (-direct +dispatched): %s", diff)
	}
	t.Logf("bytes allocated by Prepare with a %d-character tool result: %d direct, %d dispatched", n, direct, dispatched)
	if dispatched > direct && dispatched-direct >= n/2 {
		t.Errorf("scanning for the marker cost %d more bytes than selecting the model directly; want less than %d", dispatched-direct, n/2)
	}
}
