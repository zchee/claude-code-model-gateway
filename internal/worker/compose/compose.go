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
	"math"
	"net/http"
	"net/url"
	"sync"
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
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

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

	// SentryEnabled controls request-side refusals and overflow learning, not error normalization.
	// SentryEnvTrigger overrides derived triggers when positive.
	SentryEnabled    bool
	SentryEnvTrigger float64

	// Routes holds the published routing snapshot shared with ModelCache and worker.NewHandler.
	Routes *route.Store
	// ModelCache supplies /v1/models responses, /healthz catalog state, and Anthropic refreshes.
	ModelCache *modelcache.Cache
	// Sentry holds shared live session latches and learned ceilings across all branches.
	Sentry *sentry.State
	// Upstream executes pooled HTTP/1.1 inference requests for Anthropic, proxy, and Grok, each bound
	// to ServiceContext. The owner of the worker lifetime closes it after the request handlers have
	// returned; New never closes it.
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
	// SettingsLevelBaseURL is the startup fact /healthz reports as usage.settingsLevelBaseUrl: the
	// gateway's base URL was wired when the worker started.
	SettingsLevelBaseURL bool

	// Now returns the current time for readiness and upstream-state timestamps; nil defaults to time.Now.
	Now func() time.Time
	// BeginDrain triggers graceful worker shutdown after /drain is accepted.
	BeginDrain func()
	// Diagnostics receives bounded classifier-refusal logs.
	Diagnostics io.Writer

	// ReplyRetries and ReplyRetryDelay optionally override buffered reply retry defaults.
	// When nil, the worker's environment settings apply.
	ReplyRetries    *int
	ReplyRetryDelay *time.Duration
	// HeartbeatInterval optionally overrides the SSE heartbeat interval; a negative value disables
	// heartbeats. When zero, the worker's environment setting applies.
	HeartbeatInterval time.Duration
}

// New validates opts and returns the worker.Options wired to all live branches, telemetry,
// usage observation, catalog hooks, and readiness reporting. New starts no goroutines, timers,
// or catalog refreshes and registers no callback; the caller owns ModelCache.Start, ServiceContext
// cancellation, the closing of Upstream and every join at shutdown.
//
// The composed branches enforce the worker lifetime contract: every upstream send and SSE relay
// runs under ServiceContext, reply and compactguard receive the client request context, relays
// receive ClientDone bound to the client context, usage capture starts before branch execution
// and is exported once both the upstream reply was observed and the response was delivered (except
// Grok, which observes usage and finishes its route span without emitting a usage record), every
// output operation toward a client that has left is dropped, and request-side context-sentry
// refusals are written only by reply.WriteSentry after admission.
func New(opts Options) (worker.Options, error) {
	if err := validate(opts); err != nil {
		return worker.Options{}, err
	}
	c := newComposer(opts)

	out := worker.Options{
		Version:      opts.Version,
		ControlToken: opts.ControlToken,
		Lookup:       opts.Lookup,
		Routes:       opts.Routes,
		Anthropic:    c.anthropic,
		Proxy:        c.proxy,
		Grok:         c.grok,
		BeginDrain:   opts.BeginDrain,
		Diagnostics:  c.diagnostics,
		BeforeBody:   c.beforeBody,
		Models:       opts.ModelCache.ModelsResponse,
		HealthCatalog: func() (string, int) {
			return opts.ModelCache.HealthCatalog()
		},
		// The cache refreshes under the worker context it was started with, never the request's.
		RefreshAnthropic: func(_ context.Context, incoming http.Header) {
			opts.ModelCache.TriggerAnthropicRefresh(incoming)
		},
		LogRoute:             opts.Telemetry.LogRoute,
		SettingsLevelBaseURL: opts.SettingsLevelBaseURL,
	}
	if check := opts.ReadinessCheck; check != nil {
		store := *opts.UpstreamState
		health := readiness.HealthInfo{OK: true, Version: opts.Version}
		readinessOpts := readiness.CodexReadinessOptions{
			InstalledVersion: opts.InstalledVersion,
			CommandPath:      opts.CommandPath,
			LogsDir:          opts.LogsDir,
			UpstreamStore:    store,
		}
		out.CodexReadiness = func() readiness.CodexReadiness {
			return readiness.HealthzCodexReadiness(check.Latest(), health, c.now(), readinessOpts)
		}
	}
	return out, nil
}

