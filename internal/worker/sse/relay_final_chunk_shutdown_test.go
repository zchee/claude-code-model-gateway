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
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
)

// TestRelayServiceEndsDuringFinalChunkWrite ends the service context while the relay writes the
// last chunk of a body whose final read returned that chunk together with io.EOF. The relay has
// then seen both the shutdown and the clean end of the body; the shutdown must win: the relay
// writes no synthesized message_stop and returns no clean status. When the client leaves in the
// same write, the shutdown is still reported, with a nil error since no client response is left to
// abort. The control cases run the same streams with the service left alive and show that the
// harness otherwise completes the turn.
func TestRelayServiceEndsDuringFinalChunkWrite(t *testing.T) {
	// literalMessageStop restates sse.SynthesizedMessageStop, which is also the upstream's own
	// message_stop frame in these streams.
	const literalMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	// abortedOverride restates the relay's label for a response its upstream did not complete.
	const abortedOverride = "upstream_aborted"

	tests := map[string]struct {
		relay string
		// chunks are the upstream reads; the last one arrives together with io.EOF.
		chunks []string
		// endService ends the service during the client write of the last chunk.
		endService bool
		// clientLeaves also makes the client leave during that write, after its bytes were accepted.
		clientLeaves bool
		// wantStops is the number of message_stop frames the client may be sent: the upstream's own.
		wantStops int
	}{
		"error: proxy relay writes no stop for a finished turn when the service ends during the final write": {
			relay:      "proxy relay",
			chunks:     []string{departureStartFrame, departureDeltaFrame},
			endService: true,
		},
		"error: proxy relay returns no clean status when the final chunk carries message_stop and the service ends during its write": {
			relay:      "proxy relay",
			chunks:     []string{departureStartFrame, departureDeltaFrame + departureStopFrame},
			endService: true,
			wantStops:  1,
		},
		"error: LM Studio proxy relay writes no stop when the peek's only read held the whole turn and the service ends during its write": {
			relay:      "LM Studio proxy relay",
			chunks:     []string{departureStartFrame + departureDeltaFrame},
			endService: true,
		},
		"error: Anthropic passthrough returns no clean status when the service ends during the final write": {
			relay:      "Anthropic passthrough",
			chunks:     []string{departureStartFrame, departureDeltaFrame},
			endService: true,
		},
		"error: proxy passthrough returns no clean status when the service ends during the final write": {
			relay:      "proxy passthrough",
			chunks:     []string{departureStartFrame, departureDeltaFrame},
			endService: true,
		},
		"error: proxy relay reports the shutdown with a nil error when the client leaves in the same final write": {
			relay:        "proxy relay",
			chunks:       []string{departureStartFrame, departureDeltaFrame},
			endService:   true,
			clientLeaves: true,
		},
		"error: Anthropic passthrough reports the shutdown with a nil error when the client leaves in the same final write": {
			relay:        "Anthropic passthrough",
			chunks:       []string{departureStartFrame, departureDeltaFrame},
			endService:   true,
			clientLeaves: true,
		},
		"error: proxy passthrough reports the shutdown with a nil error when the client leaves in the same final write": {
			relay:        "proxy passthrough",
			chunks:       []string{departureStartFrame, departureDeltaFrame},
			endService:   true,
			clientLeaves: true,
		},
		"success: proxy relay with a live service synthesizes the stop and returns a clean status": {
			relay:     "proxy relay",
			chunks:    []string{departureStartFrame, departureDeltaFrame},
			wantStops: 1,
		},
		"success: LM Studio proxy relay with a live service synthesizes the stop and returns a clean status": {
			relay:     "LM Studio proxy relay",
			chunks:    []string{departureStartFrame + departureDeltaFrame},
			wantStops: 1,
		},
		"success: Anthropic passthrough with a live service returns a clean status": {
			relay:  "Anthropic passthrough",
			chunks: []string{departureStartFrame, departureDeltaFrame},
		},
		"success: proxy passthrough with a live service returns a clean status": {
			relay:  "proxy passthrough",
			chunks: []string{departureStartFrame, departureDeltaFrame},
		},
	}

	relays := shutdownRelays()
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			relay, ok := relays[tt.relay]
			if !ok {
				t.Fatalf("no relay named %q", tt.relay)
			}
			chunks := make([][]byte, len(tt.chunks))
			for i, c := range tt.chunks {
				chunks[i] = []byte(c)
			}
			last := tt.chunks[len(tt.chunks)-1]
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := newEOFChunkBody(chunks...)
			rec := newStreamingRecorder()
			// The final chunk's bytes reach the client unchanged in every relay here (no model
			// field to rewrite, no cache count to subtract), so its write is recognized by them.
			endedInWrite := false
			clientDone := make(chan struct{})
			if tt.endService {
				rec.onWrite = func(chunk string) {
					if strings.Contains(chunk, last) && src.endDelivered() && !endedInWrite {
						endedInWrite = true
						if tt.clientLeaves {
							close(clientDone)
						}
						cancel()
					}
				}
			}

			res, err := relay(ctx, rec, sseResponse(src, nil), clientDone)

			if tt.endService && !endedInWrite {
				t.Fatalf("the service was not ended during the final chunk's write; the setup is broken: attempted writes=%q", rec.attemptedSnapshot())
			}
			if got := src.remaining(); got != 0 {
				t.Fatalf("%d scripted chunks were never read; the setup is broken", got)
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", got)
			}
			attempts := strings.Join(rec.attemptedSnapshot(), "")
			if got := strings.Count(attempts, literalMessageStop); got != tt.wantStops {
				t.Errorf("message_stop frames sent to the client = %d, want %d (the upstream's own only); client bytes=%q", got, tt.wantStops, attempts)
			}
			if !strings.Contains(attempts, last) {
				t.Errorf("client bytes %q do not hold the final chunk %q", attempts, last)
			}

			if !tt.endService {
				if err != nil {
					t.Errorf("relay error = %v, want nil with a live service", err)
				}
				if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, res); diff != "" {
					t.Errorf("relay result with a live service (-want +got):\n%s", diff)
				}
				return
			}
			// The promised outcome: a service that ended is not reported as a completed response.
			if err == nil && res.StatusOverride == "" {
				t.Errorf("relay returned a clean status after the service ended: result=%+v err=nil", res)
			}
			// The shape a shutdown after commitment has everywhere else in the relays: an abort of the
			// committed response that carries the shutdown cause, or a nil error once the client is
			// gone, labelled as an aborted upstream either way.
			switch {
			case tt.clientLeaves && err != nil:
				t.Errorf("relay error = %v, want nil once the client has gone", err)
			case !tt.clientLeaves && (!errors.Is(err, http.ErrAbortHandler) || !errors.Is(err, context.Canceled)):
				t.Errorf("relay error = %v, want http.ErrAbortHandler wrapping context.Canceled", err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK, StatusOverride: abortedOverride}, res); diff != "" {
				t.Errorf("relay result after the service ended (-want +got):\n%s", diff)
			}
		})
	}
}

