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
	"net/http"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func TestRequestHeaders(t *testing.T) {
	// Source sets from lib/request-worker.js:1827-1828,2262,2591, lib/runtime.js:42-43, and lib/control-auth.js:8.
	fullInput := func() http.Header {
		return http.Header{
			"Host":                          {"127.0.0.1:18764"},
			"Connection":                    {"keep-alive"},
			"Content-Length":                {"99999"},
			"Keep-Alive":                    {"timeout=5"},
			"X-Model-Gateway-Control-Token": {"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			"Traceparent":                   {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
			"Tracestate":                    {"vendor=private"},
			"Baggage":                       {"key=private"},
			"Authorization":                 {"Bearer anthropic-token"},
			"Proxy-Authorization":           {"Basic cHJveHk6c2VjcmV0"},
			"X-Api-Key":                     {"sk-ant-secret"},
			"Cookie":                        {"session=abc"},
			"Content-Type":                  {"application/json"},
			"Anthropic-Version":             {"2023-06-01"},
			"Anthropic-Beta":                {"prompt-caching-2024-07-31"},
			"Accept-Encoding":               {"br, gzip"},
			"X-Claude-Code-Session-Id":      {"sess-1"},
			"X-Claude-Code-Agent-Id":        {"agent-1"},
			"X-Claude-Code-Parent-Agent-Id": {"parent-1"},
			"User-Agent":                    {"claude-cli/2.1.332"},
			"X-Stainless-Lang":              {"js"},
			"X-Custom-Multi":                {"one", "two"},
		}
	}

	nonCanonicalInput := func() http.Header {
		return http.Header{
			"host":                          {"127.0.0.1:18764"},
			"CONNECTION":                    {"close"},
			"content-length":                {"12"},
			"keep-alive":                    {"timeout=5"},
			"x-model-gateway-control-token": {"secret-control-token"},
			"traceparent":                   {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
			"TRACESTATE":                    {"vendor=x"},
			"baggage":                       {"k=v"},
			"authorization":                 {"Bearer lower-auth"},
			"PROXY-AUTHORIZATION":           {"Basic upper-proxy"},
			"x-api-key":                     {"lower-key"},
			"COOKIE":                        {"c=1"},
			"anthropic-version":             {"2023-06-01"},
		}
	}

	tests := map[string]struct {
		in     http.Header
		target upstream.Target
		want   http.Header
	}{
		"success: anthropic strips hop-by-hop, control, and trace headers but keeps auth": {
			in:     fullInput(),
			target: upstream.TargetAnthropic,
			want: http.Header{
				"Authorization":                 {"Bearer anthropic-token"},
				"Proxy-Authorization":           {"Basic cHJveHk6c2VjcmV0"},
				"X-Api-Key":                     {"sk-ant-secret"},
				"Cookie":                        {"session=abc"},
				"Content-Type":                  {"application/json"},
				"Anthropic-Version":             {"2023-06-01"},
				"Anthropic-Beta":                {"prompt-caching-2024-07-31"},
				"Accept-Encoding":               {"br, gzip"},
				"X-Claude-Code-Session-Id":      {"sess-1"},
				"X-Claude-Code-Agent-Id":        {"agent-1"},
				"X-Claude-Code-Parent-Agent-Id": {"parent-1"},
				"User-Agent":                    {"claude-cli/2.1.332"},
				"X-Stainless-Lang":              {"js"},
				"X-Custom-Multi":                {"one", "two"},
			},
		},
		"success: proxy also strips authorization, proxy-authorization, x-api-key, and cookie": {
			in:     fullInput(),
			target: upstream.TargetProxy,
			want: http.Header{
				"Content-Type":                  {"application/json"},
				"Anthropic-Version":             {"2023-06-01"},
				"Anthropic-Beta":                {"prompt-caching-2024-07-31"},
				"Accept-Encoding":               {"br, gzip"},
				"X-Claude-Code-Session-Id":      {"sess-1"},
				"X-Claude-Code-Agent-Id":        {"agent-1"},
				"X-Claude-Code-Parent-Agent-Id": {"parent-1"},
				"User-Agent":                    {"claude-cli/2.1.332"},
				"X-Stainless-Lang":              {"js"},
				"X-Custom-Multi":                {"one", "two"},
			},
		},
		"success: grok forwards no client headers": {
			in:     fullInput(),
			target: upstream.TargetGrok,
			want:   http.Header{},
		},
		"success: non-canonical keys are stripped case-insensitively on proxy": {
			in:     nonCanonicalInput(),
			target: upstream.TargetProxy,
			want: http.Header{
				"Anthropic-Version": {"2023-06-01"},
			},
		},
		"success: non-canonical keys keep auth and strip control and trace on anthropic": {
			in:     nonCanonicalInput(),
			target: upstream.TargetAnthropic,
			want: http.Header{
				"Authorization":       {"Bearer lower-auth"},
				"Proxy-Authorization": {"Basic upper-proxy"},
				"X-Api-Key":           {"lower-key"},
				"Cookie":              {"c=1"},
				"Anthropic-Version":   {"2023-06-01"},
			},
		},
		"success: nil input returns empty header map": {
			in:     nil,
			target: upstream.TargetAnthropic,
			want:   http.Header{},
		},
		"error: unknown target fails closed with empty headers": {
			in:     fullInput(),
			target: upstream.Target("unknown"),
			want:   http.Header{},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := tt.in.Clone()
			got := upstream.RequestHeaders(tt.in, tt.target)
			if got == nil {
				t.Fatal("RequestHeaders returned nil map")
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("RequestHeaders mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(before, tt.in); diff != "" {
				t.Fatalf("RequestHeaders mutated input (-before +after):\n%s", diff)
			}
			// Mutating a returned value slice must not alias the input slice.
			if vals := got["X-Custom-Multi"]; len(vals) > 0 {
				vals[0] = "mutated"
				if tt.in["X-Custom-Multi"][0] == "mutated" {
					t.Fatal("RequestHeaders aliased input value slice")
				}
			}
		})
	}
}

func TestResponseHeaders(t *testing.T) {
	// Source strips only transfer-encoding, connection, and keep-alive (lib/request-worker.js:1841-1842,2265-2266).
	tests := map[string]struct {
		in   http.Header
		want http.Header
	}{
		"success: strips only transfer-encoding, connection, and keep-alive": {
			in: http.Header{
				"Transfer-Encoding":                 {"chunked"},
				"Connection":                        {"keep-alive"},
				"Keep-Alive":                        {"timeout=5"},
				"Content-Type":                      {"application/json"},
				"Content-Length":                    {"42"},
				"Content-Encoding":                  {"gzip"},
				"Upgrade":                           {"websocket"},
				"Te":                                {"trailers"},
				"Trailer":                           {"X-Checksum"},
				"Proxy-Authenticate":                {"Basic realm=test"},
				"Proxy-Authorization":               {"Basic abc"},
				"X-Openai-Request-Id":               {"req_123"},
				"Openai-Processing-Ms":              {"15"},
				"Retry-After":                       {"30", "120"},
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700", "1790597715"},
				"Set-Cookie":                        {"a=1", "b=2"},
			},
			want: http.Header{
				"Content-Type":                      {"application/json"},
				"Content-Length":                    {"42"},
				"Content-Encoding":                  {"gzip"},
				"Upgrade":                           {"websocket"},
				"Te":                                {"trailers"},
				"Trailer":                           {"X-Checksum"},
				"Proxy-Authenticate":                {"Basic realm=test"},
				"Proxy-Authorization":               {"Basic abc"},
				"X-Openai-Request-Id":               {"req_123"},
				"Openai-Processing-Ms":              {"15"},
				"Retry-After":                       {"30", "120"},
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700", "1790597715"},
				"Set-Cookie":                        {"a=1", "b=2"},
			},
		},
		"success: non-canonical keys are stripped case-insensitively and values are not trimmed": {
			in: http.Header{
				"transfer-encoding":                 {"chunked"},
				"CONNECTION":                        {"close"},
				"keep-alive":                        {"timeout=5"},
				"Retry-After":                       {" Mon, 28 Sep 2026 10:05:00 GMT "},
				"anthropic-ratelimit-unified-reset": {" 1790597700 "},
			},
			want: http.Header{
				"Retry-After":                       {" Mon, 28 Sep 2026 10:05:00 GMT "},
				"Anthropic-Ratelimit-Unified-Reset": {" 1790597700 "},
			},
		},
		"success: nil input returns empty header map": {
			in:   nil,
			want: http.Header{},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := tt.in.Clone()
			got := upstream.ResponseHeaders(tt.in)
			if got == nil {
				t.Fatal("ResponseHeaders returned nil map")
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Fatalf("ResponseHeaders mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(before, tt.in); diff != "" {
				t.Fatalf("ResponseHeaders mutated input (-before +after):\n%s", diff)
			}
			if vals := got["Retry-After"]; len(vals) > 0 {
				vals[0] = "mutated"
				if tt.in["Retry-After"][0] == "mutated" {
					t.Fatal("ResponseHeaders aliased input value slice")
				}
			}
		})
	}
}

func TestResponseHeadersUpstreamStatePreservation(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		headers http.Header
		wantExp string
	}{
		"success: untrimmed Retry-After date is not pre-trimmed and falls through to reset header": {
			headers: http.Header{
				"Retry-After":                       {" Mon, 28 Sep 2026 10:05:00 GMT "},
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700"},
			},
			wantExp: "2026-09-28T12:15:00.000Z",
		},
		"success: first Retry-After value wins when repeated": {
			headers: http.Header{
				"Retry-After": {"30", "120"},
			},
			wantExp: "2026-09-28T10:00:30.000Z",
		},
		"success: duplicate reset headers stay separate so upstreamstate joins them to default 60s": {
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700", "1790597715"},
			},
			wantExp: "2026-09-28T10:01:00.000Z",
		},
		"success: single reset header sets exact epoch expiry": {
			headers: http.Header{
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700"},
			},
			wantExp: "2026-09-28T12:15:00.000Z",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			filtered := upstream.ResponseHeaders(tt.headers)
			if filtered == nil {
				t.Fatal("ResponseHeaders returned nil map")
			}
			store := upstreamstate.Store{
				Dir: t.TempDir(),
				Now: func() time.Time { return now },
			}
			block, err := store.SetBlocked(http.StatusTooManyRequests, "headers:x-openai-request-id", filtered)
			if err != nil {
				t.Fatalf("SetBlocked returned error: %v", err)
			}
			if diff := gocmp.Diff(tt.wantExp, block.ExpiresAt); diff != "" {
				t.Fatalf("expiry mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
