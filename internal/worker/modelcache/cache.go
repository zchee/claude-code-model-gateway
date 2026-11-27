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
	"math"
	"net"
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
	// llamaProvider and studioProvider are the route providers served by the local engines.
	llamaProvider  = "local"
	studioProvider = "lmstudio"
	// maxListingBytes bounds the proxy listing body; a larger body counts as an unreadable listing.
	maxListingBytes = 16 << 20
	// nodeTimerMax is the largest delay in milliseconds a Node timer keeps.
	nodeTimerMax = 1<<31 - 1
	// maxRedirects bounds the redirect chain of a proxy listing request.
	maxRedirects = 10
)

// directTransport carries the cache's requests when the injected client has no transport of its own
// or names the process-wide default. It never consults the environment's proxy settings and keeps no
// idle connections, so it outlives no request. It neither asks for nor decodes a compressed body, as
// the source sends no Accept-Encoding and reads the bytes as sent, so the size bounds count the
// bytes on the wire.
var directTransport = &http.Transport{
	Proxy:              nil,
	DialContext:        (&net.Dialer{}).DialContext,
	DisableKeepAlives:  true,
	DisableCompression: true,
}

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
	listingClient *http.Client
	catalogClient *http.Client
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

	current atomic.Pointer[committed]
	// tickets orders refreshes by their start; a commit is dropped when a later-started refresh
	// already committed.
	tickets  atomic.Uint64
	commitMu sync.Mutex
	// committedTicket is guarded by commitMu.
	committedTicket uint64

	// discoveryMu serializes the read-compare-write of the discovery cache. No network work runs under it.
	discoveryMu sync.Mutex

	// life bounds every Anthropic catalog refresh. Start ties it to the worker context, so a refresh
	// started before Start ends with the worker too.
	life     context.Context
	stopLife context.CancelFunc
	// lifeDone is life's Done channel, read once so that no context method is called under mu.
	lifeDone <-chan struct{}
	// catalogMu makes the catalog read and the election of the one refresh in flight one operation.
	catalogMu sync.Mutex
	flight    *catalogFlight

	// mu guards workerDone, started and closed, and is the gate between registering background work
	// with wg and Wait: registration checks and adds under it, and Wait crosses it before joining.
	// Nothing under it calls into a context or another caller-supplied value, so Wait always crosses it.
	mu sync.Mutex
	// workerDone is the Done channel of the worker context from Start, captured before mu is taken;
	// nil until Start.
	workerDone <-chan struct{}
	started    bool
	// closed is set by Wait; no background work is registered afterwards.
	closed bool
	wg     sync.WaitGroup
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
	// Each request kind gets its own client built from the injected transport only: never a cookie
	// jar, a client timeout or the process-wide default transport.
	transport := opts.Client.Transport
	if transport == nil || transport == http.DefaultTransport {
		transport = directTransport
	}
	c := &Cache{
		opts:          opts,
		listingClient: &http.Client{Transport: transport, CheckRedirect: followListingRedirect},
		catalogClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		lookup:        opts.Lookup,
		windows:       maps.Clone(opts.Windows),
		clock:         opts.Clock,
		logf:          opts.Logf,
	}
	c.life, c.stopLife = context.WithCancel(context.Background())
	c.lifeDone = c.life.Done()
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
		// Credentials in the URL would become a Basic Authorization header the caller never sent.
		if base.User != nil {
			c.logf("model-gateway: the Anthropic catalog base URL carries credentials; the catalog is not refreshed")
		} else {
			c.anthropicBase = base
		}
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
	c.commit(context.Background(), 0, nil, &committed{
		snapshot: route.NewSnapshot(resolver, "fallback", catalog, c.grokIDs()),
		source:   "fallback",
		rows:     catalog,
		count:    len(rows),
	})
	return c, nil
}

// followListingRedirect follows a proxy listing redirect as the source's fetch helper does, without a
// Referer naming the previous URL and never to a target that carries credentials.
func followListingRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	if req.URL.User != nil {
		return errors.New("redirect target carries credentials")
	}
	req.Header.Del("Referer")
	return nil
}

