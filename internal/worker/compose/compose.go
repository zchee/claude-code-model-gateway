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

// Package compose wires the worker's live collaborators into worker.Options.
package compose

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

var errNotImplemented = errors.New("not implemented")

// Compile-time checks that real gateway collaborators satisfy every package interface directly without adapters.
var (
	_ reply.UsageCapture         = (*usage.Capture)(nil)
	_ sse.UsageCapture           = (*usage.Capture)(nil)
	_ compactguard.UsageCapture  = (*usage.Capture)(nil)
	_ grok.UsageObserver         = (*usage.Capture)(nil)
	_ modelcache.RoutePublisher  = (*route.Store)(nil)
	_ modelcache.SentryPublisher = (*sentry.State)(nil)
	_ catalog.ReadinessSource    = (*catalog.LiveReadinessSource)(nil)
	_ sentry.ModelResolver       = policy.Resolver{}
)

// Options supplies the worker's lifetime context, configuration, and live collaborators.
// Every required collaborator and base URL is validated at construction; New returns an error
// naming any missing collaborator, including a nil ServiceContext or nil CompactGuard.ServiceContext.
type Options struct {
	// ServiceContext bounds all upstream requests, streaming relays, and compaction guard work.
	// Canceling ServiceContext terminates in-flight upstream work across all branches.
	ServiceContext context.Context

	// Version and ControlToken identify the worker and authenticate /drain control requests.
	Version      string
	ControlToken string

	// Lookup resolves worker environment variables without ambient process reads.
	Lookup config.LookupEnv

	// AnthropicBaseURL and ProxyBaseURL are the default upstream and local proxy origins.
	AnthropicBaseURL string
	ProxyBaseURL     string

	// CommandPath, InstalledVersion, and LogsDir supply installation metadata for error
	// messages and /healthz Codex readiness reporting.
	CommandPath      string
	InstalledVersion string
	LogsDir          string

	// Windows holds per-backend context window caps and compaction thresholds.
	Windows policy.Windows

	// SentryEnabled controls request-side context-sentry refusals and upstream overflow
	// normalization and learning. SentryEnvTrigger overrides derived triggers when positive.
	SentryEnabled    bool
	SentryEnvTrigger float64

	// Routes holds the published routing snapshot shared with ModelCache and worker.NewHandler.
	Routes *route.Store
	// ModelCache supplies /v1/models responses, /healthz catalog state, and Anthropic refreshes.
	ModelCache *modelcache.Cache
	// Sentry holds shared live session latches and learned ceilings across all branches.
	Sentry *sentry.State
	// Upstream executes pooled HTTP/1.1 inference requests for Anthropic, proxy, and Grok.
	Upstream *upstream.Client
	// UpstreamState persists Codex blocked and unavailable state across turns.
	UpstreamState *upstreamstate.Store
	// ProxyReq prepares proxy requests, resolving dispatch routes and tool schemas.
	ProxyReq *proxyreq.Handler
	// CompactGuard buffers and retries eligible proxy compaction SSE streams.
	// CompactGuard.ServiceContext must be non-nil at construction.
	CompactGuard *compactguard.Runner
	// Grok serves native Grok inference requests.
	Grok *grok.Branch
	// Usage creates request captures and exports counts-only OTLP usage records.
	Usage *usage.Emitter
	// HighWater optionally tracks per-session largest request body sizes.
	HighWater *usage.HighWater
	// Telemetry records local route audits and exports route spans.
	Telemetry *telemetry.Recorder
	// ReadinessCheck optionally supplies the latest shared proxy and Codex auth check for /healthz.
	// When nil, worker.Options.CodexReadiness is left nil.
	ReadinessCheck *readiness.SharedCheck

	// Now returns the current time for readiness and upstream-state timestamps; nil defaults to time.Now.
	Now func() time.Time
	// BeginDrain triggers graceful worker shutdown after /drain is accepted.
	BeginDrain func()
	// Diagnostics receives bounded classifier-refusal logs.
	Diagnostics io.Writer
	// NewMessageID optionally supplies message IDs for synthesized empty-completion replies.
	NewMessageID func() string

	// ReplyRetries and ReplyRetryDelay optionally override buffered reply retry defaults.
	ReplyRetries    *int
	ReplyRetryDelay *time.Duration
	// HeartbeatInterval, FirstEventWait, and MaxObserveBytes optionally override SSE relay defaults.
	HeartbeatInterval time.Duration
	FirstEventWait    time.Duration
	MaxObserveBytes   int
}

// New validates opts and returns the worker.Options wired to all live branches, telemetry,
// usage observation, catalog hooks, and readiness reporting.
//
// The composed branches enforce the worker lifetime contract: every upstream send and SSE relay
// runs under ServiceContext, reply and compactguard receive the client request context, relays
// receive ClientDone bound to the client context, usage capture starts before branch execution
// and finishes after observation (except Grok, which observes usage and finishes its route span
// without emitting a usage record), and request-side context-sentry refusals are written only by
// reply.WriteSentry after admission.
func New(opts Options) (worker.Options, error) {
	_ = opts
	return worker.Options{}, errNotImplemented
}

// GrokOptions configures a Grok auth store and branch with an auth transport isolated from inference.
type GrokOptions struct {
	Endpoint        string
	VersionFile     string
	AuthFile        string
	Shadow          bool
	Skew            time.Duration
	InferenceClient *upstream.Client
	AuthTransport   http.RoundTripper
	Sentry          *sentry.State
	SentryEnabled   bool
	Now             func() time.Time
}

// NewGrokBranch builds a Grok AuthStore and Branch using a dedicated auth transport and inference client.
// It rejects a nil AuthTransport or nil InferenceClient and propagates constructor errors without
// falling back to default transports or clients.
func NewGrokBranch(opts GrokOptions) (*grok.Branch, error) {
	_ = opts
	return nil, errNotImplemented
}

// NewGrokModelIDs returns a path-bound closure delegating to grok.ModelIDsFromCache for modelcache.Options.GrokIDs.
func NewGrokModelIDs(cacheFile string) func() []string {
	_ = cacheFile
	return nil
}

// NewGrokReadiness returns a read-only readiness callback delegating to grok.Readiness over grok.CheckAuth
// for catalog.LiveReadinessOptions.ReadGrok without refreshing credentials.
func NewGrokReadiness(authFile string) func(context.Context) catalog.Readiness {
	_ = authFile
	return nil
}
