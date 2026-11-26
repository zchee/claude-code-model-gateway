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
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
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

func compareRecords(t *testing.T, label string, want, got usage.Record) {
	t.Helper()
	if diff := gocmp.Diff(want.EventName, got.EventName); diff != "" {
		t.Errorf("%s EventName (-want +got):\n%s", label, diff)
	}
	if !want.ObservedAt.Equal(got.ObservedAt) {
		t.Errorf("%s ObservedAt = %v, want %v", label, got.ObservedAt, want.ObservedAt)
	}
	if diff := gocmp.Diff(want.TraceID, got.TraceID); diff != "" {
		t.Errorf("%s TraceID (-want +got):\n%s", label, diff)
	}
	if diff := gocmp.Diff(want.SpanID, got.SpanID); diff != "" {
		t.Errorf("%s SpanID (-want +got):\n%s", label, diff)
	}
	if diff := gocmp.Diff(want.Attributes, got.Attributes); diff != "" {
		t.Errorf("%s Attributes (-want +got):\n%s", label, diff)
	}
	wantOTLP, err := want.OTLP()
	if err != nil {
		t.Fatalf("%s want.OTLP(): %v", label, err)
	}
	gotOTLP, err := got.OTLP()
	if err != nil {
		t.Fatalf("%s got.OTLP(): %v", label, err)
	}
	if diff := gocmp.Diff([]byte(wantOTLP), []byte(gotOTLP)); diff != "" {
		t.Errorf("%s OTLP (-want +got):\n%s", label, diff)
	}
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
				compareRecords(t, "emitted record", *first, emitted)
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
			compareRecords(t, "emitted record", *returned[0], emitted)
			if again, p := callFinishGuarded(c); p != nil || again != nil {
				t.Errorf("trailing Finish = (%+v, panic=%v), want (nil, nil)", again, p)
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("emit call count after trailing Finish = %d, want 1", got)
			}
		})
	}
}

// offsetRandom fills every buffer with the low byte of each byte's offset in its stream. Once, on
// the read that would deliver the byte at panicAt, it panics instead and delivers nothing; a
// negative panicAt never panics.
type offsetRandom struct {
	mu      sync.Mutex
	offset  int
	panicAt int
	fired   bool
}

func (r *offsetRandom) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.fired && r.panicAt >= r.offset && r.panicAt < r.offset+len(p) {
		r.fired = true
		panic("injected random panic")
	}
	for i := range p {
		p[i] = byte(r.offset + i)
	}
	r.offset += len(p)
	return len(p), nil
}

