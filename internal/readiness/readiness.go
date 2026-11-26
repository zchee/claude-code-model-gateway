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
	"net/http"
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

// ProbeProxyAndAuth checks binary presence and, when present, probes /v1/models and codex auth status in parallel.
func ProbeProxyAndAuth(ctx context.Context, probe ProxyAuthProbe) ProxyAuthResult {
	_, _ = ctx, probe
	return ProxyAuthResult{}
}

// SharedCheck deduplicates concurrent proxy and auth checks and serves the latest result immediately.
// When no check has completed or the latest result is at least FreshnessTTL old, Latest starts at most
// one background check bound to the worker-lifetime context supplied to NewSharedCheck.
type SharedCheck struct{}

// NewSharedCheck creates a single-flight check using workerCtx, run, and clock.
// Background checks inherit workerCtx (or context.Background when nil) so an immediate /healthz response
// does not cancel an in-flight probe, while worker shutdown cancels it cleanly. A nil clock defaults to time.Now.
func NewSharedCheck(workerCtx context.Context, run func(context.Context) ProxyAuthResult, clock func() time.Time) *SharedCheck {
	_, _, _ = workerCtx, run, clock
	return &SharedCheck{}
}

// Latest returns the most recently completed check immediately, or nil before the first check finishes.
// If the result is absent or stale, it starts at most one background check under the worker context.
func (c *SharedCheck) Latest() *ProxyAuthResult {
	return nil
}

// Wait blocks until any in-flight background check completes.
func (c *SharedCheck) Wait() {}

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
func HealthzCodexReadiness(latest *ProxyAuthResult, health HealthInfo, now time.Time, opts CodexReadinessOptions) CodexReadiness {
	_, _, _, _ = latest, health, now, opts
	return CodexReadiness{}
}
