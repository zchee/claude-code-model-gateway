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

package modelcache_test

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

var (
	defaultGrokListingRows = []modelRow{
		{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7", Type: "model", MaxInputTokens: 500000},
		{ID: "claude-grok-4.6[1m]", DisplayName: "Grok 4.6", Type: "model", MaxInputTokens: 500000},
		{ID: "claude-grok-4.5[1m]", DisplayName: "Grok 4.5", Type: "model", MaxInputTokens: 500000},
	}
	defaultGrokDiscoveryRows = []idDisplay{
		{ID: "claude-grok-4.7[1m]", DisplayName: "Grok 4.7"},
		{ID: "claude-grok-4.6[1m]", DisplayName: "Grok 4.6"},
		{ID: "claude-grok-4.5[1m]", DisplayName: "Grok 4.5"},
	}
)

func newProxyListingServer(t *testing.T, rows []idDisplay) *httptest.Server {
	t.Helper()
	payload, err := json.Marshal(struct {
		Data []idDisplay `json:"data"`
	}{Data: rows})
	if err != nil {
		t.Fatalf("marshal proxy listing fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func defaultGatewayWindows() policy.Windows {
	return policy.Windows{
		"codex": {Value: config.ContextWindow{Tokens: 272000}, Source: "default"},
		"grok":  {Value: config.ContextWindow{Full: true}, Source: "default"},
	}
}

func unmarshalModelsResponseRows(t *testing.T, raw jsontext.Value) []modelRow {
	t.Helper()
	var doc struct {
		Data []modelRow `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal /v1/models response: %v", err)
	}
	return doc.Data
}

func assertPublishedRowsBelowLimits(t *testing.T, surface string, rows []modelRow, limits map[string]int64) {
	t.Helper()
	if len(limits) == 0 {
		return
	}
	seen := make(map[string]bool, len(limits))
	for _, row := range rows {
		limit, ok := limits[row.ID]
		if !ok {
			continue
		}
		seen[row.ID] = true
		if row.MaxInputTokens >= limit {
			t.Errorf("%s row %q max_input_tokens = %d, want strictly below backend window %d", surface, row.ID, row.MaxInputTokens, limit)
		}
	}
	for id := range limits {
		if !seen[id] {
			t.Errorf("%s missing expected row %q for backend window bound check", surface, id)
		}
	}
}

func TestPublishedListingWindows(t *testing.T) {
	tests := map[string]struct {
		proxyRows []idDisplay
		wantRows  []modelRow
		maxBelow  map[string]int64
	}{
		"success: grok fast proxy variants advertise 500000": {
			proxyRows: []idDisplay{
				{ID: "grok-4.7-fast", DisplayName: "grok-4.7-fast (grok)"},
				{ID: "grok-4.7-build-fast", DisplayName: "grok-4.7-build-fast (grok)"},
			},
			wantRows: slices.Concat(defaultGrokListingRows, []modelRow{
				{ID: "claude-grok-4.7-fast[1m]", DisplayName: "Grok 4.7 fast", Type: "model", MaxInputTokens: 500000},
				{ID: "claude-grok-4.7-build-fast[1m]", DisplayName: "Grok 4.7 build fast", Type: "model", MaxInputTokens: 500000},
			}),
		},
		"success: astra fast advertises 272000": {
			proxyRows: []idDisplay{
				{ID: "gpt-6-astra-fast", DisplayName: "gpt-6-astra-fast (codex)"},
			},
			wantRows: slices.Concat([]modelRow{
				{ID: "claude-gpt-6-astra-fast[1m]", DisplayName: "GPT-6-astra-fast (Codex)", Type: "model", MaxInputTokens: 272000},
			}, defaultGrokListingRows),
		},
		"success: astra ultrafast advertises 272000": {
			proxyRows: []idDisplay{
				{ID: "gpt-6-astra-ultrafast", DisplayName: "gpt-6-astra-ultrafast (codex)"},
			},
			wantRows: slices.Concat([]modelRow{
				{ID: "claude-gpt-6-astra-ultrafast[1m]", DisplayName: "GPT-6-astra-ultrafast (Codex)", Type: "model", MaxInputTokens: 272000},
			}, defaultGrokListingRows),
		},
		"success: muse rows advertise 1000000 below backend window": {
			proxyRows: []idDisplay{
				{ID: "muse-spark-1.3", DisplayName: "muse-spark-1.3 (muse)"},
				{ID: "muse-spark-1.2", DisplayName: "muse-spark-1.2 (muse)"},
				{ID: "muse-wonder-1.0", DisplayName: "muse-wonder-1.0 (muse)"},
				{ID: "muse-wonder-full-1.0", DisplayName: "muse-wonder-full-1.0 (muse)"},
				{ID: "muse-ember-1.0", DisplayName: "muse-ember-1.0 (muse)"},
			},
			wantRows: slices.Concat(defaultGrokListingRows, []modelRow{
				{ID: "claude-muse-spark-1.3[1m]", DisplayName: "muse-spark-1.3 (Muse)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-muse-spark-1.2[1m]", DisplayName: "muse-spark-1.2 (Muse)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-muse-wonder-1.0[1m]", DisplayName: "muse-wonder-1.0 (Muse)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-muse-wonder-full-1.0[1m]", DisplayName: "muse-wonder-full-1.0 (Muse)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-muse-ember-1.0[1m]", DisplayName: "muse-ember-1.0 (Muse)", Type: "model", MaxInputTokens: 1000000},
			}),
			maxBelow: map[string]int64{
				"claude-muse-spark-1.3[1m]":       1007997,
				"claude-muse-spark-1.2[1m]":       1007997,
				"claude-muse-wonder-1.0[1m]":      1007997,
				"claude-muse-wonder-full-1.0[1m]": 1007997,
				"claude-muse-ember-1.0[1m]":       1007997,
			},
		},
		"success: measured vertex rows advertise documented windows": {
			proxyRows: []idDisplay{
				{ID: "gemini-4-argon", DisplayName: "gemini-4-argon (vertex)"},
				{ID: "vertex/gemini-3.5-flash-lite", DisplayName: "vertex/gemini-3.5-flash-lite (vertex)"},
				{ID: "gemini-4-hydrogen", DisplayName: "gemini-4-hydrogen (vertex)"},
				{ID: "vertex/gemini-4-neon", DisplayName: "vertex/gemini-4-neon (vertex)"},
				{ID: "vertex/anthropic/claude-fable-5-2", DisplayName: "vertex/anthropic/claude-fable-5-2 (vertex)"},
				{ID: "vertex/xai/grok-4.7", DisplayName: "vertex/xai/grok-4.7 (vertex)"},
				{ID: "vertex/xai/grok-4.20-non-reasoning", DisplayName: "vertex/xai/grok-4.20-non-reasoning (vertex)"},
				{ID: "vertex/openai/gpt-6.1-sol", DisplayName: "vertex/openai/gpt-6.1-sol (vertex)"},
				{ID: "vertex/meta/muse-spark-1.3", DisplayName: "vertex/meta/muse-spark-1.3 (vertex)"},
				{ID: "vertex/zai-org/glm-5.2-maas", DisplayName: "vertex/zai-org/glm-5.2-maas (vertex)"},
				{ID: "vertex/google/gemma-4-26b-a4b-it-maas", DisplayName: "vertex/google/gemma-4-26b-a4b-it-maas (vertex)"},
				{ID: "vertex/meta/llama-4-maverick-17b-128e-instruct-maas", DisplayName: "vertex/meta/llama-4-maverick-17b-128e-instruct-maas (vertex)"},
				{ID: "vertex/meta/llama-4-scout-17b-16e-instruct-maas", DisplayName: "vertex/meta/llama-4-scout-17b-16e-instruct-maas (vertex)"},
			},
			wantRows: slices.Concat(defaultGrokListingRows, []modelRow{
				{ID: "claude-gemini-4-argon[1m]", DisplayName: "gemini-4-argon (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-vertex/gemini-3.5-flash-lite[1m]", DisplayName: "vertex/gemini-3.5-flash-lite (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-gemini-4-hydrogen[1m]", DisplayName: "gemini-4-hydrogen (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-vertex/gemini-4-neon[1m]", DisplayName: "vertex/gemini-4-neon (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-vertex/anthropic/fable-5-2[1m]", DisplayName: "vertex/anthropic/claude-fable-5-2 (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-vertex/xai/grok-4.7[1m]", DisplayName: "vertex/xai/grok-4.7 (Vertex AI)", Type: "model", MaxInputTokens: 524288},
				{ID: "claude-vertex/xai/grok-4.20-non-reasoning[1m]", DisplayName: "vertex/xai/grok-4.20-non-reasoning (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-vertex/openai/gpt-6.1-sol[1m]", DisplayName: "vertex/openai/gpt-6.1-sol (Vertex AI)", Type: "model", MaxInputTokens: 922000},
				{ID: "claude-vertex/meta/muse-spark-1.3[1m]", DisplayName: "vertex/meta/muse-spark-1.3 (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-vertex/zai-org/glm-5.2-maas[1m]", DisplayName: "vertex/zai-org/glm-5.2-maas (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-vertex/google/gemma-4-26b-a4b-it-maas[1m]", DisplayName: "vertex/google/gemma-4-26b-a4b-it-maas (Vertex AI)", Type: "model", MaxInputTokens: 262144},
				{ID: "claude-vertex/meta/llama-4-maverick-17b-128e-instruct-maas[1m]", DisplayName: "vertex/meta/llama-4-maverick-17b-128e-instruct-maas (Vertex AI)", Type: "model", MaxInputTokens: 524288},
				{ID: "claude-vertex/meta/llama-4-scout-17b-16e-instruct-maas[1m]", DisplayName: "vertex/meta/llama-4-scout-17b-16e-instruct-maas (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
			}),
		},
		"success: unmeasured vertex rows advertise 200000": {
			proxyRows: []idDisplay{
				{ID: "vertex/anthropic/claude-opus-4-5@20251101", DisplayName: "vertex/anthropic/claude-opus-4-5@20251101 (vertex)"},
				{ID: "vertex/anthropic/claude-haiku-4-5@20251001", DisplayName: "vertex/anthropic/claude-haiku-4-5@20251001 (vertex)"},
				{ID: "vertex/xai/grok-4.3", DisplayName: "vertex/xai/grok-4.3 (vertex)"},
				{ID: "vertex/mistralai/mistral-medium-3", DisplayName: "vertex/mistralai/mistral-medium-3 (vertex)"},
				{ID: "vertex/mistralai/mistral-small-2503", DisplayName: "vertex/mistralai/mistral-small-2503 (vertex)"},
				{ID: "vertex/mistralai/codestral-2", DisplayName: "vertex/mistralai/codestral-2 (vertex)"},
				{ID: "vertex/openai/gpt-oss-120b-maas", DisplayName: "vertex/openai/gpt-oss-120b-maas (vertex)"},
				{ID: "vertex/deepseek-ai/deepseek-v4.1-flash-maas", DisplayName: "vertex/deepseek-ai/deepseek-v4.1-flash-maas (vertex)"},
				{ID: "vertex/endpoint/my-llm", DisplayName: "vertex/endpoint/my-llm (vertex)"},
			},
			wantRows: slices.Concat(defaultGrokListingRows, []modelRow{
				{ID: "claude-vertex/anthropic/claude-opus-4-5@20251101", DisplayName: "vertex/anthropic/claude-opus-4-5@20251101 (Vertex AI)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/anthropic/claude-haiku-4-5@20251001", DisplayName: "vertex/anthropic/claude-haiku-4-5@20251001 (Vertex AI)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/xai/grok-4.3", DisplayName: "vertex/xai/grok-4.3 (Vertex AI)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/mistralai/mistral-medium-3", DisplayName: "vertex/mistralai/mistral-medium-3 (Vertex AI)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/mistralai/mistral-small-2503", DisplayName: "vertex/mistralai/mistral-small-2503 (Vertex AI)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/mistralai/codestral-2", DisplayName: "vertex/mistralai/codestral-2 (Vertex AI)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/openai/gpt-oss-120b-maas", DisplayName: "vertex/openai/gpt-oss-120b-maas (Vertex AI)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/deepseek-ai/deepseek-v4.1-flash-maas", DisplayName: "vertex/deepseek-ai/deepseek-v4.1-flash-maas (Vertex AI)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/endpoint/my-llm", DisplayName: "vertex/endpoint/my-llm (Vertex AI)", Type: "model", MaxInputTokens: 200000},
			}),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			proxySrv := newProxyListingServer(t, tt.proxyRows)
			_, lookup, resolver := newTestPaths(t, nil)
			routes := &route.Store{}
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				Windows:        defaultGatewayWindows(),
				ProxyBaseURL:   proxySrv.URL,
				DefaultBaseURL: "http://127.0.0.1:19999",
				Client:         newTestClient(t),
				Routes:         routes,
				Sentry:         &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := cache.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			snapFromStore, ok := routes.Load()
			if !ok {
				t.Fatal("route.Store empty after Refresh")
			}
			snapFromCache, _ := cache.Snapshot()
			srcStore, rawStore := snapFromStore.Catalog()
			srcCache, rawCache := snapFromCache.Catalog()
			if srcStore != "proxy" || srcCache != "proxy" {
				t.Errorf("catalog sources = (store=%q, cache=%q), want proxy", srcStore, srcCache)
			}
			if !bytes.Equal(rawStore, rawCache) {
				t.Error("route.Store catalog differs from Cache.Snapshot catalog")
			}

			var snapRows []modelRow
			if err := json.Unmarshal(rawStore, &snapRows); err != nil {
				t.Fatalf("unmarshal snapshot catalog: %v", err)
			}
			if diff := gocmp.Diff(tt.wantRows, snapRows); diff != "" {
				t.Errorf("published snapshot catalog mismatch (-want +got):\n%s", diff)
			}
			assertPublishedRowsBelowLimits(t, "published snapshot catalog", snapRows, tt.maxBelow)

			rawResp, err := cache.ModelsResponse(t.Context(), http.Header{})
			if err != nil {
				t.Fatalf("ModelsResponse: %v", err)
			}
			respRows := unmarshalModelsResponseRows(t, rawResp)
			if diff := gocmp.Diff(tt.wantRows, respRows); diff != "" {
				t.Errorf("GET /v1/models rows mismatch (-want +got):\n%s", diff)
			}
			assertPublishedRowsBelowLimits(t, "GET /v1/models", respRows, tt.maxBelow)
		})
	}
}

func TestDiscoveryCacheRespellsProxyFable(t *testing.T) {
	const defaultURL = "http://127.0.0.1:19999"

	tests := map[string]struct {
		proxyRows           []idDisplay
		wantPublished       []modelRow
		wantDiscovery       []idDisplay
		forbidReservedFable bool
	}{
		"success: vertex fable row is respelled in listing and discovery cache": {
			proxyRows: []idDisplay{
				{ID: "vertex/anthropic/claude-fable-5-2", DisplayName: "vertex/anthropic/claude-fable-5-2 (vertex)"},
			},
			wantPublished: slices.Concat(defaultGrokListingRows, []modelRow{
				{ID: "claude-vertex/anthropic/fable-5-2[1m]", DisplayName: "vertex/anthropic/claude-fable-5-2 (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
			}),
			wantDiscovery: slices.Concat(defaultGrokDiscoveryRows, []idDisplay{
				{ID: "claude-vertex/anthropic/fable-5-2[1m]", DisplayName: "vertex/anthropic/claude-fable-5-2 (Vertex AI)"},
			}),
			forbidReservedFable: true,
		},
		"success: opencode fable and vertex opus rows preserve spelling rules in listing and discovery cache": {
			proxyRows: []idDisplay{
				{ID: "opencode-go/claude-fable-5", DisplayName: "opencode-go/claude-fable-5 (opencode)"},
				{ID: "vertex/anthropic/claude-opus-5-5", DisplayName: "vertex/anthropic/claude-opus-5-5 (vertex)"},
			},
			wantPublished: slices.Concat(defaultGrokListingRows, []modelRow{
				{ID: "claude-opencode-go/fable-5", DisplayName: "opencode-go/claude-fable-5 (OpenCode)", Type: "model", MaxInputTokens: 200000},
				{ID: "claude-vertex/anthropic/claude-opus-5-5[1m]", DisplayName: "vertex/anthropic/claude-opus-5-5 (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
			}),
			wantDiscovery: slices.Concat(defaultGrokDiscoveryRows, []idDisplay{
				{ID: "claude-opencode-go/fable-5", DisplayName: "opencode-go/claude-fable-5 (OpenCode)"},
				{ID: "claude-vertex/anthropic/claude-opus-5-5[1m]", DisplayName: "vertex/anthropic/claude-opus-5-5 (Vertex AI)"},
			}),
			forbidReservedFable: true,
		},
		"success: colliding alias spelling retains raw fable id to avoid duplicate picker ids": {
			proxyRows: []idDisplay{
				{ID: "vertex/anthropic/claude-fable-5-2", DisplayName: "vertex/anthropic/claude-fable-5-2 (vertex)"},
				{ID: "vertex/anthropic/fable-5-2", DisplayName: "vertex/anthropic/fable-5-2 (vertex)"},
			},
			wantPublished: slices.Concat(defaultGrokListingRows, []modelRow{
				{ID: "claude-vertex/anthropic/claude-fable-5-2[1m]", DisplayName: "vertex/anthropic/claude-fable-5-2 (Vertex AI)", Type: "model", MaxInputTokens: 1000000},
				{ID: "claude-vertex/anthropic/fable-5-2", DisplayName: "vertex/anthropic/fable-5-2 (Vertex AI)", Type: "model", MaxInputTokens: 200000},
			}),
			wantDiscovery: slices.Concat(defaultGrokDiscoveryRows, []idDisplay{
				{ID: "claude-vertex/anthropic/claude-fable-5-2[1m]", DisplayName: "vertex/anthropic/claude-fable-5-2 (Vertex AI)"},
				{ID: "claude-vertex/anthropic/fable-5-2", DisplayName: "vertex/anthropic/fable-5-2 (Vertex AI)"},
			}),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			proxySrv := newProxyListingServer(t, tt.proxyRows)
			home, lookup, resolver := newTestPaths(t, nil)
			routes := &route.Store{}
			nowMs := int64(1700000000000)

			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				Windows:          defaultGatewayWindows(),
				ProxyBaseURL:     proxySrv.URL,
				DefaultBaseURL:   defaultURL,
				Client:           newTestClient(t),
				Clock:            func() time.Time { return time.UnixMilli(nowMs) },
				Routes:           routes,
				Sentry:           &sentry.State{},
				EffectiveBaseURL: baseURLFunc(defaultURL),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := cache.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			snap, ok := routes.Load()
			if !ok {
				t.Fatal("route.Store empty after Refresh")
			}
			_, rawCatalog := snap.Catalog()
			var publishedRows []modelRow
			if err := json.Unmarshal(rawCatalog, &publishedRows); err != nil {
				t.Fatalf("unmarshal published catalog: %v", err)
			}
			if diff := gocmp.Diff(tt.wantPublished, publishedRows); diff != "" {
				t.Errorf("published catalog mismatch (-want +got):\n%s", diff)
			}

			rawResp, err := cache.ModelsResponse(t.Context(), http.Header{})
			if err != nil {
				t.Fatalf("ModelsResponse: %v", err)
			}
			respRows := unmarshalModelsResponseRows(t, rawResp)
			if diff := gocmp.Diff(tt.wantPublished, respRows); diff != "" {
				t.Errorf("GET /v1/models rows mismatch (-want +got):\n%s", diff)
			}

			cachePath := filepath.Join(home, ".claude", "cache", "gateway-models.json")
			readDiscovery := func(stage string, wantFetchedAt int64) []idDisplay {
				t.Helper()
				raw, err := os.ReadFile(cachePath)
				if err != nil {
					t.Fatalf("%s: read discovery cache: %v", stage, err)
				}
				var keys map[string]jsontext.Value
				if err := json.Unmarshal(raw, &keys); err != nil {
					t.Fatalf("%s: unmarshal discovery cache keys: %v", stage, err)
				}
				if diff := gocmp.Diff([]string{"baseUrl", "fetchedAt", "models"}, slices.Sorted(maps.Keys(keys))); diff != "" {
					t.Errorf("%s: discovery cache keys mismatch (-want +got):\n%s", stage, diff)
				}
				var doc struct {
					BaseURL   string      `json:"baseUrl"`
					FetchedAt int64       `json:"fetchedAt"`
					Models    []idDisplay `json:"models"`
				}
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatalf("%s: unmarshal discovery cache: %v", stage, err)
				}
				if doc.BaseURL != defaultURL || doc.FetchedAt != wantFetchedAt {
					t.Errorf("%s: discovery header = (%q, %d), want (%q, %d)", stage, doc.BaseURL, doc.FetchedAt, defaultURL, wantFetchedAt)
				}
				if diff := gocmp.Diff(tt.wantDiscovery, doc.Models); diff != "" {
					t.Errorf("%s: discovery cache models mismatch (-want +got):\n%s", stage, diff)
				}
				return doc.Models
			}

			persistedInitial := readDiscovery("after Refresh", 1700000000000)

			nowMs = 1700000005000
			forced, err := cache.SyncDiscoveryCache(true)
			if err != nil {
				t.Fatalf("SyncDiscoveryCache(true): %v", err)
			}
			if forced.State != "wrote" {
				t.Errorf("SyncDiscoveryCache(true) state = %q, want %q", forced.State, "wrote")
			}
			persistedForced := readDiscovery("after SyncDiscoveryCache(true)", 1700000005000)

			if tt.forbidReservedFable {
				for _, row := range publishedRows {
					if strings.Contains(row.ID, "claude-fable-") {
						t.Errorf("published catalog row %q retains reserved claude-fable- spelling", row.ID)
					}
				}
				for _, row := range respRows {
					if strings.Contains(row.ID, "claude-fable-") {
						t.Errorf("GET /v1/models row %q retains reserved claude-fable- spelling", row.ID)
					}
				}
				for _, row := range persistedInitial {
					if strings.Contains(row.ID, "claude-fable-") {
						t.Errorf("persisted discovery row %q after Refresh retains reserved claude-fable- spelling", row.ID)
					}
				}
				for _, row := range persistedForced {
					if strings.Contains(row.ID, "claude-fable-") {
						t.Errorf("persisted discovery row %q after SyncDiscoveryCache retains reserved claude-fable- spelling", row.ID)
					}
				}
			}
		})
	}
}
