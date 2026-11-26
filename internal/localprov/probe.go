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

package localprov

import (
	"context"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/proxyconfig"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

// DefaultProbeTimeout is the default timeout for live local provider probes.
const DefaultProbeTimeout = 1500 * time.Millisecond

// ProbeOptions configures live HTTP probes against local model servers.
// Zero values use DefaultProbeTimeout and a loopback-safe client that does not follow redirects.
type ProbeOptions struct {
	Client  *http.Client
	Timeout time.Duration
}

// LlamaState records the result of probing the configured llama-server.
// Zero Port or ContextWindow means unset or unknown.
type LlamaState struct {
	Port          uint16
	Configured    bool
	Reachable     bool
	ContextWindow int64
	Error         string
}

// ProbeLlama probes http://127.0.0.1:<CCP_LLAMA_PORT>/props and reports every failure as state.
// Connection-refused transport errors report "ECONNREFUSED" in Error.
func ProbeLlama(ctx context.Context, lookup config.LookupEnv, opts ProbeOptions) LlamaState {
	_, _, _ = ctx, lookup, opts
	return LlamaState{}
}

// LlamaTracker retains the most recent llama-server probe across listing refreshes.
// Its zero value is safe for concurrent use.
type LlamaTracker struct{}

// Refresh probes llama-server when routes contain a local provider, preserving the last measured
// window during an outage on the same port and clearing state when no local route is listed.
func (t *LlamaTracker) Refresh(ctx context.Context, routes []proxymodels.Route, lookup config.LookupEnv, opts ProbeOptions) *LlamaState {
	_, _, _, _ = ctx, routes, lookup, opts
	return nil
}

// State returns a copy of the latest tracked llama-server state, or nil when no local route is active.
func (t *LlamaTracker) State() *LlamaState {
	return nil
}

// StudioState records the result of probing the configured LM Studio server.
// API keys are never stored in this state.
type StudioState struct {
	Enabled      bool
	BaseURL      string
	HasAPIKey    bool
	Reachable    bool
	Models       []Model
	Invalid      *proxyconfig.InvalidSetting
	AuthRequired bool
	Error        string
}

// Windows returns a fresh map of model key to positive measured context window.
func (s StudioState) Windows() map[string]int64 {
	return nil
}

// ProbeStudio probes <baseUrl>/api/v1/models for the LM Studio settings resolved from lookup and configDir.
// configDir must be a clean, absolute path when non-empty. Connection-refused transport errors report
// "ECONNREFUSED" in Error.
func ProbeStudio(ctx context.Context, lookup config.LookupEnv, configDir string, opts ProbeOptions) StudioState {
	_, _, _, _ = ctx, lookup, configDir, opts
	return StudioState{}
}

// StudioTracker retains the most recent LM Studio probe across listing refreshes.
// Its zero value is safe for concurrent use.
type StudioTracker struct{}

// Refresh probes LM Studio when routes contain an lmstudio provider, preserving the last measured
// models during an outage at the same base URL and clearing state when no lmstudio route is listed.
func (t *StudioTracker) Refresh(ctx context.Context, routes []proxymodels.Route, lookup config.LookupEnv, configDir string, opts ProbeOptions) *StudioState {
	_, _, _, _, _ = ctx, routes, lookup, configDir, opts
	return nil
}

// State returns a deep copy of the latest tracked LM Studio state, or nil when no lmstudio route is active.
func (t *StudioTracker) State() *StudioState {
	return nil
}
