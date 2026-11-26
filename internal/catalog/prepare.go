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

package catalog

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

// Prepared holds a snapshot of validated models and supplied readiness observations.
type Prepared struct {
	models []Model
	inputs Inputs
}

// Prepare resolves model policy and validates labels and window notes before a refresh can build a catalog.
// It ignores inputs.Policy and returns no prepared catalog on failure.
func Prepare(ids []string, inputs Inputs, modelInputs policy.ResolverInputs, lookup config.LookupEnv) (*Prepared, error) {
	inputs.Policy = nil
	inputs.Providers = maps.Clone(inputs.Providers)
	if inputs.Codex != nil {
		inputs.Codex = new(*inputs.Codex)
	}
	if inputs.SharedChecks != nil {
		inputs.SharedChecks = new(*inputs.SharedChecks)
	}
	modelInputs.Windows = maps.Clone(modelInputs.Windows)
	resolver := policy.NewResolver(modelInputs)
	observe := func(id string) Policy {
		p := Policy{ClientID: resolver.ClientID(id), Window: int(resolver.AdvertisedWindow(id))}
		// Route membership resolves the bare spelling; the client ID and window resolve the original.
		if routing := resolver.Resolve(strings.TrimSuffix(id, longWindow)); routing != nil {
			p.BackendID, p.Provider, p.Transport = routing.BackendID, routing.Backend, routing.Transport
		}
		return p
	}
	note := func(id, provider string) (string, error) {
		windows := modelInputs.Windows
		return policy.CheckedWindowNote(provider, windows.Cap(provider), windows[provider].CompactAt, resolver.Resolve(id), lookup)
	}
	models, err := prepareModels(ids, observe, note)
	if err != nil {
		return nil, err
	}
	return &Prepared{models: models, inputs: inputs}, nil
}

// Build constructs a fresh catalog from the validated observations, probing fallback readiness again on each call.
// Concurrent builds require a concurrency-safe injected fallback.
func (p *Prepared) Build(ctx context.Context) Catalog {
	return build(ctx, p.inputs, slices.Clone(p.models))
}
