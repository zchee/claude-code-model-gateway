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

package catalog_test

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

func TestVertexNotConfiguredWithoutProject(t *testing.T) {
	listing := jsontext.Value(`[
		{"id":"gpt-6-sol","display_name":"gpt-6-sol (codex)"},
		{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},
		{"id":"gemini-4-argon","display_name":"gemini-4-argon (vertex)"},
		{"id":"vertex/gemini-4-argon","display_name":"vertex/gemini-4-argon (vertex)"}
	]`)
	routes := proxymodels.NewTable(proxymodels.ParseListing(listing))
	configDir := t.TempDir()
	lookup := config.LookupEnv(func(key string) (string, bool) {
		if key == "CCP_CONFIG_DIR" {
			return configDir, true
		}
		return "", false
	})

	var statusCalls atomic.Int32
	source := catalog.NewLiveReadinessSource(catalog.LiveReadinessOptions{
		Lookup:      lookup,
		ConfigDir:   configDir,
		ProxyBinary: filepath.Join(t.TempDir(), "missing-claude-code-proxy"),
		CommandPath: filepath.Join(t.TempDir(), "model-gateway"),
		RunStatus: func(context.Context, string, []string) (int, error) {
			statusCalls.Add(1)
			return 0, errors.New("status process must not run without a Vertex project")
		},
	})

	inputs := catalog.Inputs{
		Codex:        &catalog.Readiness{Ready: true, State: "ready", Message: "ready"},
		SharedChecks: &catalog.Checks{ProxyBinary: true, ProxyModels: true, ShimRunning: true, ServingVersionMatches: true},
		Fallback:     source,
		UpdatedAt:    time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC),
		Version:      "0.57.0",
	}
	prepared, err := catalog.Prepare([]string{"claude-gemini-4-argon[1m]"}, inputs, policy.ResolverInputs{Routes: routes}, lookup)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	doc := prepared.Build(t.Context())

	if got := statusCalls.Load(); got != 0 {
		t.Errorf("Vertex status subprocess calls = %d, want 0", got)
	}

	t.Run("models", func(t *testing.T) {
		type projection struct {
			ID            string
			Provider      string
			ContextWindow int
		}
		got := make([]projection, 0, len(doc.Models))
		for _, m := range doc.Models {
			got = append(got, projection{ID: m.ID, Provider: m.Provider, ContextWindow: m.ContextWindow})
		}
		want := []projection{{ID: "claude-gemini-4-argon[1m]", Provider: "vertex", ContextWindow: 1000000}}
		if diff := gocmp.Diff(want, got); diff != "" {
			t.Errorf("models mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("vertex_readiness", func(t *testing.T) {
		vertex, ok := doc.Providers["vertex"]
		if !ok {
			t.Fatalf("vertex readiness missing from providers: %+v", doc.Providers)
		}
		if vertex.Ready {
			t.Error("vertex Ready = true, want false")
		}
		if vertex.State != "config-invalid" {
			t.Errorf("vertex State = %q, want %q", vertex.State, "config-invalid")
		}
		wantMsg := regexp.MustCompile(`missing or invalid Vertex AI setting, most often an unset Google Cloud project`)
		if !wantMsg.MatchString(vertex.Message) {
			t.Errorf("vertex Message = %q, does not match %s", vertex.Message, wantMsg)
		}
	})
}
