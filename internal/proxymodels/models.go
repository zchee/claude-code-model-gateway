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

import "encoding/json/jsontext"

// Route identifies a backend model and the provider that serves it.
type Route struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

// ParseListing parses the data member of a proxy listing in discovery order.
func ParseListing(rows jsontext.Value) []Route { return nil }

// ModelsFileRoutes reads Codex routes from a legacy JSON array of model IDs.
func ModelsFileRoutes(value jsontext.Value) []Route { return nil }

// ProviderLabel returns the display label for a provider.
func ProviderLabel(provider string) string { return "" }

// DisplayName formats the backend ID with its provider label.
func DisplayName(id, provider string) string { return "" }

// Table is an immutable snapshot of a listing. Its zero value has no listed routes.
type Table struct{}

// NewTable indexes routes by backend ID, retaining their discovery order.
func NewTable(routes []Route) Table { return Table{} }

// Routes returns the listed backend IDs with their current providers.
func (Table) Routes() []Route { return nil }

// Provider returns the provider of a backend ID, or the empty string if unknown.
func (Table) Provider(id string) string { return "" }

// AdvertisedPart returns the picker spelling of a backend ID, without the gateway prefix.
func (Table) AdvertisedPart(id string) string { return "" }

// RouteIDForAdvertisedPart resolves a renamed picker spelling, not a listed ID.
func (Table) RouteIDForAdvertisedPart(part string) (string, bool) { return "", false }

// AdvertisedRoutes returns the listed routes to show in the picker, in discovery order.
func AdvertisedRoutes(routes []Route) []Route { return nil }
