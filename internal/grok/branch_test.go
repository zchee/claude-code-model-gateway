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

package grok_test

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	compactionPrompt  = "You are a helpful AI assistant tasked with summarizing conversations."
	threadRefusalBody = `{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: beta_header:message-threads-2026-08-12; the model-gateway proxy and Grok backends hold no conversation state. Resend this turn with the full message history.","details":{"error_code":"thread_unsupported_request"}}}`
)

type recordedTelemetry struct {
	mu       sync.Mutex
	routes   []grok.RouteInfo
	overflow []grok.OverflowDetails
	finishes []struct {
		status  int
		outcome string
	}
}

func (r *recordedTelemetry) SetRoute(info grok.RouteInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = append(r.routes, info)
}

func (r *recordedTelemetry) SetOverflow(overflow grok.OverflowDetails) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overflow = append(r.overflow, overflow)
}

func (r *recordedTelemetry) Finish(status int, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finishes = append(r.finishes, struct {
		status  int
		outcome string
	}{status: status, outcome: outcome})
}

type recordedUsage struct {
	mu            sync.Mutex
	status        int
	headers       http.Header
	responseBytes int
	events        []string
	jsonBodies    []string
}

func (u *recordedUsage) SetResponse(status int, headers http.Header) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = status
	u.headers = headers.Clone()
}

func (u *recordedUsage) NoteResponseBytes(n int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.responseBytes += n
}

func (u *recordedUsage) ObserveEvent(event jsontext.Value) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.events = append(u.events, string(event))
}

func (u *recordedUsage) ObserveJSON(body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.jsonBodies = append(u.jsonBodies, string(body))
}

func validAuthFixture(t *testing.T, now time.Time) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "auth.json")
	writeFixtureFile(t, file, fmt.Sprintf(`{"https://auth.x.ai::fixture-client":{"key":"fixture-token","refresh_token":"fixture-refresh","expires_at":%d,"oidc_client_id":"fixture-client"}}`, now.Add(time.Hour).UnixMilli()))
	return file
}

func grokSentryTarget(t *testing.T, backendID string, envTrigger float64) sentry.Target {
	t.Helper()
	resolver := policy.NewResolver(policy.ResolverInputs{})
	target, err := sentry.ResolveTarget(resolver, backendID, envTrigger, 0, 0)
	if err != nil {
		t.Fatalf("ResolveTarget(%q): %v", backendID, err)
	}
	return target
}

func postBranch(t *testing.T, srv *httptest.Server, path, body string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, resp.Header.Clone(), string(raw)
}

