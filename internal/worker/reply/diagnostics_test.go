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

package reply_test

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
)

// withDiagnostics routes the reply's diagnostic lines to sink. It is the only place these tests
// name the diagnostics field of reply.Request.
func withDiagnostics(req reply.Request, sink io.Writer) reply.Request {
	req.Diagnostics = sink
	return req
}

// diagnosed is one buffered reply together with the diagnostics it wrote.
type diagnosed struct {
	code   int
	header http.Header
	body   string
	lines  string
}

// diagnose answers req from an upstream that replies with status, header and body, and returns
// the client response and the diagnostic output.
func diagnose(t *testing.T, req reply.Request, status int, header http.Header, body string) diagnosed {
	t.Helper()
	var sink bytes.Buffer
	w, _ := handle(t, withDiagnostics(req, &sink), newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		for name, values := range header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	got := w.Result().Header.Clone()
	got.Del("Date")
	return diagnosed{code: w.Code, header: got, body: w.Body.String(), lines: sink.String()}
}

// checkPayloadFree fails when the diagnostic output contains any of the private values.
func checkPayloadFree(t *testing.T, lines string, private ...string) {
	t.Helper()
	for _, value := range private {
		if value != "" && strings.Contains(lines, value) {
			t.Errorf("diagnostic output %q contains private value %q", lines, value)
		}
	}
}

// checkOneLine fails unless lines is exactly one non-empty, newline-terminated line.
func checkOneLine(t *testing.T, lines string) {
	t.Helper()
	text, found := strings.CutSuffix(lines, "\n")
	if !found || text == "" || strings.Contains(text, "\n") {
		t.Errorf("diagnostic output = %q, want exactly one newline-terminated line", lines)
	}
}

// TestDiagnosticLines checks the fixed diagnostic line written for an attributed OpenAI
// rejection and for an empty Codex completion: exactly one line, the same bytes whatever the
// header values, upstream body, request body and message ID were, and none of them in it.
func TestDiagnosticLines(t *testing.T) {
	type upstreamReply struct {
		header      http.Header
		body        string
		request, id string
	}
	payload := func(tag string, status int) upstreamReply {
		return upstreamReply{
			header:  http.Header{"Content-Type": {"application/json"}, "X-Openai-Request-Id": {"private-header-" + tag}, "Retry-After": {"30"}},
			body:    fmt.Sprintf(`{"error":{"message":"private-body-%s: OpenAI refused status %d"}}`, tag, status),
			request: `{"messages":[{"role":"user","content":"private-prompt-` + tag + `"}]}`,
			id:      "msg_private_" + tag,
		}
	}
	empty := func(tag string, stream bool) upstreamReply {
		return upstreamReply{
			header:  http.Header{"Content-Type": {"application/json"}, "X-Private-Header": {"private-header-" + tag}},
			body:    `{"message":"Codex completed without producing output","detail":"private-body-` + tag + `"}`,
			request: fmt.Sprintf(`{"stream":%t,"messages":[{"role":"user","content":"private-prompt-%s"}]}`, stream, tag),
			id:      "msg_private_" + tag,
		}
	}
	tests := map[string]struct {
		status     int
		first, alt upstreamReply
		wantCode   int
	}{
		"error: attributed OpenAI forbidden":       {http.StatusForbidden, payload("a", 403), payload("b", 403), http.StatusForbidden},
		"error: attributed OpenAI unauthorized":    {http.StatusUnauthorized, payload("a", 401), payload("b", 401), http.StatusUnauthorized},
		"error: attributed OpenAI rate limit":      {http.StatusTooManyRequests, payload("a", 429), payload("b", 429), http.StatusTooManyRequests},
		"success: empty completion answered JSON":  {http.StatusServiceUnavailable, empty("a", false), empty("b", false), http.StatusOK},
		"success: empty completion answered SSE":   {http.StatusServiceUnavailable, empty("a", true), empty("b", true), http.StatusOK},
		"success: empty completion either framing": {http.StatusServiceUnavailable, empty("a", false), empty("b", true), http.StatusOK},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var outputs []string
			for _, up := range []upstreamReply{tc.first, tc.alt} {
				dir := t.TempDir()
				store := &upstreamstate.Store{Dir: dir, Now: func() time.Time { return time.Unix(1000, 0) }}
				req := reply.Request{Provider: "codex", AdvertisedModel: "shown", BackendModel: "engine", Body: []byte(up.request), Upstream: store, NewMessageID: func() string { return up.id }}
				got := diagnose(t, req, tc.status, up.header, up.body)
				equal(t, tc.wantCode, got.code)
				checkOneLine(t, got.lines)
				var private []string
				for _, values := range up.header {
					for _, value := range values {
						if strings.HasPrefix(value, "private-") {
							private = append(private, value)
						}
					}
				}
				checkPayloadFree(t, got.lines, append(private, up.body, up.request, up.id, "private-body-", "private-prompt-", dir)...)
				outputs = append(outputs, got.lines)
			}
			if outputs[0] != outputs[1] {
				t.Errorf("diagnostic line depends on the payload:\n first: %q\nsecond: %q", outputs[0], outputs[1])
			}
		})
	}
}

