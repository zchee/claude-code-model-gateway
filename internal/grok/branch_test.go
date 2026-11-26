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
	"bufio"
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

var (
	branchNonBearerSecrets = []string{
		"fixture-refresh",
		"fixture-expired",
		"SENTINEL_CLIENT_AUTH",
		"SENTINEL_PROXY_AUTH",
		"SENTINEL_API_KEY",
		"SENTINEL_COOKIE",
		"SENTINEL_CONTROL_TOKEN",
	}
	branchAllSecrets = append([]string{"fixture-token"}, branchNonBearerSecrets...)
)

func assertBranchText(t *testing.T, label, value string) {
	t.Helper()
	assertNoSecrets(t, label, value, branchAllSecrets...)
}

func assertBranchOutcome(t *testing.T, res outcome.Result, err error) {
	t.Helper()
	encoded, encodeErr := json.Marshal(res)
	if encodeErr != nil {
		t.Fatal("unable to inspect branch outcome for leaked credentials")
	}
	assertBranchText(t, "outcome.Result", string(encoded))
	if err != nil {
		assertBranchText(t, "branch error", err.Error())
	}
}

func assertUpstreamRequestSafe(t *testing.T, hdrs http.Header, body string) {
	t.Helper()
	if !gocmp.Equal([]string{"Bearer fixture-token"}, hdrs.Values("Authorization")) {
		t.Error("upstream Authorization header did not carry expected fixture bearer token (values redacted)")
	}
	assertNoSecrets(t, "upstream headers", fmt.Sprint(hdrs), branchNonBearerSecrets...)
	nonAuth := hdrs.Clone()
	nonAuth.Del("Authorization")
	assertBranchText(t, "upstream non-authorization headers", fmt.Sprint(nonAuth))
	assertBranchText(t, "upstream request body", body)
}

type captureProbe struct {
	capture *usage.Capture
	mu      sync.Mutex
	emitted []usage.Record
}

func newUsageCapture(t *testing.T, now time.Time, rawBody string) *captureProbe {
	t.Helper()
	wireBody, err := jsonx.DecodeWire([]byte(rawBody))
	if err != nil {
		t.Fatalf("decode request fixture for usage capture: %v", err)
	}
	probe := &captureProbe{}
	builder := usage.NewBuilder(usage.Options{
		Now: func() time.Time { return now },
	})
	probe.capture = usage.NewCapture(builder, usage.Request{
		Body:      wireBody,
		BodyBytes: int64(len(rawBody)),
	}, 0, func(rec usage.Record) {
		probe.mu.Lock()
		probe.emitted = append(probe.emitted, rec)
		probe.mu.Unlock()
	})
	return probe
}

func (p *captureProbe) emittedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.emitted)
}

func (p *captureProbe) finishRecord(t *testing.T) *usage.Record {
	t.Helper()
	if got := p.emittedCount(); got != 0 {
		t.Errorf("usage records emitted before caller Finish() = %d, want 0 (Grok branch never finishes a usage record)", got)
	}
	rec := p.capture.Finish()
	if rec != nil {
		assertBranchText(t, "usage record attributes", fmt.Sprint(rec.Attributes))
	}
	return rec
}

// signalingCapture delegates every observation directly to a real *usage.Capture and signals
// onEvent solely for deterministic test synchronization.
type signalingCapture struct {
	*usage.Capture
	onEvent chan struct{}
}

func (s *signalingCapture) ObserveEvent(event jsonx.WireValue) {
	s.Capture.ObserveEvent(event)
	if s.onEvent != nil {
		select {
		case s.onEvent <- struct{}{}:
		default:
		}
	}
}

type handleCallResult struct {
	res outcome.Result
	err error
}

type branchCallResult struct {
	prepRes      outcome.Result
	prepRefused  bool
	handleCalled bool
	handleRes    outcome.Result
	handleErr    error
}

type upstreamCapture struct {
	method  string
	path    string
	headers http.Header
	body    string
}

func recvWithin[T any](t *testing.T, label string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
		var zero T
		return zero
	}
}

func waitClosed(t *testing.T, label string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Errorf("timed out waiting for %s", label)
	}
}

// registerBranchTeardown registers a cleanup that cancels gates and closes both client and
// upstream sides before httptest.Server.Close runs, so a stuck handler cannot wedge teardown.
func registerBranchTeardown(t *testing.T, gw *httptest.Server, client *upstream.Client, upSrv *httptest.Server, release func()) {
	t.Helper()
	t.Cleanup(func() {
		if release != nil {
			release()
		}
		if gw != nil {
			gw.CloseClientConnections()
		}
		if client != nil {
			_ = client.Close()
		}
		if upSrv != nil {
			upSrv.CloseClientConnections()
		}
	})
}

type truncatedPeer struct {
	addr     string
	shutdown func()
}

func startTruncatedSSEPeer(t *testing.T, frames string) *truncatedPeer {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var shuttingDown atomic.Bool
	var activeConn atomic.Pointer[net.Conn]
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		activeConn.Store(&conn)
		defer func() {
			activeConn.Store(nil)
			_ = conn.Close()
		}()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		req, readErr := http.ReadRequest(bufio.NewReader(conn))
		if readErr != nil {
			if !shuttingDown.Load() {
				t.Error("abort fixture could not read HTTP request headers")
			}
			return
		}
		_, drainErr := io.Copy(io.Discard, req.Body)
		closeErr := req.Body.Close()
		if (drainErr != nil || closeErr != nil) && !shuttingDown.Load() {
			t.Error("abort fixture could not drain HTTP request body")
			return
		}
		raw := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(frames), frames)
		_, _ = io.WriteString(conn, raw)
		// Close without writing the terminating 0-length chunk.
	}()

	shutdown := sync.OnceFunc(func() {
		shuttingDown.Store(true)
		_ = ln.Close()
		if c := activeConn.Load(); c != nil {
			_ = (*c).Close()
		}
		waitClosed(t, "truncated SSE peer goroutine", serverDone)
	})
	t.Cleanup(shutdown)
	return &truncatedPeer{addr: ln.Addr().String(), shutdown: shutdown}
}

