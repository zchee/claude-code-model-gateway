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

package reply

import (
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"uuid"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
)

// ECMAScript \s: whitespace and line terminators, including no-break spaces and the BOM.
const jsSpace = `[\t\n\v\f\r\p{Zs}\x{2028}\x{2029}\x{FEFF}]`

// Patterns are matched against lowerASCII text, which reproduces the source's ASCII-only /i.
var (
	openAIWord        = regexp.MustCompile(`\bopenai\b`)
	authenticationMsg = regexp.MustCompile(`\b(?:not authenticated|unauthenticated|authentication (?:is )?required|authentication failed)\b`)
	obsoleteLogin     = regexp.MustCompile(`claude-code-proxy` + jsSpace + `+codex` + jsSpace + `+auth` + jsSpace + `+login\b`)
)

const (
	emptyCompletionDetail = "Codex completed without producing output"
	loginReplacement      = "the model-gateway login command above"
)

// noteCodexRejection records an attributed OpenAI rejection so readiness reports the block.
// The evidence names only headers, never their values. Header names are sorted because the
// parsed header map does not keep their arrival order.
func noteCodexRejection(store *upstreamstate.Store, status int, headers http.Header, body []byte) {
	if store == nil || !HasOpenAIRejectionEvidence(status, headers, body) {
		return
	}
	var names []string
	for name := range headers {
		name = strings.ToLower(name)
		if strings.HasPrefix(name, "x-openai-") || name == "openai-processing-ms" || name == "content-type" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	evidence := "body:openai"
	if len(names) > 0 {
		evidence = "headers:" + strings.Join(names, ",")
	}
	// A state write failure leaves readiness as it was; the client reply does not depend on it.
	_, _ = store.SetBlocked(status, evidence, headers)
}

// errorDetail returns error.message or message when it is a non-empty string. With plainText,
// a body without such a detail yields its trimmed text instead; otherwise ok is false.
func errorDetail(body []byte, plainText bool) (detail []uint16, ok bool) {
	if v, err := jsonx.DecodeWire(body); err == nil {
		d := member(member(v, "error"), "message")
		if !truthy(d) {
			d = member(v, "message")
		}
		if d.Kind == '"' && len(d.Text) > 0 {
			return d.Text, true
		}
	}
	if !plainText {
		return nil, false
	}
	return units(config.JSTrim(bufferText(body))), true
}

func upstreamErrorMessage(body []byte, status int) []uint16 {
	prefix := "model-gateway: upstream returned " + jsNumber(float64(status))
	if detail, ok := errorDetail(body, false); ok {
		return append(units(prefix+": "), detail...)
	}
	return units(prefix + " with no readable error body")
}

// codexAuthMissing is the readiness advice for a missing ChatGPT sign-in, naming the stable binary.
func codexAuthMissing(commandPath string) string {
	return "Codex dispatch refused: ChatGPT sign-in is required. Run `\"" + commandPath + "\" login`, finish browser OAuth, then run `\"" + commandPath + "\" setup` and retry. Credentials live in `~/.config/claude-code-proxy/`."
}

// codexAuthFailure maps a 401, or a 403 whose detail reports missing authentication, to the
// sign-in advice followed by the upstream detail with its obsolete login command replaced.
func codexAuthFailure(body []byte, status int, commandPath string) (message []uint16, ok bool) {
	detail, _ := errorDetail(body, true)
	text := wtf8(detail)
	if status != http.StatusUnauthorized && (status != http.StatusForbidden || !authenticationMsg.MatchString(lowerASCII(text))) {
		return nil, false
	}
	var preserved strings.Builder
	last := 0
	for _, loc := range obsoleteLogin.FindAllStringIndex(lowerASCII(text), -1) {
		preserved.WriteString(text[last:loc[0]])
		preserved.WriteString(loginReplacement)
		last = loc[1]
	}
	preserved.WriteString(text[last:])
	tail := fromWTF8(preserved.String())
	if len(tail) == 0 {
		tail = units("HTTP " + jsNumber(float64(status)))
	}
	return append(units(codexAuthMissing(commandPath)+" Upstream detail: "), tail...), true
}

// errorDocument is the Messages API error envelope.
func errorDocument(kind string, message []uint16) ([]byte, error) {
	return encode(object(field("type", wireString("error")), field("error", object(field("type", wireString(kind)), field("message", wireUnits(message))))))
}

func sseErrorFrame(kind string, message []uint16) ([]byte, error) {
	doc, err := errorDocument(kind, message)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("event: error\ndata: "), doc...), "\n\n"...), nil
}

func isUpgradeRejection(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	v, err := jsonx.DecodeWire(body)
	if err != nil {
		return false
	}
	e := member(v, "error")
	return isString(member(e, "type"), "permission_error") && isString(member(e, "message"), "WebSocket upgrade was rejected")
}

func upgradeExhausted(attempts int) ([]byte, error) {
	return errorDocument("api_error", units("model-gateway: Codex WebSocket upgrade was temporarily rejected after "+jsNumber(float64(attempts))+" attempts; retry the request."))
}

func isEmptyCompletion(status int, body []byte) bool {
	if status != http.StatusServiceUnavailable {
		return false
	}
	detail, ok := errorDetail(body, false)
	return ok && wtf8(detail) == emptyCompletionDetail
}

