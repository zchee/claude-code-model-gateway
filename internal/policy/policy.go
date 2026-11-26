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

// Package policy resolves gateway model identities, windows and compaction limits.
package policy

import (
	"cmp"
	"fmt"
	"maps"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

// Model describes a table row or a resolved model. Zero window sizes mean unknown.
type Model struct {
	Backend, Transport, BackendID                         string
	BackendWindow, AdvertisedWindow                       int64
	Measurement, PickerAlias, PickerAliasTemplate, Sentry string
	CompactHeadroom                                       int64
}

// ResolverInputs supplies a route snapshot and observed server windows without querying servers.
// LMStudioWindows is keyed by the server's model key, without the provider prefix.
// Nil Windows selects the gateway defaults; zero live windows indicate no measurement.
type ResolverInputs struct {
	Routes          proxymodels.Table
	Windows         Windows
	LocalWindow     int64
	LocalPort       int
	LMStudioURL     string
	LMStudioWindows map[string]int64
}

// Resolver is an immutable snapshot. Its zero value uses defaults with no listed routes.
type Resolver struct{ inputs ResolverInputs }

// NewResolver takes ownership of copies of the supplied windows; the route table is already immutable.
func NewResolver(inputs ResolverInputs) Resolver {
	inputs.Windows = maps.Clone(inputs.Windows)
	inputs.LMStudioWindows = maps.Clone(inputs.LMStudioWindows)
	return Resolver{inputs: inputs}
}

// BackendID resolves a picker spelling to the backend ID, including any service tier.
func (r Resolver) BackendID(id string) string {
	part := strings.TrimPrefix(strings.TrimSuffix(id, "[1m]"), "claude-")
	routes := r.inputs.Routes
	if routes.Provider(part) != "" {
		return part
	}
	if listed, ok := routes.RouteIDForAdvertisedPart(part); ok {
		return listed
	}
	fallback := part
	for i := range modelRows {
		key := modelRows[i].BackendID
		if key == part {
			return part
		}
		if routes.AdvertisedPart(key) == part {
			fallback = key
		} // Later respellings replace earlier ones.
	}
	return fallback
}

func baseID(id string) string {
	if strings.HasPrefix(id, "gpt-") {
		if base, ok := strings.CutSuffix(id, "-ultrafast"); ok {
			return base
		}
		return strings.TrimSuffix(id, "-fast")
	}
	return id
}

// WindowModelID returns the ID used to share a window between Codex service tiers.
func (r Resolver) WindowModelID(id string) string { return cmp.Or(baseID(r.BackendID(id)), "default") }

func (r Resolver) providerPolicy(provider, id string) Model {
	m := Model{Backend: provider, Transport: "proxy", PickerAliasTemplate: aliasTemplate, AdvertisedWindow: unknownWindow, Sentry: "none"}
	var n int64
	switch provider {
	case "local":
		n = r.inputs.LocalWindow
	case "lmstudio":
		if key, ok := strings.CutPrefix(id, "lmstudio/"); ok {
			n = r.inputs.LMStudioWindows[key]
		}
	}
	if n <= 0 || n > maxSafe {
		m.Measurement = "unmeasured default for claude-code-proxy " + provider + " rows absent from this table"
		return m
	}
	if provider == "local" {
		m.Measurement = fmt.Sprintf("llama-server /props n_ctx %d on 127.0.0.1:%d, read live", n, r.inputs.LocalPort)
	} else {
		m.Measurement = fmt.Sprintf("LM Studio /api/v1/models loaded context_length %d at %s, read live", n, r.inputs.LMStudioURL)
	}
	m.BackendWindow = n
	m.AdvertisedWindow = min(n, clientLimit)
	if n >= 32768 {
		m.Sentry = guarded
		m.CompactHeadroom = min(compactHeadroom, n/3)
	}
	return m
}

// replaceFirst uses JavaScript's replacement-string substitutions (without capture groups).
func replaceFirst(s, match, replacement string) string {
	before, after, ok := strings.Cut(s, match)
	if !ok {
		return s
	}
	if strings.Contains(replacement, "$") {
		replacement = strings.NewReplacer("$$", "$", "$&", match, "$`", before, "$'", after).Replace(replacement)
	}
	return before + replacement + after
}

// Resolve returns the model's uncapped policy, or nil for an unrecognized spelling.
func (r Resolver) Resolve(id string) *Model {
	return r.resolveBackend(r.BackendID(id), strings.HasPrefix(id, "claude-") && !strings.HasPrefix(id, "claude-codex-"))
}

// ResolveBackend returns a fresh policy for an already resolved backend ID. Use it instead of
// Resolve for stored backend keys: it never strips picker decorations or follows advertised aliases.
func (r Resolver) ResolveBackend(id string) *Model { return r.resolveBackend(id, false) }

func (r Resolver) resolveBackend(backend string, nativeFallback bool) *Model {
	base := baseID(backend)
	var m Model
	if base != "default" {
		for i := range modelRows {
			if modelRows[i].BackendID == base {
				m = modelRows[i]
				break
			}
		}
	}
	if m.Backend == "" {
		switch provider := r.inputs.Routes.Provider(backend); {
		case strings.HasPrefix(backend, "gpt-"):
			m = modelRows[0]
		case provider != "":
			m = r.providerPolicy(provider, backend)
		case nativeFallback:
			m = modelRows[len(modelRows)-1]
		default:
			return nil
		}
	}
	if m.Transport == "" {
		switch m.Backend {
		case "anthropic", "grok":
			m.Transport = m.Backend
		default:
			m.Transport = "proxy"
		}
	}
	part := r.pickerPart(m.Transport, backend)
	switch {
	case m.BackendID == backend && m.PickerAlias != "":
		m.PickerAlias = replaceFirst(m.PickerAlias, backend, part)
	case m.PickerAliasTemplate != "":
		m.PickerAlias = replaceFirst(m.PickerAliasTemplate, "{backendId}", part)
	default:
		m.PickerAlias = "claude-" + part + "[1m]"
	}
	m.BackendID = backend
	return &m
}

func (r Resolver) pickerPart(transport, id string) string {
	if transport == "proxy" {
		return r.inputs.Routes.AdvertisedPart(id)
	}
	return id
}

// ClientID returns the model spelling to advertise with this resolver's caps.
func (r Resolver) ClientID(id string) string {
	m := r.Resolve(id)
	if m == nil {
		return id
	}
	if r.cappedWindow(m) > unknownWindow {
		return m.PickerAlias
	}
	return "claude-" + r.pickerPart(m.Transport, m.BackendID)
}

func (r Resolver) cappedWindow(m *Model) int64 {
	if m == nil {
		return 0
	}
	if cap := r.inputs.Windows.Cap(m.Backend); cap != 0 && m.AdvertisedWindow != 0 {
		return min(cap, m.AdvertisedWindow)
	}
	return m.AdvertisedWindow
}

// AdvertisedWindow returns the capped client window, or zero when it is unknown.
func (r Resolver) AdvertisedWindow(id string) int64 { return r.cappedWindow(r.Resolve(id)) }

// CodexWindow returns the advertised window or the Codex fallback when unknown.
func (r Resolver) CodexWindow(id string) int64 {
	return cmp.Or(r.AdvertisedWindow(id), modelRows[0].AdvertisedWindow)
}
