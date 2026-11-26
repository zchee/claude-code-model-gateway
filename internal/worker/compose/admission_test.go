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
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdmissionAndCounting(t *testing.T) {
	t.Run("success: proxy accepted counts and logs at admission before upstream replies", func(t *testing.T) {
		entered := make(chan struct{})
		markEntered := sync.OnceFunc(func() { close(entered) })
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		t.Cleanup(unblock)

		h := newHarness(t, harnessConfig{
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				markEntered()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_ok","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":2}}`)
			},
		})

		type result struct {
			status int
		}
		done := make(chan result, 1)
		go func() {
			status, _, _ := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-gpt-6.1-sol[1m]", false), http.Header{
				"X-Claude-Code-Session-Id": {"session-admit-order"},
			})
			done <- result{status: status}
		}()
		t.Cleanup(func() {
			unblock()
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				t.Error("in-flight proxy request did not exit during cleanup")
			}
		})

		waitSignal(t, "proxy upstream entry", entered, 3*time.Second)

		// Before upstream responds, served.codex must already be 1 and the audit entry must already exist.
		health := h.healthz()
		served, _ := health["served"].(map[string]any)
		mustDiff(t, "served.codex while in flight", any(float64(1)), served["codex"])
		midAudit := h.readAuditLines()
		mustDiff(t, "audit count while in flight", 1, len(midAudit))
		mustDiff(t, "audit backend while in flight", any("codex"), midAudit[0]["backend"])

		unblock()
		var got result
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case got = <-done:
		case <-timer.C:
			t.Fatal("proxy request did not finish after release")
		}
		mustDiff(t, "proxy HTTP status", http.StatusOK, got.status)

		// Completion must not log a second audit entry or increment served a second time.
		h.telemetryRec.Wait()
		h.usageEmitter.Wait()
		finalHealth := h.healthz()
		finalServed, _ := finalHealth["served"].(map[string]any)
		mustDiff(t, "served.codex after completion", any(float64(1)), finalServed["codex"])
		finalAudit := h.readAuditLines()
		mustDiff(t, "audit count after completion", 1, len(finalAudit))
	})

	t.Run("success: proxy sentry 413 uses reply writer, counts at admission, and logs audit before 413", func(t *testing.T) {
		h := newHarness(t, harnessConfig{
			sentryEnvTrigger: 100,
			env:              map[string]string{"CODEX_GATEWAY_COMPACT_TRIGGER": "100"},
			proxyHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":30,\"cache_read_input_tokens\":50,\"cache_creation_input_tokens\":21,\"output_tokens\":2}}\n\n")
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			},
		})

		body := messageRequestJSON("claude-gpt-5.6-sol", true)
		hdrs := http.Header{"X-Claude-Code-Session-Id": {"sentry-codex-session"}}

		// Turn 1: exceeds trigger (30 + 50 + 21 = 101 > 100) and arms sentry.
		s1, _, _ := h.do(http.MethodPost, "/v1/messages", body, hdrs)
		mustDiff(t, "turn 1 status", http.StatusOK, s1)

		// Turn 2: admitted (served.codex -> 2, audit -> 2), then refused locally by reply.WriteSentry with 413.
		s2, respHdr, respRaw := h.do(http.MethodPost, "/v1/messages", body, hdrs)
		mustDiff(t, "turn 2 sentry status", http.StatusRequestEntityTooLarge, s2)
		if got := respHdr.Get("X-Model-Gateway-Upstream-Status"); got != "" {
			t.Errorf("request-side sentry 413 must not set X-Model-Gateway-Upstream-Status, got %q", got)
		}
		errDoc := decodeJSONObject(t, respRaw)
		errObj, _ := errDoc["error"].(map[string]any)
		mustDiff(t, "sentry 413 error.type", any("request_too_large"), errObj["type"])
		msg, _ := errObj["message"].(string)
		const wantPrefix = "Prompt is too long for the Codex context window; compact and retry. (101 tokens > 100 tokens) [model-gateway: phase first_turn, refused by gateway_sentry;"
		if !strings.HasPrefix(msg, wantPrefix) {
			t.Errorf("sentry 413 message = %q, want prefix %q", msg, wantPrefix)
		}
		mustDiff(t, "upstream proxy forwards", int64(1), h.proxyHits.Load())

		// Admission happened before sentry refusal: served.codex == 2 and both turns were logged once.
		health := h.healthz()
		served, _ := health["served"].(map[string]any)
		mustDiff(t, "served.codex includes sentry refusal", any(float64(2)), served["codex"])
		audits := h.readAuditLines()
		mustDiff(t, "audit entries for turn 1 + turn 2", 2, len(audits))

		h.telemetryRec.Wait()
		spans := decodeOTLPSpans(t, h.traceCollector.snapshot())
		mustDiff(t, "exported route spans", 2, len(spans))
		mustDiff(t, "turn 2 span status_code", int64(http.StatusRequestEntityTooLarge), spans[1].Attributes["status_code"])
	})

	t.Run("success: grok sentry 413 uses reply writer, counts at admission, logs audit, and skips auth", func(t *testing.T) {
		h := newHarness(t, harnessConfig{
			sentryEnvTrigger: 100,
			env:              map[string]string{"CODEX_GATEWAY_COMPACT_TRIGGER": "100"},
			grokHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_fixture","status":"completed","output":[],"usage":{"input_tokens":101,"output_tokens":1}}`)
			},
		})

		body := `{"model":"claude-grok-4.5[1m]","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`
		hdrs := http.Header{"X-Claude-Code-Session-Id": {"sentry-grok-session"}}

		s1, _, _ := h.do(http.MethodPost, "/v1/messages", body, hdrs)
		mustDiff(t, "grok turn 1 status", http.StatusOK, s1)
		authBefore := h.grokAuthHits.Load()

		s2, respHdr, respRaw := h.do(http.MethodPost, "/v1/messages", body, hdrs)
		mustDiff(t, "grok turn 2 sentry status", http.StatusRequestEntityTooLarge, s2)
		if got := respHdr.Get("X-Model-Gateway-Upstream-Status"); got != "" {
			t.Errorf("grok request-side sentry 413 must not set X-Model-Gateway-Upstream-Status, got %q", got)
		}
		errDoc := decodeJSONObject(t, respRaw)
		errObj, _ := errDoc["error"].(map[string]any)
		mustDiff(t, "grok sentry 413 error.type", any("request_too_large"), errObj["type"])
		msg, _ := errObj["message"].(string)
		const wantPrefix = "Prompt is too long for the Grok context window; compact and retry. (101 tokens > 100 tokens) [model-gateway:"
		if !strings.HasPrefix(msg, wantPrefix) {
			t.Errorf("grok sentry 413 message = %q, want prefix %q", msg, wantPrefix)
		}
		wantOverflow := map[string]any{
			"phase":         "first_turn",
			"refused_by":    "gateway_sentry",
			"request_bytes": float64(len(body)),
			"tokens":        float64(101),
			"tokens_from":   "previous_turn_usage",
			"limit_tokens":  float64(100),
		}
		mustDiff(t, "grok sentry 413 error.overflow", wantOverflow, errObj["overflow"])
		mustDiff(t, "grok upstream forwards", int64(1), h.grokHits.Load())
		mustDiff(t, "grok auth hits during sentry refusal", authBefore, h.grokAuthHits.Load())

		health := h.healthz()
		served, _ := health["served"].(map[string]any)
		mustDiff(t, "served.grok includes sentry refusal", any(float64(2)), served["grok"])
		audits := h.readAuditLines()
		mustDiff(t, "grok audit entries", 2, len(audits))
	})

	localRefusals := map[string]struct {
		body             string
		wantOverride     string
		wantAuditEntries int
		wantAuditVia     string
		servedKey        string
	}{
		"error: proxy thread refusal 400 is not counted and not audited": {
			body:             `{"model":"claude-gpt-6.1-sol[1m]","thread":{"type":"continue","id":"th_1"},"messages":[{"role":"user","content":"hi"}]}`,
			wantOverride:     "thread_unsupported",
			wantAuditEntries: 0,
			servedKey:        "codex",
		},
		"error: proxy tool schema refusal 400 is not counted and not audited": {
			body:             `{"model":"claude-gpt-6.1-sol[1m]","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"bad_tool","input_schema":{"type":"object","properties":{"x":{"type":"string","pattern":"\\p{InvalidProp}"}}}}]}`,
			wantOverride:     "invalid_tool_schema",
			wantAuditEntries: 0,
			servedKey:        "codex",
		},
		"error: proxy dispatch-unbound 400 logs audit once without counting served": {
			body:             `{"model":"claude-codex-auto","messages":[{"role":"user","content":"no marker"}]}`,
			wantOverride:     "invalid_route",
			wantAuditEntries: 1,
			wantAuditVia:     "dispatch-unbound",
			servedKey:        "codex",
		},
		"error: grok thread refusal 400 is not counted and not audited": {
			body:             `{"model":"claude-grok-4.7[1m]","thread":{"type":"continue","id":"th_1"},"messages":[{"role":"user","content":"hi"}]}`,
			wantOverride:     "thread_unsupported",
			wantAuditEntries: 0,
			servedKey:        "grok",
		},
	}

	for name, tt := range localRefusals {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, harnessConfig{})
			status, _, _ := h.do(http.MethodPost, "/v1/messages", tt.body, http.Header{
				"X-Claude-Code-Session-Id": {"local-refusal-session"},
			})
			mustDiff(t, "local refusal status", http.StatusBadRequest, status)
			mustDiff(t, "anthropic hits", int64(0), h.anthropicHits.Load())
			mustDiff(t, "proxy hits", int64(0), h.proxyHits.Load())
			mustDiff(t, "grok hits", int64(0), h.grokHits.Load())
			mustDiff(t, "grok auth hits", int64(0), h.grokAuthHits.Load())

			health := h.healthz()
			served, _ := health["served"].(map[string]any)
			mustDiff(t, "served."+tt.servedKey, any(float64(0)), served[tt.servedKey])

			h.telemetryRec.Wait()
			audits := h.readAuditLines()
			mustDiff(t, "audit log entries", tt.wantAuditEntries, len(audits))
			if tt.wantAuditVia != "" {
				mustDiff(t, "dispatch-unbound audit via", any(tt.wantAuditVia), audits[0]["via"])
			}

			spans := decodeOTLPSpans(t, h.traceCollector.snapshot())
			mustDiff(t, "route span count", 1, len(spans))
			mustDiff(t, "span status_code", int64(http.StatusBadRequest), spans[0].Attributes["status_code"])
			mustDiff(t, "span status", any(tt.wantOverride), spans[0].Attributes["status"])
		})
	}
}
