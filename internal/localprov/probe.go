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
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/proxyconfig"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

// DefaultProbeTimeout is the default timeout for live local provider probes.
const DefaultProbeTimeout = 1500 * time.Millisecond

const (
	llamaProvider  = "local"
	studioProvider = "lmstudio"

	// maxRedirects bounds the redirect chain a probe follows; the source follows any number.
	maxRedirects = 10
	// maxProbeBody bounds the bytes read from one probe response.
	maxProbeBody = 16 << 20
)

// probeTransport is the dedicated transport of probes that are not given a client. It never consults
// the environment's proxy settings and keeps no idle connections, so it outlives no probe.
var probeTransport = &http.Transport{
	Proxy:             nil,
	DialContext:       (&net.Dialer{}).DialContext,
	DisableKeepAlives: true,
}

// ProbeOptions configures live HTTP probes against local model servers.
// Zero values use DefaultProbeTimeout and a client that follows HTTP 3xx redirects
// while retaining the Authorization header only on the same origin.
type ProbeOptions struct {
	Client  *http.Client
	Timeout time.Duration
}

// probeResponse is one completed probe: the final status and the bounded body.
type probeResponse struct {
	status int
	body   []byte
}

// get issues one GET through the configured client. The redirect policy is always the probe's own,
// whichever client carries the request. A transport failure is returned as a classified, payload-free
// code such as "ECONNREFUSED".
func (o ProbeOptions) get(ctx context.Context, target, authorization string) (probeResponse, string) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &http.Client{Transport: probeTransport}
	if o.Client != nil {
		copied := *o.Client
		client = &copied
		client.Timeout = 0
	}
	client.CheckRedirect = followProbeRedirect

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return probeResponse{}, "invalid request"
	}
	req.Header.Set("User-Agent", "model-gateway")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := client.Do(req)
	if err != nil {
		return probeResponse{}, transportFailure(ctx, err, target)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return probeResponse{}, transportFailure(ctx, err, target)
	}
	return probeResponse{status: resp.StatusCode, body: body}, ""
}

// followProbeRedirect follows redirects as the source's fetch helper does: the Authorization header
// survives only while every hop stays on the origin of the first request.
func followProbeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return errors.New("too many redirects")
	}
	first := origin(via[0].URL)
	crossed := origin(req.URL) != first
	for _, hop := range via[1:] {
		crossed = crossed || origin(hop.URL) != first
	}
	if crossed {
		req.Header.Del("Authorization")
	}
	return nil
}

// origin is the scheme, lowercase host and effective port of u.
func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// transportFailure names a failed probe without the transport's own text, which can carry
// addresses or response fragments. Known socket errors keep their errno name as the source reports
// them; an expired probe deadline reports the source's timeout wording.
func transportFailure(ctx context.Context, err error, target string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout: " + target
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		if name, ok := errnoNames[errno]; ok {
			return name
		}
	}
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
		if dnsErr.IsNotFound {
			return "ENOTFOUND"
		}
		return "EAI_AGAIN"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "request failed"
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
	port := config.LlamaServerPort.Get(lookup)
	if !port.Configured {
		return LlamaState{}
	}
	if port.Port == 0 {
		return LlamaState{Configured: true, Error: "CCP_LLAMA_PORT=" + port.Invalid + " is not a port"}
	}
	state := LlamaState{Port: port.Port, Configured: true}
	resp, failure := opts.get(ctx, "http://127.0.0.1:"+strconv.Itoa(int(port.Port))+"/props", "")
	switch {
	case failure != "":
		state.Error = failure
	case resp.status != http.StatusOK:
		state.Error = "GET /props answered HTTP " + strconv.Itoa(resp.status)
	default:
		state.Reachable = true
		state.ContextWindow = LlamaWindow(resp.body)
	}
	return state
}

// LlamaTracker retains the most recent llama-server probe across listing refreshes.
// Its zero value is safe for concurrent use.
type LlamaTracker struct {
	mu    sync.Mutex
	state *LlamaState
}

