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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/sse"
)

type exchange struct {
	frame sse.Frame
	want  string
}

func event(payload, want string) exchange {
	return exchange{sse.Frame{Body: "data: " + payload, Delimiter: "\n\n"}, want}
}

func downstream(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

func messageStart(id, usage string) string {
	return downstream("message_start", `{"type":"message_start","message":{"id":"`+id+`","type":"message","role":"assistant","model":"claude-grok-4.5","content":[],"stop_reason":null,"stop_sequence":null,"usage":`+usage+`}}`)
}

func terminal(reason, usage string) string {
	return downstream("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+reason+`","stop_sequence":null},"usage":`+usage+`}`) + downstream("message_stop", `{"type":"message_stop"}`)
}

func blockStart(index, block string) string {
	return downstream("content_block_start", `{"type":"content_block_start","index":`+index+`,"content_block":`+block+`}`)
}

func blockStop(index string) string {
	return downstream("content_block_stop", `{"type":"content_block_stop","index":`+index+`}`)
}

func blockDelta(index, delta string) string {
	return downstream("content_block_delta", `{"type":"content_block_delta","index":`+index+`,"delta":`+delta+`}`)
}

const (
	created       = `{"type":"response.created","response":{"id":"resp_fixture"}}`
	webItem       = `{"type":"web_search_call","id":"ws_1","action":{"type":"search","query":"Tokyo population","sources":[{"type":"url","url":"https://example.com/tokyo"}]}}`
	toolItem      = `{"type":"function_call","id":"fc_resp_0","call_id":"call_fixture_0","name":"Read"}`
	toolBlock     = `{"type":"tool_use","id":"call_fixture_0","name":"Read","input":{}}`
	textBlock     = `{"type":"text","text":""}`
	textHi        = `{"type":"text_delta","text":"hi"}`
	argumentDelta = `{"type":"input_json_delta","partial_json":"{\"file_path\":\"config.toml\"}"}`
	emptyArgs     = `{"type":"input_json_delta","partial_json":"{}"}`
	toolComplete  = `{"type":"response.completed","response":{"id":"resp_fixture","output":[` + toolItem + `]}}`
)

func checkStream(t *testing.T, steps []exchange, eof string) {
	t.Helper()
	stream := grok.NewStream(model, clock)
	for i, step := range steps {
		if diff := gocmp.Diff(step.want, stream.Frame(step.frame)); diff != "" {
			t.Fatalf("frame %d %q (-want +got):\n%s", i, step.frame.Body, diff)
		}
	}
	if diff := gocmp.Diff(eof, stream.End()); diff != "" {
		t.Fatalf("EOF (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff("", stream.End()); diff != "" {
		t.Fatalf("duplicate EOF (-want +got):\n%s", diff)
	}
}

func TestStream(t *testing.T) {
	start := event(created, messageStart("resp_fixture", zeroUsage))
	tests := map[string]struct {
		steps []exchange
		eof   string
	}{
		"success: text completion": {[]exchange{
			event(`{"type":"response.created","response":`+textResponse+`}`, messageStart("resp_fixture", textUsage)),
			event(`{"type":"response.output_text.delta","delta":"hi","response":`+textResponse+`}`, blockStart("0", textBlock)+blockDelta("0", textHi)),
			event(`{"type":"response.completed","response":`+textResponse+`}`, blockStop("0")+terminal("end_turn", textUsage)),
		}, ""},
		"success: text deltas reuse the item block": {[]exchange{
			start,
			event(`{"type":"response.output_text.delta","item_id":"first","output_index":0,"delta":"hi"}`, blockStart("0", textBlock)+blockDelta("0", textHi)),
			event(`{"type":"response.output_text.delta","item_id":"first","output_index":0,"delta":"hi"}`, blockDelta("0", textHi)),
			event(`{"type":"response.output_text.delta","item_id":"second","output_index":0,"delta":"bye"}`, blockStop("0")+blockStart("1", textBlock)+blockDelta("1", `{"type":"text_delta","text":"bye"}`)),
			event(`{"type":"response.completed"}`, blockStop("1")+terminal("end_turn", zeroUsage)),
			event(`{"type":"response.completed"}`, ""),
		}, ""},
		"success: successive argument fragments keep their index": {[]exchange{
			start,
			event(`{"type":"response.output_item.added","output_index":1,"item":`+toolItem+`}`, blockStart("0", toolBlock)),
			event(`{"type":"response.function_call_arguments.delta","item_id":"fc_resp_0","delta":"{\"n\":"}`, blockDelta("0", `{"type":"input_json_delta","partial_json":"{\"n\":"}`)),
			event(`{"type":"response.function_call_arguments.delta","item_id":"fc_resp_0","delta":"1"}`, blockDelta("0", `{"type":"input_json_delta","partial_json":"1"}`)),
			event(`{"type":"response.function_call_arguments.delta","item_id":"fc_resp_0","delta":"1"}`, blockDelta("0", `{"type":"input_json_delta","partial_json":"1"}`)),
			event(`{"type":"response.function_call_arguments.delta","item_id":"fc_resp_0","delta":"}"}`, blockDelta("0", `{"type":"input_json_delta","partial_json":"}"}`)),
			event(`{"type":"response.function_call_arguments.done","item_id":"fc_resp_0","arguments":"{\"n\":11}"}`, ""),
			event(`{"type":"response.output_item.done","output_index":1,"item":`+toolItem+`}`, blockStop("0")),
			event(`{"type":"response.output_item.done","output_index":1,"item":`+toolItem+`}`, ""),
			event(toolComplete, terminal("tool_use", zeroUsage)),
		}, ""},
		"success: repeated arguments done without deltas emits again": {[]exchange{
			start,
			event(`{"type":"response.output_item.added","output_index":1,"item":`+toolItem+`}`, blockStart("0", toolBlock)),
			event(`{"type":"response.function_call_arguments.done","item_id":"fc_resp_0","arguments":"{}"}`, blockDelta("0", emptyArgs)),
			event(`{"type":"response.function_call_arguments.done","item_id":"fc_resp_0","arguments":"{}"}`, blockDelta("0", emptyArgs)),
			event(toolComplete, blockStop("0")+terminal("tool_use", zeroUsage)),
		}, ""},
		"success: web search blocks": {[]exchange{
			start,
			event(`{"type":"response.output_item.added","output_index":0,"item":`+webItem+`}`, ""),
			event(`{"type":"response.output_item.done","output_index":0,"item":`+webItem+`}`, blockStart("0", `{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"type":"search","query":"Tokyo population"}}`)+blockStop("0")+blockStart("1", `{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[{"type":"web_search_result","title":"https://example.com/tokyo","url":"https://example.com/tokyo"}]}`)+blockStop("1")),
			event(`{"type":"response.output_item.done","output_index":0,"item":`+webItem+`}`, ""),
			event(`{"type":"response.completed","response":{"id":"resp_fixture","output":[`+webItem+`]}}`, terminal("end_turn", zeroUsage)),
		}, ""},
		"success: function call identity": {[]exchange{
			start,
			event(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_resp_0","call_id":"call_fixture_0","name":"Read","arguments":"","status":"in_progress"}}`, blockStart("0", toolBlock)),
			event(`{"type":"response.function_call_arguments.delta","item_id":"fc_resp_0","output_index":1,"delta":"{\"file_path\":\"config.toml\"}"}`, blockDelta("0", argumentDelta)),
			event(`{"type":"response.function_call_arguments.done","item_id":"fc_resp_0","arguments":"{\"file_path\":\"config.toml\"}"}`, ""),
			event(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_resp_0"}}`, blockStop("0")), event(toolComplete, terminal("tool_use", zeroUsage)),
		}, ""},
		"success: malformed and unknown frames ignored": {[]exchange{
			{sse.Frame{Body: ": heartbeat", Delimiter: "\r\n\r\n"}, ""},
			event(" \t[DONE] ", ""), event("", ""), event("{broken", ""), event(`{"type":"new.event"}`, ""),
			{sse.Frame{Body: "data: invalid\r\ndata: " + created, Delimiter: "\r\n\r\n"}, ""},
			{sse.Frame{Body: "event: ignored\r\ndata: " + created + string(rune(0xfeff)) + "\r\ndata: invalid", Delimiter: "\r\n\r\n"}, messageStart("resp_fixture", zeroUsage)},
			event(created, ""),
		}, terminal("end_turn", zeroUsage)},
		"error: failed response during text": {[]exchange{
			start, event(`{"type":"response.output_text.delta","delta":"hi"}`, blockStart("0", textBlock)+blockDelta("0", textHi)),
			event(`{"type":"response.failed","response":{"error":{"message":"busy"}}}`, downstream("error", `{"type":"error","error":{"type":"api_error","message":"model-gateway: Grok upstream failed: busy"}}`)),
		}, terminal("end_turn", zeroUsage)},
		"error: upstream error before start":             {[]exchange{event(`{"type":"error"}`, downstream("error", `{"type":"error","error":{"type":"api_error","message":"model-gateway: Grok upstream failed: Grok response failed"}}`))}, ""},
		"success: incomplete before start emits nothing": {[]exchange{event(`{"type":"response.incomplete"}`, "")}, ""},
		"success: incomplete gets normal EOF":            {[]exchange{start, event(`{"type":"response.incomplete"}`, "")}, terminal("end_turn", zeroUsage)},
		"success: unterminated completion ignored":       {[]exchange{start, {sse.Frame{Body: "data: " + toolComplete}, ""}}, terminal("end_turn", zeroUsage)},
		"success: unterminated start ignored":            {[]exchange{{sse.Frame{Body: "data: " + created}, ""}}, ""},
		"success: implicit start uses clock":             {[]exchange{event(`{"type":"response.output_text.delta","delta":"hi"}`, messageStart("msg_grok_123456789", zeroUsage)+blockStart("0", textBlock)+blockDelta("0", textHi))}, terminal("end_turn", zeroUsage)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { checkStream(t, tt.steps, tt.eof) })
	}
}

