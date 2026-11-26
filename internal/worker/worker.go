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

// Package worker serves the gateway's local endpoints and routes inference requests.
package worker

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// Forward handles a classified request using one catalog generation and the original body bytes.
// An accepting branch calls the last argument once before its sentry check or fallback probe.
// The returned result describes the terminal outcome, even when the response must be aborted.
type Forward func(http.ResponseWriter, *http.Request, route.Snapshot, route.Decision, []byte, func(outcome.Route, *outcome.Audit)) (outcome.Result, error)

// Options supplies the worker's startup settings and its three upstream handlers.
type Options struct {
	Version, ControlToken  string
	Lookup                 config.LookupEnv
	Routes                 *route.Store
	Anthropic, Proxy, Grok Forward
	BeginDrain             func()
	// Diagnostics receives bounded refusal reasons and statuses, not request contents.
	Diagnostics io.Writer
	// BeforeBody starts observation after local endpoints and before the first body read.
	// When supplied, its finalizer receives one terminal result, including cancellation.
	BeforeBody func(*http.Request) func(outcome.Result)
	// Models returns the serialized response for a local catalog request.
	Models func(context.Context, http.Header) (jsontext.Value, error)
	// HealthCatalog returns the source and row count without waiting for a refresh.
	HealthCatalog func() (string, int)
	// RefreshAnthropic starts catalog refresh for an accepted Anthropic request.
	RefreshAnthropic func(context.Context, http.Header)
	// LogRoute records an accepted or dispatch-unbound route audit entry immediately, never at completion.
	// When nil, no route audit entry is written.
	LogRoute func(*http.Request, outcome.Audit)
	// CodexReadiness computes the /healthz codexReadiness summary when non-nil.
	// When nil, /healthz reports the default checking summary.
	CodexReadiness func() readiness.CodexReadiness
}

// NewHandler constructs the handler shared by the TCP and Unix listeners.
func NewHandler(options Options) (http.Handler, error) { return nil, errors.New("not implemented") }

// ListenUnix binds path, reclaiming a refused socket but never a live owner or non-socket file.
func ListenUnix(path string) (net.Listener, error) { return nil, errors.New("not implemented") }
