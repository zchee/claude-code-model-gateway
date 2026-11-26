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

package proxyreq

import (
	"slices"
	"strconv"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// The rewrites below reproduce the JavaScript object operations of the original worker on a decoded tree:
// a property keeps its position when its value is replaced, a new property is appended after the existing
// ones, and every comparison of a name or a string is made on UTF-16 code units.

// threadRefused reports a thread member that is not a plain object with type "create".
func threadRefused(payload jsonx.WireValue) bool {
	i := memberIndex(payload, "thread")
	if i < 0 {
		return false
	}
	thread := payload.Members[i].Value
	return thread.Kind != '{' || !isText(memberValue(thread, "type"), "create")
}

// effortOf returns output_config.effort when it is a string.
func effortOf(payload jsonx.WireValue) (string, bool) {
	effort := memberValue(memberValue(payload, "output_config"), "effort")
	if effort.Kind != '"' {
		return "", false
	}
	return string(utf16.Decode(effort.Text)), true
}

// effectiveEffort prefers the prepared output_config.effort and falls back to the client's request.
func effectiveEffort(payload jsonx.WireValue, requested string) string {
	if effort, ok := effortOf(payload); ok {
		return effort
	}
	return requested
}

// overrideEffort assigns output_config = {...output_config, effort}. Spreading copies an object's members,
// a string's code units and an array's elements under their indices, and nothing from any other value.
func overrideEffort(payload *jsonx.WireValue, effort string) {
	spread := jsonx.WireValue{Kind: '{'}
	i := memberIndex(*payload, "output_config")
	if i >= 0 {
		switch v := payload.Members[i].Value; v.Kind {
		case '{':
			spread.Members = slices.Clone(v.Members)
		case '"':
			for n, u := range v.Text {
				spread.Members = append(spread.Members, jsonx.Member{Name: units(strconv.Itoa(n)), Value: jsonx.WireValue{Kind: '"', Text: []uint16{u}}})
			}
		case '[':
			for n, e := range v.Elements {
				spread.Members = append(spread.Members, jsonx.Member{Name: units(strconv.Itoa(n)), Value: e})
			}
		}
	}
	setMember(&spread, "effort", wireText(effort))
	if i >= 0 {
		payload.Members[i].Value = spread
		return
	}
	payload.Members = append(payload.Members, jsonx.Member{Name: units("output_config"), Value: spread})
}

// definitions keeps hoisted tool definitions by name in first-insertion order with the last value.
type definitions struct {
	order []string
	byKey map[string]jsonx.WireValue
}

func (d *definitions) set(name []uint16, definition jsonx.WireValue) {
	k := unitKey(name)
	if _, ok := d.byKey[k]; !ok {
		d.order = append(d.order, k)
	}
	d.byKey[k] = definition
}

// resolveDeferredTools moves tool_addition blocks into the tools list, renders tool references as text, and
// loads deferred tools that the conversation references; no proxy backend accepts those blocks or markers.
func resolveDeferredTools(payload *jsonx.WireValue) {
	referenced := make(map[string]bool)
	added := definitions{byKey: make(map[string]jsonx.WireValue)}
	addedRefs := make(map[string]bool)
	if mi := memberIndex(*payload, "messages"); mi >= 0 && payload.Members[mi].Value.Kind == '[' {
		messages := payload.Members[mi].Value.Elements
		kept := make([]jsonx.WireValue, 0, len(messages))
		for _, message := range messages {
			ci := memberIndex(message, "content")
			if ci < 0 || message.Members[ci].Value.Kind != '[' {
				kept = append(kept, message)
				continue
			}
			content := message.Members[ci].Value.Elements
			stripped := stripToolAdditions(content, &added, addedRefs)
			if len(stripped) == 0 && len(content) > 0 {
				continue
			}
			message.Members[ci].Value = jsonx.WireValue{Kind: '[', Elements: renderToolReferences(stripped, referenced)}
			kept = append(kept, message)
		}
		payload.Members[mi].Value = jsonx.WireValue{Kind: '[', Elements: kept}
	}

	ti := memberIndex(*payload, "tools")
	isArray := ti >= 0 && payload.Members[ti].Value.Kind == '['
	if !isArray && len(added.order) == 0 {
		return
	}
	var tools []jsonx.WireValue
	if isArray {
		tools = payload.Members[ti].Value.Elements
	}
	// A referenced tool without input_schema is an Anthropic server tool, which no proxy backend can run.
	loads := func(tool jsonx.WireValue) bool {
		name := memberValue(tool, "name")
		if name.Kind != '"' {
			return false
		}
		k := unitKey(name.Text)
		_, defined := added.byKey[k]
		return referenced[k] || defined || addedRefs[k] && memberIndex(tool, "input_schema") >= 0
	}
	loaded := make([]jsonx.WireValue, 0, len(tools)+len(added.order))
	present := make(map[string]bool)
	for _, tool := range tools {
		if tool.Kind == '{' {
			di := memberIndex(tool, "defer_loading")
			if di >= 0 && tool.Members[di].Value.Kind == 't' && !loads(tool) {
				continue
			}
			if di >= 0 {
				tool = withoutMember(tool, di)
			}
			if name := memberValue(tool, "name"); name.Kind == '"' {
				present[unitKey(name.Text)] = true
			}
		}
		loaded = append(loaded, tool)
	}
	for _, k := range added.order {
		if present[k] {
			continue
		}
		definition := added.byKey[k]
		if di := memberIndex(definition, "defer_loading"); di >= 0 {
			definition = withoutMember(definition, di)
		}
		loaded = append(loaded, definition)
	}
	setMember(payload, "tools", jsonx.WireValue{Kind: '[', Elements: loaded})
}

// stripToolAdditions removes every tool_addition block, collecting definitions with a string name and
// references by name.
func stripToolAdditions(content []jsonx.WireValue, added *definitions, addedRefs map[string]bool) []jsonx.WireValue {
	kept := make([]jsonx.WireValue, 0, len(content))
	for _, block := range content {
		if block.Kind != '{' || !isText(memberValue(block, "type"), "tool_addition") {
			kept = append(kept, block)
			continue
		}
		tool := memberValue(block, "tool")
		switch kind := memberValue(tool, "type"); {
		case isText(kind, "tool_definition"):
			definition := memberValue(tool, "definition")
			if name := memberValue(definition, "name"); name.Kind == '"' {
				added.set(name.Text, definition)
			}
		case isText(kind, "tool_reference"):
			if name := memberValue(tool, "name"); name.Kind == '"' {
				addedRefs[unitKey(name.Text)] = true
			}
		}
	}
	return kept
}

// renderToolReferences replaces tool_reference blocks with text, recursing into tool_result content.
func renderToolReferences(content []jsonx.WireValue, referenced map[string]bool) []jsonx.WireValue {
	out := make([]jsonx.WireValue, len(content))
	for i, block := range content {
		out[i] = block
		if block.Kind != '{' {
			continue
		}
		kind := memberValue(block, "type")
		if isText(kind, "tool_reference") {
			if name := memberValue(block, "tool_name"); name.Kind == '"' {
				referenced[unitKey(name.Text)] = true
				text := append(units("Tool reference: "), name.Text...)
				out[i] = object(
					jsonx.Member{Name: units("type"), Value: wireText("text")},
					jsonx.Member{Name: units("text"), Value: jsonx.WireValue{Kind: '"', Text: text}},
				)
				continue
			}
		}
		if !isText(kind, "tool_result") {
			continue
		}
		if ci := memberIndex(block, "content"); ci >= 0 && block.Members[ci].Value.Kind == '[' {
			block.Members = slices.Clone(block.Members)
			block.Members[ci].Value = jsonx.WireValue{Kind: '[', Elements: renderToolReferences(block.Members[ci].Value.Elements, referenced)}
			out[i] = block
		}
	}
	return out
}

// removePlanTools hides the plan-mode tools, which non-Claude models call spuriously.
func removePlanTools(payload *jsonx.WireValue) {
	ti := memberIndex(*payload, "tools")
	if ti < 0 || payload.Members[ti].Value.Kind != '[' {
		return
	}
	tools := slices.DeleteFunc(slices.Clone(payload.Members[ti].Value.Elements), func(tool jsonx.WireValue) bool {
		name := memberValue(tool, "name")
		return isText(name, "EnterPlanMode") || isText(name, "ExitPlanMode")
	})
	payload.Members[ti].Value = jsonx.WireValue{Kind: '[', Elements: tools}
}

// memberIndex returns the position of an object's member with an ASCII name, or -1 for a missing member
// or a value that is not an object.
func memberIndex(v jsonx.WireValue, name string) int {
	if v.Kind != '{' {
		return -1
	}
	want := units(name)
	for i, m := range v.Members {
		if slices.Equal(m.Name, want) {
			return i
		}
	}
	return -1
}

// memberValue returns an object's member, or the zero value, which matches no JSON kind.
func memberValue(v jsonx.WireValue, name string) jsonx.WireValue {
	if i := memberIndex(v, name); i >= 0 {
		return v.Members[i].Value
	}
	return jsonx.WireValue{}
}

// setMember replaces a member's value in place or appends the member.
func setMember(v *jsonx.WireValue, name string, value jsonx.WireValue) {
	if i := memberIndex(*v, name); i >= 0 {
		v.Members[i].Value = value
		return
	}
	v.Members = append(v.Members, jsonx.Member{Name: units(name), Value: value})
}

// withoutMember returns a shallow copy of an object without one member.
func withoutMember(v jsonx.WireValue, i int) jsonx.WireValue {
	v.Members = slices.Delete(slices.Clone(v.Members), i, i+1)
	return v
}

func object(members ...jsonx.Member) jsonx.WireValue {
	return jsonx.WireValue{Kind: '{', Members: members}
}

func isText(v jsonx.WireValue, s string) bool {
	return v.Kind == '"' && slices.Equal(v.Text, units(s))
}

func wireText(s string) jsonx.WireValue {
	return jsonx.WireValue{Kind: '"', Text: units(s)}
}

func units(s string) []uint16 {
	return utf16.Encode([]rune(s))
}

// unitKey maps UTF-16 units to a string key without merging unpaired surrogates.
func unitKey(text []uint16) string {
	b := make([]byte, 0, 2*len(text))
	for _, u := range text {
		b = append(b, byte(u>>8), byte(u))
	}
	return string(b)
}
