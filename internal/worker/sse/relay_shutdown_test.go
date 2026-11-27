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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// shutdownRelay runs one relay variant against resp with the given service context and client signal.
type shutdownRelay func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error)

// relayObservation is where a relay variant reports what it observes: a usage spy, and a sentry
// state with the session and target it observes into.
type relayObservation struct {
	usage   *usageSpy
	sentry  *sentry.State
	session string
	target  sentry.Target
}

func shutdownRelays() map[string]shutdownRelay {
	return observedShutdownRelays(nil)
}

// observedShutdownRelays is shutdownRelays reporting to obs. With a nil obs every run gets a usage
// spy of its own and no sentry.
func observedShutdownRelays(obs *relayObservation) map[string]shutdownRelay {
	observation := func() relayObservation {
		if obs == nil {
			return relayObservation{usage: &usageSpy{}}
		}
		return *obs
	}
	return map[string]shutdownRelay{
		"proxy relay": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error) {
			o := observation()
			return sse.RelayProxy(ctx, w, resp, sse.ProxyOptions{
				Provider:          "codex",
				AdvertisedModel:   "claude-gpt-5.6-sol",
				BackendModel:      "gpt-5.6-sol",
				HeartbeatInterval: -1,
				ClientDone:        clientDone,
				Usage:             o.usage,
				Sentry:            o.sentry,
				SessionKey:        o.session,
				SentryTarget:      o.target,
			})
		},
		"LM Studio proxy relay": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error) {
			o := observation()
			return sse.RelayProxy(ctx, w, resp, sse.ProxyOptions{
				Provider:          sse.LMStudioProvider,
				AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
				BackendModel:      "lmstudio/qwen3-coder-30b",
				HeartbeatInterval: -1,
				FirstEventWait:    5 * time.Second,
				ClientDone:        clientDone,
				Usage:             o.usage,
				Sentry:            o.sentry,
				SessionKey:        o.session,
				SentryTarget:      o.target,
				OnOverflow:        testEngineOverflowResponder,
			})
		},
		"Anthropic passthrough": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error) {
			o := observation()
			return sse.RelayPassthrough(ctx, w, resp, sse.PassthroughOptions{
				ClientDone:   clientDone,
				Usage:        o.usage,
				Sentry:       o.sentry,
				SessionKey:   o.session,
				SentryTarget: o.target,
			})
		},
		"proxy passthrough": func(ctx context.Context, w http.ResponseWriter, resp *upstream.Response, clientDone <-chan struct{}) (outcome.Result, error) {
			o := observation()
			return sse.RelayPassthrough(ctx, w, resp, sse.PassthroughOptions{
				ProxyProvider:     "codex",
				HeartbeatInterval: -1,
				ClientDone:        clientDone,
				Usage:             o.usage,
				Sentry:            o.sentry,
				SessionKey:        o.session,
				SentryTarget:      o.target,
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
				// The bubble's fake clock keeps the LM Studio first-event wait from being a real
				// deadline: a relay that waits on it instead of seeing the ended context still returns.
				synctest.Test(t, func(t *testing.T) {
					// The whole stream is readable at once, so a relay that reads before it looks at
					// the context can deliver or decide on data after shutdown. Repeat to catch either
					// order. No read may be requested at all.
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
						reads := src.reads.Load()
						if errOK && reads == 0 && !wroteHeader && rec.attemptCount() == 0 && flushes == 0 {
							continue
						}
						bad++
						if firstBad == "" {
							firstBad = fmt.Sprintf("%s upstream reads=%d", formatRelayOutcome(round, res, err, wroteHeader, status, body), reads)
						}
					}
					if bad != 0 {
						t.Errorf("%d of %d rounds read the body, committed output or returned the wrong error after shutdown; first: %s", bad, rounds, firstBad)
					}
				})
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
			// The read count also catches a read whose result is dropped instead of delivered.
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
				reads := src.reads.Load()
				if !delivered && reads == 1 && len(attempts) == 1 && errors.Is(err, http.ErrAbortHandler) && errors.Is(err, context.Canceled) &&
					res.Status == http.StatusOK && res.StatusOverride == "upstream_aborted" && !res.Cancelled {
					continue
				}
				bad++
				if firstBad == "" {
					firstBad = fmt.Sprintf("%s upstream reads=%d (want 1)", formatRelayOutcome(round, res, err, wroteHeader, status, body), reads)
				}
			}
			if bad != 0 {
				t.Errorf("%d of %d rounds read on after shutdown or did not abort the committed response; first: %s", bad, rounds, firstBad)
			}
		})
	}
}
