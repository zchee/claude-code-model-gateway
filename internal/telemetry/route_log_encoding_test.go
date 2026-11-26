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

package telemetry_test

import (
	"bytes"
	json "encoding/json/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
)

// TestRouteLogTextEncoding checks that the route log interprets the raw request-target bytes and
// the fallback session header as UTF-8, where the source reads them as Latin-1: valid non-ASCII
// text is logged as the same characters, and each malformed byte becomes U+FFFD, so every line
// is valid UTF-8 JSON.
func TestRouteLogTextEncoding(t *testing.T) {
	tests := map[string]struct {
		target, session       string
		wantPath, wantSession string
	}{
		"success: valid non-ASCII target and session keep their characters": {
			target:      "/v1/messages/日本語?q=秘密",
			session:     "セッション-1",
			wantPath:    "/v1/messages/日本語",
			wantSession: "セッション-1",
		},
		"success: four-byte characters stay single characters": {
			target:      "/v1/\xf0\x9f\x8e\xb5",
			session:     "s-\xf0\x9f\x8e\xb5",
			wantPath:    "/v1/\U0001F3B5",
			wantSession: "s-\U0001F3B5",
		},
		"error: malformed target and session bytes become replacement characters": {
			target:      "/v1/\xffx\xc3?k=\xfe",
			session:     "s\xfe\xc3",
			wantPath:    "/v1/�x�",
			wantSession: "s��",
		},
		"error: truncated multi-byte sequence before the query is replaced": {
			target:      "/v1/\xe6\x97?x=1",
			session:     "\xe6\x97\xa5\xe6",
			wantPath:    "/v1/��",
			wantSession: "日�",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "logs", "request-routes.jsonl")
			var diag bytes.Buffer
			r := telemetry.NewRecorder(telemetry.Options{
				Lookup:      func(string) (string, bool) { return "", false },
				Now:         func() time.Time { return time.Unix(1, 0) },
				Random:      bytes.NewReader(bytes.Repeat([]byte{0x22}, 256)),
				LogPath:     logPath,
				Diagnostics: &diag,
			})
			// httptest.NewRequest parses the request line with the server's own parser, so the
			// raw target bytes arrive in RequestURI as a server handler would see them.
			req := httptest.NewRequestWithContext(t.Context(), "POST", tt.target, strings.NewReader(`{}`))
			if req.RequestURI != tt.target {
				t.Fatalf("RequestURI = %q, want the raw target %q", req.RequestURI, tt.target)
			}
			req.Header["X-Claude-Code-Session-Id"] = []string{tt.session}
			r.LogRoute(req, telemetry.LogEntry{Backend: "codex"})
			if diag.Len() != 0 {
				t.Fatalf("unexpected diagnostic: %q", diag.String())
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			line, ok := bytes.CutSuffix(raw, []byte("\n"))
			if !ok || bytes.Contains(line, []byte("\n")) {
				t.Fatalf("log = %q, want exactly one newline-terminated line", raw)
			}
			if !utf8.Valid(line) {
				t.Errorf("log line %q is not valid UTF-8", line)
			}
			var row map[string]any
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatalf("log line %q: %v", line, err)
			}
			want := map[string]any{"at": "1970-01-01T00:00:01.000Z", "backend": "codex", "model": nil, "path": tt.wantPath, "sessionId": tt.wantSession}
			if diff := gocmp.Diff(want, row); diff != "" {
				t.Errorf("route log row (-want +got):\n%s", diff)
			}
		})
	}
}
