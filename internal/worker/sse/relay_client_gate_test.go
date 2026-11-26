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

// departure says when a departingWriter raises its client-done signal.
type departure int

const (
	// departNever keeps the client connected.
	departNever departure = iota
	// departBeforeEntry has the client gone before the relay starts.
	departBeforeEntry
	// departOnHeader raises the signal from the first Header call, after the map is returned.
	departOnHeader
	// departOnWriteHeader raises the signal from the first WriteHeader call, after the status is
	// recorded.
	departOnWriteHeader
)

// departingWriter is a client writer that raises the client-done signal from inside its own
// operations, the way a client that disconnects while its head is being written looks to the relay.
// It counts WriteHeader calls itself: the recorder it wraps marks a header as written on any Write
// or Flush, so the recorder's write attempts and flushes are what show output after the departure.
type departingWriter struct {
	*streamingRecorder
	done         chan struct{}
	when         departure
	leaveOnce    sync.Once
	writeHeaders atomic.Int32
}

func newDepartingWriter(when departure) *departingWriter {
	w := &departingWriter{streamingRecorder: newStreamingRecorder(), done: make(chan struct{}), when: when}
	if when == departBeforeEntry {
		w.leave()
	}
	return w
}

func (w *departingWriter) leave() { w.leaveOnce.Do(func() { close(w.done) }) }

func (w *departingWriter) Header() http.Header {
	h := w.streamingRecorder.Header()
	if w.when == departOnHeader {
		w.leave()
	}
	return h
}

func (w *departingWriter) WriteHeader(code int) {
	w.writeHeaders.Add(1)
	w.streamingRecorder.WriteHeader(code)
	if w.when == departOnWriteHeader {
		w.leave()
	}
}

// assertNoOutputAfterHead fails when a body write or a flush reached the writer, or when the
// number of WriteHeader calls is not want.
func assertNoOutputAfterHead(t *testing.T, w *departingWriter, want int32) {
	t.Helper()
	_, _, _, _, flushes := w.snapshot()
	if got := w.writeHeaders.Load(); got != want {
		t.Errorf("client WriteHeader calls = %d, want %d", got, want)
	}
	if attempts := w.attemptedSnapshot(); len(attempts) != 0 || flushes != 0 {
		t.Errorf("client output after it left: flushes=%d attempted writes=%q, want none", flushes, attempts)
	}
}

