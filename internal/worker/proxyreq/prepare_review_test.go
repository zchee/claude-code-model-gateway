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
func TestRequestedEffortUnits(t *testing.T) {
	tests := map[string]struct {
		effort, wantBody, wantEffort string
	}{
		"success: ordinary effort":            {`"high"`, `"high"`, "high"},
		"success: paired surrogates":          {`"\ud83d\ude00"`, "\"\U0001F600\"", "\U0001F600"},
		"success: unpaired high surrogate":    {`"\ud800"`, `"\ud800"`, "�"},
		"success: unpaired low surrogate":     {`"x\udc00y"`, `"x\udc00y"`, "x�y"},
		"success: literal replacement stays":  {`"�"`, "\"�\"", "�"},
		"success: reversed surrogate pair":    {`"\ude00\ud83d"`, `"\ude00\ud83d"`, "��"},
		"success: surrogate before a letter":  {`"\ud83dz"`, `"\ud83dz"`, "�z"},
		"success: surrogate at the very end":  {`"low\udbff"`, `"low\udbff"`, "low�"},
		"success: escaped ASCII is unescaped": {`"\u0068\u0069\u0067\u0068"`, `"high"`, "high"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h, err := proxyreq.New(proxyreq.Options{Now: func() time.Time { return time.UnixMilli(1234) }})
			if err != nil {
				t.Fatal(err)
			}
			raw := `{"model":"claude-gpt-5.6-sol","output_config":{"effort":` + tc.effort + `}}`
			res, err := newPendingTurn(t, "/v1/messages", raw, nil).prepare(h)
			if err != nil || res.LocalStatus != 0 {
				t.Fatalf("error %v, local status %d", err, res.LocalStatus)
			}
			if diff := gocmp.Diff(`{"model":"gpt-5.6-sol","output_config":{"effort":`+tc.wantBody+`}}`, string(res.Body)); diff != "" {
				t.Errorf("prepared body (-want +got): %s", diff)
			}
			wantRoute := outcome.Route{SelectedModel: "claude-gpt-5.6-sol", EffectiveModel: "gpt-5.6-sol", Backend: "codex", Effort: tc.wantEffort, Via: "direct"}
			if diff := gocmp.Diff(wantRoute, res.Route); diff != "" {
				t.Errorf("route (-want +got): %s", diff)
			}
			wantAudit := &outcome.Audit{Backend: "codex", Model: new("gpt-5.6-sol"), Effort: tc.wantEffort}
			if diff := gocmp.Diff(wantAudit, res.Audit); diff != "" {
				t.Errorf("route log entry (-want +got): %s", diff)
			}
		})
	}
}

// TestKeepPlanToolsStartupSnapshot checks that the plan tools switch is read once, when the handler is built:
// changing what the lookup reports afterwards does not change how later requests are prepared.
func TestKeepPlanToolsStartupSnapshot(t *testing.T) {
	const raw = `{"model":"claude-gpt-5.6-sol","tools":[{"name":"EnterPlanMode"},{"name":"Read"}]}`
	const kept = `{"model":"gpt-5.6-sol","tools":[{"name":"EnterPlanMode"},{"name":"Read"}]}`
	const removed = `{"model":"gpt-5.6-sol","tools":[{"name":"Read"}]}`
	tests := map[string]struct {
		atStart, later *string
		want           string
	}{
		"success: switched on after startup stays off": {nil, new("1"), removed},
		"success: switched off after startup stays on": {new("1"), nil, kept},
		"success: emptied after startup stays on":      {new("1"), new(""), kept},
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
			delete(env, key)
			if tc.later != nil {
				env[key] = *tc.later
			}
			res, err := newPendingTurn(t, "/v1/messages", raw, nil).prepare(h)
			if err != nil || res.LocalStatus != 0 {
				t.Fatalf("error %v, local status %d", err, res.LocalStatus)
			}
			if diff := gocmp.Diff(tc.want, string(res.Body)); diff != "" {
				t.Errorf("prepared body (-want +got): %s", diff)
			}
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