// offsetIdentifier spells the version 4 identifier an offsetRandom delivers from offset start.
func offsetIdentifier(start int) string {
	var id [16]byte
	for i := range id {
		id[i] = byte(start + i)
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	h := hex.EncodeToString(id[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func TestFinishPanicOutsideCallerCallback(t *testing.T) {
	// One MCP server makes the emitter derive one footprint record after the primary record.
	const mcpBody = `{"tools":[{"name":"mcp__alpha__one","description":"alpha schema"}],"messages":[{"role":"user","content":"hello"}]}`
	const response = `{"usage":{"input_tokens":100,"output_tokens":5}}`
	tests := map[string]struct {
		// emitter starts the capture from an emitter whose random source panics on the first
		// byte after the primary record's identifiers; otherwise the capture is standalone and
		// its caller's callback panics.
		emitter bool
	}{
		"error: random source panic while the emitter derives a record reaches the caller of Finish": {emitter: true},
		"success: panicking caller callback on a standalone capture still returns its record":        {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if !tt.emitter {
				var calls atomic.Int32
				c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, mcpBody)}, 0, func(usage.Record) {
					calls.Add(1)
					panic("callback failure")
				})
				c.SetResponse(http.StatusOK, nil)
				c.ObserveJSON([]byte(response))
				rec, panicked := callFinishGuarded(c)
				if panicked != nil {
					t.Errorf("Finish panicked with the caller callback's panic: %v", panicked)
				}
				if rec == nil || rec.EventName != "gateway.token.usage" {
					t.Errorf("Finish returned %+v, want the gateway.token.usage record", rec)
				}
				if got := calls.Load(); got != 1 {
					t.Errorf("callback calls = %d, want 1", got)
				}
				return
			}
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer collector.Close()
			endpoint := collector.URL + "/v1/logs"
			lookup := func(k string) (string, bool) { return endpoint, k == "CODEX_GATEWAY_USAGE_ENDPOINT" }
			finish := func(random *offsetRandom) (*usage.Record, any) {
				opts := newTestOptions(t)
				opts.Random = random
				e := usage.NewEmitter(lookup, usage.NewBuilder(opts))
				defer e.Wait()
				c := e.Start(usage.Request{Body: decodeWire(t, mcpBody)})
				c.SetResponse(http.StatusOK, nil)
				c.ObserveJSON([]byte(response))
				return callFinishGuarded(c)
			}

			// Without a panic, the start draws bytes 0 to 15 and the primary record bytes 16 to
			// 31, so the read of byte 32 belongs to the derived footprint record.
			control := &offsetRandom{panicAt: -1}
			rec, panicked := finish(control)
			if panicked != nil || rec == nil {
				t.Fatalf("Finish without a panicking source = (%+v, panic=%v), want the primary record", rec, panicked)
			}
			if diff := gocmp.Diff(any("gateway-usage-"+offsetIdentifier(16)), rec.Attributes["source_event_id"]); diff != "" {
				t.Fatalf("primary record identifier is not drawn from bytes 16 to 31 (-want +got):\n%s", diff)
			}
			if control.offset <= 32 {
				t.Fatalf("the emitter drew %d random bytes, want a derived record drawn after byte 32", control.offset)
			}

			random := &offsetRandom{panicAt: 32}
			rec, panicked = finish(random)
			if !random.fired {
				t.Fatal("the random source never reached byte 32")
			}
			if diff := gocmp.Diff(any("injected random panic"), panicked); diff != "" {
				t.Errorf("panic reaching the caller of Finish (-want +got):\n%s", diff)
			}
			if rec != nil {
				t.Errorf("Finish returned %+v while its emitter panicked, want the panic", rec)
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
				record := c.Finish()
				return record, emitted, rng.read
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
		bodyBytes     int64
		wantBodyBytes int64
	}{
		"success: explicit body bytes and composition survive body mutation": {bodyBytes: 999, wantBodyBytes: 999},
		"success: measured compact body size survives body mutation":         {bodyBytes: -1, wantBodyBytes: 267},
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
			wantAttrs := map[string]any{
				"request_body_bytes":           tt.wantBodyBytes,
				"input_system_bytes":           int64(14),
				"input_tools_bytes":            int64(96),
				"input_native_tools_bytes":     int64(38),
				"input_mcp_tools_bytes":        int64(55),
				"input_messages_bytes":         int64(125),
				"input_first_message_bytes":    int64(37),
				"input_history_bytes":          int64(85),
				"input_tool_results_bytes":     int64(57),
				"input_tools_tokens":           float64(207),
				"input_native_tools_tokens":    float64(85),
				"input_mcp_tools_tokens":       float64(122),
				"input_mcp_tools_alpha_tokens": float64(122),
				"input_system_tokens":          float64(30),
				"input_first_message_tokens":   float64(80),
				"input_history_tokens":         float64(183),
				"input_tool_results_tokens":    float64(120),
			}
			for attr, want := range wantAttrs {
				if diff := gocmp.Diff(want, refRec.Attributes[attr]); diff != "" {
					t.Errorf("unmutated Attributes[%q] (-want +got):\n%s", attr, diff)
				}
				if diff := gocmp.Diff(want, mutRec.Attributes[attr]); diff != "" {
					t.Errorf("mutated Attributes[%q] (-want +got):\n%s", attr, diff)
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

// TestCaptureJSONCoercionKeepsIdentity checks parity with lib/usage-observability.js lines
// 743-748 and 770-775, where observeIdentity runs before mergeUsage so the first safe response id
// and model are retained even when numeric conversion fails and drops usage and response mode.
func TestCaptureJSONCoercionKeepsIdentity(t *testing.T) {
	const (
		throwingDoc = `{"id":"msg-kept-first","model":"claude-kept-first","usage":{"input_tokens":15,"extra":{"toString":null}}}`
		followUpDoc = `{"id":"msg-second","model":"claude-second","usage":{"input_tokens":20,"output_tokens":3}}`
	)
	tests := map[string]struct {
		chunked      bool
		followUp     string
		status       int
		headers      http.Header
		wantEvent    string
		wantID       string
		wantIDSrc    string
		wantModel    string
		wantMode     any
		wantInputTok any
	}{
		"success: ObserveJSON keeps id and model on limit signal when usage coercion fails": {
			status:    http.StatusTooManyRequests,
			headers:   http.Header{"Retry-After": {"2"}},
			wantEvent: "gateway.limit.signal",
			wantID:    "msg-kept-first",
			wantIDSrc: "message_id",
			wantModel: "claude-kept-first",
			wantMode:  nil,
		},
		"success: ObserveChunk keeps id and model on limit signal when usage coercion fails": {
			chunked:   true,
			status:    http.StatusTooManyRequests,
			headers:   http.Header{"Retry-After": {"2"}},
			wantEvent: "gateway.limit.signal",
			wantID:    "msg-kept-first",
			wantIDSrc: "message_id",
			wantModel: "claude-kept-first",
			wantMode:  nil,
		},
		"success: ObserveJSON keeps first id and model across a subsequent valid JSON document": {
			followUp:     followUpDoc,
			status:       http.StatusOK,
			wantEvent:    "gateway.token.usage",
			wantID:       "msg-kept-first",
			wantIDSrc:    "message_id",
			wantModel:    "claude-kept-first",
			wantMode:     "json",
			wantInputTok: float64(20),
		},
		"success: ObserveJSON keeps first id and model when followed by an SSE event": {
			status:       http.StatusOK,
			wantEvent:    "gateway.token.usage",
			wantID:       "msg-kept-first",
			wantIDSrc:    "message_id",
			wantModel:    "claude-kept-first",
			wantMode:     "sse",
			wantInputTok: float64(9),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`), Backend: "anthropic"}, 0, nil)
			c.SetResponse(tt.status, tt.headers.Clone())
			if tt.chunked {
				mid := len(throwingDoc) / 2
				c.ObserveChunk([]byte(throwingDoc[:mid]))
				c.ObserveChunk([]byte(throwingDoc[mid:]))
			} else {
				c.ObserveJSON([]byte(throwingDoc))
				if diff := gocmp.Diff(any(nil), c.Snapshot()["input_tokens"]); diff != "" {
					t.Errorf("Snapshot input_tokens after throwing JSON (-want +got):\n%s", diff)
				}
			}
			if tt.followUp != "" {
				c.ObserveJSON([]byte(tt.followUp))
			} else if tt.wantMode == "sse" {
				c.ObserveEvent(decodeWire(t, `{"type":"message_start","message":{"id":"msg-sse","model":"claude-sse","usage":{"input_tokens":9,"output_tokens":1}}}`))
			}

			rec := c.Finish()
			if rec == nil {
				t.Fatalf("Finish returned nil, want %q", tt.wantEvent)
			}
			for attr, want := range map[string]any{
				"event_name":        tt.wantEvent,
				"request_id":        tt.wantID,
				"request_id_source": tt.wantIDSrc,
				"model":             tt.wantModel,
				"response_mode":     tt.wantMode,
			} {
				if diff := gocmp.Diff(want, rec.Attributes[attr]); diff != "" {
					t.Errorf("Attributes[%q] (-want +got):\n%s", attr, diff)
				}
			}
			if tt.wantInputTok != nil {
				if diff := gocmp.Diff(tt.wantInputTok, rec.Attributes["input_tokens"]); diff != "" {
					t.Errorf("Attributes[input_tokens] (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestCaptureJSONCoercionBoundaries verifies the exact coercion edges of numericTree in
// lib/usage-observability.js lines 535-558: array element stringification, the depth-6 traversal
// ceiling, a usage root carrying its own toString member, __proto__ properties evaluated before
// merge skipping, and valueOf alone without own toString.
func TestCaptureJSONCoercionBoundaries(t *testing.T) {
	tests := map[string]struct {
		doc      string
		wantDrop bool
	}{
		"error: array element object with own toString drops JSON observation": {
			doc:      `{"usage":{"input_tokens":15,"extra":[{"toString":null}]}}`,
			wantDrop: true,
		},
		"error: nested array element object with own toString drops JSON observation": {
			doc:      `{"usage":{"input_tokens":15,"extra":[1,[2,{"toString":"bad"}]]}}`,
			wantDrop: true,
		},
		"success: array containing ordinary object with nested toString merges": {
			doc:      `{"usage":{"input_tokens":15,"extra":[{"nested":{"toString":null}}]}}`,
			wantDrop: false,
		},
		"error: object with own toString at depth-6 entry drops JSON observation": {
			doc:      `{"usage":{"input_tokens":15,"d1":{"d2":{"d3":{"d4":{"d5":{"d6":{"d7":{"toString":null}}}}}}}}}`,
			wantDrop: true,
		},
		"success: object with own toString past depth-6 boundary merges": {
			doc:      `{"usage":{"input_tokens":15,"d1":{"d2":{"d3":{"d4":{"d5":{"d6":{"d7":{"d8":{"toString":null}}}}}}}}}}`,
			wantDrop: false,
		},
		"success: usage root with own null toString merges without drop": {
			doc:      `{"usage":{"input_tokens":15,"toString":null}}`,
			wantDrop: false,
		},
		"success: usage root with own string toString merges without drop": {
			doc:      `{"usage":{"input_tokens":15,"toString":"bad"}}`,
			wantDrop: false,
		},
		"error: __proto__ object with own toString drops JSON observation": {
			doc:      `{"usage":{"input_tokens":15,"__proto__":{"toString":null}}}`,
			wantDrop: true,
		},
		"error: nested object under __proto__ with own toString drops JSON observation": {
			doc:      `{"usage":{"input_tokens":15,"__proto__":{"nested":{"toString":null}}}}`,
			wantDrop: true,
		},
		"success: __proto__ without toString merges without drop": {
			doc:      `{"usage":{"input_tokens":15,"__proto__":{}}}`,
			wantDrop: false,
		},
		"success: sibling object with null valueOf alone merges without drop": {
			doc:      `{"usage":{"input_tokens":15,"extra":{"valueOf":null}}}`,
			wantDrop: false,
		},
		"success: sibling object with string valueOf alone merges without drop": {
			doc:      `{"usage":{"input_tokens":15,"extra":{"valueOf":"1"}}}`,
			wantDrop: false,
		},
	}
	for name, tt := range tests {
		for _, chunked := range []bool{false, true} {
			mode := "ObserveJSON"
			if chunked {
				mode = "ObserveChunk"
			}
			t.Run(name+"/"+mode, func(t *testing.T) {
				c := usage.NewCapture(usage.NewBuilder(newTestOptions(t)), usage.Request{Body: decodeWire(t, `{}`)}, 0, nil)
				c.SetResponse(http.StatusOK, nil)
				if chunked {
					mid := len(tt.doc) / 2
					c.ObserveChunk([]byte(tt.doc[:mid]))
					c.ObserveChunk([]byte(tt.doc[mid:]))
				} else {
					c.ObserveJSON([]byte(tt.doc))
					var wantSnap any = float64(15)
					if tt.wantDrop {
						wantSnap = nil
					}
					if diff := gocmp.Diff(wantSnap, c.Snapshot()["input_tokens"]); diff != "" {
						t.Errorf("Snapshot input_tokens (-want +got):\n%s", diff)
					}
				}
				rec := c.Finish()
				if tt.wantDrop {
					if rec != nil {
						t.Errorf("Finish returned %+v, want nil", rec)
					}
					return
				}
				if rec == nil {
					t.Fatal("Finish returned nil, want gateway.token.usage record")
				}
				if diff := gocmp.Diff("gateway.token.usage", rec.EventName); diff != "" {
					t.Errorf("EventName (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(any(float64(15)), rec.Attributes["input_tokens"]); diff != "" {
					t.Errorf("input_tokens (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(any("json"), rec.Attributes["response_mode"]); diff != "" {
					t.Errorf("response_mode (-want +got):\n%s", diff)
				}
			})
		}
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
