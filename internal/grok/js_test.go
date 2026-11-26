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

package grok

import (
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/sse"
)

func wire(t *testing.T, input string) jsonx.WireValue {
	t.Helper()
	v, err := jsonx.DecodeWire([]byte(input))
	if err != nil {
		t.Fatalf("bad fixture %s: %v", input, err)
	}
	return v
}

// TestCount pins Number(v) || 0 as anthropicUsage applies it to every usage field. The expected
// values are what ECMAScript's StringToNumber and ToPrimitive give.
func TestCount(t *testing.T) {
	tests := map[string]struct {
		input string
		want  float64
	}{
		"success: integer":                        {`7`, 7},
		"success: negative zero is zero":          {`-0`, 0},
		"success: overflow is infinite":           {`1e400`, math.Inf(1)},
		"success: padded decimal string":          {`" \t\n2.5e1\u00a0\ufeff"`, 25},
		"success: empty string is zero":           {`""`, 0},
		"success: blank string is zero":           {`"\u2028 \u3000"`, 0},
		"success: leading dot":                    {`".5"`, 0.5},
		"success: trailing dot with exponent":     {`"5.e2"`, 500},
		"success: signed infinity":                {`"-Infinity"`, math.Inf(-1)},
		"success: hexadecimal integer":            {`"0xFF"`, 255},
		"success: octal integer":                  {`"0o17"`, 15},
		"success: binary integer":                 {`"0B101"`, 5},
		"success: hexadecimal tie rounds to even": {`"0x20000000000001"`, 9007199254740992},
		"success: true is one":                    {`true`, 1},
		"success: single element array":           {`[[" 9 "]]`, 9},
		"success: empty array is zero":            {`[]`, 0},
		"error: signed hexadecimal is NaN":        {`"-0x10"`, 0},
		"error: hexadecimal float is NaN":         {`"0x1p3"`, 0},
		"error: digit separator is NaN":           {`"1_000"`, 0},
		"error: lower case infinity is NaN":       {`"inf"`, 0},
		"error: nan spelling is NaN":              {`"NaN"`, 0},
		"error: bare exponent is NaN":             {`"1e"`, 0},
		"error: lone sign is NaN":                 {`"+"`, 0},
		"error: next line is not white space":     {`"\u00851"`, 0},
		"error: two element array is NaN":         {`[1,2]`, 0},
		"error: object is NaN":                    {`{"valueOf":1}`, 0},
		"error: non-ASCII digits are NaN":         {`"\uff11"`, 0},
		"error: empty prefixed integer is NaN":    {`"0x"`, 0},
		"error: binary digit out of range":        {`"0b102"`, 0},
		"success: null is zero":                   {`null`, 0},
		"success: positive sign and fraction":     {`"+1.25"`, 1.25},
		"success: upper case exponent and sign":   {`"1E-2"`, 0.01},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, count(wire(t, tt.input))); diff != "" {
				t.Errorf("Number(%s) || 0 (-want +got):\n%s", tt.input, diff)
			}
		})
	}
}

// TestJSString pins String(v) for every JSON kind, as template literals and join apply it.
func TestJSString(t *testing.T) {
	tests := map[string]struct {
		input, want string
	}{
		"success: string":              {`"a"`, "a"},
		"success: number spelling":     {`1e21`, "1e+21"},
		"success: small number":        {`1e-7`, "1e-7"},
		"success: negative zero":       {`-0`, "0"},
		"success: infinity":            {`-1e400`, "-Infinity"},
		"success: null":                {`null`, "null"},
		"success: boolean":             {`false`, "false"},
		"success: object":              {`{"a":1}`, "[object Object]"},
		"success: array join":          {`[1,null,"x",[2,[3]],{},true]`, "1,,x,2,3,[object Object],true"},
		"success: empty array":         {`[]`, ""},
		"success: array of one null":   {`[null]`, ""},
		"success: nested empty arrays": {`[[],[]]`, ","},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, string(utf16.Decode(jsString(wire(t, tt.input))))); diff != "" {
				t.Errorf("String(%s) (-want +got):\n%s", tt.input, diff)
			}
		})
	}
	if diff := gocmp.Diff("undefined", string(utf16.Decode(jsString(jsonx.WireValue{})))); diff != "" {
		t.Errorf("String(undefined) (-want +got):\n%s", diff)
	}
}

func encode(t *testing.T, v jsonx.WireValue) string {
	t.Helper()
	b, err := jsonx.EncodeWire(v)
	if err != nil {
		t.Fatalf("cannot serialize: %v", err)
	}
	return string(b)
}

