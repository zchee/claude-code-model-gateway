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
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// UsageObserver records upstream response metadata, decoded events, byte counts,
// and translated JSON bodies for one Grok request.
// The Grok branch observes usage but does not finish or emit a usage record.
type UsageObserver interface {
	SetResponse(status int, headers http.Header)
	ObserveEvent(event jsonx.WireValue)
	NoteResponseBytes(n int64)
	ObserveJSON(body []byte)
}

// BranchOptions configures a Grok HTTP branch handler without ambient reads.
type BranchOptions struct {
	// Endpoint is the Grok Responses URL. Empty uses DefaultEndpoint.
	Endpoint string
	// VersionFile is the path to version.json. Empty uses VersionFallback.
	VersionFile string
	// Auth supplies and refreshes Grok CLI access tokens.
	Auth *AuthStore
	// Client executes upstream inference HTTP requests to Endpoint.
	// Callers must supply a non-nil Client; Handle never falls back to http.DefaultTransport.
	Client *upstream.Client
	// Sentry holds session latches and learned ceilings for response-side observation.
	// When nil or SentryEnabled is false, response-side context-sentry observation is skipped.
	Sentry        *sentry.State
	SentryEnabled bool
	// Now returns the current time for fallback message and tool IDs. If nil, time.Now is used.
	Now func() time.Time
}

// Branch handles HTTP requests routed to the native Grok backend.
type Branch struct{}

// NewBranch constructs a Grok branch handler. It returns an error if opts.Client is nil.
func NewBranch(opts BranchOptions) (*Branch, error) {
	_ = opts
	return &Branch{}, nil
}

// Request carries one classified Grok inference request into Branch.Prepare and Branch.Handle.
type Request struct {
	// RawBody is the original client request body bytes before any translation.
	RawBody []byte
	// Path is the request URL path without query string (for example "/v1/messages" or "/v1/messages/count_tokens").
	Path string
	// AdvertisedModel is the picker model ID requested by the client (for example "claude-grok-4.7[1m]").
	AdvertisedModel string
	// BackendModel is the resolved Grok upstream model ID (for example "grok-4.7").
	BackendModel string
	// SessionKey is the context-sentry session/agent key computed by the worker from the incoming headers.
	SessionKey string
	// SentryTarget identifies the model key and effective sentry policy for this request.
	SentryTarget sentry.Target
	// Usage optionally records response metadata, byte counts, and translated usage.
	Usage UsageObserver
}

// Prepare validates a Grok request before admission, auth, or upstream contact.
// On exact path "/v1/messages", an unsupported thread field is answered immediately with HTTP 400
// and Prepare returns (result, true) with resolved route metadata, Status 400, and StatusOverride
// "thread_unsupported". Otherwise Prepare writes nothing and returns (result, false) with the
// resolved route metadata populated.
func (b *Branch) Prepare(w http.ResponseWriter, r *http.Request, req Request) (outcome.Result, bool) {
	_, _, _, _ = b, w, r, req
	return outcome.Result{}, false
}

// Handle serves one accepted Grok request, performing authentication, request and response
// translation, SSE streaming, response-side context-sentry observation, and usage observation.
// Admission and request-side context-sentry checks are performed by the caller before Handle;
// Handle never runs an admission check and never finishes a usage record.
//
// A Grok response that cannot be translated or encoded before headers are committed is answered
// locally with HTTP 502, error type "api_error", fixed message
// "model-gateway: Grok response could not be translated.", and StatusOverride "upstream_error".
//
// After r.Context() is canceled, Handle writes nothing more to w but continues consuming
// the upstream stream; the configured Client governs its lifetime. outcome.Result.Cancelled
// is always false here and is decided by the worker root. If the upstream breaks after response
// headers are committed, Handle returns the terminal outcome facts alongside http.ErrAbortHandler
// rather than panicking.
func (b *Branch) Handle(w http.ResponseWriter, r *http.Request, req Request) (outcome.Result, error) {
	_, _, _, _ = b, w, r, req
	return outcome.Result{}, errNotImplemented
}
