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

package config

import (
	"cmp"
	"slices"
	"strings"
)

const publicPortDefault = "18764"

var (
	AnthropicBaseURL        = rawKey("ANTHROPIC_BASE_URL", "Effective Claude base URL before settings and probe environment handling.", "Presence counts even if empty; the pin and picker probes remove the inherited key.", "lib/settings-wiring.js:40–42; lib/recovery-policy.js:22–24; lib/commands.js:1473; lib/request-worker.js:1219; lib/pins.js:241–257,472–475; lib/picker-guard.js:90–99")
	AppData                 = candidate("APPDATA", "home/AppData/Roaming", "Windows proxy config parent directory.", "Use any nonempty string without trimming and append claude-code-proxy; ignored outside Windows.", "lib/proxy-config.js:12–16")
	ClaudeAutoCompactWindow = key("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "project-local, project-shared, then user settings", "First candidate for Claude's automatic compaction window.", "Apply Number; accept positive integers only.", "lib/commands.js:1249–1262", func(s string, set bool) *float64 {
		if n := envNumber(s, set); integerPositive(n) {
			return &n
		}
		return nil
	})
	ClaudeBin      = key("CODEX_GATEWAY_CLAUDE_BIN", "claude", "Claude CLI for probes; a .cmd or .bat uses the Windows shell.", `Use a nonempty command without trimming. The source's test-mode probe checks the raw value: any nonempty override is truthy, even "claude".`, "lib/runtime.js:549–550; lib/pins.js:212–214,251–262; lib/picker-guard.js:32–35,94", func(s string, _ bool) string { return cmp.Or(s, "claude") })
	ClaudeConfig   = candidate("CLAUDE_CONFIG_DIR", "home/.claude (global config: home/.claude.json)", "Claude config, registry, discovery cache and transcripts.", "Use any nonempty string without trimming; only the registry search makes it absolute.", "lib/runtime.js:15,552,682–693; lib/previous-picker-rows.js:13–17; lib/usage-observability.js:134; lib/commands.js:320; hooks/registry-writer.js:43–51")
	CompactGuard   = toggle("CODEX_GATEWAY_COMPACT_STREAM_GUARD", "Buffer Codex compaction streams for retry before committing the response.", "lib/request-worker.js:305", true)
	CompactMax     = numberKey("CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES", "Maximum buffered compaction response bytes.", "accept a finite positive value.", "lib/request-worker.js:314–317", 16*1024*1024, finitePositive)
	CompactRetries = numberKey("CODEX_GATEWAY_COMPACT_STREAM_RETRIES", "Number of retries for a failed compaction stream.", "accept a nonnegative integer.", "lib/request-worker.js:306–309", 2, integerNonnegative)
	CompactDelay   = numberKey("CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS", "Delay between compaction stream retries in milliseconds.", "accept a finite nonnegative value.", "lib/request-worker.js:310–313", 250, finiteNonnegative)
	CompactTrigger = key("CODEX_GATEWAY_COMPACT_TRIGGER", "no override", "Global synthetic compaction trigger candidate, unless compact-at is saved.", "Apply Number; only finite positive values become candidates. A saved-setting diagnostic prints the raw spelling, including empty, and omits it only when unset.", "lib/runtime.js:320,339,358; lib/context-window.js:55–64", func(s string, set bool) float64 {
		return envNumber(s, set)
	})
	CodexContextWindow = key("CODEX_GATEWAY_CONTEXT_WINDOW", "saved setting takes precedence; default 272000", "Legacy Codex context window cap.", `Accept the exact string "full", or apply Number and accept integers from 185000 to 1000000 inclusive.`, "lib/runtime.js:247,251–279,307–320", func(s string, set bool) ContextWindow {
		if s == "full" {
			return ContextWindow{Full: true}
		}
		if n := envNumber(s, set); integer(n) && n >= 185000 && n <= 1000000 {
			return ContextWindow{Tokens: int(n)}
		}
		return ContextWindow{}
	})
	DispatchCachePath  = candidate("CODEX_GATEWAY_DISPATCH_CACHE_PATH", "state/dispatch-routes.json", "Dispatch route cache file.", "Use any nonempty string without trimming.", "lib/runtime.js:38; lib/commands.js:128")
	DispatchMax        = numberKey("CODEX_GATEWAY_DISPATCH_CACHE_MAX_SESSIONS", "Maximum session identities in the dispatch cache and usage context snapshots.", "accept a positive integer.", "lib/request-worker.js:719–722; lib/usage-observability.js:993–996", 500, integerPositive)
	DispatchTTL        = numberKey("CODEX_GATEWAY_DISPATCH_CACHE_TTL_MS", "Dispatch cache and usage context snapshot TTL in milliseconds.", "accept a finite positive value.", "lib/request-worker.js:715–718; lib/usage-observability.js:989–992", 4*60*60*1000, finitePositive)
	DrainTimeout       = numberKey("CODEX_GATEWAY_DRAIN_TIMEOUT_MS", "Milliseconds allowed for a graceful drain; ensure waits at least 12000 ms and adds 12000 ms to this timeout.", "fall back only for zero or NaN.", "lib/commands.js:568,2490; lib/process-supervision.js:918,945", 30000, truthyNumber)
	GitHubToken        = trimmedKey("GITHUB_TOKEN", "gh auth token, then unset", "Second choice for GitHub release API authentication.", "lib/github-release.js:8–27")
	GHToken            = trimmedKey("GH_TOKEN", "GITHUB_TOKEN, then gh auth token", "First choice for GitHub release API authentication.", "lib/github-release.js:8–27")
	GoogleCloudProject = trimmedKey("GOOGLE_CLOUD_PROJECT", "vertex.project in proxy config", "Second Vertex project presence check.", "lib/commands.js:1674–1677")
	GrokEndpoint       = stringKey("CODEX_GATEWAY_GROK_ENDPOINT", "https://cli-chat-proxy.grok.com/v1/responses", "Grok backend endpoint.", "lib/runtime.js:31; lib/commands.js:113; lib/grok-backend.js:8")
	GrokHome           = candidate("CODEX_GATEWAY_GROK_HOME", "home/.grok", "Grok CLI state directory.", "Use any nonempty string without trimming.", "lib/grok-backend.js:18–20")
	Home               = key("HOME", "injected account home lookup if unset", "POSIX user home and the test-runner settings diagnostic.", "Use the value as provided when nonempty. Unlike Node, which keeps an explicitly empty HOME and derives relative paths, the Go port reports an error naming HOME and does not look up the account's home.", "lib/runtime.js:15–16 (os.homedir); lib/settings-wiring.js:104; Node v24.11.1 deps/uv/src/unix/core.c:1195–1209", presentString)
	HostsFile          = candidate("CODEX_GATEWAY_HOSTS_FILE", "Windows: SystemRoot/System32/drivers/etc/hosts; otherwise /etc/hosts", "Hosts file that doctor and the migration check read to detect a block written by the Node gateway. The Go gateway never writes this file.", "Use any nonempty string without trimming.", "internal/paths/paths.go (HostsFile)")
	KeepPlanTools      = toggle("CODEX_GATEWAY_KEEP_PLAN_TOOLS", "Keep plan tools visible in adapted provider requests.", "lib/request-worker.js:2547", false)
	ListDispatchModel  = toggle("CODEX_GATEWAY_LIST_DISPATCH_MODEL", "List the virtual auto dispatch model.", "lib/runtime.js:39; lib/commands.js:129", false)
	LlamaServerPort    = key("CCP_LLAMA_PORT", "unset", "Local llama-server port.", "Distinguish unset from invalid; accept ASCII decimal digits with an optional leading plus and a value from 1 to 65535.", "lib/local-llama.js:13,17–22; lib/commands.js:1707", parseLlamaPort)
	LMStudioAPIKey     = trimmedKey("CCP_LMSTUDIO_API_KEY", "LM_API_TOKEN, then lmstudio.apiKey in proxy config", "First LM Studio bearer token candidate.", "lib/lmstudio.js:19,24–26,84")
	LMStudioBaseURL    = key("CCP_LMSTUDIO_BASE_URL", "lmstudio.baseUrl in proxy config, then http://127.0.0.1:1234", "LM Studio server origin.", "Trim ECMAScript whitespace; blank is unset. Otherwise require an HTTPS origin or a loopback HTTP origin, without credentials, a path, query or fragment. A Unicode or punycode host is an invalid override, not a fallback."+urlLimits, "lib/lmstudio.js:16,18,24–26,35–54,78–84", func(s string, _ bool) LMOrigin { return parseLMStudioOrigin(jsTrim(s)) })
	LMStudioEnabled    = key("CCP_LMSTUDIO_ENABLED", "lmstudio.enabled in proxy config, then true", "Enable the LM Studio provider.", "Trim and lowercase. 1, true, yes and on enable; 0, false, no and off disable; anything else is unset.", "lib/lmstudio.js:17,28–32,75", func(s string, _ bool) *bool {
		switch strings.ToLower(jsTrim(s)) {
		case "1", "true", "yes", "on":
			return new(true)
		case "0", "false", "no", "off":
			return new(false)
		default:
			return nil
		}
	})
	LMStudioFirstWait = numberKey("CODEX_GATEWAY_LMSTUDIO_FIRST_EVENT_WAIT_MS", "Max milliseconds to wait for the first LM Studio stream event.", "accept a finite nonnegative value.", "lib/request-worker.js:276–279", 10000, finiteNonnegative)
	LMToken           = trimmedKey("LM_API_TOKEN", "lmstudio.apiKey in proxy config", "Second LM Studio bearer token candidate.", "lib/lmstudio.js:21,24–26,84")
	ModelsRefresh     = numberKey("CODEX_GATEWAY_MODELS_REFRESH_MS", "Model listing refresh interval in milliseconds; a fraction is truncated, a value below 1 refreshes every millisecond, and a value above 2147483647, positive infinity included, refreshes every 2147483647 milliseconds.", "accept any positive value, including positive infinity.", "lib/request-worker.js:266–267", 60000, func(n float64) bool { return n > 0 })
	NodeTest          = key("NODE_TEST_CONTEXT", "off", "Under Node's test runner, restrict writes and disallow invoking the default Claude CLI.", "Any nonempty string enables the restrictions, including 0 and whitespace.", "lib/pins.js:212–214; lib/settings-wiring.js:88–105", func(s string, _ bool) bool { return s != "" })
	NoProxy           = stringKey("NO_PROXY", "", "First inherited bypass list for the pin probe; followed by no_proxy and loopback.", "lib/pins.js:246–257")
	NoProxyLower      = stringKey("no_proxy", "", "Second inherited bypass list for the pin probe; followed by loopback.", "lib/pins.js:246–257")
	OpenCodeProxyKey  = stringKey("CCP_OPENCODE_API_KEY", "", "First OpenCode credential presence check, before OPENCODE_API_KEY and opencode.apiKey in proxy config.", "lib/commands.js:1680–1685")
	OpenCodeKey       = stringKey("OPENCODE_API_KEY", "", "Second OpenCode credential presence check, before opencode.apiKey in proxy config.", "lib/commands.js:1680–1685")
	PinCacheTTL       = numberKey("CODEX_GATEWAY_PIN_CACHE_TTL_MS", "Detected-pin cache TTL in milliseconds.", "fall back only for zero or NaN.", "lib/runtime.js:547", 24*60*60*1000, truthyNumber)
	PinProbeTimeout   = numberKey("CODEX_GATEWAY_PIN_PROBE_TIMEOUT_MS", "Claude pin probe timeout in milliseconds.", "fall back only for zero or NaN.", "lib/runtime.js:548", 5000, truthyNumber)
	PluginCache       = candidate("CLAUDE_CODE_PLUGIN_CACHE_DIR", "unset", "Extra plugin registry directory, searched first.", "Use any nonempty string without trimming; resolve it to an absolute path before searching.", "lib/runtime.js:682–689; hooks/registry-writer.js:43–51")
	PluginRoot        = candidate("CLAUDE_PLUGIN_ROOT", "plugin's own root", "Root of the current plugin installation.", "Use any nonempty string without trimming; resolve it to an absolute path. Claude also expands this in the hook command.", "hooks/ensure.js:24–27; hooks/registry-writer.js:125–127; hooks/hooks.json:9")
	ProbeTimeoutValue = key("CODEX_GATEWAY_PROBE_TIMEOUT_MS", "8000 on Windows, 2000 otherwise", "Listener ownership probe timeout in milliseconds.", "Apply Number, fall back for zero or NaN, then clamp to at least 1.", "lib/process-supervision.js:349–354", func(s string, set bool) *float64 {
		if n := envNumber(s, set); truthyNumber(n) {
			return new(max(1, n))
		}
		return nil
	})
	ProxyCompression   = rawKey("CCP_CODEX_REQUEST_COMPRESSION", "Proxy request compression override.", "Any defined string, even empty, wins over the gateway override.", "lib/process-supervision.js:1009–1013")
	GatewayCompression = rawKey("CODEX_GATEWAY_REQUEST_COMPRESSION", "Legacy proxy request compression override.", "Pass the raw value, including empty, only if CCP_CODEX_REQUEST_COMPRESSION is unset.", "lib/process-supervision.js:1009–1013")
	ProxyConfig        = candidate("CCP_CONFIG_DIR", `Windows: APPDATA/claude-code-proxy (or home/AppData/Roaming/claude-code-proxy); macOS: home/.config/claude-code-proxy; other platforms: XDG_CONFIG_HOME/claude-code-proxy (or home/.config/claude-code-proxy)`, "Explicit proxy config directory.", "Use any nonempty string without trimming; otherwise use the platform default.", "lib/proxy-config.js:12–16")
	ProxyPort          = portKey("CODEX_GATEWAY_PROXY_PORT", "Underlying claude-code-proxy port.", "lib/runtime.js:26; lib/commands.js:90", "18765")
	ProxyRecovery      = key("CODEX_GATEWAY_PROXY_RECOVERY_INTERVAL_MS", "5000", "Proxy recovery polling interval in milliseconds.", "Apply Number, fall back for zero or NaN, then clamp to at least 1000.", "lib/commands.js:2504", func(s string, set bool) float64 {
		return max(1000, numericOr(envNumber(s, set), 5000))
	})
	PublicPort          = key("CODEX_GATEWAY_PORT", publicPortDefault, "Public shim port.", "Use the default if unset or empty, then apply Number without validating. Recovery compares the raw spelling with no numeric conversion.", "lib/runtime.js:24,46; lib/commands.js:88; lib/recovery-policy.js:29", func(s string, _ bool) float64 { return jsNumber(defaultPublicPort(s)) })
	RegistryClaudeHome  = candidate("MODEL_GATEWAY_CLAUDE_HOME", "normal registry search", "Whole-config-directory override for the registry-writer launcher only.", "Use any nonempty string without trimming; skip both normal registry candidates.", "hooks/registry-writer.js:40–51")
	RequestBodyDir      = candidate("MODEL_GATEWAY_REQUEST_BODY_DIR", "Go: state/request-body; Node: home/.claude/model-gateway/request-body", "Request-body high-water mark directory.", "Use any nonempty string without trimming.", "lib/usage-observability.js:23–24")
	RequestLog          = toggle("CODEX_GATEWAY_REQUEST_LOG", "Write request route logs.", "lib/runtime.js:35; lib/commands.js:126", true)
	RequestLogPath      = candidate("CODEX_GATEWAY_REQUEST_LOG_PATH", "state/logs/request-routes.jsonl", "Request route log file.", "Use any nonempty string without trimming.", "lib/runtime.js:36; lib/commands.js:127")
	Sentry              = toggle("CODEX_GATEWAY_SENTRY", "Enable the synthetic compaction sentry.", "lib/request-worker.js:255", true)
	SocketPath          = candidate("CODEX_GATEWAY_SOCKET_PATH", `state/gateway.sock; Windows: \\.\pipe\model-gateway`, "Local gateway socket or Windows named pipe.", "Use any nonempty string without trimming.", "lib/runtime.js:47–49")
	SSEHeartbeatSeconds = numberKey("CODEX_GATEWAY_SSE_HEARTBEAT_S", "Stream heartbeat interval in seconds; zero disables heartbeats.", "accept a finite nonnegative value and convert seconds to milliseconds at the worker.", "lib/request-worker.js:268–271", 20, finiteNonnegative)
	StartupGeneration   = rawKey("MODEL_GATEWAY_STARTUP_GENERATION", "Generation recorded in the supervisor failure diagnostic.", "Preserve an explicitly empty string; omit the field if unset.", "lib/commands.js:624")
	StateDirectory      = candidate("MODEL_GATEWAY_STATE_DIR", "gateway home", "Go gateway state override; does not move the stable installation or Claude settings.", "Use any nonempty string without trimming.", "Go port only: internal/paths/paths.go")
	SystemRoot          = key("SystemRoot", `C:\Windows for tar and hosts; windir fallback for temp`, "Windows system directory parent for tar.exe, the hosts file read by doctor and the migration check, and the default temporary directory.", `Use any nonempty value without trimming. The temporary directory uses the raw value or windir, then appends \temp.`, "lib/commands.js:460; internal/paths/paths.go (HostsFile); Node v24.11.1 lib/os.js:180–194 (os.tmpdir)", func(s string, _ bool) string { return cmp.Or(s, `C:\Windows`) })
	Temp                = tempPathKey("TEMP", `Windows: TMP, then SystemRoot or windir; POSIX: /tmp`, "First Windows temporary directory candidate; third on POSIX.")
	Tmp                 = tempPathKey("TMP", `Windows: SystemRoot or windir; POSIX: TEMP, then /tmp`, "Second temporary directory candidate on each platform.")
	TmpDir              = tempPathKey("TMPDIR", "TMP, then TEMP, then /tmp", "First POSIX temporary directory candidate; ignored on Windows.")
	WinDir              = candidate("windir", `undefined\temp when neither Windows root value is defined`, "Fallback Windows system directory parent for the temporary directory.", `Used when TEMP, TMP and SystemRoot are empty or unset. Append \temp to the raw value, including empty.`, "lib/settings-wiring.js:103–104; lib/picker-guard.js:95; Node v24.11.1 lib/os.js:180–194")
	Traceparent         = toggle("CLAUDE_CODE_PROPAGATE_TRACEPARENT", "Enable route tracing and traceparent propagation.", "lib/runtime.js:40; lib/commands.js:130", false)
	Upstream            = stringKey("CODEX_GATEWAY_ANTHROPIC_UPSTREAM", "https://api.anthropic.com", "Upstream for Anthropic passthrough and catalog fetches.", "lib/runtime.js:34; lib/commands.js:122; lib/anthropic-catalog.js:61 (via runtime)")
	UsageMaxResponse    = key("CODEX_GATEWAY_USAGE_MAX_RESPONSE_BYTES", "4194304", "Usage response side-buffer ceiling in bytes.", "Apply Number, fall back for zero or NaN, then clamp to at least 1024.", "lib/usage-observability.js:986–988", func(s string, set bool) float64 {
		return max(1024, numericOr(envNumber(s, set), 4*1024*1024))
	})
	UserProfile     = candidate("USERPROFILE", "injected home lookup", "Windows user home.", "Use any nonempty value on Windows; ignored elsewhere.", "lib/runtime.js:15–16 (os.homedir)")
	VertexProject   = trimmedKey("CCP_VERTEX_PROJECT", "GOOGLE_CLOUD_PROJECT, then vertex.project in proxy config", "First Vertex project presence check.", "lib/commands.js:1674–1677")
	WorkerPortValue = candidate("CODEX_GATEWAY_WORKER_PORT", "public port (runtime); 0 (supervisor)", "Worker port override.", "Use the context-specific default if unset or empty, otherwise apply Number without validating. The worker also checks whether the raw override is nonempty.", "lib/runtime.js:25; lib/commands.js:89,2487; lib/request-worker.js:2713")
	WSRetries       = numberKey("CODEX_GATEWAY_WS_UPGRADE_RETRIES", "WebSocket upgrade retries.", "accept a nonnegative integer.", "lib/request-worker.js:280–283", 2, integerNonnegative)
	WSRetryDelay    = numberKey("CODEX_GATEWAY_WS_UPGRADE_RETRY_DELAY_MS", "Milliseconds between WebSocket upgrade retries.", "accept a finite nonnegative value.", "lib/request-worker.js:284–287", 250, finiteNonnegative)
	XDGConfigHome   = candidate("XDG_CONFIG_HOME", "home/.config", "Proxy config parent directory on non-Windows, non-macOS systems.", "Use any nonempty string without trimming and append claude-code-proxy; ignored on Windows and macOS.", "lib/proxy-config.js:12–16")
)