// TestTranslateEdges covers coercions of the source that the request and response tables do not
// reach: truthy non-string values passed through `||`, String() of non-string text, and
// JSON.parse of non-string arguments.
func TestTranslateEdges(t *testing.T) {
	clock := func() time.Time { return time.UnixMilli(42) }
	requests := map[string]struct{ input, want string }{
		"success: non-string text is stringified": {
			`{"system":[{"type":"text","text":7},{"type":"text","text":null},{"type":"text"}],"messages":[{"role":"assistant","content":[{"type":"text","text":[1,2]},{"type":"text","text":0},""]},"bare",5,[]]}`,
			`{"model":"m","input":[{"role":"developer","content":[{"type":"input_text","text":"7\n\n"}]},{"role":"assistant","content":[{"type":"output_text","text":"1,2"},{"type":"output_text","text":""},{"type":"output_text","text":""}]}],"stream":false}`,
		},
		"success: falsy tool input becomes an empty object and a string input is quoted": {
			`{"messages":[{"content":[{"type":"tool_use","id":null,"input":0},{"type":"tool_use","input":"s"}]}],"max_tokens":null}`,
			`{"model":"m","input":[{"type":"function_call","call_id":null,"arguments":"{}"},{"type":"function_call","arguments":"\"s\""}],"max_output_tokens":null,"stream":false}`,
		},
		"success: web search detection and truthy names": {
			`{"tools":[{"name":"web_search","input_schema":{"type":"object"}},{"type":7,"name":1,"description":"d","input_schema":0},null],"output_config":{"effort":3}}`,
			`{"model":"m","input":[],"stream":false,"tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}},{"type":"function","name":1,"description":"d","parameters":{"type":"object","properties":{}}}]}`,
		},
		"success: string system and content": {
			`{"system":"sys","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`,
			`{"model":"m","input":[{"role":"developer","content":[{"type":"input_text","text":"sys"}]},{"role":"user","content":[{"type":"input_text","text":"hi"}]},{"role":"assistant","content":[{"type":"output_text","text":"yo"}]}],"stream":false}`,
		},
	}
	for name, tt := range requests {
		t.Run(name, func(t *testing.T) {
			got, err := TranslateRequest(wire(t, tt.input), "m")
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.want, encode(t, got)); diff != "" {
				t.Errorf("request (-want +got):\n%s", diff)
			}
		})
	}

	responses := map[string]struct{ input, want string }{
		"success: non-string arguments go through String before JSON.parse": {
			`{"id":5,"output":[{"type":"function_call","arguments":5},{"type":"function_call","arguments":[true]},{"type":"function_call","arguments":{"a":1}},{"type":"message","content":[{"type":"output_text","text":9},{"type":"output_text"}]}]}`,
			`{"id":5,"type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","input":5},{"type":"tool_use","input":true},{"type":"tool_use","input":{}},{"type":"text","text":9},{"type":"text","text":""}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0}}}`,
		},
		"success: web search without id or action uses the clock": {
			`{"output":[{"type":"web_search_call","action":["q"]},{"type":"web_search_call","id":"w","action":{"sources":[{"url":"u","title":""},{"url":1},null]}}],"usage":{"output_tokens_details":{"reasoning_tokens":null},"reasoning_tokens":3}}`,
			`{"id":"msg_grok_42","type":"message","role":"assistant","model":"m","content":[{"type":"server_tool_use","id":"web_search_42","name":"web_search","input":{"0":"q"}},{"type":"web_search_tool_result","tool_use_id":"web_search_42","content":[]},{"type":"server_tool_use","id":"w","name":"web_search","input":{}},{"type":"web_search_tool_result","tool_use_id":"w","content":[{"type":"web_search_result","title":"u","url":"u"}]}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":3}}}`,
		},
	}
	for name, tt := range responses {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff(tt.want, encode(t, TranslateResponse(wire(t, tt.input), "m", clock))); diff != "" {
				t.Errorf("response (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTranslateRequestNullMessageHidesContent checks that the error for a null message carries
// none of the request's content.
func TestTranslateRequestNullMessageHidesContent(t *testing.T) {
	_, err := TranslateRequest(wire(t, `{"messages":[{"role":"user","content":"secret-prompt"},null]}`), "grok-4.5")
	if err == nil {
		t.Fatal("accepted a null message")
	}
	if strings.Contains(err.Error(), "secret-prompt") {
		t.Errorf("error %q repeats request content", err)
	}
}

// TestStreamKeys pins how the source's Maps key raw values: a numeric and a string item id share
// the template-literal block key but not the tool record, and an object item id never finds its
// tool record again.
func TestStreamKeys(t *testing.T) {
	frame := func(payload string) sse.Frame { return sse.Frame{Body: "data: " + payload, Delimiter: "\n\n"} }
	tests := map[string]struct {
		events []string
		want   []string
	}{
		"success: numeric and string ids share a block but not the delta flag": {
			[]string{
				`{"type":"response.function_call_arguments.delta","item_id":1,"delta":"{"}`,
				`{"type":"response.function_call_arguments.done","item_id":"1","arguments":"{}"}`,
				`{"type":"response.function_call_arguments.done","item_id":1,"arguments":"{}"}`,
			},
			[]string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_grok_42\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":1,\"name\":\"\",\"input\":{}}}\n\n" +
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\"}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n",
				"",
			},
		},
		"success: object ids reuse the block and forget the delta flag": {
			[]string{
				`{"type":"response.function_call_arguments.delta","item_id":{"k":1},"delta":"x"}`,
				`{"type":"response.function_call_arguments.done","item_id":{"k":2},"arguments":"y"}`,
				`{"type":"response.output_text.delta","item_id":null,"output_index":[0],"delta":{"t":1}}`,
				`{"type":"response.output_item.done","output_index":[0]}`,
			},
			[]string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_grok_42\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":{\"k\":1},\"name\":\"\",\"input\":{}}}\n\n" +
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"x\"}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"y\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
					"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":{\"t\":1}}}\n\n",
				"",
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			stream := NewStream("m", func() time.Time { return time.UnixMilli(42) })
			for i, event := range tt.events {
				if diff := gocmp.Diff(tt.want[i], stream.Frame(frame(event))); diff != "" {
					t.Errorf("event %d (-want +got):\n%s", i, diff)
				}
			}
		})
	}
}
