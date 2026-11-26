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
	"slices"
	"strings"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

// Picker spellings shared by the routing rules (lib/runtime.js:27-30, lib/proxy-models.js:7).
const (
	pickerPrefix       = "claude-"
	grokPrefix         = "claude-grok-"
	legacyCodexPrefix  = "claude-codex-"
	dispatchModelID    = "claude-codex-auto"
	codexFamilyPrefix  = "gpt-"
	extendedSuffix     = "[1m]"
	grokBackendIDStart = "grok-"
)

// Snapshot is the immutable routing and advertised model data of a completed refresh.
type Snapshot struct {
	resolver policy.Resolver
	source   string
	catalog  jsontext.Value
	grokIDs  []string
}

// NewSnapshot freezes the resolved listing and live server measurements together with the catalog derived from them.
// The catalog and Grok IDs are copied, so later changes by the caller do not reach the snapshot. A Grok ID that is
// not well-formed UTF-8 is dropped rather than compared, as the listing parser drops such rows.
func NewSnapshot(resolver policy.Resolver, source string, catalog jsontext.Value, grokIDs []string) Snapshot {
	ids := slices.DeleteFunc(slices.Clone(grokIDs), func(id string) bool { return !utf8.ValidString(id) })
	return Snapshot{resolver: resolver, source: source, catalog: catalog.Clone(), grokIDs: ids}
}

// Resolver returns the generation's resolver without giving the caller mutable access to the snapshot.
func (s Snapshot) Resolver() policy.Resolver { return s.resolver }

// Catalog returns the listing source and advertised rows, independent of caller mutations.
func (s Snapshot) Catalog() (string, jsontext.Value) { return s.source, s.catalog.Clone() }

// Store publishes complete model generations. Its zero value has no snapshot.
type Store struct{ current atomic.Pointer[Snapshot] }

// Publish replaces the visible generation only after its snapshot has been built.
func (s *Store) Publish(snapshot Snapshot) { s.current.Store(&snapshot) }

// Load returns a snapshot value for the lifetime of a request, or false before publication.
func (s *Store) Load() (Snapshot, bool) {
	if current := s.current.Load(); current != nil {
		return *current, true
	}
	return Snapshot{}, false
}

// ProxyID returns the upstream route for a proxy-served picker ID (lib/pins.js:17-28).
func (s Snapshot) ProxyID(id string) (proxymodels.Route, bool) {
	bare := strings.TrimSuffix(id, extendedSuffix)
	if bare == dispatchModelID {
		return proxymodels.Route{ID: "auto", Provider: "codex"}, true
	}
	if base, ok := strings.CutPrefix(bare, legacyCodexPrefix); ok {
		if !strings.HasPrefix(base, codexFamilyPrefix) {
			return proxymodels.Route{}, false
		}
		return proxymodels.Route{ID: base, Provider: "codex"}, true
	}
	if !strings.HasPrefix(bare, pickerPrefix) {
		return proxymodels.Route{}, false
	}
	m := s.resolver.Resolve(bare)
	if m == nil || m.Transport != "proxy" {
		return proxymodels.Route{}, false
	}
	return proxymodels.Route{ID: m.BackendID, Provider: m.Backend}, true
}

// CodexBase returns the Codex backend ID, or an empty string for an absent or unclaimed model (lib/pins.js:30-33).
func (s Snapshot) CodexBase(id *string) string {
	if id == nil {
		return ""
	}
	if r, ok := s.ProxyID(*id); ok && r.Provider == "codex" {
		return r.ID
	}
	return ""
}

// IsGatewayID reports whether the proxy or the native Grok backend serves the picker ID (lib/pins.js:54-56).
func (s Snapshot) IsGatewayID(id string) bool {
	_, ok := s.ProxyID(id)
	return ok || strings.HasPrefix(id, grokPrefix)
}

// Decision identifies a local endpoint or upstream transport. Only proxy and Grok decisions carry a provider and backend ID.
type Decision struct{ Branch, Provider, Model string }

