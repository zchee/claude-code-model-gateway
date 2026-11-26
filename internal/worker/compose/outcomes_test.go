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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminalOutcomesAndUsage(t *testing.T) {
	t.Run("success: buffered proxy reply preserves headers, status, body and emits one span and one usage record", func(t *testing.T) {
		var preparedSize atomic.Int64
		h := newHarness(t, harnessConfig{
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				n, err := io.Copy(io.Discard, r.Body)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				preparedSize.Store(n)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Custom-Relay", "preserved-value")
				w.Header().Set("Request-Id", "req-buffered-1")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":"msg_buf","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"buffered hello"}],"usage":{"input_tokens":42,"output_tokens":9,"cache_read_input_tokens":10,"cache_creation_input_tokens":3}}`)
			},
		})

		reqBody := `{"model":"claude-gpt-6.1-sol[1m]","max_tokens":32,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hello"}]}`
		status, hdr, body := h.do(http.MethodPost, "/v1/messages", reqBody, http.Header{
			"X-Claude-Code-Session-Id": {"session-buffered-1"},
		})
		mustDiff(t, "buffered HTTP status", http.StatusOK, status)
		mustDiff(t, "buffered custom header", "preserved-value", hdr.Get("X-Custom-Relay"))
		mustDiff(t, "buffered Request-Id header", "req-buffered-1", hdr.Get("Request-Id"))

		respObj := decodeJSONObject(t, body)
		wantBody := decodeJSONObject(t, []byte(`{"id":"msg_buf","type":"message","model":"claude-gpt-6.1-sol[1m]","stop_reason":"end_turn","content":[{"type":"text","text":"buffered hello"}],"usage":{"input_tokens":42,"output_tokens":9,"cache_read_input_tokens":10,"cache_creation_input_tokens":3}}`))
		mustDiff(t, "entire buffered response", wantBody, respObj)
		if preparedSize.Load() == 0 || preparedSize.Load() == int64(len(reqBody)) {
			t.Fatal("fixture must distinguish prepared from original bytes")
		}
		raw, err := os.ReadFile(h.highWaterPath("session-buffered-1"))
		if err != nil {
			t.Fatal("missing accepted high-water record")
		}
		mustDiff(t, "prepared bytes at high-water", float64(preparedSize.Load()), decodeJSONObject(t, raw)["value"])

		h.telemetryRec.Wait()
		h.usageEmitter.Wait()

		spans := decodeOTLPSpans(t, h.traceCollector.snapshot())
		mustDiff(t, "span count", 1, len(spans))
		mustDiff(t, "span status", any("ok"), spans[0].Attributes["status"])
		mustDiff(t, "span status_code", int64(http.StatusOK), spans[0].Attributes["status_code"])
		mustDiff(t, "span backend", any("codex"), spans[0].Attributes["backend"])
		mustDiff(t, "span selected_model", any("claude-gpt-6.1-sol[1m]"), spans[0].Attributes["selected_model"])
		mustDiff(t, "span effective_model", any("gpt-6.1-sol"), spans[0].Attributes["effective_model"])

		logs := decodeOTLPLogs(t, h.usageCollector.snapshot())
		mustDiff(t, "usage record count", 1, len(logs))
		mustDiff(t, "usage event_name", "gateway.token.usage", logs[0].EventName)
		wantAttrs := map[string]any{
			"request_id":            "req-buffered-1",
			"request_id_source":     "response_header",
			"requested_model":       "claude-gpt-6.1-sol[1m]",
			"model":                 "gpt-6.1-sol",
			"backend":               "codex",
			"effort":                "high",
			"via":                   "direct",
			"status":                "ok",
			"status_code":           int64(http.StatusOK),
			"response_mode":         "json",
			"input_tokens":          int64(42),
			"output_tokens":         int64(9),
			"cache_read_tokens":     int64(10),
			"cache_creation_tokens": int64(3),
		}
		for k, want := range wantAttrs {
			mustDiff(t, "usage attr "+k, want, logs[0].Attributes[k])
		}
	})

	t.Run("success: rejected websocket upgrade retry emits one span and one usage record", func(t *testing.T) {
		var attempts atomic.Int32
		zeroDelay := time.Duration(0)
		retries := 2
		h := newHarness(t, harnessConfig{
			replyRetries:    &retries,
			replyRetryDelay: &zeroDelay,
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				if n <= 2 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, upgradeRejection)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Request-Id", "req-ws-retry-ok")
				_, _ = io.WriteString(w, `{"id":"msg_ws","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"recovered"}],"usage":{"input_tokens":14,"output_tokens":6}}`)
			},
		})

		status, _, _ := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-gpt-6.1-sol[1m]", false), nil)
		mustDiff(t, "retry recovered status", http.StatusOK, status)
		mustDiff(t, "upstream attempts", int32(3), attempts.Load())

		h.telemetryRec.Wait()
		h.usageEmitter.Wait()
		mustDiff(t, "one route span across retries", 1, len(decodeOTLPSpans(t, h.traceCollector.snapshot())))
		mustDiff(t, "one usage record across retries", 1, len(decodeOTLPLogs(t, h.usageCollector.snapshot())))
	})

	t.Run("success: three client requests with failed semantic span and compaction retry emit three spans", func(t *testing.T) {
		var attempts atomic.Int32
		h := newHarness(t, harnessConfig{
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				if n == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-OpenAI-Request-Id", "req-telemetry-attributed")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = io.WriteString(w, `{"error":{"message":"OpenAI rate limit"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if n == 2 || n == 3 {
					_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream failed\"}}\n\n")
					return
				}
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			},
		})

		directBody := `{"model":"claude-gpt-5.6-sol","max_tokens":1,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"prompt-secret-must-not-enter-telemetry"}],"tools":[{"name":"PrivateTool","description":"tool-secret-must-not-enter-telemetry"}]}`
		compactBody := compactRequestJSON("claude-gpt-5.6-sol")

		s1, _, _ := h.do(http.MethodPost, "/v1/messages", directBody, traced(traceOne))
		mustDiff(t, "request 1 status", http.StatusTooManyRequests, s1)
		s2, _, _ := h.do(http.MethodPost, "/v1/messages", directBody, traced(traceTwo))
		mustDiff(t, "request 2 status", http.StatusOK, s2)
		s3, _, _ := h.do(http.MethodPost, "/v1/messages", compactBody, traced(traceThree))
		mustDiff(t, "request 3 status", http.StatusOK, s3)

		h.telemetryRec.Wait()
		spans := decodeOTLPSpans(t, h.traceCollector.snapshot())
		mustDiff(t, "three client requests emit three spans", 3, len(spans))
		assertSpan(t, spans, traceOne, "client_error", http.StatusTooManyRequests)
		assertSpan(t, spans, traceTwo, "upstream_error", http.StatusOK)
		assertSpan(t, spans, traceThree, "ok", http.StatusOK)
		mustDiff(t, "total upstream attempts across 3 requests", int32(4), attempts.Load())
	})

	tests := map[string]struct {
		stream bool
	}{
		"success: grok buffered finishes one route span and emits zero usage records":  {stream: false},
		"success: grok streaming finishes one route span and emits zero usage records": {stream: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessConfig{
				sentryEnvTrigger: 100,
				env:              map[string]string{"CODEX_GATEWAY_COMPACT_TRIGGER": "100"},
				grokHandler: func(w http.ResponseWriter, r *http.Request) {
					if tt.stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_gs\",\"model\":\"grok-4.7\"}}\n\n")
						_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
						_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_gs\",\"status\":\"completed\",\"usage\":{\"input_tokens\":105,\"output_tokens\":7}}}\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp_gb","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":105,"output_tokens":7}}`)
				},
			})

			body := messageRequestJSON("claude-grok-4.7[1m]", tt.stream)
			hdrs := http.Header{"X-Claude-Code-Session-Id": {"grok-no-usage-session"}}
			status, _, _ := h.do(http.MethodPost, "/v1/messages", body, hdrs)
			mustDiff(t, "grok status", http.StatusOK, status)

			h.telemetryRec.Wait()
			h.usageEmitter.Wait()

			// Route span MUST be finished once.
			spans := decodeOTLPSpans(t, h.traceCollector.snapshot())
			mustDiff(t, "grok route span count", 1, len(spans))
			mustDiff(t, "grok span backend", any("grok"), spans[0].Attributes["backend"])
			mustDiff(t, "grok span status_code", int64(http.StatusOK), spans[0].Attributes["status_code"])

			// Neither Grok branch nor root finishes the usage capture: zero emitted usage records.
			mustDiff(t, "grok emitted usage records", 0, len(decodeOTLPLogs(t, h.usageCollector.snapshot())))

			// Response-side sentry observation still ran: turn 2 must trip sentry 413 without contacting Grok.
			s2, _, _ := h.do(http.MethodPost, "/v1/messages", body, hdrs)
			mustDiff(t, "grok turn 2 sentry 413 confirms response observation ran", http.StatusRequestEntityTooLarge, s2)
			mustDiff(t, "grok upstream hits stayed 1", int64(1), h.grokHits.Load())
		})
	}

	t.Run("success: lmstudio combined cross-package usage record applies exclusive cache correction", func(t *testing.T) {
		h := newHarness(t, harnessConfig{
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Request-Id", "req-lm-1")
				// LM Studio reports inclusive input_tokens (100 includes 60 cached); exclusive input_tokens must be 40.
				_, _ = io.WriteString(w, `{"id":"msg_lm","type":"message","model":"lmstudio/ibm/granite-4-micro","stop_reason":"end_turn","content":[{"type":"text","text":"local"}],"usage":{"input_tokens":100,"cache_read_input_tokens":60,"output_tokens":11}}`)
			},
		})

		reqBody := `{"model":"claude-lmstudio/ibm/granite-4-micro","max_tokens":16,"messages":[{"role":"user","content":"check cache"}]}`
		status, _, _ := h.do(http.MethodPost, "/v1/messages", reqBody, http.Header{
			"X-Claude-Code-Session-Id": {"session-lm-usage"},
		})
		mustDiff(t, "lmstudio status", http.StatusOK, status)

		h.usageEmitter.Wait()
		logs := decodeOTLPLogs(t, h.usageCollector.snapshot())
		mustDiff(t, "lmstudio usage records", 1, len(logs))
		mustDiff(t, "lmstudio backend", any("lmstudio"), logs[0].Attributes["backend"])
		for _, key := range []string{"model", "requested_model"} {
			if _, exists := logs[0].Attributes[key]; exists {
				t.Errorf("slash-containing %s must not be exported", key)
			}
		}
		mustDiff(t, "lmstudio corrected exclusive input_tokens", int64(40), logs[0].Attributes["input_tokens"])
		mustDiff(t, "lmstudio cache_read_tokens", int64(60), logs[0].Attributes["cache_read_tokens"])
		mustDiff(t, "lmstudio output_tokens", int64(11), logs[0].Attributes["output_tokens"])
		mustDiff(t, "lmstudio request_id", any("req-lm-1"), logs[0].Attributes["request_id"])
	})

	t.Run("success: dispatch usage records marker ticket only for originating agent, not inherited child", func(t *testing.T) {
		h := newHarness(t, harnessConfig{
			sentryDisabled: true,
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_disp","type":"message","model":"gpt-5.6-terra","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			},
		})

		parentStatus, _, _ := h.do(http.MethodPost, "/v1/messages",
			`{"model":"claude-codex-auto","messages":[{"role":"user","content":"[sidequest-route model=gpt-5.6-terra effort=high ticket=SQ-1234] work"}]}`,
			http.Header{
				"X-Claude-Code-Session-Id": {"ticket-session"},
				"X-Claude-Code-Agent-Id":   {"parent-agent"},
			},
		)
		mustDiff(t, "parent dispatch status", http.StatusOK, parentStatus)

		childStatus, _, _ := h.do(http.MethodPost, "/v1/messages",
			`{"model":"claude-codex-auto","messages":[{"role":"user","content":"continue"}]}`,
			http.Header{
				"X-Claude-Code-Session-Id":      {"ticket-session"},
				"X-Claude-Code-Agent-Id":        {"child-agent"},
				"X-Claude-Code-Parent-Agent-Id": {"parent-agent"},
			},
		)
		mustDiff(t, "child inherited dispatch status", http.StatusOK, childStatus)

		h.usageEmitter.Wait()
		logs := decodeOTLPLogs(t, h.usageCollector.snapshot())
		mustDiff(t, "dispatch usage record count", 2, len(logs))
		byAgent := make(map[string]map[string]any, len(logs))
		for _, rec := range logs {
			if agent, ok := rec.Attributes["agent_id"].(string); ok {
				byAgent[agent] = rec.Attributes
			}
		}
		mustDiff(t, "parent ticket_ref", any("SQ-1234"), byAgent["parent-agent"]["ticket_ref"])
		mustDiff(t, "parent via", any("dispatch"), byAgent["parent-agent"]["via"])
		mustDiff(t, "child via", any("dispatch-inherited"), byAgent["child-agent"]["via"])
		if _, hasTicket := byAgent["child-agent"]["ticket_ref"]; hasTicket {
			t.Errorf("inherited child record must not carry ticket_ref, got %v", byAgent["child-agent"]["ticket_ref"])
		}
	})

	t.Run("success: codex upstream state updates even with OTLP disabled and non-codex 401 leaves it untouched", func(t *testing.T) {
		var mode atomic.Int32
		h := newHarness(t, harnessConfig{
			disableTrace: true,
			disableUsage: true,
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				switch mode.Load() {
				case 0:
					if providerModel(t, r) != "kimi-k3" {
						t.Error("non-Kimi request reached rejection fixture")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Openai-Processing-Ms", "3")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"kimi token expired"}}`)
				case 1:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"upstream down"}}`)
				default:
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_ok","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"recovered"}],"usage":{"input_tokens":5,"output_tokens":2}}`)
				}
			},
		})

		// Non-Codex (Kimi) 401 with OpenAI-style header must pass through unchanged and never touch Codex state.
		if h.telemetryRec.Enabled() {
			t.Fatal("expected tracing disabled before first request")
		}
		sKimi, _, kimiRaw := h.do(http.MethodPost, "/v1/messages", `{"model":"claude-kimi-k3","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, nil)
		mustDiff(t, "Kimi proxy contacts", int64(1), h.proxyHits.Load())
		mustDiff(t, "unintended native contacts", int64(0), h.anthropicHits.Load())
		mustDiff(t, "kimi 401 status", http.StatusUnauthorized, sKimi)
		kimiDoc := decodeJSONObject(t, kimiRaw)
		mustDiff(t, "kimi error body", map[string]any{"type": "authentication_error", "message": "kimi token expired"}, kimiDoc["error"])
		if _, err := os.Stat(filepath.Join(h.upstreamState.Dir, "codex-upstream-blocked.json")); !os.IsNotExist(err) {
			t.Fatalf("non-Codex 401 must not create codex-upstream-blocked.json (err=%v)", err)
		}
		if _, err := os.Stat(filepath.Join(h.upstreamState.Dir, "codex-upstream-unavailable.json")); !os.IsNotExist(err) {
			t.Fatalf("non-Codex 401 must not create codex-upstream-unavailable.json (err=%v)", err)
		}

		// Codex 502 records unavailable even though OTLP telemetry is disabled.
		mode.Store(1)
		sFail, _, _ := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-gpt-6.1-sol[1m]", false), nil)
		mustDiff(t, "codex 502 status", http.StatusBadGateway, sFail)
		if h.upstreamState.ReadUnavailable() == nil {
			t.Fatal("Codex 502 did not set upstream unavailable when OTLP is disabled")
		}

		// Subsequent Codex 200 clears blocked and unavailable even with OTLP disabled.
		if _, err := h.upstreamState.SetBlocked(http.StatusUnauthorized, "fixture-block", nil); err != nil {
			t.Fatalf("SetBlocked: %v", err)
		}
		mode.Store(2)
		sOK, _, _ := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-gpt-6.1-sol[1m]", false), nil)
		mustDiff(t, "codex 200 status", http.StatusOK, sOK)
		if h.upstreamState.ReadBlocked() != nil {
			t.Error("Codex 200 did not clear upstream blocked state")
		}
		if h.upstreamState.ReadUnavailable() != nil {
			t.Error("Codex 200 did not clear upstream unavailable state")
		}
	})

	t.Run("success: route-only proxy tracing starts capture for compaction diagnostics without exporting usage", func(t *testing.T) {
		var attempts atomic.Int32
		h := newHarness(t, harnessConfig{
			disableUsage: true,
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if n == 1 {
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ro1\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":19,\"output_tokens\":0}}}\n\n")
					return
				}
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_ro2\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":23,\"output_tokens\":0}}}\n\n")
				_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\n")
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			},
		})

		status, _, _ := h.do(http.MethodPost, "/v1/messages", compactRequestJSON("claude-gpt-6.1-sol[1m]"), nil)
		mustDiff(t, "route-only compaction status", http.StatusOK, status)

		h.telemetryRec.Wait()
		h.usageEmitter.Wait()

		spans := decodeOTLPSpans(t, h.traceCollector.snapshot())
		mustDiff(t, "route-only span count", 1, len(spans))
		mustDiff(t, "compaction input tokens populated via capture snapshot", int64(23), spans[0].Attributes["compaction_input_tokens"])
		mustDiff(t, "compaction output tokens populated via capture snapshot", int64(8), spans[0].Attributes["compaction_output_tokens"])
		mustDiff(t, "usage collector received zero posts when disabled", 0, h.usageCollector.count())
	})
}
