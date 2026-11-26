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

//go:build process

package reply_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// errClientWrite is what a client writer returns when its body write fails.
var errClientWrite = errors.New("client connection broke during the body write")

// eventLog orders what one reply hands to the client writer and to the diagnostics writer.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

// clientWriter is the client's response writer. It records each Header, WriteHeader and Write
// call, ends the client context with leave inside the method leaveIn names ("Header" or
// "WriteHeader"), fails every body Write when failWrite is set, and adds each head and body write
// to log when one is given.
type clientWriter struct {
	*httptest.ResponseRecorder
	leaveIn     string
	leave       func()
	failWrite   bool
	log         *eventLog
	headerCalls int
	heads       []int
	writes      []string
}

func (w *clientWriter) Header() http.Header {
	w.headerCalls++
	if w.leaveIn == "Header" {
		w.leave()
	}
	return w.ResponseRecorder.Header()
}

func (w *clientWriter) WriteHeader(status int) {
	w.heads = append(w.heads, status)
	w.log.add("client head " + strconv.Itoa(status))
	w.ResponseRecorder.WriteHeader(status)
	if w.leaveIn == "WriteHeader" {
		w.leave()
	}
}

func (w *clientWriter) Write(p []byte) (int, error) {
	w.writes = append(w.writes, string(p))
	w.log.add("client body")
	if w.failWrite {
		return 0, errClientWrite
	}
	return w.ResponseRecorder.Write(p)
}

// loggedDiagnostics is a diagnostics writer that keeps each line and adds it to the shared log.
type loggedDiagnostics struct {
	log   *eventLog
	lines []string
}

func (d *loggedDiagnostics) Write(p []byte) (int, error) {
	d.lines = append(d.lines, string(p))
	d.log.add("diagnostics line")
	return len(p), nil
}

const (
	completedReply = `{"type":"message","model":"engine","stop_reason":"end_turn","content":[],"usage":{"input_tokens":2950,"output_tokens":4}}`
	emptyReply     = `{"message":"Codex completed without producing output"}`
)

