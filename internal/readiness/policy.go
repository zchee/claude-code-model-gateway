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

// Package readiness decides whether recovery applies to the effective Claude settings.
package readiness

import "github.com/zchee/claude-code-model-gateway/internal/config"

// compatBaseURL is the remote-control compatibility endpoint (lib/runtime.js:44,50).
const compatBaseURL = "http://api.anthropic.com"

// Settings contains the settings members consulted by the recovery policy. Non-boolean plugin entries do not participate.
type Settings struct {
	BaseURL        *string
	EnabledPlugins map[string]any
}

// Policy returns unwired, disabled, legacy or recover using settings in decreasing precedence order.
// Legacy means the effective base URL is the retired remote-control compatibility URL: the gateway owns it, but
// the port has no listener that serves it, so a healthy gateway is no evidence that such a session can reach
// it, and recovery does not apply.
func Policy(lookup config.LookupEnv, settings []Settings, pluginID string) string {
	// The launcher compares the raw port spelling, with no numeric conversion (lib/recovery-policy.js:29).
	endpoint, ok := recoveryEndpoint(lookup, settings)
	if !ok || (endpoint != "http://127.0.0.1:"+config.RecoveryPublicPort(lookup) && endpoint != compatBaseURL) {
		return "unwired"
	}
	if enabled, ok := pluginEnabled(settings, pluginID); ok && !enabled {
		return "disabled"
	}
	if endpoint == compatBaseURL {
		return "legacy"
	}
	return "recover"
}

// recoveryEndpoint prefers a process value, even an empty one, over the first settings file that defines one
// (lib/recovery-policy.js:22-25).
func recoveryEndpoint(lookup config.LookupEnv, settings []Settings) (string, bool) {
	if value := config.AnthropicBaseURL.Get(lookup); value != nil {
		return *value, true
	}
	for _, s := range settings {
		if s.BaseURL != nil {
			return *s.BaseURL, true
		}
	}
	return "", false
}

// pluginEnabled returns the first boolean entry for the plugin; other value types are skipped
// (lib/recovery-policy.js:17-20).
func pluginEnabled(settings []Settings, pluginID string) (enabled, ok bool) {
	for _, s := range settings {
		if v, isBool := s.EnabledPlugins[pluginID].(bool); isBool {
			return v, true
		}
	}
	return false, false
}