// TestRoutingGrokCountTokensGenerates verifies the retained behavior that a count_tokens request
// for a native gateway Grok model starts a Responses generation rather than a counting-only call,
// and does not apply exact-path thread refusal.
func TestRoutingGrokCountTokensGenerates(t *testing.T) {
	tests := map[string]struct {
		body string
	}{
		"success: count_tokens forwards translated Responses generation": {
			body: `{"model":"claude-grok-4.5","max_tokens":16,"messages":[{"role":"user","content":"count me"}]}`,
		},
		"success: count_tokens with continuation thread is not refused by exact-path thread check": {
			body: `{"model":"claude-grok-4.5","max_tokens":16,"messages":[{"role":"user","content":"count me"}],"thread":{"type":"continue","previous_message_id":"msg_1"}}`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			now := time.UnixMilli(1_700_000_000_000)
			var mu sync.Mutex
			var gotMethod, gotPath, gotOverride string
			var gotBody map[string]any
			var upstreamHits atomic.Int32

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotOverride = r.Header.Get("X-Grok-Model-Override")
				_ = json.Unmarshal(raw, &gotBody)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_count","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"generated"}]}],"usage":{"input_tokens":5,"output_tokens":3}}`)
			}))
			t.Cleanup(upstream.Close)
			t.Cleanup(upstream.Client().CloseIdleConnections)

			branch := grok.NewBranch(grok.BranchOptions{
				Endpoint:  upstream.URL + "/v1/responses",
				Auth:      grok.NewAuthStore(grok.AuthOptions{File: validAuthFixture(t, now), Now: func() time.Time { return now }}),
				Transport: upstream.Client().Transport,
				Now:       func() time.Time { return now },
			})

			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				branch.Handle(w, r, grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.5",
					BackendModel:    "grok-4.5",
				})
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)

			status, _, respBody := postBranch(t, gw, "/v1/messages/count_tokens", tt.body, nil)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", status, respBody)
			}
			if upstreamHits.Load() != 1 {
				t.Fatalf("upstream hits = %d, want 1", upstreamHits.Load())
			}

			mu.Lock()
			method, path, override, body := gotMethod, gotPath, gotOverride, gotBody
			mu.Unlock()
			if diff := gocmp.Diff(http.MethodPost, method); diff != "" {
				t.Errorf("upstream method (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("/v1/responses", path); diff != "" {
				t.Errorf("upstream path (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("grok-4.5", override); diff != "" {
				t.Errorf("upstream X-Grok-Model-Override (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("grok-4.5", body["model"]); diff != "" {
				t.Errorf("upstream body model (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(float64(16), body["max_output_tokens"]); diff != "" {
				t.Errorf("upstream max_output_tokens (-want +got):\n%s", diff)
			}

			var decoded map[string]any
			if err := json.Unmarshal([]byte(respBody), &decoded); err != nil {
				t.Fatalf("unmarshal response %q: %v", respBody, err)
			}
			if diff := gocmp.Diff("message", decoded["type"]); diff != "" {
				t.Errorf("response type (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBranchForwardJSON(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)

	tests := map[string]struct {
		missingAuth     bool
		expiredShadow   bool
		transportErr    bool
		versionContent  string
		clientHeaders   map[string]string
		requestBody     string
		upstreamStatus  int
		upstreamHeaders map[string]string
		upstreamBody    string
		wantStatus      int
		wantUpstreamHit int32
		wantErrType     string
		wantErrMessage  string
		wantText        string
		wantOutcome     string
		checkSentinels  []string
	}{
		"success: forwards translated request and strips client credential sentinels": {
			versionContent: `{"version":"1.2.3"}`,
			clientHeaders: map[string]string{
				"Authorization":                 "Bearer SENTINEL_CLIENT_AUTH",
				"Proxy-Authorization":           "Basic SENTINEL_PROXY_AUTH",
				"X-Api-Key":                     "SENTINEL_API_KEY",
				"Cookie":                        "session=SENTINEL_COOKIE",
				"X-Model-Gateway-Control-Token": "SENTINEL_CONTROL_TOKEN",
			},
			requestBody:    `{"model":"claude-grok-4.7[1m]","max_tokens":10,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus: http.StatusOK,
			upstreamHeaders: map[string]string{
				"X-Upstream-Echo": "kept",
				"Connection":      "keep-alive",
				"Keep-Alive":      "timeout=5",
			},
			upstreamBody:    `{"id":"resp_fixture","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`,
			wantStatus:      http.StatusOK,
			wantUpstreamHit: 1,
			wantText:        "hello",
			checkSentinels: []string{
				"SENTINEL_CLIENT_AUTH",
				"SENTINEL_PROXY_AUTH",
				"SENTINEL_API_KEY",
				"SENTINEL_COOKIE",
				"SENTINEL_CONTROL_TOKEN",
			},
		},
		"success: translates unparseable 200 non-stream body from null": {
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusOK,
			upstreamBody:    `not valid json`,
			wantStatus:      http.StatusOK,
			wantUpstreamHit: 1,
		},
		"error: translation failure before committing headers returns 502": {
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusOK,
			upstreamBody:    `{"id":"resp_bad","status":"completed","output":[],"usage":{"input_tokens":{"toString":1}}}`,
			wantStatus:      http.StatusBadGateway,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantOutcome:     "upstream_error",
		},
		"error: upstream 426 returns outdated CLI version message": {
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusUpgradeRequired,
			upstreamBody:    `{"error":{"message":"upgrade required"}}`,
			wantStatus:      http.StatusUpgradeRequired,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: Grok CLI version header is outdated. Update the grok CLI, then retry.",
		},
		"error: upstream 401 maps to authentication_error": {
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusUnauthorized,
			upstreamBody:    `{"error":{"message":"token rejected"}}`,
			wantStatus:      http.StatusUnauthorized,
			wantUpstreamHit: 1,
			wantErrType:     "authentication_error",
			wantErrMessage:  "model-gateway: token rejected",
		},
		"error: upstream 413 passes through as api_error without overflow normalization": {
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusRequestEntityTooLarge,
			upstreamBody:    `{"error":{"message":"context length exceeded"}}`,
			wantStatus:      http.StatusRequestEntityTooLarge,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: context length exceeded",
		},
		"error: upstream 500 with string error preserves detail": {
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusInternalServerError,
			upstreamBody:    `{"error":"upstream overloaded"}`,
			wantStatus:      http.StatusInternalServerError,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: upstream overloaded",
		},
		"error: upstream 503 with non-json body uses status fallback": {
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusServiceUnavailable,
			upstreamBody:    `bad gateway html`,
			wantStatus:      http.StatusServiceUnavailable,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: Grok upstream returned 503",
		},
		"error: missing auth returns 401 authentication_error without contacting upstream": {
			missingAuth:     true,
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusUnauthorized,
			wantUpstreamHit: 0,
			wantErrType:     "authentication_error",
			wantErrMessage:  "model-gateway: Grok CLI auth is missing. Run `grok` and log in again.",
			wantOutcome:     "authentication_error",
		},
		"error: shadow mode expired auth returns 401 without contacting upstream": {
			expiredShadow:   true,
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusUnauthorized,
			wantUpstreamHit: 0,
			wantErrType:     "authentication_error",
			wantErrMessage:  "model-gateway: Grok CLI auth cannot refresh. Run `grok` and log in again.",
			wantOutcome:     "authentication_error",
		},
		"error: upstream transport failure returns 502 api_error": {
			transportErr:    true,
			requestBody:     `{"model":"claude-grok-4.5","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusBadGateway,
			wantUpstreamHit: 0,
			wantErrType:     "api_error",
			wantOutcome:     "upstream_error",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			var mu sync.Mutex
			var seenHeaders http.Header
			var seenBody string
			var upstreamHits atomic.Int32

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				seenHeaders = r.Header.Clone()
				seenBody = string(raw)
				mu.Unlock()
				for k, v := range tt.upstreamHeaders {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.upstreamStatus)
				_, _ = io.WriteString(w, tt.upstreamBody)
			}))
			t.Cleanup(upstream.Close)
			t.Cleanup(upstream.Client().CloseIdleConnections)

			authFile := filepath.Join(t.TempDir(), "auth.json")
			switch {
			case tt.missingAuth:
				// Leave authFile absent.
			case tt.expiredShadow:
				writeFixtureFile(t, authFile, `{"https://auth.x.ai::fixture-client":{"key":"fixture-expired","refresh_token":"fixture-refresh","expires_at":0,"oidc_client_id":"fixture-client"}}`)
			default:
				authFile = validAuthFixture(t, now)
			}

			var versionFile string
			if tt.versionContent != "" {
				versionFile = filepath.Join(t.TempDir(), "version.json")
				writeFixtureFile(t, versionFile, tt.versionContent)
			}

			endpoint := upstream.URL + "/v1/responses"
			tr := upstream.Client().Transport
			if tt.transportErr {
				// Use an already-closed ephemeral listener so RoundTrip fails immediately on loopback.
				dead := httptest.NewServer(http.NewServeMux())
				endpoint = dead.URL + "/v1/responses"
				dead.Close()
			}

			tel := &recordedTelemetry{}
			usageObs := &recordedUsage{}
			branch := grok.NewBranch(grok.BranchOptions{
				Endpoint:    endpoint,
				VersionFile: versionFile,
				Auth: grok.NewAuthStore(grok.AuthOptions{
					File:      authFile,
					Transport: tr,
					Now:       func() time.Time { return now },
					Shadow:    tt.expiredShadow,
				}),
				Transport: tr,
				Now:       func() time.Time { return now },
			})

			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				branch.Handle(w, r, grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.7[1m]",
					BackendModel:    "grok-4.7",
					Telemetry:       tel,
					Usage:           usageObs,
				})
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)

			status, respHeaders, respBody := postBranch(t, gw, "/v1/messages", tt.requestBody, tt.clientHeaders)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", status, tt.wantStatus, respBody)
			}
			if got := upstreamHits.Load(); got != tt.wantUpstreamHit {
				t.Errorf("upstream hits = %d, want %d", got, tt.wantUpstreamHit)
			}
			if got := respHeaders.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}

			var decoded map[string]any
			if err := json.Unmarshal([]byte(respBody), &decoded); err != nil {
				t.Fatalf("unmarshal response %q: %v", respBody, err)
			}

			if tt.wantErrType != "" {
				errObj, _ := decoded["error"].(map[string]any)
				if diff := gocmp.Diff(tt.wantErrType, errObj["type"]); diff != "" {
					t.Errorf("error.type (-want +got):\n%s", diff)
				}
				if tt.wantErrMessage != "" {
					if diff := gocmp.Diff(tt.wantErrMessage, errObj["message"]); diff != "" {
						t.Errorf("error.message (-want +got):\n%s", diff)
					}
				} else if msg, _ := errObj["message"].(string); !strings.HasPrefix(msg, "model-gateway: Grok upstream failed:") {
					t.Errorf("error.message = %q, want prefix model-gateway: Grok upstream failed:", msg)
				}
				if errObj["overflow"] != nil {
					t.Errorf("Grok error must not attach normalized overflow field: %#v", errObj["overflow"])
				}
			} else {
				if diff := gocmp.Diff("message", decoded["type"]); diff != "" {
					t.Errorf("response type (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("claude-grok-4.7[1m]", decoded["model"]); diff != "" {
					t.Errorf("response model (-want +got):\n%s", diff)
				}
				if tt.wantText != "" {
					blocks, _ := decoded["content"].([]any)
					if len(blocks) != 1 {
						t.Fatalf("content len = %d, want 1: %#v", len(blocks), blocks)
					}
					first, _ := blocks[0].(map[string]any)
					if diff := gocmp.Diff(tt.wantText, first["text"]); diff != "" {
						t.Errorf("content[0].text (-want +got):\n%s", diff)
					}
					if diff := gocmp.Diff("kept", respHeaders.Get("X-Upstream-Echo")); diff != "" {
						t.Errorf("X-Upstream-Echo header (-want +got):\n%s", diff)
					}
					if got := respHeaders.Get("Keep-Alive"); got != "" {
						t.Errorf("Keep-Alive hop-by-hop header leaked: %q", got)
					}
					if diff := gocmp.Diff(strconv.Itoa(len(respBody)), respHeaders.Get("Content-Length")); diff != "" {
						t.Errorf("Content-Length (-want +got):\n%s", diff)
					}
					usageObs.mu.Lock()
					obsStatus, obsBytes, obsJSON := usageObs.status, usageObs.responseBytes, len(usageObs.jsonBodies)
					usageObs.mu.Unlock()
					if obsStatus != http.StatusOK || obsBytes != len(tt.upstreamBody) || obsJSON != 1 {
						t.Errorf("usage observer got status=%d bytes=%d jsonCount=%d, want 200/%d/1", obsStatus, obsBytes, obsJSON, len(tt.upstreamBody))
					}
				}
			}

			if len(tt.checkSentinels) > 0 {
				mu.Lock()
				hdrs, body := seenHeaders, seenBody
				mu.Unlock()
				if diff := gocmp.Diff("Bearer fixture-token", hdrs.Get("Authorization")); diff != "" {
					t.Errorf("upstream Authorization (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("1.2.3", hdrs.Get("X-Grok-Client-Version")); diff != "" {
					t.Errorf("upstream X-Grok-Client-Version (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("xai-grok-build/1.2.3", hdrs.Get("User-Agent")); diff != "" {
					t.Errorf("upstream User-Agent (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("xai-grok-cli", hdrs.Get("X-Grok-Client-Identifier")); diff != "" {
					t.Errorf("upstream X-Grok-Client-Identifier (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("grok-4.7", hdrs.Get("X-Grok-Model-Override")); diff != "" {
					t.Errorf("upstream X-Grok-Model-Override (-want +got):\n%s", diff)
				}
				var flatHeaders strings.Builder
				for k, vals := range hdrs {
					flatHeaders.WriteString(k)
					flatHeaders.WriteByte(':')
					flatHeaders.WriteString(strings.Join(vals, ","))
					flatHeaders.WriteByte('\n')
				}
				for _, sentinel := range tt.checkSentinels {
					if strings.Contains(flatHeaders.String(), sentinel) {
						t.Errorf("client sentinel %q leaked into upstream headers: %s", sentinel, flatHeaders.String())
					}
					if strings.Contains(body, sentinel) {
						t.Errorf("client sentinel %q leaked into upstream body: %s", sentinel, body)
					}
				}
			}

			tel.mu.Lock()
			finishes := append([]struct {
				status  int
				outcome string
			}(nil), tel.finishes...)
			tel.mu.Unlock()
			if len(finishes) == 0 {
				t.Fatalf("telemetry Finish was never called")
			}
			last := finishes[len(finishes)-1]
			if last.status != tt.wantStatus {
				t.Errorf("telemetry finish status = %d, want %d", last.status, tt.wantStatus)
			}
			if tt.wantOutcome != "" && last.outcome != tt.wantOutcome {
				t.Errorf("telemetry finish outcome = %q, want %q", last.outcome, tt.wantOutcome)
			}
		})
	}
}

func TestBranchThreadRefusal(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)

	tests := map[string]struct {
		body            string
		configureAuth   bool
		wantStatus      int
		wantUpstreamHit int32
	}{
		"error: continuation refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":{"type":"continue","previous_message_id":"message_123"}}`,
			configureAuth:   false,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"error: null thread refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":null}`,
			configureAuth:   false,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"error: string thread refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":"continue"}`,
			configureAuth:   false,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"error: array thread refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":[]}`,
			configureAuth:   false,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"error: unknown thread type refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":{"type":"resume"}}`,
			configureAuth:   false,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"success: thread create is allowed and forwarded": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":{"type":"create"}}`,
			configureAuth:   true,
			wantStatus:      http.StatusOK,
			wantUpstreamHit: 1,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			var upstreamHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_ok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			t.Cleanup(upstream.Close)
			t.Cleanup(upstream.Client().CloseIdleConnections)

			authFile := filepath.Join(t.TempDir(), "missing-auth.json")
			if tt.configureAuth {
				authFile = validAuthFixture(t, now)
			}

			tel := &recordedTelemetry{}
			branch := grok.NewBranch(grok.BranchOptions{
				Endpoint:  upstream.URL + "/v1/responses",
				Auth:      grok.NewAuthStore(grok.AuthOptions{File: authFile, Now: func() time.Time { return now }}),
				Transport: upstream.Client().Transport,
				Now:       func() time.Time { return now },
			})

			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				branch.Handle(w, r, grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.5",
					BackendModel:    "grok-4.5",
					Telemetry:       tel,
				})
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)

			status, hdrs, respBody := postBranch(t, gw, "/v1/messages", tt.body, nil)
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", status, tt.wantStatus, respBody)
			}
			if got := upstreamHits.Load(); got != tt.wantUpstreamHit {
				t.Errorf("upstream hits = %d, want %d", got, tt.wantUpstreamHit)
			}
			if tt.wantStatus == http.StatusBadRequest {
				if got := hdrs.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				if diff := gocmp.Diff(threadRefusalBody, respBody); diff != "" {
					t.Errorf("thread refusal body (-want +got):\n%s", diff)
				}
				tel.mu.Lock()
				finishes := append([]struct {
					status  int
					outcome string
				}(nil), tel.finishes...)
				tel.mu.Unlock()
				if len(finishes) != 1 || finishes[0].status != http.StatusBadRequest || finishes[0].outcome != "thread_unsupported" {
					t.Errorf("telemetry finishes = %#v, want [{400 thread_unsupported}]", finishes)
				}
			}
		})
	}
}

