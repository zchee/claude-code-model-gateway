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
//
// Create a Stream with NewStream; the zero value, and a Stream after End, translate nothing. A
// Stream belongs to one goroutine, which feeds it the frames of one upstream response in order,
// and it must not be copied. Late events after response.completed are still translated until End,
// as in the source.
//
// An event is handled whole or not at all where the source's String() or Number() would throw: the
// worker catches that exception per event (lib/request-worker.js:363-377), and the translator finds
// it before the event changes anything, so the event yields no frame and no state change. When a
// frame cannot be encoded (an upstream value deeper than the wire policy's nesting limit), handling
// of that event stops at that frame: frames already produced for the event are kept, and the
// message start or content block the frame would have announced is not registered.
//
// An `event: error` frame relays the upstream error message to the client, as the source does;
// that is response content passed through, not a Go error or a log line.
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

// NewStream starts a translator for the advertised model, using now for missing IDs. now must not
// be nil.
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
	if s.byKey == nil || frame.Delimiter == "" {
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
	// An error only ends the handling of this event; see the Stream documentation.
	_ = s.event(event)
	return s.flush()
}

// End emits any final frames on orderly upstream EOF and releases the stream's state. Do not call
// End on transport failure.
//
// A stream that started and saw no response.completed ends with message_delta (end_turn, zero
// usage) and message_stop; blocks still open get no content_block_stop (lib/grok-backend.js:389).
func (s *Stream) End() string {
	if s.byKey == nil {
		return ""
	}
	if s.started && !s.stopped {
		zero, _ := usage(jsonx.WireValue{}) // Absent fields never throw.
		_ = s.stop(zero, "end_turn")
	}
	out := s.flush()
	*s = Stream{}
	return out
}

func (s *Stream) flush() string {
	out := string(s.out)
	s.out = s.out[:0]
	return out
}

// write appends `event: <type>\ndata: <JSON>\n\n`, or nothing when the frame cannot be encoded.
func (s *Stream) write(typ string, members ...jsonx.Member) error {
	data, err := jsonx.EncodeWire(obj(append([]jsonx.Member{member("type", str(typ))}, members...)...))
	if err != nil {
		return err
	}
	s.out = append(s.out, "event: "...)
	s.out = append(s.out, typ...)
	s.out = append(s.out, "\ndata: "...)
	s.out = append(s.out, data...)
	s.out = append(s.out, "\n\n"...)
	return nil
}

// event follows the transformer's event handler (lib/grok-backend.js:340-388). Each case computes
// every String() and Number() the source applies before its first frame or state change.
func (s *Stream) event(event jsonx.WireValue) error {
	typ := get(event, "type")
	response := get(event, "response")
	item := get(event, "item")
	switch {
	case isString(typ, "response.created"):
		opening, err := s.opening(response)
		if err != nil {
			return err
		}
		return s.start(opening)
	case isString(typ, "response.output_text.delta"):
		opening, err := s.opening(response)
		if err != nil {
			return err
		}
		name, err := toString(coalesce(coalesce(get(event, "item_id"), get(event, "output_index")), str("default")))
		if err != nil {
			return err
		}
		if err := s.start(opening); err != nil {
			return err
		}
		b, err := s.startBlock("text:"+unitsKey(name), get(event, "output_index"), obj(member("type", str("text")), member("text", str(""))))
		if err != nil {
			return err
		}
		return s.write("content_block_delta", member("index", num(float64(b.index))), member("delta", obj(member("type", str("text_delta")), member("text", or(get(event, "delta"), str(""))))))
	case isString(typ, "response.output_item.added") && isString(get(item, "type"), "function_call"):
		_, err := s.toolEvent(event, item, response)
		return err
	case isString(typ, "response.function_call_arguments.delta"):
		t, err := s.toolEvent(event, jsonx.WireValue{}, response)
		if err != nil {
			return err
		}
		t.sawArgumentDelta = true
		return s.argumentDelta(t, get(event, "delta"))
	case isString(typ, "response.function_call_arguments.done"):
		t, err := s.toolEvent(event, jsonx.WireValue{}, response)
		if err != nil || t.sawArgumentDelta {
			return err
		}
		return s.argumentDelta(t, get(event, "arguments"))
	case isString(typ, "response.output_item.done") && isString(get(item, "type"), "web_search_call"):
		opening, err := s.opening(response)
		if err != nil {
			return err
		}
		use, result := webSearchBlocks(item, s.now)
		name, err := toString(get(use, "id"))
		if err != nil {
			return err
		}
		if err := s.start(opening); err != nil {
			return err
		}
		b, err := s.startBlock("web_search_use:"+unitsKey(name), jsonx.WireValue{}, use)
		if err != nil {
			return err
		}
		if err := s.stopBlock(b); err != nil {
			return err
		}
		if b, err = s.startBlock("web_search_result:"+unitsKey(name), jsonx.WireValue{}, result); err != nil {
			return err
		}
		return s.stopBlock(b)
	case isString(typ, "response.output_item.done"):
		// The source does not start the message here.
		var b *block
		if k, ok := mapKey(or(get(item, "id"), get(event, "item_id"))); ok && s.byItem[k] != nil {
			b = s.byItem[k].block
		} else if k, ok := mapKey(get(event, "output_index")); ok {
			b = s.byOutput[k]
		}
		return s.stopBlock(b)
	case isString(typ, "response.completed"):
		opening, err := s.opening(response)
		if err != nil {
			return err
		}
		var closing jsonx.WireValue
		if !s.stopped {
			if closing, err = usage(get(response, "usage")); err != nil {
				return err
			}
		}
		_, toolUse := responseContent(response, s.now)
		if err := s.start(opening); err != nil {
			return err
		}
		for _, b := range s.blocks {
			if err := s.stopBlock(b); err != nil {
				return err
			}
		}
		return s.stop(closing, stopReason(toolUse))
	case isString(typ, "error") || isString(typ, "response.failed"):
		message, err := toString(or(or(get(get(event, "error"), "message"), get(get(response, "error"), "message")), str("Grok response failed")))
		if err != nil {
			return err
		}
		text := append(units("model-gateway: Grok upstream failed: "), message...)
		return s.write("error", member("error", obj(member("type", str("api_error")), member("message", strUnits(text)))))
	}
	return nil
}

