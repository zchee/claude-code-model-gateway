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
	"encoding/json/jsontext"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// RouteInfo describes the Grok route metadata reported to telemetry and route logging.
type RouteInfo struct {
	SelectedModel  string
	EffectiveModel string
	Backend        string
	Effort         string
	Fallback       bool
	Via            string
}

// OverflowDetails carries structured context-overflow diagnostics attached to synthetic 413 responses.
type OverflowDetails struct {
	Phase        string   `json:"phase"`
	RefusedBy    string   `json:"refused_by"`
	RequestBytes int      `json:"request_bytes"`
	Tokens       *float64 `json:"tokens"`
	TokensFrom   *string  `json:"tokens_from"`
	LimitTokens  float64  `json:"limit_tokens"`
}

// Telemetry receives Grok route, overflow, and outcome observations.
type Telemetry interface {
	SetRoute(info RouteInfo)
	SetOverflow(overflow OverflowDetails)
	Finish(status int, outcome string)
}

// UsageObserver records upstream response metadata, byte counts, and translated usage for one request.
type UsageObserver interface {
	SetResponse(status int, headers http.Header)
	NoteResponseBytes(n int)
	ObserveEvent(event jsontext.Value)
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
	// Transport executes upstream HTTP requests to Endpoint.
	// If nil, http.DefaultTransport is used.
	Transport http.RoundTripper
	// Sentry holds session latches and learned ceilings. When nil or SentryEnabled is false,
	// context-sentry checks and usage recording are skipped.
	Sentry        *sentry.State
	SentryEnabled bool
	// Now returns the current time for fallback message and tool IDs. If nil, time.Now is used.
	Now func() time.Time
}

// Branch handles HTTP requests routed to the native Grok backend.
type Branch struct{}

// NewBranch constructs a Grok branch handler.
func NewBranch(opts BranchOptions) *Branch {
	return &Branch{}
}

// Request carries one classified Grok inference request into Branch.Handle.
type Request struct {
	// RawBody is the original client request body bytes before any translation.
	RawBody []byte
	// Path is the request URL path without query string (for example "/v1/messages" or "/v1/messages/count_tokens").
	Path string
	// AdvertisedModel is the picker model ID requested by the client (for example "claude-grok-4.7[1m]").
	AdvertisedModel string
	// BackendModel is the resolved Grok upstream model ID (for example "grok-4.7").
	BackendModel string
	// SentryTarget identifies the model key and effective sentry policy for this request.
	SentryTarget sentry.Target
	// Telemetry optionally records route, overflow, and finish status.
	Telemetry Telemetry
	// Usage optionally records response metadata, byte counts, and translated usage.
	Usage UsageObserver
}

// Handle serves one Grok request, enforcing stateless thread refusal, context-sentry gating,
// authentication, request and response translation, SSE streaming, and usage observation.
func (b *Branch) Handle(w http.ResponseWriter, r *http.Request, req Request) {}