// Classify selects a handler for a non-draining worker without modifying the buffered body.
// Target is the original request target including any query. Headers do not select the route.
//
// The order follows lib/request-worker.js:2361-2416 and 2430-2648: the drain endpoint, the health endpoint for any
// method, the catalog, any HEAD request, then the body of a request whose path starts with /v1/messages. A body
// that does not parse as exactly one JSON document, including one nested deeper than the wire decoder accepts,
// takes the source's catch path to Anthropic, as does a body whose top-level value is null.
func (s Snapshot) Classify(method, target string, body []byte) Decision {
	path, _, _ := strings.Cut(target, "?")
	switch {
	case method == "POST" && path == "/drain":
		return Decision{Branch: "drain"}
	case path == "/healthz":
		return Decision{Branch: "health"}
	case method == "GET" && path == "/v1/models":
		return Decision{Branch: "models"}
	case method == "HEAD":
		return Decision{Branch: "head"}
	}
	if len(body) == 0 || !strings.HasPrefix(path, "/v1/messages") {
		return Decision{Branch: "anthropic"}
	}
	model, ok := requestedModel(body)
	if !ok {
		return Decision{Branch: "anthropic"}
	}
	// The proxy branch is taken only for a truthy base (lib/request-worker.js:2444).
	if r, ok := s.ProxyID(model); ok && r.ID != "" {
		return Decision{Branch: "proxy", Provider: r.Provider, Model: r.ID}
	}
	if picker, ok := strings.CutPrefix(model, grokPrefix); ok {
		return Decision{Branch: "grok", Provider: "grok", Model: s.grokModel(strings.TrimSuffix(picker, extendedSuffix))}
	}
	return Decision{Branch: "anthropic"}
}

// requestedModel returns the string model member of a JSON object body. It reports false when the body does not
// parse, when the model is absent or not a string, and when the model holds an unpaired UTF-16 surrogate: such a
// spelling cannot become a Go string without U+FFFD standing in for the surrogate and merging distinct IDs, so the
// request is passed through to Anthropic with its original bytes instead.
func requestedModel(body []byte) (string, bool) {
	v, err := jsonx.DecodeWire(body)
	if err != nil || v.Kind != '{' {
		return "", false
	}
	for _, m := range v.Members {
		if !unitsEqual(m.Name, "model") {
			continue
		}
		if m.Value.Kind != '"' {
			return "", false
		}
		return wellFormed(m.Value.Text)
	}
	return "", false
}

// grokModel maps a picker part to the first snapshot Grok ID with the same dot-flattened spelling, else to
// grok-<picker> (lib/grok-backend.js:413-423). The source's fallback for an empty cache
// (lib/request-worker.js:2603) passes the default model objects, whose string form never equals a picker part
// built from an ID, so an empty list behaves as a list without a match. The source returns the first match only
// when it is a non-empty string (match || ...), so a matching empty ID also yields grok-<picker>.
func (s Snapshot) grokModel(picker string) string {
	flattened := strings.ReplaceAll(picker, ".", "-")
	for _, id := range s.grokIDs {
		if strings.ReplaceAll(strings.TrimPrefix(id, grokBackendIDStart), ".", "-") == flattened {
			if id == "" {
				break
			}
			return id
		}
	}
	return grokBackendIDStart + picker
}

// unitsEqual reports whether UTF-16 units spell the ASCII string s.
func unitsEqual(units []uint16, s string) bool {
	if len(units) != len(s) {
		return false
	}
	for i, u := range units {
		if u != uint16(s[i]) {
			return false
		}
	}
	return true
}

// wellFormed converts UTF-16 units to a string, reporting false for an unpaired surrogate.
func wellFormed(units []uint16) (string, bool) {
	var b strings.Builder
	b.Grow(len(units))
	for i := 0; i < len(units); i++ {
		r := rune(units[i])
		if utf16.IsSurrogate(r) {
			if i+1 == len(units) {
				return "", false
			}
			if r = utf16.DecodeRune(r, rune(units[i+1])); r == utf8.RuneError {
				return "", false
			}
			i++
		}
		b.WriteRune(r)
	}
	return b.String(), true
}
