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
	"strings"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/sse"
)

type block struct {
	index   int
	stopped bool
}

type tool struct {
	block            *block
	sawArgumentDelta bool
}

// Stream holds translation state for one Responses event stream.
type Stream struct {
	model string
	now   func() time.Time

	started, stopped bool
	// active is the index of the open block that a new block closes, or -1.
	active int
	// blocks is in creation order, so a block's index is its position.
	blocks []*block
	// byKey, byOutput and byItem are the source's three Maps: block by text, tool or web search
	// key; block by output_index; tool by item id. The last two are keyed by mapKey.
	byKey    map[string]*block
	byOutput map[string]*block
	byItem   map[string]*tool

	out []byte
}

// NewStream starts a translator for the advertised model, using now for missing IDs.
func NewStream(model string, now func() time.Time) *Stream {
	return &Stream{
		model:    model,
		now:      now,
		active:   -1,
		byKey:    make(map[string]*block),
		byOutput: make(map[string]*block),
		byItem:   make(map[string]*tool),
	}
}

// Frame translates one scanned upstream frame into zero or more complete downstream frames.
// Unterminated tails do not dispatch events.
//
// The event is the first data line trimmed as String.prototype.trim trims; an empty line,
// [DONE], or data that JSON.parse rejects is skipped (lib/request-worker.js:363-377).
func (s *Stream) Frame(frame sse.Frame) string {
	if frame.Delimiter == "" {
		return ""
	}
	data, ok := frame.Data()
	if !ok {
		return ""
	}
	data = strings.TrimFunc(data, isSpace)
	if data == "" || data == "[DONE]" {
		return ""
	}
	event, err := jsonx.DecodeWire([]byte(data))
	if err != nil {
		return ""
	}
	s.event(event)
	return s.flush()
}

// End emits any final frames on orderly upstream EOF. Do not call End on transport failure.
//
// A stream that started and saw no response.completed ends with message_delta (end_turn, zero
// usage) and message_stop; blocks still open get no content_block_stop (lib/grok-backend.js:389).
func (s *Stream) End() string {
	if s.started && !s.stopped {
		s.stop(jsonx.WireValue{}, "end_turn")
	}
	return s.flush()
}

func (s *Stream) flush() string {
	out := string(s.out)
	s.out = s.out[:0]
	return out
}

// write appends `event: <type>\ndata: <JSON>\n\n`. Encoding fails only when an upstream value
// nests deeper than the wire policy allows; that frame is dropped.
func (s *Stream) write(typ string, members ...jsonx.Member) {
	data, err := jsonx.EncodeWire(obj(append([]jsonx.Member{member("type", str(typ))}, members...)...))
	if err != nil {
		return
	}
	s.out = append(s.out, "event: "...)
	s.out = append(s.out, typ...)
	s.out = append(s.out, "\ndata: "...)
	s.out = append(s.out, data...)
	s.out = append(s.out, "\n\n"...)
}

// event follows the transformer's event handler (lib/grok-backend.js:340-388).
func (s *Stream) event(event jsonx.WireValue) {
	typ := get(event, "type")
	response := get(event, "response")
	item := get(event, "item")
	switch {
	case isString(typ, "response.created"):
		s.start(response)
	case isString(typ, "response.output_text.delta"):
		s.start(response)
		key := coalesce(coalesce(get(event, "item_id"), get(event, "output_index")), str("default"))
		b := s.startBlock("text:", key, get(event, "output_index"), obj(member("type", str("text")), member("text", str(""))))
		s.write("content_block_delta", member("index", num(float64(b.index))), member("delta", obj(member("type", str("text_delta")), member("text", or(get(event, "delta"), str(""))))))
	case isString(typ, "response.output_item.added") && isString(get(item, "type"), "function_call"):
		s.start(response)
		s.toolBlock(event, item)
	case isString(typ, "response.function_call_arguments.delta"):
		s.start(response)
		t := s.toolBlock(event, jsonx.WireValue{})
		t.sawArgumentDelta = true
		s.argumentDelta(t, get(event, "delta"))
	case isString(typ, "response.function_call_arguments.done"):
		s.start(response)
		if t := s.toolBlock(event, jsonx.WireValue{}); !t.sawArgumentDelta {
			s.argumentDelta(t, get(event, "arguments"))
		}
	case isString(typ, "response.output_item.done") && isString(get(item, "type"), "web_search_call"):
		s.start(response)
		use, result := webSearchBlocks(item, s.now)
		id := get(use, "id")
		s.stopBlock(s.startBlock("web_search_use:", id, jsonx.WireValue{}, use))
		s.stopBlock(s.startBlock("web_search_result:", id, jsonx.WireValue{}, result))
	case isString(typ, "response.output_item.done"):
		// The source does not start the message here.
		var b *block
		if k, ok := mapKey(or(get(item, "id"), get(event, "item_id"))); ok && s.byItem[k] != nil {
			b = s.byItem[k].block
		} else if k, ok := mapKey(get(event, "output_index")); ok {
			b = s.byOutput[k]
		}
		s.stopBlock(b)
	case isString(typ, "response.completed"):
		s.start(response)
		for _, b := range s.blocks {
			s.stopBlock(b)
		}
		_, toolUse := responseContent(response, s.now)
		s.stop(get(response, "usage"), stopReason(toolUse))
	case isString(typ, "error") || isString(typ, "response.failed"):
		message := or(or(get(get(event, "error"), "message"), get(get(response, "error"), "message")), str("Grok response failed"))
		text := append(units("model-gateway: Grok upstream failed: "), jsString(message)...)
		s.write("error", member("error", obj(member("type", str("api_error")), member("message", strUnits(text)))))
	}
}

