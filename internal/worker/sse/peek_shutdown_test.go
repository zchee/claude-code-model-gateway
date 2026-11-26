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
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
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
			// The bubble's fake clock keeps a positive wait from being a real deadline.
			synctest.Test(t, func(t *testing.T) {
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
			// The bubble's fake clock keeps the first-event wait from being a real deadline.
			synctest.Test(t, func(t *testing.T) {
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
			// The bubble's fake clock keeps the first-event wait from being a real deadline.
			synctest.Test(t, func(t *testing.T) {
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
		})
	}
}

// settlingContext is a service context whose every Done call lets every other goroutine of the
// bubble settle before it returns the channel. A read the peek has requested before the call has
// then completed, and its result waits to be received; when that read ended the context, the peek
// is offered the read result and the ended context at the same moment, and which one it takes is
// left to its select. Settling on every call, not only the first, keeps a Done call made before the
// read is requested from using the settle up. A Done channel taken once and kept across reads is
// not seen again, so with such a caller the overlap is left to natural scheduling.
//
// With pastWait set, the first settle that finds the context ended also sleeps past that wait on
// the bubble's fake clock, so a first-event deadline started with it has expired as well: the
// peek is offered the read result, its deadline and the ended context at once.
//
// The context keeps its contract: Done is closed exactly when Err is non-nil. Done must be called
// only from the bubble's root goroutine, with nothing else calling synctest.Wait.
type settlingContext struct {
	context.Context
	pastWait time.Duration
	slept    atomic.Bool
}

func newSettlingContext(parent context.Context) (*settlingContext, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	return &settlingContext{Context: ctx}, cancel
}

// newDeadlineTieContext returns a settlingContext that also lets a first-event wait of wait expire
// once the context has ended.
func newDeadlineTieContext(parent context.Context, wait time.Duration) (*settlingContext, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	return &settlingContext{Context: ctx, pastWait: wait + time.Millisecond}, cancel
}

func (c *settlingContext) Done() <-chan struct{} {
	synctest.Wait()
	if c.pastWait > 0 && c.Err() != nil && c.slept.CompareAndSwap(false, true) {
		time.Sleep(c.pastWait)
	}
	return c.Context.Done()
}

// endingReadBody ends the service context inside its first Read and returns, from that same Read,
// its data and its err. With errBodyAborted it is a read cut short by shutdown that still hands over
// the bytes it had received; with a nil err it is a read that completed as the service ended.
// Later reads fail.
type endingReadBody struct {
	cancel context.CancelFunc
	data   []byte
	err    error
	reads  atomic.Int32
	closes atomic.Int32
}

func (b *endingReadBody) Read(p []byte) (int, error) {
	if b.reads.Add(1) > 1 || b.closes.Load() > 0 {
		return 0, io.ErrClosedPipe
	}
	b.cancel()
	return copy(p, b.data), b.err
}

func (b *endingReadBody) Close() error {
	b.closes.Add(1)
	return nil
}

// failedReadFirstEvents are the first events a failed read hands over after shutdown: whatever they
// are, the shutdown decides the peek.
func failedReadFirstEvents() map[string]string {
	return map[string]string{
		"an ordinary first event": lmStudioOrdinaryStream(19, 0),
		"an overflow first event": lmStudioOverflowFrame(6017, 4096),
	}
}

// failedReadRounds repeats each failed-read scenario: the select in the peek picks the read result
// over the ended context in about half of the rounds, so a peek that looks at the bytes of a failed
// read before the context fails one of these rounds with near certainty.
const failedReadRounds = 64

func TestPeekFirstEventShutdownWinsOverFailedReadData(t *testing.T) {
	for event, frame := range failedReadFirstEvents() {
		t.Run("error: a failed read that carries "+event+" after shutdown returns the context error and nothing to relay", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var bad int
				var firstBad string
				for round := range failedReadRounds {
					ctx, cancel := newSettlingContext(t.Context())
					src := &endingReadBody{cancel: cancel, data: []byte(frame), err: errBodyAborted}

					res, err := sse.PeekFirstEvent(ctx, src, 5*time.Second)
					cancel()
					bodyReturned := res.Body != nil
					if bodyReturned {
						_ = res.Body.Close()
					}
					if got := src.closes.Load(); got != 1 {
						t.Fatalf("round %d of %d: body close count = %d, want 1", round, failedReadRounds, got)
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
					t.Errorf("%d of %d rounds decided on the bytes of a failed read after shutdown; first: %s", bad, failedReadRounds, firstBad)
				}
			})
		})
	}
}

