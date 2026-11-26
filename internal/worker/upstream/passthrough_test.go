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

package upstream_test

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

type capturedRequest struct {
	Method        string
	RequestURI    string
	Host          string
	ContentLength int64
	Header        http.Header
	Body          []byte
}

func startCaptureServer(t *testing.T, status int, replyBody string) (*httptest.Server, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var recorded []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mu.Lock()
		recorded = append(recorded, capturedRequest{
			Method:        r.Method,
			RequestURI:    r.URL.RequestURI(),
			Host:          r.Host,
			ContentLength: r.ContentLength,
			Header:        r.Header.Clone(),
			Body:          body,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, replyBody)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]capturedRequest, len(recorded))
		copy(out, recorded)
		return out
	}
}

func makeFourMiBValidJSON() []byte {
	const prefix = `{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[{"role":"user","content":"`
	const suffix = `"}]}`
	const targetSize = 4 * 1024 * 1024
	pad := targetSize - len(prefix) - len(suffix)
	var b bytes.Buffer
	b.Grow(targetSize)
	b.WriteString(prefix)
	b.Write(bytes.Repeat([]byte("a"), pad))
	b.WriteString(suffix)
	return b.Bytes()
}

func TestPassthroughFidelity(t *testing.T) {
	table := proxymodels.NewTable([]proxymodels.Route{{ID: "gpt-5.6-sol", Provider: "codex"}})
	snapshot := route.NewSnapshot(policy.NewResolver(policy.ResolverInputs{Routes: table}), "proxy", nil, nil)

	srv, captured := startCaptureServer(t, http.StatusOK, `{"id":"msg_1","type":"message"}`)
	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })

	tests := map[string]struct {
		requestURI string
		body       []byte
	}{
		"success: 4 MiB valid JSON": {
			requestURI: "/v1/messages",
			body:       makeFourMiBValidJSON(),
		},
		"success: two top-level model members with last selecting Anthropic": {
			requestURI: "/v1/messages",
			body:       []byte(`{"model":"claude-gpt-5.6-sol","messages":[{"role":"user","content":"hi"}],"model":"claude-sonnet-4-6"}`),
		},
		"success: nested duplicate keys": {
			requestURI: "/v1/messages",
			body:       []byte(`{"model":"claude-sonnet-4-6","metadata":{"k":1,"k":2,"nested":{"z":"first","a":"mid","z":"second"}}}`),
		},
		"success: ill-formed UTF-8 in a string": {
			requestURI: "/v1/messages",
			body:       []byte("{\"model\":\"claude-sonnet-4-6\",\"messages\":[{\"role\":\"user\",\"content\":\"bad-\xff\xfe-utf8\"}]}"),
		},
		"success: not JSON": {
			requestURI: "/v1/messages",
			body:       []byte(`this is not valid json {{{`),
		},
		"success: empty body": {
			requestURI: "/v1/messages",
			body:       []byte{},
		},
		"success: integer above 2^53": {
			requestURI: "/v1/messages",
			body:       []byte(`{"model":"claude-sonnet-4-6","seed":9007199254740993}`),
		},
		"success: overflow float 1e400": {
			requestURI: "/v1/messages",
			body:       []byte(`{"model":"claude-sonnet-4-6","huge":1e400}`),
		},
		"success: escaped lone surrogate in a non-identifier field": {
			requestURI: "/v1/messages",
			body:       []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"lone \uD800 surrogate"}]}`),
		},
		"success: CRLF-heavy formatting": {
			requestURI: "/v1/messages",
			body:       []byte("{\r\n  \"model\": \"claude-sonnet-4-6\",\r\n  \"messages\": [\r\n    {\r\n      \"role\": \"user\",\r\n      \"content\": \"line1\\r\\nline2\"\r\n    }\r\n  ]\r\n}\r\n"),
		},
		"success: leading byte-order mark": {
			requestURI: "/v1/messages",
			body:       append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"model":"claude-sonnet-4-6","messages":[]}`)...),
		},
		"success: JSON sent to a path outside /v1/messages": {
			requestURI: "/v1/complete?beta=true&x=1",
			body:       []byte(`{"model":"claude-gpt-5.6-sol","prompt":"outside messages path"}`),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			decision := snapshot.Classify(http.MethodPost, tt.requestURI, tt.body)
			if diff := gocmp.Diff("anthropic", decision.Branch); diff != "" {
				t.Fatalf("Classify branch (-want +got):\n%s", diff)
			}

			before := len(captured())
			resp, err := client.Do(t.Context(), upstream.Request{
				Target:        upstream.TargetAnthropic,
				BaseURL:       srv.URL,
				Method:        http.MethodPost,
				RequestURI:    tt.requestURI,
				ClientHeaders: http.Header{"Content-Type": {"application/json"}},
				Body:          tt.body,
			})
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
				t.Fatalf("status (-want +got):\n%s", diff)
			}

			gotAll := captured()
			if len(gotAll) != before+1 {
				t.Fatalf("expected 1 upstream request, got %d", len(gotAll)-before)
			}
			got := gotAll[len(gotAll)-1]
			if diff := gocmp.Diff(tt.requestURI, got.RequestURI); diff != "" {
				t.Fatalf("RequestURI (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(int64(len(tt.body)), got.ContentLength); diff != "" {
				t.Fatalf("ContentLength (-want +got):\n%s", diff)
			}
			wantSum := sha256.Sum256(tt.body)
			gotSum := sha256.Sum256(got.Body)
			if diff := gocmp.Diff(wantSum, gotSum); diff != "" {
				t.Fatalf("SHA-256 mismatch (want len=%d, got len=%d) (-want +got):\n%s", len(tt.body), len(got.Body), diff)
			}
		})
	}
}

