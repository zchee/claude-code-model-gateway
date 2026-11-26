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

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
)

// StatusRunner executes `<binary> <provider> auth status` and returns its exit code.
// A non-nil error indicates the command failed to run or timed out without an exit status.
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
	_, _, _ = ctx, provider, opts
	return ""
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
	_, _, _, _ = provider, checks, authState, opts
	return ProviderReadiness{}
}

// DescribeProviderAuth formats the doctor summary line for a proxy provider's auth or server state.
func DescribeProviderAuth(provider, state string, opts ProviderMessageOptions) string {
	_, _, _ = provider, state, opts
	return ""
}

// SharedFailureMessage formats the refusal message for a failed shared proxy check.
func SharedFailureMessage(state string, opts ProviderMessageOptions) string {
	_, _ = state, opts
	return ""
}
