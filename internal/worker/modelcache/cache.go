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

// Package modelcache maintains the worker model catalog, refresh schedule, and snapshot publication.
package modelcache

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	// DefaultRefreshInterval is the default periodic proxy listing refresh interval.
	DefaultRefreshInterval = 60 * time.Second
	// InitialRetryDelay is the first backoff delay after an empty or failed proxy listing refresh.
	InitialRetryDelay = time.Second
	// MaxRetryDelay is the upper bound for exponential retry backoff after empty or failed refreshes.
	MaxRetryDelay = 30 * time.Second
	// ProxyListingTimeout is the HTTP timeout for reading GET /v1/models from claude-code-proxy.
	ProxyListingTimeout = 2500 * time.Millisecond
	// AnthropicPageTimeout is the per-page HTTP timeout for fetching the upstream Anthropic catalog.
	AnthropicPageTimeout = 5 * time.Second
	// MaxAnthropicPageUTF16Units is the maximum decoded UTF-16 code-unit length permitted for one upstream Anthropic catalog page.
	MaxAnthropicPageUTF16Units = 2 * 1024 * 1024
	// MaxAnthropicPages is the maximum number of paginated requests allowed per Anthropic catalog fetch.
	MaxAnthropicPages = 10
)

var errNotImplemented = errors.New("not implemented")

// RoutePublisher receives each completed routing and advertised-model snapshot.
type RoutePublisher interface {
	Publish(snapshot route.Snapshot)
}

// SentryPublisher receives each refresh generation's immutable resolver, backend windows, and env trigger.
type SentryPublisher interface {
	Publish(generation uint64, resolver sentry.ModelResolver, windows policy.Windows, env float64)
}

// Options configures a worker model cache without ambient environment or filesystem reads.
// EffectiveBaseURL gates discovery-cache writes; an error must never enable a write.
// CurrentPins supplies effective pin model IDs per family (opus, sonnet, fable) for relabelling older rows.
// GrokIDs supplies cached Grok model IDs; nil or an empty slice falls back to the built-in Grok defaults.
type Options struct {
	Lookup           config.LookupEnv
	Paths            paths.Resolver
	Windows          policy.Windows
	ProxyBaseURL     string
	AnthropicBaseURL string
	DefaultBaseURL   string
	Shadow           bool
	Client           *http.Client
	Clock            func() time.Time
	Routes           RoutePublisher
	Sentry           SentryPublisher
	EffectiveBaseURL func() (string, error)
	CurrentPins      func() map[string]string
	GrokIDs          func() []string
	Logf             func(format string, args ...any)
}

// DiscoverySyncResult reports the outcome of syncing the Claude Code gateway discovery cache.
type DiscoverySyncResult struct {
	State      string
	Reason     string
	CachePath  string
	ModelCount int
}

// Cache holds the worker's advertised model list, route snapshot, sentry policy generation,
// live local-provider probes, Anthropic catalog refresher, and discovery-cache writer.
type Cache struct{}

// New initializes the fallback model catalog and publishes generation 1 to Routes and Sentry.
// Resolver inputs and supplied window maps are copied so later caller mutations do not affect snapshots.
func New(opts Options) (*Cache, error) {
	_ = opts
	return &Cache{}, errNotImplemented
}

// Start runs the initial refresh and schedules periodic refreshes, exponential retries, and
// background Anthropic catalog refreshes under ctx until ctx is canceled.
func (c *Cache) Start(ctx context.Context) {
	_ = ctx
}

// Wait blocks until all background refresh, retry, and Anthropic catalog goroutines have exited.
func (c *Cache) Wait() {}

// Refresh fetches the proxy listing and probes live local providers without holding commit locks across
// network calls, then commits in order: obtain the next generation once, publish Sentry first with the
// snapshot's resolver, publish Routes next, and expose (snapshot, generation) together. An older refresh
// that finishes after a newer committed refresh does not overwrite it.
func (c *Cache) Refresh(ctx context.Context) error {
	_ = ctx
	return errNotImplemented
}

// Snapshot returns the most recently published route snapshot and its generation.
func (c *Cache) Snapshot() (route.Snapshot, uint64) {
	return route.Snapshot{}, 0
}

// Generation returns the strictly increasing generation of the most recently published snapshot.
func (c *Cache) Generation() uint64 {
	return 0
}

// HealthCatalog returns the active catalog source ("proxy", "models.json", or "fallback") and model count.
func (c *Cache) HealthCatalog() (source string, count int) {
	return "", 0
}

// ModelsResponse builds the GET /v1/models JSON response body using allowed credential headers from incoming.
// When serving stale cached rows immediately, any background refresh runs under the worker context from Start rather than ctx.
func (c *Cache) ModelsResponse(ctx context.Context, incoming http.Header) (jsontext.Value, error) {
	_, _ = ctx, incoming
	return nil, errNotImplemented
}

// TriggerAnthropicRefresh initiates a non-blocking Anthropic catalog refresh under the worker context from Start when stale and credentials are present.
func (c *Cache) TriggerAnthropicRefresh(incoming http.Header) {
	_ = incoming
}

// SyncDiscoveryCache writes the gateway discovery cache when EffectiveBaseURL returns DefaultBaseURL
// without error and Shadow is false. An error from EffectiveBaseURL is returned without writing.
func (c *Cache) SyncDiscoveryCache(force bool) (DiscoverySyncResult, error) {
	_ = force
	return DiscoverySyncResult{}, errNotImplemented
}

// LlamaState returns a copy of the latest tracked llama-server state, or nil if inactive.
func (c *Cache) LlamaState() *localprov.LlamaState {
	return nil
}

// StudioState returns a deep copy of the latest tracked LM Studio state, or nil if inactive.
func (c *Cache) StudioState() *localprov.StudioState {
	return nil
}