// Refresh probes llama-server when routes contain a local provider, preserving the last measured
// window during an outage on the same port and clearing state when no local route is listed.
func (t *LlamaTracker) Refresh(ctx context.Context, routes []proxymodels.Route, lookup config.LookupEnv, opts ProbeOptions) *LlamaState {
	if !listsProvider(routes, llamaProvider) {
		t.mu.Lock()
		t.state = nil
		t.mu.Unlock()
		return nil
	}
	previous := t.State()
	// The lock is not held across the probe, so readers never wait on the network.
	state := ProbeLlama(ctx, lookup, opts)
	if !state.Reachable && state.Port != 0 && previous != nil && previous.Port == state.Port && previous.ContextWindow != 0 {
		state.ContextWindow = previous.ContextWindow
	}
	t.mu.Lock()
	t.state = &state
	t.mu.Unlock()
	return new(state)
}

// State returns a copy of the latest tracked llama-server state, or nil when no local route is active.
func (t *LlamaTracker) State() *LlamaState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		return nil
	}
	return new(*t.state)
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
	windows := make(map[string]int64, len(s.Models))
	for _, m := range s.Models {
		if m.ContextWindow > 0 {
			windows[m.Key] = m.ContextWindow
		}
	}
	return windows
}

// clone returns a deep copy that shares no mutable state with s.
func (s StudioState) clone() *StudioState {
	s.Models = slices.Clone(s.Models)
	if s.Invalid != nil {
		s.Invalid = new(*s.Invalid)
	}
	return &s
}

// ProbeStudio probes <baseUrl>/api/v1/models for the LM Studio settings resolved from lookup and configDir.
// configDir must be a clean, absolute path when non-empty. Connection-refused transport errors report
// "ECONNREFUSED" in Error.
func ProbeStudio(ctx context.Context, lookup config.LookupEnv, configDir string, opts ProbeOptions) StudioState {
	settings := proxyconfig.ReadStudio(lookup, configDir)
	if !settings.Enabled {
		return StudioState{}
	}
	if settings.Invalid != nil {
		return StudioState{Enabled: true, Invalid: new(*settings.Invalid)}
	}
	state := StudioState{Enabled: true, BaseURL: settings.BaseURL, HasAPIKey: settings.APIKey != ""}
	authorization := ""
	if settings.APIKey != "" {
		authorization = "Bearer " + settings.APIKey
	}
	resp, failure := opts.get(ctx, settings.BaseURL+"/api/v1/models", authorization)
	switch {
	case failure != "":
		state.Error = failure
	case resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden:
		state.Reachable = true
		state.AuthRequired = true
	case resp.status != http.StatusOK:
		state.Error = "GET /api/v1/models answered HTTP " + strconv.Itoa(resp.status)
	default:
		models, err := StudioModels(resp.body)
		if err != nil {
			state.Error = "GET /api/v1/models returned no model list"
			break
		}
		state.Reachable = true
		state.Models = models
	}
	return state
}

// StudioTracker retains the most recent LM Studio probe across listing refreshes.
// Its zero value is safe for concurrent use.
type StudioTracker struct {
	mu    sync.Mutex
	state *StudioState
}

// Refresh probes LM Studio when routes contain an lmstudio provider, preserving the last measured
// models during an outage at the same base URL and clearing state when no lmstudio route is listed.
func (t *StudioTracker) Refresh(ctx context.Context, routes []proxymodels.Route, lookup config.LookupEnv, configDir string, opts ProbeOptions) *StudioState {
	if !listsProvider(routes, studioProvider) {
		t.mu.Lock()
		t.state = nil
		t.mu.Unlock()
		return nil
	}
	previous := t.State()
	// The lock is not held across the probe, so readers never wait on the network.
	state := ProbeStudio(ctx, lookup, configDir, opts)
	if !state.Reachable && state.BaseURL != "" && previous != nil && previous.BaseURL == state.BaseURL {
		state.Models = previous.Models
	}
	t.mu.Lock()
	t.state = state.clone()
	t.mu.Unlock()
	return state.clone()
}

// State returns a deep copy of the latest tracked LM Studio state, or nil when no lmstudio route is active.
func (t *StudioTracker) State() *StudioState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		return nil
	}
	return t.state.clone()
}

func listsProvider(routes []proxymodels.Route, provider string) bool {
	return slices.ContainsFunc(routes, func(r proxymodels.Route) bool { return r.Provider == provider })
}