func TestLMStudioPeekShutdownWinsOverFailedReadData(t *testing.T) {
	for event, frame := range failedReadFirstEvents() {
		t.Run("error: a failed read that carries "+event+" after shutdown returns the context error, calls no responder and commits nothing", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var bad int
				var firstBad string
				for round := range failedReadRounds {
					ctx, cancel := newSettlingContext(t.Context())
					src := &endingReadBody{cancel: cancel, data: []byte(frame), err: errBodyAborted}
					rec := newStreamingRecorder()
					var responderCalls int
					responder := func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
						responderCalls++
						return testEngineOverflowResponder(w, upstreamStatus, counts)
					}

					res, err := sse.RelayProxy(ctx, rec, sseResponse(src, nil), sse.ProxyOptions{
						Provider:          sse.LMStudioProvider,
						AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
						BackendModel:      "lmstudio/qwen3-coder-30b",
						HeartbeatInterval: -1,
						FirstEventWait:    5 * time.Second,
						OnOverflow:        responder,
					})
					cancel()
					if got := src.closes.Load(); got != 1 {
						t.Fatalf("round %d of %d: owned resp.Body close count = %d, want 1", round, failedReadRounds, got)
					}
					status, wroteHeader, body, _, flushes := rec.snapshot()
					if errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrAbortHandler) && responderCalls == 0 &&
						!wroteHeader && rec.attemptCount() == 0 && flushes == 0 && len(rec.Header()) == 0 {
						continue
					}
					bad++
					if firstBad == "" {
						firstBad = fmt.Sprintf("%s responder calls=%d header=%v", formatRelayOutcome(round, res, err, wroteHeader, status, body), responderCalls, rec.Header())
					}
				}
				if bad != 0 {
					t.Errorf("%d of %d rounds acted on the bytes of a failed read after shutdown; first: %s", bad, failedReadRounds, firstBad)
				}
			})
		})
	}
}

// expiredWaitFirstEventWait is the first-event wait of the expired-wait tests, on the bubble's fake
// clock.
const expiredWaitFirstEventWait = 5 * time.Second

// expiredWaitRounds repeats each expired-wait scenario. The peek is offered three ready cases at
// once, the read result, its expired wait and the ended service, and the select picks each with
// probability one third per round, so a given case is never picked in these rounds with probability
// (2/3)^64, below 1e-11.
const expiredWaitRounds = 64

// expiredWaitReads are the reads that end the service while the first-event wait runs out: each
// would, with the service still running, give the peek's caller a body or an overflow to act on.
func expiredWaitReads() map[string]struct {
	data string
	err  error
} {
	return map[string]struct {
		data string
		err  error
	}{
		"a failed read that carries part of a frame": {
			data: lmStudioOrdinaryStream(19, 0)[:30],
			err:  errBodyAborted,
		},
		"a failed read that carries an ordinary first event": {
			data: lmStudioOrdinaryStream(19, 0),
			err:  errBodyAborted,
		},
		"a successful read that carries an ordinary first event": {
			data: lmStudioOrdinaryStream(19, 0),
		},
		"a successful read that carries an overflow first event": {
			data: lmStudioOverflowFrame(6017, 4096),
		},
	}
}

func TestPeekFirstEventShutdownWinsOverExpiredWait(t *testing.T) {
	for read, tt := range expiredWaitReads() {
		t.Run("error: "+read+" as the service ends and the wait runs out returns the context error and closes the body once", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var bad int
				var firstBad string
				for round := range expiredWaitRounds {
					ctx, cancel := newDeadlineTieContext(t.Context(), expiredWaitFirstEventWait)
					src := &endingReadBody{cancel: cancel, data: []byte(tt.data), err: tt.err}

					res, err := sse.PeekFirstEvent(ctx, src, expiredWaitFirstEventWait)
					// The close count is taken before a returned Body is closed: the peek itself must
					// have closed the body. A returned Body still owns the reader and is closed here
					// so the round ends with no goroutine left.
					closes := src.closes.Load()
					bodyReturned := res.Body != nil
					if bodyReturned {
						_ = res.Body.Close()
					}
					cancel()
					if errors.Is(err, context.Canceled) && closes == 1 && !bodyReturned && res.Overflow == nil && res.EarlyEndOverride == "" {
						continue
					}
					bad++
					if firstBad == "" {
						firstBad = fmt.Sprintf("%s body closes by the peek=%d", formatPeekOutcome(round, res, bodyReturned, err), closes)
					}
				}
				if bad != 0 {
					t.Errorf("%d of %d rounds did not report the shutdown when the read, the wait and the service ended together; first: %s",
						bad, expiredWaitRounds, firstBad)
				}
			})
		})
	}
}

