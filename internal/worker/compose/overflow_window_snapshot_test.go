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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	// overflowWindowListedBefore lists kimi-k3, whose provider row resolves to the 200000-token window
	// for proxy rows absent from the window table.
	overflowWindowListedBefore = `{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"},{"id":"gpt-6.1-sol","display_name":"gpt-6.1-sol (codex)"}]}`
	// overflowWindowListedAfter no longer lists kimi-k3: resolved against it, the model has no window
	// of its own and the 920000-token Codex fallback would apply.
	overflowWindowListedAfter = `{"data":[{"id":"gpt-6.1-sol","display_name":"gpt-6.1-sol (codex)"}]}`
	overflowWindowModel       = "claude-kimi-k3"
)

// overflowWindowAnswer is what the composed handler wrote for the held request.
type overflowWindowAnswer struct {
	status int
	header http.Header
	body   []byte
}

// TestOverflowWindowStaysWithRequestSnapshot decides that an overflow answer without an exact backend
// limit falls back to the window of the route snapshot the request was sent with. The proxy listing
// is replaced and published between the send and the overflow reply; the request's snapshot gives
// 200000 tokens, the newly published listing would give 920000. A limit the backend states itself
// still wins over either.
func TestOverflowWindowStaysWithRequestSnapshot(t *testing.T) {
	tests := map[string]struct {
		status             int
		body               string
		wantLimit          float64
		wantPrefix         string
		wantUpstreamStatus string
	}{
		"success: a request_too_large reply without counts takes the window of the request's snapshot": {
			status:     http.StatusRequestEntityTooLarge,
			body:       `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`,
			wantLimit:  200000,
			wantPrefix: "Request exceeds the maximum size (200001 tokens > 200000 tokens) [model-gateway: phase first_turn, refused by upstream;",
		},
		"success: an engine context error without its context size takes the window of the request's snapshot": {
			status:             http.StatusBadRequest,
			body:               `{"error":{"type":"exceed_context_size_error","message":"the request exceeds the available context size"}}`,
			wantLimit:          200000,
			wantPrefix:         "Input exceeds the model context window; compact and retry. (200001 tokens > 200000 tokens) [model-gateway: phase first_turn, refused by upstream;",
			wantUpstreamStatus: "400",
		},
		"success: a request_too_large reply with its own counts keeps the upstream limit": {
			status:     http.StatusRequestEntityTooLarge,
			body:       `{"type":"error","error":{"type":"request_too_large","message":"prompt is too long: 70000 tokens > 50000 tokens"}}`,
			wantLimit:  50000,
			wantPrefix: "prompt is too long: 70000 tokens > 50000 tokens [model-gateway: phase first_turn, refused by upstream;",
		},
		"success: an engine context error with its context size keeps the engine limit": {
			status:             http.StatusBadRequest,
			body:               `{"error":{"type":"exceed_context_size_error","message":"the request exceeds the available context size","n_prompt_tokens":70000,"n_ctx":50000}}`,
			wantLimit:          50000,
			wantPrefix:         "Input exceeds the model context window; compact and retry. (70000 tokens > 50000 tokens) [model-gateway: phase first_turn, refused by upstream;",
			wantUpstreamStatus: "400",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var listing atomic.Value
			listing.Store(overflowWindowListedBefore)
			entered := make(chan struct{})
			enteredOnce := sync.OnceFunc(func() { close(entered) })
			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			var sends atomic.Int32

			proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					_, _ = io.WriteString(w, listing.Load().(string))
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" || providerModel(t, r) != "kimi-k3" {
					w.WriteHeader(http.StatusTeapot)
					return
				}
				sends.Add(1)
				enteredOnce()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer proxySrv.Close()
			// Runs before the server closes, so a failed test never leaves the provider held.
			defer releaseOnce()
			native := httptest.NewServer(http.NotFoundHandler())
			defer native.Close()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := upstream.NewClient(upstream.Options{})
			defer func() { _ = client.Close() }()

			dir := t.TempDir()
			lookup := mapLookup(map[string]string{
				"CODEX_GATEWAY_USAGE_ENDPOINT":     "0",
				"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
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
				Client:           proxySrv.Client(),
				Clock:            fixedClock,
				Routes:           routes,
				Sentry:           state,
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
			opts.SentryEnabled = true
			workerOpts, err := compose.New(opts)
			if err != nil {
				t.Fatalf("compose.New: %v", err)
			}
			wh, err := worker.NewHandler(workerOpts)
			if err != nil {
				t.Fatalf("worker.NewHandler: %v", err)
			}
			if err := mc.Refresh(ctx); err != nil {
				t.Fatalf("modelcache.Refresh before the send: %v", err)
			}
			mustDiff(t, "advertised window of the model before the send", map[string]float64{overflowWindowModel: 200000}, overflowWindowAdvertised(t, wh))

			done := make(chan overflowWindowAnswer, 1)
			go func() {
				rec := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(messageRequestJSON(overflowWindowModel, false)))
				defer func() {
					if p := recover(); p != nil {
						if err, ok := p.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
							panic(p)
						}
					}
					done <- overflowWindowAnswer{status: rec.Code, header: rec.Header().Clone(), body: rec.Body.Bytes()}
				}()
				wh.ServeHTTP(rec, req)
			}()
			waitSignal(t, "the upstream send", entered, 5*time.Second)

			listing.Store(overflowWindowListedAfter)
			if err := mc.Refresh(ctx); err != nil {
				t.Fatalf("modelcache.Refresh between send and reply: %v", err)
			}
			// The replacement is published before the overflow reply arrives.
			mustDiff(t, "advertised window of the model after the replacement", map[string]float64{}, overflowWindowAdvertised(t, wh))

			releaseOnce()
			var got overflowWindowAnswer
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the overflow answer")
			}
			mustDiff(t, "upstream sends", int32(1), sends.Load())
			mustDiff(t, "client status", http.StatusRequestEntityTooLarge, got.status)
			mustDiff(t, "X-Model-Gateway-Upstream-Status", tt.wantUpstreamStatus, got.header.Get("X-Model-Gateway-Upstream-Status"))
			doc := decodeJSONObject(t, got.body)
			errObj, _ := doc["error"].(map[string]any)
			mustDiff(t, "error.type", any("request_too_large"), errObj["type"])
			overflow, _ := errObj["overflow"].(map[string]any)
			mustDiff(t, "error.overflow.limit_tokens", any(tt.wantLimit), overflow["limit_tokens"])
			message, _ := errObj["message"].(string)
			if !strings.HasPrefix(message, tt.wantPrefix) {
				t.Errorf("error.message = %q, want prefix %q", message, tt.wantPrefix)
			}
		})
	}
}

// overflowWindowAdvertised returns the advertised window of every listed kimi row, read through
// the composed handler's GET /v1/models.
func overflowWindowAdvertised(t *testing.T, wh http.Handler) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	wh.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d", rec.Code)
	}
	var doc struct {
		Data []struct {
			ID     string  `json:"id"`
			Window float64 `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	out := map[string]float64{}
	for _, row := range doc.Data {
		if strings.Contains(row.ID, "kimi") {
			out[row.ID] = row.Window
		}
	}
	return out
}
