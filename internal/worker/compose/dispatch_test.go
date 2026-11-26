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
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

func TestDispatch(t *testing.T) {
	tests := map[string]struct {
		model                string
		body                 string
		proxyMode            string
		wantAnthropicHits    int64
		wantProxyHits        int64
		wantGrokHits         int64
		wantStatus           int
		wantBackendModel     string
		wantResponseModel    string
		wantResponseContains string
	}{
		"success: default upstream pass-through": {
			model:             "claude-sonnet-4-6",
			body:              messageRequestJSON("claude-sonnet-4-6", false),
			wantAnthropicHits: 1,
			wantStatus:        http.StatusOK,
			wantBackendModel:  "claude-sonnet-4-6",
			wantResponseModel: "claude-sonnet-4-6",
		},
		"success: proxy provider buffered": {
			model:             "claude-gpt-6.1-sol[1m]",
			body:              messageRequestJSON("claude-gpt-6.1-sol[1m]", false),
			proxyMode:         "buffered",
			wantProxyHits:     1,
			wantStatus:        http.StatusOK,
			wantBackendModel:  "gpt-6.1-sol",
			wantResponseModel: "claude-gpt-6.1-sol[1m]",
		},
		"success: proxy provider streaming": {
			model:                "claude-gpt-6.1-sol[1m]",
			body:                 messageRequestJSON("claude-gpt-6.1-sol[1m]", true),
			proxyMode:            "stream",
			wantProxyHits:        1,
			wantStatus:           http.StatusOK,
			wantBackendModel:     "gpt-6.1-sol",
			wantResponseContains: `"model":"claude-gpt-6.1-sol[1m]"`,
		},
		"success: compaction-guarded": {
			model:                "claude-gpt-6.1-sol[1m]",
			body:                 compactRequestJSON("claude-gpt-6.1-sol[1m]"),
			proxyMode:            "compaction-retry",
			wantProxyHits:        2,
			wantStatus:           http.StatusOK,
			wantBackendModel:     "gpt-6.1-sol",
			wantResponseContains: "message_stop",
		},
		"success: grok": {
			model:             "claude-grok-4.7[1m]",
			body:              messageRequestJSON("claude-grok-4.7[1m]", false),
			wantGrokHits:      1,
			wantStatus:        http.StatusOK,
			wantBackendModel:  "grok-4.7",
			wantResponseModel: "claude-grok-4.7[1m]",
		},
		"success: local engine lmstudio": {
			model:             "claude-lmstudio/ibm/granite-4-micro",
			body:              messageRequestJSON("claude-lmstudio/ibm/granite-4-micro", false),
			proxyMode:         "buffered",
			wantProxyHits:     1,
			wantStatus:        http.StatusOK,
			wantBackendModel:  "lmstudio/ibm/granite-4-micro",
			wantResponseModel: "claude-lmstudio/ibm/granite-4-micro",
		},
		"success: local engine llama": {
			model:             "claude-local/gguf",
			body:              messageRequestJSON("claude-local/gguf", false),
			proxyMode:         "buffered",
			wantProxyHits:     1,
			wantStatus:        http.StatusOK,
			wantBackendModel:  "local/gguf",
			wantResponseModel: "claude-local/gguf",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var seenAnthropicBody string
			var seenProxyModel string
			var seenProxyAuth string
			var seenGrokModel string
			var seenGrokClientKey string
			var proxyAttempt atomic.Int32

			h := newHarness(t, harnessConfig{
				anthropicHandler: func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					mu.Lock()
					seenAnthropicBody = string(raw)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_ant","type":"message","model":"claude-sonnet-4-6","stop_reason":"end_turn","content":[{"type":"text","text":"native"}],"usage":{"input_tokens":10,"output_tokens":5}}`)
				},
				proxyHandler: func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					obj := decodeJSONObject(t, raw)
					model, _ := obj["model"].(string)
					mu.Lock()
					seenProxyModel = model
					seenProxyAuth = r.Header.Get("X-Api-Key") + r.Header.Get("Authorization")
					mu.Unlock()
					attempt := proxyAttempt.Add(1)
					switch tt.proxyMode {
					case "stream":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"type\":\"message\",\"model\":\""+model+"\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n")
						_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n")
						_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					case "compaction-retry":
						w.Header().Set("Content-Type", "text/event-stream")
						if attempt == 1 {
							_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_c1\",\"type\":\"message\",\"model\":\""+model+"\",\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n")
							return
						}
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_c2\",\"type\":\"message\",\"model\":\""+model+"\",\"usage\":{\"input_tokens\":7,\"output_tokens\":0}}}\n\n")
						_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n")
						_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					default:
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"msg_p","type":"message","model":"`+model+`","stop_reason":"end_turn","content":[{"type":"text","text":"proxied"}],"usage":{"input_tokens":8,"output_tokens":3}}`)
					}
				},
				grokHandler: func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					obj := decodeJSONObject(t, raw)
					model, _ := obj["model"].(string)
					mu.Lock()
					seenGrokModel = model
					seenGrokClientKey = r.Header.Get("X-Api-Key")
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp_g","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"grok"}]}],"usage":{"input_tokens":9,"output_tokens":4}}`)
				},
			})

			headers := http.Header{"X-Api-Key": {fixtureAnthropicKey}}
			status, _, respBody := h.do(http.MethodPost, "/v1/messages", tt.body, headers)
			mustDiff(t, "HTTP status", tt.wantStatus, status)
			mustDiff(t, "anthropic upstream hits", tt.wantAnthropicHits, h.anthropicHits.Load())
			mustDiff(t, "proxy upstream hits", tt.wantProxyHits, h.proxyHits.Load())
			mustDiff(t, "grok upstream hits", tt.wantGrokHits, h.grokHits.Load())

			mu.Lock()
			defer mu.Unlock()
			if tt.wantAnthropicHits > 0 {
				mustDiff(t, "anthropic body preserved byte-for-byte", tt.body, seenAnthropicBody)
			}
			if tt.wantProxyHits > 0 {
				mustDiff(t, "proxy backend model", tt.wantBackendModel, seenProxyModel)
				if seenProxyAuth != "" {
					t.Error("client credentials leaked to proxy upstream")
				}
			}
			if tt.wantGrokHits > 0 {
				mustDiff(t, "grok backend model", tt.wantBackendModel, seenGrokModel)
				if seenGrokClientKey != "" {
					t.Error("client X-Api-Key leaked to Grok upstream")
				}
			}
			if tt.wantResponseModel != "" {
				got := decodeJSONObject(t, respBody)
				mustDiff(t, "response model", tt.wantResponseModel, got["model"])
			}
			if tt.wantResponseContains != "" && !strings.Contains(string(respBody), tt.wantResponseContains) {
				t.Errorf("response body missing %q: %s", tt.wantResponseContains, string(respBody))
			}
		})
	}
}