func TestLMStudioPeekShutdownWinsOverExpiredWait(t *testing.T) {
	for read, tt := range expiredWaitReads() {
		t.Run("error: "+read+" as the service ends and the wait runs out returns the context error, calls no responder and commits nothing", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				want := outcome.Result{Status: http.StatusOK, StatusOverride: "upstream_aborted"}
				var bad int
				var firstBad string
				for round := range expiredWaitRounds {
					ctx, cancel := newDeadlineTieContext(t.Context(), expiredWaitFirstEventWait)
					src := &endingReadBody{cancel: cancel, data: []byte(tt.data), err: tt.err}
					rec := newStreamingRecorder()
					var responderCalls int
					responder := func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
						responderCalls++
						return testEngineOverflowResponder(w, upstreamStatus, counts)
					}

					res, err := sse.RelayProxy(ctx, rec, sseResponse(src, nil), sse.ProxyOptions{
						Provider:          sse.LMStudioProvider,
						AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
						BackendModel:      "lmstudio/qwen3-coder-30b",
						HeartbeatInterval: -1,
						FirstEventWait:    expiredWaitFirstEventWait,
						OnOverflow:        responder,
					})
					cancel()
					if got := src.closes.Load(); got != 1 {
						t.Fatalf("round %d of %d: owned resp.Body close count = %d, want 1", round, expiredWaitRounds, got)
					}
					status, wroteHeader, body, _, flushes := rec.snapshot()
					if errors.Is(err, context.Canceled) && !errors.Is(err, http.ErrAbortHandler) && gocmp.Equal(want, res) && responderCalls == 0 &&
						!wroteHeader && rec.attemptCount() == 0 && flushes == 0 && len(rec.Header()) == 0 {
						continue
					}
					bad++
					if firstBad == "" {
						firstBad = fmt.Sprintf("%s responder calls=%d header=%v", formatRelayOutcome(round, res, err, wroteHeader, status, body), responderCalls, rec.Header())
					}
				}
				if bad != 0 {
					t.Errorf("%d of %d rounds did not end as a service shutdown before headers with result %+v; first: %s", bad, expiredWaitRounds, want, firstBad)
				}
			})
		})
	}
}

// closeEndsServiceBody hands over a complete first event with its first Read and ends the service
// context when it is closed, so the service ends exactly while the peek closes the body of an
// overflow first event it has just recognized.
type closeEndsServiceBody struct {
	cancel context.CancelFunc
	data   []byte
	reads  atomic.Int32
	closes atomic.Int32
}

func (b *closeEndsServiceBody) Read(p []byte) (int, error) {
	if b.reads.Add(1) > 1 || b.closes.Load() > 0 {
		return 0, io.EOF
	}
	return copy(p, b.data), nil
}

func (b *closeEndsServiceBody) Close() error {
	b.closes.Add(1)
	b.cancel()
	return nil
}

func TestLMStudioOverflowShutdownDuringRecognition(t *testing.T) {
	t.Run("error: a service that ends while the peek closes the body of an overflow first event tells the sentry once and gets the context error and no client write", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := &closeEndsServiceBody{cancel: cancel, data: []byte(lmStudioOverflowFrame(6017, 4096))}
			rec := newStreamingRecorder()
			var calls []recordedOverflow
			// The overflow was recognized while the service still ran, so the sentry must still learn
			// of it through exactly one responder call, on a writer that keeps nothing: what the
			// responder writes and flushes may not reach the client, and its result is not returned.
			responder := func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
				calls = append(calls, recordedOverflow{Counts: counts, UpstreamStatus: upstreamStatus})
				res, err := testEngineOverflowResponder(w, upstreamStatus, counts)
				_ = http.NewResponseController(w).Flush()
				return res, err
			}

			res, err := sse.RelayProxy(ctx, rec, sseResponse(src, nil), sse.ProxyOptions{
				Provider:          sse.LMStudioProvider,
				AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
				BackendModel:      "lmstudio/qwen3-coder-30b",
				HeartbeatInterval: -1,
				FirstEventWait:    5 * time.Second,
				OnOverflow:        responder,
			})
			if !errors.Is(err, context.Canceled) || errors.Is(err, http.ErrAbortHandler) {
				t.Errorf("RelayProxy error = %v, want context.Canceled without http.ErrAbortHandler: nothing was committed before the service ended", err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK, StatusOverride: "upstream_aborted"}, res); diff != "" {
				t.Errorf("outcome.Result returned with the context error (-want +got):\n%s", diff)
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", got)
			}
			want := []recordedOverflow{{
				Counts:         sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))},
				UpstreamStatus: http.StatusOK,
			}}
			if diff := gocmp.Diff(want, calls); diff != "" {
				t.Errorf("overflow responder calls (-want +got):\n%s", diff)
			}
			assertNothingWritten(t, rec)
		})
	})
}
