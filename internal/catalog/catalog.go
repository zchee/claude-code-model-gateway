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

// Package catalog builds the model catalog consumed by Sidequest.
package catalog

import (
	"context"
	"time"
)

// Policy describes how the gateway serves and advertises an input model ID.
// A zero window means no window is advertised. An unresolved policy is the zero
// value: an eligible legacy ID keeps its original client spelling.
type Policy struct {
	BackendID, Provider, Transport, ClientID string
	Window                                   int
	WindowNote                               string
}

// Readiness is the public summary; diagnostic checks are not part of the catalog.
type Readiness struct {
	Ready   bool   `json:"ready"`
	State   string `json:"state"`
	Message string `json:"message"`
}

// Checks records the gateway checks that every proxy provider shares with Codex.
type Checks struct {
	ProxyBinary, ProxyModels, ShimRunning, ServingVersionMatches bool
}

// ReadinessSource provides observations not already supplied by the caller.
type ReadinessSource interface {
	// Probe reports provider-specific readiness. Grok does not share the proxy checks.
	Probe(ctx context.Context, provider string) Readiness
	// SharedFailureMessage describes a failed shared check using the caller's installation paths.
	SharedFailureMessage(state string) string
}

// Inputs supplies observed readiness, model policy and writer metadata without ambient reads.
// Providers overrides individual diagnoses; Codex and SharedChecks are the flat readiness input.
type Inputs struct {
	Policy       func(id string) Policy
	Providers    map[string]Readiness
	Codex        *Readiness
	SharedChecks *Checks
	Fallback     ReadinessSource
	UpdatedAt    time.Time
	Version      string
}

// Model is one discoverable routing choice.
type Model struct {
	Slug              string `json:"slug"`
	ID                string `json:"id"`
	Label             string `json:"label"`
	Provider          string `json:"provider"`
	ContextWindow     int    `json:"contextWindow,omitzero"`
	ContextWindowNote string `json:"contextWindowNote,omitzero"`
}

// Catalog is the provider-generic document read by Sidequest.
type Catalog struct {
	SchemaVersion int    `json:"schemaVersion"`
	Source        string `json:"source"`
	UpdatedAt     string `json:"updatedAt"`
	// WrittenBy is the port's non-empty writer version, never JSON null.
	WrittenBy string `json:"writtenBy"`
	// Providers publishes canonical JavaScript array-index keys first in ascending numeric order.
	// Other provider names follow in the order of their first retained row in Models.
	Providers      map[string]Readiness `json:"providers"`
	CodexReadiness *Readiness           `json:"codexReadiness"`
	Models         []Model              `json:"models"`
}

// Build constructs a catalog in discovery order, omitting dispatch and native models.
func Build(ctx context.Context, ids []string, inputs Inputs) Catalog {
	return Catalog{}
}
