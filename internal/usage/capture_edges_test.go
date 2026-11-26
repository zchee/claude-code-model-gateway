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

package usage_test

import (
	"bytes"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
)

type countingRandom struct {
	next byte
	read int
}

func (r *countingRandom) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.next
		r.next++
	}
	r.read += len(p)
	return len(p), nil
}

func decodeWire(t *testing.T, raw string) jsonx.WireValue {
	t.Helper()
	v, err := jsonx.DecodeWire([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeWire(%q): %v", raw, err)
	}
	return v
}

func newTestOptions(t *testing.T) usage.Options {
	t.Helper()
	return usage.Options{
		ProjectsDir: t.TempDir(),
		Now:         func() time.Time { return time.Unix(123, 0) },
		Random:      &countingRandom{},
	}
}

func callFinishGuarded(c *usage.Capture) (rec *usage.Record, panicked any) {
	defer func() { panicked = recover() }()
	return c.Finish(), nil
}

func findEvents(records []usage.Record, name string) []usage.Record {
	var out []usage.Record
	for _, r := range records {
		if r.EventName == name {
			out = append(out, r)
		}
	}
	return out
}

func TestCaptureFinishCallbackRecovery(t *testing.T) {
	tests := map[string]struct {
		status    int
		headers   http.Header
		body      string
		reentrant bool
		panicEmit bool
		wantEvent string
		wantAttr  string
		wantVal   any
	}{
		"success: panicking emit returns token record once": {
			status:    http.StatusOK,
			body:      `{"id":"msg-panic","usage":{"input_tokens":12,"output_tokens":3}}`,
			panicEmit: true,
			wantEvent: "gateway.token.usage",
			wantAttr:  "input_tokens",
			wantVal:   float64(12),
		},
		"success: panicking emit returns limit signal once": {
			status:    http.StatusTooManyRequests,
			headers:   http.Header{"Retry-After": {"5"}},
			panicEmit: true,
			wantEvent: "gateway.limit.signal",
			wantAttr:  "retry_after_ms",
			wantVal:   float64(5000),
		},
		"success: reentrant Finish inside emit emits once": {
			status:    http.StatusOK,
			body:      `{"id":"msg-reentrant","usage":{"input_tokens":18,"output_tokens":4}}`,
			reentrant: true,
			wantEvent: "gateway.token.usage",
			wantAttr:  "input_tokens",
			wantVal:   float64(18),
		},
		"success: reentrant Finish inside panicking emit emits once": {
			status:    http.StatusOK,
			body:      `{"id":"msg-reentrant-panic","usage":{"input_tokens":21,"output_tokens":6}}`,
			reentrant: true,
			panicEmit: true,
			wantEvent: "gateway.token.usage",
			wantAttr:  "input_tokens",
			wantVal:   float64(21),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var emitted usage.Record
			var nested *usage.Record
			var c *usage.Capture
			emit := func(r usage.Record) {
				calls.Add(1)
				emitted = r
				if tt.reentrant {
					nested = c.Finish()
				}
				if tt.panicEmit {
					panic("callback failure")
				}
			}
			c = usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 0, emit)
			c.SetResponse(tt.status, tt.headers)
			if tt.body != "" {
				c.ObserveJSON([]byte(tt.body))
			}

			first, panicked := callFinishGuarded(c)
			if panicked != nil {
				t.Errorf("first Finish panicked: %v", panicked)
			}
			if first == nil {
				t.Errorf("first Finish returned nil, want %q record", tt.wantEvent)
			} else {
				if diff := gocmp.Diff(tt.wantEvent, first.EventName); diff != "" {
					t.Errorf("EventName (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.wantVal, first.Attributes[tt.wantAttr]); diff != "" {
					t.Errorf("Attributes[%q] (-want +got):\n%s", tt.wantAttr, diff)
				}
				if diff := gocmp.Diff(*first, emitted, gocmp.AllowUnexported(usage.Record{})); diff != "" {
					t.Errorf("emitted record (-returned +emitted):\n%s", diff)
				}
			}
			if tt.reentrant && nested != nil {
				t.Errorf("reentrant Finish returned %+v, want nil", nested)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("emit call count after first Finish = %d, want 1", got)
			}

			second, secondPanic := callFinishGuarded(c)
			if secondPanic != nil {
				t.Errorf("second Finish panicked: %v", secondPanic)
			}
			if second != nil {
				t.Errorf("second Finish returned %+v, want nil", second)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("emit call count after second Finish = %d, want 1", got)
			}
		})
	}
}