func TestRelayHeadGatedOnClientDeparture(t *testing.T) {
	tests := map[string]struct {
		relay            string
		when             departure
		wantWriteHeaders int32
		drains           bool
	}{
		"success: proxy relay whose client leaves at WriteHeader of the head gets no body write and no flush": {
			relay:            "proxy relay",
			when:             departOnWriteHeader,
			wantWriteHeaders: 1,
			drains:           true,
		},
		"success: proxy relay whose client leaves while the head is copied gets no WriteHeader, body write or flush": {
			relay:  "proxy relay",
			when:   departOnHeader,
			drains: true,
		},
		"success: LM Studio proxy relay whose client leaves at WriteHeader of the head after the peek gets no body write and no flush": {
			relay:            "LM Studio proxy relay",
			when:             departOnWriteHeader,
			wantWriteHeaders: 1,
			drains:           true,
		},
		"success: LM Studio proxy relay whose client leaves while the head is copied after the peek gets no WriteHeader, body write or flush": {
			relay:  "LM Studio proxy relay",
			when:   departOnHeader,
			drains: true,
		},
		"success: Anthropic passthrough whose client leaves at WriteHeader of the head gets no body write and no flush": {
			relay:            "Anthropic passthrough",
			when:             departOnWriteHeader,
			wantWriteHeaders: 1,
		},
		"success: Anthropic passthrough whose client leaves while the head is copied gets no WriteHeader, body write or flush": {
			relay: "Anthropic passthrough",
			when:  departOnHeader,
		},
		"success: proxy passthrough whose client leaves at WriteHeader of the head gets no body write and no flush": {
			relay:            "proxy passthrough",
			when:             departOnWriteHeader,
			wantWriteHeaders: 1,
		},
		"success: proxy passthrough whose client leaves while the head is copied gets no WriteHeader, body write or flush": {
			relay: "proxy passthrough",
			when:  departOnHeader,
		},
	}

	relays := shutdownRelays()
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				relay := relays[tt.relay]
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				src := newEOFChunkBody([]byte(departureStartFrame), []byte(departureDeltaFrame+departureStopFrame))
				w := newDepartingWriter(tt.when)

				done := make(chan handlerOutcome, 1)
				go func() {
					res, err := relay(ctx, w, sseResponse(src, nil), w.done)
					done <- handlerOutcome{res: res, err: err, bodyCloses: src.closes.Load()}
				}()

				// A proxy relay drains the finite tail for a client that has gone and returns on its
				// own; a passthrough holds the provider by back-pressure until the service ends.
				synctest.Wait()
				var out handlerOutcome
				if tt.drains {
					select {
					case out = <-done:
					default:
						t.Errorf("%s did not return after draining the tail for a departed client", tt.relay)
						cancel()
						out = <-done
					}
					// Draining means reading the body to its end: no scripted chunk is left and the
					// clean end was delivered. The number of reads is not part of that, since a read
					// may return only part of a chunk.
					if left, ended := src.remaining(), src.endDelivered(); left != 0 || !ended {
						t.Errorf("after the drain: %d chunks left, end of the body delivered = %t, want no chunk left and the end delivered", left, ended)
					}
				} else {
					select {
					case out = <-done:
						t.Errorf("%s returned (%+v, %v) before the service ended, want it to wait with the provider held by back-pressure", tt.relay, out.res, out.err)
					default:
						if reads, closes := src.reads.Load(), src.closes.Load(); reads != 0 || closes != 0 {
							t.Errorf("before the service ended: upstream reads = %d, body closes = %d, want no read and no close", reads, closes)
						}
						cancel()
						out = <-done
					}
				}

				if out.err != nil {
					t.Errorf("relay error = %v, want nil once the client is gone", out.err)
				}
				if out.bodyCloses != 1 {
					t.Errorf("owned resp.Body close count = %d, want 1", out.bodyCloses)
				}
				assertNoOutputAfterHead(t, w, tt.wantWriteHeaders)
				if status, wroteHeader, _, _, _ := w.snapshot(); tt.wantWriteHeaders == 0 && wroteHeader {
					t.Errorf("client saw status %d although it left before WriteHeader", status)
				}
			})
		})
	}
}

func TestLMStudioEarlyEndGatedOnClientDeparture(t *testing.T) {
	tests := map[string]struct {
		when             departure
		wantWriteHeaders int32
	}{
		"success: client that leaves at WriteHeader of the early-end 502 gets no early-end body": {
			when:             departOnWriteHeader,
			wantWriteHeaders: 1,
		},
		"success: client that leaves while the early-end head is set gets no WriteHeader and no early-end body": {
			when: departOnHeader,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				src := &trackedChunkBody{trailingErr: errors.New("connection reset by peer")}
				w := newDepartingWriter(tt.when)

				res, err := sse.RelayProxy(t.Context(), w, sseResponse(src, nil), sse.ProxyOptions{
					Provider:          sse.LMStudioProvider,
					AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
					BackendModel:      "lmstudio/qwen3-coder-30b",
					HeartbeatInterval: -1,
					FirstEventWait:    5 * time.Second,
					ClientDone:        w.done,
					OnOverflow:        testEngineOverflowResponder,
				})
				if err != nil {
					t.Errorf("RelayProxy error = %v, want nil", err)
				}
				if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK, StatusOverride: "upstream_aborted"}, res); diff != "" {
					t.Errorf("outcome.Result (-want +got):\n%s", diff)
				}
				if got := src.closes.Load(); got != 1 {
					t.Errorf("owned resp.Body close count = %d, want 1", got)
				}
				assertNoOutputAfterHead(t, w, tt.wantWriteHeaders)
				status, wroteHeader, _, _, _ := w.snapshot()
				switch {
				case tt.wantWriteHeaders == 0 && wroteHeader:
					t.Errorf("client saw status %d although it left before WriteHeader", status)
				case tt.wantWriteHeaders == 1 && status != http.StatusBadGateway:
					// The WriteHeader that raised the signal was already running and is not retracted.
					t.Errorf("client status = %d, want the early-end 502 whose WriteHeader was running when the client left", status)
				}
			})
		})
	}
}

