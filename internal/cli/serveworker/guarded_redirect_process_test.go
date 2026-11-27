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

//go:build process && (darwin || linux)

package serveworker_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

const (
	workerRedirectLocation = "https://relay.example.test/v1/messages?moved=1"
	workerRedirectBody     = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_moved\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\"}}\n\n"
	workerCompactionBody   = `{"model":"claude-gpt-6.1-sol[1m]","max_tokens":1,"stream":true,"system":[{"type":"text","text":"You are a helpful AI assistant tasked with summarizing conversations."}],"messages":[{"role":"user","content":"summarize"}]}`
)

// workerRedirectAnswer is the client's view of one worker answer.
type workerRedirectAnswer struct {
	Status   int
	Location string
	Fixture  string
	Body     string
	ReadErr  string
	Contacts int64
}

// TestGuardedRedirectThroughWorker decides, through the worker command, what a compaction request
// makes of a proxy upstream that answers with a redirect whose body looks like an incomplete event
// stream: with the stream guard at its default the client gets the status, the Location header and
// the body bytes unchanged after one upstream contact, exactly as with the stream guard turned off.
func TestGuardedRedirectThroughWorker(t *testing.T) {
	tests := map[string]struct{ guardOff bool }{
		"success: the stream guard at its default relays the redirect once": {},
		"success: the stream guard turned off relays the redirect once":     {guardOff: true},
	}
	want := workerRedirectAnswer{Status: http.StatusFound, Location: workerRedirectLocation, Fixture: "kept", Body: workerRedirectBody, Contacts: 1}
	answers := make(map[bool]workerRedirectAnswer, len(tests))
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			home, env, _ := sandbox(t)
			if tt.guardOff {
				env["CODEX_GATEWAY_COMPACT_STREAM_GUARD"] = "0"
			}
			// A retry, if one happened, would follow at once.
			env["CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS"] = "0"
			var contacts atomic.Int64
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, codexListing)
			})
			mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
				contacts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", workerRedirectLocation)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Redirect-Fixture", "kept")
				w.WriteHeader(http.StatusFound)
				_, _ = io.WriteString(w, workerRedirectBody)
			})
			env["CODEX_GATEWAY_PROXY_PORT"] = portOf(t, env.StartUpstream(t, mux))
			var stdout, stderr lockedSink
			w := startWorker(t, home, env, &stdout, &stderr)
			pollHealth(t, w.public, "a proxy catalog", func(doc map[string]any) bool { return doc["catalog"] == "proxy" })

			ctx, cancel := context.WithTimeout(t.Context(), failureBound)
			defer cancel()
			tr := &http.Transport{DisableKeepAlives: true}
			defer tr.CloseIdleConnections()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+w.public+"/v1/messages", strings.NewReader(workerCompactionBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			var got workerRedirectAnswer
			if resp, err := tr.RoundTrip(req); err != nil {
				got.ReadErr = "no response: " + err.Error()
			} else {
				raw, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				got.Status, got.Location, got.Fixture, got.Body = resp.StatusCode, resp.Header.Get("Location"), resp.Header.Get("X-Redirect-Fixture"), string(raw)
				if readErr != nil {
					got.ReadErr = readErr.Error()
				}
			}
			got.Contacts = contacts.Load()
			answers[tt.guardOff] = got
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("answer to a compaction request whose upstream redirected (-want +got):\n%s", diff)
			}
			stopReturns(t, w, "a redirect had been relayed")
		})
	}
	if diff := gocmp.Diff(answers[true], answers[false]); diff != "" {
		t.Errorf("answer with the stream guard at its default against turned off (-off +default):\n%s", diff)
	}
}