func TestStreamBlockOrder(t *testing.T) {
	tests := map[string]struct{ steps []exchange }{
		"success: text before tool": {[]exchange{
			event(`{"type":"response.output_text.delta","output_index":0,"delta":"before"}`, blockStart("0", textBlock)+blockDelta("0", `{"type":"text_delta","text":"before"}`)),
			event(`{"type":"response.output_item.added","output_index":1,"item":`+toolItem+`}`, blockStop("0")+blockStart("1", toolBlock)),
			event(`{"type":"response.function_call_arguments.done","item_id":"fc_resp_0","arguments":"{}"}`, blockDelta("1", emptyArgs)),
			event(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_resp_0"}}`, blockStop("1")),
		}},
		"success: tool before text": {[]exchange{
			event(`{"type":"response.output_item.added","output_index":0,"item":`+toolItem+`}`, blockStart("0", toolBlock)),
			event(`{"type":"response.function_call_arguments.done","item_id":"fc_resp_0","arguments":"{}"}`, blockDelta("0", emptyArgs)),
			event(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_resp_0"}}`, blockStop("0")),
			event(`{"type":"response.output_text.delta","output_index":1,"delta":"after"}`, blockStart("1", textBlock)+blockDelta("1", `{"type":"text_delta","text":"after"}`)),
			event(`{"type":"response.output_item.done","output_index":1,"item":{"type":"message"}}`, blockStop("1")),
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			steps := append([]exchange{event(created, messageStart("resp_fixture", zeroUsage))}, tt.steps...)
			steps = append(steps, event(toolComplete, terminal("tool_use", zeroUsage)))
			checkStream(t, steps, "")
		})
	}
}