func TestCaptureFinishConcurrent(t *testing.T) {
	tests := map[string]struct {
		workers   int
		panicEmit bool
	}{
		"success: concurrent Finish calls emit once":                    {workers: 16, panicEmit: false},
		"success: concurrent Finish calls with panicking emit run once": {workers: 16, panicEmit: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			var emitted usage.Record
			emit := func(r usage.Record) {
				calls.Add(1)
				emitted = r
				if tt.panicEmit {
					panic("concurrent callback failure")
				}
			}
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 0, emit)
			c.SetResponse(http.StatusOK, nil)
			c.ObserveJSON([]byte(`{"id":"msg-concurrent","usage":{"input_tokens":44,"output_tokens":9}}`))

			results := make([]*usage.Record, tt.workers)
			panics := make([]any, tt.workers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range tt.workers {
				wg.Go(func() {
					<-start
					results[i], panics[i] = callFinishGuarded(c)
				})
			}
			close(start)
			wg.Wait()

			var returned []*usage.Record
			for i := range tt.workers {
				if panics[i] != nil {
					t.Errorf("worker %d Finish panicked: %v", i, panics[i])
				}
				if results[i] != nil {
					returned = append(returned, results[i])
				}
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("emit call count = %d, want 1", got)
			}
			if len(returned) != 1 {
				t.Fatalf("non-nil Finish results = %d, want 1", len(returned))
			}
			if diff := gocmp.Diff(*returned[0], emitted, gocmp.AllowUnexported(usage.Record{})); diff != "" {
				t.Errorf("emitted record (-returned +emitted):\n%s", diff)
			}
			if again, p := callFinishGuarded(c); p != nil || again != nil {
				t.Errorf("trailing Finish = (%+v, panic=%v), want (nil, nil)", again, p)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("emit call count after trailing Finish = %d, want 1", got)
			}
		})
	}
}

func TestStandaloneCaptureBuilderIsolation(t *testing.T) {
	const (
		toolBaseMessages = `{"role":"user","content":"run the tool"},{"role":"assistant","content":[{"type":"tool_use","id":"tool-exact","name":"mcp__sidequest__claim","input":{}}]}`
		toolBaseBody     = `{"messages":[` + toolBaseMessages + `]}`
		toolNextBody     = `{"messages":[` + toolBaseMessages + `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-exact","content":"private exact result"}]}]}`
	)
	tests := map[string]struct {
		captureBody     string
		captureStatus   int
		captureHeaders  http.Header
		captureUsage    string
		captureComplete bool
		wantCapture     string
	}{
		"success: emitted standalone capture with tool result preserves sequence and baseline": {
			captureBody:     toolNextBody,
			captureStatus:   http.StatusOK,
			captureUsage:    `{"usage":{"input_tokens":137,"output_tokens":1}}`,
			captureComplete: true,
			wantCapture:     "gateway.token.usage",
		},
		"success: emitted standalone capture with higher baseline tokens does not shift delta": {
			captureBody:     toolBaseBody,
			captureStatus:   http.StatusOK,
			captureUsage:    `{"usage":{"input_tokens":120,"output_tokens":1}}`,
			captureComplete: true,
			wantCapture:     "gateway.token.usage",
		},
		"success: suppressed standalone capture preserves builder sequence": {
			captureBody:     toolNextBody,
			captureStatus:   http.StatusOK,
			captureUsage:    `{"usage":{"input_tokens":137,"output_tokens":1}}`,
			captureComplete: false,
			wantCapture:     "",
		},
		"success: limit-signal standalone capture reports null sequence and preserves builder sequence": {
			captureBody:     toolBaseBody,
			captureStatus:   http.StatusTooManyRequests,
			captureHeaders:  http.Header{"Retry-After": {"2"}},
			captureComplete: true,
			wantCapture:     "gateway.limit.signal",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			b := usage.NewBuilder(newTestOptions(t))
			headers := http.Header{
				"X-Claude-Code-Session-Id": {"session-isolation"},
				"X-Claude-Code-Agent-Id":   {"agent-isolation"},
			}

			firstBuild := b.Build(usage.Request{
				Body:       decodeWire(t, toolBaseBody),
				BodyBytes:  int64(len(toolBaseBody)),
				Headers:    headers.Clone(),
				StatusCode: http.StatusOK,
				Usage:      map[string]any{"input_tokens": 100, "output_tokens": 1},
			})
			firstPrimary := findEvents(firstBuild, "gateway.token.usage")
			if len(firstPrimary) != 1 {
				t.Fatalf("first Build primary records = %d, want 1", len(firstPrimary))
			}
			if diff := gocmp.Diff(any(uint64(1)), firstPrimary[0].Attributes["request_sequence"]); diff != "" {
				t.Errorf("first Build request_sequence (-want +got):\n%s", diff)
			}

			var emitted []usage.Record
			c := usage.NewCapture(b, usage.Request{
				Body:      decodeWire(t, tt.captureBody),
				BodyBytes: int64(len(tt.captureBody)),
				Headers:   headers.Clone(),
			}, 0, func(r usage.Record) { emitted = append(emitted, r) })
			c.SetResponse(tt.captureStatus, tt.captureHeaders)
			if tt.captureUsage != "" {
				c.ObserveJSON([]byte(tt.captureUsage))
			}
			c.SetResponseComplete(tt.captureComplete)
			gotCapture := c.Finish()
			if tt.wantCapture == "" {
				if gotCapture != nil || len(emitted) != 0 {
					t.Errorf("suppressed capture returned %+v (emitted %d), want nil and 0", gotCapture, len(emitted))
				}
			} else {
				if gotCapture == nil {
					t.Fatalf("standalone capture returned nil, want %q", tt.wantCapture)
				}
				if diff := gocmp.Diff(tt.wantCapture, gotCapture.EventName); diff != "" {
					t.Errorf("standalone EventName (-want +got):\n%s", diff)
				}
				seqVal, hasSeq := gotCapture.Attributes["request_sequence"]
				if !hasSeq || seqVal != nil {
					t.Errorf("standalone request_sequence = (%v, present=%t), want (nil, true)", seqVal, hasSeq)
				}
				if len(emitted) != 1 {
					t.Errorf("standalone emitted %d records, want 1", len(emitted))
				}
			}

			secondBuild := b.Build(usage.Request{
				Body:       decodeWire(t, toolNextBody),
				BodyBytes:  int64(len(toolNextBody)),
				Headers:    headers.Clone(),
				StatusCode: http.StatusOK,
				Usage:      map[string]any{"input_tokens": 137, "output_tokens": 1},
			})
			secondPrimary := findEvents(secondBuild, "gateway.token.usage")
			if len(secondPrimary) != 1 {
				t.Fatalf("second Build primary records = %d, want 1", len(secondPrimary))
			}
			if diff := gocmp.Diff(any(uint64(2)), secondPrimary[0].Attributes["request_sequence"]); diff != "" {
				t.Errorf("second Build request_sequence (-want +got):\n%s", diff)
			}

			toolRecords := findEvents(secondBuild, "gateway.tool_result.usage")
			if len(toolRecords) != 1 {
				t.Fatalf("second Build tool_result records = %d, want 1", len(toolRecords))
			}
			for attr, want := range map[string]any{
				"tool_use_id":                "tool-exact",
				"tool_name":                  "mcp__sidequest__claim",
				"tool_result_tokens":         float64(37),
				"tool_result_tokens_quality": "derived_exact",
				"request_sequence":           uint64(2),
			} {
				if diff := gocmp.Diff(want, toolRecords[0].Attributes[attr]); diff != "" {
					t.Errorf("tool_result Attributes[%q] (-want +got):\n%s", attr, diff)
				}
			}
		})
	}
}

