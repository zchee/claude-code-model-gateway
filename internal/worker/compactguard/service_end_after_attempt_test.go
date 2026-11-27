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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

var errServiceEnded = errors.New("worker service ended")

// serviceEndingCapture is a usage capture that runs onBytes each time the guard notes response
// bytes, which the guard does on its own goroutine as soon as a read's data arrives.
type serviceEndingCapture struct{ onBytes func() }

var _ compactguard.UsageCapture = (*serviceEndingCapture)(nil)

func (c *serviceEndingCapture) NoteResponseBytes(int64) {
	if c.onBytes != nil {
		c.onBytes()
	}
}
func (*serviceEndingCapture) ObserveEvent(jsonx.WireValue) {}
func (*serviceEndingCapture) Snapshot() map[string]any     { return nil }
func (*serviceEndingCapture) ResetUsage()                  {}
func (*serviceEndingCapture) SetResponseComplete(bool)     {}

// TestGuardServiceEndsAfterAttemptSettles ends the service at points where the guard has already
// seen the attempt's outcome: inside the dispatch that hands over the attempt's response, in the
// handling of the read that ends a finished stream, and in the close of a stream that ended
// cleanly, finished or not. The guard must act on the shutdown, not on the attempt: it commits no
// response head that was not committed yet, writes none of the held summary, starts no retry, and
// returns the service's cause with the response's status. A
// retry whose dispatch ends the service is run for many rounds, since the guard's wait on the
// read and on the service can pick either once both are ready. The control case shows the same
// harness delivering the summary with a live service.
func TestGuardServiceEndsAfterAttemptSettles(t *testing.T) {
	const rounds = 64
	summary := completeStream("summary held until the turn ends")
	// shutdownResult is the result of a shutdown during an attempt whose upstream status is
	// known: the contact, that status, and the aborted label.
	shutdownResult := outcome.Result{Contacted: true, Status: http.StatusOK, StatusOverride: "upstream_aborted"}

	type served struct {
		res    outcome.Result
		err    error
		w      *headRecorder
		closes []int32
		ended  bool
		// dispatches counts the calls of Next.
		dispatches int
	}
	tests := map[string]struct {
		// serve runs one guarded request with service ending by stop as the case arranges.
		serve func(t *testing.T, service context.Context, stop context.CancelCauseFunc) served
		// committedBefore is set when an earlier attempt committed the head before the service ended.
		committedBefore bool
		endService      bool
		rounds          int
		// wantDispatches is how many attempts the guard may start; zero means one.
		wantDispatches int
	}{
		"error: dispatch that ends the service before handing over the first attempt commits no head": {
			endService: true,
			serve: func(t *testing.T, service context.Context, stop context.CancelCauseFunc) served {
				req := guardRequest()
				body := &countedBody{ReadCloser: io.NopCloser(strings.NewReader(summary))}
				dispatches := 0
				req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
					dispatches++
					stop(errServiceEnded)
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				w := &headRecorder{rec: httptest.NewRecorder()}
				res, err := guardRunner(service, 1).Serve(t.Context(), w, req)
				return served{res: res, err: err, w: w, closes: []int32{body.closes.Load()}, ended: service.Err() != nil, dispatches: dispatches}
			},
		},
		"error: dispatch of a retry that ends the service before handing over its response writes no summary": {
			endService:      true,
			committedBefore: true,
			rounds:          rounds,
			serve: func(t *testing.T, service context.Context, stop context.CancelCauseFunc) served {
				req := guardRequest()
				var bodies []*countedBody
				dispatches := 0
				req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
					dispatches++
					stream := truncatedStream()
					if len(bodies) == 1 {
						stop(errServiceEnded)
						stream = summary
					}
					body := &countedBody{ReadCloser: io.NopCloser(strings.NewReader(stream))}
					bodies = append(bodies, body)
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				w := &headRecorder{rec: httptest.NewRecorder()}
				res, err := guardRunner(service, 1).Serve(t.Context(), w, req)
				closes := make([]int32, len(bodies))
				for i, b := range bodies {
					closes[i] = b.closes.Load()
				}
				if len(bodies) != 2 {
					t.Fatalf("attempts dispatched = %d, want 2; the setup is broken", len(bodies))
				}
				return served{res: res, err: err, w: w, closes: closes, ended: service.Err() != nil, dispatches: dispatches}
			},
			wantDispatches: 2,
		},
		"error: the service ending in the close of an unfinished stream starts no retry": {
			endService:      true,
			committedBefore: true,
			serve: func(t *testing.T, service context.Context, stop context.CancelCauseFunc) served {
				req := guardRequest()
				dispatches := 0
				var bodies []*closeHookBody
				req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
					dispatches++
					// The stream ends cleanly without a finished turn, which the guard would retry.
					body := &closeHookBody{Reader: strings.NewReader(truncatedStream()), onClose: func() { stop(errServiceEnded) }}
					bodies = append(bodies, body)
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				w := &headRecorder{rec: httptest.NewRecorder()}
				res, err := guardRunner(service, 1).Serve(t.Context(), w, req)
				closes := make([]int32, len(bodies))
				for i, b := range bodies {
					closes[i] = b.closes.Load()
				}
				return served{res: res, err: err, w: w, closes: closes, ended: service.Err() != nil, dispatches: dispatches}
			},
		},
		"error: the service ending while the stream's last read is handled writes no summary": {
			endService:      true,
			committedBefore: true,
			serve: func(t *testing.T, service context.Context, stop context.CancelCauseFunc) served {
				req := guardRequest()
				// One read returns the whole finished stream together with io.EOF.
				body := &countedBody{ReadCloser: io.NopCloser(iotest.DataErrReader(strings.NewReader(summary)))}
				req.Usage = &serviceEndingCapture{onBytes: func() { stop(errServiceEnded) }}
				dispatches := 0
				req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
					dispatches++
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				w := &headRecorder{rec: httptest.NewRecorder()}
				res, err := guardRunner(service, 1).Serve(t.Context(), w, req)
				return served{res: res, err: err, w: w, closes: []int32{body.closes.Load()}, ended: service.Err() != nil, dispatches: dispatches}
			},
		},
		"error: the service ending in the close of the finished stream writes no summary": {
			endService:      true,
			committedBefore: true,
			serve: func(t *testing.T, service context.Context, stop context.CancelCauseFunc) served {
				req := guardRequest()
				body := &closeHookBody{Reader: strings.NewReader(summary), onClose: func() { stop(errServiceEnded) }}
				dispatches := 0
				req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
					dispatches++
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				w := &headRecorder{rec: httptest.NewRecorder()}
				res, err := guardRunner(service, 1).Serve(t.Context(), w, req)
				return served{res: res, err: err, w: w, closes: []int32{body.closes.Load()}, ended: service.Err() != nil, dispatches: dispatches}
			},
		},
		"success: a live service gets the held summary at the end of the finished stream": {
			serve: func(t *testing.T, service context.Context, _ context.CancelCauseFunc) served {
				req := guardRequest()
				body := &closeHookBody{Reader: strings.NewReader(summary)}
				dispatches := 0
				req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
					dispatches++
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				w := &headRecorder{rec: httptest.NewRecorder()}
				res, err := guardRunner(service, 1).Serve(t.Context(), w, req)
				return served{res: res, err: err, w: w, closes: []int32{body.closes.Load()}, ended: service.Err() != nil, dispatches: dispatches}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			n := max(tt.rounds, 1)
			var bad int
			var firstBad string
			for round := range n {
				// The bubble makes the retry wait virtual, so a round costs no real time.
				synctest.Test(t, func(t *testing.T) {
					service, stop := context.WithCancelCause(t.Context())
					defer stop(nil)
					got := tt.serve(t, service, stop)
					if got.ended != tt.endService {
						t.Fatalf("service ended = %t, want %t; the setup is broken", got.ended, tt.endService)
					}
					if want := max(tt.wantDispatches, 1); got.dispatches != want {
						t.Errorf("attempts dispatched = %d, want %d (no retry after the service ended)", got.dispatches, want)
					}
					for i, c := range got.closes {
						if c != 1 {
							t.Errorf("attempt %d body closed %d times, want 1", i+1, c)
						}
					}
					if problems := settledShutdownProblems(got.res, got.err, got.w, tt.endService, tt.committedBefore, summary, shutdownResult); len(problems) != 0 {
						bad++
						if firstBad == "" {
							firstBad = fmt.Sprintf("round %d of %d: %s", round, n, strings.Join(problems, "; "))
						}
					}
				})
			}
			if bad != 0 {
				t.Errorf("%d of %d rounds failed; first: %s", bad, n, firstBad)
			}
		})
	}
}