// TestEachClientOutputChecked checks that the client is checked before each output operation on
// its own: a client that leaves inside Header() gets no WriteHeader and no Write, and the Result
// carries only the facts from before the reply; a client that leaves inside WriteHeader gets no
// body Write, and the Result reports the head that was committed with the status and status
// override the same reply reports to a client that stays. Neither gets the empty-completion line,
// which follows only a reply that was written.
func TestEachClientOutputChecked(t *testing.T) {
	type shape struct {
		provider string
		sentryOn bool
		// refused makes the upstream unreachable, so the reply is the gateway's transport error.
		refused bool
		status  int
		// length declares a longer body than the upstream sends, so the read ends early.
		length   int
		body     string
		wantHead int
		wantLine bool
	}
	shapes := map[string]shape{
		"JSON reply": {
			provider: "codex", status: http.StatusOK, body: completedReply, wantHead: http.StatusOK,
		},
		"relayed error reply": {
			provider: "codex", status: http.StatusInternalServerError, body: `{"error":{"type":"api_error","message":"upstream failed"}}`,
			wantHead: http.StatusInternalServerError,
		},
		"early end error reply": {
			provider: "codex", status: http.StatusInternalServerError, length: 100, body: "truncated", wantHead: http.StatusBadGateway,
		},
		"transport error reply": {
			provider: "codex", refused: true, wantHead: http.StatusBadGateway,
		},
		"engine overflow reply": {
			provider: "local", sentryOn: true, status: http.StatusBadRequest,
			body:     `{"error":{"type":"exceed_context_size_error","n_prompt_tokens":3100,"n_ctx":3000,"message":"backend detail"}}`,
			wantHead: http.StatusRequestEntityTooLarge,
		},
		"empty completion reply": {
			provider: "codex", status: http.StatusServiceUnavailable, body: emptyReply, wantHead: http.StatusOK, wantLine: true,
		},
	}
	type run struct {
		w      *clientWriter
		result reply.Result
		err    error
		lines  []string
		left   bool
	}
	do := func(t *testing.T, sh shape, leaveIn string) run {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if sh.length != 0 {
				w.Header().Set("Content-Length", strconv.Itoa(sh.length))
			}
			w.WriteHeader(sh.status)
			_, _ = io.WriteString(w, sh.body)
		})
		if sh.refused {
			fixture.server.Close()
		}
		sink := &loggedDiagnostics{}
		req := reply.Request{
			Provider: sh.provider, AdvertisedModel: "shown", BackendModel: "engine", Session: factsSession, Body: []byte(factsRequest),
			SentryEnabled: sh.sentryOn, Window: 3000, Target: factsTarget, Sentry: new(sentry.State), Usage: newCapture(),
			NewMessageID: func() string { return "msg_fixed" },
		}
		req = withDiagnostics(req, sink)
		w := &clientWriter{ResponseRecorder: httptest.NewRecorder(), leaveIn: leaveIn, leave: cancel}
		result, err := reply.Handle(ctx, w, req, fixture.forward(req.Body))
		if result.Stream != nil {
			_ = result.Stream.Body.Close()
		}
		return run{w: w, result: result, err: err, lines: sink.lines, left: ctx.Err() != nil}
	}
	points := map[string]string{
		"error: client leaves inside Header":      "Header",
		"error: client leaves inside WriteHeader": "WriteHeader",
	}
	for name, sh := range shapes {
		t.Run(name, func(t *testing.T) {
			// The client that stays is the control: it shows the head and the Result this reply
			// produces when nothing interrupts it.
			live := do(t, sh, "")
			if live.err != nil {
				t.Fatalf("Handle for a client that stays: %v", live.err)
			}
			if live.result.Stream != nil {
				t.Fatal("a buffered reply was handed on as a stream")
			}
			equal(t, []int{sh.wantHead}, live.w.heads)
			if len(live.w.writes) == 0 || live.w.Body.Len() == 0 {
				t.Fatalf("client that stays got no body: writes %q", live.w.writes)
			}
			if sh.wantLine {
				if len(live.lines) != 1 {
					t.Fatalf("diagnostic lines for a client that stays = %q, want one", live.lines)
				}
				checkOneLine(t, live.lines[0])
			} else {
				equal(t, []string(nil), live.lines)
			}

			for label, leaveIn := range points {
				t.Run(label, func(t *testing.T) {
					got := do(t, sh, leaveIn)
					if !got.left || got.w.headerCalls == 0 {
						t.Fatalf("the client never left inside %s: left %v, Header calls %d", leaveIn, got.left, got.w.headerCalls)
					}
					if got.err != nil {
						t.Errorf("Handle error = %v, want nil", got.err)
					}
					if got.result.Stream != nil {
						t.Error("a buffered reply for a departed client handed a stream on")
					}
					equal(t, []string(nil), got.w.writes)
					equal(t, "", got.w.Body.String())
					equal(t, []string(nil), got.lines)
					equal(t, true, got.result.Contacted)
					equal(t, false, got.result.Cancelled)
					switch leaveIn {
					case "Header":
						equal(t, []int(nil), got.w.heads)
						equal(t, 0, got.result.Status)
						equal(t, "", got.result.StatusOverride)
					case "WriteHeader":
						equal(t, live.w.heads, got.w.heads)
						equal(t, live.result.Status, got.result.Status)
						equal(t, live.result.StatusOverride, got.result.StatusOverride)
					}
				})
			}
		})
	}
}

