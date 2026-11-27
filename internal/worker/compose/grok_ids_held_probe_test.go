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
	"context"
	json "encoding/json/v2"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const (
	// grokHeldProbeStudioHost and grokHeldProbeLlamaHost are the addresses the two local-engine probes
	// are configured with; grokHeldProbeTransport answers both itself, so neither is dialled.
	grokHeldProbeStudioHost = "127.0.0.1:1"
	grokHeldProbeLlamaHost  = "127.0.0.1:2"
	grokHeldProbeListing    = `{"data":[` +
		`{"id":"gpt-6.1-sol","display_name":"gpt-6.1-sol (codex)"},` +
		`{"id":"lmstudio/ibm/granite-4-micro","display_name":"lmstudio/ibm/granite-4-micro (lmstudio)"},` +
		`{"id":"local/gguf","display_name":"local/gguf (local)"}` +
		`]}`
)

// grokHeldProbeTransport answers the local-engine probes with 404 and passes every other request to
// base. Once armed, the probe of the held host waits for release and nothing else: it ignores the
// request's context, so the probe's own deadline cannot end the hold.
type grokHeldProbeTransport struct {
	base    http.RoundTripper
	held    string
	armed   atomic.Bool
	entered chan struct{}
	enter   func()
	release chan struct{}
	mu      sync.Mutex
	probes  map[string]int32
}

func (g *grokHeldProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != grokHeldProbeStudioHost && r.URL.Host != grokHeldProbeLlamaHost {
		return g.base.RoundTrip(r)
	}
	g.mu.Lock()
	g.probes[r.URL.Host]++
	g.mu.Unlock()
	if g.armed.Load() && r.URL.Host == g.held {
		g.enter()
		<-g.release
	}
	return &http.Response{
		Status: "404 Not Found", StatusCode: http.StatusNotFound, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), ContentLength: 0, Request: r,
	}, nil
}

func (g *grokHeldProbeTransport) counts() map[string]int32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]int32, len(g.probes))
	maps.Copy(out, g.probes)
	return out
}

// TestGrokIDsSampledBeforeHeldProbe decides, through the composed worker's GET /v1/models, that a model
// refresh reads the Grok model IDs before it probes the local engines and publishes that sample. The
// saved Grok IDs are grok-4.7 when the worker starts, grok-4.6 when the second refresh starts, and
// grok-4.5 from the moment one of that refresh's probes is held until the refresh commits. A refresh
// that read the IDs after its probes would publish Grok 4.5; one that did not commit would keep
// Grok 4.7. Each local engine is held in its own case: the refresh probes both together.
func TestGrokIDsSampledBeforeHeldProbe(t *testing.T) {
	tests := map[string]struct {
		held string
	}{
		"success: the IDs read before a held LM Studio probe are published": {
			held: grokHeldProbeStudioHost,
		},
		"success: the IDs read before a held llama-server probe are published": {
			held: grokHeldProbeLlamaHost,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					_, _ = io.WriteString(w, grokHeldProbeListing)
					return
				}
				w.WriteHeader(http.StatusTeapot)
			}))
			defer proxySrv.Close()
			native := httptest.NewServer(http.NotFoundHandler())
			defer native.Close()

			base := &http.Transport{}
			defer base.CloseIdleConnections()
			gate := &grokHeldProbeTransport{base: base, held: tt.held, entered: make(chan struct{}), release: make(chan struct{}), probes: map[string]int32{}}
			gate.enter = sync.OnceFunc(func() { close(gate.entered) })
			releaseOnce := sync.OnceFunc(func() { close(gate.release) })
			defer releaseOnce()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := upstream.NewClient(upstream.Options{})
			defer func() { _ = client.Close() }()

			dir := t.TempDir()
			cachePath := filepath.Join(dir, "models_cache.json")
			grokHeldProbeWriteIDs(t, cachePath, "grok-4.7")
			lookup := mapLookup(map[string]string{
				"CODEX_GATEWAY_USAGE_ENDPOINT":     "0",
				"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
				"CCP_LMSTUDIO_BASE_URL":            "http://" + grokHeldProbeStudioHost,
				"CCP_LLAMA_PORT":                   strings.TrimPrefix(grokHeldProbeLlamaHost, "127.0.0.1:"),
			})
			routes := &route.Store{}
			state := &sentry.State{}
			mc, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            paths.New(lookup, func() (string, error) { return dir, nil }, func() (string, error) { return dir, nil }),
				ProxyBaseURL:     proxySrv.URL,
				AnthropicBaseURL: native.URL,
				DefaultBaseURL:   "http://127.0.0.1:18764",
				Shadow:           true,
				Client:           &http.Client{Transport: gate},
				Clock:            fixedClock,
				Routes:           routes,
				Sentry:           state,
				GrokIDs:          compose.NewGrokModelIDs(cachePath),
			})
			if err != nil {
				t.Fatalf("modelcache.New: %v", err)
			}
			defer func() { cancel(); mc.Wait() }()

			opts := minimalOptions(t, ctx, client)
			opts.AnthropicBaseURL = native.URL
			opts.ProxyBaseURL = proxySrv.URL
			opts.Routes = routes
			opts.ModelCache = mc
			opts.Sentry = state
			workerOpts, err := compose.New(opts)
			if err != nil {
				t.Fatalf("compose.New: %v", err)
			}
			wh, err := worker.NewHandler(workerOpts)
			if err != nil {
				t.Fatalf("worker.NewHandler: %v", err)
			}

			if err := mc.Refresh(ctx); err != nil {
				t.Fatalf("modelcache.Refresh when the worker starts: %v", err)
			}
			mustDiff(t, "probes while the worker starts", map[string]int32{grokHeldProbeStudioHost: 1, grokHeldProbeLlamaHost: 1}, gate.counts())
			mustDiff(t, "Grok rows published when the worker starts", []string{"Grok 4.7"}, grokHeldProbeRows(t, wh))

			grokHeldProbeWriteIDs(t, cachePath, "grok-4.6")
			gate.armed.Store(true)
			refreshed := make(chan error, 1)
			go func() { refreshed <- mc.Refresh(ctx) }()
			waitSignal(t, "the held probe", gate.entered, 5*time.Second)

			grokHeldProbeWriteIDs(t, cachePath, "grok-4.5")
			// A reader of the saved IDs sees the change while the probe is held.
			mustDiff(t, "saved Grok IDs during the held probe", []string{"grok-4.5"}, compose.NewGrokModelIDs(cachePath)())
			mustDiff(t, "Grok rows during the held probe", []string{"Grok 4.7"}, grokHeldProbeRows(t, wh))

			releaseOnce()
			select {
			case err := <-refreshed:
				if err != nil {
					t.Fatalf("modelcache.Refresh: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the refresh")
			}
			mustDiff(t, "probes after the refresh", map[string]int32{grokHeldProbeStudioHost: 2, grokHeldProbeLlamaHost: 2}, gate.counts())
			mustDiff(t, "Grok rows published by the refresh", []string{"Grok 4.6"}, grokHeldProbeRows(t, wh))
		})
	}
}

// grokHeldProbeWriteIDs replaces the saved Grok model cache with ids, in the Grok CLI's file format.
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
func grokHeldProbeRows(t *testing.T, wh http.Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d", rec.Code)
	}
	var doc struct {
		Data []struct {
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
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
