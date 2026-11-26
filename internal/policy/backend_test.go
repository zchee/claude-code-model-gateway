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

package policy_test

import (
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

func TestResolveBackend(t *testing.T) {
	const backend = "claude-fable-live"
	routes := proxymodels.NewTable([]proxymodels.Route{{ID: backend, Provider: "local"}})
	r := policy.NewResolver(policy.ResolverInputs{Routes: routes, LocalWindow: 60000})
	tests := map[string]struct{ id, want string }{
		"success: exact listed key":  {backend, backend},
		"error: advertised alias":    {"fable-live", ""},
		"error: extra picker prefix": {"claude-" + backend, ""},
		"error: extra window suffix": {backend + "[1m]", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := r.ResolveBackend(tt.id)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("unexpected policy for %q: %+v", tt.id, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("missing policy for %q", tt.id)
			}
			if diff := gocmp.Diff(tt.want, got.BackendID); diff != "" {
				t.Fatal(diff)
			}
			want := *got
			got.BackendWindow = 1
			if diff := gocmp.Diff(&want, r.ResolveBackend(tt.id)); diff != "" {
				t.Errorf("backend policy changed through caller's copy (-want +got): %s", diff)
			}
		})
	}
}
