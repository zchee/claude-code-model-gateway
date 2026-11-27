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
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
)

// sentryOffRefusal is the advertised proxy's own 413, which a disabled sentry relays untouched.
const sentryOffRefusal = `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`

// TestSentryOffOverflowRouteSpan decides that an advertised proxy's 413 with the sentry disabled
// finishes exactly one route span through the composed worker: the 413 status for a clean reply,
// and the same status with the aborted override when the reply body breaks before its declared end.
// A clean 413 is not taken as a Codex outage.
func TestSentryOffOverflowRouteSpan(t *testing.T) {
	tests := map[string]struct {
		broken     bool
		wantStatus string
	}{
		"success: a clean 413 exports one client_error span": {
			wantStatus: "client_error",
		},
		"success: a 413 whose body ends before its declared length exports one upstream_aborted span": {
			broken:     true,
			wantStatus: "upstream_aborted",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// The route finalizer is observed so that waiting for the span export is ordered after
			// the span was finished: a dropped connection carries no write that would order it.
			var finals atomic.Int32
			finalized := make(chan struct{})
			h := newHarness(t, harnessConfig{
				sentryDisabled: true,
				observeOptions: func(o *worker.Options) {
					start := o.BeforeBody
					o.BeforeBody = func(r *http.Request) func(outcome.Result) {
						finish := start(r)
						return func(res outcome.Result) {
							finish(res)
							if finals.Add(1) == 1 {
								close(finalized)
							}
						}
					}
				},
				proxyHandler: func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					if tt.broken {
						// One byte more than is sent: net/http closes the connection after the
						// handler, so the gateway's read ends early with io.ErrUnexpectedEOF.
						w.Header().Set("Content-Length", strconv.Itoa(len(sentryOffRefusal)+1))
					}
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					_, _ = io.WriteString(w, sentryOffRefusal)
				},
			})

			got := h.request(http.MethodPost, "/v1/messages", messageRequestJSON("claude-gpt-6.1-sol[1m]", false), traced(traceOne))
			if tt.broken {
				// The gateway drops the client connection instead of answering a broken body.
				if got.err == nil {
					t.Fatalf("client got status %d for a broken upstream body, want a dropped connection", got.status)
				}
			} else {
				if got.err != nil {
					t.Fatalf("POST failed: %T", got.err)
				}
				mustDiff(t, "client status", http.StatusRequestEntityTooLarge, got.status)
				errObj, _ := decodeJSONObject(t, got.body)["error"].(map[string]any)
				mustDiff(t, "relayed error.type", any("request_too_large"), errObj["type"])
				mustDiff(t, "relayed error.message without overflow diagnostics", any("Request exceeds the maximum size"), errObj["message"])
			}
			mustDiff(t, "upstream sends", int64(1), h.proxyHits.Load())

			waitSignal(t, "the route finalizer", finalized, 5*time.Second)
			h.telemetryRec.Wait()
			mustDiff(t, "route finalizations", int32(1), finals.Load())
			spans := decodeOTLPSpans(t, h.traceCollector.snapshot())
			mustDiff(t, "exported route spans", 1, len(spans))
			assertSpan(t, spans, traceOne, tt.wantStatus, http.StatusRequestEntityTooLarge)
			if !tt.broken {
				mustDiff(t, "Codex unavailable state after a clean 413", (*upstreamstate.Unavailable)(nil), h.upstreamState.ReadUnavailable())
			}
		})
	}
}