func TestLMStudioOverflowResponderWriterGatedOnClientDeparture(t *testing.T) {
	counts := sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))}
	reference := newStreamingRecorder()
	wantResult, _ := testEngineOverflowResponder(reference, http.StatusOK, counts)
	_, _, wantBody, _, _ := reference.snapshot()

	tests := map[string]struct {
		when departure
	}{
		"success: responder that writes and flushes for a connected client reaches the client with its head, body and flush": {
			when: departNever,
		},
		"success: responder that started with a connected client and lost it at its WriteHeader gets no body write and no flush to the client": {
			when: departOnWriteHeader,
		},
		"success: responder that writes and flushes for a client gone before the overflow reaches nothing": {
			when: departBeforeEntry,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				src := newEOFChunkBody([]byte(lmStudioOverflowFrame(6017, 4096)))
				w := newDepartingWriter(tt.when)
				var calls []recordedOverflow
				var flushErr error
				// The responder flushes what it wrote, as a responder that streams its reply would;
				// the writer it is given must let that reach a connected client and nothing else.
				responder := func(rw http.ResponseWriter, upstreamStatus int, c sentry.Counts) (outcome.Result, error) {
					calls = append(calls, recordedOverflow{Counts: c, UpstreamStatus: upstreamStatus})
					res, err := testEngineOverflowResponder(rw, upstreamStatus, c)
					flushErr = http.NewResponseController(rw).Flush()
					return res, err
				}

				res, err := sse.RelayProxy(t.Context(), w, sseResponse(src, nil), sse.ProxyOptions{
					Provider:          sse.LMStudioProvider,
					AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
					BackendModel:      "lmstudio/qwen3-coder-30b",
					HeartbeatInterval: -1,
					FirstEventWait:    5 * time.Second,
					ClientDone:        w.done,
					OnOverflow:        responder,
				})
				if err != nil {
					t.Errorf("RelayProxy error = %v, want the responder's nil error", err)
				}
				if diff := gocmp.Diff(wantResult, res); diff != "" {
					t.Errorf("outcome.Result is not the responder's result (-want +got):\n%s", diff)
				}
				if got := src.closes.Load(); got != 1 {
					t.Errorf("owned resp.Body close count = %d, want 1", got)
				}
				if diff := gocmp.Diff([]recordedOverflow{{Counts: counts, UpstreamStatus: http.StatusOK}}, calls); diff != "" {
					t.Fatalf("overflow responder calls (-want +got):\n%s", diff)
				}

				status, wroteHeader, body, _, flushes := w.snapshot()
				switch tt.when {
				case departNever:
					if flushErr != nil {
						t.Errorf("responder Flush error = %v, want nil for a connected client", flushErr)
					}
					if got := w.writeHeaders.Load(); got != 1 || status != http.StatusRequestEntityTooLarge {
						t.Errorf("client WriteHeader calls = %d status = %d, want 1 and 413", got, status)
					}
					if diff := gocmp.Diff(wantBody, body); diff != "" {
						t.Errorf("client body (-want +got):\n%s", diff)
					}
					if flushes != 1 {
						t.Errorf("client flushes = %d, want the responder's 1", flushes)
					}
				case departOnWriteHeader:
					assertNoOutputAfterHead(t, w, 1)
					if status != http.StatusRequestEntityTooLarge {
						t.Errorf("client status = %d, want the 413 whose WriteHeader was running when the client left", status)
					}
				case departBeforeEntry:
					if got := w.writeHeaders.Load(); got != 0 || wroteHeader {
						t.Errorf("client WriteHeader calls = %d wroteHeader = %t, want none", got, wroteHeader)
					}
					assertNothingWritten(t, w.streamingRecorder)
				}
			})
		})
	}
}
