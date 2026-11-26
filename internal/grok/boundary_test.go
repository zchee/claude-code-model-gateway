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
	"errors"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

func toolMessage(input string) string {
	return `{"id":"msg_grok_123456789","type":"message","role":"assistant","model":"claude-grok-4.5","content":[{"type":"tool_use","input":` + input + `}],"stop_reason":"tool_use","stop_sequence":null,"usage":` + zeroUsage + `}`
}

// TestTranslateResponseArgumentUTF16 checks that function arguments are parsed from their UTF-16
// units as JSON.parse parses a JavaScript string, so an unpaired surrogate is neither replaced nor
// merged with U+FFFD.
func TestTranslateResponseArgumentUTF16(t *testing.T) {
	tests := map[string]struct{ arguments, input string }{
		"success: literal unpaired unit in a string literal": {`"{\"k\":\"\ud800\"}"`, `{"k":"\ud800"}`},
		"success: already escaped unpaired unit":             {`"{\"k\":\"\\ud800\"}"`, `{"k":"\ud800"}`},
		"success: lone unit and U+FFFD stay distinct names":  {`"{\"\ud800\":1,\"\ufffd\":2}"`, "{\"\\ud800\":1,\"\ufffd\":2}"},
		"success: escaped backslash then a lone unit":        {`"{\"k\":\"\\\\\ud800\"}"`, `{"k":"\\\ud800"}`},
		"error: lone unit after the escape backslash":        {`"{\"k\":\"\\\ud800\"}"`, `{}`},
		"error: lone unit outside a string literal":          {`"{\"k\":1}\ud800"`, `{}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			response := `{"output":[{"type":"function_call","arguments":` + tt.arguments + `}]}`
			checkWire(t, toolMessage(tt.input), grok.TranslateResponse(decode(t, response), model, clock))
		})
	}
}

// TestCoercionBoundaries pins where the source's String() and Number() throw on an object with its
// own toString member, and what each entry point does about it.
func TestCoercionBoundaries(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		tests := map[string]struct {
			input, want string
			wantErr     bool
		}{
			"error: system text object with own toString":        {input: `{"system":[{"type":"text","text":{"toString":"secret"}}]}`, wantErr: true},
			"error: message text array holding such an object":   {input: `{"messages":[{"role":"user","content":[{"type":"text","text":[1,{"toString":"secret"}]}]}]}`, wantErr: true},
			"error: nested array holding such an object":         {input: `{"messages":[{"role":"assistant","content":[{"type":"text","text":[[[{"toString":"secret"}]]]}]}]}`, wantErr: true},
			"success: own valueOf alone does not throw":          {input: `{"system":[{"type":"text","text":{"valueOf":"secret"}}]}`, want: `{"model":"grok-4.5","input":[{"role":"developer","content":[{"type":"input_text","text":"[object Object]"}]}],"stream":false}`},
			"success: tool input is serialized, not stringified": {input: `{"messages":[{"content":[{"type":"tool_use","input":{"toString":1}}]}]}`, want: `{"model":"grok-4.5","input":[{"type":"function_call","arguments":"{\"toString\":1}"}],"stream":false}`},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				got, err := grok.TranslateRequest(decode(t, tt.input), "grok-4.5")
				if !tt.wantErr {
					if err != nil {
						t.Fatal(err)
					}
					checkWire(t, tt.want, got)
					return
				}
				if _, ok := errors.AsType[*grok.CoercionError](err); !ok {
					t.Fatalf("error %v, want *grok.CoercionError", err)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Errorf("error %q repeats request content", err)
				}
				if diff := gocmp.Diff(jsonx.WireValue{}, got); diff != "" {
					t.Errorf("returned a partial request (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("response", func(t *testing.T) {
		tests := map[string]struct {
			input, want string
			fails       bool
		}{
			"error: usage count with own toString":             {input: `{"output":[],"usage":{"input_tokens":{"toString":1}}}`, fails: true},
			"error: reasoning fallback array with such object": {input: `{"usage":{"output_tokens_details":{},"reasoning_tokens":[{"toString":1}]}}`, fails: true},
			"success: caught inside the arguments parse":       {input: `{"output":[{"type":"function_call","arguments":{"toString":1}}]}`, want: toolMessage(`{}`)},
			"success: usage object without toString is zero":   {input: `{"usage":{"input_tokens":{"a":1},"output_tokens":[[]]}}`, want: `{"id":"msg_grok_123456789","type":"message","role":"assistant","model":"claude-grok-4.5","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":` + zeroUsage + `}`},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				got := grok.TranslateResponse(decode(t, tt.input), model, clock)
				if !tt.fails {
					checkWire(t, tt.want, got)
					return
				}
				if diff := gocmp.Diff(jsonx.WireValue{}, got); diff != "" {
					t.Errorf("translation did not fail (-want +got):\n%s", diff)
				}
				if _, err := jsonx.EncodeWire(got); err == nil {
					t.Error("the failure value can be encoded")
				}
			})
		}
	})

	t.Run("stream", func(t *testing.T) {
		start := event(created, messageStart("resp_fixture", zeroUsage))
		text := event(`{"type":"response.output_text.delta","delta":"hi"}`, blockStart("0", textBlock)+blockDelta("0", textHi))
		tests := map[string]struct {
			steps []exchange
			eof   string
		}{
			"error: text key before start changes nothing": {[]exchange{
				event(`{"type":"response.output_text.delta","item_id":{"toString":1},"delta":"hi"}`, ""),
				start, text,
			}, terminal("end_turn", zeroUsage)},
			"error: start usage leaves the message unstarted": {[]exchange{
				event(`{"type":"response.created","response":{"id":"r","usage":{"output_tokens":[{"toString":1}]}}}`, ""),
				event(`{"type":"response.output_text.delta","delta":"hi"}`, messageStart("msg_grok_123456789", zeroUsage)+blockStart("0", textBlock)+blockDelta("0", textHi)),
			}, terminal("end_turn", zeroUsage)},
			"error: completion usage leaves blocks open and the message unstopped": {[]exchange{
				start, text,
				event(`{"type":"response.completed","response":{"usage":{"input_tokens":{"toString":1}}}}`, ""),
				event(`{"type":"response.completed"}`, blockStop("0")+terminal("end_turn", zeroUsage)),
			}, ""},
			"error: upstream error message with own toString": {[]exchange{
				start,
				event(`{"type":"error","error":{"message":{"toString":"x"}}}`, ""),
			}, terminal("end_turn", zeroUsage)},
			"error: web search id with own toString": {[]exchange{
				start,
				event(`{"type":"response.output_item.done","item":{"type":"web_search_call","id":{"toString":1}}}`, ""),
				text,
			}, terminal("end_turn", zeroUsage)},
			"error: tool item id with own toString before start": {[]exchange{
				event(`{"type":"response.output_item.added","item":{"type":"function_call","id":{"toString":1}}}`, ""),
			}, ""},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) { checkStream(t, tt.steps, tt.eof) })
		}
	})
}

// TestStreamDepthFailureDoesNotRegisterTool checks the frame encoding limit: a content block whose
// start frame would hold 10001 nested containers is not written and not registered, so no later
// event closes a block the client never saw; 10000 containers still encode.
func TestStreamDepthFailureDoesNotRegisterTool(t *testing.T) {
	added := func(depth int) string {
		return `{"type":"response.output_item.added","item":{"type":"function_call","id":"t"},"name":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`
	}
	done := `{"type":"response.output_item.done","item_id":"t"}`
	start := messageStart("msg_grok_123456789", zeroUsage)
	tests := map[string]struct{ steps []exchange }{
		"error: frame of 10001 containers is dropped and the tool is not registered": {[]exchange{
			event(added(9999), start),
			event(done, ""),
			event(`{"type":"response.output_text.delta","delta":"hi"}`, blockStart("0", textBlock)+blockDelta("0", textHi)),
		}},
		"success: frame of 10000 containers is written": {[]exchange{
			event(added(9998), start+blockStart("0", `{"type":"tool_use","id":"t","name":`+strings.Repeat("[", 9998)+strings.Repeat("]", 9998)+`,"input":{}}`)),
			event(done, blockStop("0")),
			event(`{"type":"response.output_text.delta","delta":"hi"}`, blockStart("1", textBlock)+blockDelta("1", textHi)),
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { checkStream(t, tt.steps, terminal("end_turn", zeroUsage)) })
	}
}

// TestStreamInterleavedToolCalls follows two tool calls whose output indexes cross: a late
// argument delta still goes to its own, already closed, block, and a done event with only an
// output index closes the block registered under that index.
func TestStreamInterleavedToolCalls(t *testing.T) {
	first := `{"type":"function_call","id":"fa","call_id":"ca","name":"A"}`
	second := `{"type":"function_call","id":"fb","call_id":"cb","name":"B"}`
	tests := map[string]struct{ steps []exchange }{
		"success: crossed indexes and a late delta": {[]exchange{
			event(created, messageStart("resp_fixture", zeroUsage)),
			event(`{"type":"response.output_item.added","output_index":1,"item":`+first+`}`, blockStart("0", `{"type":"tool_use","id":"ca","name":"A","input":{}}`)),
			event(`{"type":"response.output_item.added","output_index":0,"item":`+second+`}`, blockStop("0")+blockStart("1", `{"type":"tool_use","id":"cb","name":"B","input":{}}`)),
			event(`{"type":"response.function_call_arguments.delta","item_id":"fa","delta":"{\"x\":1}"}`, blockDelta("0", `{"type":"input_json_delta","partial_json":"{\"x\":1}"}`)),
			event(`{"type":"response.function_call_arguments.delta","item_id":"fb","delta":"{}"}`, blockDelta("1", emptyArgs)),
			event(`{"type":"response.output_item.done","output_index":0}`, blockStop("1")),
			event(`{"type":"response.output_item.done","output_index":1}`, ""),
			event(`{"type":"response.function_call_arguments.done","item_id":"fa","arguments":"{\"x\":1}"}`, ""),
			event(`{"type":"response.completed","response":{"id":"resp_fixture","output":[`+first+`,`+second+`]}}`, terminal("tool_use", zeroUsage)),
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) { checkStream(t, tt.steps, "") })
	}
}

// TestStreamLifetime checks that events after response.completed are still translated until EOF,
// and that the zero value and a stream after End translate nothing without panicking.
func TestStreamLifetime(t *testing.T) {
	late := event(`{"type":"response.output_text.delta","delta":"hi"}`, blockStart("0", textBlock)+blockDelta("0", textHi))
	tests := map[string]struct {
		stream func() *grok.Stream
		steps  []exchange
		eof    string
	}{
		"success: late event after completion": {
			func() *grok.Stream { return grok.NewStream(model, clock) },
			[]exchange{event(created, messageStart("resp_fixture", zeroUsage)), event(`{"type":"response.completed"}`, terminal("end_turn", zeroUsage)), late},
			"",
		},
		"success: zero value is inert": {
			func() *grok.Stream { return &grok.Stream{} },
			[]exchange{{event(created, "").frame, ""}, {late.frame, ""}},
			"",
		},
		"success: ended stream is inert": {
			func() *grok.Stream {
				s := grok.NewStream(model, clock)
				s.Frame(event(created, "").frame)
				if got := s.End(); got != terminal("end_turn", zeroUsage) {
					t.Errorf("first End = %q", got)
				}
				return s
			},
			[]exchange{{event(created, "").frame, ""}, {late.frame, ""}},
			"",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := tt.stream()
			for i, step := range tt.steps {
				if diff := gocmp.Diff(step.want, s.Frame(step.frame)); diff != "" {
					t.Fatalf("frame %d (-want +got):\n%s", i, diff)
				}
			}
			if diff := gocmp.Diff(tt.eof, s.End()); diff != "" {
				t.Errorf("EOF (-want +got):\n%s", diff)
			}
		})
	}
}
