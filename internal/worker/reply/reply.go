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

// Package reply handles buffered inference replies before the streaming relay takes ownership.
package reply

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// UsageCapture observes buffered responses and the headers of streams passed to the relay.
type UsageCapture interface {
	SetResponse(int, http.Header)
	NoteResponseBytes(int64)
	ObserveJSON([]byte)
	SetResponseComplete(bool)
}

// Request describes one forwarded inference turn. Body contains the serialized upstream request.
// SentryEnabled controls refusal normalization and learning separately from usage observation.
// Nil or negative retry settings select two retries separated by 250 milliseconds; explicit zero
// is respected.
// NewMessageID supplies the complete ID for a synthesized message.
// Diagnostics receives fixed operator lines that carry no request or response content; nil
// discards them. A Codex rejection line is written whether or not the client is still there; the
// empty-completion line only after the empty reply was written to the client and its body Write
// returned no error. Each line is written with exactly one Write call, and the writer must be safe
// for concurrent use by every user of the same underlying sink.
type Request struct {
	Provider, AdvertisedModel, BackendModel, Session, CommandPath, Origin string
	Body                                                                  []byte
	FilterPlanTools, Compaction, HeadWritten, SentryEnabled               bool
	Window                                                                int64
	Target                                                                sentry.Target
	Sentry                                                                *sentry.State
	Upstream                                                              *upstreamstate.Store
	Retries                                                               *int
	RetryDelay                                                            *time.Duration
	NewMessageID                                                          func() string
	Usage                                                                 UsageCapture
	Diagnostics                                                           io.Writer
}

// Result carries terminal facts, or an unsettled response whose open body belongs to the caller.
type Result struct {
	outcome.Result
	Stream *upstream.Response
}

// Handle calls forward for each attempt, closes buffered bodies and writes one client response.
// The ctx argument is the client context: no attempt starts once ctx is done (neither the first
// attempt nor any retry, including with a zero retry delay), and an attempt in flight is never
// cancelled by it because Handle passes context.WithoutCancel(ctx) to forward.
//
// What the upstream sent is recorded whether or not the client is still there: the Codex
// rejection state and its diagnostic line, the overflow latch and ceiling learning, and the usage
// and sentry observations of a complete JSON body. The sentry observes such a body with one
// call, including the rearming of a fired refusal that an ordinary completed turn brings, whatever
// the client's state and whether or not the reply is then written.
//
// Each output operation toward the client has its own check of the client context, made after
// every such callback and after any rewriting: after the header map was prepared and immediately
// before WriteHeader, and before each later Write. A client that left before a head was committed
// gets nothing written to w; for a buffered body Handle reads and closes it and returns a nil
// error, Stream nil, Contacted as it happened, Status zero, and Cancelled false. When the caller
// already wrote the head, the one in-band error frame is the only output and the same applies to
// a client that left before it. A head that Handle committed is reported with its status and the
// turn's facts, also when the client left before the body or the body write failed.
//
// A response that belongs to the stream relay is returned untouched and open whatever the client
// context says; the relay, which learns of the departure itself, owns its body. Otherwise Handle
// returns an error wrapping http.ErrAbortHandler when the client connection must abort.
func Handle(ctx context.Context, w http.ResponseWriter, req Request, forward upstream.Send) (Result, error) {
	retries, delay := defaultRetries, defaultRetryDelay
	// A negative setting falls back to the default, as the source's configuration does.
	if req.Retries != nil && *req.Retries >= 0 {
		retries = *req.Retries
	}
	// An explicit zero delay retries immediately; it is not Node's minimum timer tick.
	if req.RetryDelay != nil && *req.RetryDelay >= 0 {
		delay = *req.RetryDelay
	}
	if req.Usage == nil {
		req.Usage = noUsage{}
	}
	var facts outcome.Result
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return Result{Result: facts}, nil
		}
		res, err := forward(context.WithoutCancel(ctx))
		facts.Contacted = true
		if err != nil {
			if res != nil && res.Body != nil {
				_ = res.Body.Close()
			}
			return transportFailure(ctx, w, req, err, facts)
		}
		t := &turn{ctx: ctx, w: w, req: req, res: res, result: facts}
		out, retry, err := t.respond(attempt, retries)
		if errors.Is(err, errDeparted) {
			return Result{Result: facts}, nil
		}
		if !retry {
			return out, err
		}
		if delay > 0 && !wait(ctx, delay) {
			return Result{Result: facts}, nil
		}
	}
}

const (
	defaultRetries    = 2
	defaultRetryDelay = 250 * time.Millisecond
)

// wait sleeps for d unless the client context ends first, and reports whether the sleep completed.
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// WriteSentry reports a local refusal using the original request bytes and the provider's display label.
func WriteSentry(w http.ResponseWriter, original []byte, provider string, cross sentry.Crossing) (outcome.Result, error) {
	o := overflowDetails(original, "gateway_sentry", cross.Usage, "previous_turn_usage", cross.Trigger)
	body, err := overflowBody("Prompt is too long for the "+proxymodels.ProviderLabel(provider)+" context window; compact and retry."+comparison(cross.Usage, cross.Trigger), o)
	if err != nil {
		return outcome.Result{}, err
	}
	writeOverflow(w, body, nil)
	return outcome.Result{Route: outcome.Route{Overflow: o}, Status: http.StatusRequestEntityTooLarge}, nil
}

// WriteEngineOverflow normalizes an upstream refusal using the prepared request and source HTTP status.
// It always returns Contacted true because the refusal came from the upstream.
func WriteEngineOverflow(w http.ResponseWriter, req Request, status int, counts sentry.Counts) (outcome.Result, error) {
	o, body, err := engineOverflow(req, status, counts)
	if err != nil {
		return outcome.Result{Contacted: true}, err
	}
	writeOverflow(w, body, &status)
	return outcome.Result{Route: outcome.Route{Overflow: o}, Status: http.StatusRequestEntityTooLarge, Contacted: true}, nil
}

// HasOpenAIRejectionEvidence reports whether an eligible HTTP rejection identifies OpenAI.
func HasOpenAIRejectionEvidence(status int, headers http.Header, body []byte) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
	default:
		return false
	}
	for name := range headers {
		name = strings.ToLower(name)
		if strings.HasPrefix(name, "x-openai-") || name == "openai-processing-ms" {
			return true
		}
	}
	return openAIWord.MatchString(lowerASCII(string(body)))
}
