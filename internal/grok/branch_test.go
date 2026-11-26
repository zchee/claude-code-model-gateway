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
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// Compile-time check that *usage.Capture directly satisfies grok.UsageObserver with no adapter.
var _ grok.UsageObserver = (*usage.Capture)(nil)

const (
	compactionPrompt               = "You are a helpful AI assistant tasked with summarizing conversations."
	threadRefusalBody              = `{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: beta_header:message-threads-2026-08-12; the model-gateway proxy and Grok backends hold no conversation state. Resend this turn with the full message history.","details":{"error_code":"thread_unsupported_request"}}}`
	sanitizedTranslationErrMessage = "model-gateway: Grok response could not be translated."
)

type usageSpy struct {
	mu            sync.Mutex
	responseCalls int
	status        int
	headers       http.Header
	responseBytes int64
	events        []jsonx.WireValue
	jsonBodies    [][]byte
	finishCalls   int
	onEvent       chan jsonx.WireValue
}

func (u *usageSpy) SetResponse(status int, headers http.Header) {
	u.mu.Lock()
	u.responseCalls++
	u.status = status
	u.headers = headers.Clone()
	u.mu.Unlock()
}

func (u *usageSpy) NoteResponseBytes(n int64) {
	u.mu.Lock()
	u.responseBytes += n
	u.mu.Unlock()
}

func (u *usageSpy) ObserveEvent(event jsonx.WireValue) {
	u.mu.Lock()
	u.events = append(u.events, event)
	ch := u.onEvent
	u.mu.Unlock()
	if ch != nil {
		select {
		case ch <- event:
		default:
		}
	}
}

func (u *usageSpy) ObserveJSON(body []byte) {
	u.mu.Lock()
	u.jsonBodies = append(u.jsonBodies, slices.Clone(body))
	u.mu.Unlock()
}

// Finish is intentionally not part of grok.UsageObserver; defining it here detects any
// mistaken type-assertion attempt to finish or emit a usage record from the Grok branch.
func (u *usageSpy) Finish() *usage.Record {
	u.mu.Lock()
	u.finishCalls++
	u.mu.Unlock()
	return nil
}

type usageSnapshot struct {
	responseCalls int
	status        int
	headers       http.Header
	responseBytes int64
	events        []string
	jsonBodies    []string
	finishCalls   int
}

func (u *usageSpy) snapshot(t *testing.T) usageSnapshot {
	t.Helper()
	u.mu.Lock()
	snap := usageSnapshot{
		responseCalls: u.responseCalls,
		status:        u.status,
		headers:       u.headers.Clone(),
		responseBytes: u.responseBytes,
		finishCalls:   u.finishCalls,
	}
	rawEvents := slices.Clone(u.events)
	rawBodies := make([][]byte, len(u.jsonBodies))
	for i, b := range u.jsonBodies {
		rawBodies[i] = slices.Clone(b)
	}
	u.mu.Unlock()

	if len(rawEvents) > 0 {
		snap.events = make([]string, len(rawEvents))
		for i, ev := range rawEvents {
			enc, err := jsonx.EncodeWire(ev)
			if err != nil {
				t.Fatalf("encode observed usage event #%d: %v", i, err)
			}
			snap.events[i] = string(enc)
		}
	}
	if len(rawBodies) > 0 {
		snap.jsonBodies = make([]string, len(rawBodies))
		for i, b := range rawBodies {
			snap.jsonBodies[i] = string(b)
		}
	}
	return snap
}

