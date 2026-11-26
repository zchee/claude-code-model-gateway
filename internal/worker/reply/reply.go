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
	"net/http"
	"time"

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
// Nil retry settings select two retries separated by 250 milliseconds; explicit zero is respected.
// NewMessageID supplies the complete ID for a synthesized message.
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
}

// Result carries terminal facts, or an unsettled response whose open body belongs to the caller.
type Result struct {
	outcome.Result
	Stream *upstream.Response
}

// Handle calls forward for each attempt, closes buffered bodies and writes one client response.
// It returns an untouched open response for the stream relay, or an error wrapping http.ErrAbortHandler when the client connection must abort.
func Handle(ctx context.Context, w http.ResponseWriter, req Request, forward upstream.Send) (Result, error) {
	return Result{}, errors.New("not implemented")
}

// WriteSentry reports a local refusal using the original request bytes and the provider's display label.
func WriteSentry(w http.ResponseWriter, original []byte, provider string, cross sentry.Crossing) (outcome.Result, error) {
	return outcome.Result{}, errors.New("not implemented")
}

// WriteEngineOverflow normalizes an upstream refusal using the prepared request and source HTTP status.
func WriteEngineOverflow(w http.ResponseWriter, req Request, status int, counts sentry.Counts) (outcome.Result, error) {
	return outcome.Result{}, errors.New("not implemented")
}

// HasOpenAIRejectionEvidence reports whether an eligible HTTP rejection identifies OpenAI.
func HasOpenAIRejectionEvidence(status int, headers http.Header, body []byte) bool { return false }
