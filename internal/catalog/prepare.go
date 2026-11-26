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
	"regexp"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
)

// Prepared binds the discovered IDs to their validated policy observations.
type Prepared struct {
	ids    []string
	inputs Inputs
}

// Prepare resolves model policy and validates labels and window notes before a refresh can build a catalog.
// It replaces inputs.Policy with resolved observations and returns no prepared catalog on failure.
func Prepare(ids []string, inputs Inputs, modelInputs policy.ResolverInputs, lookup config.LookupEnv) (*Prepared, error) {
	resolver := policy.NewResolver(modelInputs)
	pattern := regexp.MustCompile(codexLabelPattern)
	policies := make(map[string]Policy, len(ids))
	retained := make([]string, 0, len(ids))
	for _, id := range ids {
		if !candidate(id) {
			continue
		}
		if _, known := policies[id]; known {
			retained = append(retained, id)
			continue
		}
		p := Policy{ClientID: resolver.ClientID(id)}
		m := resolver.Resolve(id)
		routing := m
		// Route membership resolves the bare spelling; the client ID and window resolve the original.
		if bare, ok := strings.CutSuffix(id, longWindow); ok {
			routing = resolver.Resolve(bare)
		}
		if routing != nil {
			p.BackendID, p.Provider, p.Transport = routing.BackendID, routing.Backend, routing.Transport
		}
		provider, base := route(id, p)
		if provider == "" {
			continue
		}
		if provider == "codex" {
			if _, err := modelLabel(pattern, provider, base); err != nil {
				return nil, err
			}
		}
		retained = append(retained, id)
		policies[id] = p
	}
	// The source resolves all labels before formatting the first window note.
	windows := modelInputs.Windows
	for _, id := range retained {
		p := policies[id]
		p.Window = int(resolver.AdvertisedWindow(id))
		if p.Window != 0 {
			provider, _ := route(id, p)
			note, err := policy.CheckedWindowNote(provider, windows.Cap(provider), windows[provider].CompactAt, resolver.Resolve(id), lookup)
			if err != nil {
				return nil, err
			}
			p.WindowNote = note
		}
		policies[id] = p
	}
	inputs.Policy = func(id string) Policy { return policies[id] }
	return &Prepared{ids: retained, inputs: inputs}, nil
}

// Build constructs the catalog from the observations validated by Prepare.
func (p *Prepared) Build(ctx context.Context) Catalog { return Build(ctx, p.ids, p.inputs) }
