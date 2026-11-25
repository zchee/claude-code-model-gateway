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
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestOTLPEnvelope(t *testing.T) {
	const traceID = "00112233445566778899aabbccddeeff"
	const spanID = "0123456789abcdef"
	tests := map[string]struct{ trace, span, fields string }{
		"success: no trace":    {},
		"success: trace only":  {trace: traceID, fields: ` ,"traceId":"00112233445566778899aabbccddeeff"`},
		"success: span only":   {span: spanID, fields: ` ,"spanId":"0123456789abcdef"`},
		"success: linked span": {trace: traceID, span: spanID, fields: ` ,"traceId":"00112233445566778899aabbccddeeff","spanId":"0123456789abcdef"`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			record := Record{EventName: "gateway.token.usage", ObservedAt: time.Unix(123, 456789123), TraceID: tt.trace, SpanID: tt.span, Attributes: map[string]any{"event.name": "gateway.token.usage", "complete": false, "tokens": 7, "duration_ms": 1.5, "ticket_ref": nil}}
			payload, err := record.OTLP()
			if err != nil {
				t.Fatal(err)
			}
			expected := `{"resourceLogs":[{
    "resource":{"attributes":[{"key":"service.name","value":{"stringValue":"codex-gateway"}}]},
    "scopeLogs":[{
      "scope":{"name":"eigenwise.codex-gateway.usage","version":"1"},
      "logRecords":[{
        "timeUnixNano":"123456000000", "observedTimeUnixNano":"123456000000",
        "eventName":"gateway.token.usage", "body":{"stringValue":"gateway.token.usage"},
        "attributes":[
          {"key":"event.name","value":{"stringValue":"gateway.token.usage"}},
          {"key":"complete","value":{"boolValue":false}},
          {"key":"tokens","value":{"intValue":"7"}},
          {"key":"duration_ms","value":{"doubleValue":1.5}}
        ]` + tt.fields + `
      }]
    }]
  }]}`
			var want, got any
			if err := json.Unmarshal([]byte(expected), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatal(err)
			}
			// Record.Attributes is a map; only the ordering of OTLP attributes is immaterial.
			byKey := cmpopts.SortSlices(func(a, b any) bool {
				left, _ := a.(map[string]any)
				right, _ := b.(map[string]any)
				x, _ := left["key"].(string)
				y, _ := right["key"].(string)
				return x < y
			})
			if diff := gocmp.Diff(want, got, byKey); diff != "" {
				t.Errorf("OTLP envelope (-want +got):\n%s", diff)
			}
		})
	}
}
