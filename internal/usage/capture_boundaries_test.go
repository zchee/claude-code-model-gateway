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
	json "encoding/json/v2"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
)

type oneShotPanicClock struct {
	arm atomic.Bool
}

func (c *oneShotPanicClock) now() time.Time {
	if c.arm.Swap(false) {
		panic("injected clock panic")
	}
	return time.Unix(123, 0)
}

type oneShotPanicRandom struct {
	arm  atomic.Bool
	mu   sync.Mutex
	next byte
}

func (r *oneShotPanicRandom) Read(p []byte) (int, error) {
	if r.arm.Swap(false) {
		panic("injected random panic")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range p {
		p[i] = r.next
		r.next++
	}
	return len(p), nil
}

func overwriteWireStorage(v *jsonx.WireValue) {
	for i := range v.Text {
		v.Text[i] = 'X'
	}
	if v.Kind == '0' {
		v.Number = 999999
	}
	for i := range v.Members {
		for j := range v.Members[i].Name {
			v.Members[i].Name[j] = 'X'
		}
		overwriteWireStorage(&v.Members[i].Value)
	}
	for i := range v.Elements {
		overwriteWireStorage(&v.Elements[i])
	}
}

func overwriteBytes(buf []byte) {
	for i := range buf {
		buf[i] = 'X'
	}
}

func TestCaptureFinishPanicUnlocksBuilder(t *testing.T) {
	const lockTimeout = 2 * time.Second
	tests := map[string]struct {
		panicRandom bool
		wantPanic   string
	}{
		"error: panicking clock during Finish escapes caller and unlocks builder": {
			panicRandom: false,
			wantPanic:   "injected clock panic",
		},
		"error: panicking random source during Finish escapes caller and unlocks builder": {
			panicRandom: true,
			wantPanic:   "injected random panic",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			clk := &oneShotPanicClock{}
			rng := &oneShotPanicRandom{}
			opts := newTestOptions(t)
			opts.Now = clk.now
			opts.Random = rng
			b := usage.NewBuilder(opts)

			var emitCalls atomic.Int32
			c1 := usage.NewCapture(b, usage.Request{Body: decodeWire(t, `{}`)}, 0, func(usage.Record) { emitCalls.Add(1) })
			c1.SetResponse(http.StatusOK, nil)
			c1.ObserveJSON([]byte(`{"id":"msg-panic","usage":{"input_tokens":10,"output_tokens":2}}`))

			if tt.panicRandom {
				rng.arm.Store(true)
			} else {
				clk.arm.Store(true)
			}

			rec, panicked := callFinishGuarded(c1)
			if diff := gocmp.Diff(any(tt.wantPanic), panicked); diff != "" {
				t.Errorf("Finish panic (-want +got):\n%s", diff)
			}
			if rec != nil {
				t.Errorf("Finish returned %+v after panic, want nil", rec)
			}
			if got := emitCalls.Load(); got != 0 {
				t.Errorf("emit call count = %d, want 0 when primary construction panicked", got)
			}

			buildDone := make(chan []usage.Record, 1)
			go func() {
				buildDone <- b.Build(usage.Request{
					Body:       decodeWire(t, `{}`),
					StatusCode: http.StatusOK,
					Usage:      map[string]any{"input_tokens": 25, "output_tokens": 4},
				})
			}()
			buildTimer := time.NewTimer(lockTimeout)
			defer buildTimer.Stop()
			var built []usage.Record
			select {
			case built = <-buildDone:
			case <-buildTimer.C:
				t.Fatalf("Builder.Build did not complete within %s after recovered Finish panic", lockTimeout)
			}
			primary := findEvents(built, "gateway.token.usage")
			if len(primary) != 1 {
				t.Fatalf("Builder.Build primary records = %d, want 1", len(primary))
			}
			if diff := gocmp.Diff(any(float64(25)), primary[0].Attributes["input_tokens"]); diff != "" {
				t.Errorf("Builder.Build input_tokens (-want +got):\n%s", diff)
			}

			captureDone := make(chan *usage.Record, 1)
			go func() {
				c2 := usage.NewCapture(b, usage.Request{Body: decodeWire(t, `{}`)}, 0, nil)
				c2.SetResponse(http.StatusOK, nil)
				c2.ObserveJSON([]byte(`{"id":"msg-after","usage":{"input_tokens":31,"output_tokens":6}}`))
				captureDone <- c2.Finish()
			}()
			captureTimer := time.NewTimer(lockTimeout)
			defer captureTimer.Stop()
			var rec2 *usage.Record
			select {
			case rec2 = <-captureDone:
			case <-captureTimer.C:
				t.Fatalf("later Capture.Finish did not complete within %s after recovered Finish panic", lockTimeout)
			}
			if rec2 == nil {
				t.Fatal("later Capture.Finish returned nil, want gateway.token.usage record")
			}
			if diff := gocmp.Diff("gateway.token.usage", rec2.EventName); diff != "" {
				t.Errorf("later Capture EventName (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(any(float64(31)), rec2.Attributes["input_tokens"]); diff != "" {
				t.Errorf("later Capture input_tokens (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCaptureObservedEventOwnership(t *testing.T) {
	tests := map[string]struct {
		mode      string
		wantID    string
		wantModel string
		wantSnap  map[string]any
	}{
		"success: ObserveEvent message_start survives member name storage overwrite": {
			mode:      "event-start",
			wantID:    "msg-start-owned",
			wantModel: "claude-start-owned",
			wantSnap: map[string]any{
				"input_tokens":             float64(42),
				"output_tokens":            float64(3),
				"cache_creation_tokens":    float64(15),
				"cache_creation_5m_tokens": float64(10),
				"cache_creation_1h_tokens": float64(5),
				"web_search_requests":      float64(2),
				"server_tool_use_count":    float64(2),
			},
		},
		"success: ObserveEvent message_delta survives member name storage overwrite": {
			mode:      "event-delta",
			wantID:    "msg-delta-owned",
			wantModel: "claude-delta-owned",
			wantSnap: map[string]any{
				"input_tokens":          float64(20),
				"output_tokens":         float64(17),
				"thinking_tokens":       float64(6),
				"web_fetch_requests":    float64(3),
				"server_tool_use_count": float64(3),
			},
		},
		"success: ObserveJSON survives buffer overwrite after observation": {
			mode:      "json",
			wantID:    "msg-json-owned",
			wantModel: "claude-json-owned",
			wantSnap: map[string]any{
				"input_tokens":             float64(55),
				"output_tokens":            float64(8),
				"cache_creation_tokens":    float64(12),
				"cache_creation_5m_tokens": float64(7),
				"cache_creation_1h_tokens": float64(5),
				"web_search_requests":      float64(4),
				"server_tool_use_count":    float64(4),
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`), Backend: "anthropic"}, 0, nil)
			c.SetResponse(http.StatusOK, nil)

			switch tt.mode {
			case "event-start":
				ev := decodeWire(t, `{"type":"message_start","message":{"id":"msg-start-owned","model":"claude-start-owned","usage":{"input_tokens":42,"output_tokens":3,"cache_creation_input_tokens":15,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":5},"server_tool_use":{"web_search_requests":2}}}}`)
				c.ObserveEvent(ev)
				overwriteWireStorage(&ev)
			case "event-delta":
				startEv := decodeWire(t, `{"type":"message_start","message":{"id":"msg-delta-owned","model":"claude-delta-owned","usage":{"input_tokens":20,"output_tokens":1}}}`)
				c.ObserveEvent(startEv)
				deltaEv := decodeWire(t, `{"type":"message_delta","usage":{"output_tokens":17,"output_tokens_details":{"reasoning_tokens":6},"server_tool_use":{"web_fetch_requests":3}}}`)
				c.ObserveEvent(deltaEv)
				overwriteWireStorage(&startEv)
				overwriteWireStorage(&deltaEv)
			case "json":
				buf := []byte(`{"id":"msg-json-owned","model":"claude-json-owned","usage":{"input_tokens":55,"output_tokens":8,"cache_creation_input_tokens":12,"cache_creation":{"ephemeral_5m_input_tokens":7,"ephemeral_1h_input_tokens":5},"server_tool_use":{"web_search_requests":4}}}`)
				c.ObserveJSON(buf)
				overwriteBytes(buf)
			}

			snap := c.Snapshot()
			for attr, want := range tt.wantSnap {
				if diff := gocmp.Diff(want, snap[attr]); diff != "" {
					t.Errorf("Snapshot[%q] (-want +got):\n%s", attr, diff)
				}
			}

			rec := c.Finish()
			if rec == nil {
				t.Fatal("Finish returned nil, want gateway.token.usage record")
			}
			if diff := gocmp.Diff("gateway.token.usage", rec.EventName); diff != "" {
				t.Errorf("EventName (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(any(tt.wantID), rec.Attributes["request_id"]); diff != "" {
				t.Errorf("request_id (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(any(tt.wantModel), rec.Attributes["model"]); diff != "" {
				t.Errorf("model (-want +got):\n%s", diff)
			}
			for attr, want := range tt.wantSnap {
				if diff := gocmp.Diff(want, rec.Attributes[attr]); diff != "" {
					t.Errorf("Finish Attributes[%q] (-want +got):\n%s", attr, diff)
				}
			}
		})
	}
}

func TestCaptureObservedEventConcurrentMutation(t *testing.T) {
	tests := map[string]struct {
		mode     string
		wantSnap map[string]any
	}{
		"success: concurrent overwrite after ObserveEvent message_start does not race or alter counts": {
			mode: "event-start",
			wantSnap: map[string]any{
				"input_tokens":             float64(64),
				"output_tokens":            float64(5),
				"cache_creation_5m_tokens": float64(9),
				"web_search_requests":      float64(1),
				"server_tool_use_count":    float64(1),
			},
		},
		"success: concurrent overwrite after ObserveEvent message_delta does not race or alter counts": {
			mode: "event-delta",
			wantSnap: map[string]any{
				"input_tokens":          float64(18),
				"output_tokens":         float64(29),
				"thinking_tokens":       float64(4),
				"web_fetch_requests":    float64(2),
				"server_tool_use_count": float64(2),
			},
		},
		"success: concurrent overwrite after ObserveJSON does not race or alter counts": {
			mode: "json",
			wantSnap: map[string]any{
				"input_tokens":             float64(73),
				"output_tokens":            float64(11),
				"cache_creation_5m_tokens": float64(6),
				"web_search_requests":      float64(3),
				"server_tool_use_count":    float64(3),
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`), Backend: "anthropic"}, 0, nil)
			c.SetResponse(http.StatusOK, nil)

			var mutate func()
			switch tt.mode {
			case "event-start":
				ev := decodeWire(t, `{"type":"message_start","message":{"id":"msg-race-start","model":"claude-race","usage":{"input_tokens":64,"output_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":9},"server_tool_use":{"web_search_requests":1}}}}`)
				c.ObserveEvent(ev)
				mutate = func() { overwriteWireStorage(&ev) }
			case "event-delta":
				startEv := decodeWire(t, `{"type":"message_start","message":{"id":"msg-race-delta","model":"claude-race","usage":{"input_tokens":18,"output_tokens":1}}}`)
				c.ObserveEvent(startEv)
				deltaEv := decodeWire(t, `{"type":"message_delta","usage":{"output_tokens":29,"output_tokens_details":{"reasoning_tokens":4},"server_tool_use":{"web_fetch_requests":2}}}`)
				c.ObserveEvent(deltaEv)
				mutate = func() {
					overwriteWireStorage(&startEv)
					overwriteWireStorage(&deltaEv)
				}
			case "json":
				buf := []byte(`{"id":"msg-race-json","model":"claude-race","usage":{"input_tokens":73,"output_tokens":11,"cache_creation":{"ephemeral_5m_input_tokens":6},"server_tool_use":{"web_search_requests":3}}}`)
				c.ObserveJSON(buf)
				mutate = func() { overwriteBytes(buf) }
			}

			var wg sync.WaitGroup
			wg.Go(mutate)
			snap := c.Snapshot()
			rec := c.Finish()
			wg.Wait()

			if rec == nil {
				t.Fatal("Finish returned nil, want gateway.token.usage record")
			}
			for attr, want := range tt.wantSnap {
				if diff := gocmp.Diff(want, snap[attr]); diff != "" {
					t.Errorf("Snapshot[%q] (-want +got):\n%s", attr, diff)
				}
				if diff := gocmp.Diff(want, rec.Attributes[attr]); diff != "" {
					t.Errorf("Finish Attributes[%q] (-want +got):\n%s", attr, diff)
				}
			}
		})
	}
}

func TestEmitterExportsSaturatedResponseBytes(t *testing.T) {
	tests := map[string]struct {
		drive     func(c *usage.Capture)
		wantEvent string
	}{
		"success: token usage exports saturated response_body_bytes as exact MaxInt64 decimal": {
			drive: func(c *usage.Capture) {
				c.SetResponse(http.StatusOK, nil)
				c.NoteResponseBytes(math.MaxInt64 - 5)
				c.ObserveJSON([]byte(`{"usage":{"input_tokens":5,"output_tokens":1}}`))
			},
			wantEvent: "gateway.token.usage",
		},
		"success: limit signal exports saturated response_body_bytes as exact MaxInt64 decimal": {
			drive: func(c *usage.Capture) {
				c.SetResponse(http.StatusTooManyRequests, http.Header{"Retry-After": {"2"}})
				c.NoteResponseBytes(math.MaxInt64)
				c.NoteResponseBytes(10)
			},
			wantEvent: "gateway.limit.signal",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

			var mu sync.Mutex
			var payloads [][]byte
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				payloads = append(payloads, body)
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer collector.Close()

			e := usage.NewEmitter(
				func(k string) (string, bool) { return collector.URL + "/v1/logs", k == "CODEX_GATEWAY_USAGE_ENDPOINT" },
				usage.NewBuilder(newTestOptions(t)),
			)
			c := e.Start(usage.Request{Body: decodeWire(t, `{}`)})
			tt.drive(c)
			rec := c.Finish()
			e.Wait()

			if rec == nil {
				t.Fatalf("Finish returned nil, want %q record", tt.wantEvent)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(payloads) != 1 {
				t.Fatalf("collector received %d payloads, want 1", len(payloads))
			}

			var envelope struct {
				ResourceLogs []struct {
					ScopeLogs []struct {
						LogRecords []struct {
							EventName  string `json:"eventName"`
							Attributes []struct {
								Key   string         `json:"key"`
								Value map[string]any `json:"value"`
							} `json:"attributes"`
						} `json:"logRecords"`
					} `json:"scopeLogs"`
				} `json:"resourceLogs"`
			}
			if err := json.Unmarshal(payloads[0], &envelope); err != nil {
				t.Fatalf("unmarshal exported OTLP payload: %v", err)
			}
			if len(envelope.ResourceLogs) != 1 || len(envelope.ResourceLogs[0].ScopeLogs) != 1 || len(envelope.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
				t.Fatalf("unexpected OTLP envelope structure: %s", payloads[0])
			}
			logRec := envelope.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
			if diff := gocmp.Diff(tt.wantEvent, logRec.EventName); diff != "" {
				t.Errorf("exported eventName (-want +got):\n%s", diff)
			}
			var gotBytesAttr map[string]any
			for _, attr := range logRec.Attributes {
				if attr.Key == "response_body_bytes" {
					gotBytesAttr = attr.Value
					break
				}
			}
			if diff := gocmp.Diff(map[string]any{"intValue": "9223372036854775807"}, gotBytesAttr); diff != "" {
				t.Errorf("exported response_body_bytes OTLP value (-want +got):\n%s", diff)
			}
		})
	}
}