// settledShutdownProblems lists how one guarded run differs from what the case promises. The first
// group of checks is what the service shutdown must achieve whatever the guard returns; the second
// is the shape a shutdown during an attempt has elsewhere in the guard.
func settledShutdownProblems(res outcome.Result, err error, w *headRecorder, endService, committedBefore bool, summary string, shutdownResult outcome.Result) []string {
	var problems []string
	if !endService {
		if err != nil {
			problems = append(problems, fmt.Sprintf("Serve error = %v, want nil with a live service", err))
		}
		if diff := gocmp.Diff([]int{http.StatusOK}, w.heads); diff != "" {
			problems = append(problems, fmt.Sprintf("response heads (-want +got): %s", diff))
		}
		if got := w.bodyString(); got != delivered(summary) {
			problems = append(problems, fmt.Sprintf("client body = %q, want the delivered summary %q", got, delivered(summary)))
		}
		if res.Status != http.StatusOK || res.StatusOverride != "" {
			problems = append(problems, fmt.Sprintf("result status=%d override=%q, want 200 and none", res.Status, res.StatusOverride))
		}
		return problems
	}

	// What the shutdown must achieve.
	var wantHeads []int
	if committedBefore {
		wantHeads = []int{http.StatusOK}
	}
	if diff := gocmp.Diff(wantHeads, w.heads); diff != "" {
		problems = append(problems, fmt.Sprintf("response heads committed (-want +got): %s", diff))
	}
	if !committedBefore && len(w.Header()) != 0 {
		problems = append(problems, fmt.Sprintf("client header fields = %v, want none", w.Header()))
	}
	if got := w.bodyString(); got != "" {
		problems = append(problems, fmt.Sprintf("client body after the service ended = %q, want none of the held summary", got))
	}
	if !errors.Is(err, errServiceEnded) {
		problems = append(problems, fmt.Sprintf("Serve error = %v, want the service's cause %v", err, errServiceEnded))
	}

	// The shape of a shutdown during an attempt.
	if committedBefore != errors.Is(err, http.ErrAbortHandler) {
		problems = append(problems, fmt.Sprintf("Serve error = %v: aborts the response = %t, want %t (a committed response is aborted, an uncommitted one is not)", err, errors.Is(err, http.ErrAbortHandler), committedBefore))
	}
	if diff := gocmp.Diff(shutdownResult, res); diff != "" {
		problems = append(problems, fmt.Sprintf("Serve result (-want +got): %s", diff))
	}
	return problems
}

