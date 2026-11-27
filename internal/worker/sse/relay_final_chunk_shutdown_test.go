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
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// errFinalWriteShutdown is the cause the service ends with during the final chunk's write.
var errFinalWriteShutdown = errors.New("service ended during the final chunk's write")

// TestRelayServiceEndsDuringFinalChunkWrite ends the service context while the relay writes the
// last chunk of a body whose final read returned that chunk together with io.EOF. The relay has
// then seen both the shutdown and the clean end of the body; the shutdown must win: the relay
// writes no synthesized message_stop and returns no clean status, and its error carries the cause
// the service ended with. When the client leaves in the same write, the shutdown is still
// reported, with a nil error since no client response is left to abort. The control cases run the
// same streams with the service left alive and show that the harness otherwise completes the turn.
//
// In the cases with a trailing event, the final read also holds an unterminated event after its
// complete frames, which only the relay's end-of-stream handling would emit or observe. Those cases
// report to a usage spy and a sentry state the test reads: after the shutdown neither holds that
// event, and the proxy relay records no completed turn in the sentry. Their controls show the same
// harness seeing the event with a live service. Each case runs in a bubble so the LM Studio
// first-event wait is virtual and cannot hand the read back on a real deadline.
func TestRelayServiceEndsDuringFinalChunkWrite(t *testing.T) {
	// literalMessageStop restates sse.SynthesizedMessageStop, which is also the upstream's own
	// message_stop frame in these streams.
	const literalMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	// abortedOverride restates the relay's label for a response its upstream did not complete.
	const abortedOverride = "upstream_aborted"
	// sentryTrigger is the environment trigger of the sentry target the observed cases report to.
	const sentryTrigger = 100
	const (
		// observedStartFrame opens a turn whose input count reaches the attempt's merged usage but
		// not the sentry, which records only message_delta usage.
		observedStartFrame = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-final\",\"usage\":{\"input_tokens\":140,\"output_tokens\":0}}}\n\n"
		// trailingEvent is an unterminated message_delta whose input count is above the trigger.
		trailingEvent = "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":150}}"
		// trailingMarker appears in the client bytes only when the trailing event's bytes reach it.
		trailingMarker = `"input_tokens":150`
	)
	// completeEvents are the events of observedStartFrame and departureDeltaFrame as observed.
	completeEvents := []string{
		`{"type":"message_start","message":{"id":"msg-final","usage":{"input_tokens":140,"output_tokens":0}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`,
	}
	withTrailing := append(slices.Clone(completeEvents), `{"type":"message_delta","usage":{"input_tokens":150}}`)
	ordinaryReq := []byte(`{"model":"claude-gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	session := sentry.SessionKey(true, "session-final-chunk", nil)
	target, err := sentry.ResolveTarget(policy.NewResolver(policy.ResolverInputs{}), "claude-gpt-5.6-sol", sentryTrigger, 0, 0)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}

	tests := map[string]struct {
		relay string
		// chunks are the upstream reads; the last one arrives together with io.EOF.
		chunks []string
		// trailing, when set, is an unterminated event appended to the last read after its
		// complete frames, and the case reports to the test's usage spy and sentry state.
		trailing string
		// endService ends the service during the client write of the last chunk.
		endService bool
		// clientLeaves also makes the client leave during that write, after its bytes were accepted.
		clientLeaves bool
		// wantStops is the number of message_stop frames the client may be sent: the upstream's own.
		wantStops int
		// wantEvents are the events the usage spy must have observed, in order.
		wantEvents []string
		// wantCrossing is what the sentry's check of the next ordinary request reports; nil means
		// no crossing.
		wantCrossing *sentry.Crossing
		// trailingSent says whether the trailing event's bytes may reach the client.
		trailingSent bool
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
		"error: proxy relay neither emits nor observes an unterminated trailing event when the service ends during the final write": {
			relay:      "proxy relay",
			chunks:     []string{observedStartFrame, departureDeltaFrame},
			trailing:   trailingEvent,
			endService: true,
			wantEvents: completeEvents,
		},
		"error: LM Studio proxy relay neither emits nor observes an unterminated trailing event held by the peek's only read when the service ends during its write": {
			relay:      "LM Studio proxy relay",
			chunks:     []string{observedStartFrame + departureDeltaFrame},
			trailing:   trailingEvent,
			endService: true,
			wantEvents: completeEvents,
		},
		"error: Anthropic passthrough does not observe an unterminated trailing event when the service ends during the final write": {
			relay:        "Anthropic passthrough",
			chunks:       []string{observedStartFrame, departureDeltaFrame},
			trailing:     trailingEvent,
			endService:   true,
			wantEvents:   completeEvents,
			trailingSent: true,
		},
		"error: proxy passthrough does not observe an unterminated trailing event when the service ends during the final write": {
			relay:        "proxy passthrough",
			chunks:       []string{observedStartFrame, departureDeltaFrame},
			trailing:     trailingEvent,
			endService:   true,
			wantEvents:   completeEvents,
			trailingSent: true,
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
		"success: proxy relay with a live service emits and observes the unterminated trailing event": {
			relay:        "proxy relay",
			chunks:       []string{observedStartFrame, departureDeltaFrame},
			trailing:     trailingEvent,
			wantStops:    1,
			wantEvents:   withTrailing,
			wantCrossing: &sentry.Crossing{Usage: 150, Trigger: sentryTrigger},
			trailingSent: true,
		},
		"success: LM Studio proxy relay with a live service emits and observes the unterminated trailing event": {
			relay:        "LM Studio proxy relay",
			chunks:       []string{observedStartFrame + departureDeltaFrame},
			trailing:     trailingEvent,
			wantStops:    1,
			wantEvents:   withTrailing,
			wantCrossing: &sentry.Crossing{Usage: 150, Trigger: sentryTrigger},
			trailingSent: true,
		},
		"success: Anthropic passthrough with a live service observes the unterminated trailing event and never reports to the sentry": {
			relay:        "Anthropic passthrough",
			chunks:       []string{observedStartFrame, departureDeltaFrame},
			trailing:     trailingEvent,
			wantEvents:   withTrailing,
			trailingSent: true,
		},
		"success: proxy passthrough with a live service observes the unterminated trailing event": {
			relay:        "proxy passthrough",
			chunks:       []string{observedStartFrame, departureDeltaFrame},
			trailing:     trailingEvent,
			wantEvents:   withTrailing,
			wantCrossing: &sentry.Crossing{Usage: 150, Trigger: sentryTrigger},
			trailingSent: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				spy := &usageSpy{}
				var state sentry.State
				relays := shutdownRelays()
				if tt.trailing != "" {
					relays = observedShutdownRelays(&relayObservation{usage: spy, sentry: &state, session: session, target: target})
				}
				relay, ok := relays[tt.relay]
				if !ok {
					t.Fatalf("no relay named %q", tt.relay)
				}
				chunks := make([][]byte, len(tt.chunks))
				for i, c := range tt.chunks {
					chunks[i] = []byte(c)
				}
				// last is the final read's complete frames; the trailing event follows them in that read.
				last := tt.chunks[len(tt.chunks)-1]
				chunks[len(chunks)-1] = []byte(last + tt.trailing)
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				src := newEOFChunkBody(chunks...)
				rec := newStreamingRecorder()
				// The final chunk's complete frames reach the client unchanged in every relay here (no
				// model field to rewrite, no cache count to subtract), so its write is recognized by them.
				endedInWrite := false
				clientDone := make(chan struct{})
				if tt.endService {
					rec.onWrite = func(chunk string) {
						if strings.Contains(chunk, last) && src.endDelivered() && !endedInWrite {
							endedInWrite = true
							if tt.clientLeaves {
								close(clientDone)
							}
							cancel(errFinalWriteShutdown)
						}
					}
				}

				start := time.Now()
				res, err := relay(ctx, rec, sseResponse(src, nil), clientDone)

				if elapsed := time.Since(start); elapsed != 0 {
					t.Errorf("relay returned after %v of virtual time, want 0: a timer expired", elapsed)
				}
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
				if tt.trailing != "" {
					if got := strings.Contains(attempts, trailingMarker); got != tt.trailingSent {
						t.Errorf("client bytes hold the trailing event = %t, want %t; client bytes=%q", got, tt.trailingSent, attempts)
					}
					events, _, _, _ := spy.snapshot(t)
					if diff := gocmp.Diff(tt.wantEvents, events); diff != "" {
						t.Errorf("events the usage spy observed (-want +got):\n%s", diff)
					}
					crossing, fired := state.Check(session, target, ordinaryReq)
					var gotCrossing *sentry.Crossing
					if fired {
						gotCrossing = &crossing
					}
					if diff := gocmp.Diff(tt.wantCrossing, gotCrossing); diff != "" {
						t.Errorf("sentry check of the next ordinary request (-want +got):\n%s", diff)
					}
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
				// committed response that carries the service's cause, or a nil error once the client is
				// gone, labelled as an aborted upstream either way.
				switch {
				case tt.clientLeaves && err != nil:
					t.Errorf("relay error = %v, want nil once the client has gone", err)
				case !tt.clientLeaves && (!errors.Is(err, http.ErrAbortHandler) || !errors.Is(err, errFinalWriteShutdown)):
					t.Errorf("relay error = %v, want http.ErrAbortHandler wrapping the service's cause %v", err, errFinalWriteShutdown)
				}
				if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK, StatusOverride: abortedOverride}, res); diff != "" {
					t.Errorf("relay result after the service ended (-want +got):\n%s", diff)
				}
			})
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
