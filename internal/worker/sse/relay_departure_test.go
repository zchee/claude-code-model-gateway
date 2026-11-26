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
	"bytes"
	"context"
	"errors"
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

const (
	departureStartFrame = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-departure\",\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n"
	departureDeltaFrame = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n"
	departureStopFrame  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	departureStartEvent = `{"type":"message_start","message":{"id":"msg-departure","usage":{"input_tokens":9,"output_tokens":0}}}`
	departureDeltaEvent = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`
	departureStopEvent  = `{"type":"message_stop"}`
)

func TestPassthroughFinishesObservationAfterLastChunk(t *testing.T) {
	// The final frame has no blank-line terminator, so only the end-of-stream flush of the observer
	// reports it: a relay that abandons observation instead of finishing it loses that event.
	trailing := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}"
	wantEvents := []string{departureStartEvent, departureDeltaEvent}

	tests := map[string]struct {
		encoding  string
		failWrite bool
	}{
		"success: uncompressed stream is observed to its end when the client leaves while the last chunk is written": {},
		"success: gzip stream is observed to its end when the client leaves while the last chunk is written": {
			encoding: "gzip",
		},
		"success: deflate stream is observed to its end when the client leaves while the last chunk is written": {
			encoding: "deflate",
		},
		"success: br stream is observed to its end when the client leaves while the last chunk is written": {
			encoding: "br",
		},
		"success: uncompressed stream is observed to its end when the write of the last chunk fails": {
			failWrite: true,
		},
		"success: gzip stream is observed to its end when the write of the last chunk fails": {
			encoding:  "gzip",
			failWrite: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var chunks [][]byte
			if tt.encoding == "" {
				chunks = [][]byte{[]byte(departureStartFrame), []byte(trailing)}
			} else {
				wire := compressBytes(t, tt.encoding, []byte(departureStartFrame+trailing))
				half := len(wire) / 2
				chunks = [][]byte{wire[:half], wire[half:]}
			}
			wire := bytes.Join(chunks, nil)
			firstChunk, lastChunk := string(chunks[0]), string(chunks[1])

			header := http.Header{}
			if tt.encoding != "" {
				header.Set("Content-Encoding", tt.encoding)
			}
			src := newEOFChunkBody(chunks...)
			rec := newStreamingRecorder()
			usage := &usageSpy{}
			clientDone := make(chan struct{})
			var leaveOnce sync.Once
			rec.onWrite = func(chunk string) {
				switch {
				case tt.failWrite && chunk == firstChunk:
					rec.setWriteError(errors.New("broken pipe"))
				case !tt.failWrite && chunk == lastChunk:
					// The client has every byte; it leaves before the relay looks at ClientDone again.
					leaveOnce.Do(func() { close(clientDone) })
				}
			}

			res, err := sse.RelayPassthrough(t.Context(), rec, sseResponse(src, header), sse.PassthroughOptions{
				ClientDone: clientDone,
				Usage:      usage,
			})
			if err != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil", err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
			if got := src.closes.Load(); got != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", got)
			}

			wantDelivered := wire
			if tt.failWrite {
				wantDelivered = chunks[0]
			}
			_, _, delivered, _, _ := rec.snapshot()
			if diff := gocmp.Diff(string(wantDelivered), delivered); diff != "" {
				t.Errorf("delivered bytes (-want +got):\n%s", diff)
			}

			events, chunksSeen, totalBytes, overflows := usage.snapshot(t)
			if diff := gocmp.Diff(wantEvents, events); diff != "" {
				t.Errorf("observed events after every chunk was observed and the stream ended cleanly (-want +got):\n%s", diff)
			}
			if overflows != 0 {
				t.Errorf("MarkOverflow calls = %d, want 0", overflows)
			}
			if len(chunksSeen) != 0 {
				t.Errorf("ObserveChunk received %d bytes on an SSE stream, want 0", len(chunksSeen))
			}
			if totalBytes != int64(len(wire)) {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, len(wire))
			}
		})
	}
}

func TestPassthroughAbandonsObservationOfDiscardedChunk(t *testing.T) {
	t.Run("success: a chunk that arrives after the client left is discarded and the frame left pending by the observed chunks is not reported", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// The observed chunk ends with a complete, valid frame that lacks only its blank-line
			// terminator, so only the observer's end-of-stream flush could report it. The discarded
			// chunk would terminate that frame and add another one. Abandoning reports neither; a relay
			// that finishes the observation instead reports the pending frame, and one that observes
			// the discarded chunk reports both.
			pending := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}"
			observed := departureStartFrame + pending
			discardedChunk := "\n\n" + departureStopFrame
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := newHeldBody()
			rec := newStreamingRecorder()
			usage := &usageSpy{}
			clientDone := make(chan struct{})

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayPassthrough(ctx, rec, sseResponse(src, nil), sse.PassthroughOptions{
					ClientDone: clientDone,
					Usage:      usage,
				})
				done <- handlerOutcome{res: res, err: err, bodyCloses: src.closes.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					cancel()
					_ = src.Close()
					<-done
				}
			}()

			src.release(observed, nil)
			synctest.Wait()
			attemptsBefore := rec.attemptCount()
			if attemptsBefore != 1 {
				t.Fatalf("client write attempts after the first chunk = %d, want 1 (writes=%q)", attemptsBefore, rec.attemptedSnapshot())
			}

			close(clientDone)
			synctest.Wait()
			src.release(discardedChunk, io.EOF)
			synctest.Wait()

			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayPassthrough did not return after the in-flight read ended the stream")
			}
			if out.err != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil", out.err)
			}
			if out.bodyCloses != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", out.bodyCloses)
			}
			if got := rec.attemptCount(); got != attemptsBefore {
				t.Errorf("client write attempts after ClientDone = %d, want %d (writes=%q)", got, attemptsBefore, rec.attemptedSnapshot())
			}
			events, _, _, overflows := usage.snapshot(t)
			if diff := gocmp.Diff([]string{departureStartEvent}, events); diff != "" {
				t.Errorf("observed events: the pending frame of the observed chunk must not be flushed and the discarded chunk must not be observed (-want +got):\n%s", diff)
			}
			if overflows != 0 {
				t.Errorf("MarkOverflow calls = %d, want 0 (abandoning is not a decode failure)", overflows)
			}
		})
	})
}

func TestRelayWritesNothingToDepartedClient(t *testing.T) {
	lmStudioOptions := func(clientDone <-chan struct{}, onPeek func(), onOverflow sse.OverflowResponder) sse.ProxyOptions {
		return sse.ProxyOptions{
			Provider:          sse.LMStudioProvider,
			AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
			BackendModel:      "lmstudio/qwen3-coder-30b",
			HeartbeatInterval: -1,
			FirstEventWait:    5 * time.Second,
			ClientDone:        clientDone,
			OnPeek:            onPeek,
			OnOverflow:        onOverflow,
		}
	}

	t.Run("success: proxy relay entered with the client gone writes no header, body or flush and still drains the tail", func(t *testing.T) {
		src := newEOFChunkBody([]byte(departureStartFrame), []byte(departureDeltaFrame+departureStopFrame))
		rec := newStreamingRecorder()
		usage := &usageSpy{}
		clientDone := make(chan struct{})
		close(clientDone)

		res, err := sse.RelayProxy(t.Context(), rec, sseResponse(src, nil), sse.ProxyOptions{
			Provider:          "codex",
			AdvertisedModel:   "claude-gpt-5.6-sol",
			BackendModel:      "gpt-5.6-sol",
			HeartbeatInterval: -1,
			ClientDone:        clientDone,
			Usage:             usage,
		})
		if err != nil {
			t.Fatalf("RelayProxy error = %v, want nil", err)
		}
		if res.Status != http.StatusOK || res.Cancelled {
			t.Errorf("outcome.Result = %+v, want Status 200 and Cancelled false", res)
		}
		if got := src.closes.Load(); got != 1 {
			t.Errorf("owned resp.Body close count = %d, want 1", got)
		}
		assertNothingWritten(t, rec)
		events, _, totalBytes, _ := usage.snapshot(t)
		if diff := gocmp.Diff([]string{departureStartEvent, departureDeltaEvent, departureStopEvent}, events); diff != "" {
			t.Errorf("observed events while draining for a departed client (-want +got):\n%s", diff)
		}
		if want := int64(len(departureStartFrame + departureDeltaFrame + departureStopFrame)); totalBytes != want {
			t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, want)
		}
	})

	passthroughTests := map[string]struct {
		proxyProvider string
	}{
		"success: Anthropic passthrough entered with the client gone writes nothing and returns nil at shutdown": {},
		"success: proxy passthrough entered with the client gone writes no header and no heartbeat and returns nil at shutdown": {
			proxyProvider: "codex",
		},
	}
	for name, tt := range passthroughTests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				src := newHeldBody()
				rec := newStreamingRecorder()
				clientDone := make(chan struct{})
				close(clientDone)

				done := make(chan handlerOutcome, 1)
				go func() {
					res, err := sse.RelayPassthrough(ctx, rec, sseResponse(src, nil), sse.PassthroughOptions{
						ProxyProvider: tt.proxyProvider,
						ClientDone:    clientDone,
						Usage:         &usageSpy{},
					})
					done <- handlerOutcome{res: res, err: err, bodyCloses: src.closes.Load()}
				}()
				joined := false
				defer func() {
					if !joined {
						cancel()
						_ = src.Close()
						<-done
					}
				}()

				// Two default heartbeat intervals pass without upstream data. The relay must still be
				// waiting for the end of the service, with the body neither read nor closed: the
				// provider is held by back-pressure, not drained or dropped.
				time.Sleep(2*sse.DefaultHeartbeatInterval + time.Second)
				synctest.Wait()
				select {
				case out := <-done:
					joined = true
					t.Fatalf("RelayPassthrough returned before the service context ended: result=%+v err=%v body closes=%d", out.res, out.err, out.bodyCloses)
				default:
				}
				if got := src.reads.Load(); got != 0 {
					t.Errorf("upstream body reads before shutdown = %d, want 0 for a client gone on entry", got)
				}
				if got := src.closes.Load(); got != 0 {
					t.Errorf("upstream body closes before shutdown = %d, want 0 while the relay waits", got)
				}
				assertNothingWritten(t, rec)

				cancel()
				synctest.Wait()
				var out handlerOutcome
				select {
				case out = <-done:
					joined = true
				default:
					t.Fatal("RelayPassthrough did not return after the service context ended")
				}
				if out.err != nil {
					t.Errorf("RelayPassthrough error = %v, want nil once the client is gone", out.err)
				}
				if out.bodyCloses != 1 {
					t.Errorf("owned resp.Body close count = %d, want 1", out.bodyCloses)
				}
				if out.res.Cancelled {
					t.Errorf("outcome.Cancelled = true, want false")
				}
				assertNothingWritten(t, rec)
			})
		})
	}

	t.Run("success: LM Studio peek interrupted by client departure and then service shutdown returns nil and writes nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := newHeldBody()
			rec := newStreamingRecorder()
			clientDone := make(chan struct{})
			var peeks atomic.Int32

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(ctx, rec, sseResponse(src, nil), lmStudioOptions(clientDone, func() { peeks.Add(1) }, testEngineOverflowResponder))
				done <- handlerOutcome{res: res, err: err, bodyCloses: src.closes.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					cancel()
					_ = src.Close()
					<-done
				}
			}()

			synctest.Wait()
			if got := peeks.Load(); got != 1 {
				t.Fatalf("OnPeek calls = %d, want 1 before the client leaves", got)
			}
			close(clientDone)
			synctest.Wait()
			cancel()
			synctest.Wait()

			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after the service context ended")
			}
			if out.err != nil {
				t.Errorf("RelayProxy error = %v, want nil when the context ends after the client left", out.err)
			}
			if out.bodyCloses != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", out.bodyCloses)
			}
			assertNothingWritten(t, rec)
		})
	})

	t.Run("success: LM Studio early end after the client left writes no 502 and reports the aborted upstream", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := newHeldBody()
			rec := newStreamingRecorder()
			clientDone := make(chan struct{})
			var peeks atomic.Int32

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(ctx, rec, sseResponse(src, nil), lmStudioOptions(clientDone, func() { peeks.Add(1) }, testEngineOverflowResponder))
				done <- handlerOutcome{res: res, err: err, bodyCloses: src.closes.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					cancel()
					_ = src.Close()
					<-done
				}
			}()

			synctest.Wait()
			if got := peeks.Load(); got != 1 {
				t.Fatalf("OnPeek calls = %d, want 1 before the client leaves", got)
			}
			close(clientDone)
			synctest.Wait()
			src.release("", errors.New("connection reset by peer"))
			synctest.Wait()

			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after the upstream read failed")
			}
			if out.err != nil {
				t.Errorf("RelayProxy error = %v, want nil once the client is gone", out.err)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK, StatusOverride: "upstream_aborted"}, out.res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
			if out.bodyCloses != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", out.bodyCloses)
			}
			assertNothingWritten(t, rec)
		})
	})

	t.Run("success: LM Studio overflow after the client left calls the overflow responder once and nothing reaches the client", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := newHeldBody()
			rec := newStreamingRecorder()
			clientDone := make(chan struct{})
			var peeks atomic.Int32
			var mu sync.Mutex
			var overflows []recordedOverflow
			var responderResult outcome.Result
			responder := func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
				res, err := testEngineOverflowResponder(w, upstreamStatus, counts)
				mu.Lock()
				overflows = append(overflows, recordedOverflow{Counts: counts, UpstreamStatus: upstreamStatus})
				responderResult = res
				mu.Unlock()
				return res, err
			}

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(ctx, rec, sseResponse(src, nil), lmStudioOptions(clientDone, func() { peeks.Add(1) }, responder))
				done <- handlerOutcome{res: res, err: err, bodyCloses: src.closes.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					cancel()
					_ = src.Close()
					<-done
				}
			}()

			synctest.Wait()
			if got := peeks.Load(); got != 1 {
				t.Fatalf("OnPeek calls = %d, want 1 before the client leaves", got)
			}
			close(clientDone)
			synctest.Wait()
			src.release(lmStudioOverflowFrame(6017, 4096), nil)
			synctest.Wait()

			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after the overflow first event")
			}
			if out.err != nil {
				t.Errorf("RelayProxy error = %v, want the responder's nil error", out.err)
			}
			if out.bodyCloses != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", out.bodyCloses)
			}
			mu.Lock()
			gotOverflows := overflows
			wantResult := responderResult
			mu.Unlock()
			want := []recordedOverflow{{Counts: sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))}, UpstreamStatus: http.StatusOK}}
			if diff := gocmp.Diff(want, gotOverflows); diff != "" {
				t.Fatalf("overflow responder calls with the client gone (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(wantResult, out.res); diff != "" {
				t.Errorf("outcome.Result is not the responder's result (-want +got):\n%s", diff)
			}
			// The responder still runs so the sentry latches, but what it writes is discarded: the
			// relay had not touched the client's header before the peek, so any header field, status,
			// body byte or flush on the real writer came from the responder.
			assertNothingWritten(t, rec)
		})
	})

	t.Run("success: proxy relay does not flush after the client leaves during a write", func(t *testing.T) {
		src := newEOFChunkBody([]byte(departureStartFrame), []byte(departureDeltaFrame+departureStopFrame))
		rec := newStreamingRecorder()
		clientDone := make(chan struct{})
		var leaveOnce sync.Once
		flushesAtLeave := -1
		rec.onWrite = func(string) {
			leaveOnce.Do(func() {
				_, _, _, _, flushesAtLeave = rec.snapshot()
				close(clientDone)
			})
		}

		res, err := sse.RelayProxy(t.Context(), rec, sseResponse(src, nil), sse.ProxyOptions{
			Provider:          "codex",
			AdvertisedModel:   "claude-gpt-5.6-sol",
			BackendModel:      "gpt-5.6-sol",
			HeartbeatInterval: -1,
			ClientDone:        clientDone,
			Usage:             &usageSpy{},
		})
		if err != nil {
			t.Fatalf("RelayProxy error = %v, want nil", err)
		}
		if res.Status != http.StatusOK {
			t.Errorf("outcome.Status = %d, want 200", res.Status)
		}
		if got := src.closes.Load(); got != 1 {
			t.Errorf("owned resp.Body close count = %d, want 1", got)
		}
		if flushesAtLeave < 0 {
			t.Fatal("the relay never wrote the first frame")
		}
		_, _, _, _, flushes := rec.snapshot()
		if flushes != flushesAtLeave {
			t.Errorf("flushes = %d, want %d: the relay flushed after ClientDone closed", flushes, flushesAtLeave)
		}
		if got := rec.attemptCount(); got != 1 {
			t.Errorf("client write attempts = %d, want 1 (writes=%q)", got, rec.attemptedSnapshot())
		}
	})

	t.Run("success: passthrough does not flush after the client leaves during a write", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			src := newEOFChunkBody([]byte(departureStartFrame), []byte(departureDeltaFrame))
			rec := newStreamingRecorder()
			clientDone := make(chan struct{})
			var leaveOnce sync.Once
			flushesAtLeave := -1
			rec.onWrite = func(string) {
				leaveOnce.Do(func() {
					_, _, _, _, flushesAtLeave = rec.snapshot()
					close(clientDone)
				})
			}

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayPassthrough(ctx, rec, sseResponse(src, nil), sse.PassthroughOptions{
					ClientDone: clientDone,
					Usage:      &usageSpy{},
				})
				done <- handlerOutcome{res: res, err: err, bodyCloses: src.closes.Load()}
			}()

			// The relay either ends on the clean end of the body or holds back-pressure until shutdown.
			synctest.Wait()
			cancel()
			out := <-done
			if out.err != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil", out.err)
			}
			if out.bodyCloses != 1 {
				t.Errorf("owned resp.Body close count = %d, want 1", out.bodyCloses)
			}
			if flushesAtLeave < 0 {
				t.Fatal("the relay never wrote the first chunk")
			}
			_, _, _, _, flushes := rec.snapshot()
			if flushes != flushesAtLeave {
				t.Errorf("flushes = %d, want %d: the relay flushed after ClientDone closed", flushes, flushesAtLeave)
			}
			if got := rec.attemptCount(); got != 1 {
				t.Errorf("client write attempts = %d, want 1 (writes=%q)", got, rec.attemptedSnapshot())
			}
		})
	})
}