func (s *Stream) argumentDelta(t *tool, fragment jsonx.WireValue) {
	s.write("content_block_delta", member("index", num(float64(t.block.index))), member("delta", obj(member("type", str("input_json_delta")), member("partial_json", or(fragment, str(""))))))
}

func (s *Stream) start(response jsonx.WireValue) {
	if s.started {
		return
	}
	s.started = true
	id := get(response, "id")
	if !truthy(id) {
		id = fallbackID("msg_grok_", s.now)
	}
	s.write("message_start", member("message", obj(
		member("id", id),
		member("type", str("message")),
		member("role", str("assistant")),
		member("model", str(s.model)),
		member("content", array(nil)),
		member("stop_reason", null()),
		member("stop_sequence", null()),
		member("usage", usage(get(response, "usage"))),
	)))
}

func (s *Stream) stop(u jsonx.WireValue, reason string) {
	if s.stopped {
		return
	}
	s.stopped = true
	s.write("message_delta", member("delta", obj(member("stop_reason", str(reason)), member("stop_sequence", null()))), member("usage", usage(u)))
	s.write("message_stop")
}

func (s *Stream) stopBlock(b *block) {
	if b == nil || b.stopped {
		return
	}
	b.stopped = true
	s.write("content_block_stop", member("index", num(float64(b.index))))
	if s.active == b.index {
		s.active = -1
	}
}

// startBlock returns the block of the key `${prefix}${name}`, opening it after closing the
// active block when it does not exist yet. An existing block is returned without any frame.
func (s *Stream) startBlock(prefix string, name, outputIndex, content jsonx.WireValue) *block {
	key := prefix + unitsKey(jsString(name))
	if b, ok := s.byKey[key]; ok {
		return b
	}
	if s.active >= 0 {
		s.stopBlock(s.blocks[s.active])
	}
	b := &block{index: len(s.blocks)}
	s.blocks = append(s.blocks, b)
	s.byKey[key] = b
	if outputIndex.Kind != 0 && outputIndex.Kind != 'n' {
		if k, ok := mapKey(outputIndex); ok {
			s.byOutput[k] = b
		}
	}
	s.active = b.index
	s.write("content_block_start", member("index", num(float64(b.index))), member("content_block", content))
	return b
}

// toolBlock follows lib/grok-backend.js:316-325. item is the added output item, or undefined for
// argument events.
func (s *Stream) toolBlock(event, item jsonx.WireValue) *tool {
	itemID := or(or(or(get(item, "id"), get(event, "item_id")), get(event, "call_id")), str("tool"))
	k, keyed := mapKey(itemID)
	if t := s.byItem[k]; keyed && t != nil {
		return t
	}
	callID := or(or(get(item, "call_id"), get(event, "call_id")), itemID)
	name := or(or(get(item, "name"), get(event, "name")), str(""))
	b := s.startBlock("tool:", itemID, get(event, "output_index"), obj(
		member("type", str("tool_use")),
		member("id", callID),
		member("name", name),
		member("input", obj()),
	))
	t := &tool{block: b}
	if keyed {
		s.byItem[k] = t
	}
	return t
}
