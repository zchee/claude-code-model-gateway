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
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

var (
	errDispatchFailed   = errors.New("dispatch failed after the upstream answered")
	errDispatchShutdown = errors.New("worker stopped while the attempt was dispatched")
)

// dispatchErrorStatus is the status of the response an attempt returns together with an error.
// It differs from the first attempt's 200 and from the dispatch result's own status, so the
// result shows which of the three the guard reported.
const dispatchErrorStatus = http.StatusServiceUnavailable

// dispatchErrorResult is the result the attempt returns beside its response and its error.
var dispatchErrorResult = outcome.Result{
	Contacted:      true,
	Status:         http.StatusBadGateway,
	StatusOverride: "upstream_error",
	Route:          outcome.Route{Backend: "codex", Via: "proxy"},
}

// dispatchErrorServe runs one guarded request whose attempt, the first one or the second after
// a discarded first attempt whose head the client already received, returns a response with
// the status dispatchErrorStatus together with errDispatchFailed. With endService the attempt
// ends the service before it returns.
type dispatchErrorServe struct {
	committed  bool
	endService bool
	cause      error
}

// dispatchErrorServed is what one dispatchErrorServe run observed.
type dispatchErrorServed struct {
	res outcome.Result
	err error
	// heads lists the head committed flag Next received on each call.
	heads []bool
	// closes counts the closes of each attempt's body, in attempt order.
	closes []int32
	w      *headRecorder
	// serviceEnded reports whether the service context had ended when Serve returned.
	serviceEnded bool
}

func (d dispatchErrorServe) run(t *testing.T) dispatchErrorServed {
	t.Helper()
	service, stop := context.WithCancelCause(t.Context())
	defer stop(nil)
	retries := 0.0
	if d.committed {
		retries = 1
	}
	req := guardRequest()
	var (
		heads  []bool
		bodies []*countedBody
	)
	req.Next = func(_ context.Context, head bool) (*upstream.Response, outcome.Result, error) {
		heads = append(heads, head)
		if d.committed && len(heads) == 1 {
			body := &countedBody{ReadCloser: io.NopCloser(strings.NewReader(truncatedStream()))}
			bodies = append(bodies, body)
			return sseResponse(body), outcome.Result{Contacted: true}, nil
		}
		body := &countedBody{ReadCloser: io.NopCloser(strings.NewReader(completeStream("this attempt must not reach the client")))}
		bodies = append(bodies, body)
		resp := sseResponse(body)
		resp.StatusCode = dispatchErrorStatus
		if d.endService {
			stop(d.cause)
		}
		return resp, dispatchErrorResult, errDispatchFailed
	}
	w := &headRecorder{rec: httptest.NewRecorder()}
	res, err := guardRunner(service, retries).Serve(t.Context(), w, req)
	closes := make([]int32, len(bodies))
	for i, body := range bodies {
		closes[i] = body.closes.Load()
	}
	return dispatchErrorServed{res: res, err: err, heads: heads, closes: closes, w: w, serviceEnded: service.Err() != nil}
}

// wantDispatchHeads is the head committed flag of each attempt of a dispatchErrorServe run.
func (d dispatchErrorServe) wantDispatchHeads() []bool {
	if d.committed {
		return []bool{false, true}
	}
	return []bool{false}
}

// checkDispatchErrorClient checks that the client received nothing from the attempt that
// returned an error: no status, header field, body or flush when no head was committed, and
// only the first attempt's committed 200 head with an empty body otherwise.
func checkDispatchErrorClient(t *testing.T, committed bool, w *headRecorder) {
	t.Helper()
	if committed {
		if diff := gocmp.Diff([]int{http.StatusOK}, w.heads); diff != "" {
			t.Errorf("client statuses (-want +got):\n%s", diff)
		}
	} else {
		if len(w.heads) != 0 {
			t.Errorf("client received statuses %v, want none", w.heads)
		}
		if h := w.Header(); len(h) != 0 {
			t.Errorf("client received header fields %v, want none", h)
		}
		if w.rec.Flushed {
			t.Error("client response was flushed, want no flush")
		}
	}
	if body := w.bodyString(); body != "" {
		t.Errorf("client received body %q, want none", body)
	}
}

// TestGuardDispatchErrorAfterServiceEnded checks an attempt that returns a response together
// with an error after the service has ended: the guard closes the response body once, relays
// nothing of it, and returns the shutdown result with the response's status and the service's
// cause instead of the dispatch result and error. A client already holding the committed head
// of an earlier attempt gets an abort that wraps the cause.
func TestGuardDispatchErrorAfterServiceEnded(t *testing.T) {
	// The shutdown result of an attempt that carried a status: the contact, that status and the
	// aborted route status (compactguard's overrideAborted).
	shutdownResult := outcome.Result{Contacted: true, Status: dispatchErrorStatus, StatusOverride: "upstream_aborted"}
	tests := map[string]struct {
		committed bool
		cause     error
		wantCause error
	}{
		"error: first attempt, service ended with a cause": {
			cause:     errDispatchShutdown,
			wantCause: errDispatchShutdown,
		},
		"error: first attempt, service ended without a cause": {
			wantCause: context.Canceled,
		},
		"error: attempt after a committed head, service ended with a cause": {
			committed: true,
			cause:     errDispatchShutdown,
			wantCause: errDispatchShutdown,
		},
		"error: attempt after a committed head, service ended without a cause": {
			committed: true,
			wantCause: context.Canceled,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := dispatchErrorServe{committed: tt.committed, endService: true, cause: tt.cause}
				got := d.run(t)
				if !got.serviceEnded {
					t.Fatal("the attempt did not end the service; the test setup is broken")
				}
				if diff := gocmp.Diff(d.wantDispatchHeads(), got.heads); diff != "" {
					t.Fatalf("attempts started (head committed per attempt) mismatch (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(shutdownResult, got.res); diff != "" {
					t.Errorf("Serve result (-want +got):\n%s", diff)
				}
				if !errors.Is(got.err, tt.wantCause) {
					t.Errorf("Serve error = %v, want the service context's cause %v", got.err, tt.wantCause)
				}
				if errors.Is(got.err, errDispatchFailed) {
					t.Errorf("Serve error = %v carries the dispatch error, want the shutdown instead", got.err)
				}
				if aborted := errors.Is(got.err, http.ErrAbortHandler); aborted != tt.committed {
					t.Errorf("Serve error = %v: abort of the committed response = %t, want %t", got.err, aborted, tt.committed)
				}
				for i, n := range got.closes {
					if n != 1 {
						t.Errorf("attempt %d body closed %d times, want 1", i+1, n)
					}
				}
				checkDispatchErrorClient(t, tt.committed, got.w)
			})
		})
	}
}