func newLoopbackClient(t *testing.T, allowedAddrs ...string) *upstream.Client {
	t.Helper()
	var dialer net.Dialer
	client := upstream.NewClient(upstream.Options{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !slices.Contains(allowedAddrs, addr) {
				t.Errorf("blocked an unapproved inference dial")
				return nil, fmt.Errorf("blocked unapproved dial to %q", addr)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestNewBranch(t *testing.T) {
	tests := map[string]struct {
		client *upstream.Client
	}{
		"error: nil client is rejected":        {},
		"success: explicit client is accepted": {client: newLoopbackClient(t)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			branch, err := grok.NewBranch(grok.BranchOptions{Client: tt.client})
			if tt.client == nil {
				if err == nil || branch != nil {
					t.Fatal("NewBranch accepted a nil Client; want a nil branch and a non-nil error")
				}
			} else if err != nil || branch == nil {
				t.Fatal("NewBranch rejected an explicit Client")
			}
		})
	}
}

func mustNewBranch(t *testing.T, opts grok.BranchOptions) *grok.Branch {
	t.Helper()
	branch, err := grok.NewBranch(opts)
	if err != nil || branch == nil {
		t.Fatal("NewBranch failed with an explicit Client")
	}
	return branch
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
		assertBranchText(t, "HTTP POST error", err.Error())
		t.Fatalf("POST %s: %v", path, err)
	}
	assertBranchText(t, "response body", respBody)
	assertBranchText(t, "response headers", fmt.Sprint(hdrs))
	return status, hdrs, respBody
}

func decodeFullJSONObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("invalid JSON response object (len=%d)", len(raw))
	}
	assertBranchText(t, "decoded response", fmt.Sprint(decoded))
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
			upReqs := make(chan upstreamCapture, 1)
			var upstreamHits atomic.Int32

			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				raw, _ := io.ReadAll(r.Body)
				upReqs <- upstreamCapture{
					method:  r.Method,
					path:    r.URL.Path,
					headers: r.Header.Clone(),
					body:    string(raw),
				}
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
			client := newLoopbackClient(t, upSrv.Listener.Addr().String())
			branch := mustNewBranch(t, grok.BranchOptions{
				Endpoint: upSrv.URL + "/v1/responses",
				Auth:     authStore,
				Client:   client,
				Now:      func() time.Time { return now },
			})

			results := make(chan branchCallResult, 1)
			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var out branchCallResult
				defer func() { results <- out }()
				raw, _ := io.ReadAll(r.Body)
				req := grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.5",
					BackendModel:    "grok-4.5",
				}
				out.prepRes, out.prepRefused = branch.Prepare(w, r, req)
				if out.prepRefused {
					return
				}
				out.handleCalled = true
				out.handleRes, out.handleErr = branch.Handle(w, r, req)
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)
			registerBranchTeardown(t, gw, client, upSrv, nil)

			status, _, respBody := postBranch(t, gw, "/v1/messages/count_tokens", tt.body, nil)
			got := recvWithin(t, "branch call result", results)

			assertBranchOutcome(t, got.prepRes, nil)
			assertBranchOutcome(t, got.handleRes, got.handleErr)
			if got.prepRefused {
				t.Fatalf("Prepare() refused count_tokens request: %+v", got.prepRes)
			}
			wantRoute := outcome.Route{
				SelectedModel:  "claude-grok-4.5",
				EffectiveModel: "grok-4.5",
				Backend:        "grok",
				Via:            "direct",
			}
			if diff := gocmp.Diff(outcome.Result{Route: wantRoute}, got.prepRes); diff != "" {
				t.Errorf("Prepare() result (-want +got):\n%s", diff)
			}
			if got.handleErr != nil {
				t.Fatalf("Handle() unexpected error: %v", got.handleErr)
			}
			wantHandle := outcome.Result{
				Route:     wantRoute,
				Status:    http.StatusOK,
				Contacted: true,
			}
			if diff := gocmp.Diff(wantHandle, got.handleRes); diff != "" {
				t.Errorf("Handle() result (-want +got):\n%s", diff)
			}
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if upstreamHits.Load() != 1 {
				t.Fatalf("upstream hits = %d, want 1", upstreamHits.Load())
			}

			upReq := recvWithin(t, "upstream request", upReqs)
			assertUpstreamRequestSafe(t, upReq.headers, upReq.body)
			if diff := gocmp.Diff(http.MethodPost, upReq.method); diff != "" {
				t.Errorf("upstream method (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("/v1/responses", upReq.path); diff != "" {
				t.Errorf("upstream path (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("grok-4.5", upReq.headers.Get("X-Grok-Model-Override")); diff != "" {
				t.Errorf("upstream X-Grok-Model-Override (-want +got):\n%s", diff)
			}
			upBody := decodeFullJSONObject(t, upReq.body)
			if diff := gocmp.Diff("grok-4.5", upBody["model"]); diff != "" {
				t.Errorf("upstream body model (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(float64(16), upBody["max_output_tokens"]); diff != "" {
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

	// Repeated headers distinguish raw upstream capture headers (unjoined values) from relayed
	// client headers (comma-joined values).
	rawDistinguishingHeaders := func(extra http.Header) http.Header {
		h := http.Header{
			"Request-Id": {"req-raw-1", "req-raw-2"},
			"Anthropic-Ratelimit-Input-Tokens-Remaining": {"42", "99"},
		}
		for k, vals := range extra {
			h[k] = slices.Clone(vals)
		}
		return h
	}

	const (
		wantTranslatedHello = `{"id":"resp_fixture","type":"message","role":"assistant","model":"claude-grok-4.7[1m]","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1}}}`
		wantTranslatedNull  = `{"id":"msg_grok_1700000000000","type":"message","role":"assistant","model":"claude-grok-4.7[1m]","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0}}}`
	)

	tests := map[string]struct {
		missingAuth         bool
		expiredShadow       bool
		transportErr        bool
		versionContent      string
		clientHeaders       map[string]string
		requestBody         string
		upstreamStatus      int
		upstreamHeaders     http.Header
		upstreamBody        string
		wantStatus          int
		wantUpstreamHit     int32
		wantErrType         string
		wantErrMessage      string
		wantErrPrefix       string
		wantTranslatedBody  string
		wantCheckClientHdrs bool
		wantResult          outcome.Result
		wantUsageEvent      string
		wantUsageStatus     int
		wantUsageInput      float64
		wantUsageOutput     float64
		wantUsageThinking   float64
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
			upstreamHeaders: rawDistinguishingHeaders(http.Header{
				"X-Upstream-Echo": {"kept"},
				"Connection":      {"keep-alive"},
				"Keep-Alive":      {"timeout=5"},
			}),
			upstreamBody:        `{"id":"resp_fixture","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1}}}`,
			wantStatus:          http.StatusOK,
			wantUpstreamHit:     1,
			wantTranslatedBody:  wantTranslatedHello,
			wantCheckClientHdrs: true,
			wantResult:          outcome.Result{Route: highEffortRoute, Status: http.StatusOK, Contacted: true},
			wantUsageEvent:      "gateway.token.usage",
			wantUsageStatus:     http.StatusOK,
			wantUsageInput:      3,
			wantUsageOutput:     2,
			wantUsageThinking:   1,
		},
		"success: translates unparseable 200 non-stream body from null": {
			requestBody:        `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:     http.StatusOK,
			upstreamHeaders:    rawDistinguishingHeaders(nil),
			upstreamBody:       `not valid json`,
			wantStatus:         http.StatusOK,
			wantUpstreamHit:    1,
			wantTranslatedBody: wantTranslatedNull,
			wantResult:         outcome.Result{Route: baseRoute, Status: http.StatusOK, Contacted: true},
			wantUsageEvent:     "gateway.token.usage",
			wantUsageStatus:    http.StatusOK,
			wantUsageInput:     0,
			wantUsageOutput:    0,
			wantUsageThinking:  0,
		},
		"error: translation failure before committing headers returns sanitized 502": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusOK,
			upstreamHeaders: rawDistinguishingHeaders(nil),
			upstreamBody:    `{"id":"resp_bad","status":"completed","output":[],"usage":{"input_tokens":{"toString":1}}}`,
			wantStatus:      http.StatusBadGateway,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  sanitizedTranslationErrMessage,
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusBadGateway, StatusOverride: "upstream_error", Contacted: true},
		},
		"error: encoding failure after translation before committing headers returns sanitized 502": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusOK,
			upstreamHeaders: rawDistinguishingHeaders(nil),
			upstreamBody:    encodeOverflowBody,
			wantStatus:      http.StatusBadGateway,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  sanitizedTranslationErrMessage,
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusBadGateway, StatusOverride: "upstream_error", Contacted: true},
		},
		"error: upstream 426 returns outdated CLI version message": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusUpgradeRequired,
			upstreamHeaders: rawDistinguishingHeaders(nil),
			upstreamBody:    `{"error":{"message":"upgrade required"}}`,
			wantStatus:      http.StatusUpgradeRequired,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: Grok CLI version header is outdated. Update the grok CLI, then retry.",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusUpgradeRequired, Contacted: true},
			wantUsageEvent:  "gateway.limit.signal",
			wantUsageStatus: http.StatusUpgradeRequired,
		},
		"error: upstream 401 maps to authentication_error": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusUnauthorized,
			upstreamHeaders: rawDistinguishingHeaders(nil),
			upstreamBody:    `{"error":{"message":"token rejected"}}`,
			wantStatus:      http.StatusUnauthorized,
			wantUpstreamHit: 1,
			wantErrType:     "authentication_error",
			wantErrMessage:  "model-gateway: token rejected",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusUnauthorized, Contacted: true},
			wantUsageEvent:  "gateway.limit.signal",
			wantUsageStatus: http.StatusUnauthorized,
		},
		"error: upstream 413 passes through as api_error without overflow normalization": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusRequestEntityTooLarge,
			upstreamHeaders: rawDistinguishingHeaders(nil),
			upstreamBody:    `{"error":{"message":"context length exceeded"}}`,
			wantStatus:      http.StatusRequestEntityTooLarge,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: context length exceeded",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusRequestEntityTooLarge, Contacted: true},
			wantUsageEvent:  "gateway.limit.signal",
			wantUsageStatus: http.StatusRequestEntityTooLarge,
		},
		"error: upstream 500 with string error preserves detail": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusInternalServerError,
			upstreamHeaders: rawDistinguishingHeaders(nil),
			upstreamBody:    `{"error":"upstream overloaded"}`,
			wantStatus:      http.StatusInternalServerError,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: upstream overloaded",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusInternalServerError, Contacted: true},
			wantUsageEvent:  "gateway.limit.signal",
			wantUsageStatus: http.StatusInternalServerError,
		},
		"error: upstream 503 with non-json body uses status fallback": {
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamStatus:  http.StatusServiceUnavailable,
			upstreamHeaders: rawDistinguishingHeaders(nil),
			upstreamBody:    `bad gateway html`,
			wantStatus:      http.StatusServiceUnavailable,
			wantUpstreamHit: 1,
			wantErrType:     "api_error",
			wantErrMessage:  "model-gateway: Grok upstream returned 503",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusServiceUnavailable, Contacted: true},
			wantUsageEvent:  "gateway.limit.signal",
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
		},
		"error: shadow mode expired auth returns 401 without contacting upstream": {
			expiredShadow:   true,
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusUnauthorized,
			wantUpstreamHit: 0,
			wantErrType:     "authentication_error",
			wantErrMessage:  "model-gateway: Grok CLI auth cannot refresh. Run `grok` and log in again.",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusUnauthorized, StatusOverride: "authentication_error", Contacted: false},
		},
		"error: upstream transport failure returns 502 api_error": {
			transportErr:    true,
			requestBody:     `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
			wantStatus:      http.StatusBadGateway,
			wantUpstreamHit: 0,
			wantErrType:     "api_error",
			wantErrPrefix:   "model-gateway: Grok upstream failed:",
			wantResult:      outcome.Result{Route: baseRoute, Status: http.StatusBadGateway, StatusOverride: "upstream_error", Contacted: true},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			upReqs := make(chan upstreamCapture, 1)
			var upstreamHits atomic.Int32

			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				raw, _ := io.ReadAll(r.Body)
				upReqs <- upstreamCapture{
					method:  r.Method,
					path:    r.URL.Path,
					headers: r.Header.Clone(),
					body:    string(raw),
				}
				for k, vals := range tt.upstreamHeaders {
					for _, v := range vals {
						w.Header().Add(k, v)
					}
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
			probe := newUsageCapture(t, now, tt.requestBody)
			client := newLoopbackClient(t, allowedAddr)
			branch := mustNewBranch(t, grok.BranchOptions{
				Endpoint:    endpoint,
				VersionFile: versionFile,
				Auth:        authStore,
				Client:      client,
				Now:         func() time.Time { return now },
			})

			results := make(chan handleCallResult, 1)
			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var out handleCallResult
				defer func() { results <- out }()
				raw, _ := io.ReadAll(r.Body)
				out.res, out.err = branch.Handle(w, r, grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.7[1m]",
					BackendModel:    "grok-4.7",
					Usage:           probe.capture,
				})
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)
			registerBranchTeardown(t, gw, client, upSrv, nil)

			status, respHeaders, respBody := postBranch(t, gw, "/v1/messages", tt.requestBody, tt.clientHeaders)
			got := recvWithin(t, "Handle result", results)

			assertBranchOutcome(t, got.res, got.err)
			if got.err != nil {
				t.Fatalf("Handle() unexpected error: %v", got.err)
			}
			if diff := gocmp.Diff(tt.wantResult, got.res); diff != "" {
				t.Errorf("Handle() outcome.Result (-want +got):\n%s", diff)
			}

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if hits := upstreamHits.Load(); hits != tt.wantUpstreamHit {
				t.Errorf("upstream hits = %d, want %d", hits, tt.wantUpstreamHit)
			}
			if gotCT := respHeaders.Get("Content-Type"); gotCT != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", gotCT)
			}
			if diff := gocmp.Diff(strconv.Itoa(len(respBody)), respHeaders.Get("Content-Length")); diff != "" {
				t.Errorf("Content-Length (-want +got):\n%s", diff)
			}

			if tt.wantErrType != "" {
				decoded := decodeFullJSONObject(t, respBody)
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
				if diff := gocmp.Diff(tt.wantTranslatedBody, respBody); diff != "" {
					t.Errorf("translated JSON response (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("req-raw-1, req-raw-2", respHeaders.Get("Request-Id")); diff != "" {
					t.Errorf("relayed Request-Id header (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("42, 99", respHeaders.Get("Anthropic-Ratelimit-Input-Tokens-Remaining")); diff != "" {
					t.Errorf("relayed rate-limit header (-want +got):\n%s", diff)
				}
				if tt.wantCheckClientHdrs {
					if diff := gocmp.Diff("kept", respHeaders.Get("X-Upstream-Echo")); diff != "" {
						t.Errorf("X-Upstream-Echo header (-want +got):\n%s", diff)
					}
					if gotKA := respHeaders.Get("Keep-Alive"); gotKA != "" {
						t.Errorf("Keep-Alive hop-by-hop header leaked: %q", gotKA)
					}
				}
			}

			rec := probe.finishRecord(t)
			switch tt.wantUsageEvent {
			case "":
				if rec != nil {
					t.Errorf("usage Finish() = %q, want nil record", rec.EventName)
				}
			case "gateway.token.usage":
				if rec == nil {
					t.Fatal("usage Finish() returned nil, want gateway.token.usage record")
				}
				wantAttrs := map[string]any{
					"event_name":                        "gateway.token.usage",
					"status_code":                       tt.wantUsageStatus,
					"response_mode":                     "json",
					"response_body_bytes":               int64(len(tt.upstreamBody) + len(tt.wantTranslatedBody)),
					"input_tokens":                      tt.wantUsageInput,
					"output_tokens":                     tt.wantUsageOutput,
					"thinking_tokens":                   tt.wantUsageThinking,
					"request_id":                        "req-raw-1",
					"request_id_source":                 "response_header",
					"rate_limit_input_tokens_remaining": float64(42),
				}
				for k, wantVal := range wantAttrs {
					if diff := gocmp.Diff(wantVal, rec.Attributes[k]); diff != "" {
						t.Errorf("usage record attribute %q (-want +got):\n%s", k, diff)
					}
				}
			case "gateway.limit.signal":
				if rec == nil {
					t.Fatal("usage Finish() returned nil, want gateway.limit.signal record")
				}
				wantAttrs := map[string]any{
					"event_name":                        "gateway.limit.signal",
					"status_code":                       tt.wantUsageStatus,
					"response_mode":                     nil,
					"response_body_bytes":               int64(0),
					"request_id":                        "req-raw-1",
					"request_id_source":                 "response_header",
					"rate_limit_input_tokens_remaining": float64(42),
				}
				for k, wantVal := range wantAttrs {
					if diff := gocmp.Diff(wantVal, rec.Attributes[k]); diff != "" {
						t.Errorf("usage record attribute %q (-want +got):\n%s", k, diff)
					}
				}
			}

			if tt.wantUpstreamHit > 0 {
				upReq := recvWithin(t, "upstream request", upReqs)
				assertUpstreamRequestSafe(t, upReq.headers, upReq.body)
				if tt.versionContent != "" {
					if diff := gocmp.Diff("1.2.3", upReq.headers.Get("X-Grok-Client-Version")); diff != "" {
						t.Errorf("upstream X-Grok-Client-Version (-want +got):\n%s", diff)
					}
					if diff := gocmp.Diff("xai-grok-build/1.2.3", upReq.headers.Get("User-Agent")); diff != "" {
						t.Errorf("upstream User-Agent (-want +got):\n%s", diff)
					}
					if diff := gocmp.Diff("xai-grok-cli", upReq.headers.Get("X-Grok-Client-Identifier")); diff != "" {
						t.Errorf("upstream X-Grok-Client-Identifier (-want +got):\n%s", diff)
					}
					if diff := gocmp.Diff("grok-4.7", upReq.headers.Get("X-Grok-Model-Override")); diff != "" {
						t.Errorf("upstream X-Grok-Model-Override (-want +got):\n%s", diff)
					}
				}
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
			client := newLoopbackClient(t, upSrv.Listener.Addr().String())
			branch := mustNewBranch(t, grok.BranchOptions{
				Endpoint: upSrv.URL + "/v1/responses",
				Auth:     authStore,
				Client:   client,
				Now:      func() time.Time { return now },
			})

			results := make(chan branchCallResult, 1)
			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var out branchCallResult
				defer func() { results <- out }()
				raw, _ := io.ReadAll(r.Body)
				req := grok.Request{
					RawBody:         raw,
					Path:            r.URL.Path,
					AdvertisedModel: "claude-grok-4.5",
					BackendModel:    "grok-4.5",
				}
				out.prepRes, out.prepRefused = branch.Prepare(w, r, req)
				if out.prepRefused {
					return
				}
				out.handleCalled = true
				out.handleRes, out.handleErr = branch.Handle(w, r, req)
			}))
			t.Cleanup(gw.Close)
			t.Cleanup(gw.Client().CloseIdleConnections)
			registerBranchTeardown(t, gw, client, upSrv, nil)

			status, hdrs, respBody := postBranch(t, gw, "/v1/messages", tt.body, nil)
			got := recvWithin(t, "branch call result", results)

			wantRoute := outcome.Route{
				SelectedModel:  "claude-grok-4.5",
				EffectiveModel: "grok-4.5",
				Backend:        "grok",
				Effort:         tt.wantEffort,
				Via:            "direct",
			}

			assertBranchOutcome(t, got.prepRes, nil)
			assertBranchOutcome(t, got.handleRes, got.handleErr)
			if got.prepRefused != tt.wantRefused {
				t.Fatalf("Prepare() refused = %v, want %v", got.prepRefused, tt.wantRefused)
			}
			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if hits := upstreamHits.Load(); hits != tt.wantUpstreamHit {
				t.Errorf("upstream hits = %d, want %d", hits, tt.wantUpstreamHit)
			}

			if tt.wantRefused {
				wantPrep := outcome.Result{
					Route:          wantRoute,
					Status:         http.StatusBadRequest,
					StatusOverride: "thread_unsupported",
					Contacted:      false,
				}
				if diff := gocmp.Diff(wantPrep, got.prepRes); diff != "" {
					t.Errorf("Prepare() refusal result (-want +got):\n%s", diff)
				}
				if got.handleCalled {
					t.Error("Handle() must not run when Prepare() refused")
				}
				if gotCT := hdrs.Get("Content-Type"); gotCT != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", gotCT)
				}
				if diff := gocmp.Diff(strconv.Itoa(len(threadRefusalBody)), hdrs.Get("Content-Length")); diff != "" {
					t.Errorf("Content-Length (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(threadRefusalBody, respBody); diff != "" {
					t.Errorf("thread refusal body (-want +got):\n%s", diff)
				}
				return
			}

			if diff := gocmp.Diff(outcome.Result{Route: wantRoute}, got.prepRes); diff != "" {
				t.Errorf("Prepare() accepted result (-want +got):\n%s", diff)
			}
			if got.handleErr != nil {
				t.Fatalf("Handle() unexpected error: %v", got.handleErr)
			}
			wantHandle := outcome.Result{
				Route:     wantRoute,
				Status:    http.StatusOK,
				Contacted: true,
			}
			if diff := gocmp.Diff(wantHandle, got.handleRes); diff != "" {
				t.Errorf("Handle() result (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBranchStream(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)

	t.Run("success: translates uninterrupted SSE stream and reports usage", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		const reqBody = `{"model":"claude-grok-4.5[1m]","stream":true,"messages":[{"role":"user","content":"hi"}]}`
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
			w.Header().Add("Request-Id", "req-sse-1")
			w.Header().Add("Request-Id", "req-sse-2")
			w.Header().Add("Anthropic-Ratelimit-Input-Tokens-Remaining", "55")
			w.Header().Add("Anthropic-Ratelimit-Input-Tokens-Remaining", "88")
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
		probe := newUsageCapture(t, now, reqBody)
		client := newLoopbackClient(t, upSrv.Listener.Addr().String())
		branch := mustNewBranch(t, grok.BranchOptions{
			Endpoint: upSrv.URL + "/v1/responses",
			Auth:     authStore,
			Client:   client,
			Now:      func() time.Time { return now },
		})

		results := make(chan handleCallResult, 1)
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var out handleCallResult
			defer func() { results <- out }()
			raw, _ := io.ReadAll(r.Body)
			out.res, out.err = branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
				Usage:           probe.capture,
			})
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)
		registerBranchTeardown(t, gw, client, upSrv, nil)

		status, hdrs, body := postBranch(t, gw, "/v1/messages", reqBody, nil)
		got := recvWithin(t, "Handle result", results)

		assertBranchOutcome(t, got.res, got.err)
		if got.err != nil {
			t.Fatalf("Handle() unexpected error: %v", got.err)
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
		if diff := gocmp.Diff(wantRes, got.res); diff != "" {
			t.Errorf("Handle() outcome.Result (-want +got):\n%s", diff)
		}
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if gotCT := hdrs.Get("Content-Type"); gotCT != "text/event-stream" {
			t.Errorf("Content-Type = %q, want text/event-stream", gotCT)
		}
		if gotHdr := hdrs.Get("X-Upstream-Header"); gotHdr != "present" {
			t.Errorf("X-Upstream-Header = %q, want present", gotHdr)
		}
		if diff := gocmp.Diff("req-sse-1, req-sse-2", hdrs.Get("Request-Id")); diff != "" {
			t.Errorf("relayed Request-Id header (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff("55, 88", hdrs.Get("Anthropic-Ratelimit-Input-Tokens-Remaining")); diff != "" {
			t.Errorf("relayed rate-limit header (-want +got):\n%s", diff)
		}
		for _, want := range []string{`"type":"message_start"`, `"model":"claude-grok-4.5[1m]"`, `"type":"text_delta","text":"hi"`, `"stop_reason":"end_turn"`, `"type":"message_stop"`} {
			if !strings.Contains(body, want) {
				t.Errorf("stream body missing %s", want)
			}
		}

		rec := probe.finishRecord(t)
		if rec == nil {
			t.Fatal("usage Finish() returned nil, want gateway.token.usage record")
		}
		wantAttrs := map[string]any{
			"event_name":                        "gateway.token.usage",
			"status_code":                       http.StatusOK,
			"response_mode":                     "sse",
			"response_body_bytes":               int64(len(ssePayload)),
			"input_tokens":                      float64(10),
			"output_tokens":                     float64(4),
			"thinking_tokens":                   float64(1),
			"request_id":                        "req-sse-1",
			"request_id_source":                 "response_header",
			"rate_limit_input_tokens_remaining": float64(55),
		}
		for k, wantVal := range wantAttrs {
			if diff := gocmp.Diff(wantVal, rec.Attributes[k]); diff != "" {
				t.Errorf("usage record attribute %q (-want +got):\n%s", k, diff)
			}
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
		client := newLoopbackClient(t, upSrv.Listener.Addr().String())
		branch := mustNewBranch(t, grok.BranchOptions{
			Endpoint: upSrv.URL + "/v1/responses",
			Auth:     authStore,
			Client:   client,
			Now:      func() time.Time { return now },
		})

		results := make(chan handleCallResult, 1)
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var out handleCallResult
			defer func() { results <- out }()
			raw, _ := io.ReadAll(r.Body)
			out.res, out.err = branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5",
				BackendModel:    "grok-4.5",
			})
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)
		registerBranchTeardown(t, gw, client, upSrv, nil)

		status, _, body := postBranch(t, gw, "/v1/messages", `{"model":"claude-grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
		got := recvWithin(t, "Handle result", results)

		assertBranchOutcome(t, got.res, got.err)
		if got.err != nil {
			t.Fatalf("Handle() unexpected error: %v", got.err)
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
		if diff := gocmp.Diff(wantRes, got.res); diff != "" {
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

		peer := startTruncatedSSEPeer(t, `data: {"type":"response.created","response":{"id":"resp_1"}}`+"\n\n")

		authStore := mustNewAuthStore(t, grok.AuthOptions{
			File:      validAuthFixture(t, now),
			Transport: denyNetworkTransport(t),
			Now:       func() time.Time { return now },
		})
		client := newLoopbackClient(t, peer.addr)
		branch := mustNewBranch(t, grok.BranchOptions{
			Endpoint: "http://" + peer.addr + "/v1/responses",
			Auth:     authStore,
			Client:   client,
			Now:      func() time.Time { return now },
		})

		results := make(chan handleCallResult, 1)
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var out handleCallResult
			defer func() { results <- out }()
			raw, _ := io.ReadAll(r.Body)
			out.res, out.err = branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
			})
			if errors.Is(out.err, http.ErrAbortHandler) {
				panic(http.ErrAbortHandler)
			}
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		// Bound client Do + io.Copy with a distinct watchdog so a hang fails as a timeout rather
		// than masquerading as an upstream abort.
		clientCtx, cancelClient := context.WithCancel(t.Context())
		defer cancelClient()
		registerBranchTeardown(t, gw, client, nil, func() {
			cancelClient()
			peer.shutdown()
		})

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
			assertBranchText(t, "HTTP POST error", err.Error())
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
		assertBranchText(t, "partial stream body", partial.String())
		if !strings.Contains(partial.String(), `"type":"message_start"`) {
			t.Error("partial stream body missing message_start")
		}

		got := recvWithin(t, "server handler completion", results)
		assertBranchOutcome(t, got.res, got.err)
		if !errors.Is(got.err, http.ErrAbortHandler) {
			t.Errorf("Handle() error = %v, want wrapped http.ErrAbortHandler", got.err)
		}
		wantRoute := outcome.Route{
			SelectedModel:  "claude-grok-4.5[1m]",
			EffectiveModel: "grok-4.5",
			Backend:        "grok",
			Via:            "direct",
		}
		if diff := gocmp.Diff(wantRoute, got.res.Route); diff != "" {
			t.Errorf("Handle() abort Route (-want +got):\n%s", diff)
		}
		if got.res.Status != http.StatusOK || (got.res.StatusOverride != "upstream_error" && got.res.StatusOverride != "upstream_aborted") || !got.res.Contacted || got.res.Cancelled {
			t.Errorf("Handle() abort outcome = %+v, want Status=200 StatusOverride=(upstream_error|upstream_aborted) Contacted=true Cancelled=false", got.res)
		}
	})
}

type disconnectWriter struct {
	recorder   *httptest.ResponseRecorder
	wrote      chan struct{}
	closed     atomic.Bool
	lateWrites atomic.Int64
}

func (w *disconnectWriter) Header() http.Header { return w.recorder.Header() }

func (w *disconnectWriter) WriteHeader(status int) {
	if w.closed.Load() {
		w.lateWrites.Add(1)
	}
	w.recorder.WriteHeader(status)
}

func (w *disconnectWriter) Write(p []byte) (int, error) {
	if w.closed.Load() {
		w.lateWrites.Add(1)
	}
	n, err := w.recorder.Write(p)
	if w.wrote != nil && strings.Contains(w.recorder.Body.String(), `"type":"message_start"`) {
		select {
		case w.wrote <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (w *disconnectWriter) Flush() { w.recorder.Flush() }

func TestBranchClientDisconnect(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })

	const (
		reqBody = `{"stream":true,"model":"claude-grok-4.5","messages":[{"role":"user","content":"hi"}]}`
		first   = `data: {"type":"response.created","response":{"id":"resp_live","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n"
		middle  = `data: {"type":"response.output_text.delta","delta":"after disconnect","response":{"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
		last    = `data: {"type":"response.completed","response":{"id":"resp_live","output":[],"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"
	)

	clientCtx, cancelClient := context.WithCancel(t.Context())
	defer cancelClient()

	provider := make(chan context.Context, 1)
	sendMiddle := make(chan struct{})
	releaseEnd := make(chan struct{})
	unlockMiddle := sync.OnceFunc(func() { close(sendMiddle) })
	unlockEnd := sync.OnceFunc(func() { close(releaseEnd) })

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		provider <- r.Context()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Add("Request-Id", "req-disc-1")
		w.Header().Add("Request-Id", "req-disc-2")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, first)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-sendMiddle:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, middle)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-releaseEnd:
			_, _ = io.WriteString(w, last)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(up.Client().CloseIdleConnections)

	client := newLoopbackClient(t, up.Listener.Addr().String())
	now := clock()
	branch := mustNewBranch(t, grok.BranchOptions{
		Endpoint: up.URL + "/v1/responses",
		Client:   client,
		Auth:     mustNewAuthStore(t, grok.AuthOptions{File: validAuthFixture(t, now), Transport: denyNetworkTransport(t), Now: clock}),
		Now:      clock,
	})

	probe := newUsageCapture(t, now, reqBody)
	onEvent := make(chan struct{}, 4)
	obs := &signalingCapture{Capture: probe.capture, onEvent: onEvent}

	r := httptest.NewRequestWithContext(clientCtx, http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	w := &disconnectWriter{recorder: httptest.NewRecorder(), wrote: make(chan struct{}, 1)}

	done := make(chan struct{})
	results := make(chan handleCallResult, 1)
	go func() {
		defer close(done)
		res, err := branch.Handle(w, r, grok.Request{
			Path:            r.URL.Path,
			RawBody:         []byte(reqBody),
			AdvertisedModel: "claude-grok-4.5",
			BackendModel:    "grok-4.5",
			Usage:           obs,
		})
		results <- handleCallResult{res: res, err: err}
	}()
	t.Cleanup(func() {
		cancelClient()
		unlockMiddle()
		unlockEnd()
		_ = client.Close()
		up.CloseClientConnections()
		waitClosed(t, "Handle goroutine after upstream shutdown", done)
	})

	// Wait for frame 1 to be both observed and written to the client before disconnecting.
	select {
	case <-onEvent:
	case <-done:
		t.Fatal("Handle returned before consuming the first upstream frame")
	case <-time.After(3 * time.Second):
		t.Fatal("Handle did not observe the open upstream stream")
	}
	select {
	case <-w.wrote:
	case <-done:
		t.Fatal("Handle returned before writing message_start for the first upstream frame")
	case <-time.After(3 * time.Second):
		t.Fatal("Handle did not write the first upstream frame to the client")
	}
	providerCtx := recvWithin(t, "upstream provider context", provider)

	// Disconnect downstream while the upstream stream stays held open.
	w.closed.Store(true)
	cancelClient()
	unlockMiddle()

	// The upstream request context must stay live and Handle must keep consuming the stream.
	select {
	case <-providerCtx.Done():
		t.Fatal("client cancellation ended the upstream request")
	case <-done:
		t.Fatal("Handle returned before draining the open upstream stream")
	case <-onEvent:
	case <-time.After(3 * time.Second):
		t.Fatal("Handle stopped consuming frames after the client disconnected")
	}
	if providerCtx.Err() != nil {
		t.Fatal("upstream context was canceled before the test released it")
	}

	unlockEnd()
	got := recvWithin(t, "Handle result after upstream EOF", results)
	waitClosed(t, "Handle goroutine after upstream EOF", done)

	assertBranchOutcome(t, got.res, got.err)
	if got.err != nil {
		t.Fatalf("Handle() unexpected error: %v", got.err)
	}
	if got.res.Cancelled {
		t.Error("Handle marked the branch result Cancelled; the worker owns that decision")
	}
	if gotWrites := w.lateWrites.Load(); gotWrites != 0 {
		t.Errorf("writes after client cancellation = %d, want 0", gotWrites)
	}

	rec := probe.finishRecord(t)
	if rec == nil {
		t.Fatal("usage Finish() returned nil, want gateway.token.usage record")
	}
	wantAttrs := map[string]any{
		"event_name":          "gateway.token.usage",
		"status_code":         http.StatusOK,
		"response_mode":       "sse",
		"response_body_bytes": int64(len(first) + len(middle) + len(last)),
		"input_tokens":        float64(1),
		"output_tokens":       float64(2),
		"request_id":          "req-disc-1",
		"request_id_source":   "response_header",
	}
	for k, wantVal := range wantAttrs {
		if diff := gocmp.Diff(wantVal, rec.Attributes[k]); diff != "" {
			t.Errorf("usage record attribute %q (-want +got):\n%s", k, diff)
		}
	}
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
		upSrv     *httptest.Server
		client    *upstream.Client
		results   <-chan handleCallResult
		state     *sentry.State
		target    sentry.Target
		forwarded *atomic.Int32
		setUsage  func(grok.UsageObserver)
		setKey    func(string)
		post      func(t *testing.T, path, body string) (int, string, handleCallResult)
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
		client := newLoopbackClient(t, upSrv.Listener.Addr().String())
		branch := mustNewBranch(t, grok.BranchOptions{
			Endpoint:      upSrv.URL + "/v1/responses",
			Auth:          authStore,
			Client:        client,
			Sentry:        state,
			SentryEnabled: enabled,
			Now:           func() time.Time { return now },
		})

		var mu sync.Mutex
		var nextUsage grok.UsageObserver
		sessionKey := sentry.SessionKey(enabled, "grok-sentry-session", nil)
		results := make(chan handleCallResult, 1)

		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var out handleCallResult
			defer func() { results <- out }()
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
			out.res, out.err = branch.Handle(w, r, req)
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)
		registerBranchTeardown(t, gw, client, upSrv, nil)

		return sentryHarness{
			gw:        gw,
			upSrv:     upSrv,
			client:    client,
			results:   results,
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
			post: func(t *testing.T, path, body string) (int, string, handleCallResult) {
				t.Helper()
				s, _, respBody := postBranch(t, gw, path, body, nil)
				got := recvWithin(t, "Handle result", results)
				assertBranchOutcome(t, got.res, got.err)
				return s, respBody, got
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
		status, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON)
		if got.err != nil {
			t.Fatalf("Handle() unexpected error: %v", got.err)
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

		if s, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON); s != http.StatusOK || got.err != nil {
			t.Fatalf("turn 1 status=%d err=%v, want 200/nil", s, got.err)
		}
		// Fire latch using exact original whitespace-rich body.
		assertCrossing(t, h.state, key, h.target, prettyOrdinaryJSON, 101, 100)

		if s, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON); s != http.StatusOK || got.err != nil {
			t.Fatalf("incomplete JSON turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "after incomplete JSON", h.state, key, h.target, prettyOrdinaryJSON)

		if s, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON); s != http.StatusOK || got.err != nil {
			t.Fatalf("missing-status JSON turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "after missing-status JSON", h.state, key, h.target, prettyOrdinaryJSON)

		if s, _, got := h.post(t, "/v1/messages", prettyCompactionSSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("compaction JSON turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "after compaction JSON", h.state, key, h.target, prettyOrdinaryJSON)

		if s, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON); s != http.StatusOK || got.err != nil {
			t.Fatalf("completed ordinary JSON turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertCrossing(t, h.state, key, h.target, prettyOrdinaryJSON, 105, 100)
		if h.forwarded.Load() != 5 {
			t.Fatalf("forwarded = %d, want 5", h.forwarded.Load())
		}
	})

	t.Run("success: SSE observes per-frame usage in flight and rearms only deliverable provider-completed non-compaction", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		releaseInFlight := make(chan struct{})
		releaseNow := sync.OnceFunc(func() { close(releaseInFlight) })

		h := newHarness(t, true, []http.HandlerFunc{
			// 1: running compaction emits two frames at 102 and blocks until released, then completes at 104.
			// Receiving the second frame's observation guarantees frame 1 finished both usage and sentry updates.
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, sseEvent("response.created", 102))
				_ = http.NewResponseController(w).Flush()
				_, _ = io.WriteString(w, sseEvent("response.in_progress", 102))
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
			// 6: deliverable completed ordinary SSE whose response.completed omits usage -> uses prior frame's 107 and rearms.
			sseResp(sseEvent("response.created", 107) + sseEventNoUsage("response.completed")),
			// 7: deliverable completed ordinary SSE rearms at 108 so turn 8 can test compaction exemption while armed.
			sseResp(sseEvent("response.completed", 108)),
			// 8: compaction updates armed usage to 109 without consuming the armed refusal.
			sseResp(sseEvent("response.completed", 109)),
			// 9: low-usage compaction (20 < low watermark 75) resets fired latch.
			sseResp(sseEvent("response.completed", 20)),
			// 10: high incomplete ordinary SSE (110, no response.completed) arms the reset latch.
			sseResp(sseEvent("response.created", 110)),
		})
		key := sentry.SessionKey(true, "grok-sentry-session", nil)

		// 1. Verify in-flight branch observation while stream is still open using a real *usage.Capture.
		probe := newUsageCapture(t, now, prettyCompactionSSE)
		onEvent := make(chan struct{}, 8)
		h.setUsage(&signalingCapture{Capture: probe.capture, onEvent: onEvent})

		type postOutcome struct {
			status int
			body   string
			err    error
		}
		inFlightCtx, cancelInFlight := context.WithCancel(t.Context())
		defer cancelInFlight()
		inFlightDone := make(chan struct{})
		inFlightResCh := make(chan postOutcome, 1)
		go func() {
			defer close(inFlightDone)
			s, _, b, err := doBranchPost(inFlightCtx, h.gw.Client(), h.gw.URL+"/v1/messages", prettyCompactionSSE, nil)
			inFlightResCh <- postOutcome{status: s, body: b, err: err}
		}()
		t.Cleanup(func() {
			cancelInFlight()
			releaseNow()
			h.gw.CloseClientConnections()
			_ = h.client.Close()
			h.upSrv.CloseClientConnections()
			waitClosed(t, "in-flight compaction client goroutine", inFlightDone)
		})

		// Wait for two observed usage frames so frame 1 is guaranteed fully processed by both
		// Usage.ObserveEvent and Sentry.ObserveWire regardless of intra-frame call order.
		for frame := 1; frame <= 2; frame++ {
			select {
			case <-onEvent:
			case <-inFlightDone:
				inFlightRes := recvWithin(t, "early in-flight response", inFlightResCh)
				got := recvWithin(t, "early in-flight Handle result", h.results)
				assertBranchOutcome(t, got.res, got.err)
				t.Fatalf("in-flight compaction ended before observing frame %d (status=%d httpError=%t handleErr=%v)", frame, inFlightRes.status, inFlightRes.err != nil, got.err)
			case <-time.After(3 * time.Second):
				releaseNow()
				t.Fatalf("timed out waiting for branch-side in-flight SSE frame %d before EOF", frame)
			}
		}

		// While the stream is still open, Check must immediately cross at 102 with no polling.
		assertCrossing(t, h.state, key, h.target, prettyOrdinarySSE, 102, 100)

		// Let the compaction finish with response.completed at 104; compaction must NOT rearm the fired latch.
		releaseNow()
		waitClosed(t, "in-flight compaction completion", inFlightDone)
		inFlightRes := recvWithin(t, "in-flight compaction response", inFlightResCh)
		inFlightHandle := recvWithin(t, "in-flight compaction Handle result", h.results)
		assertBranchOutcome(t, inFlightHandle.res, inFlightHandle.err)
		if inFlightRes.err != nil || inFlightRes.status != http.StatusOK || inFlightHandle.err != nil {
			t.Fatalf("in-flight compaction failed: status=%d httpError=%t handleErr=%v", inFlightRes.status, inFlightRes.err != nil, inFlightHandle.err)
		}
		if got := probe.emittedCount(); got != 0 {
			t.Errorf("in-flight compaction emitted %d usage records before Finish(), want 0", got)
		}
		h.setUsage(nil)
		assertNoCrossing(t, "after completed compaction", h.state, key, h.target, prettyOrdinarySSE)

		// 2. Orderly EOF without provider response.completed synthesizes message_stop but must NOT complete sentry.
		s, b, got := h.post(t, "/v1/messages", prettyOrdinarySSE)
		if s != http.StatusOK || got.err != nil {
			t.Fatalf("EOF-without-completed turn status=%d err=%v, want 200/nil", s, got.err)
		}
		if !strings.Contains(b, `"type":"message_stop"`) {
			t.Error("EOF-without-completed stream did not emit synthesized message_stop")
		}
		assertNoCrossing(t, "after synthetic message_stop without response.completed", h.state, key, h.target, prettyOrdinarySSE)

		// 3. Bare error + response.completed must NOT rearm.
		if s, _, got = h.post(t, "/v1/messages", prettyOrdinarySSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("bare error turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "after bare error + response.completed", h.state, key, h.target, prettyOrdinarySSE)

		// 4. response.failed + response.completed must NOT rearm.
		if s, _, got = h.post(t, "/v1/messages", prettyOrdinarySSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("response.failed turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "after response.failed + response.completed", h.state, key, h.target, prettyOrdinarySSE)

		// 5. response.incomplete + response.completed must NOT rearm.
		if s, _, got = h.post(t, "/v1/messages", prettyOrdinarySSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("response.incomplete turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "after response.incomplete + response.completed", h.state, key, h.target, prettyOrdinarySSE)

		// 6. Deliverable completed ordinary SSE where response.completed itself omits usage -> rearms using 107!
		// Assert the exact 107 crossing immediately before any further observation overwrites it.
		if s, _, got = h.post(t, "/v1/messages", prettyOrdinarySSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("completed-without-final-usage turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertCrossing(t, h.state, key, h.target, prettyOrdinarySSE, 107, 100)

		// 7-8. Rearm at 108, verify compaction is exempt while armed, and verify compaction updates usage to 109.
		if s, _, got = h.post(t, "/v1/messages", prettyOrdinarySSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("rearm turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "compaction exempt while armed", h.state, key, h.target, prettyCompactionSSE)
		if s, _, got = h.post(t, "/v1/messages", prettyCompactionSSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("compaction bypass turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertCrossing(t, h.state, key, h.target, prettyOrdinarySSE, 109, 100)

		// 9. Low-usage compaction resets the fired latch.
		if s, _, got = h.post(t, "/v1/messages", prettyCompactionSSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("low-usage compaction status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "immediately after low-usage compaction", h.state, key, h.target, prettyOrdinarySSE)

		// 10. A subsequent high incomplete ordinary SSE (no response.completed) can cross ONLY if the
		// low-usage compaction actually reset the fired latch.
		if s, _, got = h.post(t, "/v1/messages", prettyOrdinarySSE); s != http.StatusOK || got.err != nil {
			t.Fatalf("post-reset high incomplete turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertCrossing(t, h.state, key, h.target, prettyOrdinarySSE, 110, 100)

		if h.forwarded.Load() != 10 {
			t.Fatalf("forwarded = %d, want 10", h.forwarded.Load())
		}
	})

	t.Run("error: provider completion followed by broken transport does not rearm fired sentry", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		state := &sentry.State{}
		target := grokSentryTarget(t, "grok-4.5", 100)
		key := sentry.SessionKey(true, "grok-sentry-broken-eof", nil)

		// Pre-arm and fire the latch so only a clean deliverable end may rearm it.
		state.Observe(key, target, []byte(`{"type":"message_delta","usage":{"input_tokens":101}}`), false)
		assertCrossing(t, state, key, target, prettyOrdinarySSE, 101, 100)
		assertNoCrossing(t, "pre-fired latch before broken transport", state, key, target, prettyOrdinarySSE)

		frames := sseEvent("response.created", 112) + sseEvent("response.completed", 112)
		peer := startTruncatedSSEPeer(t, frames)

		authStore := mustNewAuthStore(t, grok.AuthOptions{
			File:      validAuthFixture(t, now),
			Transport: denyNetworkTransport(t),
			Now:       func() time.Time { return now },
		})
		client := newLoopbackClient(t, peer.addr)
		branch := mustNewBranch(t, grok.BranchOptions{
			Endpoint:      "http://" + peer.addr + "/v1/responses",
			Auth:          authStore,
			Client:        client,
			Sentry:        state,
			SentryEnabled: true,
			Now:           func() time.Time { return now },
		})

		results := make(chan handleCallResult, 1)
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var out handleCallResult
			defer func() { results <- out }()
			raw, _ := io.ReadAll(r.Body)
			out.res, out.err = branch.Handle(w, r, grok.Request{
				RawBody:         raw,
				Path:            r.URL.Path,
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
				SessionKey:      key,
				SentryTarget:    target,
			})
			if errors.Is(out.err, http.ErrAbortHandler) {
				panic(http.ErrAbortHandler)
			}
		}))
		t.Cleanup(gw.Close)
		t.Cleanup(gw.Client().CloseIdleConnections)

		clientCtx, cancelClient := context.WithCancel(t.Context())
		defer cancelClient()
		registerBranchTeardown(t, gw, client, nil, func() {
			cancelClient()
			peer.shutdown()
		})

		var watchdogFired atomic.Bool
		watchdog := time.AfterFunc(3*time.Second, func() {
			watchdogFired.Store(true)
			cancelClient()
		})
		defer watchdog.Stop()

		req, err := http.NewRequestWithContext(clientCtx, http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(prettyOrdinarySSE))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := gw.Client().Do(req)
		if watchdogFired.Load() {
			t.Fatal("client timed out before receiving response headers")
		}
		if err != nil {
			assertBranchText(t, "HTTP POST error", err.Error())
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
			t.Fatal("client timed out waiting for broken transport abort")
		}
		if readErr == nil {
			t.Fatal("expected broken transport error after response.completed, got clean EOF")
		}
		assertBranchText(t, "partial stream body", partial.String())

		got := recvWithin(t, "broken-transport Handle completion", results)
		assertBranchOutcome(t, got.res, got.err)
		if !errors.Is(got.err, http.ErrAbortHandler) {
			t.Errorf("Handle() error = %v, want wrapped http.ErrAbortHandler", got.err)
		}
		if got.res.Status != http.StatusOK || (got.res.StatusOverride != "upstream_error" && got.res.StatusOverride != "upstream_aborted") || !got.res.Contacted || got.res.Cancelled {
			t.Errorf("Handle() outcome = %+v, want Status=200 StatusOverride=(upstream_error|upstream_aborted) Contacted=true Cancelled=false", got.res)
		}
		// Because transport broke before clean EOF, sentry completion must NOT have fired on response.completed.
		assertNoCrossing(t, "after response.completed followed by broken transport", state, key, target, prettyOrdinarySSE)
	})

	t.Run("success: sentry disabled never records usage", func(t *testing.T) {
		t.Cleanup(func() { goleak.VerifyNone(t) })

		h := newHarness(t, false, []http.HandlerFunc{
			jsonResp(http.StatusOK, `{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":500}}`),
		})
		key := sentry.SessionKey(true, "grok-sentry-session", nil)
		h.setKey(key)

		if s, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON); s != http.StatusOK || got.err != nil {
			t.Fatalf("turn 1 status=%d err=%v, want 200/nil", s, got.err)
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
		if s, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON); s != http.StatusOK || got.err != nil {
			t.Fatalf("main session turn status=%d err=%v, want 200/nil", s, got.err)
		}

		h.setKey(keySibling)
		if s, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON); s != http.StatusOK || got.err != nil {
			t.Fatalf("sibling agent turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "sibling agent", h.state, keySibling, h.target, prettyOrdinaryJSON)

		h.setKey(keyOther)
		if s, _, got := h.post(t, "/v1/messages", prettyOrdinaryJSON); s != http.StatusOK || got.err != nil {
			t.Fatalf("other session turn status=%d err=%v, want 200/nil", s, got.err)
		}
		assertNoCrossing(t, "other session", h.state, keyOther, h.target, prettyOrdinaryJSON)

		assertCrossing(t, h.state, keyMain, h.target, prettyOrdinaryJSON, 101, 100)
		if h.forwarded.Load() != 3 {
			t.Fatalf("forwarded = %d, want 3", h.forwarded.Load())
		}
	})
}
