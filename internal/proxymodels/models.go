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

// Package proxymodels interprets the model listings served by the proxy.
package proxymodels

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Route identifies a backend model and the provider that serves it.
type Route struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// ParseListing parses the data member of a proxy listing in discovery order.
func ParseListing(rows jsontext.Value) []Route {
	var entries []jsontext.Value
	if err := json.Unmarshal(rows, &entries, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
		return []Route{}
	}
	routes := make([]Route, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var row struct {
			ID          jsontext.Value `json:"id"`
			DisplayName jsontext.Value `json:"display_name"`
		}
		if err := json.Unmarshal(entry, &row, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
			continue
		}
		var id string
		if err := json.Unmarshal(row.ID, &id, jsontext.AllowInvalidUTF8(true)); err != nil || id == "" {
			continue
		}
		switch id {
		case "haiku", "sonnet", "opus", "fable":
			continue
		}
		if strings.HasPrefix(id, "claude-") || strings.HasPrefix(id, "anthropic-") {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		var displayName string
		_ = json.Unmarshal(row.DisplayName, &displayName, jsontext.AllowInvalidUTF8(true))
		provider := listedProvider(id, displayName)
		if provider == "" {
			if !strings.HasPrefix(id, "gpt-") {
				continue
			}
			provider = "codex"
		}
		seen[id] = struct{}{}
		routes = append(routes, Route{ID: id, Provider: provider})
	}
	return routes
}

func listedProvider(id, displayName string) string {
	model, suffix, ok := strings.CutLast(displayName, " (")
	// JavaScript's dot excludes line feed, carriage return, and Unicode line and paragraph separators.
	const lineTerminators = "\n\r\U00002028\U00002029"
	if !ok || model != id || strings.ContainsAny(id, lineTerminators) {
		return ""
	}
	provider, ok := strings.CutSuffix(suffix, ")")
	if !ok || provider == "" || provider[0] == '-' {
		return ""
	}
	for i := range len(provider) {
		c := provider[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return ""
		}
	}
	return provider
}

// ModelsFileRoutes reads Codex routes from a legacy JSON array of model IDs.
func ModelsFileRoutes(value jsontext.Value) []Route {
	var entries []jsontext.Value
	if err := json.Unmarshal(value, &entries, jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)); err != nil {
		return []Route{}
	}
	routes := make([]Route, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var id string
		if err := json.Unmarshal(entry, &id, jsontext.AllowInvalidUTF8(true)); err != nil || !strings.HasPrefix(id, "gpt-") {
			continue
		}
		if _, duplicate := seen[id]; !duplicate {
			seen[id] = struct{}{}
			routes = append(routes, Route{ID: id, Provider: "codex"})
		}
	}
	return routes
}

// ProviderLabel returns the display label for a provider.
func ProviderLabel(provider string) string {
	switch provider {
	case "":
		return "Proxy"
	case "lmstudio":
		return "LM Studio"
	case "local":
		return "Local GGUF"
	case "opencode":
		return "OpenCode"
	case "vertex":
		return "Vertex AI"
	}
	_, size := utf8.DecodeRuneInString(provider)
	// The source uppercases one UTF-16 code unit, not a surrogate pair.
	if size == utf8.UTFMax {
		return provider
	}
	return cases.Upper(language.Und).String(provider[:size]) + provider[size:]
}

// DisplayName formats the backend ID with its provider label.
func DisplayName(id, provider string) string { return id + " (" + ProviderLabel(provider) + ")" }

// Table is an immutable snapshot of a listing. Its zero value has no listed routes.
type Table struct {
	routes    []Route
	positions map[string]int
	aliases   map[string]string
}

// NewTable indexes routes by backend ID, retaining their discovery order.
func NewTable(routes []Route) Table {
	t := Table{
		routes:    make([]Route, 0, len(routes)),
		positions: make(map[string]int, len(routes)),
	}
	for _, route := range routes {
		if pos, ok := t.positions[route.ID]; ok {
			t.routes[pos] = route
			continue
		}
		t.positions[route.ID] = len(t.routes)
		t.routes = append(t.routes, route)
	}
	for _, route := range t.routes {
		if part := t.AdvertisedPart(route.ID); part != route.ID {
			if t.aliases == nil {
				t.aliases = make(map[string]string)
			}
			t.aliases[part] = route.ID
		}
	}
	return t
}

// Routes returns the listed backend IDs with their current providers.
func (t Table) Routes() []Route { return append([]Route{}, t.routes...) }

// Provider returns Codex ("codex") for any gpt-* ID, even if unlisted or listed under another provider.
// For other IDs it returns the listed provider, or the empty string if unknown.
func (t Table) Provider(id string) string {
	if strings.HasPrefix(id, "gpt-") {
		return "codex"
	}
	if pos, ok := t.positions[id]; ok && id != "" {
		return t.routes[pos].Provider
	}
	return ""
}

// AdvertisedPart returns the picker spelling of a backend ID, without the gateway prefix.
func (t Table) AdvertisedPart(id string) string {
	part := strings.ReplaceAll(id, "claude-fable-", "fable-")
	if _, listed := t.positions[part]; listed {
		return id
	}
	return part
}

// RouteIDForAdvertisedPart resolves a renamed picker spelling, not a listed ID.
func (t Table) RouteIDForAdvertisedPart(part string) (string, bool) {
	// NewTable only adds aliases that are not themselves listed IDs.
	id, ok := t.aliases[part]
	return id, ok
}

// AdvertisedRoutes returns the listed routes to show in the picker, in discovery order.
func AdvertisedRoutes(routes []Route) []Route {
	vertexIDs := make(map[string]struct{})
	for _, route := range routes {
		if route.Provider == "vertex" {
			vertexIDs[route.ID] = struct{}{}
		}
	}
	advertised := make([]Route, 0, len(routes))
	for _, route := range routes {
		if route.Provider == "vertex" {
			if bare, prefixed := strings.CutPrefix(route.ID, "vertex/"); prefixed {
				if _, found := vertexIDs[bare]; found {
					continue
				}
			}
		}
		advertised = append(advertised, route)
	}
	return advertised
}