func tempPathKey(name, fallback, meaning string) Key[string] {
	return candidate(name, fallback, meaning, "First nonempty candidate wins. Remove one trailing slash on POSIX unless the value is the root; on Windows remove one trailing backslash unless it is a root or follows a colon.", "lib/settings-wiring.js:103–104; lib/picker-guard.js:95; Node v24.11.1 lib/os.js:180–194 and src/node_credentials.cc:119–139")
}

const (
	pinExclusion = "lib/pins.js:241–247"
	pickerMarker = "lib/picker-guard.js:17,90–99"
	pickerOwned  = "lib/picker-guard.js:90–99"
)

var (
	anthropicAPIKey  = filteredKey("ANTHROPIC_API_KEY", "Pin probe", pinExclusion, true)
	anthropicAuth    = filteredKey("ANTHROPIC_AUTH_TOKEN", "Pin probe", pinExclusion, true)
	anthropicFable   = filteredKey("ANTHROPIC_DEFAULT_FABLE_MODEL", "Pin and picker probes", pinExclusion+"; "+pickerOwned+"; lib/runtime.js:528–532", true)
	anthropicOpus    = filteredKey("ANTHROPIC_DEFAULT_OPUS_MODEL", "Pin and picker probes", pinExclusion+"; "+pickerOwned+"; lib/runtime.js:528–532", true)
	anthropicSonnet  = filteredKey("ANTHROPIC_DEFAULT_SONNET_MODEL", "Pin and picker probes", pinExclusion+"; "+pickerOwned+"; lib/runtime.js:528–532", true)
	claudeOAuth      = filteredKey("CLAUDE_CODE_OAUTH_TOKEN", "Pin probe", pinExclusion, true)
	claudeCode       = filteredKey("CLAUDECODE", "Picker probe", pickerMarker, false)
	claudeEntrypoint = filteredKey("CLAUDE_CODE_ENTRYPOINT", "Picker probe", pickerMarker, false)
	claudeSSEPort    = filteredKey("CLAUDE_CODE_SSE_PORT", "Picker probe", pickerMarker, false)
	claudeSessionID  = filteredKey("CLAUDE_CODE_SESSION_ID", "Picker probe", pickerMarker, false)
	claudeDiscovery  = filteredKey("CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY", "Picker probe", pickerOwned+"; lib/runtime.js:522–527", false)
	claudeNoFallback = filteredKey("CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK", "Picker probe", pickerOwned+"; lib/runtime.js:522–527", false)
	claudeMaxOutput  = filteredKey("CLAUDE_CODE_MAX_OUTPUT_TOKENS", "Picker probe", pickerOwned+"; lib/runtime.js:522–527", false)
	enableToolSearch = filteredKey("ENABLE_TOOL_SEARCH", "Picker probe", pickerOwned+"; lib/runtime.js:522–527", false)
	customModel      = filteredKey("ANTHROPIC_CUSTOM_MODEL_OPTION", "Picker probe", pickerOwned+"; lib/pins.js:394–398,414–425", false)
	customModelName  = filteredKey("ANTHROPIC_CUSTOM_MODEL_OPTION_NAME", "Picker probe", pickerOwned+"; lib/pins.js:394–398,414–425", false)
	customModelDesc  = filteredKey("ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION", "Picker probe", pickerOwned+"; lib/pins.js:394–398,414–425", false)
)

