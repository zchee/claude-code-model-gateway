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
	"context"
	"net/http"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// Frames without a model field or a cache count, so every relay below forwards their bytes as
// they arrived.
const (
	batchStartFrame      = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-batch\",\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n\n"
	batchBlockStartFrame = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	batchTextFrame       = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"batched\"}}\n\n"
	batchBlockStopFrame  = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	batchDeltaFrame      = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"
	batchStopFrame       = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// batchingRelays are the relays whose per-read output is checked: the proxy relay, which
// transforms frames, and the proxy passthrough, which observes them on the side.
func batchingRelays(usage *usageSpy) map[string]func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response) (outcome.Result, error) {
	return map[string]func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response) (outcome.Result, error){
		"proxy relay": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response) (outcome.Result, error) {
			return sse.RelayProxy(ctx, w, resp, sse.ProxyOptions{
				Provider:          "codex",
				AdvertisedModel:   "claude-gpt-5.6-sol",
				BackendModel:      "gpt-5.6-sol",
				HeartbeatInterval: -1,
				Usage:             usage,
			})
		},
		"proxy passthrough": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response) (outcome.Result, error) {
			return sse.RelayPassthrough(ctx, w, resp, sse.PassthroughOptions{
				ProxyProvider:     "codex",
				HeartbeatInterval: -1,
				Usage:             usage,
			})
		},
	}
}

// TestRelayObservesWholeReadBeforeOneWrite checks that a relay observes every frame of one upstream
// read before it writes anything of that read, and then writes the read's frames to the client in
// a single write. The source observes and emits frame by frame. Each upstream read here carries
// three frames, so the client must see exactly one write per read, made after all three of its
// events were observed.
func TestRelayObservesWholeReadBeforeOneWrite(t *testing.T) {
	reads := []string{
		batchStartFrame + batchBlockStartFrame + batchTextFrame,
		batchBlockStopFrame + batchDeltaFrame + batchStopFrame,
	}
	tests := map[string]struct {
		relay string
	}{
		"success: proxy relay writes each read once after observing all of its frames":       {relay: "proxy relay"},
		"success: proxy passthrough writes each read once after observing all of its frames": {relay: "proxy passthrough"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			usage := &usageSpy{}
			relay := batchingRelays(usage)[tt.relay]
			src := newEOFChunkBody([]byte(reads[0]), []byte(reads[1]))
			rec := newStreamingRecorder()
			// observedAtWrite holds how many events had been observed when each client write ran.
			var observedAtWrite []int
			rec.onWrite = func(string) {
				events, _, _, _ := usage.snapshot(t)
				observedAtWrite = append(observedAtWrite, len(events))
			}

			res, err := relay(t.Context(), rec, sseResponse(src, nil))
			if err != nil {
				t.Errorf("relay error = %v, want nil", err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, res); diff != "" {
				t.Errorf("relay result (-want +got):\n%s", diff)
			}
			_, _, _, writes, _ := rec.snapshot()
			if diff := gocmp.Diff(reads, writes); diff != "" {
				t.Errorf("client writes (-want one write per upstream read +got):\n%s", diff)
			}
			if diff := gocmp.Diff([]int{3, 6}, observedAtWrite); diff != "" {
				t.Errorf("events observed when each client write ran (-want all of the read's frames first +got):\n%s", diff)
			}
		})
	}
}

// TestRelayReadsNothingWhileClientWriteStalls checks the back-pressure that follows from writing
// each read in one client write: while that write is blocked, the relay requests no further
// upstream read, so a stalled client holds the upstream. The source's transformer ignores a write
// that reports a full buffer and keeps consuming the upstream.
func TestRelayReadsNothingWhileClientWriteStalls(t *testing.T) {
	tests := map[string]struct {
		relay string
	}{
		"success: proxy relay reads no further while the first client write is blocked":       {relay: "proxy relay"},
		"success: proxy passthrough reads no further while the first client write is blocked": {relay: "proxy passthrough"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				usage := &usageSpy{}
				relay := batchingRelays(usage)[tt.relay]
				first := batchStartFrame + batchBlockStartFrame + batchTextFrame
				second := batchBlockStopFrame + batchDeltaFrame + batchStopFrame
				src := newEOFChunkBody([]byte(first), []byte(second))
				rec := newStreamingRecorder()
				release := make(chan struct{})
				writeEntered := make(chan struct{})
				blocked := false
				rec.onWrite = func(string) {
					if blocked {
						return
					}
					blocked = true
					close(writeEntered)
					<-release
				}
				type relayOut struct {
					res outcome.Result
					err error
				}
				done := make(chan relayOut, 1)
				go func() {
					res, err := relay(t.Context(), rec, sseResponse(src, nil))
					done <- relayOut{res: res, err: err}
				}()
				released, joined := false, false
				defer func() {
					if !released {
						close(release)
					}
					if !joined {
						<-done
					}
				}()

				<-writeEntered
				synctest.Wait()
				if got := src.reads.Load(); got != 1 {
					t.Errorf("upstream reads while the first client write is blocked = %d, want 1", got)
				}
				if got := rec.attemptCount(); got != 1 {
					t.Errorf("client writes attempted while the first is blocked = %d, want 1", got)
				}

				close(release)
				released = true
				out := <-done
				joined = true
				if out.err != nil {
					t.Errorf("relay error = %v, want nil", out.err)
				}
				if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, out.res); diff != "" {
					t.Errorf("relay result (-want +got):\n%s", diff)
				}
				if got := src.reads.Load(); got != 2 {
					t.Errorf("upstream reads after the write was released = %d, want 2", got)
				}
				_, _, body, _, _ := rec.snapshot()
				if diff := gocmp.Diff(first+second, body); diff != "" {
					t.Errorf("client body (-want +got):\n%s", diff)
				}
			})
		})
	}
}
