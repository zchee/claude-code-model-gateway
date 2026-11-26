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

//go:build process

package proxyreq_test

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

// Request preparation owns the refusal; the running worker must not forward or reroute it.
func TestToolSchemaEngineQuirkHTTPRefusal(t *testing.T) {
	t.Run("prepare", func(t *testing.T) {
		out, w := prepare(t, newHandler(t, t.TempDir(), nil), route.Snapshot{}, "/v1/messages", propertyQuirk, nil)
		if diff := gocmp.Diff(propertyRefusal, w.Body.String()); diff != "" || w.Code != http.StatusBadRequest || out.LocalStatus != w.Code || len(out.Body) != 0 || w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("local status %d, HTTP %d %v, forwardable body %q, refusal (-want +got): %s", out.LocalStatus, w.Code, w.Header(), out.Body, diff)
		}
	})
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	var contacts atomic.Int32
	upstream := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacts.Add(1)
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5.6-sol"}]}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = upstream
	env["CODEX_GATEWAY_GROK_ENDPOINT"] = upstream + "/v1/responses"
	withWorker(t, env, upstream, func(base string) {
		client := &http.Client{Transport: &http.Transport{}}
		defer client.CloseIdleConnections()
		tests := map[string]struct{ body, refusal string }{
			"error: non ASCII property spelling": {propertyQuirk, propertyRefusal},
			"error: capture refusal precedes syntax checking": {
				`{"model":"claude-gpt-5.6-sol[1m]","tools":[{"name":"artifact_tool","input_schema":{"pattern":"(?<x>a)(?:b|(?<x>c))[^\\p{Cc}x]"}}]}`,
				`{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway: Codex tool schema compatibility refused tool artifact_tool at /tools/0/input_schema/pattern; reason=capture-or-unsupported-group. Nothing was forwarded or rerouted."}}`,
			},
		}
		for name, tc := range tests {
			t.Run(name, func(t *testing.T) {
				before := contacts.Load()
				status, body := post(t, client, base, "/v1/messages", tc.body, nil)
				calls := contacts.Load() - before
				if diff := gocmp.Diff(tc.refusal, string(body)); diff != "" || status != 400 || calls != 0 {
					t.Errorf("HTTP %d, upstream requests=%d, body (-want +got): %s", status, calls, diff)
				}
			})
		}
	})
}

// The client reply and matching trace need response usage and telemetry wired into the running worker.
func TestSentryOriginalBytesOverHTTP(t *testing.T) {
	const body = `{ "model": "claude-gpt-5.6-sol", "max_tokens":1, "messages":[{"role":"assistant","content":"prior"},{"role":"user","content":"next"}] }`
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, map[string]string{"CODEX_GATEWAY_COMPACT_TRIGGER": "100", "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"})
	spans := make(chan map[string]string, 1)
	env["CODEX_GATEWAY_TELEMETRY_ENDPOINT"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Resources []struct {
				Scopes []struct {
					Spans []struct {
						Attributes []struct {
							Key   string `json:"key"`
							Value struct {
								String string         `json:"stringValue"`
								Int    jsontext.Value `json:"intValue"`
							} `json:"value"`
						} `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := json.UnmarshalRead(r.Body, &batch); err != nil {
			t.Error(err)
			return
		}
		for _, resource := range batch.Resources {
			for _, scope := range resource.Scopes {
				for _, span := range scope.Spans {
					attrs := map[string]string{}
					for _, a := range span.Attributes {
						if strings.HasPrefix(a.Key, "overflow_") || a.Key == "status_code" {
							value := a.Value.String
							if len(a.Value.Int) != 0 {
								value = strings.Trim(string(a.Value.Int), `"`)
							}
							attrs[a.Key] = value
						}
					}
					if attrs["status_code"] == "413" {
						select {
						case spans <- attrs:
						default:
							t.Error("duplicate sentry span")
						}
					}
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	})) + "/v1/traces"
	var forwarded, other atomic.Int32
	env["CODEX_GATEWAY_ANTHROPIC_UPSTREAM"] = env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			other.Add(1)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	proxy := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-5.6-sol"}]}`)
		case "/v1/messages":
			forwarded.Add(1)
			_, _ = fmt.Fprint(w, `{"type":"message","model":"gpt-5.6-sol","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":150}}`)
		case "/v1/messages/count_tokens":
			forwarded.Add(1)
			_, _ = fmt.Fprint(w, `{"input_tokens":7}`)
		default:
			http.NotFound(w, r)
		}
	}))
	withWorker(t, env, proxy, func(base string) {
		client := &http.Client{Transport: &http.Transport{}}
		defer client.CloseIdleConnections()
		bad := `{"model":"claude-gpt-5.6-sol","tools":[{"name":"fixture","input_schema":` + schema + `}]}`
		if status, b := post(t, client, base, "/v1/messages", bad, nil); status != 400 || !strings.Contains(string(b), "forbidden-schema-applicator") || forwarded.Load() != 0 || other.Load() != 0 {
			t.Fatalf("schema refusal: %d %s, upstreams proxy=%d other=%d", status, b, forwarded.Load(), other.Load())
		}
		if status, b := post(t, client, base, "/v1/messages", body, nil); status != 200 {
			t.Fatalf("initial turn: %d %s", status, b)
		}
		if status, b := post(t, client, base, "/v1/messages/count_tokens?beta=true", body, nil); status != 200 {
			t.Fatalf("count tokens: %d %s", status, b)
		}
		status, b := post(t, client, base, "/v1/messages?beta=true", body, nil)
		if status != 413 || forwarded.Load() != 2 || other.Load() != 0 {
			t.Fatalf("sentry response: %d %s, upstreams proxy=%d other=%d", status, b, forwarded.Load(), other.Load())
		}
		var reply struct {
			Error struct {
				Type     string         `json:"type"`
				Overflow map[string]any `json:"overflow"`
			} `json:"error"`
		}
		if err := json.Unmarshal(b, &reply); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"phase": "continuation", "refused_by": "gateway_sentry", "request_bytes": float64(len(body)), "tokens": float64(150), "tokens_from": "previous_turn_usage", "limit_tokens": float64(100)}
		if diff := gocmp.Diff(want, reply.Error.Overflow); diff != "" || reply.Error.Type != "request_too_large" {
			t.Errorf("error type %s, overflow diff %s", reply.Error.Type, diff)
		}
		select {
		case got := <-spans:
			wantAttrs := map[string]string{"status_code": "413", "overflow_phase": "continuation", "overflow_refused_by": "gateway_sentry", "overflow_request_bytes": strconv.Itoa(len(body)), "overflow_tokens": "150", "overflow_tokens_from": "previous_turn_usage", "overflow_limit_tokens": "100"}
			if diff := gocmp.Diff(wantAttrs, got); diff != "" {
				t.Error(diff)
			}
		case <-time.After(5 * time.Second):
			t.Error("no synthetic 413 route span reached the HTTP collector")
		}
	})
}