// TestDiagnosticStateWriteFailure checks that an attributed rejection whose upstream state cannot
// be written reports the failure on the diagnostics writer without the state path, any header
// value or any body bytes, and answers the client exactly as when the write succeeds.
func TestDiagnosticStateWriteFailure(t *testing.T) {
	tests := map[string]struct {
		status int
	}{
		"error: forbidden with unwritable state":    {http.StatusForbidden},
		"error: unauthorized with unwritable state": {http.StatusUnauthorized},
		"error: rate limit with unwritable state":   {http.StatusTooManyRequests},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			header := http.Header{"Content-Type": {"application/json"}, "X-Openai-Request-Id": {"private-header-value"}, "Retry-After": {"30"}}
			body := `{"error":{"message":"private-body-bytes from OpenAI"}}`
			request := `{"messages":[{"role":"user","content":"private-prompt"}]}`
			run := func(dir string) (diagnosed, *upstreamstate.Store) {
				store := &upstreamstate.Store{Dir: dir, Now: func() time.Time { return time.Unix(1000, 0) }}
				req := reply.Request{Provider: "codex", AdvertisedModel: "shown", BackendModel: "engine", Body: []byte(request), Upstream: store}
				return diagnose(t, req, tc.status, header, body), store
			}

			writable := t.TempDir()
			ok, okStore := run(writable)
			if okStore.ReadBlocked() == nil {
				t.Fatal("writable state did not record the attributed rejection")
			}

			// A directory below a regular file cannot be created, whoever runs the test.
			blocker := filepath.Join(t.TempDir(), "state-is-a-file")
			if err := os.WriteFile(blocker, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			unwritable := filepath.Join(blocker, "codex")
			failed, failedStore := run(unwritable)
			if failedStore.ReadBlocked() != nil {
				t.Fatal("unwritable state recorded a rejection")
			}

			equal(t, ok.code, failed.code)
			equal(t, ok.header, failed.header)
			equal(t, ok.body, failed.body)

			okLines := strings.SplitAfter(ok.lines, "\n")
			var added []string
			for line := range strings.SplitAfterSeq(failed.lines, "\n") {
				if line != "" && !slices.Contains(okLines, line) {
					added = append(added, line)
				}
			}
			if len(added) == 0 {
				t.Errorf("failed state write left no diagnostic line of its own: failed=%q succeeded=%q", failed.lines, ok.lines)
			}
			if failed.lines != "" && !strings.HasSuffix(failed.lines, "\n") {
				t.Errorf("diagnostic output %q does not end with a newline", failed.lines)
			}
			for _, lines := range []string{ok.lines, failed.lines} {
				checkPayloadFree(t, lines, "private-header-value", "private-body-bytes", "private-prompt", writable, unwritable, blocker)
			}
		})
	}
}

// TestNoDiagnosticsForOrdinaryReplies checks that replies the source does not report on write
// nothing to the diagnostics writer.
func TestNoDiagnosticsForOrdinaryReplies(t *testing.T) {
	tests := map[string]struct {
		provider string
		status   int
		header   http.Header
		body     string
	}{
		"success: Codex JSON success":             {"codex", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "X-Openai-Request-Id": {"req"}}, `{"type":"message","content":[],"stop_reason":"end_turn"}`},
		"error: unattributed Codex forbidden":     {"codex", http.StatusForbidden, http.Header{"Content-Type": {"application/json"}}, `{"error":{"message":"forbidden"}}`},
		"error: attributed Codex server error":    {"codex", http.StatusInternalServerError, http.Header{"Content-Type": {"application/json"}, "X-Openai-Request-Id": {"req"}}, `{"error":{"message":"OpenAI failed"}}`},
		"error: attributed rejection of other":    {"kimi", http.StatusTooManyRequests, http.Header{"Content-Type": {"application/json"}, "X-Openai-Request-Id": {"req"}}, `{"error":{"message":"OpenAI refused"}}`},
		"error: ordinary Codex unavailable":       {"codex", http.StatusServiceUnavailable, http.Header{"Content-Type": {"application/json"}}, `{"message":"upstream overloaded"}`},
		"error: empty completion of other vendor": {"kimi", http.StatusServiceUnavailable, http.Header{"Content-Type": {"application/json"}}, `{"message":"Codex completed without producing output"}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			store := &upstreamstate.Store{Dir: t.TempDir(), Now: func() time.Time { return time.Unix(1000, 0) }}
			req := reply.Request{Provider: tc.provider, AdvertisedModel: "shown", Body: []byte(`{}`), Upstream: store}
			got := diagnose(t, req, tc.status, tc.header, tc.body)
			equal(t, tc.status, got.code)
			equal(t, "", got.lines)
		})
	}
}

// TestNilDiagnosticsDiscards checks that a reply with no diagnostics writer still answers an
// attributed rejection and an empty completion normally.
func TestNilDiagnosticsDiscards(t *testing.T) {
	tests := map[string]struct {
		status   int
		header   http.Header
		body     string
		wantCode int
	}{
		"error: attributed rejection":  {http.StatusForbidden, http.Header{"X-Openai-Request-Id": {"req"}}, `{"error":{"message":"refused"}}`, http.StatusForbidden},
		"success: empty completion":    {http.StatusServiceUnavailable, nil, `{"message":"Codex completed without producing output"}`, http.StatusOK},
		"error: unwritable state file": {http.StatusUnauthorized, http.Header{"X-Openai-Request-Id": {"req"}}, `{"error":{"message":"refused"}}`, http.StatusUnauthorized},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.status == http.StatusUnauthorized {
				blocker := filepath.Join(dir, "file")
				if err := os.WriteFile(blocker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(blocker, "codex")
			}
			store := &upstreamstate.Store{Dir: dir, Now: func() time.Time { return time.Unix(1000, 0) }}
			req := reply.Request{Provider: "codex", AdvertisedModel: "shown", Body: []byte(`{}`), Upstream: store}
			w, result := handle(t, withDiagnostics(req, nil), newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				maps.Copy(w.Header(), tc.header)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			equal(t, tc.wantCode, w.Code)
			equal(t, tc.wantCode, result.Status)
		})
	}
}
