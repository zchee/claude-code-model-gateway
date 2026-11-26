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
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/proxyconfig"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

const (
	localProvider  = "local"
	studioProvider = "lmstudio"
	// minSentryWindow is the smallest local window that keeps a compaction sentry (lib/runtime.js:414).
	minSentryWindow = 32768
)

// StatusRunner executes `<binary> <provider> auth status` and returns its exit code.
// A non-nil error indicates the command failed to run or timed out without an exit status.
// The context it receives ends after the source's fifteen-second command timeout.
type StatusRunner func(ctx context.Context, binary string, args []string) (exitCode int, err error)

// ProviderAuthOptions supplies injected configuration, subprocess execution, and local server states.
type ProviderAuthOptions struct {
	Lookup      config.LookupEnv
	ConfigDir   string
	ProxyBinary string
	RunStatus   StatusRunner
	LlamaState  *localprov.LlamaState
	StudioState *localprov.StudioState
}

// ProviderAuthState returns a proxy provider's auth or local-server state without ambient reads.
// Returned states are "ready", "auth-missing", "auth-unknown", "config-invalid", or "backend-down".
func ProviderAuthState(ctx context.Context, provider string, opts ProviderAuthOptions) string {
	switch provider {
	case "opencode":
		if config.OpenCodeProxyKey.Get(opts.Lookup) != "" || config.OpenCodeKey.Get(opts.Lookup) != "" {
			return "ready"
		}
		if key, ok := configString(opts.ConfigDir, "opencode", "apiKey"); ok && key != "" {
			return "ready"
		}
		return "auth-missing"
	case localProvider:
		return llamaReadinessState(opts.Lookup, opts.LlamaState)
	case studioProvider:
		return studioReadinessState(proxyconfig.ReadStudio(opts.Lookup, opts.ConfigDir), opts.StudioState)
	}
	// Without a project the status exits 2 whatever its token attempt returns, and the attempt can wait on a
	// metadata server that only exists on Google Cloud, so that answer is given without starting a process.
	if provider == "vertex" && !vertexProjectIsSet(opts.Lookup, opts.ConfigDir) {
		return "config-invalid"
	}
	if opts.RunStatus == nil {
		return "auth-unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	code, err := opts.RunStatus(ctx, opts.ProxyBinary, []string{provider, "auth", "status"})
	switch {
	case err != nil:
		return "auth-unknown"
	case code == 0:
		return "ready"
	case code == 1:
		return "auth-missing"
	case provider == "vertex" && code == 2:
		return "config-invalid"
	default:
		return "auth-unknown"
	}
}

// vertexProjectIsSet mirrors the proxy's project sources and blank rule (lib/commands.js:1674-1677).
func vertexProjectIsSet(lookup config.LookupEnv, configDir string) bool {
	if config.VertexProject.Get(lookup) != "" || config.GoogleCloudProject.Get(lookup) != "" {
		return true
	}
	project, ok := configString(configDir, "vertex", "project")
	return ok && config.VertexProject.Get(func(string) (string, bool) { return project, true }) != ""
}

// configString reads a string member of a section of the proxy's config.json the way JSON.parse
// presents it to the source: the last duplicate member wins, and a missing or malformed file is no config.
func configString(configDir, section, name string) (string, bool) {
	if !filepath.IsAbs(configDir) || filepath.Clean(configDir) != configDir {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		return "", false
	}
	doc, err := jsonx.DecodeWire(data)
	if err != nil {
		return "", false
	}
	value := lastMember(lastMember(doc, section), name)
	if value.Kind != '"' {
		return "", false
	}
	return string(utf16.Decode(value.Text)), true
}

func lastMember(value jsonx.WireValue, name string) jsonx.WireValue {
	key := utf16.Encode([]rune(name))
	for _, m := range slices.Backward(value.Members) {
		if slices.Equal(m.Name, key) {
			return m.Value
		}
	}
	return jsonx.WireValue{}
}

// llamaReadinessState is the local provider's state: whether the llama-server named by CCP_LLAMA_PORT
// answered the last probe of that port (lib/commands.js:1710-1715).
func llamaReadinessState(lookup config.LookupEnv, state *localprov.LlamaState) string {
	port := config.LlamaServerPort.Get(lookup)
	switch {
	case port.Port == 0:
		return "config-invalid"
	case state == nil || state.Port != port.Port:
		return "auth-unknown"
	case state.Reachable:
		return "ready"
	default:
		return "backend-down"
	}
}

// studioReadinessState is LM Studio's state: what its own listing answered on the last probe of the
// address this check's settings name (lib/commands.js:1741-1746).
func studioReadinessState(settings proxyconfig.StudioSettings, state *localprov.StudioState) string {
	switch {
	case !settings.Enabled || settings.Invalid != nil:
		return "config-invalid"
	case state == nil || state.BaseURL != settings.BaseURL:
		return "auth-unknown"
	case state.AuthRequired:
		return "auth-missing"
	case state.Reachable:
		return "ready"
	default:
		return "backend-down"
	}
}

// SharedChecks holds the gateway checks shared by every proxy provider.
type SharedChecks struct {
	ProxyBinary           bool
	ProxyModels           bool
	ShimRunning           bool
	ServingVersionMatches bool
}

// ProviderReadiness summarizes readiness for a single proxy provider.
type ProviderReadiness struct {
	Ready   bool   `json:"ready"`
	State   string `json:"state"`
	Message string `json:"message"`
}

// ProviderMessageOptions supplies environment, paths, and local server states for readiness messages.
type ProviderMessageOptions struct {
	Lookup      config.LookupEnv
	ConfigDir   string
	ProxyBinary string
	CommandPath string
	LlamaState  *localprov.LlamaState
	StudioState *localprov.StudioState
}

// ProxyProviderReadiness combines shared proxy checks with the provider's own auth or server state.
// authState is invoked only when every shared check passes.
func ProxyProviderReadiness(provider string, checks SharedChecks, authState func(provider string) string, opts ProviderMessageOptions) ProviderReadiness {
	state := sharedFailure(checks)
	if state == "" {
		state = authState(provider)
	}
	label := proxymodels.ProviderLabel(provider)
	if state == "ready" {
		return ProviderReadiness{
			Ready: true,
			State: state,
			Message: label + " readiness confirms local binary, /v1/models, " + credentialCheck(provider) +
				", shim, and serving-version checks. It does not prove a streaming request will succeed.",
		}
	}
	return ProviderReadiness{State: state, Message: opts.reason(state, provider, label)}
}

// sharedFailure names the first failed shared check, or returns "" when all pass.
func sharedFailure(checks SharedChecks) string {
	switch {
	case !checks.ProxyBinary:
		return "binary-missing"
	case !checks.ProxyModels:
		return "proxy-down"
	case !checks.ShimRunning:
		return "shim-down"
	case !checks.ServingVersionMatches:
		return "serving-version-mismatch"
	}
	return ""
}

// credentialCheck is what a provider's ready state proved about its credentials.
func credentialCheck(provider string) string {
	switch provider {
	case "opencode":
		return "API key presence"
	case localProvider:
		return "llama-server reachability"
	case studioProvider:
		return "LM Studio server reachability"
	case "vertex":
		return "Google credentials and Vertex settings"
	}
	return "authentication"
}

// DescribeProviderAuth formats the doctor summary line for a proxy provider's auth or server state.
func DescribeProviderAuth(provider, state string, opts ProviderMessageOptions) string {
	if state == "ready" {
		switch provider {
		case localProvider:
			return describeLlama(opts.LlamaState)
		case studioProvider:
			return describeStudio(opts.StudioState)
		case "opencode":
			return "API key present"
		}
		return "authenticated"
	}
	reason := opts.reason(state, provider, proxymodels.ProviderLabel(provider))
	switch state {
	case "backend-down":
		return "NOT RUNNING (" + reason + ")"
	case "auth-missing":
		return "MISSING (" + reason + ")"
	case "config-invalid":
		return "NOT CONFIGURED (" + reason + ")"
	}
	return "status unavailable"
}

// SharedFailureMessage formats the refusal message for a failed shared proxy check.
func SharedFailureMessage(state string, opts ProviderMessageOptions) string {
	return opts.proxyReason(state, "", "")
}

// reason explains a provider state. The local engines have their own wording for the states they
// share with sign-in providers (lib/commands.js:1781-1785).
func (o ProviderMessageOptions) reason(state, provider, label string) string {
	switch provider {
	case localProvider:
		if text, ok := o.llamaReason(state); ok {
			return text
		}
	case studioProvider:
		if text, ok := o.studioReason(state); ok {
			return text
		}
	}
	return o.proxyReason(state, provider, label)
}

func (o ProviderMessageOptions) configFile() string {
	return filepath.Join(o.ConfigDir, "config.json")
}

// restartHint names the stable binary, with no interpreter, for a gateway restart.
func (o ProviderMessageOptions) restartHint() string {
	return "`\"" + o.CommandPath + "\" stop`, then `\"" + o.CommandPath + "\" start`"
}

func (o ProviderMessageOptions) proxyReason(state, provider, label string) string {
	switch state {
	case "binary-missing":
		return "claude-code-proxy is missing. Run `\"" + o.CommandPath + "\" setup`."
	case "proxy-down":
		return "claude-code-proxy is not answering on /v1/models."
	case "shim-down":
		return "the model-gateway shim is down."
	case "serving-version-mismatch":
		return "model-gateway is serving a stale shim version."
	case "auth-missing":
		switch provider {
		case "opencode":
			return "OpenCode Go needs an API key: set CCP_OPENCODE_API_KEY or OPENCODE_API_KEY where the gateway starts, or opencode.apiKey in " + o.configFile() + "."
		case "vertex":
			return "claude-code-proxy got no Google access token for Vertex AI: Application Default Credentials are missing or expired, or Google could not be reached. Run `gcloud auth application-default login`, or set GOOGLE_APPLICATION_CREDENTIALS to a credentials file where the gateway starts; `\"" + o.ProxyBinary + "\" vertex auth status` prints the reason."
		}
		return "claude-code-proxy has no " + label + " sign-in. Run `\"" + o.ProxyBinary + "\" " + provider + " auth login`."
	case "auth-unknown":
		return "claude-code-proxy could not report " + label + " sign-in status."
	case "config-invalid":
		return "claude-code-proxy reports a missing or invalid " + label + " setting, most often an unset Google Cloud project: set vertex.project in " + o.configFile() + ", or CCP_VERTEX_PROJECT or GOOGLE_CLOUD_PROJECT where the gateway starts. Run `\"" + o.ProxyBinary + "\" " + provider + " auth status` to see which setting."
	}
	return ""
}

// llamaReason explains the local provider's own states (lib/commands.js:1723-1737).
func (o ProviderMessageOptions) llamaReason(state string) (string, bool) {
	switch state {
	case "config-invalid":
		port := config.LlamaServerPort.Get(o.Lookup)
		problem := "CCP_LLAMA_PORT is not set"
		if port.Configured && port.Port == 0 {
			problem = "CCP_LLAMA_PORT=" + jsString(port.Invalid) + " is not a port"
		}
		return "claude-code-proxy lists local/gguf, but " + problem + " where the gateway runs, so the gateway cannot reach the llama-server behind it. Set CCP_LLAMA_PORT to llama-server's --port where the gateway starts (the proxy inherits the gateway's environment), then restart the gateway: " + o.restartHint() + ".", true
	case "backend-down":
		port := "null"
		if o.LlamaState != nil && o.LlamaState.Port != 0 {
			port = strconv.Itoa(int(o.LlamaState.Port))
		} else if p := config.LlamaServerPort.Get(o.Lookup).Port; p != 0 {
			port = strconv.Itoa(int(p))
		}
		detail := ""
		if o.LlamaState != nil && o.LlamaState.Error != "" {
			detail = " (" + o.LlamaState.Error + ")"
		}
		return "llama-server is not answering on 127.0.0.1:" + port + detail + ". Start it with `llama-server -m <model>.gguf --jinja --host 127.0.0.1 --port " + port + "`.", true
	case "auth-unknown":
		return "the llama-server behind local/gguf has not been probed in this process yet.", true
	}
	return "", false
}

// studioPlaces names where the proxy reads an LM Studio setting from (lib/commands.js:1749-1751).
func (o ProviderMessageOptions) studioPlaces(configKey string) string {
	return "where the gateway starts (the proxy inherits the gateway's environment), or " + configKey + " in " + o.configFile()
}

// studioReason explains LM Studio's own states (lib/commands.js:1753-1772).
func (o ProviderMessageOptions) studioReason(state string) (string, bool) {
	baseURL := "undefined"
	if o.StudioState != nil {
		baseURL = cmp.Or(o.StudioState.BaseURL, "null")
	}
	switch state {
	case "config-invalid":
		var problem string
		if invalid := proxyconfig.ReadStudio(o.Lookup, o.ConfigDir).Invalid; invalid != nil {
			problem = "the LM Studio setting " + invalid.Setting + " is invalid where this check runs (" + invalid.Reason + "), and the proxy fails every LM Studio request while it is. Correct CCP_LMSTUDIO_BASE_URL " + o.studioPlaces("lmstudio.baseUrl")
		} else {
			problem = "the provider is turned off where this check runs, so its settings differ from the running gateway's. Use the same CCP_LMSTUDIO_ENABLED " + o.studioPlaces("lmstudio.enabled")
		}
		return "claude-code-proxy lists LM Studio models, but " + problem + ", then restart the gateway: " + o.restartHint() + ".", true
	case "backend-down":
		detail := ""
		if o.StudioState != nil && o.StudioState.Error != "" {
			detail = " (" + o.StudioState.Error + ")"
		}
		return "LM Studio's server is not answering at " + baseURL + detail + ". Start it with `lms server start` or from the LM Studio app's Developer page; LM Studio 0.4.1 or later is required.", true
	case "auth-missing":
		fix := "requires an API token: set"
		if o.StudioState != nil && o.StudioState.HasAPIKey {
			fix = "refused the API token it was sent: check"
		}
		return "LM Studio at " + baseURL + " " + fix + " CCP_LMSTUDIO_API_KEY (or LM_API_TOKEN) " + o.studioPlaces("lmstudio.apiKey") + "; the token comes from LM Studio's server settings. Then restart the gateway: " + o.restartHint() + ".", true
	case "auth-unknown":
		return "the LM Studio server has not been probed in this process yet.", true
	}
	return "", false
}

// describeLlama is doctor's line for an answering llama-server (lib/commands.js:1813-1820).
func describeLlama(state *localprov.LlamaState) string {
	port, window := "undefined", int64(0)
	if state != nil {
		port, window = "null", state.ContextWindow
		if state.Port != 0 {
			port = strconv.Itoa(int(state.Port))
		}
	}
	context := "context unknown (/props reported no n_ctx, as in router mode), so the row is an unmeasured 200k row"
	small := ""
	if window != 0 {
		context = fmt.Sprintf("context %d tokens", window)
		if window < minSentryWindow {
			small = fmt.Sprintf("; WARNING: below %d, so there is no compaction sentry and Claude Code's own first request (about 13k tokens) leaves little room: restart llama-server with -c %d or more", minSentryWindow, minSentryWindow)
		}
	}
	return "llama-server answering on 127.0.0.1:" + port + ", " + context + small
}

// describeStudio is doctor's line for an answering LM Studio server (lib/commands.js:1824-1837).
func describeStudio(state *localprov.StudioState) string {
	baseURL := "undefined"
	var models []localprov.Model
	if state != nil {
		baseURL, models = cmp.Or(state.BaseURL, "null"), state.Models
	}
	var contexts, small []string
	loaded := 0
	for _, m := range models {
		if !m.Loaded {
			continue
		}
		loaded++
		window := "unknown"
		if m.ContextWindow != 0 {
			window = fmt.Sprintf("%d tokens", m.ContextWindow)
			if m.ContextWindow < minSentryWindow {
				small = append(small, m.Key)
			}
		}
		contexts = append(contexts, m.Key+" context "+window)
	}
	plural := "s"
	if len(models) == 1 {
		plural = ""
	}
	first := fmt.Sprintf("LM Studio answering at %s, %d model%s, %d loaded", baseURL, len(models), plural, loaded)
	if len(contexts) > 0 {
		first += " (" + strings.Join(contexts, ", ") + ")"
	}
	parts := []string{first}
	if len(small) > 0 {
		parts = append(parts, fmt.Sprintf("WARNING: %s loaded below %d, so there is no compaction sentry and Claude Code's own first request (about 13k tokens) leaves little room: `lms unload <model key>`, then `lms load <model key> --context-length %d` or more", strings.Join(small, ", "), minSentryWindow, minSentryWindow))
	}
	if loaded < len(models) {
		parts = append(parts, "a model that is not loaded is an unmeasured 200k row with no compaction sentry until LM Studio loads it, which it does on the first request with that model's default context: load it first with `lms load <model key> --context-length <tokens>`")
	}
	return strings.Join(parts, "; ")
}

// jsString is JSON.stringify of a string, as the source quotes a rejected setting.
func jsString(s string) string {
	data, err := jsonx.EncodeWire(jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(s))})
	if err != nil {
		return strconv.Quote(s)
	}
	return string(data)
}