// writeLog records each client write separately, next to the head and body a headRecorder keeps.
type writeLog struct {
	headRecorder
	writes []string
}

func (w *writeLog) Write(b []byte) (int, error) {
	w.writes = append(w.writes, string(b))
	return w.headRecorder.Write(b)
}

// TestGuardWritesHeldFramesTogether checks that the guard writes the frames it held for a finished
// compaction stream to the client in a single write, followed by a synthesized stop as a write of
// its own when the stream lacked one. Each frame arrives in its own upstream read here; the source
// writes the held frames one by one.
func TestGuardWritesHeldFramesTogether(t *testing.T) {
	// literalMessageStop restates sse.SynthesizedMessageStop.
	const literalMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	withStop := []string{frameMessageStart, frameBlockStart, frameTextDelta("held"), frameBlockStop, frameMessageDelta, frameMessageStop}
	withoutStop := withStop[:len(withStop)-1]
	tests := map[string]struct {
		frames     []string
		wantWrites []string
	}{
		"success: the held frames of a stream with its own stop reach the client in one write": {
			frames:     withStop,
			wantWrites: []string{delivered(strings.Join(withStop, ""))},
		},
		"success: the held frames reach the client in one write and the synthesized stop in another": {
			frames:     withoutStop,
			wantWrites: []string{delivered(strings.Join(withoutStop, "")), literalMessageStop},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			readers := make([]io.Reader, len(tt.frames))
			for i, f := range tt.frames {
				readers[i] = strings.NewReader(f)
			}
			// io.MultiReader returns at most one of its readers' data per read.
			body := &countedBody{ReadCloser: io.NopCloser(io.MultiReader(readers...))}
			req := guardRequest()
			req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
				return sseResponse(body), outcome.Result{Contacted: true}, nil
			}
			w := &writeLog{rec: httptest.NewRecorder()}

			res, err := guardRunner(t.Context(), 1).Serve(t.Context(), w, req)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if res.Status != http.StatusOK || res.StatusOverride != "" {
				t.Errorf("result status=%d override=%q, want 200 and none", res.Status, res.StatusOverride)
			}
			if diff := gocmp.Diff(tt.wantWrites, w.writes); diff != "" {
				t.Errorf("client writes (-want +got):\n%s", diff)
			}
			if got := body.closes.Load(); got != 1 {
				t.Errorf("body closed %d times, want 1", got)
			}
		})
	}
}
