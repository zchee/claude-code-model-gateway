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

package sse

import (
	"bytes"
	"slices"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	sseframe "github.com/zchee/claude-code-model-gateway/internal/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// fatalErrorTypes are the upstream error types a retry would only resend to the same refusal.
var fatalErrorTypes = []string{
	"invalid_request_error",
	"authentication_error",
	"permission_error",
	"not_found_error",
	"request_too_large",
	"rate_limit_error",
	"billing_error",
}

// planTools are the tool names filtered out of proxy responses when plan-tool filtering is on.
var planTools = []string{"EnterPlanMode", "ExitPlanMode"}

// textIn returns the element of set that the wire string v spells, if any.
func textIn(v jsonx.WireValue, set []string) (string, bool) {
	if v.Kind != '"' {
		return "", false
	}
	for _, s := range set {
		if sameASCII(v.Text, s) {
			return s, true
		}
	}
	return "", false
}

// NoteEvent updates attempt structure, error state, and merged usage from one decoded SSE event.
// Non-object or absent events have no effect.
func (a *Attempt) NoteEvent(event jsonx.WireValue) {
	if event.Kind != '{' {
		return
	}
	typ := member(event, "type")
	switch {
	case isText(typ, "message_stop"):
		a.Terminal = true
		a.TerminalCode = "message_stop"
	case isText(typ, "message_delta"):
		if stop := member(member(event, "delta"), "stop_reason"); isText(stop, "end_turn") || isText(stop, "stop_sequence") {
			a.EndOfGenerationStop = true
		}
	case isText(typ, "content_block_start"), isText(typ, "content_block_delta"), isText(typ, "content_block_stop"):
		if a.EndOfGenerationStop {
			a.ContentAfterStop = true
		}
		if isText(typ, "content_block_start") {
			a.OpenBlocks++
		} else if isText(typ, "content_block_stop") {
			a.OpenBlocks--
		}
	}
	if isText(typ, "content_block_delta") {
		delta := member(event, "delta")
		if text := member(delta, "text"); isText(member(delta, "type"), "text_delta") && text.Kind == '"' && !jsBlank(text.Text) {
			a.HasVisibleText = true
		}
	}
	if isText(typ, "error") {
		a.SawError = true
		e := member(event, "error")
		a.ErrorCode = errorCode(e)
		if _, fatal := textIn(member(e, "type"), fatalErrorTypes); fatal {
			a.Fatal = true
		}
	}
	a.MergedUsage = sentry.MergeWireUsage(a.MergedUsage, event)
}

// errorCode classifies an upstream error object into a bounded diagnostic label.
func errorCode(e jsonx.WireValue) string {
	if e.Kind != '{' && e.Kind != '[' {
		return "unknown_error"
	}
	typ := member(e, "type")
	if isText(typ, "api_error") && isText(member(e, "message"), "websocket_missing_terminal") {
		return "websocket_missing_terminal"
	}
	if code, ok := textIn(typ, fatalErrorTypes); ok {
		return code
	}
	return "unknown_error"
}

// Finished reports whether the stream reached message_stop or a clean end-of-generation stop with no open blocks.
func (a *Attempt) Finished() bool {
	return a.Terminal || a.EndOfGenerationStop && !a.ContentAfterStop && a.OpenBlocks == 0
}

// Deliverable reports whether the attempt finished cleanly without seeing an upstream error event.
func (a *Attempt) Deliverable() bool {
	return a.Finished() && !a.SawError
}

// NewTransformer creates a proxy SSE frame transformer.
func NewTransformer(cfg TransformerConfig) *Transformer {
	return &Transformer{cfg: cfg, advertised: units(cfg.AdvertisedModel), backend: units(cfg.BackendModel)}
}

// Feed processes one upstream chunk and passes each emitted wire frame to emit.
func (t *Transformer) Feed(chunk []byte, emit func([]byte) error) error {
	for _, f := range t.scanner.Feed(chunk) {
		if err := t.frame(f, emit); err != nil {
			return err
		}
	}
	return nil
}

// End flushes any remaining unterminated frame and passes it to emit.
func (t *Transformer) End(emit func([]byte) error) error {
	for _, f := range t.scanner.End() {
		if err := t.frame(f, emit); err != nil {
			return err
		}
	}
	return nil
}

// frame forwards one frame untouched, drops it as a filtered plan-tool event, or re-encodes
// its first data line after observation and rewriting.
func (t *Transformer) frame(f sseframe.Frame, emit func([]byte) error) error {
	data, ok := f.Data()
	raw := jsTrimStart(data)
	if !ok || raw == "" || raw == "[DONE]" {
		return emit([]byte(f.Body + f.Delimiter))
	}
	event, err := jsonx.DecodeWire([]byte(raw))
	if err != nil {
		return emit([]byte(f.Body + f.Delimiter))
	}
	if t.cfg.Provider == LMStudioProvider {
		event = exclusiveLMStudioUsage(event)
	}
	if t.cfg.Observe != nil {
		t.cfg.Observe(event)
	}
	if t.cfg.AdvertisedModel != "" {
		event, _ = t.rewriteModel(event)
	}
	if t.cfg.FilterPlanTools {
		var drop bool
		if event, drop = t.filterPlanTools(event); drop {
			return nil
		}
	}
	payload, err := jsonx.EncodeWire(event)
	if err != nil {
		return emit([]byte(f.Body + f.Delimiter))
	}
	rewritten := sseframe.Frame{Body: nodeUTF8String(f.Body), Delimiter: f.Delimiter}
	return emit([]byte(rewritten.WithData(string(payload))))
}

// rewriteModel replaces every "model" string member that names a gpt- model or the exact
// backend model with the advertised model, at any depth. Changed containers are copied.
func (t *Transformer) rewriteModel(v jsonx.WireValue) (jsonx.WireValue, bool) {
	switch v.Kind {
	case '{':
		var members []jsonx.Member
		for i, m := range v.Members {
			var value jsonx.WireValue
			changed := false
			if sameASCII(m.Name, "model") && m.Value.Kind == '"' {
				if t.rewritable(m.Value.Text) {
					value, changed = jsonx.WireValue{Kind: '"', Text: t.advertised}, true
				}
			} else {
				value, changed = t.rewriteModel(m.Value)
			}
			if changed {
				if members == nil {
					members = slices.Clone(v.Members)
				}
				members[i].Value = value
			}
		}
		if members != nil {
			v.Members = members
			return v, true
		}
	case '[':
		var elements []jsonx.WireValue
		for i, e := range v.Elements {
			if value, changed := t.rewriteModel(e); changed {
				if elements == nil {
					elements = slices.Clone(v.Elements)
				}
				elements[i] = value
			}
		}
		if elements != nil {
			v.Elements = elements
			return v, true
		}
	}
	return v, false
}

// rewritable reports whether a response model ID is one the client must see as the advertised model.
func (t *Transformer) rewritable(text []uint16) bool {
	const prefix = "gpt-"
	if len(text) >= len(prefix) && sameASCII(text[:len(prefix)], prefix) {
		return true
	}
	return len(t.backend) > 0 && slices.Equal(text, t.backend)
}

// filterPlanTools drops plan-tool content blocks, shifts later block indexes down, and turns
// a tool_use stop into end_turn when the only tool calls were dropped.
func (t *Transformer) filterPlanTools(event jsonx.WireValue) (jsonx.WireValue, bool) {
	indexAt := memberIndex(event, "index")
	hasIndex := indexAt >= 0 && integerNumber(event.Members[indexAt].Value)
	var index float64
	if hasIndex {
		index = event.Members[indexAt].Value.Number
	}
	typ := member(event, "type")
	if isText(typ, "content_block_start") && hasIndex {
		block := member(event, "content_block")
		if isPlanToolBlock(block) {
			if t.dropped == nil {
				t.dropped = make(map[float64]struct{})
			}
			t.dropped[index] = struct{}{}
			t.droppedCount++
			return event, true
		}
		if isText(member(block, "type"), "tool_use") {
			t.keptToolUse = true
		}
	}
	if hasIndex {
		if _, dropped := t.dropped[index]; dropped {
			return event, true
		}
		if t.droppedCount > 0 {
			event = withMember(event, indexAt, jsonx.WireValue{Kind: '0', Number: index - float64(t.droppedCount)})
		}
	}
	if isText(typ, "message_delta") && t.droppedCount > 0 && !t.keptToolUse {
		if deltaAt := memberIndex(event, "delta"); deltaAt >= 0 {
			delta := event.Members[deltaAt].Value
			if stopAt := memberIndex(delta, "stop_reason"); stopAt >= 0 && isText(delta.Members[stopAt].Value, "tool_use") {
				delta = withMember(delta, stopAt, jsonx.WireValue{Kind: '"', Text: units("end_turn")})
				event = withMember(event, deltaAt, delta)
			}
		}
	}
	return event, false
}

// isPlanToolBlock reports whether a content block is a tool call to a plan tool.
func isPlanToolBlock(block jsonx.WireValue) bool {
	if !isText(member(block, "type"), "tool_use") {
		return false
	}
	_, ok := textIn(member(block, "name"), planTools)
	return ok
}

// exclusiveLMStudioUsage takes the cached part out of LM Studio's inclusive input_tokens,
// leaving an input below its cache read untouched. Changed containers are copied.
func exclusiveLMStudioUsage(event jsonx.WireValue) jsonx.WireValue {
	if event.Kind != '{' {
		return event
	}
	holder, holderAt := event, -1
	if isText(member(event, "type"), "message_start") {
		holderAt = memberIndex(event, "message")
		if holderAt < 0 {
			return event
		}
		holder = event.Members[holderAt].Value
	}
	usageAt := memberIndex(holder, "usage")
	if usageAt < 0 {
		return event
	}
	usage := holder.Members[usageAt].Value
	inputAt := memberIndex(usage, "input_tokens")
	if inputAt < 0 {
		return event
	}
	input, cached := usage.Members[inputAt].Value, member(usage, "cache_read_input_tokens")
	if !finiteNumber(input) || !finiteNumber(cached) || cached.Number <= 0 || input.Number < cached.Number {
		return event
	}
	usage = withMember(usage, inputAt, jsonx.WireValue{Kind: '0', Number: input.Number - cached.Number})
	holder = withMember(holder, usageAt, usage)
	if holderAt < 0 {
		return holder
	}
	return withMember(event, holderAt, holder)
}

// NewEventObserver creates a side-branch SSE observer with the given pending byte limit.
// A non-positive maxPendingBytes defaults to DefaultMaxObserveBytes.
func NewEventObserver(maxPendingBytes int, observe func(jsonx.WireValue), onOverflow func()) *EventObserver {
	if maxPendingBytes <= 0 {
		maxPendingBytes = DefaultMaxObserveBytes
	}
	return &EventObserver{maxPendingBytes: maxPendingBytes, observe: observe, onOverflow: onOverflow}
}

// Write feeds raw or decompressed stream bytes into the observer.
func (o *EventObserver) Write(chunk []byte) {
	if o.overflowed {
		return
	}
	data := chunk
	if len(o.carry) > 0 {
		data = append(o.carry, chunk...)
	}
	keep := incompleteTail(data)
	o.pending = appendNodeUTF8(o.pending, data[:len(data)-keep])
	o.carry = bytes.Clone(data[len(data)-keep:])
	if o.overflowIfNeeded() {
		return
	}
	rest, from := o.pending, o.scanFrom
	for {
		start, end, ok := findDelimiter(rest, from)
		if !ok {
			break
		}
		o.inspect(rest[:start])
		rest, from = rest[end:], 0
	}
	o.pending = append(o.pending[:0], rest...)
	o.scanFrom = max(len(o.pending)-3, 0)
}

// End flushes any remaining unterminated frame if the observer has not overflowed.
func (o *EventObserver) End() {
	if o.overflowed {
		return
	}
	if len(o.carry) > 0 {
		o.pending = appendNodeUTF8(o.pending, o.carry)
		o.carry = nil
	}
	if o.overflowIfNeeded() {
		return
	}
	if len(o.pending) > 0 {
		o.inspect(o.pending)
	}
	o.pending, o.scanFrom = nil, 0
}

// overflowIfNeeded drops the pending text and stops observation once it exceeds the budget.
func (o *EventObserver) overflowIfNeeded() bool {
	if len(o.pending) <= o.maxPendingBytes {
		return false
	}
	o.pending, o.carry, o.scanFrom = nil, nil, 0
	o.overflowed = true
	if o.onOverflow != nil {
		o.onOverflow()
	}
	return true
}

// inspect observes the first data line of one frame when it holds JSON; anything else is ignored.
func (o *EventObserver) inspect(frame []byte) {
	data, ok := sseframe.Frame{Body: string(frame)}.Data()
	raw := jsTrimStart(data)
	if !ok || raw == "" || raw == "[DONE]" {
		return
	}
	event, err := jsonx.DecodeWire([]byte(raw))
	if err != nil || o.observe == nil {
		return
	}
	o.observe(event)
}