// TestGuardDispatchErrorWithLiveService checks that while the service is alive, an attempt that
// returns a response together with an error still gives the dispatch result and error: the
// response body is closed once and nothing of it reaches the client.
func TestGuardDispatchErrorWithLiveService(t *testing.T) {
	tests := map[string]struct {
		committed bool
	}{
		"error: first attempt returns the dispatch error":                  {},
		"error: attempt after a committed head returns the dispatch error": {committed: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := dispatchErrorServe{committed: tt.committed}
				got := d.run(t)
				if got.serviceEnded {
					t.Fatal("the service ended without being stopped; the test setup is broken")
				}
				if diff := gocmp.Diff(d.wantDispatchHeads(), got.heads); diff != "" {
					t.Fatalf("attempts started (head committed per attempt) mismatch (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(dispatchErrorResult, got.res); diff != "" {
					t.Errorf("Serve result (-want +got):\n%s", diff)
				}
				if !errors.Is(got.err, errDispatchFailed) {
					t.Errorf("Serve error = %v, want the dispatch error %v", got.err, errDispatchFailed)
				}
				if errors.Is(got.err, http.ErrAbortHandler) {
					t.Errorf("Serve error = %v is an abort, want the dispatch error alone", got.err)
				}
				for i, n := range got.closes {
					if n != 1 {
						t.Errorf("attempt %d body closed %d times, want 1", i+1, n)
					}
				}
				checkDispatchErrorClient(t, tt.committed, got.w)
			})
		})
	}
}

// TestGuardSettledVerdictAfterServiceEnded checks that an attempt which settles its own verdict
// (it returns no response) keeps that verdict and its error unchanged although the service
// ended while it ran: the answer it already gave is not rewritten as a shutdown.
func TestGuardSettledVerdictAfterServiceEnded(t *testing.T) {
	verdict := outcome.Result{
		Contacted: true,
		Status:    http.StatusUnauthorized,
		Route:     outcome.Route{Backend: "codex", Via: "proxy", Overflow: &outcome.Overflow{Phase: "upstream"}},
	}
	tests := map[string]struct {
		committed bool
		err       error
	}{
		"success: first attempt settles without an error":             {},
		"error: first attempt settles with its dispatch error":        {err: errDispatchFailed},
		"success: attempt after a committed head settles":             {committed: true},
		"error: attempt after a committed head settles with an error": {committed: true, err: errDispatchFailed},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, stop := context.WithCancelCause(t.Context())
				defer stop(nil)
				retries := 0.0
				if tt.committed {
					retries = 1
				}
				req := guardRequest()
				var (
					heads     []bool
					firstBody *countedBody
				)
				req.Next = func(_ context.Context, head bool) (*upstream.Response, outcome.Result, error) {
					heads = append(heads, head)
					if tt.committed && len(heads) == 1 {
						firstBody = &countedBody{ReadCloser: io.NopCloser(strings.NewReader(truncatedStream()))}
						return sseResponse(firstBody), outcome.Result{Contacted: true}, nil
					}
					stop(errDispatchShutdown)
					return nil, verdict, tt.err
				}
				w := &headRecorder{rec: httptest.NewRecorder()}
				got, err := guardRunner(service, retries).Serve(t.Context(), w, req)

				if service.Err() == nil {
					t.Fatal("the attempt did not end the service; the test setup is broken")
				}
				wantHeads := []bool{false}
				if tt.committed {
					wantHeads = []bool{false, true}
				}
				if diff := gocmp.Diff(wantHeads, heads); diff != "" {
					t.Fatalf("attempts started (head committed per attempt) mismatch (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(verdict, got); diff != "" {
					t.Errorf("Serve result (-want +got):\n%s", diff)
				}
				if !errors.Is(err, tt.err) || (tt.err == nil && err != nil) {
					t.Errorf("Serve error = %v, want the attempt's own error %v", err, tt.err)
				}
				if errors.Is(err, errDispatchShutdown) {
					t.Errorf("Serve error = %v carries the service's cause, want the settled verdict's error", err)
				}
				if firstBody != nil {
					if n := firstBody.closes.Load(); n != 1 {
						t.Errorf("attempt 1 body closed %d times, want 1", n)
					}
				}
				checkDispatchErrorClient(t, tt.committed, w)
			})
		})
	}
}