func assertNoSentinelAnywhere(t *testing.T, got capturedRequest, sentinels []string) {
	t.Helper()
	for _, s := range sentinels {
		if strings.Contains(string(got.Body), s) {
			t.Errorf("sentinel %q leaked into upstream body", s)
		}
		for k, vals := range got.Header {
			if strings.Contains(k, s) {
				t.Errorf("sentinel %q leaked into upstream header name %q", s, k)
			}
			for _, v := range vals {
				if strings.Contains(v, s) {
					t.Errorf("sentinel %q leaked into upstream header %q", s, k)
				}
			}
		}
	}
}

func TestHeaderSentinels(t *testing.T) {
	const (
		sentinelAuth        = "sentinel-authorization-8c1a"
		sentinelProxyAuth   = "sentinel-proxy-authorization-4f2b"
		sentinelAPIKey      = "sentinel-x-api-key-9e3c"
		sentinelCookie      = "sentinel-cookie-7d4e"
		sentinelControl     = "sentinel-control-token-5a6f"
		sentinelTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
		sentinelTracestate  = "vendor=sentinel-tracestate-1b2c"
		sentinelBaggage     = "key=sentinel-baggage-3d4e"
	)

	clientHeaders := func() http.Header {
		return http.Header{
			"Host":                          {"127.0.0.1:18764"},
			"Connection":                    {"keep-alive"},
			"Keep-Alive":                    {"timeout=5"},
			"Content-Length":                {"999999"},
			"Authorization":                 {"Bearer " + sentinelAuth},
			"Proxy-Authorization":           {"Basic " + sentinelProxyAuth},
			"X-Api-Key":                     {sentinelAPIKey},
			"Cookie":                        {"session=" + sentinelCookie},
			"X-Model-Gateway-Control-Token": {sentinelControl},
			"Traceparent":                   {sentinelTraceparent},
			"Tracestate":                    {sentinelTracestate},
			"Baggage":                       {sentinelBaggage},
			"Content-Type":                  {"application/json"},
			"Anthropic-Version":             {"2023-06-01"},
			"Anthropic-Beta":                {"prompt-caching-2024-07-31"},
			"Accept-Encoding":               {"br, gzip"},
			"X-Claude-Code-Session-Id":      {"sess-sentinel"},
			"X-Claude-Code-Agent-Id":        {"agent-sentinel"},
			"X-Claude-Code-Parent-Agent-Id": {"parent-sentinel"},
			"User-Agent":                    {"claude-cli/2.1.332"},
			"X-Stainless-Lang":              {"js"},
			"X-Multi":                       {"one", "two"},
		}
	}

	anthropicSrv, anthropicCaptured := startCaptureServer(t, http.StatusOK, `{"ok":true}`)
	proxySrv, proxyCaptured := startCaptureServer(t, http.StatusOK, `{"ok":true}`)
	grokSrv, grokCaptured := startCaptureServer(t, http.StatusOK, `{"ok":true}`)

	hostOf := func(rawURL string) string {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", rawURL, err)
		}
		return u.Host
	}

	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })

	allAuthAndControlSentinels := []string{
		sentinelAuth, sentinelProxyAuth, sentinelAPIKey, sentinelCookie, sentinelControl,
		sentinelTraceparent, sentinelTracestate, sentinelBaggage,
	}

	tests := map[string]struct {
		req      upstream.Request
		captured func() []capturedRequest
		check    func(t *testing.T, got capturedRequest)
	}{
		"success: anthropic strips control and trace headers while keeping auth and client headers": {
			req: upstream.Request{
				Target:        upstream.TargetAnthropic,
				BaseURL:       anthropicSrv.URL,
				Method:        http.MethodPost,
				RequestURI:    "/v1/messages?beta=true",
				ClientHeaders: clientHeaders(),
				Body:          []byte(`{"model":"claude-sonnet-4-6"}`),
			},
			captured: anthropicCaptured,
			check: func(t *testing.T, got capturedRequest) {
				assertNoSentinelAnywhere(t, got, []string{sentinelControl, sentinelTraceparent, sentinelTracestate, sentinelBaggage})
				if diff := gocmp.Diff(hostOf(anthropicSrv.URL), got.Host); diff != "" {
					t.Errorf("Host (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(int64(len(`{"model":"claude-sonnet-4-6"}`)), got.ContentLength); diff != "" {
					t.Errorf("ContentLength (-want +got):\n%s", diff)
				}
				for _, stripped := range []string{"X-Model-Gateway-Control-Token", "Traceparent", "Tracestate", "Baggage", "Keep-Alive", "Connection"} {
					if vals := got.Header.Values(stripped); len(vals) != 0 {
						t.Errorf("header %q should be stripped, got %v", stripped, vals)
					}
				}
				wantKept := map[string][]string{
					"Authorization":                 {"Bearer " + sentinelAuth},
					"Proxy-Authorization":           {"Basic " + sentinelProxyAuth},
					"X-Api-Key":                     {sentinelAPIKey},
					"Cookie":                        {"session=" + sentinelCookie},
					"Content-Type":                  {"application/json"},
					"Anthropic-Version":             {"2023-06-01"},
					"Anthropic-Beta":                {"prompt-caching-2024-07-31"},
					"Accept-Encoding":               {"br, gzip"},
					"X-Claude-Code-Session-Id":      {"sess-sentinel"},
					"X-Claude-Code-Agent-Id":        {"agent-sentinel"},
					"X-Claude-Code-Parent-Agent-Id": {"parent-sentinel"},
					"User-Agent":                    {"claude-cli/2.1.332"},
					"X-Stainless-Lang":              {"js"},
					"X-Multi":                       {"one", "two"},
				}
				for k, want := range wantKept {
					if diff := gocmp.Diff(want, got.Header.Values(k)); diff != "" {
						t.Errorf("header %q (-want +got):\n%s", k, diff)
					}
				}
			},
		},
		"success: proxy strips all auth, control, and trace sentinels while keeping non-auth client headers": {
			req: upstream.Request{
				Target:        upstream.TargetProxy,
				BaseURL:       proxySrv.URL,
				Method:        http.MethodPost,
				RequestURI:    "/v1/messages",
				ClientHeaders: clientHeaders(),
				Body:          []byte(`{"model":"gpt-5.6-sol"}`),
			},
			captured: proxyCaptured,
			check: func(t *testing.T, got capturedRequest) {
				assertNoSentinelAnywhere(t, got, allAuthAndControlSentinels)
				if diff := gocmp.Diff(hostOf(proxySrv.URL), got.Host); diff != "" {
					t.Errorf("Host (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(int64(len(`{"model":"gpt-5.6-sol"}`)), got.ContentLength); diff != "" {
					t.Errorf("ContentLength (-want +got):\n%s", diff)
				}
				for _, stripped := range []string{
					"Authorization", "Proxy-Authorization", "X-Api-Key", "Cookie",
					"X-Model-Gateway-Control-Token", "Traceparent", "Tracestate", "Baggage", "Keep-Alive", "Connection",
				} {
					if vals := got.Header.Values(stripped); len(vals) != 0 {
						t.Errorf("header %q should be stripped, got %v", stripped, vals)
					}
				}
				wantKept := map[string][]string{
					"Content-Type":                  {"application/json"},
					"Anthropic-Version":             {"2023-06-01"},
					"Anthropic-Beta":                {"prompt-caching-2024-07-31"},
					"Accept-Encoding":               {"br, gzip"},
					"X-Claude-Code-Session-Id":      {"sess-sentinel"},
					"X-Claude-Code-Agent-Id":        {"agent-sentinel"},
					"X-Claude-Code-Parent-Agent-Id": {"parent-sentinel"},
					"User-Agent":                    {"claude-cli/2.1.332"},
					"X-Stainless-Lang":              {"js"},
					"X-Multi":                       {"one", "two"},
				}
				for k, want := range wantKept {
					if diff := gocmp.Diff(want, got.Header.Values(k)); diff != "" {
						t.Errorf("header %q (-want +got):\n%s", k, diff)
					}
				}
			},
		},
		"success: grok strips all client headers and posts only Grok headers to configured endpoint": {
			req: upstream.Request{
				Target:        upstream.TargetGrok,
				BaseURL:       grokSrv.URL + "/v1/responses",
				Method:        http.MethodGet,
				RequestURI:    "/v1/messages/count_tokens?ignored=1",
				ClientHeaders: clientHeaders(),
				GrokHeaders: http.Header{
					"Authorization":            {"Bearer grok-native-token"},
					"Content-Type":             {"application/json"},
					"User-Agent":               {"xai-grok-build/1.0.60"},
					"X-Grok-Client-Version":    {"1.0.60"},
					"X-Grok-Client-Identifier": {"xai-grok-cli"},
					"X-Grok-Model-Override":    {"grok-4.7"},
				},
				Body: []byte(`{"model":"grok-4.7","input":[]}`),
			},
			captured: grokCaptured,
			check: func(t *testing.T, got capturedRequest) {
				assertNoSentinelAnywhere(t, got, allAuthAndControlSentinels)
				if diff := gocmp.Diff(http.MethodPost, got.Method); diff != "" {
					t.Errorf("Method (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("/v1/responses", got.RequestURI); diff != "" {
					t.Errorf("RequestURI (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(hostOf(grokSrv.URL), got.Host); diff != "" {
					t.Errorf("Host (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(int64(len(`{"model":"grok-4.7","input":[]}`)), got.ContentLength); diff != "" {
					t.Errorf("ContentLength (-want +got):\n%s", diff)
				}
				for _, clientOnly := range []string{
					"Proxy-Authorization", "X-Api-Key", "Cookie", "X-Model-Gateway-Control-Token",
					"Traceparent", "Tracestate", "Baggage", "Anthropic-Version", "Anthropic-Beta",
					"Accept-Encoding", "X-Claude-Code-Session-Id", "X-Stainless-Lang", "X-Multi",
				} {
					if vals := got.Header.Values(clientOnly); len(vals) != 0 {
						t.Errorf("client header %q leaked to Grok: %v", clientOnly, vals)
					}
				}
				wantGrok := map[string][]string{
					"Authorization":            {"Bearer grok-native-token"},
					"Content-Type":             {"application/json"},
					"User-Agent":               {"xai-grok-build/1.0.60"},
					"X-Grok-Client-Version":    {"1.0.60"},
					"X-Grok-Client-Identifier": {"xai-grok-cli"},
					"X-Grok-Model-Override":    {"grok-4.7"},
				}
				for k, want := range wantGrok {
					if diff := gocmp.Diff(want, got.Header.Values(k)); diff != "" {
						t.Errorf("Grok header %q (-want +got):\n%s", k, diff)
					}
				}
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := len(tt.captured())
			resp, err := client.Do(t.Context(), tt.req)
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			gotAll := tt.captured()
			if len(gotAll) != before+1 {
				t.Fatalf("expected 1 upstream request, got %d", len(gotAll)-before)
			}
			tt.check(t, gotAll[len(gotAll)-1])
		})
	}
}

func TestNoSynthesizedHeaders(t *testing.T) {
	srv, captured := startCaptureServer(t, http.StatusOK, `{"ok":true}`)
	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })

	tests := map[string]upstream.Request{
		"success: anthropic does not synthesize User-Agent or Accept-Encoding": {
			Target:        upstream.TargetAnthropic,
			BaseURL:       srv.URL,
			Method:        http.MethodPost,
			RequestURI:    "/v1/messages",
			ClientHeaders: http.Header{"Content-Type": {"application/json"}},
			Body:          []byte(`{"model":"claude-sonnet-4-6"}`),
		},
		"success: proxy does not synthesize User-Agent or Accept-Encoding": {
			Target:        upstream.TargetProxy,
			BaseURL:       srv.URL,
			Method:        http.MethodPost,
			RequestURI:    "/v1/messages",
			ClientHeaders: http.Header{"Content-Type": {"application/json"}},
			Body:          []byte(`{"model":"gpt-5.6-sol"}`),
		},
		"success: grok does not synthesize User-Agent or Accept-Encoding": {
			Target:      upstream.TargetGrok,
			BaseURL:     srv.URL + "/v1/responses",
			GrokHeaders: http.Header{"Content-Type": {"application/json"}},
			Body:        []byte(`{"model":"grok-4.7"}`),
		},
	}

	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			before := len(captured())
			resp, err := client.Do(t.Context(), req)
			if err != nil {
				t.Fatalf("Do returned error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatalf("reading response body: %v", err)
			}
			gotAll := captured()
			if len(gotAll) != before+1 {
				t.Fatalf("expected 1 request, got %d", len(gotAll)-before)
			}
			got := gotAll[len(gotAll)-1]
			if vals := got.Header.Values("User-Agent"); len(vals) != 0 {
				t.Errorf("unexpected synthesized User-Agent: %v", vals)
			}
			if vals := got.Header.Values("Accept-Encoding"); len(vals) != 0 {
				t.Errorf("unexpected synthesized Accept-Encoding: %v", vals)
			}
		})
	}
}