func (s *Stream) argumentDelta(t *tool, fragment jsonx.WireValue) error {
	return s.write("content_block_delta", member("index", num(float64(t.block.index))), member("delta", obj(member("type", str("input_json_delta")), member("partial_json", or(fragment, str(""))))))
}

// opening builds the message_start message for response, or nothing once the message started.
func (s *Stream) opening(response jsonx.WireValue) (jsonx.WireValue, error) {
	if s.started {
		return jsonx.WireValue{}, nil
	}
	u, err := usage(get(response, "usage"))
	if err != nil {
		return jsonx.WireValue{}, err
	}
	id := get(response, "id")
	if !truthy(id) {
		id = fallbackID("msg_grok_", s.now)
	}
	return obj(
		member("id", id),
		member("type", str("message")),
		member("role", str("assistant")),
		member("model", str(s.model)),
		member("content", array(nil)),
		member("stop_reason", null()),
		member("stop_sequence", null()),
		member("usage", u),
	), nil
}

// start writes message_start once; the message counts as started only when the frame is written.
func (s *Stream) start(opening jsonx.WireValue) error {
	if s.started {
		return nil
	}
	if err := s.write("message_start", member("message", opening)); err != nil {
		return err
	}
	s.started = true
	return nil
}

func (s *Stream) stop(u jsonx.WireValue, reason string) error {
	if s.stopped {
		return nil
	}
	if err := s.write("message_delta", member("delta", obj(member("stop_reason", str(reason)), member("stop_sequence", null()))), member("usage", u)); err != nil {
		return err
	}
	if err := s.write("message_stop"); err != nil {
		return err
	}
	s.stopped = true
	return nil
}

func (s *Stream) stopBlock(b *block) error {
	if b == nil || b.stopped {
		return nil
	}
	if err := s.write("content_block_stop", member("index", num(float64(b.index)))); err != nil {
		return err
	}
	b.stopped = true
	if s.active == b.index {
		s.active = -1
	}
	return nil
}

// startBlock returns the block of key, opening it after closing the active block when it does not
// exist yet. An existing block is returned without any frame. A block is registered only once its
// content_block_start frame is written.
func (s *Stream) startBlock(key string, outputIndex, content jsonx.WireValue) (*block, error) {
	if b, ok := s.byKey[key]; ok {
		return b, nil
	}
	if s.active >= 0 {
		if err := s.stopBlock(s.blocks[s.active]); err != nil {
			return nil, err
		}
	}
	b := &block{index: len(s.blocks)}
	if err := s.write("content_block_start", member("index", num(float64(b.index))), member("content_block", content)); err != nil {
		return nil, err
	}
	s.blocks = append(s.blocks, b)
	s.byKey[key] = b
	if outputIndex.Kind != 0 && outputIndex.Kind != 'n' {
		if k, ok := mapKey(outputIndex); ok {
			s.byOutput[k] = b
		}
	}
	s.active = b.index
	return b, nil
}

// toolEvent starts the message and returns the tool of event, opening its block when it is new
// (lib/grok-backend.js:316-325). item is the added output item, or undefined for argument events.
func (s *Stream) toolEvent(event, item, response jsonx.WireValue) (*tool, error) {
	opening, err := s.opening(response)
	if err != nil {
		return nil, err
	}
	itemID := or(or(or(get(item, "id"), get(event, "item_id")), get(event, "call_id")), str("tool"))
	k, keyed := mapKey(itemID)
	existing := s.byItem[k]
	var name []uint16
	if !keyed || existing == nil {
		if name, err = toString(itemID); err != nil {
			return nil, err
		}
	}
	if err := s.start(opening); err != nil {
		return nil, err
	}
	if keyed && existing != nil {
		return existing, nil
	}
	callID := or(or(get(item, "call_id"), get(event, "call_id")), itemID)
	b, err := s.startBlock("tool:"+unitsKey(name), get(event, "output_index"), obj(
		member("type", str("tool_use")),
		member("id", callID),
		member("name", or(or(get(item, "name"), get(event, "name")), str(""))),
		member("input", obj()),
	))
	if err != nil {
		return nil, err
	}
	t := &tool{block: b}
	if keyed {
		s.byItem[k] = t
	}
	return t, nil
}
