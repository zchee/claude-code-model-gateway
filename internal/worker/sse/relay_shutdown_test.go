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
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// shutdownRelay runs one relay variant against resp with the given service context and client signal.
type shutdownRelay func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error)

func shutdownRelays() map[string]shutdownRelay {
	return map[string]shutdownRelay{
		"proxy relay": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error) {
			return sse.RelayProxy(ctx, w, resp, sse.ProxyOptions{
				Provider:          "codex",
				AdvertisedModel:   "claude-gpt-5.6-sol",
				BackendModel:      "gpt-5.6-sol",
				HeartbeatInterval: -1,
				ClientDone:        clientDone,
				Usage:             &usageSpy{},
			})
		},
		"LM Studio proxy relay": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error) {
			return sse.RelayProxy(ctx, w, resp, sse.ProxyOptions{
				Provider:          sse.LMStudioProvider,
				AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
				BackendModel:      "lmstudio/qwen3-coder-30b",
				HeartbeatInterval: -1,
				FirstEventWait:    5 * time.Second,
				ClientDone:        clientDone,
				Usage:             &usageSpy{},
				OnOverflow:        testEngineOverflowResponder,
			})
		},
		"Anthropic passthrough": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error) {
			return sse.RelayPassthrough(ctx, w, resp, sse.PassthroughOptions{
				ClientDone: clientDone,
				Usage:      &usageSpy{},
			})
		},
		"proxy passthrough": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error) {
			return sse.RelayPassthrough(ctx, w, resp, sse.PassthroughOptions{
				ProxyProvider:     "codex",
				HeartbeatInterval: -1,
				ClientDone:        clientDone,
				Usage:             &usageSpy{},
			})
		},
	}
}

func TestRelayEndedServiceContextOnEntry(t *testing.T) {
	ordinary := lmStudioOrdinaryStream(19, 0)

	for relayName, relay := range shutdownRelays() {
		tests := map[string]struct {
			clientGone bool
		}{
			"error: " + relayName + " entered after shutdown with the client connected writes nothing and returns the context error": {},
			"success: " + relayName + " entered after shutdown with the client gone writes nothing and returns nil": {
				clientGone: true,
			},
		}
		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				// The whole stream is readable at once, so a relay that reads before it looks at the
				// context can deliver or decide on data after shutdown. Repeat to catch either order.
				const rounds = 50
				var bad int
				var firstBad string
				for round := range rounds {
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					var clientDone chan struct{}
					if tt.clientGone {
						clientDone = make(chan struct{})
						close(clientDone)
					}
					src := newEOFChunkBody([]byte(ordinary))
					rec := newStreamingRecorder()

					res, err := relay(ctx, rec, sseResponse(src, nil), clientDone)
					if got := src.closes.Load(); got != 1 {
						t.Fatalf("round %d of %d: owned resp.Body close count = %d, want 1", round, rounds, got)
					}
					if res.Cancelled {
						t.Fatalf("round %d of %d: outcome.Cancelled = true, want false", round, rounds)
					}
					status, wroteHeader, body, _, flushes := rec.snapshot()
					wantErr := !tt.clientGone
					errOK := err == nil
					if wantErr {
						errOK = errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrAbortHandler)
					}
					if errOK && !wroteHeader && rec.attemptCount() == 0 && flushes == 0 {
						continue
					}
					bad++
					if firstBad == "" {
						firstBad = formatRelayOutcome(round, res, err, wroteHeader, status, body)
					}
				}
				if bad != 0 {
					t.Errorf("%d of %d rounds committed output or returned the wrong error after shutdown; first: %s", bad, rounds, firstBad)
				}
			})
		}
	}
}

func TestRelayEndedServiceContextBetweenReads(t *testing.T) {
	tests := map[string]struct {
		relay      string
		second     string
		secondMark string
	}{
		"error: proxy relay requests no read after shutdown and aborts the committed response": {
			relay:      "proxy relay",
			second:     departureDeltaFrame + departureStopFrame,
			secondMark: "message_delta",
		},
		"error: Anthropic passthrough requests no read after shutdown and aborts the committed response": {
			relay:      "Anthropic passthrough",
			second:     departureDeltaFrame,
			secondMark: "message_delta",
		},
		"error: proxy passthrough requests no read after shutdown and aborts the committed response": {
			relay:      "proxy passthrough",
			second:     departureDeltaFrame,
			secondMark: "message_delta",
		},
	}

	relays := shutdownRelays()
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			relay := relays[tt.relay]
			// The service context ends while the first chunk is written; the next chunk and the clean
			// end are already readable, so a relay that requests another read can still deliver them.
			const rounds = 2000
			var bad int
			var firstBad string
			for round := range rounds {
				ctx, cancel := context.WithCancel(t.Context())
				src := newEOFChunkBody([]byte(departureStartFrame), []byte(tt.second))
				rec := newStreamingRecorder()
				rec.onWrite = func(string) { cancel() }

				res, err := relay(ctx, rec, sseResponse(src, nil), nil)
				cancel()
				if got := src.closes.Load(); got != 1 {
					t.Fatalf("round %d of %d: owned resp.Body close count = %d, want 1", round, rounds, got)
				}
				status, wroteHeader, body, _, _ := rec.snapshot()
				attempts := rec.attemptedSnapshot()
				delivered := strings.Contains(strings.Join(attempts, ""), tt.secondMark)
				if !delivered && len(attempts) == 1 && errors.Is(err, http.ErrAbortHandler) && errors.Is(err, context.Canceled) &&
					res.Status == http.StatusOK && res.StatusOverride == "upstream_aborted" && !res.Cancelled {
					continue
				}
				bad++
				if firstBad == "" {
					firstBad = formatRelayOutcome(round, res, err, wroteHeader, status, body)
				}
			}
			if bad != 0 {
				t.Errorf("%d of %d rounds read on after shutdown or did not abort the committed response; first: %s", bad, rounds, firstBad)
			}
		})
	}
}
