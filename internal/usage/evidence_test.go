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
	"net/http"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestResponseEvidence(t *testing.T) {
	tests := map[string]struct {
		request Request
		event   string
		attrs   map[string]any
	}{
		"success: explicit null count is measured zero":                        {request: Request{StatusCode: 200, Usage: map[string]any{"input_tokens": nil}}, event: "gateway.token.usage", attrs: map[string]any{"input_tokens": 0, "context_tokens": 0, "request_body_bytes": 0}},
		"success: response identity takes precedence for Anthropic":            {request: Request{StatusCode: 200, Backend: "anthropic", EffectiveModel: "route", ResponseModel: "actual", MessageID: "message", Usage: map[string]any{"output_tokens": 1}}, event: "gateway.token.usage", attrs: map[string]any{"model": "actual", "request_id": "message", "request_id_source": "message_id"}},
		"success: other backends prefer the route and response request header": {request: Request{StatusCode: 201, Backend: "codex", EffectiveModel: "route", ResponseModel: "actual", MessageID: "message", ResponseHeaders: http.Header{"X-Request-Id": {"provider"}}, Usage: map[string]any{"output_tokens": 1}}, event: "gateway.token.usage", attrs: map[string]any{"model": "route", "request_id": "provider", "request_id_source": "response_header"}},
		"success: retry date becomes a relative delay":                         {request: Request{StatusCode: 429, ResponseHeaders: http.Header{"Retry-After": {"Thu, 01 Jan 1970 00:02:10 GMT"}}}, event: "gateway.limit.signal", attrs: map[string]any{"retry_after_ms": 7000}},
		"success: incomplete response is not evidence":                         {request: Request{StatusCode: 200, Incomplete: true, Usage: map[string]any{"input_tokens": 4}}},
		"success: overflowed response is not evidence":                         {request: Request{StatusCode: 200, Overflowed: true, Usage: map[string]any{"input_tokens": 4}}},
		"success: success needs token evidence despite limit headers":          {request: Request{StatusCode: 200, ResponseHeaders: http.Header{"Retry-After": {"3"}}}},
		"success: non-success needs limit evidence despite token counts":       {request: Request{StatusCode: 503, Usage: map[string]any{"output_tokens": 4}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			records := NewBuilder(options(t)).Build(tt.request)
			if tt.event == "" {
				if len(records) != 0 {
					t.Fatalf("got %d records without evidence", len(records))
				}
				return
			}
			r := onlyEvent(t, records, tt.event)
			got := make(map[string]any)
			for key := range tt.attrs {
				v, ok := r.Attributes[key]
				if !ok {
					t.Fatalf("missing attribute %q", key)
				}
				got[key] = v
			}
			checkJSON(t, tt.attrs, got)
		})
	}
}

func TestNumericTreeCoercion(t *testing.T) {
	tests := map[string]struct{ incoming, want map[string]any }{"success: normalize numeric leaves and omit unsupported values": {incoming: map[string]any{"null": nil, "list": []any{5.0}, "flag": true, "empty": []any{}, "hex": "0x10", "ignore": []any{1, 2}, "nested": map[string]any{"ignore": -1}}, want: map[string]any{"null": 0.0, "list": 5.0, "flag": 1.0, "empty": 0.0, "hex": 16.0}}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, MergeUsage(nil, tt.incoming)); diff != "" {
				t.Error(diff)
			}
		})
	}
}
