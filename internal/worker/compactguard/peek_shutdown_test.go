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

package compactguard_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const (
	// peekEarlyEndBody is the client body the guard writes for an LM Studio stream that broke
	// before its first data event.
	peekEarlyEndBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`

	// peekOverflowReply is the body the test overflow responder writes.
	peekOverflowReply = `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long"}}`
)

var (
	errPeekReset    = errors.New("upstream connection reset")
	errPeekShutdown = errors.New("service shutting down")
	errPeekOverflow = errors.New("overflow responder failed")
)

// closeHookBody is an upstream body that counts its closes and runs onClose on each, as a
// transport whose connection close ends the service.
type closeHookBody struct {
	io.Reader
	closes  atomic.Int32
	onClose func()
}

func (b *closeHookBody) Close() error {
	b.closes.Add(1)
	if b.onClose != nil {
		b.onClose()
	}
	return nil
}

// peekBodies are the two first-event outcomes the peek closes the body for.
var peekBodies = map[string]func() io.Reader{
	// overflow is a stream whose first data event is an engine context overflow.
	"overflow": func() io.Reader { return strings.NewReader(overflowFirstEvent) },
	// earlyEnd is an incomplete first frame followed by a read error that is not io.EOF.
	"earlyEnd": func() io.Reader {
		return io.MultiReader(strings.NewReader("event: message_start\ndata: {\"type\""), errReader{err: errPeekReset})
	},
}

// errReader fails every read with err.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// peekOverflowResult is what the test overflow responder returns.
var peekOverflowResult = outcome.Result{
	Status:    http.StatusRequestEntityTooLarge,
	Contacted: true,
	Route:     outcome.Route{Overflow: &outcome.Overflow{Phase: "upstream"}},
}

// peekResponderCall records one call of the overflow responder and whether it received the
// client's own writer.
type peekResponderCall struct {
	Status     int
	Counts     sentry.Counts
	RealWriter bool
}

// peekServe runs one LM Studio attempt whose body is made by reader, with the service ended by
// the body's close when endOnClose is set. nextContacted is what the dispatch reports, and
// responderContacted is the Contacted field of the overflow responder's result.
type peekServe struct {
	reader             func() io.Reader
	endOnClose         bool
	nextContacted      bool
	responderContacted bool
}

// peekServed is what one peekServe run observed.
type peekServed struct {
	res    outcome.Result
	err    error
	calls  []peekResponderCall
	closes int32
	w      *headRecorder
	// serviceEnded reports whether the service context had ended when Serve returned.
	serviceEnded bool
}

func (p peekServe) run(t *testing.T) peekServed {
	t.Helper()
	service, stop := context.WithCancelCause(t.Context())
	defer stop(nil)
	body := &closeHookBody{Reader: p.reader()}
	if p.endOnClose {
		body.onClose = func() { stop(errPeekShutdown) }
	}
	req := guardRequest()
	req.ProxyProvider = "lmstudio"
	req.Stream.Provider = "lmstudio"
	req.Stream.FirstEventWait = time.Hour
	nextCalls := 0
	req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
		nextCalls++
		return sseResponse(body), outcome.Result{Contacted: p.nextContacted}, nil
	}
	w := &headRecorder{rec: httptest.NewRecorder()}
	var calls []peekResponderCall
	req.Stream.OnOverflow = func(rw http.ResponseWriter, status int, counts sentry.Counts) (outcome.Result, error) {
		calls = append(calls, peekResponderCall{Status: status, Counts: counts, RealWriter: rw == http.ResponseWriter(w)})
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(rw, peekOverflowReply)
		if err := http.NewResponseController(rw).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("overflow responder flush: %v", err)
		}
		res := peekOverflowResult
		res.Contacted = p.responderContacted
		return res, errPeekOverflow
	}
	res, err := guardRunner(service, 1).Serve(t.Context(), w, req)
	if nextCalls != 1 {
		t.Errorf("Next called %d times, want 1", nextCalls)
	}
	return peekServed{res: res, err: err, calls: calls, closes: body.closes.Load(), w: w, serviceEnded: service.Err() != nil}
}

// overflowCounts are the counts of overflowFirstEvent.
func overflowCounts() sentry.Counts {
	return sentry.Counts{Tokens: new(6017.0), Limit: new(4096.0)}
}

