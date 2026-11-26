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
	json "encoding/json/v2"
	"math"
	"net/http"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"
)

func TestEmitterExportsSaturatedResponseBytes(t *testing.T) {
	tests := map[string]struct {
		drive     func(c *Capture)
		wantEvent string
	}{
		"success: token usage exports saturated response_body_bytes as exact MaxInt64 decimal": {
			drive: func(c *Capture) {
				c.SetResponse(http.StatusOK, nil)
				c.NoteResponseBytes(math.MaxInt64 - 5)
				c.ObserveJSON([]byte(`{"usage":{"input_tokens":5,"output_tokens":1}}`))
			},
			wantEvent: "gateway.token.usage",
		},
		"success: limit signal exports saturated response_body_bytes as exact MaxInt64 decimal": {
			drive: func(c *Capture) {
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
			// The pipe collector runs the export bound on virtual time, so the single post is
			// delivered however loaded the host is.
			synctest.Test(t, func(t *testing.T) {
				collector := &pipeCollector{}
				e := collector.emitter(NewBuilder(options(t)))
				c := e.Start(Request{Body: wire(t, `{}`)})
				tt.drive(c)
				rec := c.Finish()
				e.Wait()

				if rec == nil {
					t.Fatalf("Finish returned nil, want %q record", tt.wantEvent)
				}

				payloads := collector.received(t)
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
		})
	}
}