func TestStandaloneCaptureDerivesNoFootprints(t *testing.T) {
	const (
		nativeOnlyBody = `{"tools":[{"name":"Read","description":"native schema"}],"messages":[{"role":"user","content":"hello"}]}`
		mcpToolsBody   = `{"tools":[{"name":"mcp__alpha__one","description":"alpha schema"},{"name":"mcp__beta__two","description":"beta schema"}],"messages":[{"role":"user","content":"hello"}]}`
	)
	tests := map[string]struct {
		body        string
		wantServers []string
	}{
		"success: standalone capture derives no MCP footprint records": {
			body:        mcpToolsBody,
			wantServers: []string{"input_mcp_tools_alpha_tokens", "input_mcp_tools_beta_tokens"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Verify the same body derives footprints on direct Build.
			direct := usage.NewBuilder(newTestOptions(t)).Build(usage.Request{
				Body:            decodeWire(t, tt.body),
				StatusCode:      http.StatusOK,
				ResponseHeaders: http.Header{"Request-Id": {"provider-direct"}},
				Usage:           map[string]any{"input_tokens": 100, "output_tokens": 5},
			})
			if got := len(findEvents(direct, "gateway.mcp.footprint")); got != len(tt.wantServers) {
				t.Fatalf("direct Build footprint records = %d, want %d", got, len(tt.wantServers))
			}

			runCapture := func(body string) (*usage.Record, []usage.Record, int) {
				rng := &countingRandom{}
				opts := newTestOptions(t)
				opts.Random = rng
				var emitted []usage.Record
				c := usage.NewCapture(usage.NewBuilder(opts), usage.Request{Body: decodeWire(t, body)}, 0, func(r usage.Record) { emitted = append(emitted, r) })
				c.SetResponse(http.StatusOK, http.Header{"Request-Id": {"provider-capture"}})
				c.ObserveJSON([]byte(`{"usage":{"input_tokens":100,"output_tokens":5}}`))
				return c.Finish(), emitted, rng.read
			}

			nativeRec, nativeEmitted, nativeRandomBytes := runCapture(nativeOnlyBody)
			if nativeRec == nil || len(nativeEmitted) != 1 {
				t.Fatalf("native capture got (%+v, %d emitted), want 1 record", nativeRec, len(nativeEmitted))
			}
			mcpRec, mcpEmitted, mcpRandomBytes := runCapture(tt.body)
			if mcpRec == nil || len(mcpEmitted) != 1 {
				t.Fatalf("MCP capture got (%+v, %d emitted), want 1 record", mcpRec, len(mcpEmitted))
			}
			if len(findEvents(mcpEmitted, "gateway.mcp.footprint")) != 0 {
				t.Error("standalone capture emitted gateway.mcp.footprint record")
			}
			for _, attr := range tt.wantServers {
				if v, ok := mcpRec.Attributes[attr].(float64); !ok || v <= 0 {
					t.Errorf("primary record %q = %v, want positive server token attribution", attr, mcpRec.Attributes[attr])
				}
			}
			if diff := gocmp.Diff(nativeRandomBytes, mcpRandomBytes); diff != "" {
				t.Errorf("random bytes consumed by standalone capture (-nativeOnly +withMCP):\n%s", diff)
			}
		})
	}
}