// nodeTimerDelay converts a refresh interval in milliseconds the way a Node timer does: a fraction
// is truncated and a value below one millisecond runs every millisecond. A value above the largest
// timer delay, including +Inf, is held at that delay; the source's timer would instead run it every
// millisecond.
func nodeTimerDelay(ms float64) time.Duration {
	switch {
	case ms > nodeTimerMax:
		ms = nodeTimerMax
	case !(ms >= 1):
		return time.Millisecond
	}
	return time.Duration(math.Trunc(ms)) * time.Millisecond
}

// Start runs the initial refresh and schedules periodic refreshes, exponential retries, and
// background Anthropic catalog refreshes under ctx until ctx is canceled.
func (c *Cache) Start(ctx context.Context) {
	if ctx == nil {
		return
	}
	// The context is called only outside mu, so a context that blocks in a method cannot hold up Wait.
	done := ctx.Done()
	c.mu.Lock()
	if c.started || c.closed {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.workerDone = done
	c.wg.Go(func() { c.run(ctx) })
	c.mu.Unlock()
	// A catalog refresh already running, or started later, ends with the worker.
	context.AfterFunc(ctx, c.stopLife)
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
// No background work starts once Wait has been called, so none is running after it returns.
func (c *Cache) Wait() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.wg.Wait()
}

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
	grokIDs := c.grokIDs()
	var probes measurements
	// Each pass builds on the generation committed when it began. When an earlier-started refresh
	// commits meanwhile, the pass is rebuilt on that generation, so an empty listing keeps the latest
	// route table and outage preservation reads the latest committed measurement. Only refreshes that
	// started earlier can commit without superseding this one, so the loop ends.
	for base := c.current.Load(); ; base = c.current.Load() {
		routes, source := c.selectRoutes(base, listing)
		c.measure(ctx, &probes, routes)
		if err := ctx.Err(); err != nil {
			return err
		}
		next, err := c.prepare(base, routes, source, &probes, grokIDs)
		if err != nil {
			return err
		}
		switch c.commit(ctx, ticket, base, next) {
		case published:
			// A stand-in list is never persisted (lib/request-worker.js:1522-1523).
			if source == "proxy" {
				c.syncDiscovery(false)
			}
			return nil
		case superseded:
			return nil
		case canceled:
			return ctx.Err()
		}
	}
}

// selectRoutes picks the route table of a refresh. A refresh that cannot read the listing keeps the
// table of base; only before any table exists do models.json and then the built-in Codex list stand
// in (lib/request-worker.js:1485-1499).
func (c *Cache) selectRoutes(base *committed, listing []proxymodels.Route) ([]proxymodels.Route, string) {
	switch {
	case len(listing) > 0:
		return listing, "proxy"
	case len(base.routes) > 0:
		return base.routes, base.source
	}
	if routes := c.modelsFileRoutes(); len(routes) > 0 {
		return routes, "models.json"
	}
	return defaultRoutes(), "fallback"
}

// measurements are the local-engine probes one refresh took. A probe depends on the settings alone,
// not on the route table, so a rebuilt pass reuses them and probes only an engine not yet measured.
type measurements struct {
	llamaDone, studioDone bool
	llama                 localprov.LlamaState
	studio                localprov.StudioState
}

// measure asks llama-server and LM Studio in parallel when routes list their rows and this refresh
// has not measured them yet. Nothing here reaches the cache until the refresh commits.
func (c *Cache) measure(ctx context.Context, m *measurements, routes []proxymodels.Route) {
	opts := localprov.ProbeOptions{Client: c.listingClient}
	var wg sync.WaitGroup
	if !m.llamaDone && listsProvider(routes, llamaProvider) {
		m.llamaDone = true
		wg.Go(func() { m.llama = localprov.ProbeLlama(ctx, c.lookup, opts) })
	}
	if !m.studioDone && listsProvider(routes, studioProvider) {
		m.studioDone = true
		wg.Go(func() { m.studio = localprov.ProbeStudio(ctx, c.lookup, c.proxyConfigDir, opts) })
	}
	wg.Wait()
}

