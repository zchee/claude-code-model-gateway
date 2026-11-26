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

package readiness

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
)

// FreshnessTTL is the age at which a completed proxy and auth check becomes stale.
const FreshnessTTL = 15 * time.Second

// ProxyAuthResult records the shared proxy binary, /v1/models, and Codex auth status checks.
type ProxyAuthResult struct {
	ProxyBinary bool
	ProxyModels bool
	CodexAuth   bool
	CheckedAt   time.Time
}

// ProxyAuthProbe configures a live proxy and Codex auth status check.
type ProxyAuthProbe struct {
	ProxyBinary    string
	ProxyModelsURL string
	Client         *http.Client
	RunAuthStatus  func(ctx context.Context, binary string, args []string) (stdout, stderr []byte, err error)
}

// proxyModelsTimeout bounds the /v1/models answer the shared check waits for (lib/request-worker.js:56-61).
const proxyModelsTimeout = 2 * time.Second

// checkTransport carries the /v1/models probe of checks given no client. It never consults the
// environment's proxy settings and keeps no idle connections.
var checkTransport = &http.Transport{
	Proxy:             nil,
	DialContext:       (&net.Dialer{}).DialContext,
	DisableKeepAlives: true,
}

// accountPattern is the evidence of a signed-in Codex account in auth status output.
var accountPattern = regexp.MustCompile(`(?i)account`)

// ProbeProxyAndAuth checks binary presence and, when present, probes /v1/models and codex auth status in parallel.
// The result's CheckedAt is left zero; SharedCheck stamps it when the check completes.
func ProbeProxyAndAuth(ctx context.Context, probe ProxyAuthProbe) ProxyAuthResult {
	var result ProxyAuthResult
	if probe.ProxyBinary == "" {
		return result
	}
	if _, err := os.Stat(probe.ProxyBinary); err != nil {
		return result
	}
	result.ProxyBinary = true
	var wg sync.WaitGroup
	wg.Go(func() { result.ProxyModels = proxyModelsAnswering(ctx, probe) })
	wg.Go(func() { result.CodexAuth = codexAuthenticated(ctx, probe) })
	wg.Wait()
	return result
}

func proxyModelsAnswering(ctx context.Context, probe ProxyAuthProbe) bool {
	if probe.ProxyModelsURL == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, proxyModelsTimeout)
	defer cancel()
	client := &http.Client{Transport: checkTransport}
	if probe.Client != nil {
		client = probe.Client
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probe.ProxyModelsURL, http.NoBody)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "model-gateway")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func codexAuthenticated(ctx context.Context, probe ProxyAuthProbe) bool {
	if probe.RunAuthStatus == nil {
		return false
	}
	stdout, stderr, err := probe.RunAuthStatus(ctx, probe.ProxyBinary, []string{"codex", "auth", "status"})
	if err != nil {
		return false
	}
	// The source tests the two streams joined, so a match may span them.
	return accountPattern.Match(slices.Concat(stdout, stderr))
}

// SharedCheck deduplicates concurrent proxy and auth checks and serves the latest result immediately.
// When no check has completed or the latest result is at least FreshnessTTL old, Latest starts at most
// one background check bound to the worker-lifetime context supplied to NewSharedCheck.
type SharedCheck struct {
	ctx   context.Context
	run   func(context.Context) ProxyAuthResult
	clock func() time.Time

	mu     sync.Mutex
	latest *ProxyAuthResult
	// running is closed when the in-flight check ends; nil while no check runs.
	running chan struct{}
}

// NewSharedCheck creates a single-flight check using workerCtx, run, and clock.
// Background checks inherit workerCtx (or context.Background when nil) so an immediate /healthz response
// does not cancel an in-flight probe, while worker shutdown cancels it cleanly. A nil clock defaults to time.Now.
func NewSharedCheck(workerCtx context.Context, run func(context.Context) ProxyAuthResult, clock func() time.Time) *SharedCheck {
	if workerCtx == nil {
		workerCtx = context.Background()
	}
	if clock == nil {
		clock = time.Now
	}
	return &SharedCheck{ctx: workerCtx, run: run, clock: clock}
}

