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

// Package grok translates between Anthropic messages and Grok Responses wire values.
package grok

import (
	"errors"
	"strconv"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// reasoningModel reports whether the source's built-in model table marks id as accepting a
// reasoning effort. The request translation consults only that table, never the model cache
// (lib/grok-backend.js:12-16,221).
func reasoningModel(id string) bool {
	switch id {
	case "grok-4.7", "grok-4.6", "grok-4.5":
		return true
	}
	return false
}

// TranslateRequest builds a Responses input for the backend model.
//
// The call fails where the source's translateRequest throws: on a null entry in messages, and with
// a *CoercionError when String() of a text value would throw. It then returns a zero WireValue
// and an error that holds no request content, never a partial request. The result may share
// subtrees with payload; treat both as read-only.
func TranslateRequest(payload jsonx.WireValue, model string) (jsonx.WireValue, error) {
	effort := get(get(payload, "output_config"), "effort")
	input, err := translateInput(payload)
	if err != nil {
		return jsonx.WireValue{}, err
	}
	members := []jsonx.Member{
		member("model", str(model)),
		member("input", array(input)),
		member("max_output_tokens", get(payload, "max_tokens")),
		member("stream", boolean(get(payload, "stream").Kind == 't')),
	}
	if tools := translateTools(get(payload, "tools")); len(tools) > 0 {
		members = append(members, member("tools", array(tools)))
	}
	if effort.Kind == '"' && reasoningModel(model) {
		members = append(members, member("reasoning", obj(member("effort", effort))))
	}
	return obj(members...), nil
}

// translateInput follows lib/grok-backend.js:167-190: the system text becomes a developer
// message, and each message's text parts are grouped between its function items.
func translateInput(payload jsonx.WireValue) ([]jsonx.WireValue, error) {
	input := []jsonx.WireValue{}
	var systemText []uint16
	switch system := get(payload, "system"); system.Kind {
	case '"':
		systemText = system.Text
	case '[':
		joined := 0
		for _, block := range system.Elements {
			if !isString(get(block, "type"), "text") {
				continue
			}
			if joined++; joined > 1 {
				systemText = append(systemText, '\n')
			}
			if text := get(block, "text"); text.Kind != 0 && text.Kind != 'n' {
				part, err := toString(text)
				if err != nil {
					return nil, err
				}
				systemText = append(systemText, part...)
			}
		}
	}
	if len(systemText) > 0 {
		part, _ := textPart("input_text", strUnits(systemText)) // A string never throws.
		input = append(input, obj(member("role", str("developer")), member("content", array([]jsonx.WireValue{part}))))
	}
	messages := get(payload, "messages")
	if messages.Kind != '[' {
		return input, nil
	}
	for _, message := range messages.Elements {
		// The source reads message.role without optional chaining, which throws on null.
		if message.Kind == 'n' {
			return nil, errors.New("grok: request messages contain a null entry")
		}
		role := "user"
		if isString(get(message, "role"), "assistant") {
			role = "assistant"
		}
		items, err := translateContent(message, role == "assistant")
		if err != nil {
			return nil, err
		}
		var content []jsonx.WireValue
		flush := func() {
			if len(content) > 0 {
				input = append(input, obj(member("role", str(role)), member("content", array(content))))
				content = nil
			}
		}
		for _, item := range items {
			if t := get(item, "type"); isString(t, "function_call") || isString(t, "function_call_output") {
				flush()
				input = append(input, item)
			} else {
				content = append(content, item)
			}
		}
		flush()
	}
	return input, nil
}

// textPart builds an input_text or output_text part from the source's `String(text || "")`.
func textPart(kind string, text jsonx.WireValue) (jsonx.WireValue, error) {
	var u []uint16
	if truthy(text) {
		var err error
		if u, err = toString(text); err != nil {
			return jsonx.WireValue{}, err
		}
	}
	return obj(member("type", str(kind)), member("text", strUnits(u))), nil
}

// translateContent follows lib/grok-backend.js:155-165. Blocks other than strings, text,
// tool_use and tool_result are dropped.
func translateContent(message jsonx.WireValue, assistant bool) ([]jsonx.WireValue, error) {
	content := get(message, "content")
	blocks := []jsonx.WireValue{content}
	if content.Kind == '[' {
		blocks = content.Elements
	}
	kind := "input_text"
	if assistant {
		kind = "output_text"
	}
	var items []jsonx.WireValue
	for _, block := range blocks {
		switch typ := get(block, "type"); {
		case block.Kind == '"', isString(typ, "text"):
			text := block
			if block.Kind != '"' {
				text = get(block, "text")
			}
			part, err := textPart(kind, text)
			if err != nil {
				return nil, err
			}
			items = append(items, part)
		case isString(typ, "tool_use"):
			arguments, err := jsonx.EncodeWire(or(get(block, "input"), obj()))
			if err != nil {
				return nil, errors.New("grok: a tool_use input cannot be serialized")
			}
			items = append(items, obj(
				member("type", str("function_call")),
				member("call_id", get(block, "id")),
				member("name", get(block, "name")),
				member("arguments", str(string(arguments))),
			))
		case isString(typ, "tool_result"):
			items = append(items, obj(
				member("type", str("function_call_output")),
				member("call_id", get(block, "tool_use_id")),
				member("output", strUnits(textFromContent(get(block, "content")))),
			))
		}
	}
	return items, nil
}

// textFromContent keeps a string as it is and joins the string texts of an array's text blocks.
func textFromContent(content jsonx.WireValue) []uint16 {
	switch content.Kind {
	case '"':
		return content.Text
	case '[':
		var out []uint16
		for _, block := range content.Elements {
			if text := get(block, "text"); isString(get(block, "type"), "text") && text.Kind == '"' {
				out = append(out, text.Text...)
			}
		}
		return out
	}
	return nil
}

func isWebSearchTool(tool jsonx.WireValue) bool {
	return hasPrefix(get(tool, "type"), "web_search") || isString(get(tool, "name"), "web_search") && !truthy(get(tool, "input_schema"))
}

// translateTools follows lib/grok-backend.js:192-209.
func translateTools(tools jsonx.WireValue) []jsonx.WireValue {
	if tools.Kind != '[' {
		return nil
	}
	var out []jsonx.WireValue
	for _, tool := range tools.Elements {
		if isWebSearchTool(tool) {
			out = append(out, obj(member("type", str("web_search"))))
			continue
		}
		name := get(tool, "name")
		if !truthy(name) {
			continue
		}
		description := get(tool, "description")
		if !truthy(description) {
			description = jsonx.WireValue{}
		}
		out = append(out, obj(
			member("type", str("function")),
			member("name", name),
			member("description", description),
			member("parameters", or(get(tool, "input_schema"), obj(member("type", str("object")), member("properties", obj())))),
		))
	}
	return out
}

// usage is anthropicUsage (lib/grok-backend.js:225-231). It fails where Number() throws.
func usage(u jsonx.WireValue) (jsonx.WireValue, error) {
	var counts [3]float64
	fields := [3]jsonx.WireValue{
		get(u, "input_tokens"),
		get(u, "output_tokens"),
		coalesce(get(get(u, "output_tokens_details"), "reasoning_tokens"), get(u, "reasoning_tokens")),
	}
	for i, field := range fields {
		n, err := toCount(field)
		if err != nil {
			return jsonx.WireValue{}, err
		}
		counts[i] = n
	}
	return obj(
		member("input_tokens", num(counts[0])),
		member("output_tokens", num(counts[1])),
		member("output_tokens_details", obj(member("reasoning_tokens", num(counts[2])))),
	), nil
}

// fallbackID is the source's `${prefix}${Date.now()}`.
func fallbackID(prefix string, now func() time.Time) jsonx.WireValue {
	return str(prefix + strconv.FormatInt(now().UnixMilli(), 10))
}

// webSearchBlocks follows lib/grok-backend.js:233-247: the action minus its sources becomes the
// tool input, and every source with a string url becomes a result titled by its title or url.
func webSearchBlocks(item jsonx.WireValue, now func() time.Time) (use, result jsonx.WireValue) {
	action := get(item, "action")
	input := obj()
	switch action.Kind {
	case '{':
		for _, m := range action.Members {
			if !nameIs(m.Name, "sources") {
				input.Members = append(input.Members, m)
			}
		}
	case '[':
		// Spreading an array copies its elements under their index names.
		for i, el := range action.Elements {
			input.Members = append(input.Members, member(strconv.Itoa(i), el))
		}
	}
	var results []jsonx.WireValue
	if sources := get(action, "sources"); sources.Kind == '[' {
		for _, source := range sources.Elements {
			url := get(source, "url")
			if url.Kind != '"' {
				continue
			}
			results = append(results, obj(
				member("type", str("web_search_result")),
				member("title", or(get(source, "title"), url)),
				member("url", url),
			))
		}
	}
	id := get(item, "id")
	if !truthy(id) {
		id = fallbackID("web_search_", now)
	}
	use = obj(member("type", str("server_tool_use")), member("id", id), member("name", str("web_search")), member("input", input))
	result = obj(member("type", str("web_search_tool_result")), member("tool_use_id", id), member("content", array(results)))
	return use, result
}

// responseContent follows lib/grok-backend.js:249-265 and reports whether a tool_use block was
// produced. Function arguments become an empty input wherever the source's try block catches:
// String() throwing on them, or JSON.parse rejecting them.
func responseContent(response jsonx.WireValue, now func() time.Time) (content []jsonx.WireValue, toolUse bool) {
	content = []jsonx.WireValue{}
	output := get(response, "output")
	if output.Kind != '[' {
		return content, false
	}
	for _, item := range output.Elements {
		typ := get(item, "type")
		if isString(typ, "message") {
			if parts := get(item, "content"); parts.Kind == '[' {
				for _, part := range parts.Elements {
					if isString(get(part, "type"), "output_text") {
						content = append(content, obj(member("type", str("text")), member("text", or(get(part, "text"), str("")))))
					}
				}
			}
		}
		if isString(typ, "web_search_call") {
			use, result := webSearchBlocks(item, now)
			content = append(content, use, result)
		}
		if isString(typ, "function_call") {
			input := obj()
			// JSON.parse converts a non-string argument with String() first, and parses UTF-16
			// units, so an unpaired surrogate inside a string literal survives.
			if text, err := toString(or(get(item, "arguments"), str("{}"))); err == nil {
				if parsed, err := jsonx.DecodeWireUnits(text); err == nil {
					input = parsed
				}
			}
			content = append(content, obj(
				member("type", str("tool_use")),
				member("id", get(item, "call_id")),
				member("name", get(item, "name")),
				member("input", input),
			))
			toolUse = true
		}
	}
	return content, toolUse
}

func stopReason(toolUse bool) string {
	if toolUse {
		return "tool_use"
	}
	return "end_turn"
}

// TranslateResponse builds a message for the advertised model, using now, which must not be nil,
// for missing IDs. A response that could not be parsed is passed as JSON null and translates as
// the source translates null.
//
// The call fails where the source's translateResponse throws, which is when Number() of a usage
// field would throw (see CoercionError). It then returns the zero WireValue, whose Kind is 0 and
// which EncodeWire refuses, so a caller cannot send it by mistake. The result may share subtrees
// with response; treat both as read-only.
func TranslateResponse(response jsonx.WireValue, model string, now func() time.Time) jsonx.WireValue {
	u, err := usage(get(response, "usage"))
	if err != nil {
		return jsonx.WireValue{}
	}
	content, toolUse := responseContent(response, now)
	id := get(response, "id")
	if !truthy(id) {
		id = fallbackID("msg_grok_", now)
	}
	return obj(
		member("id", id),
		member("type", str("message")),
		member("role", str("assistant")),
		member("model", str(model)),
		member("content", array(content)),
		member("stop_reason", str(stopReason(toolUse))),
		member("stop_sequence", null()),
		member("usage", u),
	)
}
