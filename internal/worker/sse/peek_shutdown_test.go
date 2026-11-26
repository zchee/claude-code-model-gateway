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
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
)

// errBodyAborted stands in for the error a transport returns from a body read it aborted.
var errBodyAborted = errors.New("upstream body read aborted by the transport")

// shutdownReadBody models a transport that aborts the body read because the service context
// ended: after its lead chunks, its Read ends the context first and only then fails, so the
// shutdown and the read error are both visible when the reader looks at either.
type shutdownReadBody struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	lead   [][]byte
	closes atomic.Int32
}

func (b *shutdownReadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closes.Load() > 0 {
		return 0, io.ErrClosedPipe
	}
	if len(b.lead) > 0 {
		n := copy(p, b.lead[0])
		b.lead = b.lead[1:]
		return n, nil
	}
	b.cancel()
	return 0, errBodyAborted
}

func (b *shutdownReadBody) Close() error {
	b.closes.Add(1)
	return nil
}

func TestPeekFirstEventEndedServiceContext(t *testing.T) {
	ordinary := lmStudioOrdinaryStream(19, 0)

	tests := map[string]struct {
		wait time.Duration
	}{
		"error: context ended before entry with a negative wait closes the body and returns the context error": {
			wait: -1,
		},
		"error: context ended before entry with the default wait closes the body and returns the context error": {
			wait: 0,
		},
		"error: context ended before entry with a positive wait closes the body and returns the context error": {
			wait: 5 * time.Second,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// The body already holds a complete ordinary first event, so a peek that reads before it
			// looks at the context can decide on that event instead of reporting the shutdown. The
			// order of the two is not fixed, so repeat the call to catch either.
			const rounds = 200
			for round := range rounds {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				src := &trackedChunkBody{chunks: [][]byte{[]byte(ordinary)}}

				res, err := sse.PeekFirstEvent(ctx, src, tt.wait)
				bodyReturned := res.Body != nil
				if bodyReturned {
					_ = res.Body.Close()
				}
				if !errors.Is(err, context.Canceled) || bodyReturned || res.Overflow != nil || res.EarlyEndOverride != "" {
					t.Fatalf("round %d of %d: PeekFirstEvent = (Body returned=%t, Overflow=%v, EarlyEndOverride=%q, err=%v), want no Body, no Overflow, no EarlyEndOverride and context.Canceled",
						round, rounds, bodyReturned, res.Overflow, res.EarlyEndOverride, err)
				}
				if got := src.closes.Load(); got != 1 {
					t.Fatalf("round %d of %d: body close count = %d, want 1", round, rounds, got)
				}
				if got := src.overlapReads.Load(); got != 0 {
					t.Fatalf("round %d of %d: %d overlapping Read calls on the body", round, rounds, got)
				}
			}
		})
	}
}

func TestPeekFirstEventShutdownWinsOverReadError(t *testing.T) {
	ordinary := lmStudioOrdinaryStream(19, 0)

	tests := map[string]struct {
		lead []string
	}{
		"error: shutdown that aborts the first read reports the context error, not an early end": {},
		"error: shutdown that aborts the read after a comment frame reports the context error, not an early end": {
			lead: []string{": ping\n\n"},
		},
		"error: shutdown that aborts the read inside the first frame reports the context error, not an early end": {
			lead: []string{ordinary[:30]},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Which of the two the peek notices first depends on scheduling, so repeat the scenario and
			// count the rounds that reported an upstream early end for a service shutdown.
			const rounds = 5000
			var bad int
			var firstBad string
			for round := range rounds {
				ctx, cancel := context.WithCancel(t.Context())
				src := &shutdownReadBody{cancel: cancel}
				for _, chunk := range tt.lead {
					src.lead = append(src.lead, []byte(chunk))
				}

				res, err := sse.PeekFirstEvent(ctx, src, 5*time.Second)
				cancel()
				bodyReturned := res.Body != nil
				if bodyReturned {
					_ = res.Body.Close()
				}
				if got := src.closes.Load(); got != 1 && !bodyReturned {
					t.Fatalf("round %d of %d: body close count = %d, want 1", round, rounds, got)
				}
				if errors.Is(err, context.Canceled) && !bodyReturned && res.Overflow == nil && res.EarlyEndOverride == "" {
					continue
				}
				bad++
				if firstBad == "" {
					firstBad = formatPeekOutcome(round, res, bodyReturned, err)
				}
			}
			if bad != 0 {
				t.Errorf("%d of %d rounds did not report the shutdown; first: %s", bad, rounds, firstBad)
			}
		})
	}
}

func TestLMStudioPeekShutdownWinsOverReadError(t *testing.T) {
	ordinary := lmStudioOrdinaryStream(19, 0)

	tests := map[string]struct {
		lead []string
	}{
		"error: shutdown that aborts the first read returns the context error and writes no early-end 502": {},
		"error: shutdown that aborts the read inside the first frame returns the context error and writes no early-end 502": {
			lead: []string{ordinary[:30]},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			const rounds = 3000
			var bad int
			var firstBad string
			for round := range rounds {
				ctx, cancel := context.WithCancel(t.Context())
				src := &shutdownReadBody{cancel: cancel}
				for _, chunk := range tt.lead {
					src.lead = append(src.lead, []byte(chunk))
				}
				rec := newStreamingRecorder()

				res, err := sse.RelayProxy(ctx, rec, sseResponse(src, nil), sse.ProxyOptions{
					Provider:          sse.LMStudioProvider,
					AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
					BackendModel:      "lmstudio/qwen3-coder-30b",
					HeartbeatInterval: -1,
					FirstEventWait:    5 * time.Second,
					OnOverflow:        testEngineOverflowResponder,
				})
				cancel()
				if got := src.closes.Load(); got != 1 {
					t.Fatalf("round %d of %d: owned resp.Body close count = %d, want 1", round, rounds, got)
				}
				status, wroteHeader, body, _, flushes := rec.snapshot()
				if errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrAbortHandler) && !wroteHeader && rec.attemptCount() == 0 && flushes == 0 {
					continue
				}
				bad++
				if firstBad == "" {
					firstBad = formatRelayOutcome(round, res, err, wroteHeader, status, body)
				}
			}
			if bad != 0 {
				t.Errorf("%d of %d rounds did not end as a service shutdown before headers; first: %s", bad, rounds, firstBad)
			}
		})
	}
}
