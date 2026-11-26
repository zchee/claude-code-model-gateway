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

package reply_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
)

func TestMain(m *testing.M) { venue.Require(); goleak.VerifyTestMain(m) }

func equal[T any](t *testing.T, want, got T) {
	t.Helper()
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

type proxyFixture struct{ *httptest.Server }

func (p proxyFixture) forward(ctx context.Context) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL+"/v1/messages", nil)
	if err != nil {
		return nil, err
	}
	return p.Client().Do(r)
}

func upstream(t *testing.T, serve http.HandlerFunc) proxyFixture {
	t.Helper()
	server := httptest.NewServer(serve)
	t.Cleanup(server.Close)
	return proxyFixture{server}
}

func answer(t *testing.T, code int, body string) proxyFixture {
	t.Helper()
	return upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Test", "kept")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
}

func handle(t *testing.T, req reply.Request, forward func(context.Context) (*http.Response, error)) (*httptest.ResponseRecorder, reply.Result) {
	t.Helper()
	w := httptest.NewRecorder()
	result, err := reply.Handle(t.Context(), w, req, forward)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result.Stream != nil {
		t.Cleanup(func() { _ = result.Stream.Body.Close() })
	}
	return w, result
}

func TestCodexAuthFailure(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(t.TempDir(), "stable gateway")
	if err := os.Symlink(executable, stable); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	const detail = "API Error: 401 Not authenticated. Run: claude-code-proxy codex auth login"
	tests := map[string]struct {
		status int
		stream bool
	}{"error: buffered unauthorized": {401, false}, "error: streaming unauthorized": {401, true}, "error: forbidden auth detail": {403, false}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			body := fmt.Appendf(nil, `{"stream":%t}`, tc.stream)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				b, e := io.ReadAll(r.Body)
				if e != nil {
					t.Error(e)
				}
				equal(t, string(body), string(b))
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, detail)
			}))
			t.Cleanup(server.Close)
			w, _ := handle(t, reply.Request{Provider: "codex", AdvertisedModel: "claude-gpt-5.6-sol", Body: body, CommandPath: stable}, func(ctx context.Context) (*http.Response, error) {
				r, e := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(string(body)))
				if e != nil {
					return nil, e
				}
				return server.Client().Do(r)
			})
			equal(t, tc.status, w.Code)
			equal(t, "application/json", w.Header().Get("Content-Type"))
			equal(t, strconv.Itoa(w.Body.Len()), w.Header().Get("Content-Length"))
			var got struct {
				Error struct{ Type, Message string }
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got, json.MatchCaseInsensitiveNames(true)); err != nil {
				t.Fatal(err)
			}
			equal(t, "authentication_error", got.Error.Type)
			match := regexp.MustCompile("`\"([^\"]+)\" login`").FindStringSubmatch(got.Error.Message)
			if len(match) != 2 {
				t.Fatalf("missing quoted stable login command: %q", got.Error.Message)
			}
			equal(t, stable, match[1])
			info, e := os.Stat(match[1])
			if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				t.Errorf("advised path %q is not an executable file: %v", match[1], e)
			}
			if regexp.MustCompile(`(?i)claude-code-proxy\s+codex\s+auth\s+login`).MatchString(got.Error.Message) || !strings.Contains(got.Error.Message, "API Error: 401 Not authenticated.") {
				t.Errorf("upstream detail was not preserved and sanitized: %q", got.Error.Message)
			}
		})
	}
	equal(t, int32(3), attempts.Load())
}

func TestOpenAIRejectionEvidence(t *testing.T) {
	tests := map[string]struct {
		status  int
		headers http.Header
		body    string
		want    bool
	}{
		"header": {403, http.Header{"X-Openai-Request-Id": {"req_1"}}, "", true}, "body": {429, nil, "OpenAI rejected this request", true}, "unattributed": {403, nil, "forbidden", false}, "server_error": {500, http.Header{"X-Openai-Request-Id": {"req_1"}}, "", false}, "word_boundary": {401, nil, "notopenai", false}, "processing_header": {401, http.Header{"OPENAI-PROCESSING-MS": {""}}, "", true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			equal(t, tc.want, reply.HasOpenAIRejectionEvidence(tc.status, tc.headers, []byte(tc.body)))
		})
	}
}

func TestUpgradeRetry(t *testing.T) {
	const rejection = `{"error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`
	tests := map[string]struct {
		provider  string
		retries   *int
		delay     *time.Duration
		recover   bool
		wantCodes []time.Duration
		status    int
		want      string
	}{
		"success: third attempt":           {"codex", nil, nil, true, []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond}, 200, `{"model":"public"}`},
		"error: exhausted non Codex proxy": {"kimi", nil, nil, false, []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond}, 503, `{"type":"error","error":{"type":"api_error","message":"model-gateway: Codex WebSocket upgrade was temporarily rejected after 3 attempts; retry the request."}}`},
		"error: configured retry budget":   {"codex", new(1), new(7 * time.Millisecond), false, []time.Duration{0, 7 * time.Millisecond}, 503, `{"type":"error","error":{"type":"api_error","message":"model-gateway: Codex WebSocket upgrade was temporarily rejected after 2 attempts; retry the request."}}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var calls []time.Duration
				w, res := handle(t, reply.Request{Provider: tc.provider, AdvertisedModel: "public", Body: []byte(`{}`), Retries: tc.retries, RetryDelay: tc.delay}, func(context.Context) (*http.Response, error) {
					calls = append(calls, time.Since(start))
					if len(calls) > len(tc.wantCodes) {
						return nil, errors.New("unexpected extra attempt")
					}
					r := httptest.NewRecorder()
					r.Header().Set("X-Attempt", "kept")
					if tc.recover && len(calls) == 3 {
						r.WriteHeader(200)
						_, _ = io.WriteString(r, `{"model":"gpt-test"}`)
					} else {
						r.WriteHeader(403)
						_, _ = io.WriteString(r, rejection)
					}
					return r.Result(), nil
				})
				equal(t, tc.wantCodes, calls)
				equal(t, tc.status, w.Code)
				equal(t, tc.want, w.Body.String())
				equal(t, "kept", w.Header().Get("X-Attempt"))
				equal(t, tc.status, res.CaptureStatus)
				equal(t, strconv.Itoa(w.Body.Len()), w.Header().Get("Content-Length"))
			})
		})
	}
}
