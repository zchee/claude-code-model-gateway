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

package dispatch_test

import (
	"encoding/json/jsontext"
	"fmt"
	"os"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/dispatch"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func checkMarkers(t *testing.T, messages string, want *dispatch.Route, count int) {
	t.Helper()
	got, n := dispatch.FromMessages(jsontext.Value(messages))
	if n != count {
		t.Errorf("marker count = %d, want %d", n, count)
	}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("sole route (-want +got):\n%s", diff)
	}
}

func TestFromMessages(t *testing.T) {
	const briefing = `{"role":"user","content":"[sidequest-route model=gpt-5.6-terra] briefing"}`
	terra := &dispatch.Route{Model: "gpt-5.6-terra"}
	tests := map[string]struct {
		messages string
		want     *dispatch.Route
		count    int
	}{
		"success: no ticket":                 {`[{"role":"user","content":"[sidequest-route model=gpt-5.6-terra effort=high] work the ticket"}]`, &dispatch.Route{Model: "gpt-5.6-terra", Effort: new("high")}, 1},
		"success: with ticket":               {`[{"role":"user","content":"[sidequest-route model=gpt-5.6-terra effort=high ticket=SQ-1234] work the ticket"}]`, &dispatch.Route{Model: "gpt-5.6-terra", Effort: new("high"), Ticket: new("SQ-1234")}, 1},
		"success: text block":                {`[{"role":"user","content":[{"type":"text","text":"[sidequest-route model=gpt-5.6-sol]"}]}]`, &dispatch.Route{Model: "gpt-5.6-sol"}, 1},
		"success: ignore later tool result":  {`[` + briefing + `,{"role":"assistant","content":[{"type":"text","text":"reading the diff"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"echoed [sidequest-route model=codex-gpt-5-6-terra] fixture"}]}]`, terra, 1},
		"success: ignore nested tool result": {`[` + briefing + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"[sidequest-route model=codex-gpt-5-6-luna]"}]}]}]`, terra, 1},
		"error: assistant only":              {`[{"role":"assistant","content":[{"type":"text","text":"[sidequest-route model=gpt-5.6-sol]"}]},{"role":"assistant","content":"[sidequest-route model=gpt-5.6-luna]"}]`, nil, 0},
		"error: only echoes":                 {`[{"role":"user","content":[{"type":"tool_result","content":"[sidequest-route model=codex-gpt-5-6-terra]"}]},{"role":"assistant","content":"[sidequest-route model=gpt-5.6-sol]"}]`, nil, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { checkMarkers(t, tt.messages, tt.want, tt.count) })
	}
}

func TestAmbiguousMarkers(t *testing.T) {
	tests := map[string]struct{ messages string }{
		"error: duplicate in same string": {`[{"role":"user","content":"[sidequest-route model=gpt-5.6-sol effort=low] [sidequest-route model=gpt-5.6-sol effort=low]"}]`},
		"error: conflicting messages":     {`[{"role":"user","content":"[sidequest-route model=gpt-5.6-sol effort=low]"},{"role":"user","content":[{"type":"text","text":"[sidequest-route model=gpt-5.6-terra effort=xhigh]"}]}]`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { checkMarkers(t, tt.messages, nil, 2) })
	}
}

func TestMarkerGrammar(t *testing.T) {
	model, ticket := "0"+strings.Repeat("a", 63), "Z"+strings.Repeat("_", 63)
	tests := map[string]struct {
		text string
		want *dispatch.Route
	}{
		"success: marker inside prose":      {"prefix[sidequest-route model=0.- effort=medium]suffix", &dispatch.Route{Model: "0.-", Effort: new("medium")}},
		"success: longest model and ticket": {"[sidequest-route model=" + model + " ticket=" + ticket + "]", &dispatch.Route{Model: model, Ticket: new(ticket)}},
		"success: max effort":               {"[sidequest-route model=x effort=max]", &dispatch.Route{Model: "x", Effort: new("max")}},
		"error: long model":                 {"[sidequest-route model=" + model + "a]", nil},
		"error: long ticket":                {"[sidequest-route model=x ticket=" + ticket + "a]", nil},
		"error: uppercase model":            {"[sidequest-route model=X]", nil},
		"error: underscore in model":        {"[sidequest-route model=x_y]", nil},
		"error: punctuation starts model":   {"[sidequest-route model=-x]", nil},
		"error: non ASCII ticket":           {"[sidequest-route model=x ticket=é]", nil},
		"error: digit starts ticket":        {"[sidequest-route model=x ticket=1]", nil},
		"error: unknown effort":             {"[sidequest-route model=x effort=auto]", nil},
		"error: reordered options":          {"[sidequest-route model=x ticket=X effort=low]", nil},
		"error: extra space":                {"[sidequest-route  model=x]", nil},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			count := 0
			if tt.want != nil {
				count = 1
			}
			checkMarkers(t, fmt.Sprintf(`[{"role":"user","content":%q}]`, tt.text), tt.want, count)
		})
	}
}