func TestCaptureBodyImmutability(t *testing.T) {
	const requestJSON = `{"system":"short system","tools":[{"name":"Read","description":"native"},{"name":"mcp__alpha__claim","description":"alpha tool"}],"messages":[{"role":"user","content":"first msg"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"res"}]}]}`
	tests := map[string]struct {
		bodyBytes int64
	}{
		"success: explicit body bytes and composition survive body mutation": {bodyBytes: int64(len(requestJSON))},
		"success: measured compact body size survives body mutation":         {bodyBytes: -1},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			refBody := decodeWire(t, requestJSON)
			mutBody := decodeWire(t, requestJSON)

			refCap := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: refBody, BodyBytes: tt.bodyBytes}, 0, nil)
			mutCap := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: mutBody, BodyBytes: tt.bodyBytes}, 0, nil)

			// Mutate the caller's decoded WireValue backing slices in place after NewCapture.
			for i := range mutBody.Members {
				switch string(utf16.Decode(mutBody.Members[i].Name)) {
				case "system":
					mutBody.Members[i].Value.Text = utf16.Encode([]rune("mutated much longer system prompt content that shifts all byte weights"))
				case "tools":
					for j := range mutBody.Members[i].Value.Elements {
						el := &mutBody.Members[i].Value.Elements[j]
						for k := range el.Members {
							switch string(utf16.Decode(el.Members[k].Name)) {
							case "name":
								el.Members[k].Value.Text = utf16.Encode([]rune("mcp__mutated__tool"))
							case "description":
								el.Members[k].Value.Text = utf16.Encode([]rune("mutated description that dramatically changes tool weight"))
							}
						}
					}
				case "messages":
					for j := range mutBody.Members[i].Value.Elements {
						el := &mutBody.Members[i].Value.Elements[j]
						for k := range el.Members {
							if string(utf16.Decode(el.Members[k].Name)) == "content" {
								el.Members[k].Value = jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune("mutated flat content replacing tool result"))}
							}
						}
					}
				}
			}

			for _, c := range []*usage.Capture{refCap, mutCap} {
				c.SetResponse(http.StatusOK, nil)
				c.ObserveJSON([]byte(`{"usage":{"input_tokens":500,"output_tokens":10}}`))
			}
			refRec := refCap.Finish()
			mutRec := mutCap.Finish()
			if refRec == nil || mutRec == nil {
				t.Fatalf("got records (%+v, %+v), want both non-nil", refRec, mutRec)
			}
			for _, attr := range []string{
				"request_body_bytes",
				"input_system_bytes",
				"input_tools_bytes",
				"input_native_tools_bytes",
				"input_mcp_tools_bytes",
				"input_messages_bytes",
				"input_first_message_bytes",
				"input_history_bytes",
				"input_tool_results_bytes",
				"input_tools_tokens",
				"input_native_tools_tokens",
				"input_mcp_tools_tokens",
				"input_mcp_tools_alpha_tokens",
				"input_system_tokens",
				"input_first_message_tokens",
				"input_history_tokens",
				"input_tool_results_tokens",
			} {
				if diff := gocmp.Diff(refRec.Attributes[attr], mutRec.Attributes[attr]); diff != "" {
					t.Errorf("Attributes[%q] (-unmutated +mutated):\n%s", attr, diff)
				}
			}
			if _, leaked := mutRec.Attributes["input_mcp_tools_mutated_tokens"]; leaked {
				t.Error("mutated MCP server name appeared in finished record")
			}
		})
	}
}

