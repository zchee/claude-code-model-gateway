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
	"slices"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// lowerHeaderMap folds header keys to lowercase and fails if two keys in h differ only in case.
func lowerHeaderMap(t *testing.T, h http.Header) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(h))
	for k, vals := range h {
		lk := strings.ToLower(k)
		if _, dup := out[lk]; dup {
			t.Fatalf("header map contains duplicate case-variant key %q", k)
		}
		out[lk] = slices.Clone(vals)
	}
	return out
}

// headerValuesCI returns the values for name matched case-insensitively and fails if h holds
// duplicate case-variant keys for name.
func headerValuesCI(t *testing.T, h http.Header, name string) []string {
	t.Helper()
	var found []string
	seen := false
	for k, vals := range h {
		if !strings.EqualFold(k, name) {
			continue
		}
		if seen {
			t.Fatalf("header map contains duplicate case-variant key for %q (%q)", name, k)
		}
		seen = true
		found = slices.Clone(vals)
	}
	return found
}

func TestRequestHeaders(t *testing.T) {
	// Source sets from lib/request-worker.js:1827-1828,2262,2591, lib/runtime.js:42-43, and lib/control-auth.js:8.
	fullInput := func() http.Header {
		return http.Header{
			"Host":                          {"127.0.0.1:18764"},
			"Connection":                    {"close, x-client-hop"},
			"Content-Length":                {"99999"},
			"Keep-Alive":                    {"timeout=5"},
			"X-Model-Gateway-Control-Token": {"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			"Traceparent":                   {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
			"Tracestate":                    {"vendor=private"},
			"Baggage":                       {"key=private"},
			"Authorization":                 {"Bearer anthropic-token", "Bearer dropped-second"},
			"Proxy-Authorization":           {"Basic cHJveHk6c2VjcmV0", "Basic dropped-proxy"},
			"X-Api-Key":                     {"sk-ant-secret"},
			"Cookie":                        {"session=abc", "theme=dark"},
			"Content-Type":                  {"application/json", "text/plain"},
			"Anthropic-Version":             {"2023-06-01"},
			"Anthropic-Beta":                {"prompt-caching-2024-07-31", "interleaved-thinking-2025-05-14"},
			"Accept-Encoding":               {"br, gzip"},
			"X-Claude-Code-Session-Id":      {"sess-1"},
			"X-Claude-Code-Agent-Id":        {"agent-1"},
			"X-Claude-Code-Parent-Agent-Id": {"parent-1"},
			"User-Agent":                    {"claude-cli/2.1.332", "ignored-ua"},
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
			"COOKIE":                        {"c=1", "c=2"},
			"anthropic-version":             {"2023-06-01"},
		}
	}

	tests := map[string]struct {
		in     http.Header
		target upstream.Target
		want   map[string][]string
	}{
		"success: anthropic strips hop-by-hop, control, and trace headers and normalizes repeated values": {
			in:     fullInput(),
			target: upstream.TargetAnthropic,
			want: map[string][]string{
				"authorization":                 {"Bearer anthropic-token"},
				"proxy-authorization":           {"Basic cHJveHk6c2VjcmV0"},
				"x-api-key":                     {"sk-ant-secret"},
				"cookie":                        {"session=abc; theme=dark"},
				"content-type":                  {"application/json"},
				"anthropic-version":             {"2023-06-01"},
				"anthropic-beta":                {"prompt-caching-2024-07-31, interleaved-thinking-2025-05-14"},
				"accept-encoding":               {"br, gzip"},
				"x-claude-code-session-id":      {"sess-1"},
				"x-claude-code-agent-id":        {"agent-1"},
				"x-claude-code-parent-agent-id": {"parent-1"},
				"user-agent":                    {"claude-cli/2.1.332"},
				"x-stainless-lang":              {"js"},
				"x-custom-multi":                {"one, two"},
			},
		},
		"success: proxy strips auth headers and normalizes repeated values": {
			in:     fullInput(),
			target: upstream.TargetProxy,
			want: map[string][]string{
				"content-type":                  {"application/json"},
				"anthropic-version":             {"2023-06-01"},
				"anthropic-beta":                {"prompt-caching-2024-07-31, interleaved-thinking-2025-05-14"},
				"accept-encoding":               {"br, gzip"},
				"x-claude-code-session-id":      {"sess-1"},
				"x-claude-code-agent-id":        {"agent-1"},
				"x-claude-code-parent-agent-id": {"parent-1"},
				"user-agent":                    {"claude-cli/2.1.332"},
				"x-stainless-lang":              {"js"},
				"x-custom-multi":                {"one, two"},
			},
		},
		"success: grok forwards no client headers": {
			in:     fullInput(),
			target: upstream.TargetGrok,
			want:   map[string][]string{},
		},
		"success: non-canonical keys are stripped case-insensitively on proxy": {
			in:     nonCanonicalInput(),
			target: upstream.TargetProxy,
			want: map[string][]string{
				"anthropic-version": {"2023-06-01"},
			},
		},
		"success: non-canonical keys keep normalized auth and strip control and trace on anthropic": {
			in:     nonCanonicalInput(),
			target: upstream.TargetAnthropic,
			want: map[string][]string{
				"authorization":       {"Bearer lower-auth"},
				"proxy-authorization": {"Basic upper-proxy"},
				"x-api-key":           {"lower-key"},
				"cookie":              {"c=1; c=2"},
				"anthropic-version":   {"2023-06-01"},
			},
		},
		"success: nil input returns empty header map": {
			in:     nil,
			target: upstream.TargetAnthropic,
			want:   map[string][]string{},
		},
		"error: unknown target fails closed with empty headers": {
			in:     fullInput(),
			target: upstream.Target("unknown"),
			want:   map[string][]string{},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := tt.in.Clone()
			got := upstream.RequestHeaders(tt.in, tt.target)
			if got == nil {
				t.Fatal("RequestHeaders returned nil map")
			}
			gotLower := lowerHeaderMap(t, got)
			if diff := gocmp.Diff(tt.want, gotLower); diff != "" {
				t.Fatalf("RequestHeaders mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(before, tt.in); diff != "" {
				t.Fatalf("RequestHeaders mutated input (-before +after):\n%s", diff)
			}
			// Mutating a returned value slice must not alias any input slice.
			for k, vals := range got {
				if len(vals) > 0 {
					vals[0] = "mutated"
					for inK, inVals := range tt.in {
						if strings.EqualFold(inK, k) && len(inVals) > 0 && inVals[0] == "mutated" {
							t.Fatalf("RequestHeaders aliased input value slice for %q", inK)
						}
					}
					break
				}
			}
		})
	}
}

func TestRequestHeadersCaseFoldedOccurrences(t *testing.T) {
	tests := map[string]struct {
		target   upstream.Target
		wantAuth bool
	}{
		"success: anthropic collapses differently cased occurrences into single fields": {
			target:   upstream.TargetAnthropic,
			wantAuth: true,
		},
		"success: proxy strips all case variants of auth and collapses kept fields": {
			target:   upstream.TargetProxy,
			wantAuth: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := http.Header{
				"x-model-gateway-control-token": {"ctrl-1"},
				"X-MODEL-GATEWAY-CONTROL-TOKEN": {"ctrl-2"},
				"authorization":                 {"Bearer same-token"},
				"AUTHORIZATION":                 {"Bearer same-token"},
				"cookie":                        {"a=1"},
				"COOKIE":                        {"b=2"},
				"x-case-multi":                  {"one"},
				"X-CASE-MULTI":                  {"two"},
			}
			got := upstream.RequestHeaders(in, tt.target)
			if got == nil {
				t.Fatal("RequestHeaders returned nil map")
			}
			gotLower := lowerHeaderMap(t, got)

			if _, ok := gotLower["x-model-gateway-control-token"]; ok {
				t.Fatalf("control token survived case-variant input: %v", gotLower["x-model-gateway-control-token"])
			}

			multiVals := gotLower["x-case-multi"]
			if len(multiVals) != 1 {
				t.Fatalf("x-case-multi occurrences = %v, want 1 joined value", multiVals)
			}
			parts := strings.Split(multiVals[0], ", ")
			slices.Sort(parts)
			if diff := gocmp.Diff([]string{"one", "two"}, parts); diff != "" {
				t.Fatalf("x-case-multi joined parts (-want +got):\n%s", diff)
			}

			if !tt.wantAuth {
				if _, ok := gotLower["authorization"]; ok {
					t.Fatalf("authorization survived on proxy: %v", gotLower["authorization"])
				}
				if _, ok := gotLower["cookie"]; ok {
					t.Fatalf("cookie survived on proxy: %v", gotLower["cookie"])
				}
				return
			}

			if diff := gocmp.Diff([]string{"Bearer same-token"}, gotLower["authorization"]); diff != "" {
				t.Fatalf("authorization (-want +got):\n%s", diff)
			}
			cookieVals := gotLower["cookie"]
			if len(cookieVals) != 1 {
				t.Fatalf("cookie occurrences = %v, want 1 joined value", cookieVals)
			}
			cookieParts := strings.Split(cookieVals[0], "; ")
			slices.Sort(cookieParts)
			if diff := gocmp.Diff([]string{"a=1", "b=2"}, cookieParts); diff != "" {
				t.Fatalf("cookie joined parts (-want +got):\n%s", diff)
			}
		})
	}
}

