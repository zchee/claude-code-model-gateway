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

// Package route classifies requests against one worker catalog generation.
package route

import (
	"encoding/json/jsontext"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

// Snapshot is the immutable routing and advertised model data of a completed refresh.
type Snapshot struct{}

// NewSnapshot freezes the resolved listing and live server measurements together with the catalog derived from them.
func NewSnapshot(resolver policy.Resolver, source string, catalog jsontext.Value, grokIDs []string) Snapshot {
	return Snapshot{}
}

// Resolver returns the generation's resolver without giving the caller mutable access to the snapshot.
func (Snapshot) Resolver() policy.Resolver { return policy.Resolver{} }

// Catalog returns the listing source and advertised rows, independent of caller mutations.
func (Snapshot) Catalog() (string, jsontext.Value) { return "", nil }

// Store publishes complete model generations. Its zero value has no snapshot.
type Store struct{}

// Publish replaces the visible generation only after its snapshot has been built.
func (*Store) Publish(snapshot Snapshot) {}

// Load returns one generation for the full lifetime of a request, or nil before publication.
func (*Store) Load() *Snapshot { return nil }

// ProxyID returns the upstream route for a proxy-served picker ID.
func (Snapshot) ProxyID(id string) (proxymodels.Route, bool) { return proxymodels.Route{}, false }

// CodexBase returns the Codex backend ID, or an empty string for an absent or unclaimed model.
func (Snapshot) CodexBase(id *string) string { return "" }

// IsGatewayID reports whether the proxy or the native Grok backend serves the picker ID.
func (Snapshot) IsGatewayID(id string) bool { return false }

// Decision identifies a local endpoint or upstream transport. Only proxy and Grok decisions carry a provider and backend ID.
type Decision struct{ Branch, Provider, Model string }

// Classify selects a handler for a non-draining worker without modifying the buffered body.
// Target is the original request target including any query. Headers do not select the route.
func (Snapshot) Classify(method, target string, body []byte) Decision { return Decision{} }
