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
	json "encoding/json/v2"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
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

const (
	// dispatchModel is the virtual dispatch row's backend spelling, and dispatchModelID its advertised ID
	// (lib/runtime.js:30).
	dispatchModel   = "auto"
	dispatchModelID = "claude-codex-auto"
	// maxListingBytes bounds the proxy listing body; a larger body counts as an unreadable listing.
	maxListingBytes = 16 << 20
	// nodeTimerMax is the largest delay in milliseconds a Node timer keeps; the source's interval
	// timer runs every millisecond for any value outside [1, nodeTimerMax].
	nodeTimerMax = 1<<31 - 1
)

// defaultModels are the Codex backends advertised before a listing was read (lib/request-worker.js:226-228).
var defaultModels = []string{"gpt-6.1-sol", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"}

// defaultGrokModels are listed when the Grok model cache supplies no IDs (lib/grok-backend.js:12-16).
var defaultGrokModels = []string{"grok-4.7", "grok-4.6", "grok-4.5"}

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

// committed is one published generation. It is never modified after publication, so readers load it
// without a lock and see the snapshot and its generation together.
type committed struct {
	generation uint64
	snapshot   route.Snapshot
	source     string
	// routes is the route table the next refresh keeps when the proxy listing cannot be read.
	routes []proxymodels.Route
	rows   jsontext.Value
	count  int
	llama  *localprov.LlamaState
	studio *localprov.StudioState
}

// catalogRow is one advertised model in the order and shape of the source's gatewayModel.
type catalogRow struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
	Window      int64  `json:"max_input_tokens"`
}

// Cache holds the worker's advertised model list, route snapshot, sentry policy generation,
// live local-provider probes, Anthropic catalog refresher, and discovery-cache writer.
type Cache struct {
	opts          Options
	lookup        config.LookupEnv
	windows       policy.Windows
	env           float64
	interval      time.Duration
	listDispatch  bool
	clock         func() time.Time
	logf          func(format string, args ...any)
	anthropicBase *url.URL

	stateDir       string
	discoveryPath  string
	globalConfig   string
	proxyConfigDir string

	llama  localprov.LlamaTracker
	studio localprov.StudioTracker

	current atomic.Pointer[committed]
	// tickets orders refreshes by their start; a commit is dropped when a later-started refresh
	// already committed.
	tickets  atomic.Uint64
	commitMu sync.Mutex
	// committedTicket is guarded by commitMu.
	committedTicket uint64

	// discoveryMu serializes the read-compare-write of the discovery cache. No network work runs under it.
	discoveryMu sync.Mutex

	mu      sync.Mutex
	worker  context.Context
	started bool
	flight  *catalogFlight
	wg      sync.WaitGroup
}

// New initializes the fallback model catalog and publishes generation 1 to Routes and Sentry.
// Resolver inputs and supplied window maps are copied so later caller mutations do not affect snapshots.
func New(opts Options) (*Cache, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("modelcache: an HTTP client is required")
	case opts.Routes == nil:
		return nil, errors.New("modelcache: a route publisher is required")
	case opts.Sentry == nil:
		return nil, errors.New("modelcache: a sentry publisher is required")
	}
	c := &Cache{
		opts:    opts,
		lookup:  opts.Lookup,
		windows: maps.Clone(opts.Windows),
		clock:   opts.Clock,
		logf:    opts.Logf,
	}
	if c.lookup == nil {
		c.lookup = func(string) (string, bool) { return "", false }
	}
	if c.clock == nil {
		c.clock = time.Now
	}
	if c.logf == nil {
		c.logf = func(string, ...any) {}
	}
	c.env = config.CompactTrigger.Get(c.lookup)
	c.interval = nodeTimerDelay(config.ModelsRefresh.Get(c.lookup))
	c.listDispatch = config.ListDispatchModel.Get(c.lookup)
	if base, err := url.Parse(opts.AnthropicBaseURL); err == nil && base.Scheme != "" && base.Host != "" {
		c.anthropicBase = base
	}
	// An unresolvable path leaves its file absent for reads and skipped for writes.
	c.stateDir, _ = opts.Paths.StateDir()
	c.discoveryPath, _ = opts.Paths.DiscoveryCachePath()
	c.globalConfig, _ = opts.Paths.GlobalConfigPath()
	if dir, err := opts.Paths.ProxyConfigDir(); err == nil && filepath.IsAbs(dir) && filepath.Clean(dir) == dir {
		c.proxyConfigDir = dir
	}

	// The startup catalog lists the Codex defaults only, before any listing was read
	// (lib/request-worker.js:1211-1214).
	resolver := policy.NewResolver(policy.ResolverInputs{Windows: c.windows})
	ids := defaultModels
	if c.listDispatch {
		ids = append(slices.Clone(defaultModels), dispatchModel)
	}
	rows := make([]catalogRow, 0, len(ids))
	for _, id := range ids {
		if row, ok := gatewayRow(resolver, id, "codex"); ok {
			rows = append(rows, row)
		}
	}
	catalog, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	c.commit(0, &committed{
		snapshot: route.NewSnapshot(resolver, "fallback", catalog, c.grokIDs()),
		source:   "fallback",
		rows:     catalog,
		count:    len(rows),
	})
	return c, nil
}

