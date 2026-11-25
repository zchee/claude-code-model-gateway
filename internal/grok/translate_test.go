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

package grok_test

import (
	"os"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func clock() time.Time { return time.UnixMilli(123456789) }

func decode(t *testing.T, input string) jsonx.WireValue {
	t.Helper()
	v, err := jsonx.DecodeWire([]byte(input))
	if err != nil {
		t.Fatalf("bad fixture %s: %v", input, err)
	}
	return v
}

func checkWire(t *testing.T, want string, got jsonx.WireValue) {
	t.Helper()
	wire, err := jsonx.EncodeWire(got)
	if err != nil {
		t.Fatalf("cannot serialize translated value: %v", err)
	}
	if diff := gocmp.Diff(want, string(wire)); diff != "" {
		t.Errorf("translated bytes (-want +got):\n%s", diff)
	}
}

func TestTranslateRequest(t *testing.T) {
	tests := map[string]struct{ model, input, want string }{
		"success: messages tools and effort": {
			"grok-4.5", `{
			"system":[{"type":"text","text":"Be useful."}],
			"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"text","text":"I will look that up."},{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"},{"type":"text","text":"Use this result."}]}],
			"tools":[{"name":"lookup","description":"Find things","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
			"output_config":{"effort":"high"},"max_tokens":456,"stream":true}`,
			`{"model":"grok-4.5","input":[` +
				`{"role":"developer","content":[{"type":"input_text","text":"Be useful."}]},` +
				`{"role":"user","content":[{"type":"input_text","text":"hello"}]},` +
				`{"role":"assistant","content":[{"type":"output_text","text":"I will look that up."}]},` +
				`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"},` +
				`{"type":"function_call_output","call_id":"call_1","output":"found"},` +
				`{"role":"user","content":[{"type":"input_text","text":"Use this result."}]}],` +
				`"max_output_tokens":456,"stream":true,"tools":[{"type":"function","name":"lookup","description":"Find things","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}],"reasoning":{"effort":"high"}}`,
		},
		"success: unknown model omits effort":                             {"grok-unknown", `{"output_config":{"effort":"high"}}`, `{"model":"grok-unknown","input":[],"stream":false}`},
		"success: tool only turn":                                         {"grok-4.5", `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]}]}`, `{"model":"grok-4.5","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}],"stream":false}`},
		"success: web search tools":                                       {"grok-4.5", `{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"lookup","input_schema":{"type":"object","properties":{}}}]}`, `{"model":"grok-4.5","input":[],"stream":false,"tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}`},
		"success: nested arguments retain wire order and number spelling": {"grok-4.7", `{"messages":[{"content":[{"type":"tool_use","input":{"z":-0,"10":"<","2":1e-6,"a":9007199254740993}}]}],"max_tokens":1e2}`, `{"model":"grok-4.7","input":[{"type":"function_call","arguments":"{\"2\":0.000001,\"10\":\"<\",\"z\":0,\"a\":9007199254740992}"}],"max_output_tokens":100,"stream":false}`},
		"success: unsupported content and sampling fields omitted":        {"grok-4.7", `{"system":[{"type":"text","text":"one"},{"type":"image"},{"type":"text","text":"two"}],"messages":[{"role":"assistant","content":[{"type":"thinking"}]},{"role":"other","content":[{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}]}],"stream":"true","top_p":0.2,"stop_sequences":[],"thinking":{},"temperature":1,"metadata":{},"tool_choice":{},"tools":[{}, {"name":"web_search"},{"name":"empty","description":""}]}`, `{"model":"grok-4.7","input":[{"role":"developer","content":[{"type":"input_text","text":"one\ntwo"}]},{"type":"function_call_output","call_id":"x","output":"ab"}],"stream":false,"tools":[{"type":"web_search"},{"type":"function","name":"empty","parameters":{"type":"object","properties":{}}}]}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := grok.TranslateRequest(decode(t, tt.input), tt.model)
			if err != nil {
				t.Fatal(err)
			}
			checkWire(t, tt.want, got)
		})
	}
}

const (
	model        = "claude-grok-4.5"
	zeroUsage    = `{"input_tokens":0,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0}}`
	textUsage    = `{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2}}`
	textResponse = `{"id":"resp_fixture","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":` + textUsage + `}`
)

func TestTranslateResponse(t *testing.T) {
	tests := map[string]struct{ input, want string }{
		"success: text and reasoning usage":             {textResponse, `{"id":"resp_fixture","type":"message","role":"assistant","model":"claude-grok-4.5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":` + textUsage + `}`},
		"success: web search result":                    {`{"output":[{"type":"web_search_call","id":"ws_1","action":{"type":"search","query":"Tokyo population","sources":[{"type":"url","url":"https://example.com/tokyo","title":"Tokyo data"}]}}]}`, `{"id":"msg_grok_123456789","type":"message","role":"assistant","model":"claude-grok-4.5","content":[{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"type":"search","query":"Tokyo population"}},{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[{"type":"web_search_result","title":"Tokyo data","url":"https://example.com/tokyo"}]}],"stop_reason":"end_turn","stop_sequence":null,"usage":` + zeroUsage + `}`},
		"success: function arguments and numeric usage": {`{"output":[{"type":"function_call","name":"run","call_id":"abc","arguments":"{\"b\":1.0,\"2\":-0,\"x\":\"<&>\"}"},{"type":"function_call","arguments":"broken"}],"usage":{"input_tokens":" 2 ","output_tokens":"oops","reasoning_tokens":"5"}}`, `{"id":"msg_grok_123456789","type":"message","role":"assistant","model":"claude-grok-4.5","content":[{"type":"tool_use","id":"abc","name":"run","input":{"2":0,"b":1,"x":"<&>"}},{"type":"tool_use","input":{}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":5}}}`},
		"success: null body":                            {`null`, `{"id":"msg_grok_123456789","type":"message","role":"assistant","model":"claude-grok-4.5","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":` + zeroUsage + `}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { checkWire(t, tt.want, grok.TranslateResponse(decode(t, tt.input), model, clock)) })
	}
}