func TestBranchStream(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)

	t.Run("success: translates uninterrupted SSE stream and reports usage", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		ssePayload := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_stream","usage":{"input_tokens":10,"output_tokens":0}}}`,
			``,
			`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}`,
			``,
			`data: {"type":"response.completed","response":{"id":"resp_stream","output":[],"usage":{"input_tokens":10,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":1}}}}`,
			``,
			`data: [DONE]`,
			``,
			``,
		}, "\n")

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("X-Upstream-Header", "present")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, ssePayload)
		}))
		t.Cleanup(upstream.Close)
		t.Cleanup(upstream.Client().CloseIdleConnections)

		usageObs := &recordedUsage{}
		branch := grok.NewBranch(grok.BranchOptions{
			Endpoint:  upstream.URL + "/v1/responses",
			Auth:      grok.NewAuthStore(grok.AuthOptions{File: validAuthFixture(t, now), Now: func() time.Time { return now }}),
			Transport: upstream.Client().Transport,
			Now:       func() time.Time { return now },
		})

		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
				Usage:           usageObs,
			})
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		status, hdrs, body := postBranch(t, gw, "/v1/messages", `{"model":"claude-grok-4.5[1m]","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", status, body)
		}
		if got := hdrs.Get("Content-Type"); got != "text/event-stream" {
			t.Errorf("Content-Type = %q, want text/event-stream", got)
		}
		if got := hdrs.Get("X-Upstream-Header"); got != "present" {
			t.Errorf("X-Upstream-Header = %q, want present", got)
		}
		for _, want := range []string{`"type":"message_start"`, `"model":"claude-grok-4.5[1m]"`, `"type":"text_delta","text":"hi"`, `"stop_reason":"end_turn"`, `"type":"message_stop"`} {
			if !strings.Contains(body, want) {
				t.Errorf("stream body missing %s:\n%s", want, body)
			}
		}
		usageObs.mu.Lock()
		obsBytes, obsEvents := usageObs.responseBytes, len(usageObs.events)
		usageObs.mu.Unlock()
		if obsBytes != len(ssePayload) {
			t.Errorf("usage responseBytes = %d, want %d", obsBytes, len(ssePayload))
		}
		if obsEvents != 2 {
			t.Errorf("usage observed events = %d, want 2 (response.created + response.completed)", obsEvents)
		}
	})

	t.Run("success: orderly EOF without response.completed emits synthetic end_turn stop", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		ssePayload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_eof\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, ssePayload)
		}))
		t.Cleanup(upstream.Close)
		t.Cleanup(upstream.Client().CloseIdleConnections)

		branch := grok.NewBranch(grok.BranchOptions{
			Endpoint:  upstream.URL + "/v1/responses",
			Auth:      grok.NewAuthStore(grok.AuthOptions{File: validAuthFixture(t, now), Now: func() time.Time { return now }}),
			Transport: upstream.Client().Transport,
			Now:       func() time.Time { return now },
		})

		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5",
				BackendModel:    "grok-4.5",
			})
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		status, _, body := postBranch(t, gw, "/v1/messages", `{"model":"claude-grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		for _, want := range []string{`"type":"message_start"`, `"text":"partial"`, `"stop_reason":"end_turn"`, `"type":"message_stop"`} {
			if !strings.Contains(body, want) {
				t.Errorf("stream body missing %s:\n%s", want, body)
			}
		}
	})

	t.Run("error: upstream closing mid-chunk aborts client response", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			frame := `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n"
			raw := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(frame), frame)
			_, _ = io.WriteString(conn, raw)
			// Close without writing the terminating 0-length chunk.
		}()
		t.Cleanup(func() {
			_ = ln.Close()
			<-serverDone
		})

		upTransport := &http.Transport{}
		t.Cleanup(upTransport.CloseIdleConnections)

		branch := grok.NewBranch(grok.BranchOptions{
			Endpoint:  "http://" + ln.Addr().String() + "/v1/responses",
			Auth:      grok.NewAuthStore(grok.AuthOptions{File: validAuthFixture(t, now), Now: func() time.Time { return now }}),
			Transport: upTransport,
			Now:       func() time.Time { return now },
		})

		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
			})
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"model":"claude-grok-4.5[1m]","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("POST /v1/messages: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var partial bytes.Buffer
		_, readErr := io.Copy(&partial, resp.Body)
		if readErr == nil {
			t.Fatalf("expected truncated stream transport error, got clean EOF; body = %q", partial.String())
		}
		if !strings.Contains(partial.String(), `"type":"message_start"`) {
			t.Errorf("partial stream body missing message_start: %q", partial.String())
		}
	})
}

func assertGrokSentry413(t *testing.T, status int, hdrs http.Header, body string, wantTokens, wantLimit, wantReqBytes int, wantPhase string) {
	t.Helper()
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", status, body)
	}
	if got := hdrs.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := hdrs.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %d", got, len(body))
	}
	var decoded struct {
		Type  string `json:"type"`
		Error struct {
			Type     string               `json:"type"`
			Message  string               `json:"message"`
			Overflow grok.OverflowDetails `json:"overflow"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("unmarshal 413 body %q: %v", body, err)
	}
	if diff := gocmp.Diff("error", decoded.Type); diff != "" {
		t.Errorf("type (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff("request_too_large", decoded.Error.Type); diff != "" {
		t.Errorf("error.type (-want +got):\n%s", diff)
	}
	wantPrefix := fmt.Sprintf("Prompt is too long for the Grok context window; compact and retry. (%d tokens > %d tokens) [model-gateway: phase %s, refused by gateway_sentry;", wantTokens, wantLimit, wantPhase)
	if !strings.HasPrefix(decoded.Error.Message, wantPrefix) {
		t.Errorf("error.message = %q, want prefix %q", decoded.Error.Message, wantPrefix)
	}
	wantOverflow := grok.OverflowDetails{
		Phase:        wantPhase,
		RefusedBy:    "gateway_sentry",
		RequestBytes: wantReqBytes,
		Tokens:       new(float64(wantTokens)),
		TokensFrom:   new("previous_turn_usage"),
		LimitTokens:  float64(wantLimit),
	}
	if diff := gocmp.Diff(wantOverflow, decoded.Error.Overflow); diff != "" {
		t.Errorf("error.overflow (-want +got):\n%s", diff)
	}
}