// nodeTimerDelay converts a refresh interval in milliseconds the way a Node timer does.
func nodeTimerDelay(ms float64) time.Duration {
	if !(ms >= 1 && ms <= nodeTimerMax) {
		return time.Millisecond
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// Start runs the initial refresh and schedules periodic refreshes, exponential retries, and
// background Anthropic catalog refreshes under ctx until ctx is canceled.
func (c *Cache) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || ctx == nil {
		return
	}
	c.started = true
	c.worker = ctx
	c.wg.Go(func() { c.run(ctx) })
}

// run owns the periodic and retry timers. The interval timer starts with the worker, independent of
// how long the first refresh takes, and a retry is scheduled as soon as a listing comes back empty
// (lib/request-worker.js:1461-1484,1535-1538).
func (c *Cache) run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	var retry *time.Timer
	var retryC <-chan time.Time
	defer func() {
		if retry != nil {
			retry.Stop()
		}
	}()
	delay := InitialRetryDelay
	onListing := func(listed bool) {
		if listed {
			delay = InitialRetryDelay
			return
		}
		if retry == nil {
			retry = time.NewTimer(delay)
			retryC = retry.C
			delay = min(delay*2, MaxRetryDelay)
		}
	}
	if err := c.refresh(ctx, onListing); err == nil {
		c.logSentryPolicies()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.refresh(ctx, onListing)
		case <-retryC:
			retry, retryC = nil, nil
			_ = c.refresh(ctx, onListing)
		}
	}
}

// Wait blocks until all background refresh, retry, and Anthropic catalog goroutines have exited.
func (c *Cache) Wait() { c.wg.Wait() }

// Refresh fetches the proxy listing and probes live local providers without holding commit locks across
// network calls, then commits in order: obtain the next generation once, publish Sentry first with the
// snapshot's resolver, publish Routes next, and expose (snapshot, generation) together. An older refresh
// that finishes after a newer committed refresh does not overwrite it.
func (c *Cache) Refresh(ctx context.Context) error {
	return c.refresh(ctx, nil)
}

// refresh reports whether the proxy listing had rows to onListing as soon as the listing is read.
// It returns the context's error, without committing, when ctx ends first.
func (c *Cache) refresh(ctx context.Context, onListing func(listed bool)) error {
	ticket := c.tickets.Add(1)
	listing := c.fetchListing(ctx)
	if onListing != nil {
		onListing(len(listing) > 0)
	}
	// A refresh that cannot read the listing keeps the last route table; only before any table
	// exists do models.json and then the built-in Codex list stand in (lib/request-worker.js:1485-1499).
	base := c.current.Load()
	routes, source := base.routes, base.source
	switch {
	case len(listing) > 0:
		routes, source = listing, "proxy"
	case len(routes) == 0:
		routes, source = c.modelsFileRoutes(), "models.json"
		if len(routes) == 0 {
			routes, source = defaultRoutes(), "fallback"
		}
	}
	llama, studio := c.probe(ctx, routes)
	if err := ctx.Err(); err != nil {
		return err
	}

	inputs := policy.ResolverInputs{Routes: proxymodels.NewTable(routes), Windows: c.windows}
	if llama != nil {
		inputs.LocalWindow, inputs.LocalPort = llama.ContextWindow, int(llama.Port)
	}
	if studio != nil {
		inputs.LMStudioURL, inputs.LMStudioWindows = studio.BaseURL, studio.Windows()
	}
	resolver := policy.NewResolver(inputs)
	grokIDs := c.grokIDs()
	rows := listingRows(resolver, routes, grokIDs, c.listDispatch)
	catalog, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	if !c.commit(ticket, &committed{
		snapshot: route.NewSnapshot(resolver, source, catalog, grokIDs),
		source:   source,
		routes:   routes,
		rows:     catalog,
		count:    len(rows),
		llama:    llama,
		studio:   studio,
	}) {
		return nil
	}
	// A stand-in list is never persisted (lib/request-worker.js:1522-1523).
	if source == "proxy" {
		c.syncDiscovery(false)
	}
	return nil
}