// validate names the first missing collaborator or unusable base URL.
func validate(opts Options) error {
	required := []struct {
		missing bool
		name    string
	}{
		{opts.ServiceContext == nil, "ServiceContext"},
		{opts.Lookup == nil, "Lookup"},
		{opts.Routes == nil, "Routes"},
		{opts.ModelCache == nil, "ModelCache"},
		{opts.Sentry == nil, "Sentry"},
		{opts.Upstream == nil, "Upstream"},
		{opts.UpstreamState == nil, "UpstreamState"},
		{opts.ProxyReq == nil, "ProxyReq"},
		{opts.CompactGuard == nil, "CompactGuard"},
		{opts.CompactGuard != nil && opts.CompactGuard.ServiceContext == nil, "CompactGuard.ServiceContext"},
		{opts.Grok == nil, "Grok"},
		{opts.Usage == nil, "Usage"},
		{opts.Telemetry == nil, "Telemetry"},
	}
	for _, r := range required {
		if r.missing {
			return errors.New("compose: the worker needs a non-nil " + r.name)
		}
	}
	for _, base := range []struct{ value, name string }{{opts.AnthropicBaseURL, "AnthropicBaseURL"}, {opts.ProxyBaseURL, "ProxyBaseURL"}} {
		if origin(base.value) == "" {
			return errors.New("compose: " + base.name + " must be an absolute http or https URL")
		}
	}
	return nil
}

// origin renders the scheme and host of a base URL, or the empty string when it has neither.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// composer holds the settings every request of one worker shares.
type composer struct {
	opts        Options
	diagnostics *lockedWriter
	now         func() time.Time

	anthropicOrigin, proxyOrigin string
	heartbeat, firstEventWait    time.Duration
	retries                      *int
	retryDelay                   *time.Duration
	maxObserveBytes              int
	filterPlanTools              bool
}