// doneHookContext runs onDone each time its Done method is called, before returning the wrapped
// context's channel. A relay calls Done on its own goroutine when it enters its wait, so the hook
// can make several of that wait's cases ready before the wait chooses among them.
type doneHookContext struct {
	context.Context
	onDone func()
}

func (c doneHookContext) Done() <-chan struct{} {
	if c.onDone != nil {
		c.onDone()
	}
	return c.Context.Done()
}

// TestPassthroughServiceEndsWithDiscardedFinalChunk covers the passthrough's chunk discarded after
// the client left. The read in flight when the client leaves returns the last chunk together with
// io.EOF. The service ends at the moment the relay begins waiting for that read, so the read's
// result and the shutdown are both ready when the wait chooses. Whichever it takes, the service
// ended before the relay saw the clean end, so the relay reports the shutdown: upstream_aborted,
// with a nil error since the client is gone. A discarded chunk is no exception to that. The wait's
// choice is random, so the case runs for many rounds.
func TestPassthroughServiceEndsWithDiscardedFinalChunk(t *testing.T) {
	const rounds = 64
	// abortedOverride restates the relay's label for a response its upstream did not complete.
	const abortedOverride = "upstream_aborted"
	tests := map[string]struct {
		relay string
	}{
		"error: Anthropic passthrough reports the shutdown for a discarded final chunk": {relay: "Anthropic passthrough"},
		"error: proxy passthrough reports the shutdown for a discarded final chunk":     {relay: "proxy passthrough"},
	}
	relays := shutdownRelays()
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			relay := relays[tt.relay]
			var bad int
			var firstBad string
			for round := range rounds {
				synctest.Test(t, func(t *testing.T) {
					service, cancel := context.WithCancel(t.Context())
					defer cancel()
					src := newHeldBody()
					rec := newStreamingRecorder()
					clientDone := make(chan struct{})
					fired := false
					ctx := doneHookContext{Context: service, onDone: func() {
						if fired || !closed(clientDone) {
							return
						}
						fired = true
						src.release(departureDeltaFrame, io.EOF)
						// The body's reader now holds the result, ready to hand it over.
						synctest.Wait()
						cancel()
					}}
					type relayOut struct {
						res outcome.Result
						err error
					}
					done := make(chan relayOut, 1)
					go func() {
						res, err := relay(ctx, rec, sseResponse(src, nil), clientDone)
						done <- relayOut{res: res, err: err}
					}()

					src.release(departureStartFrame, nil)
					synctest.Wait()
					if got := rec.attemptCount(); got != 1 {
						cancel()
						_ = src.Close()
						<-done
						t.Fatalf("client write attempts after the first chunk = %d, want 1; the setup is broken", got)
					}
					close(clientDone)
					out := <-done

					if !fired {
						t.Fatal("the service was not ended as the relay waited for the discarded chunk; the setup is broken")
					}
					if got := src.closes.Load(); got != 1 {
						t.Errorf("owned resp.Body close count = %d, want 1", got)
					}
					if got := rec.attemptCount(); got != 1 {
						t.Errorf("client write attempts = %d, want 1: the chunk after the client left is discarded", got)
					}
					if out.err == nil && out.res == (outcome.Result{Status: http.StatusOK, StatusOverride: abortedOverride}) {
						return
					}
					bad++
					if firstBad == "" {
						firstBad = fmt.Sprintf("round %d: result=%+v err=%v", round, out.res, out.err)
					}
				})
			}
			if bad != 0 {
				t.Errorf("%d of %d rounds returned other than {200, %s} with a nil error after the service ended; first: %s", bad, rounds, abortedOverride, firstBad)
			}
		})
	}
}

// closed reports, without blocking, whether done has been closed.
func closed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
