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

package compose_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestMalformedBodyReachesDefaultUpstreamUnchanged decides, through the composed worker, that a
// /v1/messages body the classifier still takes as ordinary reaches the default upstream with its
// original bytes, byte for byte, and no other upstream is contacted. The cases are bodies that do
// not parse, plus a parsed Claude body whose raw bytes the decoder would replace: a branch that
// sent a re-encoded payload, a truncated body or no body at all would change what the upstream
// receives.
func TestMalformedBodyReachesDefaultUpstreamUnchanged(t *testing.T) {
	const prefix = `{"model":"claude-gpt-private-model-marker","note":"private-body-marker","messages":`
	tests := map[string]struct {
		body string
	}{
		"success: a non-string member name at the deepest object goes unchanged": {
			body: prefix + strings.Repeat("[", 9998) + "{[]:0}" + strings.Repeat("]", 9998) + "}",
		},
		"success: a non-string member name at the top level goes unchanged": {
			body: `{1:"claude-gpt-9"}`,
		},
		"success: text that is not JSON goes unchanged": {
			body: " \xffnot JSON\r\n",
		},
		"success: a truncated body goes unchanged": {
			body: `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hel`,
		},
		"success: a Claude body with ill-formed UTF-8 keeps its raw bytes": {
			body: "{\"model\" : \"claude-sonnet-4-6\",\n \"max_tokens\":16,\"note\":\"\xff\xfe\",\"messages\":[{\"role\":\"user\",\"content\":\"\\u0068i\"}]}",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var received [][]byte
			h := newHarness(t, harnessConfig{
				disableTrace: true,
				disableUsage: true,
				anthropicHandler: func(w http.ResponseWriter, r *http.Request) {
					data, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("default upstream could not read the request body: %v", err)
					}
					mu.Lock()
					received = append(received, data)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"msg_default","type":"message","content":[]}`)
				},
			})

			status, _, _ := h.do(http.MethodPost, "/v1/messages", tt.body, nil)
			mustDiff(t, "client status", http.StatusOK, status)
			mustDiff(t, "default upstream sends", int64(1), h.anthropicHits.Load())
			mustDiff(t, "proxy sends", int64(0), h.proxyHits.Load())
			mustDiff(t, "grok sends", int64(0), h.grokHits.Load())

			mu.Lock()
			defer mu.Unlock()
			if len(received) != 1 {
				t.Fatalf("default upstream received %d bodies, want 1", len(received))
			}
			if got := received[0]; !bytes.Equal([]byte(tt.body), got) {
				at := 0
				for at < len(got) && at < len(tt.body) && got[at] == tt.body[at] {
					at++
				}
				t.Fatalf("default upstream received %d bytes, want the original %d; first difference at byte %d", len(got), len(tt.body), at)
			}
		})
	}
}
