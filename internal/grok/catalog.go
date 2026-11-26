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

import (
	"math"
	"net/http"
	"os"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

const (
	// DefaultEndpoint is the default Grok Responses API endpoint.
	DefaultEndpoint = "https://cli-chat-proxy.grok.com/v1/responses"
	// DefaultTokenEndpoint is the default OAuth token endpoint for Grok CLI auth refreshes.
	DefaultTokenEndpoint = "https://auth.x.ai/oauth/token"
	// DefaultIssuer is the default OIDC issuer for Grok CLI auth entries.
	DefaultIssuer = "https://auth.x.ai"
	// VersionFallback is the fallback Grok CLI client version accepted by the backend.
	// The backend rejects client versions older than 1.0.13 with HTTP 426.
	VersionFallback = "1.0.60"
)

// unknownModelContext is the context window of a cached model the built-in table does not know.
const unknownModelContext = 131072

// Model describes a Grok backend model's context window and reasoning support.
// Context retains the positive finite floating-point number from the CLI cache without integer narrowing.
type Model struct {
	ID        string
	Context   float64
	Reasoning bool
}

// defaultModels is the source's built-in model table (lib/grok-backend.js:12-16).
var defaultModels = [...]Model{
	{ID: "grok-4.7", Context: 500000, Reasoning: true},
	{ID: "grok-4.6", Context: 500000, Reasoning: true},
	{ID: "grok-4.5", Context: 500000, Reasoning: true},
}

// DefaultModels returns a fresh slice of the built-in Grok backend models in declaration order.
func DefaultModels() []Model {
	return append([]Model(nil), defaultModels[:]...)
}

func defaultModel(id string) (Model, bool) {
	for _, m := range defaultModels {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// readJSONFile parses file as the source's JSON.parse(fs.readFileSync(file, 'utf8')) does.
func readJSONFile(file string) (jsonx.WireValue, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return jsonx.WireValue{}, false
	}
	v, err := jsonx.DecodeWire(data)
	if err != nil {
		return jsonx.WireValue{}, false
	}
	return v, true
}

// isModelID is the source's /^grok-[a-z0-9.-]+$/i. Without the u flag the case-insensitive match
// folds only ASCII letters, and $ matches only at the end of the input.
func isModelID(id []uint16) bool {
	const prefix = "grok-"
	if len(id) <= len(prefix) {
		return false
	}
	for i := range len(prefix) {
		c := id[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != uint16(prefix[i]) {
			return false
		}
	}
	for _, c := range id[len(prefix):] {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// ModelsFromCache reads Grok model metadata from a CLI models_cache.json file.
// Missing, unreadable, or malformed files return nil.
func ModelsFromCache(file string) []Model {
	parsed, ok := readJSONFile(file)
	if !ok {
		return nil
	}
	var values []jsonx.WireValue
	switch models := get(parsed, "models"); {
	case parsed.Kind == '[':
		values = parsed.Elements
	case models.Kind == '[':
		values = models.Elements
	case get(parsed, "data").Kind == '[':
		values = get(parsed, "data").Elements
	case models.Kind == '{':
		for _, m := range models.Members {
			values = append(values, m.Value)
		}
	}
	var out []Model
	seen := make(map[string]bool)
	for _, value := range values {
		details := value
		if info := get(value, "info"); info.Kind == '{' || info.Kind == '[' {
			details = info
		}
		id := value
		if value.Kind != '"' {
			id = or(get(details, "id"), get(details, "model"))
		}
		if id.Kind != '"' || !isModelID(id.Text) {
			continue
		}
		// isModelID admits only ASCII, so the conversion is exact.
		name := string(utf16.Decode(id.Text))
		if seen[name] {
			continue
		}
		window := get(details, "context_window")
		if coercionThrows(window) {
			// Number() throws inside the source's try block, which then returns an empty list.
			return nil
		}
		fallback, known := defaultModel(name)
		m := Model{ID: name, Context: unknownModelContext}
		if known {
			m.Context = fallback.Context
		}
		if n := jsNumber(window); !math.IsNaN(n) && !math.IsInf(n, 0) && n > 0 {
			m.Context = n
		}
		switch reasoning := get(details, "supports_reasoning_effort"); reasoning.Kind {
		case 't', 'f':
			m.Reasoning = reasoning.Kind == 't'
		default:
			m.Reasoning = known && fallback.Reasoning
		}
		seen[name] = true
		out = append(out, m)
	}
	return out
}

// ModelIDsFromCache returns the ordered Grok model IDs read from a CLI models_cache.json file.
func ModelIDsFromCache(file string) []string {
	models := ModelsFromCache(file)
	if len(models) == 0 {
		return nil
	}
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
}

// hasVersionPrefix is the source's /^\d+\.\d+\.\d+/ over UTF-16 units; \d is ASCII without the u flag.
func hasVersionPrefix(v []uint16) bool {
	i := 0
	for part := range 3 {
		start := i
		for i < len(v) && v[i] >= '0' && v[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
		if part < 2 {
			if i >= len(v) || v[i] != '.' {
				return false
			}
			i++
		}
	}
	return true
}

// ReadVersion reads the Grok CLI version from a version.json file, returning VersionFallback
// when the file is missing, unreadable, malformed, or does not start with a numeric semver prefix.
func ReadVersion(file string) string {
	parsed, ok := readJSONFile(file)
	if !ok {
		return VersionFallback
	}
	value := get(parsed, "version")
	if value.Kind != '"' {
		value = get(parsed, "client_version")
	}
	if value.Kind != '"' || !hasVersionPrefix(value.Text) {
		return VersionFallback
	}
	return string(utf16.Decode(value.Text))
}

// Headers builds the upstream HTTP headers required by the Grok Responses endpoint.
// Because Go has no undefined default argument, an empty version uses VersionFallback;
// callers should pass the version returned by ReadVersion.
func Headers(token, model, version string) http.Header {
	if version == "" {
		version = VersionFallback
	}
	h := make(http.Header, 6)
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "xai-grok-build/"+version)
	h.Set("X-Grok-Client-Version", version)
	h.Set("X-Grok-Client-Identifier", "xai-grok-cli")
	h.Set("X-Grok-Model-Override", model)
	return h
}