// TestEmptyCompletionLineFollowsReply checks that the empty-completion line reaches the
// diagnostics writer only after the empty reply's head and bytes reached the client, in either
// framing, and that a reply whose body write fails gets no line.
func TestEmptyCompletionLineFollowsReply(t *testing.T) {
	tests := map[string]struct {
		stream    bool
		failWrite bool
		want      []string
	}{
		"success: line follows a JSON reply": {
			want: []string{"client head 200", "client body", "diagnostics line"},
		},
		"success: line follows an event stream reply": {
			stream: true, want: []string{"client head 200", "client body", "diagnostics line"},
		},
		"error: no line after a failed JSON write": {
			failWrite: true, want: []string{"client head 200", "client body"},
		},
		"error: no line after a failed event stream write": {
			stream: true, failWrite: true, want: []string{"client head 200", "client body"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			sink := &loggedDiagnostics{log: log}
			req := reply.Request{
				Provider: "codex", AdvertisedModel: "shown", BackendModel: "engine",
				Body:         []byte(`{"stream":` + strconv.FormatBool(tc.stream) + `,"messages":[]}`),
				NewMessageID: func() string { return "msg_fixed" },
			}
			req = withDiagnostics(req, sink)
			fixture := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, emptyReply)
			})
			w := &clientWriter{ResponseRecorder: httptest.NewRecorder(), failWrite: tc.failWrite, log: log}
			result, err := reply.Handle(t.Context(), w, req, fixture.forward(req.Body))
			if result.Stream != nil {
				_ = result.Stream.Body.Close()
				t.Fatal("an empty reply was handed on as a stream")
			}
			if !tc.failWrite && err != nil {
				t.Fatalf("Handle: %v", err)
			}
			equal(t, tc.want, log.events)
			if len(w.writes) != 1 || w.writes[0] == "" {
				t.Fatalf("client body writes = %q, want one non-empty write", w.writes)
			}
			wantType := "application/json"
			if tc.stream {
				wantType = "text/event-stream"
			}
			equal(t, wantType, w.Header().Get("Content-Type"))
			if !tc.failWrite {
				equal(t, 1, len(sink.lines))
				checkOneLine(t, strings.Join(sink.lines, ""))
			}
		})
	}
}

// TestCompletedJSONObservedOnce checks that a complete JSON body is observed exactly once, by the
// usage capture and by the sentry with its rearming of a fired refusal, whatever happens to the
// client: it stays, it leaves when the send returns, it leaves while the body is read, or its
// body write fails.
func TestCompletedJSONObservedOnce(t *testing.T) {
	tests := map[string]struct {
		mode      departure
		failWrite bool
		wantHeads []int
	}{
		"success: client stays":                        {mode: stays, wantHeads: []int{http.StatusOK}},
		"success: client leaves when the send returns": {mode: leavesAtSend},
		"success: client leaves during the read":       {mode: leavesInRead},
		"success: client body write fails":             {mode: stays, failWrite: true, wantHeads: []int{http.StatusOK}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			state := new(sentry.State)
			primeCompletion(state)
			capture := &recordingCapture{Capture: newCapture()}
			req := reply.Request{
				Provider: "codex", AdvertisedModel: "shown", BackendModel: "engine", Session: factsSession, Body: []byte(factsRequest),
				SentryEnabled: true, Window: 3000, Target: factsTarget, Sentry: state, Usage: capture,
			}
			held := newHeldUpstream(t, heldReply{http.StatusOK, http.Header{"Content-Type": {"application/json"}}, completedReply})
			w := &clientWriter{ResponseRecorder: httptest.NewRecorder(), failWrite: tc.failWrite}
			result, err := reply.Handle(ctx, w, req, held.send(req.Body, tc.mode, cancel))
			if result.Stream != nil {
				_ = result.Stream.Body.Close()
				t.Fatal("a buffered reply was handed on as a stream")
			}
			equal(t, tc.mode != stays, ctx.Err() != nil)
			if tc.mode != stays || !tc.failWrite {
				if err != nil {
					t.Errorf("Handle error = %v, want nil", err)
				}
			}
			equal(t, tc.wantHeads, w.heads)
			if tc.wantHeads == nil {
				equal(t, []string(nil), w.writes)
			} else if len(w.writes) != 1 {
				t.Errorf("client body writes = %q, want one", w.writes)
			}

			// The sentry's own calls cannot be counted from outside; its rearming and the usage it
			// recorded are the effect of the observation, and the capture counts the calls.
			equal[any](t, completionProbe{Rearmed: true, Usage: 2950}, probeCompletion(state))
			equal(t, []string{completedReply}, capture.observed)
			equal(t, int64(len(completedReply)), capture.bytes)
			equal(t, []int{http.StatusOK}, capture.statuses)
			snapshot := capture.Snapshot()
			equal[any](t, float64(2950), snapshot["input_tokens"])
			equal[any](t, float64(4), snapshot["output_tokens"])
			equal(t, int32(1), held.attempts.Load())
		})
	}
}