func TestCaptureTraceImmutability(t *testing.T) {
	const (
		origTrace   = "00-0123456789abcdef0123456789abcdef-1122334455667788-01"
		origTraceID = "0123456789abcdef0123456789abcdef"
		otherTrace  = "00-fedcba9876543210fedcba9876543210-8877665544332211-01"
	)
	tests := map[string]struct {
		initial     http.Header
		mutate      func(http.Header)
		wantTraceID string
		wantSpan    bool
	}{
		"success: replacing traceparent header after NewCapture preserves initial trace": {
			initial:     http.Header{"Traceparent": {origTrace}},
			mutate:      func(h http.Header) { h.Set("Traceparent", otherTrace) },
			wantTraceID: origTraceID,
			wantSpan:    true,
		},
		"success: deleting traceparent header after NewCapture preserves initial trace": {
			initial:     http.Header{"Traceparent": {origTrace}},
			mutate:      func(h http.Header) { h.Del("Traceparent") },
			wantTraceID: origTraceID,
			wantSpan:    true,
		},
		"success: mutating traceparent slice in place after NewCapture preserves initial trace": {
			initial:     http.Header{"Traceparent": {origTrace}},
			mutate:      func(h http.Header) { h["Traceparent"][0] = otherTrace },
			wantTraceID: origTraceID,
			wantSpan:    true,
		},
		"success: adding traceparent header after NewCapture does not attach a trace": {
			initial:     http.Header{},
			mutate:      func(h http.Header) { h.Set("Traceparent", otherTrace) },
			wantTraceID: "",
			wantSpan:    false,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			headers := tt.initial.Clone()
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`), Headers: headers}, 0, nil)
			tt.mutate(headers)
			c.SetResponse(http.StatusOK, nil)
			c.ObserveJSON([]byte(`{"usage":{"input_tokens":10,"output_tokens":2}}`))
			r := c.Finish()
			if r == nil {
				t.Fatal("Finish returned nil")
			}
			if diff := gocmp.Diff(tt.wantTraceID, r.TraceID); diff != "" {
				t.Errorf("TraceID (-want +got):\n%s", diff)
			}
			if gotSpan := r.SpanID != ""; gotSpan != tt.wantSpan {
				t.Errorf("SpanID=%q (present=%t), want present=%t", r.SpanID, gotSpan, tt.wantSpan)
			}
		})
	}
}

func TestCaptureSequenceImmutability(t *testing.T) {
	tests := map[string]struct {
		initial uint64
		mutated uint64
	}{
		"success: overwriting pointed-to sequence after NewCapture preserves initial value":  {initial: 7, mutated: 99},
		"success: overwriting pointed-to sequence past safe integer preserves initial value": {initial: 11, mutated: math.MaxUint64},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			seq := tt.initial
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`), Sequence: &seq}, 0, nil)
			seq = tt.mutated
			c.SetResponse(http.StatusOK, nil)
			c.ObserveJSON([]byte(`{"usage":{"input_tokens":10,"output_tokens":2}}`))
			r := c.Finish()
			if r == nil {
				t.Fatal("Finish returned nil")
			}
			if diff := gocmp.Diff(any(tt.initial), r.Attributes["request_sequence"]); diff != "" {
				t.Errorf("request_sequence (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCaptureSetResponseHeaderIsolation(t *testing.T) {
	tests := map[string]struct {
		status  int
		body    string
		initial http.Header
		mutate  func(http.Header)
		want    map[string]any
		absent  []string
	}{
		"success: mutating response headers after SetResponse preserves limit evidence": {
			status: http.StatusTooManyRequests,
			initial: http.Header{
				"Request-Id": {"provider-orig"},
				"Anthropic-Ratelimit-Input-Tokens-Remaining": {"500"},
				"Retry-After":                  {"2"},
				"X-Codex-Primary-Used-Percent": {"42"},
			},
			mutate: func(h http.Header) {
				h.Set("Request-Id", "provider-mutated")
				h.Set("Anthropic-Ratelimit-Input-Tokens-Remaining", "9999")
				h.Del("Retry-After")
				h.Set("X-Codex-Primary-Used-Percent", "99")
				h.Set("Anthropic-Ratelimit-Requests-Limit", "10")
			},
			want: map[string]any{
				"request_id":                        "provider-orig",
				"rate_limit_input_tokens_remaining": float64(500),
				"retry_after_ms":                    float64(2000),
				"codex_throttle_used_percent":       float64(42),
			},
			absent: []string{"rate_limit_requests_limit"},
		},
		"success: mutating response header slice in place after SetResponse preserves limit evidence": {
			status: http.StatusOK,
			body:   `{"usage":{"input_tokens":10,"output_tokens":2}}`,
			initial: http.Header{
				"Request-Id": {"provider-orig"},
				"Anthropic-Ratelimit-Input-Tokens-Remaining": {"250"},
				"Retry-After": {"4"},
			},
			mutate: func(h http.Header) {
				h["Request-Id"][0] = "provider-mutated"
				h["Anthropic-Ratelimit-Input-Tokens-Remaining"][0] = "1"
				h["Retry-After"][0] = "99"
			},
			want: map[string]any{
				"request_id":                        "provider-orig",
				"rate_limit_input_tokens_remaining": float64(250),
				"retry_after_ms":                    float64(4000),
			},
		},
		"success: clearing response headers after SetResponse still emits limit signal": {
			status:  http.StatusTooManyRequests,
			initial: http.Header{"Retry-After": {"3"}},
			mutate:  func(h http.Header) { clear(h) },
			want:    map[string]any{"retry_after_ms": float64(3000)},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			headers := tt.initial.Clone()
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 0, nil)
			c.SetResponse(tt.status, headers)
			tt.mutate(headers)
			if tt.body != "" {
				c.ObserveJSON([]byte(tt.body))
			}
			r := c.Finish()
			if r == nil {
				t.Fatal("Finish returned nil")
			}
			for attr, want := range tt.want {
				if diff := gocmp.Diff(want, r.Attributes[attr]); diff != "" {
					t.Errorf("Attributes[%q] (-want +got):\n%s", attr, diff)
				}
			}
			for _, attr := range tt.absent {
				if v, ok := r.Attributes[attr]; ok {
					t.Errorf("unexpected mutated attribute %q = %v", attr, v)
				}
			}
		})
	}
}