func TestResponseHeaders(t *testing.T) {
	// Source strips only transfer-encoding, connection, and keep-alive (lib/request-worker.js:1841-1842,2265-2266),
	// and forwards Node's parsed header object where singletons keep first, Set-Cookie stays separate, and
	// ordinary repeated fields are joined with ", ".
	tests := map[string]struct {
		in   http.Header
		want map[string][]string
	}{
		"success: strips only transfer-encoding, connection, and keep-alive and normalizes relay values": {
			in: http.Header{
				"Transfer-Encoding":                 {"chunked"},
				"Connection":                        {"keep-alive"},
				"Keep-Alive":                        {"timeout=5"},
				"Content-Type":                      {"application/json", "text/plain"},
				"Content-Length":                    {"42"},
				"Content-Encoding":                  {"gzip"},
				"Upgrade":                           {"websocket"},
				"Te":                                {"trailers"},
				"Trailer":                           {"X-Checksum"},
				"Proxy-Authenticate":                {"Basic realm=test"},
				"Proxy-Authorization":               {"Basic abc", "Basic def"},
				"X-Openai-Request-Id":               {"req_123"},
				"Openai-Processing-Ms":              {"15"},
				"Retry-After":                       {"30", "120"},
				"Anthropic-Ratelimit-Unified-Reset": {"1790597700", "1790597715"},
				"Set-Cookie":                        {"a=1", "b=2"},
				"X-Custom-Multi":                    {"one", "two"},
			},
			want: map[string][]string{
				"content-type":                      {"application/json"},
				"content-length":                    {"42"},
				"content-encoding":                  {"gzip"},
				"upgrade":                           {"websocket"},
				"te":                                {"trailers"},
				"trailer":                           {"X-Checksum"},
				"proxy-authenticate":                {"Basic realm=test"},
				"proxy-authorization":               {"Basic abc"},
				"x-openai-request-id":               {"req_123"},
				"openai-processing-ms":              {"15"},
				"retry-after":                       {"30"},
				"anthropic-ratelimit-unified-reset": {"1790597700, 1790597715"},
				"set-cookie":                        {"a=1", "b=2"},
				"x-custom-multi":                    {"one, two"},
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
			want: map[string][]string{
				"retry-after":                       {" Mon, 28 Sep 2026 10:05:00 GMT "},
				"anthropic-ratelimit-unified-reset": {" 1790597700 "},
			},
		},
		"success: nil input returns empty header map": {
			in:   nil,
			want: map[string][]string{},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := tt.in.Clone()
			got := upstream.ResponseHeaders(tt.in)
			if got == nil {
				t.Fatal("ResponseHeaders returned nil map")
			}
			gotLower := lowerHeaderMap(t, got)
			if diff := gocmp.Diff(tt.want, gotLower); diff != "" {
				t.Fatalf("ResponseHeaders mismatch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(before, tt.in); diff != "" {
				t.Fatalf("ResponseHeaders mutated input (-before +after):\n%s", diff)
			}
			for k, vals := range got {
				if strings.EqualFold(k, "Set-Cookie") && len(vals) > 0 {
					vals[0] = "mutated"
					if tt.in["Set-Cookie"][0] == "mutated" {
						t.Fatal("ResponseHeaders aliased input Set-Cookie value slice")
					}
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
		"success: duplicate reset headers join to default 60s": {
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
			before := tt.headers.Clone()
			if upstream.ResponseHeaders(tt.headers) == nil {
				t.Fatal("ResponseHeaders returned nil map")
			}
			if diff := gocmp.Diff(before, tt.headers); diff != "" {
				t.Fatalf("preparing relay headers altered capture headers (-want +got):\n%s", diff)
			}
			store := upstreamstate.Store{
				Dir: t.TempDir(),
				Now: func() time.Time { return now },
			}
			block, err := store.SetBlocked(http.StatusTooManyRequests, "headers:x-openai-request-id", tt.headers)
			if err != nil {
				t.Fatalf("SetBlocked returned error: %v", err)
			}
			if diff := gocmp.Diff(tt.wantExp, block.ExpiresAt); diff != "" {
				t.Fatalf("expiry mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
