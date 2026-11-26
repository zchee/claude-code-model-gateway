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

package policy

import (
	"fmt"
	"slices"
)

const (
	aliasTemplate = "claude-{backendId}[1m]"
	guarded       = "synthetic-413"
	clientLimit   = 1000000
	unknownWindow = 200000
	maxSafe       = 1<<53 - 1
	earlyCodex    = "measured 2026-09-05: 920012 accepted; 935012 refused"
	fastGrok      = "same model as grok-4.7 per claude-code-proxy grok-4.7-fast branch; window from grok-4.7 (models.dev 2026-10-17, Grok CLI README 2026-10-16); not a live probe"
	museEvidence  = "claude-code-proxy dev 1b2e80b 2026-11-10 docs/providers/muse.md (context 1007997, max output 128000 for every Muse model); not a live probe"
	vertexCatalog = "models.dev google-vertex 2026-11-12"
	vertexNew     = "models.dev google-vertex 2026-11-25"
	vertexProxy   = "claude-code-proxy dev c31326c 2026-11-12 docs/providers/vertex.md, quoting Google's input limit"
)

func measured(backend, transport, id string, window, advertised int64, evidence string) Model {
	return Model{Backend: backend, Transport: transport, BackendID: id, BackendWindow: window, AdvertisedWindow: advertised, PickerAlias: "claude-" + id + "[1m]", Measurement: evidence, Sentry: guarded}
}

func vertex(id string, window int64, source string) Model {
	return measured("vertex", "proxy", id, window, min(window, clientLimit), fmt.Sprintf("%s (context %d); not a live probe", source, window))
}