func TestDispatchUnlistedModelsReachAnthropicOnly(t *testing.T) {
	var mu sync.Mutex
	var seenAnthropicModels []string
	var seenProxyModels []string

	h := newHarness(t, harnessConfig{
		anthropicHandler: func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			obj := decodeJSONObject(t, raw)
			model, _ := obj["model"].(string)
			mu.Lock()
			seenAnthropicModels = append(seenAnthropicModels, model)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_ant","type":"message","model":"`+model+`","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		},
		proxyHandler: func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			obj := decodeJSONObject(t, raw)
			model, _ := obj["model"].(string)
			mu.Lock()
			seenProxyModels = append(seenProxyModels, model)
			mu.Unlock()
			t.Errorf("proxy must not receive Claude or unlisted model %q", model)
			w.WriteHeader(http.StatusInternalServerError)
		},
	})

	models := []string{"claude-opus-5-5", "claude-glm-99", "claude-vertex/ai21/jamba-2"}
	for _, model := range models {
		status, _, body := h.do(http.MethodPost, "/v1/messages", messageRequestJSON(model, false), nil)
		if status != http.StatusOK {
			t.Fatalf("model %q status=%d body=%s", model, status, string(body))
		}
	}

	mu.Lock()
	defer mu.Unlock()
	mustDiff(t, "anthropic received models", models, seenAnthropicModels)
	mustDiff(t, "proxy received models", []string(nil), seenProxyModels)
	mustDiff(t, "proxy hits", int64(0), h.proxyHits.Load())
}

