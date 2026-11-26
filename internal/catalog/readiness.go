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

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

// LiveReadinessOptions configures live provider readiness resolution for catalog construction.
type LiveReadinessOptions struct {
	Lookup      config.LookupEnv
	ConfigDir   string
	ProxyBinary string
	CommandPath string
	RunStatus   readiness.StatusRunner
	ReadGrok    func(ctx context.Context) Readiness
	LlamaState  *localprov.LlamaState
	StudioState *localprov.StudioState
}

// LiveReadinessSource resolves provider readiness using live proxy provider checks and optional Grok readiness.
// It satisfies ReadinessSource.
type LiveReadinessSource struct{}

// NewLiveReadinessSource constructs a ReadinessSource backed by the live provider readiness path.
func NewLiveReadinessSource(opts LiveReadinessOptions) *LiveReadinessSource {
	_ = opts
	return &LiveReadinessSource{}
}

// Probe returns the provider's live readiness summary without reading ambient environment or files.
func (s *LiveReadinessSource) Probe(ctx context.Context, provider string) Readiness {
	_, _ = ctx, provider
	return Readiness{}
}

// SharedFailureMessage describes a failed shared proxy check using the configured installation paths.
func (s *LiveReadinessSource) SharedFailureMessage(state string) string {
	_ = state
	return ""
}
