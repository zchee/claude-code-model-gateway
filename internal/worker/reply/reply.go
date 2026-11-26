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
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

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
	ObserveJSON                                                           func([]byte)
}

// Result contains the upstream stream for an unhandled reply, or the route and capture outcomes
// for a buffered reply. A stream's recipient owns its body. Overflow contains diagnostics only.
type Result struct {
	Stream                            *http.Response
	Status, CaptureStatus             int
	CaptureHeaders                    http.Header
	ResponseBytes                     int64
	StatusOverride, CompactionOutcome string
	Incomplete                        bool
	Overflow                          jsontext.Value
}

// Handle calls forward for each attempt, closes buffered bodies and writes one client response.
// It returns an untouched open response for the stream relay, or an error wrapping http.ErrAbortHandler when the client connection must abort.
func Handle(ctx context.Context, w http.ResponseWriter, req Request, forward func(context.Context) (*http.Response, error)) (Result, error) {
	return Result{}, errors.New("not implemented")
}

// HasOpenAIRejectionEvidence reports whether an eligible HTTP rejection identifies OpenAI.
func HasOpenAIRejectionEvidence(status int, headers http.Header, body []byte) bool { return false }
