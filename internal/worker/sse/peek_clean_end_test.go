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

package sse_test

import (
	"net/http"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

// TestLMStudioCleanEndBeforeFirstFrame checks what an eligible LM Studio stream gets when its body
// ends cleanly before any complete frame. The first-event peek hands the clean end back, so the
// relay commits a 200 stream holding exactly the bytes that arrived and reports the unfinished turn
// as an upstream error. It never answers with the early-end 502 that a broken read before the
// first event gets, which the source can also send for a stream that ends before its hand-off.
func TestLMStudioCleanEndBeforeFirstFrame(t *testing.T) {
	// upstreamErrorOverride restates the relay's label for a stream that ended without a
	// deliverable turn.
	const upstreamErrorOverride = "upstream_error"
	tests := map[string]struct {
		chunks []string
	}{
		"error: empty body commits an empty 200 stream": {},
		"error: body ending inside its first frame commits a 200 stream with the partial bytes": {
			chunks: []string{"event: message_start\ndata: {\"type\":\"mess"},
		},
		"error: comment frame and a partial data frame commit a 200 stream with both": {
			chunks: []string{": keepalive\n\n", "event: message_start\ndata: {\"type\""},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			chunks := make([][]byte, len(tt.chunks))
			var want string
			for i, c := range tt.chunks {
				chunks[i] = []byte(c)
				want += c
			}
			src := newEOFChunkBody(chunks...)
			rec := newStreamingRecorder()

			res, err := sse.RelayProxy(t.Context(), rec, sseResponse(src, nil), sse.ProxyOptions{
				Provider:          sse.LMStudioProvider,
				AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
				BackendModel:      "lmstudio/qwen3-coder-30b",
				HeartbeatInterval: -1,
				FirstEventWait:    time.Hour,
				OnOverflow:        testEngineOverflowResponder,
			})
			if err != nil {
				t.Errorf("RelayProxy error = %v, want nil", err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK, StatusOverride: upstreamErrorOverride}, res); diff != "" {
				t.Errorf("RelayProxy result (-want +got):\n%s", diff)
			}
			status, wroteHeader, body, _, _ := rec.snapshot()
			if !wroteHeader || status != http.StatusOK {
				t.Errorf("client head wroteHeader=%t status=%d, want a 200 head", wroteHeader, status)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Errorf("client Content-Type = %q, want the upstream's text/event-stream", got)
			}
			if diff := gocmp.Diff(want, body); diff != "" {
				t.Errorf("client body (-want the bytes that arrived +got):\n%s", diff)
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", got)
			}
		})
	}
}