// commit publishes next unless a later-started refresh already committed. Sentry policy goes first,
// then the route snapshot, then the snapshot and generation become visible to readers together.
func (c *Cache) commit(ticket uint64, next *committed) bool {
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	if ticket < c.committedTicket {
		return false
	}
	c.committedTicket = ticket
	next.generation = 1
	if previous := c.current.Load(); previous != nil {
		next.generation = previous.generation + 1
	}
	c.opts.Sentry.Publish(next.generation, next.snapshot.Resolver(), maps.Clone(c.windows), c.env)
	c.opts.Routes.Publish(next.snapshot)
	c.current.Store(next)
	return true
}

// fetchListing reads the proxy's GET /v1/models and parses the raw data member of its last
// definition. Any failure is an empty listing, as in the source (lib/request-worker.js:1478-1482).
func (c *Cache) fetchListing(ctx context.Context) []proxymodels.Route {
	ctx, cancel := context.WithTimeout(ctx, ProxyListingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.opts.ProxyBaseURL+"/v1/models", http.NoBody)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "model-gateway")
	resp, err := c.opts.Client.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := readLimited(resp.Body, maxListingBytes)
	if err != nil {
		return nil
	}
	data, ok := lastMember(body, "data")
	if !ok {
		return nil
	}
	return proxymodels.ParseListing(data)
}

// modelsFileRoutes reads the legacy models.json stand-in from the state directory.
func (c *Cache) modelsFileRoutes() []proxymodels.Route {
	if c.stateDir == "" {
		return nil
	}
	data, err := readSmallFile(filepath.Join(c.stateDir, "models.json"))
	if err != nil {
		return nil
	}
	return proxymodels.ModelsFileRoutes(data)
}

func defaultRoutes() []proxymodels.Route {
	routes := make([]proxymodels.Route, 0, len(defaultModels))
	for _, id := range defaultModels {
		routes = append(routes, proxymodels.Route{ID: id, Provider: "codex"})
	}
	return routes
}

// probe asks llama-server and LM Studio in parallel when the table lists their rows. The trackers keep
// the last measurement for an outage at the same address; the results belong to this refresh only.
func (c *Cache) probe(ctx context.Context, routes []proxymodels.Route) (llama *localprov.LlamaState, studio *localprov.StudioState) {
	opts := localprov.ProbeOptions{Client: c.opts.Client}
	var wg sync.WaitGroup
	wg.Go(func() { llama = c.llama.Refresh(ctx, routes, c.lookup, opts) })
	wg.Go(func() { studio = c.studio.Refresh(ctx, routes, c.lookup, c.proxyConfigDir, opts) })
	wg.Wait()
	return llama, studio
}

func (c *Cache) grokIDs() []string {
	if c.opts.GrokIDs == nil {
		return nil
	}
	return slices.Clone(c.opts.GrokIDs())
}