func TestCaptureSetResponseHeaderConcurrentMutation(t *testing.T) {
	tests := map[string]struct {
		mutate func(http.Header)
	}{
		"success: concurrent map mutation after SetResponse does not race or alter limit evidence": {
			mutate: func(h http.Header) {
				h.Set("Request-Id", "provider-mutated")
				h.Set("Anthropic-Ratelimit-Input-Tokens-Remaining", "9999")
				h.Del("Retry-After")
				h.Set("X-Codex-Primary-Used-Percent", "99")
			},
		},
		"success: concurrent slice mutation after SetResponse does not race or alter limit evidence": {
			mutate: func(h http.Header) {
				h["Request-Id"][0] = "provider-mutated"
				h["Anthropic-Ratelimit-Input-Tokens-Remaining"][0] = "9999"
				h["Retry-After"][0] = "99"
				h["X-Codex-Primary-Used-Percent"][0] = "99"
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			headers := http.Header{
				"Request-Id": {"provider-orig"},
				"Anthropic-Ratelimit-Input-Tokens-Remaining": {"123"},
				"Retry-After":                  {"5"},
				"X-Codex-Primary-Used-Percent": {"55"},
			}
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 0, nil)
			c.SetResponse(http.StatusTooManyRequests, headers)

			var wg sync.WaitGroup
			wg.Go(func() { tt.mutate(headers) })
			r := c.Finish()
			wg.Wait()

			if r == nil {
				t.Fatal("Finish returned nil")
			}
			for attr, want := range map[string]any{
				"request_id":                        "provider-orig",
				"rate_limit_input_tokens_remaining": float64(123),
				"retry_after_ms":                    float64(5000),
				"codex_throttle_used_percent":       float64(55),
			} {
				if diff := gocmp.Diff(want, r.Attributes[attr]); diff != "" {
					t.Errorf("Attributes[%q] (-want +got):\n%s", attr, diff)
				}
			}
		})
	}
}