func TestWorkerHooksAndHealthz(t *testing.T) {
	tests := map[string]struct {
		withReadiness bool
	}{
		"success: models, health catalog, codex readiness bound, and anthropic-only refresh": {withReadiness: true},
		"success: codex readiness nil uses default checking summary":                         {withReadiness: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			probeDone := make(chan struct{})
			probeOnce := sync.OnceFunc(func() { close(probeDone) })

			h := newHarness(t, harnessConfig{
				withReadiness: tt.withReadiness,
				readinessProbe: func(context.Context) readiness.ProxyAuthResult {
					defer probeOnce()
					return readiness.ProxyAuthResult{
						ProxyBinary: true,
						ProxyModels: true,
						CodexAuth:   true,
						CheckedAt:   fixedTime,
					}
				},
			})

			// GET /v1/models exercises Options.Models and increments served.models.
			status, hdr, modelsRaw := h.do(http.MethodGet, "/v1/models", "", http.Header{"X-Api-Key": {fixtureAnthropicKey}})
			mustDiff(t, "GET /v1/models status", http.StatusOK, status)
			mustDiff(t, "GET /v1/models content-type", "application/json", hdr.Get("Content-Type"))
			var listing struct {
				Data    []map[string]any `json:"data"`
				HasMore bool             `json:"has_more"`
			}
			if err := json.Unmarshal(modelsRaw, &listing); err != nil {
				t.Fatalf("decode /v1/models: %v", err)
			}
			if len(listing.Data) == 0 {
				t.Fatal("GET /v1/models returned empty data; Options.Models not wired to modelcache")
			}

			// First /healthz triggers the shared readiness check if bound.
			health := h.healthz()
			mustDiff(t, "/healthz catalog", any("proxy"), health["catalog"])
			served, _ := health["served"].(map[string]any)
			mustDiff(t, "/healthz served.models", any(float64(1)), served["models"])

			if tt.withReadiness {
				waitSignal(t, "readiness probe completion", probeDone, 3*time.Second)
				h.readinessCheck.Wait()
				health = h.healthz()
				cr, _ := health["codexReadiness"].(map[string]any)
				mustDiff(t, "codexReadiness.ready", any(true), cr["ready"])
				mustDiff(t, "codexReadiness.state", any("ready"), cr["state"])

				// Recording an upstream unavailable state surfaces through the bound CodexReadiness callback.
				if _, err := h.upstreamState.SetUnavailable(http.StatusBadGateway); err != nil {
					t.Fatalf("SetUnavailable: %v", err)
				}
				health = h.healthz()
				cr, _ = health["codexReadiness"].(map[string]any)
				mustDiff(t, "codexReadiness.ready after unavailable", any(false), cr["ready"])
				mustDiff(t, "codexReadiness.state after unavailable", any("upstream-unavailable"), cr["state"])
			} else {
				cr, _ := health["codexReadiness"].(map[string]any)
				mustDiff(t, "nil CodexReadiness default ready", any(false), cr["ready"])
				mustDiff(t, "nil CodexReadiness default state", any("checking"), cr["state"])
			}
		})
	}

	t.Run("success: refresh anthropic triggers only for accepted anthropic requests", func(t *testing.T) {
		h := newHarness(t, harnessConfig{})
		hdrs := http.Header{"X-Api-Key": {fixtureAnthropicKey}}

		status, _, _ := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-gpt-6.1-sol[1m]", false), hdrs)
		mustDiff(t, "proxy status", http.StatusOK, status)
		status, _, _ = h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-grok-4.7[1m]", false), hdrs)
		mustDiff(t, "grok status", http.StatusOK, status)
		status, _, _ = h.do(http.MethodPost, "/v1/messages", `{"model":"\uD800","messages":[]}`, hdrs)
		mustDiff(t, "classifier refusal status", http.StatusBadRequest, status)
		h.modelCache.Wait()
		mustDiff(t, "non-anthropic requests must not trigger anthropic catalog refresh", int64(0), h.anthropicModel.Load())

		status, _, _ = h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-sonnet-4-6", false), hdrs)
		mustDiff(t, "anthropic status", http.StatusOK, status)
		h.modelCache.Wait()
		mustDiff(t, "accepted anthropic request triggered refresh once", int64(1), h.anthropicModel.Load())
	})
}