// Latest returns the most recently completed check immediately, or nil before the first check finishes.
// If the result is absent or stale, it starts at most one background check under the worker context.
func (c *SharedCheck) Latest() *ProxyAuthResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := c.latest != nil && c.clock().UnixMilli()-c.latest.CheckedAt.UnixMilli() < FreshnessTTL.Milliseconds()
	if !fresh && c.running == nil && c.run != nil && c.ctx.Err() == nil {
		done := make(chan struct{})
		c.running = done
		go c.check(done)
	}
	if c.latest == nil {
		return nil
	}
	return new(*c.latest)
}

func (c *SharedCheck) check(done chan struct{}) {
	defer close(done)
	result := c.run(c.ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = nil
	// A check cut short by worker shutdown observed nothing about the proxy.
	if c.ctx.Err() == nil {
		result.CheckedAt = c.clock()
		c.latest = &result
	}
}

// Wait blocks until any in-flight background check completes.
func (c *SharedCheck) Wait() {
	c.mu.Lock()
	done := c.running
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

// HealthInfo carries the worker health fields consulted when computing Codex readiness.
type HealthInfo struct {
	OK                bool
	Version           string
	SupervisorVersion string
}

// CodexChecks records the individual checks behind Codex readiness on /healthz.
type CodexChecks struct {
	ProxyBinary           bool    `json:"proxyBinary"`
	ProxyModels           bool    `json:"proxyModels"`
	CodexAuth             bool    `json:"codexAuth"`
	ShimRunning           bool    `json:"shimRunning"`
	ServingVersion        *string `json:"servingVersion"`
	InstalledVersion      string  `json:"installedVersion"`
	ServingVersionMatches bool    `json:"servingVersionMatches"`
}

// CodexReadiness is the codexReadiness payload returned by /healthz.
type CodexReadiness struct {
	Ready               bool                       `json:"ready"`
	State               string                     `json:"state"`
	Message             string                     `json:"message"`
	Checks              *CodexChecks               `json:"checks"`
	UpstreamBlocked     *upstreamstate.Blocked     `json:"upstreamBlocked"`
	UpstreamUnavailable *upstreamstate.Unavailable `json:"upstreamUnavailable"`
	CheckedAt           *string                    `json:"checkedAt"`
	AgeMs               *int64                     `json:"ageMs"`
	Stale               bool                       `json:"stale"`
}

// CodexReadinessOptions supplies installation metadata and upstream state stores without ambient reads.
type CodexReadinessOptions struct {
	InstalledVersion string
	CommandPath      string
	LogsDir          string
	UpstreamStore    upstreamstate.Store
}

// HealthzCodexReadiness builds the /healthz codexReadiness summary from the latest shared check and worker health.
// Upstream holds are read from opts.UpstreamStore as of now.
func HealthzCodexReadiness(latest *ProxyAuthResult, health HealthInfo, now time.Time, opts CodexReadinessOptions) CodexReadiness {
	store := opts.UpstreamStore
	store.Now = func() time.Time { return now }
	blocked, unavailable := store.ReadBlocked(), store.ReadUnavailable()
	if latest == nil {
		return CodexReadiness{
			State:               "checking",
			Message:             "Codex readiness is still being checked (proxy /v1/models and codex auth status); ask /healthz again in a few seconds.",
			UpstreamBlocked:     blocked,
			UpstreamUnavailable: unavailable,
			Stale:               true,
		}
	}
	shimRunning := health.OK
	servingVersion := health.SupervisorVersion
	if servingVersion == "" {
		servingVersion = health.Version
	}
	checks := &CodexChecks{
		ProxyBinary:      latest.ProxyBinary,
		ProxyModels:      latest.ProxyModels,
		CodexAuth:        latest.CodexAuth,
		ShimRunning:      shimRunning,
		InstalledVersion: opts.InstalledVersion,
	}
	if servingVersion != "" {
		checks.ServingVersion = new(servingVersion)
	}
	checks.ServingVersionMatches = shimRunning && servingVersionIsCurrentOrNewer(checks.ServingVersion, opts.InstalledVersion)
	state := codexState(checks, blocked, unavailable)
	ageMs := max(0, now.UnixMilli()-latest.CheckedAt.UnixMilli())
	return CodexReadiness{
		Ready:               state == "ready",
		State:               state,
		Message:             codexMessage(state, store, opts, blocked, unavailable),
		Checks:              checks,
		UpstreamBlocked:     blocked,
		UpstreamUnavailable: unavailable,
		CheckedAt:           new(isoTimestamp(latest.CheckedAt)),
		AgeMs:               new(ageMs),
		Stale:               ageMs >= FreshnessTTL.Milliseconds(),
	}
}

// codexState orders the Codex checks as the source does (lib/request-worker.js:63-72).
func codexState(checks *CodexChecks, blocked *upstreamstate.Blocked, unavailable *upstreamstate.Unavailable) string {
	switch {
	case !checks.ProxyBinary:
		return "binary-missing"
	case !checks.ProxyModels:
		return "proxy-down"
	case !checks.CodexAuth:
		return "auth-missing"
	case !checks.ShimRunning:
		return "shim-down"
	case !checks.ServingVersionMatches:
		return "serving-version-mismatch"
	case blocked != nil:
		return "upstream-blocked"
	case unavailable != nil:
		return "upstream-unavailable"
	}
	return "ready"
}

// codexMessage explains a Codex state, naming the stable binary with no interpreter (lib/runtime.js:568-587).
func codexMessage(state string, store upstreamstate.Store, opts CodexReadinessOptions, blocked *upstreamstate.Blocked, unavailable *upstreamstate.Unavailable) string {
	command := "`\"" + opts.CommandPath + "\""
	switch state {
	case "ready":
		return "Codex readiness confirms local binary, /v1/models, authentication, shim, and serving-version checks. It does not prove a streaming request will succeed."
	case "binary-missing":
		return "Codex dispatch refused: claude-code-proxy is missing. Run " + command + " setup`, then retry. No Anthropic fallback was used."
	case "auth-missing":
		return "Codex dispatch refused: ChatGPT sign-in is required. Run " + command + " login`, finish browser OAuth, then run " + command + " setup` and retry. Credentials live in `~/.config/claude-code-proxy/`."
	case "proxy-down":
		return "Codex dispatch refused: claude-code-proxy is not answering on /v1/models. The running shim supervisor retries recovery with bounded backoff; check " + filepath.Join(opts.LogsDir, "guardian.log") + " if it does not recover. No Anthropic fallback was used."
	case "shim-down":
		return "Codex dispatch refused: the model-gateway shim is down. Run " + command + " ensure`, then retry. No Anthropic fallback was used."
	case "serving-version-mismatch":
		return "Codex dispatch refused: model-gateway is serving a stale shim version. Run " + command + " ensure`, then retry. No Anthropic fallback was used."
	case "upstream-blocked":
		return store.BlockedReadinessMessage(*blocked)
	case "upstream-unavailable":
		return unavailable.ReadinessMessage()
	}
	return ""
}

// versionPattern finds the first dotted triple, as the source's version comparison does.
var versionPattern = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)`)

// servingVersionIsCurrentOrNewer compares the first dotted triples numerically (lib/request-worker.js:196-205).
func servingVersionIsCurrentOrNewer(serving *string, installed string) bool {
	if serving != nil && *serving == installed {
		return true
	}
	var servingText string
	if serving != nil {
		servingText = *serving
	}
	have := versionPattern.FindStringSubmatch(servingText)
	want := versionPattern.FindStringSubmatch(installed)
	if have == nil || want == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		if have[i] != want[i] {
			// JavaScript's Number of a digit string; float64 parsing rounds the same way.
			a, _ := strconv.ParseFloat(have[i], 64)
			b, _ := strconv.ParseFloat(want[i], 64)
			return a > b
		}
	}
	return true
}

// isoTimestamp is Date.prototype.toISOString for a time within the four-digit year range.
func isoTimestamp(t time.Time) string {
	t = t.UTC()
	if year := t.Year(); year < 0 || year > 9999 {
		return fmt.Sprintf("%+07d%s", year, t.Format("-01-02T15:04:05.000Z"))
	}
	return t.Format("2006-01-02T15:04:05.000Z")
}