func newLoopbackClient(t *testing.T, allowedAddrs ...string) *upstream.Client {
	t.Helper()
	var dialer net.Dialer
	client := upstream.NewClient(upstream.Options{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !slices.Contains(allowedAddrs, addr) {
				t.Errorf("blocked unapproved inference dial to %q", addr)
				return nil, fmt.Errorf("blocked unapproved dial to %q", addr)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
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

func doBranchPost(ctx context.Context, client *http.Client, targetURL, body string, headers map[string]string) (int, http.Header, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(body))
	if err != nil {
		return 0, nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, resp.Header.Clone(), "", err
	}
	return resp.StatusCode, resp.Header.Clone(), string(raw), nil
}

func postBranch(t *testing.T, srv *httptest.Server, path, body string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	status, hdrs, respBody, err := doBranchPost(t.Context(), srv.Client(), srv.URL+path, body, headers)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return status, hdrs, respBody
}

func decodeFullJSONObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("unmarshal JSON response (len=%d): %v", len(raw), err)
	}
	return decoded
}

// buildEncodeOverflowUpstreamBody constructs a valid Grok JSON response whose TranslateResponseChecked
// succeeds (arguments contains 9999 nested arrays, under the 10000 decode limit) while EncodeWire of
// the translated message fails with *jsonx.WireDepthError because the output wraps input at depth 3.
func buildEncodeOverflowUpstreamBody(t *testing.T, now time.Time) string {
	t.Helper()
	const depth = 9999
	nested := strings.Repeat("[", depth) + strings.Repeat("]", depth)
	body := fmt.Sprintf(`{"id":"resp_deep","status":"completed","output":[{"type":"function_call","call_id":"call_deep","name":"deep_tool","arguments":%q}],"usage":{"input_tokens":1,"output_tokens":1}}`, nested)

	wire, err := jsonx.DecodeWire([]byte(body))
	if err != nil {
		t.Fatalf("encode-overflow fixture failed DecodeWire: %v", err)
	}
	translated, err := grok.TranslateResponseChecked(wire, "claude-grok-4.7[1m]", func() time.Time { return now })
	if err != nil {
		t.Fatalf("encode-overflow fixture failed TranslateResponseChecked (must fail at encode, not translate): %v", err)
	}
	if _, err := jsonx.EncodeWire(translated); err == nil {
		t.Fatal("encode-overflow fixture unexpectedly succeeded EncodeWire")
	} else if _, ok := errors.AsType[*jsonx.WireDepthError](err); !ok {
		t.Fatalf("encode-overflow fixture EncodeWire error = %T (%v), want *jsonx.WireDepthError", err, err)
	}
	return body
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
			var prepRes, handleRes outcome.Result
			var prepRefused bool
			var handleErr error
			var upstreamHits atomic.Int32

			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			t.Cleanup(upSrv.Close)
			t.Cleanup(upSrv.Client().CloseIdleConnections)

			authStore := mustNewAuthStore(t, grok.AuthOptions{
				File:      validAuthFixture(t, now),
				Transport: denyNetworkTransport(t),
				Now:       func() time.Time { return now },
			})
			branch := grok.NewBranch(grok.BranchOptions{
				Endpoint: upSrv.URL + "/v1/responses",
				Auth:     authStore,
				Client:   newLoopbackClient(t, upSrv.Listener.Addr().String()),
				Now:      func() time.Time { return now },
			})

			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				req := grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.5",
					BackendModel:    "grok-4.5",
				}
				pRes, refused := branch.Prepare(w, r, req)
				mu.Lock()
				prepRes, prepRefused = pRes, refused
				mu.Unlock()
				if refused {
					return
				}
				hRes, err := branch.Handle(w, r, req)
				mu.Lock()
				handleRes, handleErr = hRes, err
				mu.Unlock()
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)

			status, _, respBody := postBranch(t, gw, "/v1/messages/count_tokens", tt.body, nil)
			mu.Lock()
			gotPrepRes, gotPrepRefused, gotHandleRes, gotHandleErr := prepRes, prepRefused, handleRes, handleErr
			method, path, override, body := gotMethod, gotPath, gotOverride, gotBody
			mu.Unlock()

			if gotPrepRefused {
				t.Fatalf("Prepare() refused count_tokens request: %+v", gotPrepRes)
			}
			wantRoute := outcome.Route{
				SelectedModel:  "claude-grok-4.5",
				EffectiveModel: "grok-4.5",
				Backend:        "grok",
				Via:            "direct",
			}
			if diff := gocmp.Diff(outcome.Result{Route: wantRoute}, gotPrepRes); diff != "" {
				t.Errorf("Prepare() result (-want +got):\n%s", diff)
			}
			if gotHandleErr != nil {
				t.Fatalf("Handle() unexpected error: %v", gotHandleErr)
			}
			wantHandle := outcome.Result{
				Route:     wantRoute,
				Status:    http.StatusOK,
				Contacted: true,
			}
			if diff := gocmp.Diff(wantHandle, gotHandleRes); diff != "" {
				t.Errorf("Handle() result (-want +got):\n%s", diff)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if upstreamHits.Load() != 1 {
				t.Fatalf("upstream hits = %d, want 1", upstreamHits.Load())
			}
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

			decoded := decodeFullJSONObject(t, respBody)
			if diff := gocmp.Diff("message", decoded["type"]); diff != "" {
				t.Errorf("response type (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBranchForwardJSON(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	encodeOverflowBody := buildEncodeOverflowUpstreamBody(t, now)

	baseRoute := outcome.Route{
		SelectedModel:  "claude-grok-4.7[1m]",
		EffectiveModel: "grok-4.7",
		Backend:        "grok",
		Via:            "direct",
	}
	highEffortRoute := baseRoute
	highEffortRoute.Effort = "high"

	allFixtureSecrets := []string{
		"fixture-token",
		"fixture-refresh",
		"fixture-expired",
		"SENTINEL_CLIENT_AUTH",
		"SENTINEL_PROXY_AUTH",
		"SENTINEL_API_KEY",
		"SENTINEL_COOKIE",
		"SENTINEL_CONTROL_TOKEN",
	}

	tests := map[string]struct {
		missingAuth       bool
		expiredShadow     bool
		transportErr      bool
		versionContent    string
		clientHeaders     map[string]string
		requestBody       string
		upstreamStatus    int
		upstreamHeaders   map[string]string
		upstreamBody      string
		wantStatus        int
		wantUpstreamHit   int32
		wantErrType       string
		wantErrMessage    string
		wantErrPrefix     string
		wantText          string
		wantResult        outcome.Result
		wantUsageCalls    int
		wantUsageStatus   int
		wantUsageRawBytes bool
		wantUsageJSON     bool
		checkSentinels    []string
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
			upstreamBody:      `{"id":"resp_fixture","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`,
			wantStatus:        http.StatusOK,
			wantUpstreamHit:   1,
			wantText:          "hello",
			wantResult:        outcome.Result{Route: highEffortRoute, Status: http.StatusOK, Contacted: true},
			wantUsageCalls:    1,
			wantUsageStatus:   http.StatusOK,
			wantUsageRawBytes: true,
			wantUsageJSON:     true,
			checkSentinels: []string{
				"SENTINEL_CLIENT_AUTH",
				"SENTINEL_PROXY_AUTH",
				"SENTINEL_API_KEY",
				"SENTINEL_COOKIE",
				"SENTINEL_CONTROL_TOKEN",
			},
		},
		"success: translates unparseable 200 non-stream body from null": {
			requestBody:       `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:    http.StatusOK,
			upstreamBody:      `not valid json`,
			wantStatus:        http.StatusOK,
			wantUpstreamHit:   1,
			wantResult:        outcome.Result{Route: baseRoute, Status: http.StatusOK, Contacted: true},
			wantUsageCalls:    1,
			wantUsageStatus:   http.StatusOK,
			wantUsageRawBytes: true,
			wantUsageJSON:     true,
		},
		"error: translation failure before committing headers returns sanitized 502": {
			requestBody:       `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:    http.StatusOK,
			upstreamBody:      `{"id":"resp_bad","status":"completed","output":[],"usage":{"input_tokens":{"toString":1}}}`,
			wantStatus:        http.StatusBadGateway,
			wantUpstreamHit:   1,
			wantErrType:       "api_error",
			wantErrMessage:    sanitizedTranslationErrMessage,
			wantResult:        outcome.Result{Route: baseRoute, Status: http.StatusBadGateway, StatusOverride: "upstream_error", Contacted: true},
			wantUsageCalls:    1,
			wantUsageStatus:   http.StatusOK,
			wantUsageRawBytes: true,
			wantUsageJSON:     false,
		},
		"error: encoding failure after translation before committing headers returns sanitized 502": {
			requestBody:       `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:    http.StatusOK,
			upstreamBody:      encodeOverflowBody,
			wantStatus:        http.StatusBadGateway,
			wantUpstreamHit:   1,
			wantErrType:       "api_error",
			wantErrMessage:    sanitizedTranslationErrMessage,
			wantResult:        outcome.Result{Route: baseRoute, Status: http.StatusBadGateway, StatusOverride: "upstream_error", Contacted: true},
			wantUsageCalls:    1,
			wantUsageStatus:   http.StatusOK,
			wantUsageRawBytes: true,
			wantUsageJSON:     false,
		},
		"error: upstream 426 returns outdated CLI version message": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusUpgradeRequired,
			upstreamBody:    `{"error":{"message":"upgrade required"}}`,
			wantStatus:      http.StatusUpgradeRequired,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: Grok CLI version header is outdated. Update the grok CLI, then retry.",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusUpgradeRequired, Contacted: true},
			wantUsageCalls:  1,
			wantUsageStatus: http.StatusUpgradeRequired,
		},
		"error: upstream 401 maps to authentication_error": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusUnauthorized,
			upstreamBody:    `{"error":{"message":"token rejected"}}`,
			wantStatus:      http.StatusUnauthorized,
			wantUpstreamHit: 1,
			wantErrType:     "authentication_error",
			wantErrMessage:  "model-gateway: token rejected",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusUnauthorized, Contacted: true},
			wantUsageCalls:  1,
			wantUsageStatus: http.StatusUnauthorized,
		},
		"error: upstream 413 passes through as api_error without overflow normalization": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusRequestEntityTooLarge,
			upstreamBody:    `{"error":{"message":"context length exceeded"}}`,
			wantStatus:      http.StatusRequestEntityTooLarge,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: context length exceeded",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusRequestEntityTooLarge, Contacted: true},
			wantUsageCalls:  1,
			wantUsageStatus: http.StatusRequestEntityTooLarge,
		},
		"error: upstream 500 with string error preserves detail": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusInternalServerError,
			upstreamBody:    `{"error":"upstream overloaded"}`,
			wantStatus:      http.StatusInternalServerError,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: upstream overloaded",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusInternalServerError, Contacted: true},
			wantUsageCalls:  1,
			wantUsageStatus: http.StatusInternalServerError,
		},
		"error: upstream 503 with non-json body uses status fallback": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusServiceUnavailable,
			upstreamBody:    `bad gateway html`,
			wantStatus:      http.StatusServiceUnavailable,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: Grok upstream returned 503",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusServiceUnavailable, Contacted: true},
			wantUsageCalls:  1,
			wantUsageStatus: http.StatusServiceUnavailable,
		},
		"error: missing auth returns 401 authentication_error without contacting upstream": {
			missingAuth:     true,
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusUnauthorized,
			wantUpstreamHit: 0,
			wantErrType:     "authentication_error",
			wantErrMessage:  "model-gateway: Grok CLI auth is missing. Run `grok` and log in again.",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusUnauthorized, StatusOverride: "authentication_error", Contacted: false},
			wantUsageCalls:  0,
		},
		"error: shadow mode expired auth returns 401 without contacting upstream": {
			expiredShadow:   true,
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusUnauthorized,
			wantUpstreamHit: 0,
			wantErrType:     "authentication_error",
			wantErrMessage:  "model-gateway: Grok CLI auth cannot refresh. Run `grok` and log in again.",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusUnauthorized, StatusOverride: "authentication_error", Contacted: false},
			wantUsageCalls:  0,
		},
		"error: upstream transport failure returns 502 api_error": {
			transportErr:    true,
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusBadGateway,
			wantUpstreamHit: 0,
			wantErrType:     "api_error",
			wantErrPrefix:   "model-gateway: Grok upstream failed:",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusBadGateway, StatusOverride: "upstream_error", Contacted: true},
			wantUsageCalls:  0,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			var mu sync.Mutex
			var seenHeaders http.Header
			var seenBody string
			var handleRes outcome.Result
			var handleErr error
			var upstreamHits atomic.Int32

			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			t.Cleanup(upSrv.Close)
			t.Cleanup(upSrv.Client().CloseIdleConnections)

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

			endpoint := upSrv.URL + "/v1/responses"
			allowedAddr := upSrv.Listener.Addr().String()
			if tt.transportErr {
				// Use an already-closed ephemeral listener so dialing fails immediately on loopback.
				dead := httptest.NewServer(http.NewServeMux())
				endpoint = dead.URL + "/v1/responses"
				allowedAddr = dead.Listener.Addr().String()
				dead.Close()
			}

			authStore := mustNewAuthStore(t, grok.AuthOptions{
				File:      authFile,
				Transport: denyNetworkTransport(t),
				Now:       func() time.Time { return now },
				Shadow:    tt.expiredShadow,
			})
			usageObs := &usageSpy{}
			branch := grok.NewBranch(grok.BranchOptions{
				Endpoint:    endpoint,
				VersionFile: versionFile,
				Auth:        authStore,
				Client:      newLoopbackClient(t, allowedAddr),
				Now:         func() time.Time { return now },
			})

			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				res, err := branch.Handle(w, r, grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.7[1m]",
					BackendModel:    "grok-4.7",
					Usage:           usageObs,
				})
				mu.Lock()
				handleRes, handleErr = res, err
				mu.Unlock()
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)

			status, respHeaders, respBody := postBranch(t, gw, "/v1/messages", tt.requestBody, tt.clientHeaders)

			mu.Lock()
			gotRes, gotErr := handleRes, handleErr
			mu.Unlock()

			if gotErr != nil {
				t.Fatalf("Handle() unexpected error: %v", gotErr)
			}
			if diff := gocmp.Diff(tt.wantResult, gotRes); diff != "" {
				t.Errorf("Handle() outcome.Result (-want +got):\n%s", diff)
			}
			assertNoSecrets(t, "outcome.Result", fmt.Sprintf("%+v", gotRes), allFixtureSecrets...)
			assertNoSecrets(t, "response body", respBody, allFixtureSecrets...)

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if got := upstreamHits.Load(); got != tt.wantUpstreamHit {
				t.Errorf("upstream hits = %d, want %d", got, tt.wantUpstreamHit)
			}
			if got := respHeaders.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if diff := gocmp.Diff(strconv.Itoa(len(respBody)), respHeaders.Get("Content-Length")); diff != "" {
				t.Errorf("Content-Length (-want +got):\n%s", diff)
			}

			decoded := decodeFullJSONObject(t, respBody)
			if tt.wantErrType != "" {
				if tt.wantErrMessage != "" {
					wantObj := map[string]any{
						"type": "error",
						"error": map[string]any{
							"type":    tt.wantErrType,
							"message": tt.wantErrMessage,
						},
					}
					if diff := gocmp.Diff(wantObj, decoded); diff != "" {
						t.Errorf("error response object (-want +got):\n%s", diff)
					}
				} else {
					if diff := gocmp.Diff("error", decoded["type"]); diff != "" {
						t.Errorf("error envelope type (-want +got):\n%s", diff)
					}
					if len(decoded) != 2 {
						t.Errorf("error envelope key count = %d, want 2", len(decoded))
					}
					errObj, ok := decoded["error"].(map[string]any)
					if !ok {
						t.Fatalf("error field is not an object: %T", decoded["error"])
					}
					if len(errObj) != 2 {
						t.Errorf("error object key count = %d, want 2 (no extra fields like overflow)", len(errObj))
					}
					if diff := gocmp.Diff(tt.wantErrType, errObj["type"]); diff != "" {
						t.Errorf("error.type (-want +got):\n%s", diff)
					}
					if msg, _ := errObj["message"].(string); !strings.HasPrefix(msg, tt.wantErrPrefix) {
						t.Errorf("error.message = %q, want prefix %q", msg, tt.wantErrPrefix)
					}
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
						t.Fatalf("content len = %d, want 1", len(blocks))
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
				}
			}

			snap := usageObs.snapshot(t)
			if snap.finishCalls != 0 {
				t.Errorf("usage Finish() calls = %d, want 0 (Grok branch never finishes a usage record)", snap.finishCalls)
			}
			if snap.responseCalls != tt.wantUsageCalls {
				t.Errorf("usage SetResponse() calls = %d, want %d", snap.responseCalls, tt.wantUsageCalls)
			}
			if tt.wantUsageCalls > 0 {
				if snap.status != tt.wantUsageStatus {
					t.Errorf("usage SetResponse status = %d, want %d", snap.status, tt.wantUsageStatus)
				}
				if tt.wantText != "" && snap.headers.Get("X-Upstream-Echo") != "kept" {
					t.Errorf("usage SetResponse headers X-Upstream-Echo = %q, want kept", snap.headers.Get("X-Upstream-Echo"))
				}
			}
			if tt.wantUsageRawBytes {
				if snap.responseBytes != int64(len(tt.upstreamBody)) {
					t.Errorf("usage NoteResponseBytes = %d, want %d", snap.responseBytes, len(tt.upstreamBody))
				}
			} else if snap.responseBytes != 0 {
				t.Errorf("usage NoteResponseBytes = %d, want 0", snap.responseBytes)
			}
			if tt.wantUsageJSON {
				if diff := gocmp.Diff([]string{respBody}, snap.jsonBodies); diff != "" {
					t.Errorf("usage ObserveJSON bodies (-want +got):\n%s", diff)
				}
			} else if len(snap.jsonBodies) != 0 {
				t.Errorf("usage ObserveJSON count = %d, want 0", len(snap.jsonBodies))
			}
			if len(snap.events) != 0 {
				t.Errorf("usage ObserveEvent count = %d, want 0 on JSON response", len(snap.events))
			}

			if len(tt.checkSentinels) > 0 {
				mu.Lock()
				hdrs, body := seenHeaders, seenBody
				mu.Unlock()
				if hdrs.Get("Authorization") != "Bearer fixture-token" {
					t.Error("upstream Authorization header did not carry fixture bearer token")
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
				assertNoSecrets(t, "upstream headers", flatHeaders.String(), tt.checkSentinels...)
				assertNoSecrets(t, "upstream body", body, tt.checkSentinels...)
			}
		})
	}
}

func TestBranchThreadRefusal(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)

	tests := map[string]struct {
		body            string
		configureAuth   bool
		wantEffort      string
		wantRefused     bool
		wantStatus      int
		wantUpstreamHit int32
	}{
		"error: continuation refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","output_config":{"effort":"low"},"messages":[{"role":"user","content":"turn"}],"thread":{"type":"continue","previous_message_id":"message_123"}}`,
			configureAuth:   false,
			wantEffort:      "low",
			wantRefused:     true,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"error: null thread refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":null}`,
			configureAuth:   false,
			wantRefused:     true,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"error: string thread refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":"continue"}`,
			configureAuth:   false,
			wantRefused:     true,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"error: array thread refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":[]}`,
			configureAuth:   false,
			wantRefused:     true,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"error: unknown thread type refuses before auth or forwarding": {
			body:            `{"model":"claude-grok-4.5","messages":[{"role":"user","content":"turn"}],"thread":{"type":"resume"}}`,
			configureAuth:   false,
			wantRefused:     true,
			wantStatus:      http.StatusBadRequest,
			wantUpstreamHit: 0,
		},
		"success: thread create is allowed by Prepare and forwarded by Handle": {
			body:            `{"model":"claude-grok-4.5","output_config":{"effort":"medium"},"messages":[{"role":"user","content":"turn"}],"thread":{"type":"create"}}`,
			configureAuth:   true,
			wantEffort:      "medium",
			wantRefused:     false,
			wantStatus:      http.StatusOK,
			wantUpstreamHit: 1,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			var mu sync.Mutex
			var prepRes, handleRes outcome.Result
			var prepRefused bool
			var handleCalled bool
			var handleErr error
			var upstreamHits atomic.Int32

			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_ok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			t.Cleanup(upSrv.Close)
			t.Cleanup(upSrv.Client().CloseIdleConnections)

			authFile := filepath.Join(t.TempDir(), "missing-auth.json")
			if tt.configureAuth {
				authFile = validAuthFixture(t, now)
			}

			authStore := mustNewAuthStore(t, grok.AuthOptions{
				File:      authFile,
				Transport: denyNetworkTransport(t),
				Now:       func() time.Time { return now },
			})
			branch := grok.NewBranch(grok.BranchOptions{
				Endpoint: upSrv.URL + "/v1/responses",
				Auth:     authStore,
				Client:   newLoopbackClient(t, upSrv.Listener.Addr().String()),
				Now:      func() time.Time { return now },
			})

			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				req := grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.5",
					BackendModel:    "grok-4.5",
				}
				pRes, refused := branch.Prepare(w, r, req)
				mu.Lock()
				prepRes, prepRefused = pRes, refused
				mu.Unlock()
				if refused {
					return
				}
				hRes, err := branch.Handle(w, r, req)
				mu.Lock()
				handleCalled = true
				handleRes, handleErr = hRes, err
				mu.Unlock()
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)

			status, hdrs, respBody := postBranch(t, gw, "/v1/messages", tt.body, nil)

			mu.Lock()
			gotPrepRes, gotPrepRefused := prepRes, prepRefused
			gotHandleCalled, gotHandleRes, gotHandleErr := handleCalled, handleRes, handleErr
			mu.Unlock()

			wantRoute := outcome.Route{
				SelectedModel:  "claude-grok-4.5",
				EffectiveModel: "grok-4.5",
				Backend:        "grok",
				Effort:         tt.wantEffort,
				Via:            "direct",
			}

			if gotPrepRefused != tt.wantRefused {
				t.Fatalf("Prepare() refused = %v, want %v", gotPrepRefused, tt.wantRefused)
			}
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if got := upstreamHits.Load(); got != tt.wantUpstreamHit {
				t.Errorf("upstream hits = %d, want %d", got, tt.wantUpstreamHit)
			}

			if tt.wantRefused {
				wantPrep := outcome.Result{
					Route:          wantRoute,
					Status:         http.StatusBadRequest,
					StatusOverride: "thread_unsupported",
					Contacted:      false,
				}
				if diff := gocmp.Diff(wantPrep, gotPrepRes); diff != "" {
					t.Errorf("Prepare() refusal result (-want +got):\n%s", diff)
				}
				if gotHandleCalled {
					t.Error("Handle() must not run when Prepare() refused")
				}
				if got := hdrs.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				if diff := gocmp.Diff(strconv.Itoa(len(threadRefusalBody)), hdrs.Get("Content-Length")); diff != "" {
					t.Errorf("Content-Length (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(threadRefusalBody, respBody); diff != "" {
					t.Errorf("thread refusal body (-want +got):\n%s", diff)
				}
				return
			}

			if diff := gocmp.Diff(outcome.Result{Route: wantRoute}, gotPrepRes); diff != "" {
				t.Errorf("Prepare() accepted result (-want +got):\n%s", diff)
			}
			if gotHandleErr != nil {
				t.Fatalf("Handle() unexpected error: %v", gotHandleErr)
			}
			wantHandle := outcome.Result{
				Route:     wantRoute,
				Status:    http.StatusOK,
				Contacted: true,
			}
			if diff := gocmp.Diff(wantHandle, gotHandleRes); diff != "" {
				t.Errorf("Handle() result (-want +got):\n%s", diff)
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

		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("X-Upstream-Header", "present")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, ssePayload)
		}))
		t.Cleanup(upSrv.Close)
		t.Cleanup(upSrv.Client().CloseIdleConnections)

		authStore := mustNewAuthStore(t, grok.AuthOptions{
			File:      validAuthFixture(t, now),
			Transport: denyNetworkTransport(t),
			Now:       func() time.Time { return now },
		})
		usageObs := &usageSpy{}
		branch := grok.NewBranch(grok.BranchOptions{
			Endpoint: upSrv.URL + "/v1/responses",
			Auth:     authStore,
			Client:   newLoopbackClient(t, upSrv.Listener.Addr().String()),
			Now:      func() time.Time { return now },
		})

		var mu sync.Mutex
		var handleRes outcome.Result
		var handleErr error
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			res, err := branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
				Usage:           usageObs,
			})
			mu.Lock()
			handleRes, handleErr = res, err
			mu.Unlock()
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		status, hdrs, body := postBranch(t, gw, "/v1/messages", `{"model":"claude-grok-4.5[1m]","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)

		mu.Lock()
		gotRes, gotErr := handleRes, handleErr
		mu.Unlock()

		if gotErr != nil {
			t.Fatalf("Handle() unexpected error: %v", gotErr)
		}
		wantRes := outcome.Result{
			Route: outcome.Route{
				SelectedModel:  "claude-grok-4.5[1m]",
				EffectiveModel: "grok-4.5",
				Backend:        "grok",
				Via:            "direct",
			},
			Status:    http.StatusOK,
			Contacted: true,
		}
		if diff := gocmp.Diff(wantRes, gotRes); diff != "" {
			t.Errorf("Handle() outcome.Result (-want +got):\n%s", diff)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if got := hdrs.Get("Content-Type"); got != "text/event-stream" {
			t.Errorf("Content-Type = %q, want text/event-stream", got)
		}
		if got := hdrs.Get("X-Upstream-Header"); got != "present" {
			t.Errorf("X-Upstream-Header = %q, want present", got)
		}
		for _, want := range []string{`"type":"message_start"`, `"model":"claude-grok-4.5[1m]"`, `"type":"text_delta","text":"hi"`, `"stop_reason":"end_turn"`, `"type":"message_stop"`} {
			if !strings.Contains(body, want) {
				t.Errorf("stream body missing %s", want)
			}
		}

		snap := usageObs.snapshot(t)
		if snap.finishCalls != 0 {
			t.Errorf("usage Finish() calls = %d, want 0", snap.finishCalls)
		}
		if snap.responseCalls != 1 || snap.status != http.StatusOK || snap.headers.Get("X-Upstream-Header") != "present" {
			t.Errorf("usage SetResponse = (calls=%d status=%d header=%q), want (1, 200, present)", snap.responseCalls, snap.status, snap.headers.Get("X-Upstream-Header"))
		}
		if snap.responseBytes != int64(len(ssePayload)) {
			t.Errorf("usage NoteResponseBytes = %d, want %d", snap.responseBytes, len(ssePayload))
		}
		wantEvents := []string{
			`{"type":"message_delta","usage":{"input_tokens":10,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0}}}`,
			`{"type":"message_delta","usage":{"input_tokens":10,"output_tokens":4,"output_tokens_details":{"reasoning_tokens":1}}}`,
		}
		if diff := gocmp.Diff(wantEvents, snap.events); diff != "" {
			t.Errorf("usage ObserveEvent sequence (-want +got):\n%s", diff)
		}
		if len(snap.jsonBodies) != 0 {
			t.Errorf("usage ObserveJSON count = %d, want 0 on SSE response", len(snap.jsonBodies))
		}
	})

	t.Run("success: orderly EOF without response.completed emits synthetic end_turn stop", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		ssePayload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_eof\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, ssePayload)
		}))
		t.Cleanup(upSrv.Close)
		t.Cleanup(upSrv.Client().CloseIdleConnections)

		authStore := mustNewAuthStore(t, grok.AuthOptions{
			File:      validAuthFixture(t, now),
			Transport: denyNetworkTransport(t),
			Now:       func() time.Time { return now },
		})
		branch := grok.NewBranch(grok.BranchOptions{
			Endpoint: upSrv.URL + "/v1/responses",
			Auth:     authStore,
			Client:   newLoopbackClient(t, upSrv.Listener.Addr().String()),
			Now:      func() time.Time { return now },
		})

		var mu sync.Mutex
		var handleRes outcome.Result
		var handleErr error
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			res, err := branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5",
				BackendModel:    "grok-4.5",
			})
			mu.Lock()
			handleRes, handleErr = res, err
			mu.Unlock()
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		status, _, body := postBranch(t, gw, "/v1/messages", `{"model":"claude-grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)

		mu.Lock()
		gotRes, gotErr := handleRes, handleErr
		mu.Unlock()
		if gotErr != nil {
			t.Fatalf("Handle() unexpected error: %v", gotErr)
		}
		wantRes := outcome.Result{
			Route: outcome.Route{
				SelectedModel:  "claude-grok-4.5",
				EffectiveModel: "grok-4.5",
				Backend:        "grok",
				Via:            "direct",
			},
			Status:    http.StatusOK,
			Contacted: true,
		}
		if diff := gocmp.Diff(wantRes, gotRes); diff != "" {
			t.Errorf("Handle() outcome.Result (-want +got):\n%s", diff)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		for _, want := range []string{`"type":"message_start"`, `"text":"partial"`, `"stop_reason":"end_turn"`, `"type":"message_stop"`} {
			if !strings.Contains(body, want) {
				t.Errorf("stream body missing %s", want)
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

		authStore := mustNewAuthStore(t, grok.AuthOptions{
			File:      validAuthFixture(t, now),
			Transport: denyNetworkTransport(t),
			Now:       func() time.Time { return now },
		})
		branch := grok.NewBranch(grok.BranchOptions{
			Endpoint: "http://" + ln.Addr().String() + "/v1/responses",
			Auth:     authStore,
			Client:   newLoopbackClient(t, ln.Addr().String()),
			Now:      func() time.Time { return now },
		})

		var mu sync.Mutex
		var handleRes outcome.Result
		var handleErr error
		handlerDone := make(chan struct{})
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			raw, _ := io.ReadAll(r.Body)
			res, hErr := branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
			})
			mu.Lock()
			handleRes, handleErr = res, hErr
			mu.Unlock()
			if errors.Is(hErr, http.ErrAbortHandler) {
				panic(http.ErrAbortHandler)
			}
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		// Bound client Do + io.Copy with a distinct watchdog so a hang fails as a timeout rather
		// than masquerading as an upstream abort.
		clientCtx, cancelClient := context.WithCancel(t.Context())
		defer cancelClient()
		var watchdogFired atomic.Bool
		watchdog := time.AfterFunc(3*time.Second, func() {
			watchdogFired.Store(true)
			cancelClient()
		})
		defer watchdog.Stop()

		req, err := http.NewRequestWithContext(clientCtx, http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"model":"claude-grok-4.5[1m]","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := gw.Client().Do(req)
		if watchdogFired.Load() {
			t.Fatal("client timed out before receiving response headers (not an upstream abort)")
		}
		if err != nil {
			t.Fatalf("POST /v1/messages: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var partial bytes.Buffer
		_, readErr := io.Copy(&partial, resp.Body)
		watchdog.Stop()
		if watchdogFired.Load() {
			t.Fatal("client timed out waiting for truncated stream abort (timeout cancellation is not an upstream abort)")
		}
		if readErr == nil {
			t.Fatal("expected truncated stream transport error, got clean EOF")
		}
		if !strings.Contains(partial.String(), `"type":"message_start"`) {
			t.Error("partial stream body missing message_start")
		}

		select {
		case <-handlerDone:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for server handler completion")
		}
		mu.Lock()
		gotRes, gotErr := handleRes, handleErr
		mu.Unlock()
		if !errors.Is(gotErr, http.ErrAbortHandler) {
			t.Errorf("Handle() error = %v, want wrapped http.ErrAbortHandler", gotErr)
		}
		wantRoute := outcome.Route{
			SelectedModel:  "claude-grok-4.5[1m]",
			EffectiveModel: "grok-4.5",
			Backend:        "grok",
			Via:            "direct",
		}
		if diff := gocmp.Diff(wantRoute, gotRes.Route); diff != "" {
			t.Errorf("Handle() abort Route (-want +got):\n%s", diff)
		}
		if gotRes.Status != http.StatusOK || (gotRes.StatusOverride != "upstream_error" && gotRes.StatusOverride != "upstream_aborted") || !gotRes.Contacted || gotRes.Cancelled {
			t.Errorf("Handle() abort outcome = %+v, want Status=200 StatusOverride=(upstream_error|upstream_aborted) Contacted=true Cancelled=false", gotRes)
		}
	})
}

func TestBranchSentry(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)

	// Whitespace-rich original request bodies verify that raw request bytes are preserved without remarshaling.
	prettyOrdinaryJSON := "{\n  \"model\": \"claude-grok-4.5[1m]\",\n  \"max_tokens\": 1,\n  \"messages\": [ { \"role\": \"user\", \"content\": \"test\" } ]\n}\n"
	prettyOrdinarySSE := "{\n  \"model\": \"claude-grok-4.5[1m]\",\n  \"stream\": true,\n  \"max_tokens\": 1,\n  \"messages\": [ { \"role\": \"user\", \"content\": \"test\" } ]\n}\n"
	prettyCompactionSSE := "{\n  \"model\": \"claude-grok-4.5[1m]\",\n  \"stream\": true,\n  \"max_tokens\": 1,\n  \"system\": \"" + compactionPrompt + "\",\n  \"messages\": [ { \"role\": \"user\", \"content\": \"test\" } ]\n}\n"

	sseEvent := func(eventType string, inputTokens int) string {
		return fmt.Sprintf("data: {\"type\":%q,\"response\":{\"id\":\"resp_s\",\"output\":[],\"usage\":{\"input_tokens\":%d}}}\n\n", eventType, inputTokens)
	}
	sseEventNoUsage := func(eventType string) string {
		return fmt.Sprintf("data: {\"type\":%q,\"response\":{\"id\":\"resp_s\",\"output\":[]}}\n\n", eventType)
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

	type sentryHarness struct {
		gw        *httptest.Server
		state     *sentry.State
		target    sentry.Target
		forwarded *atomic.Int32
		setUsage  func(grok.UsageObserver)
		setKey    func(string)
		lastErr   func() error
	}

	newHarness := func(t *testing.T, enabled bool, answers []http.HandlerFunc) sentryHarness {
		t.Helper()
		var idx atomic.Int32
		var forwarded atomic.Int32
		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		t.Cleanup(upSrv.Close)
		t.Cleanup(upSrv.Client().CloseIdleConnections)

		state := &sentry.State{}
		target := grokSentryTarget(t, "grok-4.5", 100)
		authStore := mustNewAuthStore(t, grok.AuthOptions{
			File:      validAuthFixture(t, now),
			Transport: denyNetworkTransport(t),
			Now:       func() time.Time { return now },
		})
		branch := grok.NewBranch(grok.BranchOptions{
			Endpoint:      upSrv.URL + "/v1/responses",
			Auth:          authStore,
			Client:        newLoopbackClient(t, upSrv.Listener.Addr().String()),
			Sentry:        state,
			SentryEnabled: enabled,
			Now:           func() time.Time { return now },
		})

		var mu sync.Mutex
		var nextUsage grok.UsageObserver
		sessionKey := sentry.SessionKey(enabled, "grok-sentry-session", nil)
		var lastHandleErr error

		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			obs := nextUsage
			key := sessionKey
			mu.Unlock()
			req := grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
				SessionKey:      key,
				SentryTarget:    target,
				Usage:           obs,
			}
			_, err := branch.Handle(w, r, req)
			mu.Lock()
			lastHandleErr = err
			mu.Unlock()
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		return sentryHarness{
			gw:        gw,
			state:     state,
			target:    target,
			forwarded: &forwarded,
			setUsage: func(u grok.UsageObserver) {
				mu.Lock()
				nextUsage = u
				mu.Unlock()
			},
			setKey: func(k string) {
				mu.Lock()
				sessionKey = k
				mu.Unlock()
			},
			lastErr: func() error {
				mu.Lock()
				defer mu.Unlock()
				return lastHandleErr
			},
		}
	}

	assertCrossing := func(t *testing.T, state *sentry.State, sessionKey string, target sentry.Target, raw string, wantUsage, wantTrigger float64) {
		t.Helper()
		cross, ok := state.Check(sessionKey, target, []byte(raw))
		if !ok {
			t.Fatalf("sentry.Check() = false, want crossing (usage=%v > trigger=%v)", wantUsage, wantTrigger)
		}
		want := sentry.Crossing{Usage: wantUsage, Trigger: wantTrigger}
		if diff := gocmp.Diff(want, cross); diff != "" {
			t.Errorf("sentry.Check() crossing (-want +got):\n%s", diff)
		}
	}

	assertNoCrossing := func(t *testing.T, label string, state *sentry.State, sessionKey string, target sentry.Target, raw string) {
		t.Helper()
		if cross, ok := state.Check(sessionKey, target, []byte(raw)); ok {
			t.Fatalf("%s: sentry.Check() unexpectedly crossed: %+v", label, cross)
		}
	}

	t.Run("success: Handle never performs a second admission check when sentry is already armed", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		h := newHarness(t, true, []http.HandlerFunc{
			jsonResp(http.StatusOK, `{"id":"resp_pass","output":[],"usage":{"input_tokens":102}}`),
		})
		key := sentry.SessionKey(true, "grok-sentry-session", nil)

		// Pre-arm the sentry above trigger (unfired).
		h.state.Observe(key, h.target, []byte(`{"type":"message_delta","usage":{"input_tokens":101}}`), false)

		// Handle serves accepted traffic only and must NOT run Check or write 413.
		status, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil)
		if err := h.lastErr(); err != nil {
			t.Fatalf("Handle() unexpected error: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("Handle() status = %d, want 200 (must not run admission sentry check)", status)
		}
		if h.forwarded.Load() != 1 {
			t.Fatalf("forwarded = %d, want 1", h.forwarded.Load())
		}
		// Latch must still be unfired and reflect the newly observed 102 tokens.
		assertCrossing(t, h.state, key, h.target, prettyOrdinaryJSON, 102, 100)
	})

	t.Run("success: JSON arms sentry and rearms only when status is completed and request is not compaction", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		h := newHarness(t, true, []http.HandlerFunc{
			// 1: uncompleted JSON still arms an unfired session at 101.
			jsonResp(http.StatusOK, `{"id":"resp_1","output":[],"usage":{"input_tokens":101}}`),
			// 2: while latched, status="incomplete" must NOT rearm.
			jsonResp(http.StatusOK, `{"id":"resp_2","status":"incomplete","output":[],"usage":{"input_tokens":102}}`),
			// 3: while latched, missing status must NOT rearm.
			jsonResp(http.StatusOK, `{"id":"resp_3","output":[],"usage":{"input_tokens":103}}`),
			// 4: while latched, compaction with status="completed" must NOT rearm.
			jsonResp(http.StatusOK, `{"id":"resp_4","status":"completed","output":[],"usage":{"input_tokens":104}}`),
			// 5: while latched, non-compaction with status="completed" DOES rearm.
			jsonResp(http.StatusOK, `{"id":"resp_5","status":"completed","output":[],"usage":{"input_tokens":105}}`),
		})
		key := sentry.SessionKey(true, "grok-sentry-session", nil)

		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("turn 1 status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		// Fire latch using exact original whitespace-rich body.
		assertCrossing(t, h.state, key, h.target, prettyOrdinaryJSON, 101, 100)

		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("incomplete JSON turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "after incomplete JSON", h.state, key, h.target, prettyOrdinaryJSON)

		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("missing-status JSON turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "after missing-status JSON", h.state, key, h.target, prettyOrdinaryJSON)

		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyCompactionSSE, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("compaction JSON turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "after compaction JSON", h.state, key, h.target, prettyOrdinaryJSON)

		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("completed ordinary JSON turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertCrossing(t, h.state, key, h.target, prettyOrdinaryJSON, 105, 100)
		if h.forwarded.Load() != 5 {
			t.Fatalf("forwarded = %d, want 5", h.forwarded.Load())
		}
	})

	t.Run("success: SSE observes per-frame usage in flight and rearms only deliverable provider-completed non-compaction", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		releaseInFlight := make(chan struct{})
		var releaseOnce sync.Once
		releaseNow := func() { releaseOnce.Do(func() { close(releaseInFlight) }) }

		h := newHarness(t, true, []http.HandlerFunc{
			// 1: running compaction streams usage mid-flight (102), blocks until released, then completes at 104.
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, sseEvent("response.created", 102))
				_ = http.NewResponseController(w).Flush()
				select {
				case <-releaseInFlight:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, sseEvent("response.completed", 104))
			},
			// 2: orderly EOF without response.completed (Stream.End synthesizes message_stop) -> NOT provider-completed.
			sseResp("data: not-json\n\ndata: [DONE]\n\n" + sseEvent("response.created", 103) + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"),
			// 3: bare error + response.completed -> sawError -> NOT deliverable.
			sseResp("data: {\"type\":\"error\",\"error\":{\"message\":\"stream error\"}}\n\n" + sseEvent("response.completed", 105)),
			// 4: response.failed + response.completed -> sawError -> NOT deliverable.
			sseResp(sseEvent("response.failed", 105) + sseEvent("response.completed", 105)),
			// 5: response.incomplete + response.completed -> sawError -> NOT deliverable.
			sseResp(sseEvent("response.incomplete", 106) + sseEvent("response.completed", 106)),
			// 6: deliverable completed ordinary SSE whose response.completed omits usage -> uses prior frame's 107 and rearms!
			sseResp(sseEvent("response.created", 107) + sseEventNoUsage("response.completed")),
			// 7: compaction updates usage to 108 without consuming armed refusal.
			sseResp(sseEvent("response.completed", 108)),
			// 8: low-usage compaction (20 < low watermark 75) resets fired latch.
			sseResp(sseEvent("response.completed", 20)),
			// 9: high incomplete ordinary SSE (109, no response.completed) arms the reset latch.
			sseResp(sseEvent("response.created", 109)),
		})
		key := sentry.SessionKey(true, "grok-sentry-session", nil)

		// 1. Verify in-flight branch observation while stream is still open!
		onEvent := make(chan jsonx.WireValue, 4)
		h.setUsage(&usageSpy{onEvent: onEvent})

		type postOutcome struct {
			status int
			body   string
			err    error
		}
		inFlightDone := make(chan struct{})
		var inFlightRes postOutcome
		go func() {
			defer close(inFlightDone)
			s, _, b, err := doBranchPost(t.Context(), h.gw.Client(), h.gw.URL+"/v1/messages", prettyCompactionSSE, nil)
			inFlightRes = postOutcome{status: s, body: b, err: err}
		}()
		t.Cleanup(func() {
			releaseNow()
			<-inFlightDone
		})

		select {
		case <-onEvent:
			// Branch observed the first SSE frame in flight!
		case <-inFlightDone:
			t.Fatalf("in-flight compaction ended before observing first frame (status=%d err=%v handleErr=%v)", inFlightRes.status, inFlightRes.err, h.lastErr())
		case <-time.After(3 * time.Second):
			releaseNow()
			t.Fatal("timed out waiting for branch-side in-flight SSE frame observation before EOF")
		}

		// While the stream is STILL OPEN, the sentry must already reflect the in-flight 102 tokens.
		// Poll briefly after onEvent so the test does not depend on whether Usage.ObserveEvent or
		// Sentry.ObserveWire runs first within the same frame.
		var inFlightCross sentry.Crossing
		var inFlightCrossed bool
		deadline := time.Now().Add(500 * time.Millisecond)
		for {
			if cross, ok := h.state.Check(key, h.target, []byte(prettyOrdinarySSE)); ok {
				inFlightCross, inFlightCrossed = cross, true
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !inFlightCrossed {
			releaseNow()
			t.Fatal("sentry did not observe in-flight SSE usage while stream remained open")
		}
		if diff := gocmp.Diff(sentry.Crossing{Usage: 102, Trigger: 100}, inFlightCross); diff != "" {
			t.Errorf("in-flight sentry.Check() crossing (-want +got):\n%s", diff)
		}

		// Now let the compaction finish with response.completed at 104; compaction must NOT rearm the fired latch.
		releaseNow()
		<-inFlightDone
		if inFlightRes.err != nil || inFlightRes.status != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("in-flight compaction failed: status=%d err=%v handleErr=%v", inFlightRes.status, inFlightRes.err, h.lastErr())
		}
		h.setUsage(nil)
		assertNoCrossing(t, "after completed compaction", h.state, key, h.target, prettyOrdinarySSE)

		// 2. Orderly EOF without provider response.completed synthesizes message_stop but must NOT complete sentry.
		s, _, b := postBranch(t, h.gw, "/v1/messages", prettyOrdinarySSE, nil)
		if s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("EOF-without-completed turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		if !strings.Contains(b, `"type":"message_stop"`) {
			t.Error("EOF-without-completed stream did not emit synthesized message_stop")
		}
		assertNoCrossing(t, "after synthetic message_stop without response.completed", h.state, key, h.target, prettyOrdinarySSE)

		// 3. Bare error + response.completed must NOT rearm.
		if s, _, _ = postBranch(t, h.gw, "/v1/messages", prettyOrdinarySSE, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("bare error turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "after bare error + response.completed", h.state, key, h.target, prettyOrdinarySSE)

		// 4. response.failed + response.completed must NOT rearm.
		if s, _, _ = postBranch(t, h.gw, "/v1/messages", prettyOrdinarySSE, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("response.failed turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "after response.failed + response.completed", h.state, key, h.target, prettyOrdinarySSE)

		// 5. response.incomplete + response.completed must NOT rearm.
		if s, _, _ = postBranch(t, h.gw, "/v1/messages", prettyOrdinarySSE, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("response.incomplete turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "after response.incomplete + response.completed", h.state, key, h.target, prettyOrdinarySSE)

		// 6. Deliverable completed ordinary SSE where response.completed itself has no usage -> rearms using 107!
		if s, _, _ = postBranch(t, h.gw, "/v1/messages", prettyOrdinarySSE, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("completed-without-final-usage turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		// Compaction request is exempt from Check (does not consume the rearmed refusal) and updates usage to 108.
		assertNoCrossing(t, "compaction exempt while armed", h.state, key, h.target, prettyCompactionSSE)
		if s, _, _ = postBranch(t, h.gw, "/v1/messages", prettyCompactionSSE, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("compaction bypass turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		// Ordinary request now crosses with the updated 108 tokens (firing the latch).
		assertCrossing(t, h.state, key, h.target, prettyOrdinarySSE, 108, 100)

		// 8. Low-usage compaction resets the fired latch.
		if s, _, _ = postBranch(t, h.gw, "/v1/messages", prettyCompactionSSE, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("low-usage compaction status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "immediately after low-usage compaction", h.state, key, h.target, prettyOrdinarySSE)

		// 9. A subsequent high incomplete ordinary SSE (no response.completed) can cross ONLY if the
		// low-usage compaction actually reset the fired latch.
		if s, _, _ = postBranch(t, h.gw, "/v1/messages", prettyOrdinarySSE, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("post-reset high incomplete turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertCrossing(t, h.state, key, h.target, prettyOrdinarySSE, 109, 100)

		if h.forwarded.Load() != 9 {
			t.Fatalf("forwarded = %d, want 9", h.forwarded.Load())
		}
	})

	t.Run("success: sentry disabled never records usage", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		h := newHarness(t, false, []http.HandlerFunc{
			jsonResp(http.StatusOK, `{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":500}}`),
		})
		key := sentry.SessionKey(true, "grok-sentry-session", nil)
		h.setKey(key)

		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("turn 1 status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "sentry disabled", h.state, key, h.target, prettyOrdinaryJSON)
		if h.forwarded.Load() != 1 {
			t.Fatalf("forwarded = %d, want 1", h.forwarded.Load())
		}
	})

	t.Run("success: separate Request.SessionKey values stay isolated without reading session headers", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		h := newHarness(t, true, []http.HandlerFunc{
			jsonResp(http.StatusOK, `{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":101}}`),
			jsonResp(http.StatusOK, `{"id":"resp_2","status":"completed","output":[],"usage":{"input_tokens":50}}`),
			jsonResp(http.StatusOK, `{"id":"resp_3","status":"completed","output":[],"usage":{"input_tokens":50}}`),
		})

		keyMain := sentry.SessionKey(true, "grok-sentry-session", nil)
		siblingAgent := "sibling"
		keySibling := sentry.SessionKey(true, "grok-sentry-session", &siblingAgent)
		keyOther := sentry.SessionKey(true, "other-session", nil)

		// Send no session HTTP headers at all so Handle must use req.SessionKey.
		h.setKey(keyMain)
		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("main session turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}

		h.setKey(keySibling)
		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("sibling agent turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "sibling agent", h.state, keySibling, h.target, prettyOrdinaryJSON)

		h.setKey(keyOther)
		if s, _, _ := postBranch(t, h.gw, "/v1/messages", prettyOrdinaryJSON, nil); s != http.StatusOK || h.lastErr() != nil {
			t.Fatalf("other session turn status=%d err=%v, want 200/nil", s, h.lastErr())
		}
		assertNoCrossing(t, "other session", h.state, keyOther, h.target, prettyOrdinaryJSON)

		assertCrossing(t, h.state, keyMain, h.target, prettyOrdinaryJSON, 101, 100)
		if h.forwarded.Load() != 3 {
			t.Fatalf("forwarded = %d, want 3", h.forwarded.Load())
		}
	})
}
