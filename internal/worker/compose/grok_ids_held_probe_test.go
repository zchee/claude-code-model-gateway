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

package compose_test

import (
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
)

// TestGrokIDsSampledBeforeHeldProbe decides that a model refresh reads the Grok model IDs before it
// probes the local engines, and publishes that sample. The saved Grok IDs are grok-4.7 when the
// worker starts, grok-4.6 when the refresh starts, and grok-4.5 from the moment the refresh's LM
// Studio probe is held until the refresh commits. A refresh that read the IDs after its probes would
// publish Grok 4.5; one that did not commit would keep Grok 4.7.
func TestGrokIDsSampledBeforeHeldProbe(t *testing.T) {
	var armed atomic.Bool
	entered := make(chan struct{})
	enteredOnce := sync.OnceFunc(func() { close(entered) })
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	var probes atomic.Int32
	studio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		probes.Add(1)
		if armed.Load() {
			enteredOnce()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(studio.Close)

	h := newHarness(t, harnessConfig{
		env:          map[string]string{"CCP_LMSTUDIO_BASE_URL": studio.URL},
		grokCacheIDs: []string{"grok-4.7"},
	})
	// Registered after the harness, so it runs before the harness and the LM Studio fixture close.
	t.Cleanup(releaseOnce)
	mustDiff(t, "LM Studio probes while the worker starts", int32(1), probes.Load())
	mustDiff(t, "Grok rows published when the worker starts", []string{"Grok 4.7"}, grokHeldProbeRows(t, h))

	grokHeldProbeWriteIDs(t, h.grokCachePath, "grok-4.6")
	armed.Store(true)
	refreshed := make(chan error, 1)
	go func() { refreshed <- h.modelCache.Refresh(h.serviceCtx) }()
	waitSignal(t, "the held LM Studio probe", entered, 5*time.Second)

	grokHeldProbeWriteIDs(t, h.grokCachePath, "grok-4.5")
	// A reader of the saved IDs sees the change while the probe is held.
	mustDiff(t, "saved Grok IDs during the held probe", []string{"grok-4.5"}, compose.NewGrokModelIDs(h.grokCachePath)())
	mustDiff(t, "Grok rows during the held probe", []string{"Grok 4.7"}, grokHeldProbeRows(t, h))

	releaseOnce()
	select {
	case err := <-refreshed:
		if err != nil {
			t.Fatalf("modelcache.Refresh: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the refresh")
	}
	mustDiff(t, "LM Studio probes after the refresh", int32(2), probes.Load())
	mustDiff(t, "Grok rows published by the refresh", []string{"Grok 4.6"}, grokHeldProbeRows(t, h))
}

// grokHeldProbeWriteIDs replaces the saved Grok model cache with ids, in the harness's file format.
func grokHeldProbeWriteIDs(t *testing.T, path string, ids ...string) {
	t.Helper()
	rows := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, map[string]any{"id": id, "context_window": 256000})
	}
	raw, err := json.Marshal(map[string]any{"models": rows})
	if err != nil {
		t.Fatalf("marshal grok models cache: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write grok models cache: %v", err)
	}
}

// grokHeldProbeRows returns the display names of the Grok rows of the composed GET /v1/models.
func grokHeldProbeRows(t *testing.T, h *harness) []string {
	t.Helper()
	status, _, raw := h.do(http.MethodGet, "/v1/models", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d", status)
	}
	var doc struct {
		Data []struct {
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	var names []string
	for _, row := range doc.Data {
		if strings.HasPrefix(row.DisplayName, "Grok ") {
			names = append(names, row.DisplayName)
		}
	}
	return names
}