// TestCaptureJSONPrimitiveCoercionFailure checks parity with lib/usage-observability.js lines
// 172-175 (numeric), 535-547 (numericTree), 743-748 (observeJson), and 770-775 (finishJsonChunks),
// where converting a JSON object with an own "toString" member to a number throws TypeError and
// the surrounding catch drops the entire JSON observation so usage and mode remain unchanged.
func TestCaptureJSONPrimitiveCoercionFailure(t *testing.T) {
	tests := map[string]struct {
		badDoc      string
		controlDoc  string
		chunked     bool
		priorSSE    bool
		status      int
		headers     http.Header
		wantBadRec  bool
		wantBadIn   any
		wantBadMode any
		wantCtlIn   any
		wantCtlMode any
	}{
		"error: ObserveJSON drops usage when sibling object has null toString": {
			badDoc:      `{"usage":{"input_tokens":15,"extra":{"toString":null}}}`,
			controlDoc:  `{"usage":{"input_tokens":15,"extra":{}}}`,
			status:      http.StatusOK,
			wantBadRec:  false,
			wantBadIn:   nil,
			wantCtlIn:   float64(15),
			wantCtlMode: "json",
		},
		"error: ObserveJSON drops usage when sibling object has string toString": {
			badDoc:      `{"usage":{"input_tokens":15,"extra":{"toString":"1"}}}`,
			controlDoc:  `{"usage":{"input_tokens":15,"extra":{}}}`,
			status:      http.StatusOK,
			wantBadRec:  false,
			wantBadIn:   nil,
			wantCtlIn:   float64(15),
			wantCtlMode: "json",
		},
		"error: ObserveJSON with throwing usage leaves response mode unset on limit signal": {
			badDoc:      `{"usage":{"input_tokens":15,"extra":{"toString":null}}}`,
			controlDoc:  `{"usage":{"input_tokens":15,"extra":{}}}`,
			status:      http.StatusTooManyRequests,
			headers:     http.Header{"Retry-After": {"2"}},
			wantBadRec:  true,
			wantBadIn:   nil,
			wantBadMode: nil,
			wantCtlIn:   float64(15),
			wantCtlMode: "json",
		},
		"error: ObserveJSON with throwing usage preserves prior usage and SSE mode": {
			badDoc:      `{"usage":{"input_tokens":15,"extra":{"toString":null}}}`,
			controlDoc:  `{"usage":{"input_tokens":15,"extra":{}}}`,
			priorSSE:    true,
			status:      http.StatusOK,
			wantBadRec:  true,
			wantBadIn:   float64(7),
			wantBadMode: "sse",
			wantCtlIn:   float64(15),
			wantCtlMode: "sse",
		},
		"error: ObserveChunk drops buffered JSON when sibling object has own toString": {
			badDoc:      `{"usage":{"input_tokens":15,"extra":{"toString":null}}}`,
			controlDoc:  `{"usage":{"input_tokens":15,"extra":{}}}`,
			chunked:     true,
			status:      http.StatusOK,
			wantBadRec:  false,
			wantBadIn:   nil,
			wantCtlIn:   float64(15),
			wantCtlMode: "json",
		},
		"error: ObserveChunk with throwing usage leaves response mode unset on limit signal": {
			badDoc:      `{"usage":{"input_tokens":15,"extra":{"toString":null}}}`,
			controlDoc:  `{"usage":{"input_tokens":15,"extra":{}}}`,
			chunked:     true,
			status:      http.StatusTooManyRequests,
			headers:     http.Header{"Retry-After": {"2"}},
			wantBadRec:  true,
			wantBadIn:   nil,
			wantBadMode: nil,
			wantCtlIn:   float64(15),
			wantCtlMode: "json",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			run := func(doc string) (snap map[string]any, rec *usage.Record) {
				c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 0, nil)
				c.SetResponse(tt.status, tt.headers.Clone())
				if tt.priorSSE {
					c.ObserveEvent(decodeWire(t, `{"type":"message_start","message":{"usage":{"input_tokens":7}}}`))
				}
				if tt.chunked {
					mid := len(doc) / 2
					c.ObserveChunk([]byte(doc[:mid]))
					c.ObserveChunk([]byte(doc[mid:]))
				} else {
					c.ObserveJSON([]byte(doc))
				}
				snap = c.Snapshot()
				rec = c.Finish()
				return snap, rec
			}

			badSnap, badRec := run(tt.badDoc)
			if !tt.chunked {
				if diff := gocmp.Diff(tt.wantBadIn, badSnap["input_tokens"]); diff != "" {
					t.Errorf("bad document Snapshot input_tokens (-want +got):\n%s", diff)
				}
			}
			if !tt.wantBadRec {
				if badRec != nil {
					t.Errorf("bad document Finish returned %+v, want nil", badRec)
				}
			} else {
				if badRec == nil {
					t.Fatal("bad document Finish returned nil, want record")
				}
				if diff := gocmp.Diff(tt.wantBadMode, badRec.Attributes["response_mode"]); diff != "" {
					t.Errorf("bad document response_mode (-want +got):\n%s", diff)
				}
				if tt.status == http.StatusOK {
					if diff := gocmp.Diff(tt.wantBadIn, badRec.Attributes["input_tokens"]); diff != "" {
						t.Errorf("bad document input_tokens (-want +got):\n%s", diff)
					}
				}
			}

			ctlSnap, ctlRec := run(tt.controlDoc)
			if !tt.chunked {
				if diff := gocmp.Diff(tt.wantCtlIn, ctlSnap["input_tokens"]); diff != "" {
					t.Errorf("control document Snapshot input_tokens (-want +got):\n%s", diff)
				}
			}
			if ctlRec == nil {
				t.Fatal("control document Finish returned nil, want record")
			}
			if diff := gocmp.Diff(tt.wantCtlMode, ctlRec.Attributes["response_mode"]); diff != "" {
				t.Errorf("control document response_mode (-want +got):\n%s", diff)
			}
			if tt.status == http.StatusOK {
				if diff := gocmp.Diff(tt.wantCtlIn, ctlRec.Attributes["input_tokens"]); diff != "" {
					t.Errorf("control document input_tokens (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestCaptureEmptyChunks(t *testing.T) {
	paddedCeiling := append([]byte(`{"usage":{"input_tokens":19,"output_tokens":4}}`), bytes.Repeat([]byte(" "), 1024-len(`{"usage":{"input_tokens":19,"output_tokens":4}}`))...)
	tests := map[string]struct {
		status    int
		headers   http.Header
		chunks    [][]byte
		emptyEach int
		wantEvent string
		wantMode  any
		wantBytes int64
		wantIn    any
	}{
		"success: empty chunks alone leave mode unset and byte count zero": {
			status:    http.StatusTooManyRequests,
			headers:   http.Header{"Retry-After": {"1"}},
			emptyEach: 8,
			wantEvent: "gateway.limit.signal",
			wantMode:  nil,
			wantBytes: 0,
		},
		"success: repeated empty chunks do not consume byte budget at ceiling": {
			status:    http.StatusOK,
			chunks:    [][]byte{paddedCeiling[:512], paddedCeiling[512:]},
			emptyEach: 1500,
			wantEvent: "gateway.token.usage",
			wantMode:  "json",
			wantBytes: 1024,
			wantIn:    float64(19),
		},
		"success: empty chunks interleaved with JSON chunks match unpadded observation": {
			status:    http.StatusOK,
			chunks:    [][]byte{[]byte(`{"usage":{"input_`), []byte(`tokens":31,"output_tokens":6}}`)},
			emptyEach: 10,
			wantEvent: "gateway.token.usage",
			wantMode:  "json",
			wantBytes: int64(len(`{"usage":{"input_tokens":31,"output_tokens":6}}`)),
			wantIn:    float64(31),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 1024, nil)
			c.SetResponse(tt.status, tt.headers.Clone())
			for range tt.emptyEach {
				c.ObserveChunk(nil)
				c.ObserveChunk([]byte{})
			}
			for _, chunk := range tt.chunks {
				c.ObserveChunk(chunk)
				for range tt.emptyEach {
					c.ObserveChunk(nil)
					c.ObserveChunk([]byte{})
				}
			}
			r := c.Finish()
			if r == nil {
				t.Fatalf("Finish returned nil, want %q", tt.wantEvent)
			}
			if diff := gocmp.Diff(tt.wantEvent, r.EventName); diff != "" {
				t.Errorf("EventName (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantMode, r.Attributes["response_mode"]); diff != "" {
				t.Errorf("response_mode (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantBytes, r.Attributes["response_body_bytes"]); diff != "" {
				t.Errorf("response_body_bytes (-want +got):\n%s", diff)
			}
			if tt.wantIn != nil {
				if diff := gocmp.Diff(tt.wantIn, r.Attributes["input_tokens"]); diff != "" {
					t.Errorf("input_tokens (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestCaptureResponseBytesSaturation(t *testing.T) {
	tests := map[string]struct {
		drive func(c *usage.Capture)
	}{
		"success: NoteResponseBytes saturates at MaxInt64": {
			drive: func(c *usage.Capture) {
				c.SetResponse(http.StatusOK, nil)
				c.ObserveEvent(decodeWire(t, `{"type":"message_start","message":{"usage":{"input_tokens":5}}}`))
				c.NoteResponseBytes(math.MaxInt64)
				c.NoteResponseBytes(1)
				c.NoteResponseBytes(math.MaxInt64)
			},
		},
		"success: ObserveJSON byte accounting saturates at MaxInt64": {
			drive: func(c *usage.Capture) {
				c.SetResponse(http.StatusOK, nil)
				c.NoteResponseBytes(math.MaxInt64 - 5)
				c.ObserveJSON([]byte(`{"usage":{"input_tokens":5}}`))
			},
		},
		"success: ObserveJSON oversized body byte accounting saturates at MaxInt64": {
			drive: func(c *usage.Capture) {
				c.SetResponse(http.StatusTooManyRequests, http.Header{"Retry-After": {"1"}})
				c.NoteResponseBytes(math.MaxInt64 - 5)
				c.ObserveJSON(bytes.Repeat([]byte(" "), 2048))
			},
		},
		"success: ObserveChunk byte accounting saturates at MaxInt64": {
			drive: func(c *usage.Capture) {
				c.SetResponse(http.StatusOK, nil)
				c.NoteResponseBytes(math.MaxInt64 - 5)
				c.ObserveChunk([]byte(`{"usage":{"input_tokens":5}}`))
			},
		},
		"success: ObserveChunk after overflow byte accounting saturates at MaxInt64": {
			drive: func(c *usage.Capture) {
				c.SetResponse(http.StatusTooManyRequests, http.Header{"Retry-After": {"1"}})
				c.MarkOverflow()
				c.NoteResponseBytes(math.MaxInt64 - 2)
				c.ObserveChunk([]byte("overflowed-chunk"))
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 1024, nil)
			tt.drive(c)
			r := c.Finish()
			if r == nil {
				t.Fatal("Finish returned nil")
			}
			got, ok := r.Attributes["response_body_bytes"].(int64)
			if !ok {
				t.Fatalf("response_body_bytes type = %T (%v), want int64", r.Attributes["response_body_bytes"], r.Attributes["response_body_bytes"])
			}
			if got < 0 {
				t.Errorf("response_body_bytes wrapped negative: %d", got)
			}
			if diff := gocmp.Diff(int64(math.MaxInt64), got); diff != "" {
				t.Errorf("response_body_bytes (-want +got):\n%s", diff)
			}
		})
	}
}