func TestBranchSentry(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	ordinaryJSON := `{"model":"claude-grok-4.5[1m]","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`
	ordinarySSE := `{"model":"claude-grok-4.5[1m]","stream":true,"max_tokens":1,"messages":[{"role":"user","content":"test"}]}`
	compactionSSE := `{"model":"claude-grok-4.5[1m]","stream":true,"max_tokens":1,"system":"` + compactionPrompt + `","messages":[{"role":"user","content":"test"}]}`
	sessionHeaders := map[string]string{"X-Claude-Code-Session-Id": "grok-sentry-session"}

	sseEvent := func(eventType string, inputTokens int) string {
		return fmt.Sprintf("data: {\"type\":%q,\"response\":{\"id\":\"resp_s\",\"output\":[],\"usage\":{\"input_tokens\":%d}}}\n\n", eventType, inputTokens)
	}

	newHarness := func(t *testing.T, enabled bool, answers []http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
		t.Helper()
		var idx atomic.Int32
		var forwarded atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			n := int(idx.Add(1)) - 1
			forwarded.Add(1)
			if n >= len(answers) {
				t.Errorf("unexpected upstream request #%d", n+1)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			answers[n](w, r)
		}))
		t.Cleanup(upstream.Close)
		t.Cleanup(upstream.Client().CloseIdleConnections)

		state := &sentry.State{}
		target := grokSentryTarget(t, "grok-4.5", 100)
		branch := grok.NewBranch(grok.BranchOptions{
			Endpoint:      upstream.URL + "/v1/responses",
			Auth:          grok.NewAuthStore(grok.AuthOptions{File: validAuthFixture(t, now), Now: func() time.Time { return now }}),
			Transport:     upstream.Client().Transport,
			Sentry:        state,
			SentryEnabled: enabled,
			Now:           func() time.Time { return now },
		})

		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
				SentryTarget:    target,
			})
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)
		return gw, &forwarded
	}

	jsonResp := func(status int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	sseResp := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, body)
		}
	}

	t.Run("success: JSON arms sentry and refuses messages and count_tokens with exact 413", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		gw, forwarded := newHarness(t, true, []http.HandlerFunc{
			jsonResp(http.StatusOK, `{"id":"resp_1","output":[],"usage":{"input_tokens":101}}`),
			jsonResp(http.StatusOK, `{"id":"resp_2","status":"completed","output":[],"usage":{"input_tokens":104}}`),
		})

		status, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders)
		if status != http.StatusOK {
			t.Fatalf("first turn status = %d, want 200", status)
		}

		// Refusal applies to /v1/messages/count_tokens (messages prefix) as well as /v1/messages.
		status, hdrs, body := postBranch(t, gw, "/v1/messages/count_tokens", ordinaryJSON, sessionHeaders)
		assertGrokSentry413(t, status, hdrs, body, 101, 100, len(ordinaryJSON), "first_turn")
		if forwarded.Load() != 1 {
			t.Fatalf("forwarded = %d, want 1", forwarded.Load())
		}

		// Latched turn passes and rearms on completed JSON.
		status, _, _ = postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders)
		if status != http.StatusOK {
			t.Fatalf("latched turn status = %d, want 200", status)
		}
		status, hdrs, body = postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders)
		assertGrokSentry413(t, status, hdrs, body, 104, 100, len(ordinaryJSON), "first_turn")
		if forwarded.Load() != 2 {
			t.Fatalf("forwarded = %d, want 2", forwarded.Load())
		}
	})

	t.Run("success: JSON rearms only when status is completed and request is not compaction", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		gw, forwarded := newHarness(t, true, []http.HandlerFunc{
			jsonResp(http.StatusOK, `{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":101}}`),
			// While latched: status="incomplete" must NOT rearm.
			jsonResp(http.StatusOK, `{"id":"resp_2","status":"incomplete","output":[],"usage":{"input_tokens":102}}`),
			// While latched: missing status must NOT rearm.
			jsonResp(http.StatusOK, `{"id":"resp_3","output":[],"usage":{"input_tokens":103}}`),
			// While latched: compaction with status="completed" must NOT rearm.
			jsonResp(http.StatusOK, `{"id":"resp_4","status":"completed","output":[],"usage":{"input_tokens":104}}`),
			// While latched: non-compaction with status="completed" DOES rearm.
			jsonResp(http.StatusOK, `{"id":"resp_5","status":"completed","output":[],"usage":{"input_tokens":105}}`),
		})

		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders); s != http.StatusOK {
			t.Fatalf("turn 1 status = %d, want 200", s)
		}
		s, h, b := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders)
		assertGrokSentry413(t, s, h, b, 101, 100, len(ordinaryJSON), "first_turn")

		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders); s != http.StatusOK {
			t.Fatalf("incomplete JSON turn status = %d, want 200", s)
		}
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders); s != http.StatusOK {
			t.Fatalf("missing-status JSON turn after incomplete must stay latched, got %d", s)
		}
		if s, _, _ := postBranch(t, gw, "/v1/messages", compactionSSE, sessionHeaders); s != http.StatusOK {
			t.Fatalf("compaction JSON turn status = %d, want 200", s)
		}
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders); s != http.StatusOK {
			t.Fatalf("ordinary turn after compaction JSON must stay latched, got %d", s)
		}
		s, h, b = postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders)
		assertGrokSentry413(t, s, h, b, 105, 100, len(ordinaryJSON), "first_turn")
		if forwarded.Load() != 5 {
			t.Fatalf("forwarded = %d, want 5", forwarded.Load())
		}
	})

	t.Run("success: SSE observes per-frame usage without completion and rearms only deliverable completed non-compaction", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		compactionStarted := make(chan struct{})
		releaseCompaction := make(chan struct{})
		var releaseOnce sync.Once
		releaseNow := func() { releaseOnce.Do(func() { close(releaseCompaction) }) }

		gw, forwarded := newHarness(t, true, []http.HandlerFunc{
			// 1: completed ordinary SSE arms sentry at 101.
			sseResp(sseEvent("response.completed", 101)),
			// 2: running compaction streams usage mid-flight, then finishes completed at 104.
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, sseEvent("response.created", 102))
				_ = http.NewResponseController(w).Flush()
				close(compactionStarted)
				select {
				case <-releaseCompaction:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, sseEvent("response.completed", 104))
			},
			// 3: concurrent ordinary request while compaction is running (EOF without response.completed -> not deliverable).
			sseResp("data: not-json\n\ndata: [DONE]\n\n" + sseEvent("response.created", 103)),
			// 4: stream with response.failed AND response.completed -> sawError -> not deliverable.
			sseResp(sseEvent("response.failed", 105) + sseEvent("response.completed", 105)),
			// 5: stream with response.incomplete AND response.completed -> sawError -> not deliverable.
			sseResp(sseEvent("response.incomplete", 106) + sseEvent("response.completed", 106)),
			// 6: deliverable completed ordinary SSE rearms at 107.
			sseResp(sseEvent("response.completed", 107)),
			// 7: compaction bypasses armed sentry without consuming refusal.
			sseResp(sseEvent("response.completed", 108)),
			// 8: low usage below watermark resets latch.
			sseResp(sseEvent("response.completed", 20)),
		})

		// 1. Arm and fire latch.
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinarySSE, sessionHeaders); s != http.StatusOK {
			t.Fatalf("turn 1 status = %d, want 200", s)
		}
		s, h, b := postBranch(t, gw, "/v1/messages", ordinarySSE, sessionHeaders)
		assertGrokSentry413(t, s, h, b, 101, 100, len(ordinarySSE), "first_turn")

		// 2. Start compaction SSE; verify mid-stream chunk does NOT rearm while running.
		compactDone := make(chan struct{})
		var compactStatus int
		go func() {
			defer close(compactDone)
			compactStatus, _, _ = postBranch(t, gw, "/v1/messages", compactionSSE, sessionHeaders)
		}()
		t.Cleanup(func() {
			releaseNow()
			<-compactDone
		})
		select {
		case <-compactionStarted:
		case <-compactDone:
		}
		// 3. Ordinary request during running compaction stays latched (200), and its own EOF without completed does not rearm.
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinarySSE, sessionHeaders); s != http.StatusOK {
			t.Fatalf("ordinary turn during running compaction status = %d, want 200", s)
		}
		releaseNow()
		<-compactDone
		if compactStatus != http.StatusOK {
			t.Fatalf("compaction status = %d, want 200", compactStatus)
		}

		// 4. After high compaction completed, still latched -> 200 (receives response.failed + completed -> still latched).
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinarySSE, sessionHeaders); s != http.StatusOK {
			t.Fatalf("turn after compaction completion status = %d, want 200", s)
		}
		// 5. After response.failed, still latched -> 200 (receives response.incomplete + completed -> still latched).
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinarySSE, sessionHeaders); s != http.StatusOK {
			t.Fatalf("turn after response.failed status = %d, want 200", s)
		}
		// 6. After response.incomplete, still latched -> 200 (receives clean response.completed at 107 -> rearms!).
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinarySSE, sessionHeaders); s != http.StatusOK {
			t.Fatalf("turn after response.incomplete status = %d, want 200", s)
		}
		// 7. Compaction bypasses the rearmed refusal and updates usage to 108 without consuming refusal.
		if s, _, _ := postBranch(t, gw, "/v1/messages", compactionSSE, sessionHeaders); s != http.StatusOK {
			t.Fatalf("compaction bypass status = %d, want 200", s)
		}
		// Ordinary request now hits synthetic 413 reporting 108!
		s, h, b = postBranch(t, gw, "/v1/messages", ordinarySSE, sessionHeaders)
		assertGrokSentry413(t, s, h, b, 108, 100, len(ordinarySSE), "first_turn")
		// 8. Low-usage compaction succeeds and resets watermark.
		if s, _, _ := postBranch(t, gw, "/v1/messages", compactionSSE, sessionHeaders); s != http.StatusOK {
			t.Fatalf("low-usage compaction status = %d, want 200", s)
		}
		if forwarded.Load() != 8 {
			t.Fatalf("forwarded = %d, want 8", forwarded.Load())
		}
	})

	t.Run("success: sentry disabled never refuses", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		gw, forwarded := newHarness(t, false, []http.HandlerFunc{
			jsonResp(http.StatusOK, `{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":500}}`),
			jsonResp(http.StatusOK, `{"id":"resp_2","status":"completed","output":[],"usage":{"input_tokens":500}}`),
		})

		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders); s != http.StatusOK {
			t.Fatalf("turn 1 status = %d, want 200", s)
		}
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders); s != http.StatusOK {
			t.Fatalf("turn 2 status = %d, want 200", s)
		}
		if forwarded.Load() != 2 {
			t.Fatalf("forwarded = %d, want 2", forwarded.Load())
		}
	})

	t.Run("success: separate sessions and agents stay isolated", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		gw, forwarded := newHarness(t, true, []http.HandlerFunc{
			jsonResp(http.StatusOK, `{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":101}}`),
			jsonResp(http.StatusOK, `{"id":"resp_2","status":"completed","output":[],"usage":{"input_tokens":50}}`),
			jsonResp(http.StatusOK, `{"id":"resp_3","status":"completed","output":[],"usage":{"input_tokens":50}}`),
		})

		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders); s != http.StatusOK {
			t.Fatalf("turn 1 status = %d, want 200", s)
		}
		// Sibling agent in same session is not refused.
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, map[string]string{
			"X-Claude-Code-Session-Id": "grok-sentry-session",
			"X-Claude-Code-Agent-Id":   "sibling",
		}); s != http.StatusOK {
			t.Fatalf("sibling agent status = %d, want 200", s)
		}
		// Different session is not refused.
		if s, _, _ := postBranch(t, gw, "/v1/messages", ordinaryJSON, map[string]string{
			"X-Claude-Code-Session-Id": "other-session",
		}); s != http.StatusOK {
			t.Fatalf("other session status = %d, want 200", s)
		}
		// Original session/agent IS refused.
		s, h, b := postBranch(t, gw, "/v1/messages", ordinaryJSON, sessionHeaders)
		assertGrokSentry413(t, s, h, b, 101, 100, len(ordinaryJSON), "first_turn")
		if forwarded.Load() != 3 {
			t.Fatalf("forwarded = %d, want 3", forwarded.Load())
		}
	})
}