// listingRows orders the catalog as the source does: Codex proxy rows and the dispatch row, then the
// Grok backend rows, then every other proxy row (lib/request-worker.js:1504-1521).
func listingRows(resolver policy.Resolver, routes []proxymodels.Route, grokIDs []string, listDispatch bool) []catalogRow {
	var proxyRows []proxymodels.Route
	for _, r := range proxymodels.AdvertisedRoutes(routes) {
		if m := resolver.Resolve(r.ID); m != nil && m.Transport == "proxy" {
			proxyRows = append(proxyRows, r)
		}
	}
	rows := make([]catalogRow, 0, len(proxyRows)+len(defaultGrokModels)+1)
	add := func(id, backend string) {
		if row, ok := gatewayRow(resolver, id, backend); ok {
			rows = append(rows, row)
		}
	}
	for _, r := range proxyRows {
		if r.Provider == "codex" {
			add(r.ID, "codex")
		}
	}
	if listDispatch {
		add(dispatchModel, "codex")
	}
	if len(grokIDs) == 0 {
		grokIDs = defaultGrokModels
	}
	for _, id := range grokIDs {
		if m := resolver.Resolve(id); m != nil && m.Transport == "grok" {
			add(id, "grok")
		}
	}
	for _, r := range proxyRows {
		if r.Provider != "codex" {
			add(r.ID, r.Provider)
		}
	}
	return rows
}

// gatewayRow is the source's gatewayModel: a row only for a model the named backend serves
// (lib/request-worker.js:703-711).
func gatewayRow(resolver policy.Resolver, id, backend string) (catalogRow, bool) {
	if id == dispatchModel {
		return catalogRow{ID: dispatchModelID, DisplayName: "Sidequest Dispatch (Codex)", Type: "model", Window: resolver.CodexWindow(id)}, true
	}
	if m := resolver.Resolve(id); m == nil || m.Backend != backend {
		return catalogRow{}, false
	}
	return catalogRow{ID: resolver.ClientID(id), DisplayName: displayName(id, backend), Type: "model", Window: resolver.CodexWindow(id)}, true
}

// displayName labels a row per backend (lib/request-worker.js:216-220).
func displayName(id, backend string) string {
	switch backend {
	case "grok":
		if rest, ok := strings.CutPrefix(id, "grok-"); ok {
			id = "Grok " + rest
		}
		return strings.ReplaceAll(id, "-", " ")
	case "codex":
		if rest, ok := strings.CutPrefix(id, "gpt-"); ok {
			id = "GPT-" + rest
		}
		return strings.TrimSuffix(id, "[1m]") + " (Codex)"
	}
	return proxymodels.DisplayName(id, backend)
}

// logSentryPolicies reports each table row's effective sentry once, after the first refresh
// (lib/request-worker.js:1448-1458).
func (c *Cache) logSentryPolicies() {
	for _, m := range policy.Rows() {
		s, err := policy.EffectiveSentry(&m, c.env, c.windows.Cap(m.Backend), c.windows[m.Backend].CompactAt)
		switch {
		case err != nil:
			continue
		case s == nil:
			c.logf("model-gateway: sentry policy id=%s backendWindow=%d sentry=none", m.BackendID, m.BackendWindow)
		default:
			c.logf("model-gateway: sentry policy id=%s backendWindow=%d sentry=%s effectiveTrigger=%s source=%s",
				m.BackendID, m.BackendWindow, m.Sentry, strconv.FormatFloat(s.CompactTrigger, 'f', -1, 64), s.Source)
		}
	}
}

// Snapshot returns the most recently published route snapshot and its generation.
func (c *Cache) Snapshot() (route.Snapshot, uint64) {
	current := c.current.Load()
	return current.snapshot, current.generation
}

// Generation returns the strictly increasing generation of the most recently published snapshot.
func (c *Cache) Generation() uint64 {
	return c.current.Load().generation
}

// HealthCatalog returns the active catalog source ("proxy", "models.json", or "fallback") and model count.
func (c *Cache) HealthCatalog() (source string, count int) {
	current := c.current.Load()
	return current.source, current.count
}

// LlamaState returns a copy of the latest tracked llama-server state, or nil if inactive.
func (c *Cache) LlamaState() *localprov.LlamaState {
	state := c.current.Load().llama
	if state == nil {
		return nil
	}
	return new(*state)
}

// StudioState returns a deep copy of the latest tracked LM Studio state, or nil if inactive.
func (c *Cache) StudioState() *localprov.StudioState {
	state := c.current.Load().studio
	if state == nil {
		return nil
	}
	copied := *state
	copied.Models = slices.Clone(state.Models)
	if state.Invalid != nil {
		copied.Invalid = new(*state.Invalid)
	}
	return &copied
}
