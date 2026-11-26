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
	"strconv"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// The recovery advice for each request phase. A request without a prior assistant turn has nothing
// to compact, so its recovery happens before the gateway.
var overflowRecovery = map[string]string{
	"first_turn":   "nothing to compact, the request has no prior turn: shrink the first prompt (a dispatched subagent's briefing) or route to a larger-window model",
	"continuation": "compact the conversation and retry",
	"compaction":   "the compaction request itself is over the limit: drop older turns or start a fresh conversation, or route to a larger-window model",
	"UNVERIFIED":   "phase UNVERIFIED: compact if the conversation has prior turns, otherwise shrink the prompt or route to a larger-window model",
}

// overflowDetails classifies a refusal of request. A zero token count is unknown: it projects to
// null tokens and a null source.
func overflowDetails(request []byte, refusedBy string, tokens float64, tokensFrom string, limit float64) *outcome.Overflow {
	o := &outcome.Overflow{Phase: sentry.RequestPhase(request), RefusedBy: refusedBy, RequestBytes: int64(len(request)), LimitTokens: &limit}
	if tokens != 0 && !math.IsNaN(tokens) {
		o.Tokens, o.TokensFrom = &tokens, tokensFrom
	}
	return o
}

func overflowWire(o *outcome.Overflow) jsonx.WireValue {
	tokens, from := wireNull, wireNull
	if o.Tokens != nil {
		tokens, from = wireNumber(*o.Tokens), wireString(o.TokensFrom)
	}
	return object(
		field("phase", wireString(o.Phase)), field("refused_by", wireString(o.RefusedBy)),
		field("request_bytes", wireNumber(float64(o.RequestBytes))), field("tokens", tokens),
		field("tokens_from", from), field("limit_tokens", wireNumber(*o.LimitTokens)),
	)
}

// describeOverflow is appended to the refusal message. The "(N tokens > M tokens)" part before it
// stays first because Claude Code parses it to size its compaction.
func describeOverflow(o *outcome.Overflow) string {
	measured := "tokens UNVERIFIED"
	if o.Tokens != nil {
		measured = jsNumber(*o.Tokens) + " tokens from " + o.TokensFrom
	}
	return " [model-gateway: phase " + o.Phase + ", refused by " + o.RefusedBy + "; request " + strconv.FormatInt(o.RequestBytes, 10) +
		" bytes, " + measured + ", limit " + jsNumber(*o.LimitTokens) + " tokens; recovery: " + overflowRecovery[o.Phase] + "]"
}

func comparison(tokens, limit float64) string {
	return " (" + jsNumber(tokens) + " tokens > " + jsNumber(limit) + " tokens)"
}

func overflowBody(message string, o *outcome.Overflow) ([]byte, error) {
	return encode(object(field("type", wireString("error")), field("error", object(
		field("type", wireString("request_too_large")),
		field("message", wireString(message+describeOverflow(o))),
		field("overflow", overflowWire(o)),
	))))
}

// writeOverflow answers 413 with only the headers the source sends; a refusal that came from the
// upstream also names the upstream's status.
func writeOverflow(w http.ResponseWriter, body []byte, upstreamStatus *int) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if upstreamStatus != nil {
		h.Set("X-Model-Gateway-Upstream-Status", strconv.Itoa(*upstreamStatus))
	}
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	// A client that has gone away cannot be answered; the facts stand either way.
	_, _ = w.Write(body)
}

// normalizeGenuine appends the gateway's diagnostics to a proxy's own request_too_large reply.
// The upstream wording and any "N tokens > M tokens" counts stay as sent; without counts the
// previous turn's usage, or the window plus one, stands in. Any other body is returned unchanged
// with a nil overflow and latches nothing.
func normalizeGenuine(body []byte, req Request) ([]byte, *outcome.Overflow) {
	counts, ok := sentry.ParseOverflow(http.StatusRequestEntityTooLarge, body)
	if !ok {
		return body, nil
	}
	var usage float64
	if req.Sentry != nil {
		usage = req.Sentry.Overflow(req.Session, req.Target, true)
	}
	v, err := jsonx.DecodeWire(body)
	e := lookup(&v, "error")
	message := lookup(e, "message")
	if err != nil || message == nil {
		return body, nil // Unreachable: ParseOverflow accepted this error message.
	}
	var o *outcome.Overflow
	if counts.Tokens != nil {
		o = overflowDetails(req.Body, "upstream", *counts.Tokens, "upstream", *counts.Limit)
	} else {
		o = overflowDetails(req.Body, "upstream", usage, "previous_turn_usage", float64(req.Window))
		stand := usage
		if stand == 0 {
			stand = *o.LimitTokens + 1
		}
		message.Text = append(message.Text, units(comparison(stand, *o.LimitTokens))...)
	}
	message.Text = append(message.Text, units(describeOverflow(o))...)
	assign(e, "overflow", overflowWire(o))
	out, err := encode(v)
	if err != nil {
		return body, nil
	}
	return out, o
}

// engineOverflow latches the refusal and builds the normalized body for an engine or legacy context
// error; the caller writes it. An engine's own limit suppresses ceiling learning; its own token
// count replaces the previous turn's usage.
func engineOverflow(req Request, status int, counts sentry.Counts) (*outcome.Overflow, []byte, error) {
	var usage float64
	if req.SentryEnabled && req.Sentry != nil {
		usage = req.Sentry.Overflow(req.Session, req.Target, counts.Limit == nil || *counts.Limit == 0)
	}
	limit := float64(req.Window)
	if counts.Limit != nil {
		limit = *counts.Limit
	}
	engineTokens := counts.Tokens != nil && *counts.Tokens != 0 && !math.IsNaN(*counts.Tokens)
	tokens, from := usage, "previous_turn_usage"
	if engineTokens {
		tokens, from = *counts.Tokens, "upstream"
	}
	var numbers string
	if engineTokens || req.SentryEnabled {
		stand := tokens
		if stand == 0 || math.IsNaN(stand) {
			stand = limit + 1
		}
		numbers = comparison(stand, limit)
	}
	o := overflowDetails(req.Body, "upstream", tokens, from, limit)
	body, err := overflowBody("Input exceeds the model context window; compact and retry."+numbers, o)
	if err != nil {
		return nil, nil, err
	}
	return o, body, nil
}