func filteredKey(name, reader, sources string, caseInsensitive bool) Variable {
	rule := "Match the exact key regardless of its value, including empty."
	if caseInsensitive {
		rule = "Pin probe matches the key case-insensitively; picker probe, where applicable, matches it exactly. The value is never inspected."
	}
	return Variable{Name: name, Default: "unset", Meaning: reader + " strips this inherited key.", Rule: rule, Readers: sources}
}

// Variables returns a copy of the environment catalog in name order.
func Variables() []Variable {
	rows := slices.Clone(variables)
	slices.SortFunc(rows, func(a, b Variable) int { return cmp.Compare(a.Name, b.Name) })
	return rows
}

var variables = []Variable{
	AnthropicBaseURL.variable, anthropicAPIKey, anthropicAuth, anthropicFable,
	anthropicOpus, anthropicSonnet, AppData.variable, ClaudeAutoCompactWindow.variable,
	ClaudeBin.variable, ClaudeConfig.variable, CompactGuard.variable, CompactMax.variable,
	CompactRetries.variable, CompactDelay.variable, CompactTrigger.variable, CodexContextWindow.variable,
	DispatchCachePath.variable, DispatchMax.variable, DispatchTTL.variable, DrainTimeout.variable,
	GitHubToken.variable, GHToken.variable, GoogleCloudProject.variable, GrokEndpoint.variable, GrokHome.variable,
	Home.variable, HostsFile.variable, KeepPlanTools.variable, ListDispatchModel.variable, LlamaServerPort.variable,
	LMStudioAPIKey.variable, LMStudioBaseURL.variable, LMStudioEnabled.variable, LMStudioFirstWait.variable,
	LMToken.variable, ModelsRefresh.variable, NodeTest.variable, NoProxy.variable, NoProxyLower.variable,
	OpenCodeProxyKey.variable, OpenCodeKey.variable, PinCacheTTL.variable, PinProbeTimeout.variable,
	PluginCache.variable, PluginRoot.variable, ProbeTimeoutValue.variable, ProxyCompression.variable,
	GatewayCompression.variable, ProxyConfig.variable, ProxyPort.variable, ProxyRecovery.variable, PublicPort.variable,
	RegistryClaudeHome.variable, RequestBodyDir.variable, RequestLog.variable, RequestLogPath.variable, Sentry.variable,
	SocketPath.variable, SSEHeartbeatSeconds.variable, StartupGeneration.variable, StateDirectory.variable, SystemRoot.variable,
	Temp.variable, Tmp.variable, TmpDir.variable, WinDir.variable,
	Traceparent.variable, Upstream.variable, UsageMaxResponse.variable, UserProfile.variable, VertexProject.variable,
	WorkerPortValue.variable, WSRetries.variable, WSRetryDelay.variable, XDGConfigHome.variable,
	claudeOAuth, claudeCode, claudeEntrypoint, claudeSSEPort, claudeSessionID,
	claudeDiscovery, claudeNoFallback, claudeMaxOutput, enableToolSearch,
	customModel, customModelName, customModelDesc,
	TraceEndpoint.variable, OTelTracesEndpoint.variable, OTelEndpoint.variable, OTelLogsEndpoint.variable, UsageEndpoint.variable,
}