// modelRows is read-only. Keep the source order: the first row of each backend represents it in reports.
var modelRows = [...]Model{
	{Backend: "codex", BackendID: "default", BackendWindow: 920000, AdvertisedWindow: 920000, Measurement: "unmeasured default for Codex proxy rows absent from this table", PickerAliasTemplate: aliasTemplate, Sentry: guarded},
	measured("codex", "", "gpt-5.6-sol", 920012, 920000, earlyCodex),
	measured("codex", "", "gpt-5.6-terra", 920012, 920000, earlyCodex),
	measured("codex", "", "gpt-5.6-luna", 920012, 920000, earlyCodex),
	measured("codex", "", "gpt-6-astra", 920012, 920000, earlyCodex),
	measured("codex", "", "gpt-6.1-sol", 920012, 920000, "measured 2026-09-30: 920012 accepted; 935012 refused"),
	measured("grok", "", "grok-4.7", 500000, 500000, "models.dev xai/grok-4.7 2026-10-17 (context 500000) and Grok CLI models_cache.json 2026-09-23 (500000); the Grok CLI README calls its 256000 default an auto-compact threshold with 500000 selectable, not a backend limit; not a live probe"),
	measured("grok", "proxy", "grok-4.7-fast", 500000, 500000, fastGrok),
	measured("grok", "proxy", "grok-4.7-build-fast", 500000, 500000, fastGrok),
	measured("grok", "", "grok-4.6", 500000, 500000, "measured 2026-09-15 from Grok CLI models_cache.json (context_window 500000)"),
	measured("grok", "", "grok-4.5", 500000, 500000, "measured 2026-09-05 from grok-backend.js GROK_MODELS"),
	measured("muse", "proxy", "muse-spark-1.3", 1007997, clientLimit, museEvidence),
	measured("muse", "proxy", "muse-spark-1.2", 1007997, clientLimit, museEvidence),
	measured("muse", "proxy", "muse-wonder-1.0", 1007997, clientLimit, museEvidence),
	measured("muse", "proxy", "muse-wonder-full-1.0", 1007997, clientLimit, museEvidence),
	measured("muse", "proxy", "muse-ember-1.0", 1007997, clientLimit, museEvidence),
	vertex("gemini-4-argon", 1048576, vertexCatalog),
	vertex("vertex/gemini-4-argon", 1048576, vertexCatalog),
	vertex("gemini-4-carbon", 1048576, vertexCatalog),
	vertex("vertex/gemini-4-carbon", 1048576, vertexCatalog),
	vertex("gemini-3.8-flash", 1048576, vertexCatalog),
	vertex("vertex/gemini-3.8-flash", 1048576, vertexCatalog),
	vertex("gemini-3.7-flash", 1048576, vertexCatalog),
	vertex("vertex/gemini-3.7-flash", 1048576, vertexCatalog),
	vertex("gemini-3.5-flash", 1048576, vertexCatalog),
	vertex("vertex/gemini-3.5-flash", 1048576, vertexCatalog),
	vertex("gemini-3.5-flash-lite", 1048576, vertexCatalog),
	vertex("vertex/gemini-3.5-flash-lite", 1048576, vertexCatalog),
	vertex("gemini-3.1-flash-lite", 1048576, vertexCatalog),
	vertex("vertex/gemini-3.1-flash-lite", 1048576, vertexCatalog),
	vertex("gemini-3.1-pro-preview", 1048576, vertexCatalog),
	vertex("vertex/gemini-3.1-pro-preview", 1048576, vertexCatalog),
	vertex("gemini-3-flash-preview", 1048576, vertexCatalog),
	vertex("vertex/gemini-3-flash-preview", 1048576, vertexCatalog),
	vertex("vertex/anthropic/claude-fable-5-2", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-fable-5-1", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-fable-5", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-opus-5-5", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-opus-5", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-opus-4-8", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-opus-4-7", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-sonnet-5-5", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-sonnet-5", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-haiku-5-5", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-opus-4-6", 1000000, vertexCatalog),
	vertex("vertex/anthropic/claude-sonnet-4-6", 1000000, vertexCatalog),
	vertex("vertex/xai/grok-4.7", 524288, vertexCatalog),
	vertex("vertex/xai/grok-4.6", 524288, vertexCatalog),
	vertex("vertex/xai/grok-4.20-reasoning", 2000000, vertexCatalog),
	vertex("vertex/xai/grok-4.20-non-reasoning", 2000000, vertexCatalog),
	vertex("vertex/zai-org/glm-5.2-maas", 1000000, vertexCatalog),
	vertex("vertex/google/gemma-4-26b-a4b-it-maas", 262144, vertexCatalog),
	vertex("vertex/meta/llama-4-maverick-17b-128e-instruct-maas", 524288, vertexCatalog),
	vertex("vertex/meta/llama-4-scout-17b-16e-instruct-maas", 1310720, vertexCatalog),
	vertex("gemini-4-hydrogen", 1048576, vertexNew),
	vertex("vertex/gemini-4-hydrogen", 1048576, vertexNew),
	vertex("gemini-4-neon", 1048576, vertexNew),
	vertex("vertex/gemini-4-neon", 1048576, vertexNew),
	vertex("vertex/openai/gpt-6-astra", 922000, vertexProxy),
	vertex("vertex/openai/gpt-6.1-sol", 922000, vertexProxy),
	vertex("vertex/openai/gpt-6.1-terra", 922000, vertexProxy),
	vertex("vertex/openai/gpt-6-luna", 922000, vertexProxy),
	vertex("vertex/meta/muse-spark-1.3", 1048576, vertexProxy),
	vertex("vertex/meta/muse-wonder-1.0", 1048576, vertexProxy),
	vertex("vertex/meta/muse-ember-1.0", 1048576, vertexProxy),
	{Backend: "anthropic", BackendID: "anthropic", Measurement: "upstream-managed native Anthropic passthrough", PickerAlias: "claude-anthropic", Sentry: "none"},
}

// Rows returns the window table in declaration order, independent of caller mutations.
func Rows() []Model { return slices.Clone(modelRows[:]) }
