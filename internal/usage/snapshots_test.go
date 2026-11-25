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

package usage

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

const toolEvent = "gateway.tool_result.usage"

func observe(t *testing.T, b *Builder, body, session, agent, parent, ticket string, total int) []Record {
	t.Helper()
	h := http.Header{}
	h.Set("x-claude-code-session-id", session)
	if agent != "" {
		h.Set("x-claude-code-agent-id", agent)
	}
	if parent != "" {
		h.Set("x-claude-code-parent-agent-id", parent)
	}
	records := b.Build(Request{Body: wire(t, body), BodyBytes: int64(len(body)), Headers: h, StatusCode: 200, Usage: map[string]any{"input_tokens": total, "output_tokens": 1}, TicketRef: ticket})
	onlyEvent(t, records, "gateway.token.usage")
	return records
}

const (
	exactMessages = `{"role":"user","content":"run the tool"},{"role":"assistant","content":[{"type":"tool_use","id":"tool-exact","name":"mcp__sidequest__claim","input":{}}]}`
	exactBase     = `{"messages":[` + exactMessages + `]}`
	exactNext     = `{"messages":[` + exactMessages + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-exact","content":"private exact result"}]}]}`
)

func TestToolResultAttribution(t *testing.T) {
	const multiMessages = `{"role":"assistant","content":[{"type":"tool_use","id":"tool-short","name":"Read","input":{}},{"type":"tool_use","id":"tool-long","name":"mcp__sidequest__comments","input":{}}]}`
	tests := map[string]struct {
		base, next      string
		start, end      int
		ticket, private string
		want            map[string]map[string]any
	}{
		"success: one result":       {exactBase, exactNext, 100, 137, "SQ-1234", "private exact result", map[string]map[string]any{"tool-exact": {"tool_name": "mcp__sidequest__claim", "tool_result_tokens": 37, "tool_result_tokens_unit": "tokens", "tool_result_tokens_quality": "derived_exact", "ticket_ref": "SQ-1234"}}},
		"success: weighted results": {`{"messages":[` + multiMessages + `]}`, `{"messages":[` + multiMessages + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-short","content":"x"},{"type":"tool_result","tool_use_id":"tool-long","content":"` + strings.Repeat("long private result", 8) + `"}]}]}`, 200, 219, "", "long private result", map[string]map[string]any{"tool-short": {"tool_name": "Read", "tool_result_tokens": 4, "tool_result_tokens_quality": "estimate"}, "tool-long": {"tool_name": "mcp__sidequest__comments", "tool_result_tokens": 15, "tool_result_tokens_quality": "estimate"}}},
		"success: compaction":       {base: exactBase, next: exactNext, start: 300, end: 90},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b := NewBuilder(options(t))
			rs := observe(t, b, tt.base, "session", "agent", "", tt.ticket, tt.start)
			rs = append(rs, observe(t, b, tt.next, "session", "agent", "", tt.ticket, tt.end)...)
			got := events(rs, toolEvent)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d tool result records, want %d", len(got), len(tt.want))
			}
			seen := map[string]bool{}
			for _, r := range got {
				id, _ := r.Attributes["tool_use_id"].(string)
				want, ok := tt.want[id]
				if !ok || seen[id] {
					t.Fatalf("unexpected or repeated tool use ID %q", id)
				}
				seen[id] = true
				for k, v := range want {
					t.Run(id+"/"+k, func(t *testing.T) { checkJSON(t, v, r.Attributes[k]) })
				}
			}
			if tt.private != "" && bytes.Contains(encoded(t, got), []byte(tt.private)) {
				t.Error("tool result records retained private content")
			}
		})
	}
}

func TestAgentIsolation(t *testing.T) {
	b := NewBuilder(options(t))
	var all []Record
	for _, turn := range []struct {
		agent, body string
		tokens      int
	}{{"agent-a", exactBase, 100}, {"agent-b", exactBase, 400}, {"agent-a", exactNext, 111}, {"agent-b", exactNext, 407}} {
		all = append(all, observe(t, b, turn.body, "shared", turn.agent, "", "", turn.tokens)...)
	}
	r := events(all, toolEvent)
	if len(r) != 2 {
		t.Fatalf("got %d records, want 2", len(r))
	}
	totals := map[string]any{}
	for _, v := range r {
		id, _ := v.Attributes["agent_id"].(string)
		totals[id] = v.Attributes["tool_result_tokens"]
	}
	checkJSON(t, map[string]int{"agent-a": 11, "agent-b": 7}, totals)
}

func TestSnapshotBounds(t *testing.T) {
	type turn struct {
		after                        time.Duration
		session, agent, parent, body string
		total, want                  int
	}
	tests := map[string]struct {
		max   int
		ttl   time.Duration
		turns []turn
	}{
		"success: expires exactly at idle deadline":          {2, time.Hour, []turn{{0, "s", "a", "", exactBase, 100, -1}, {time.Hour, "s", "a", "", exactNext, 140, -1}}},
		"success: refresh before expiry":                     {2, time.Hour, []turn{{0, "s", "a", "", exactBase, 100, -1}, {time.Hour - time.Millisecond, "s", "a", "", exactBase, 110, -1}, {time.Millisecond, "s", "a", "", exactNext, 140, 30}}},
		"success: least recently used identity evicted":      {2, time.Hour, []turn{{0, "s", "a", "", exactBase, 100, -1}, {0, "s", "b", "", exactBase, 10, -1}, {0, "s", "a", "", exactBase, 120, -1}, {0, "s", "c", "", exactBase, 1, -1}, {0, "s", "a", "", exactNext, 133, 13}, {0, "s", "b", "", exactNext, 20, -1}}},
		"success: parent only and explicit agent identities": {8, time.Hour, []turn{{0, "s", "", "p", exactBase, 20, -1}, {0, "s", "", "q", exactNext, 80, -1}, {0, "s", "", "p", exactNext, 32, 12}, {0, "s", "a", "p", exactBase, 50, -1}, {0, "s", "a", "q", exactNext, 57, 7}, {0, "other", "a", "q", exactNext, 80, -1}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(0, 0)
			opts := options(t)
			opts.Now = func() time.Time { return now }
			opts.SnapshotTTL = tt.ttl
			opts.MaxIdentities = tt.max
			b := NewBuilder(opts)
			for i, turn := range tt.turns {
				now = now.Add(turn.after)
				got := events(observe(t, b, turn.body, turn.session, turn.agent, turn.parent, "", turn.total), toolEvent)
				if turn.want < 0 {
					if len(got) != 0 {
						t.Fatalf("turn %d: unexpected %d derived records", i, len(got))
					}
					continue
				}
				if len(got) != 1 {
					t.Fatalf("turn %d: got %d derived records, want 1", i, len(got))
				}
				checkJSON(t, turn.want, got[0].Attributes["tool_result_tokens"])
			}
		})
	}
}
