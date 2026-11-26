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
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/usage"
)

// TestCaptureRepeatedHeaderSnapshots checks that a capture reads the first value of a repeated
// request or response field, where the source reads the comma-space joined value (which is not
// a safe identifier), and that the request headers frozen at NewCapture and the response headers
// frozen at SetResponse are not changed by later edits of the caller's maps or value slices.
func TestCaptureRepeatedHeaderSnapshots(t *testing.T) {
	const (
		firstTrace   = "00-0123456789abcdef0123456789abcdef-1122334455667788-01"
		firstTraceID = "0123456789abcdef0123456789abcdef"
		secondTrace  = "00-fedcba9876543210fedcba9876543210-8877665544332211-01"
	)
	repeatedRequest := func() http.Header {
		return http.Header{
			"Traceparent":              {firstTrace, secondTrace},
			"X-Claude-Code-Session-Id": {"session-first", "session-second"},
			"X-Claude-Code-Agent-Id":   {"agent-first", "agent-second"},
			"X-Claude-Code-Request-Id": {"client-first", "client-second"},
		}
	}
	repeatedResponse := func() http.Header {
		return http.Header{
			"Request-Id":  {"provider-first", "provider-second"},
			"Retry-After": {"7", "60"},
			"Anthropic-Ratelimit-Input-Tokens-Remaining": {"11", "22"},
		}
	}
	want := map[string]any{
		"session_id":                        "session-first",
		"agent_id":                          "agent-first",
		"agent_role":                        "executor",
		"client_request_id":                 "client-first",
		"request_id":                        "provider-first",
		"request_id_source":                 "response_header",
		"retry_after_ms":                    float64(7000),
		"rate_limit_input_tokens_remaining": float64(11),
	}
	tests := map[string]struct {
		mutate func(h http.Header)
	}{
		"success: repeated fields read their first value": {
			mutate: func(http.Header) {},
		},
		"success: replacing first values in place after the snapshot": {
			mutate: func(h http.Header) {
				for _, values := range h {
					values[0] = "mutated-first"
				}
			},
		},
		"success: deleting and re-adding repeated fields in reverse order after the snapshot": {
			mutate: func(h http.Header) {
				for _, name := range slices.Sorted(maps.Keys(h)) {
					values := h[name]
					h.Del(name)
					for _, v := range slices.Backward(values) {
						h.Add(name, "reordered-"+v)
					}
				}
			},
		},
		"success: appending further values after the snapshot": {
			mutate: func(h http.Header) {
				for name := range h {
					h[name] = append(h[name], "appended-value")
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			reqHeaders, resHeaders := repeatedRequest(), repeatedResponse()
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`), Headers: reqHeaders}, 0, nil)
			tt.mutate(reqHeaders)
			c.SetResponse(http.StatusOK, resHeaders)
			tt.mutate(resHeaders)
			c.ObserveJSON([]byte(`{"usage":{"input_tokens":10,"output_tokens":2}}`))
			r := c.Finish()
			if r == nil {
				t.Fatal("Finish returned nil, want a gateway.token.usage record")
			}
			got := map[string]any{}
			for key := range want {
				got[key] = r.Attributes[key]
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("record attributes (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(firstTraceID, r.TraceID); diff != "" {
				t.Errorf("TraceID from the first traceparent value (-want +got):\n%s", diff)
			}
			if r.SpanID == "" {
				t.Error("SpanID is empty, want a new span for the first traceparent value")
			}
		})
	}
}

// TestCaptureStreamUncoercibleUsage checks that an SSE usage object holding a member whose
// numeric conversion throws in the source (an object with its own toString) keeps its usable
// numeric siblings and does not stop later events from merging, while buffered JSON with the
// same usage still drops the whole merge.
func TestCaptureStreamUncoercibleUsage(t *testing.T) {
	tests := map[string]struct {
		// json observes events[0] as one buffered JSON body instead of as SSE events.
		json bool
		// events are observed in order; afterBad is the index after which the snapshot is taken.
		events   []string
		afterBad int
		wantSnap map[string]any
		want     map[string]any
	}{
		"success: message_delta with a shadowed toString member keeps its sibling and a later event": {
			events: []string{
				`{"type":"message_start","message":{"id":"msg-delta","model":"model-delta","usage":{"input_tokens":3,"output_tokens":0}}}`,
				`{"type":"message_delta","usage":{"output_tokens":9,"extra":{"toString":null}}}`,
				`{"type":"message_delta","usage":{"cache_read_input_tokens":5}}`,
				`{"type":"message_stop"}`,
			},
			afterBad: 1,
			wantSnap: map[string]any{"input_tokens": float64(3), "output_tokens": float64(9), "cache_read_tokens": nil},
			want:     map[string]any{"input_tokens": float64(3), "output_tokens": float64(9), "cache_read_tokens": float64(5), "response_mode": "sse", "request_id": "msg-delta", "request_id_source": "message_id", "model": "model-delta"},
		},
		"success: message_start with a shadowed toString member keeps identity and siblings": {
			events: []string{
				`{"type":"message_start","message":{"id":"msg-start","model":"model-start","usage":{"input_tokens":4,"cache_creation_input_tokens":2,"bad":{"toString":"text"}}}}`,
				`{"type":"message_delta","usage":{"output_tokens":6}}`,
			},
			afterBad: 0,
			wantSnap: map[string]any{"input_tokens": float64(4), "cache_creation_tokens": float64(2), "output_tokens": nil},
			want:     map[string]any{"input_tokens": float64(4), "cache_creation_tokens": float64(2), "output_tokens": float64(6), "response_mode": "sse", "request_id": "msg-start", "request_id_source": "message_id", "model": "model-start"},
		},
		"success: counter that is itself an object with own toString is skipped and a later event sets it": {
			events: []string{
				`{"type":"message_delta","usage":{"output_tokens":{"toString":null},"input_tokens":8}}`,
				`{"type":"message_delta","usage":{"output_tokens":3}}`,
			},
			afterBad: 0,
			wantSnap: map[string]any{"input_tokens": float64(8), "output_tokens": nil},
			want:     map[string]any{"input_tokens": float64(8), "output_tokens": float64(3), "response_mode": "sse"},
		},
		"success: nested object with own toString keeps its numeric nested sibling": {
			events: []string{
				`{"type":"message_delta","usage":{"cache_creation":{"ephemeral_5m_input_tokens":4,"deep":{"toString":null}},"output_tokens":2}}`,
				`{"type":"message_delta","usage":{"input_tokens":1}}`,
			},
			afterBad: 0,
			wantSnap: map[string]any{"cache_creation_5m_tokens": float64(4), "output_tokens": float64(2), "input_tokens": nil},
			want:     map[string]any{"cache_creation_5m_tokens": float64(4), "output_tokens": float64(2), "input_tokens": float64(1), "response_mode": "sse"},
		},
		"success: array element with own toString keeps its sibling": {
			events: []string{
				`{"type":"message_delta","usage":{"output_tokens":5,"extra":[{"toString":null}]}}`,
				`{"type":"message_delta","usage":{"input_tokens":12}}`,
			},
			afterBad: 0,
			wantSnap: map[string]any{"output_tokens": float64(5), "input_tokens": nil},
			want:     map[string]any{"output_tokens": float64(5), "input_tokens": float64(12), "response_mode": "sse"},
		},
		"error: buffered JSON with the same usage still drops the merge": {
			json:     true,
			events:   []string{`{"id":"msg-json","usage":{"output_tokens":9,"extra":{"toString":null}}}`},
			afterBad: 0,
			wantSnap: map[string]any{"output_tokens": nil},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 0, nil)
			c.SetResponse(http.StatusOK, nil)
			for i, raw := range tt.events {
				if tt.json {
					c.ObserveJSON([]byte(raw))
				} else {
					c.ObserveEvent(decodeWire(t, raw))
				}
				if i != tt.afterBad {
					continue
				}
				snap := c.Snapshot()
				got := map[string]any{}
				for key := range tt.wantSnap {
					got[key] = snap[key]
				}
				if diff := gocmp.Diff(tt.wantSnap, got); diff != "" {
					t.Errorf("Snapshot after event %d (-want +got):\n%s", i, diff)
				}
			}
			r := c.Finish()
			if tt.want == nil {
				if r != nil {
					t.Errorf("Finish returned %+v, want nil", r.Attributes)
				}
				return
			}
			if r == nil {
				t.Fatal("Finish returned nil, want a gateway.token.usage record")
			}
			got := map[string]any{}
			for key := range tt.want {
				got[key] = r.Attributes[key]
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("record attributes (-want +got):\n%s", diff)
			}
		})
	}
}

// advancingClock returns base on its first reading and moves forward by step on every later
// reading, so two readings during one record construction give two different instants.
type advancingClock struct {
	mu    sync.Mutex
	base  time.Time
	step  time.Duration
	reads int
}

func (c *advancingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	at := c.base.Add(time.Duration(c.reads) * c.step)
	c.reads++
	return at
}

// TestCaptureClockSample checks that the primary record's observedAt and the evaluation of a
// date-valued Retry-After use one clock sample taken at the start of record construction, with
// a clock that moves on every reading; records derived from the primary record keep its instant.
func TestCaptureClockSample(t *testing.T) {
	base := time.Date(2026, time.July, 19, 20, 0, 0, 0, time.UTC)
	tests := map[string]struct {
		build      bool
		status     int
		body       string
		retryAfter string
		wantEvent  string
		wantRetry  float64
		// wantDerived is the least number of records Build returns besides the primary one.
		wantDerived int
	}{
		"error: limit signal through a capture": {
			status:     http.StatusTooManyRequests,
			retryAfter: base.Add(30 * time.Second).Format(time.RFC3339),
			wantEvent:  "gateway.limit.signal",
			wantRetry:  30000,
		},
		"success: token usage through a capture with an HTTP-date Retry-After": {
			status:     http.StatusOK,
			body:       `{"usage":{"input_tokens":10,"output_tokens":2}}`,
			retryAfter: base.Add(45 * time.Second).Format(http.TimeFormat),
			wantEvent:  "gateway.token.usage",
			wantRetry:  45000,
		},
		"error: limit signal through Build": {
			build:      true,
			status:     http.StatusTooManyRequests,
			retryAfter: base.Add(20 * time.Second).Format(time.RFC3339),
			wantEvent:  "gateway.limit.signal",
			wantRetry:  20000,
		},
		"success: token usage through Build keeps the instant on derived records": {
			build:       true,
			status:      http.StatusOK,
			retryAfter:  base.Add(90 * time.Second).Format(time.RFC3339),
			wantEvent:   "gateway.token.usage",
			wantRetry:   90000,
			wantDerived: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clock := &advancingClock{base: base, step: 7 * time.Second}
			opts := newTestOptions(t)
			opts.Now = clock.now
			builder := usage.NewBuilder(opts)
			headers := http.Header{"Retry-After": {tt.retryAfter}}
			var records []usage.Record
			if tt.build {
				req := usage.Request{
					Body:            decodeWire(t, `{"tools":[{"name":"mcp__alpha__lookup","description":"alpha lookup schema"}],"messages":[{"role":"user","content":"hello"}]}`),
					StatusCode:      tt.status,
					ResponseHeaders: headers,
				}
				if tt.status == http.StatusOK {
					req.WireUsage = decodeWire(t, `{"input_tokens":100,"output_tokens":3}`)
				}
				records = builder.Build(req)
			} else {
				c := usage.NewCapture(builder, usage.Request{Body: decodeWire(t, `{}`)}, 0, nil)
				c.SetResponse(tt.status, headers)
				if tt.body != "" {
					c.ObserveJSON([]byte(tt.body))
				}
				if r := c.Finish(); r != nil {
					records = append(records, *r)
				}
			}
			if len(records) < 1+tt.wantDerived {
				t.Fatalf("got %d records, want a primary record and at least %d derived", len(records), tt.wantDerived)
			}
			primary := records[0]
			if diff := gocmp.Diff(tt.wantEvent, primary.EventName); diff != "" {
				t.Errorf("EventName (-want +got):\n%s", diff)
			}
			if !primary.ObservedAt.Equal(base) {
				t.Errorf("ObservedAt = %v, want the first clock sample %v", primary.ObservedAt, base)
			}
			if diff := gocmp.Diff(any(tt.wantRetry), primary.Attributes["retry_after_ms"]); diff != "" {
				t.Errorf("retry_after_ms evaluated against the first clock sample (-want +got):\n%s", diff)
			}
			for i, r := range records[1:] {
				if !r.ObservedAt.Equal(primary.ObservedAt) {
					t.Errorf("derived record %d (%s) ObservedAt = %v, want the primary instant %v", i, r.EventName, r.ObservedAt, primary.ObservedAt)
				}
			}
		})
	}
}