func requestedStream(request []byte) bool {
	v, err := jsonx.DecodeWire(request)
	return err == nil && member(v, "stream").Kind == 't'
}

// emptyEndTurn answers a Codex turn that completed without output with an empty end_turn
// message, as JSON or as the three stream events the request asked for.
func emptyEndTurn(model, id string, stream bool) (contentType string, body []byte, err error) {
	message := func(stop jsonx.WireValue) jsonx.WireValue {
		return object(
			field("id", wireString(id)), field("type", wireString("message")), field("role", wireString("assistant")),
			field("model", wireString(model)), field("content", jsonx.WireValue{Kind: '['}), field("stop_reason", stop),
			field("stop_sequence", wireNull), field("usage", object(field("input_tokens", wireNumber(0)), field("output_tokens", wireNumber(0)))),
		)
	}
	if !stream {
		body, err = encode(message(wireString("end_turn")))
		return "application/json", body, err
	}
	events := []jsonx.WireValue{
		object(field("type", wireString("message_start")), field("message", message(wireNull))),
		object(
			field("type", wireString("message_delta")),
			field("delta", object(field("stop_reason", wireString("end_turn")), field("stop_sequence", wireNull))),
			field("usage", object(field("output_tokens", wireNumber(0)))),
		),
		object(field("type", wireString("message_stop"))),
	}
	for _, event := range events {
		data, err := encode(event)
		if err != nil {
			return "", nil, err
		}
		body = append(body, "event: "+wtf8(member(event, "type").Text)+"\ndata: "...)
		body = append(append(body, data...), "\n\n"...)
	}
	return "text/event-stream", body, nil
}

func newMessageID(next func() string) string {
	if next != nil {
		return next()
	}
	return "msg_gateway_" + strings.ReplaceAll(uuid.NewV4().String(), "-", "")
}

// rewriteModels replaces every string "model" that is a Codex model ID or the backend model with
// the advertised model; any other "model" value is searched like the rest of the tree.
func rewriteModels(v *jsonx.WireValue, advertised, backend []uint16) {
	switch v.Kind {
	case '[':
		for i := range v.Elements {
			rewriteModels(&v.Elements[i], advertised, backend)
		}
	case '{':
		for i := range v.Members {
			m := &v.Members[i]
			if isName(m.Name, "model") && m.Value.Kind == '"' && (hasUnitsPrefix(m.Value.Text, "gpt-") || len(backend) > 0 && slices.Equal(m.Value.Text, backend)) {
				m.Value = wireUnits(advertised)
				continue
			}
			rewriteModels(&m.Value, advertised, backend)
		}
	}
}

func hasUnitsPrefix(u []uint16, ascii string) bool {
	return len(u) >= len(ascii) && isName(u[:len(ascii)], ascii)
}

func isPlanTool(block jsonx.WireValue) bool {
	return block.Kind == '{' && isString(member(block, "type"), "tool_use") &&
		(isString(member(block, "name"), "EnterPlanMode") || isString(member(block, "name"), "ExitPlanMode"))
}

// rewriteCodexJSON advertises the picker model in a JSON reply and, when asked, removes plan-mode
// tool calls, ending the turn if no tool call remains. Bytes that are not JSON stay as they are.
func rewriteCodexJSON(body []byte, advertised string, filterPlanTools bool, backend string) []byte {
	v, err := jsonx.DecodeWire(body)
	if err != nil {
		return body
	}
	rewriteModels(&v, units(advertised), units(backend))
	if content := lookup(&v, "content"); filterPlanTools && content != nil && content.Kind == '[' {
		kept := slices.DeleteFunc(slices.Clone(content.Elements), isPlanTool)
		if len(kept) != len(content.Elements) {
			content.Elements = kept
			if stop := lookup(&v, "stop_reason"); stop != nil && isString(*stop, "tool_use") &&
				!slices.ContainsFunc(kept, func(b jsonx.WireValue) bool { return b.Kind == '{' && isString(member(b, "type"), "tool_use") }) {
				*stop = wireString("end_turn")
			}
		}
	}
	out, err := encode(v)
	if err != nil {
		return body
	}
	return out
}

// exclusiveLMStudioJSON removes the cached part from LM Studio's inclusive input_tokens and
// re-serializes the reply. An input below its cache read is not an inclusive count and stays.
func exclusiveLMStudioJSON(body []byte) []byte {
	v, err := jsonx.DecodeWire(body)
	if err != nil {
		return body
	}
	usage := lookup(&v, "usage")
	if isString(member(v, "type"), "message_start") {
		usage = nil
		if message := lookup(&v, "message"); message != nil {
			usage = lookup(message, "usage")
		}
	}
	if usage != nil {
		input, cached := lookup(usage, "input_tokens"), lookup(usage, "cache_read_input_tokens")
		if finite(input) && finite(cached) && cached.Number > 0 && input.Number >= cached.Number {
			*input = wireNumber(input.Number - cached.Number)
		}
	}
	out, err := encode(v)
	if err != nil {
		return body
	}
	return out
}

func finite(v *jsonx.WireValue) bool {
	return v != nil && v.Kind == '0' && !math.IsInf(v.Number, 0) && !math.IsNaN(v.Number)
}

// completedJSON reports a Messages API message without an error.
func completedJSON(body []byte) bool {
	v, err := jsonx.DecodeWire(body)
	return err == nil && isString(member(v, "type"), "message") && !truthy(member(v, "error"))
}