func newComposer(opts Options) *composer {
	c := &composer{
		opts:            opts,
		diagnostics:     &lockedWriter{w: opts.Diagnostics},
		now:             opts.Now,
		anthropicOrigin: origin(opts.AnthropicBaseURL),
		proxyOrigin:     origin(opts.ProxyBaseURL),
		heartbeat:       opts.HeartbeatInterval,
		retries:         opts.ReplyRetries,
		retryDelay:      opts.ReplyRetryDelay,
		firstEventWait:  nodeTimer(config.LMStudioFirstWait.Get(opts.Lookup)),
		maxObserveBytes: clampInt(config.UsageMaxResponse.Get(opts.Lookup)),
		filterPlanTools: !config.KeepPlanTools.Get(opts.Lookup),
	}
	if c.diagnostics.w == nil {
		c.diagnostics.w = io.Discard
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.heartbeat == 0 {
		// A zero setting turns the source's heartbeat off (lib/request-worker.js:268-271,1797).
		c.heartbeat = -1
		if ms := config.SSEHeartbeatMilliseconds(opts.Lookup); ms != 0 {
			c.heartbeat = nodeTimer(ms)
		}
	}
	if c.retries == nil {
		c.retries = new(clampInt(config.WSRetries.Get(opts.Lookup)))
	}
	if c.retryDelay == nil {
		// An explicit zero retries at once; reply documents that as its own rule.
		delay := time.Duration(0)
		if ms := config.WSRetryDelay.Get(opts.Lookup); ms != 0 {
			delay = nodeTimer(ms)
		}
		c.retryDelay = &delay
	}
	return c
}

// clampInt converts a validated count or byte setting to the platform int, saturating at the largest
// representable value. The JavaScript timer cap applies to delays only, never to counts.
func clampInt(v float64) int {
	// float64(math.MaxInt) rounds up to a power of two that int cannot hold.
	if v >= float64(math.MaxInt) {
		return math.MaxInt
	}
	return int(v)
}

// maxTimerMillis is the largest delay a JavaScript timer accepts.
const maxTimerMillis = 1<<31 - 1

// nodeTimer converts a validated millisecond setting into the delay a JavaScript timer applies:
// anything below one millisecond or above the timer limit fires after one millisecond, and a
// fraction is dropped.
func nodeTimer(ms float64) time.Duration {
	if !(ms >= 1 && ms <= maxTimerMillis) {
		return time.Millisecond
	}
	return time.Duration(math.Trunc(ms)) * time.Millisecond
}

// lockedWriter serializes whole Write calls from every request onto one sink. Each caller writes a
// line with one Write call, so lines never interleave.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// beforeBody starts the request's route span before its body is read and returns the finalizer that
// receives its one terminal result.
func (c *composer) beforeBody(r *http.Request) func(outcome.Result) {
	span := c.opts.Telemetry.Start(r.Header)
	return func(res outcome.Result) {
		// A client that left gets no span and changes no Codex state (lib/request-worker.js:1151-1155).
		if res.Cancelled {
			return
		}
		// The Codex state is updated before the span is queued for export (lib/request-worker.js:1156-1167).
		c.noteCodex(res)
		span.Finish(res.Route, res.Status, res.StatusOverride)
	}
}

// noteCodex applies a finished Codex turn to the upstream state whether or not tracing is enabled:
// a successful turn clears both holds, a server failure or a broken reply records the upstream as
// unavailable (lib/request-worker.js:1155-1163).
func (c *composer) noteCodex(res outcome.Result) {
	if res.Route.Backend != "codex" {
		return
	}
	status := routeStatus(res.Status, res.StatusOverride)
	switch {
	case status == "ok" && res.Status >= 200 && res.Status < 300:
		c.opts.UpstreamState.ClearBlocked()
		c.opts.UpstreamState.ClearUnavailable()
	case res.Status >= 500 || status == "upstream_error" || status == "upstream_aborted":
		code := res.Status
		if code == 0 {
			code = http.StatusBadGateway
		}
		_, _ = c.opts.UpstreamState.SetUnavailable(code)
	}
}

// routeStatus classifies a terminal status as the source's span does; a zero status is the absence
// of an HTTP response (lib/request-worker.js:1012-1018).
func routeStatus(code int, override string) string {
	switch {
	case override != "":
		return override
	case code == 0:
		return "upstream_error"
	case code >= 500:
		return "server_error"
	case code >= 400:
		return "client_error"
	}
	return "ok"
}

// finishUsage exports the request's usage record once its reply was delivered. A defect in the
// emitter's own handling of the record is reported on the diagnostic stream and never reaches the
// response; the consumed capture is not finished again.
func (c *composer) finishUsage(capture *usage.Capture) {
	if capture == nil {
		return
	}
	defer func() {
		if recover() != nil {
			_, _ = io.WriteString(c.diagnostics, "model-gateway: the usage record of a delivered response could not be exported\n")
		}
	}()
	capture.Finish()
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
	switch {
	case opts.AuthTransport == nil:
		return nil, errors.New("compose: the Grok branch needs an auth transport of its own")
	case opts.InferenceClient == nil:
		return nil, errors.New("compose: the Grok branch needs an inference client")
	}
	auth, err := grok.NewAuthStore(grok.AuthOptions{
		File:      opts.AuthFile,
		Transport: opts.AuthTransport,
		Now:       opts.Now,
		Skew:      opts.Skew,
		Shadow:    opts.Shadow,
	})
	if err != nil {
		return nil, err
	}
	return grok.NewBranch(grok.BranchOptions{
		Endpoint:      opts.Endpoint,
		VersionFile:   opts.VersionFile,
		Auth:          auth,
		Client:        opts.InferenceClient,
		Sentry:        opts.Sentry,
		SentryEnabled: opts.SentryEnabled,
		Now:           opts.Now,
	})
}

// NewGrokModelIDs returns a path-bound closure delegating to grok.ModelIDsFromCache for modelcache.Options.GrokIDs.
func NewGrokModelIDs(cacheFile string) func() []string {
	return func() []string { return grok.ModelIDsFromCache(cacheFile) }
}

// NewGrokReadiness returns a read-only readiness callback delegating to grok.Readiness over grok.CheckAuth
// for catalog.LiveReadinessOptions.ReadGrok without refreshing credentials.
func NewGrokReadiness(authFile string) func(context.Context) catalog.Readiness {
	return func(context.Context) catalog.Readiness {
		return grok.Readiness(func() error { return grok.CheckAuth(authFile) })
	}
}