// prepare builds the generation that follows base. An engine the routes do not list has no state. An
// engine that did not answer at the address base measured keeps base's window or models, so an outage
// does not shrink the picker (lib/local-llama.js, lib/lmstudio.js).
func (c *Cache) prepare(base *committed, routes []proxymodels.Route, source string, m *measurements, grokIDs []string) (*committed, error) {
	var llama *localprov.LlamaState
	if listsProvider(routes, llamaProvider) {
		state := m.llama
		if prev := base.llama; !state.Reachable && state.Port != 0 && prev != nil && prev.Port == state.Port && prev.ContextWindow != 0 {
			state.ContextWindow = prev.ContextWindow
		}
		llama = &state
	}
	var studio *localprov.StudioState
	if listsProvider(routes, studioProvider) {
		state := m.studio
		if prev := base.studio; !state.Reachable && state.BaseURL != "" && prev != nil && prev.BaseURL == state.BaseURL {
			state.Models = prev.Models
		}
		studio = cloneStudio(&state)
	}

	inputs := policy.ResolverInputs{Routes: proxymodels.NewTable(routes), Windows: c.windows}
	if llama != nil {
		inputs.LocalWindow, inputs.LocalPort = llama.ContextWindow, int(llama.Port)
	}
	if studio != nil {
		inputs.LMStudioURL, inputs.LMStudioWindows = studio.BaseURL, studio.Windows()
	}
	resolver := policy.NewResolver(inputs)
	rows := listingRows(resolver, routes, grokIDs, c.listDispatch)
	catalog, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	return &committed{
		snapshot: route.NewSnapshot(resolver, source, catalog, slices.Clone(grokIDs)),
		source:   source,
		routes:   routes,
		rows:     catalog,
		count:    len(rows),
		llama:    llama,
		studio:   studio,
	}, nil
}

// commitResult is the outcome of one attempt to publish a generation.
type commitResult int

const (
	published commitResult = iota
	// superseded means a later-started refresh already committed; this one is dropped whole.
	superseded
	// canceled means the refresh's context ended before publication.
	canceled
	// rebased means another generation was committed after the attempt's base was read.
	rebased
)

// commit publishes next when base is still the committed generation, the refresh was not canceled,
// and no later-started refresh committed. Sentry policy goes first, then the route snapshot, then
// the snapshot and generation become visible to readers together.
func (c *Cache) commit(ctx context.Context, ticket uint64, base, next *committed) commitResult {
	c.commitMu.Lock()
	defer c.commitMu.Unlock()
	// An ended context wins over supersession, so the caller always learns that its context ended.
	switch {
	case ctx.Err() != nil:
		return canceled
	case ticket < c.committedTicket:
		return superseded
	case c.current.Load() != base:
		return rebased
	}
	c.committedTicket = ticket
	next.generation = 1
	if base != nil {
		next.generation = base.generation + 1
	}
	c.opts.Sentry.Publish(next.generation, next.snapshot.Resolver(), maps.Clone(c.windows), c.env)
	c.opts.Routes.Publish(next.snapshot)
	c.current.Store(next)
	return published
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
	resp, err := c.listingClient.Do(req)
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

func listsProvider(routes []proxymodels.Route, provider string) bool {
	return slices.ContainsFunc(routes, func(r proxymodels.Route) bool { return r.Provider == provider })
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
		// A row marks an absent backend window with zero; the source prints its null window as "null"
		// (lib/request-worker.js:1454).
		window := "null"
		if m.BackendWindow != 0 {
			window = strconv.FormatInt(m.BackendWindow, 10)
		}
		switch {
		case err != nil:
			continue
		case s == nil:
			c.logf("model-gateway: sentry policy id=%s backendWindow=%s sentry=none", m.BackendID, window)
		default:
			c.logf("model-gateway: sentry policy id=%s backendWindow=%s sentry=%s effectiveTrigger=%s source=%s",
				m.BackendID, window, m.Sentry, strconv.FormatFloat(s.CompactTrigger, 'f', -1, 64), s.Source)
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
	return cloneStudio(c.current.Load().studio)
}

// cloneStudio returns a deep copy of state, or nil.
func cloneStudio(state *localprov.StudioState) *localprov.StudioState {
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