// TestGuardServiceEndsAtPeekClose checks a service that ends while the first-event peek
// closes the upstream body of an overflow or an early end, after the peek's last look at
// the service: the guard observes the shutdown itself. The overflow responder still runs
// once with the event's counts, on a writer that is not the client's, and its result and
// error are dropped except for contact; the client receives no header field, status, body
// or flush; Serve returns the shutdown result and the service context's cause; the body is
// closed once.
func TestGuardServiceEndsAtPeekClose(t *testing.T) {
	shutdownResult := outcome.Result{Contacted: true, Status: http.StatusOK, StatusOverride: "upstream_aborted"}
	tests := map[string]struct {
		body               string
		nextContacted      bool
		responderContacted bool
		wantCalls          []peekResponderCall
	}{
		"error: overflow first event while the close ends the service": {
			body:               "overflow",
			nextContacted:      true,
			responderContacted: true,
			wantCalls:          []peekResponderCall{{Status: http.StatusOK, Counts: overflowCounts()}},
		},
		"error: overflow responder contact carries over to the shutdown result": {
			body:               "overflow",
			nextContacted:      false,
			responderContacted: true,
			wantCalls:          []peekResponderCall{{Status: http.StatusOK, Counts: overflowCounts()}},
		},
		"error: read error before a complete first event while the close ends the service": {
			body:          "earlyEnd",
			nextContacted: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				got := peekServe{reader: peekBodies[tt.body], endOnClose: true, nextContacted: tt.nextContacted, responderContacted: tt.responderContacted}.run(t)
				if !got.serviceEnded {
					t.Fatal("the body close did not end the service; the test setup is broken")
				}
				if !errors.Is(got.err, errPeekShutdown) {
					t.Errorf("Serve error = %v, want the service context's cause %v", got.err, errPeekShutdown)
				}
				if errors.Is(got.err, errPeekOverflow) {
					t.Errorf("Serve error = %v carries the overflow responder's error, want it dropped", got.err)
				}
				if errors.Is(got.err, http.ErrAbortHandler) {
					t.Errorf("Serve error = %v aborts a response that was never committed", got.err)
				}
				if diff := gocmp.Diff(shutdownResult, got.res); diff != "" {
					t.Errorf("Serve result (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.wantCalls, got.calls); diff != "" {
					t.Errorf("overflow responder calls (-want +got):\n%s", diff)
				}
				if got.closes != 1 {
					t.Errorf("upstream body closed %d times, want 1", got.closes)
				}
				if h := got.w.Header(); len(h) != 0 {
					t.Errorf("client received header fields %v, want none", h)
				}
				if len(got.w.heads) != 0 {
					t.Errorf("client received statuses %v, want none", got.w.heads)
				}
				if body := got.w.bodyString(); body != "" {
					t.Errorf("client received body %q, want none", body)
				}
				if got.w.rec.Flushed {
					t.Error("client response was flushed, want no flush")
				}
			})
		})
	}
}

// TestGuardPeekResultWithLiveService checks that with a service that stays live through the
// peek's close, the guard acts on the peek's result as before: an overflow goes to the
// responder on the client's own writer and returns the responder's result and error, and an
// early end answers the client with a 502 JSON error and the aborted route status.
func TestGuardPeekResultWithLiveService(t *testing.T) {
	tests := map[string]struct {
		body      string
		wantRes   outcome.Result
		wantErr   error
		wantCalls []peekResponderCall
		wantHeads []int
		wantType  string
		wantBody  string
	}{
		"error: overflow first event reaches the client through the responder": {
			body:      "overflow",
			wantRes:   peekOverflowResult,
			wantErr:   errPeekOverflow,
			wantCalls: []peekResponderCall{{Status: http.StatusOK, Counts: overflowCounts(), RealWriter: true}},
			wantHeads: []int{http.StatusRequestEntityTooLarge},
			wantType:  "application/json",
			wantBody:  peekOverflowReply,
		},
		"error: read error before a complete first event answers the client": {
			body:      "earlyEnd",
			wantRes:   outcome.Result{Contacted: true, Status: http.StatusOK, StatusOverride: "upstream_aborted"},
			wantHeads: []int{http.StatusBadGateway},
			wantType:  "application/json",
			wantBody:  peekEarlyEndBody,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				got := peekServe{reader: peekBodies[tt.body], nextContacted: true, responderContacted: true}.run(t)
				if got.serviceEnded {
					t.Fatal("the service ended without being stopped; the test setup is broken")
				}
				if !errors.Is(got.err, tt.wantErr) || (tt.wantErr == nil && got.err != nil) {
					t.Errorf("Serve error = %v, want %v", got.err, tt.wantErr)
				}
				if diff := gocmp.Diff(tt.wantRes, got.res); diff != "" {
					t.Errorf("Serve result (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(tt.wantCalls, got.calls); diff != "" {
					t.Errorf("overflow responder calls (-want +got):\n%s", diff)
				}
				if got.closes != 1 {
					t.Errorf("upstream body closed %d times, want 1", got.closes)
				}
				if diff := gocmp.Diff(tt.wantHeads, got.w.heads); diff != "" {
					t.Errorf("client statuses (-want +got):\n%s", diff)
				}
				if gotType := got.w.Header().Get("Content-Type"); gotType != tt.wantType {
					t.Errorf("client Content-Type = %q, want %q", gotType, tt.wantType)
				}
				if diff := gocmp.Diff(tt.wantBody, got.w.bodyString()); diff != "" {
					t.Errorf("client body (-want +got):\n%s", diff)
				}
			})
		})
	}
}
