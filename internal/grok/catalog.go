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

package grok

import "net/http"

const (
	// DefaultEndpoint is the default Grok Responses API endpoint.
	DefaultEndpoint = "https://cli-chat-proxy.grok.com/v1/responses"
	// DefaultTokenEndpoint is the default OAuth token endpoint for Grok CLI auth refreshes.
	DefaultTokenEndpoint = "https://auth.x.ai/oauth/token"
	// DefaultIssuer is the default OIDC issuer for Grok CLI auth entries.
	DefaultIssuer = "https://auth.x.ai"
	// VersionFallback is the fallback Grok CLI client version accepted by the backend.
	// The backend rejects client versions older than 1.0.13 with HTTP 426; the implementation sets 1.0.60.
	VersionFallback = ""
)

// Model describes a Grok backend model's context window and reasoning support.
type Model struct {
	ID        string
	Context   int
	Reasoning bool
}

// DefaultModels returns a fresh slice of the built-in Grok backend models in declaration order.
func DefaultModels() []Model {
	return nil
}

// ModelsFromCache reads Grok model metadata from a CLI models_cache.json file.
// Missing, unreadable, or malformed files return nil.
func ModelsFromCache(file string) []Model {
	return nil
}

// ModelIDsFromCache returns the ordered Grok model IDs read from a CLI models_cache.json file.
func ModelIDsFromCache(file string) []string {
	return nil
}

// ReadVersion reads the Grok CLI version from a version.json file, returning VersionFallback
// when the file is missing, unreadable, malformed, or does not start with a numeric semver prefix.
func ReadVersion(file string) string {
	return ""
}

// Headers builds the upstream HTTP headers required by the Grok Responses endpoint.
// An empty version uses VersionFallback.
func Headers(token, model, version string) http.Header {
	return nil
}
