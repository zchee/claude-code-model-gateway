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
	"encoding/json/jsontext"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// readSignal reports the first Read call on an upstream body, whether or not it returns bytes, and
// counts every call.
type readSignal struct {
	io.ReadCloser
	calls  atomic.Int32
	called func()
}

func (b *readSignal) Read(p []byte) (int, error) {
	b.calls.Add(1)
	b.called()
	return b.ReadCloser.Read(p)
}

// checkNothingWritten checks the result Handle returns for a client that left: no error, no stream,
// nothing written after the status lines in pre, status zero, the upstream contacted, and
// Cancelled left to the caller.
func checkNothingWritten(t *testing.T, w *countedWriter, pre []int, result reply.Result, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("Handle error = %v, want nil", err)
	}
	if result.Stream != nil {
		_ = result.Stream.Body.Close()
		t.Error("a buffered reply for a departed client handed a stream on")
	}
	equal(t, pre, w.heads)
	equal(t, "", w.Body.String())
	equal(t, 0, result.Status)
	equal(t, "", result.StatusOverride)
	equal(t, true, result.Contacted)
	equal(t, false, result.Cancelled)
}

// TestDepartedClientRelayResponse checks that a response the relay would own is handed to it even
// when the client left while the upstream was answering: Handle returns the open stream as for a
// live client, writes nothing, and neither reads nor closes the body. The upstream holds the tail
// of its body until Handle has returned, so a Handle that read the body before deciding would be
// seen reading, and one that closed it would cut the tail off.
func TestDepartedClientRelayResponse(t *testing.T) {
	tests := map[string]struct {
		provider, model string
		headWritten     bool
		status          int
		contentType     string
		head, tail      string
		length          bool
	}{
		"success: proxy event stream": {
			provider: "codex", model: "shown", status: http.StatusOK, contentType: "text/event-stream",
			head: "event: message_start\ndata: {\"type\":\"message_start\"}\n\n", tail: "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		"success: native Anthropic JSON": {
			status: http.StatusOK, contentType: "application/json",
			head: `{"type":"message","content":[`, tail: `],"stop_reason":"end_turn"}`, length: true,
		},
		"error: native Anthropic refusal": {
			status: http.StatusRequestEntityTooLarge, contentType: "application/json",
			head: `{"type":"error","error":{"type":"request_too_large",`, tail: `"message":"context window"}}`, length: true,
		},
		"success: proxy redirect": {
			provider: "codex", model: "shown", status: http.StatusTemporaryRedirect, contentType: "application/octet-stream",
			head: "\x00\x01\x02", tail: "\xfd\xfe\xff", length: true,
		},
		"success: committed compaction stream": {
			provider: "codex", model: "shown", headWritten: true, status: http.StatusOK, contentType: "text/event-stream",
			head: "event: message_start\ndata: {}\n\n", tail: "event: message_stop\ndata: {}\n\n",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			release, handlerDone := make(chan struct{}), make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			defer releaseOnce()
			var tailSent atomic.Bool
			fixture := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", tc.contentType)
				if tc.length {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.head)+len(tc.tail)))
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.head)
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Errorf("flush upstream head: %v", err)
				}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				tailSent.Store(true)
				_, _ = io.WriteString(w, tc.tail)
			})

			readCalled := make(chan struct{})
			var (
				observed *observedBody
				signal   *readSignal
			)
			req := reply.Request{Provider: tc.provider, AdvertisedModel: tc.model, BackendModel: "engine", Body: []byte(`{"messages":[]}`), Compaction: tc.headWritten, HeadWritten: tc.headWritten, SentryEnabled: true}
			base := fixture.forward(req.Body)
			send := func(callCtx context.Context) (*upstream.Response, error) {
				res, err := base(callCtx)
				if err != nil {
					return res, err
				}
				observed, _ = res.Body.(*observedBody)
				signal = &readSignal{ReadCloser: res.Body, called: sync.OnceFunc(func() { close(readCalled) })}
				res.Body = signal
				// The client leaves while the send is returning, so Handle sees the departure
				// as soon as it has the response.
				cancel()
				return res, nil
			}

			w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
			var pre []int
			if tc.headWritten {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				pre = []int{http.StatusOK}
			}
			var result reply.Result
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				result, err = reply.Handle(ctx, w, req, send)
			}()
			select {
			case <-done:
			case <-readCalled:
				releaseOnce()
				<-done
				if result.Stream != nil {
					_ = result.Stream.Body.Close()
				}
				t.Fatal("Handle read a relay-owned body for a departed client before handing it on")
			}
			// Handle returned while the upstream still held the tail.
			equal(t, false, tailSent.Load())

			if err != nil {
				t.Errorf("Handle error = %v, want nil", err)
			}
			equal(t, pre, w.heads)
			equal(t, "", w.Body.String())
			equal(t, true, result.Contacted)
			equal(t, false, result.Cancelled)
			if observed == nil {
				t.Fatal("upstream body was not observed")
			}
			equal(t, int32(0), signal.calls.Load())
			equal(t, int32(0), observed.closes.Load())
			if result.Stream == nil {
				t.Fatal("a relay-owned response for a departed client was not handed on")
			}
			equal(t, tc.status, result.Stream.StatusCode)
			equal(t, tc.contentType, result.Stream.Header.Get("Content-Type"))

			// The stream is intact: the head is there to read, and the tail arrives once the
			// upstream sends it.
			head := make([]byte, len(tc.head))
			if _, err := io.ReadFull(result.Stream.Body, head); err != nil {
				t.Fatalf("read handed-on head: %v", err)
			}
			releaseOnce()
			tail, err := io.ReadAll(result.Stream.Body)
			if err != nil {
				t.Fatalf("read handed-on tail: %v", err)
			}
			equal(t, tc.head+tc.tail, string(head)+string(tail))
			if err := result.Stream.Body.Close(); err != nil {
				t.Errorf("close handed-on stream: %v", err)
			}
			<-handlerDone
		})
	}
}

// heldUpstream answers every attempt with the same reply in two parts: the head at once, the tail
// only when the test releases it. Each attempt publishes its release function before it answers.
type heldUpstream struct {
	fixture  proxyFixture
	releases chan func()
	attempts atomic.Int32
}

type heldReply struct {
	status int
	header http.Header
	body   string
}

func newHeldUpstream(t *testing.T, up heldReply) *heldUpstream {
	t.Helper()
	h := &heldUpstream{releases: make(chan func(), 8)}
	h.fixture = newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		h.attempts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		release := make(chan struct{})
		h.releases <- sync.OnceFunc(func() { close(release) })
		for name, values := range up.header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(up.body)))
		w.WriteHeader(up.status)
		split := len(up.body) / 2
		_, _ = io.WriteString(w, up.body[:split])
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush upstream head: %v", err)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, up.body[split:])
	})
	return h
}

// departure says when the client leaves during a reply.
type departure int

const (
	stays departure = iota
	// leavesAtSend ends the client context inside the send, so Handle sees it when the send returns.
	leavesAtSend
	// leavesInRead ends the client context once Handle has read the head of the body, before the
	// upstream sends its tail.
	leavesInRead
)

// send returns a Send that releases each attempt's tail once Handle reads the head of its body,
// and ends the client context with leave at the point mode names.
func (h *heldUpstream) send(body []byte, mode departure, leave func()) upstream.Send {
	base := h.fixture.forward(body)
	return func(ctx context.Context) (*upstream.Response, error) {
		res, err := base(ctx)
		if err != nil {
			return res, err
		}
		release := <-h.releases
		onHead := release
		switch mode {
		case leavesAtSend:
			leave()
		case leavesInRead:
			onHead = func() {
				leave()
				release()
			}
		}
		res.Body = &watchedBody{ReadCloser: res.Body, firstBytes: sync.OnceFunc(onHead)}
		return res, nil
	}
}

// recordingCapture is the real usage capture with a record of SetResponseComplete calls. When
// leaveAt names one of its methods, that method ends the client context with leave after the
// real capture has run.
type recordingCapture struct {
	*usage.Capture
	leaveAt  string
	leave    func()
	complete []bool
}

func (c *recordingCapture) called(method string) {
	if c.leaveAt == method {
		c.leave()
	}
}

func (c *recordingCapture) SetResponse(status int, headers http.Header) {
	c.Capture.SetResponse(status, headers)
	c.called("SetResponse")
}

func (c *recordingCapture) NoteResponseBytes(n int64) {
	c.Capture.NoteResponseBytes(n)
	c.called("NoteResponseBytes")
}

func (c *recordingCapture) ObserveJSON(body []byte) {
	c.Capture.ObserveJSON(body)
	c.called("ObserveJSON")
}

func (c *recordingCapture) SetResponseComplete(complete bool) {
	c.Capture.SetResponseComplete(complete)
	c.complete = append(c.complete, complete)
	c.called("SetResponseComplete")
}

// receivedFacts are the effects of one reply that record what the upstream sent.
type receivedFacts struct {
	Blocked  *upstreamstate.Blocked
	Lines    string
	Attempts int32
	Sentry   any
	Usage    map[string]any
}

// overflowProbe reports, after a refusal, whether the refused session's latch holds against a
// usage above every trigger, and which trigger another session of the model now crosses (zero
// when that session, measured between the learned and the configured trigger, does not cross).
type overflowProbe struct {
	Latched bool
	Learned float64
}

// completionProbe reports, after a successful reply on a latched session, whether the session
// crosses its trigger again (the reply rearmed the latch as a delivered completion) and the usage
// the reply recorded for it.
type completionProbe struct {
	Rearmed bool
	Usage   float64
}

const (
	factsSession = "only"
	factsRequest = `{"messages":[{"role":"user","content":"hello"}]}`
)

var factsTarget = sentry.Target{Key: "engine", Policy: &policy.Sentry{BackendWindow: 3000, CompactTrigger: 2900, Source: "derived"}, Headroom: 100}

func observeUsage(s *sentry.State, session string, tokens int) {
	s.Observe(session, factsTarget, jsontext.Value(fmt.Sprintf(`{"type":"message_delta","usage":{"input_tokens":%d}}`, tokens)), false)
}

// primeOverflow measures 2500 tokens for the session before an overflow, so learning sets the
// derived trigger to 2400.
func primeOverflow(s *sentry.State) { observeUsage(s, factsSession, 2500) }

func probeOverflow(s *sentry.State) any {
	request := []byte(factsRequest)
	observeUsage(s, factsSession, 2950)
	_, crossed := s.Check(factsSession, factsTarget, request)
	observeUsage(s, "other", 2450)
	var learned float64
	if crossing, ok := s.Check("other", factsTarget, request); ok {
		learned = crossing.Trigger
	}
	return overflowProbe{Latched: !crossed, Learned: learned}
}

// primeCompletion latches the session's refusal with no measured usage.
func primeCompletion(s *sentry.State) { s.Overflow(factsSession, factsTarget, false) }

func probeCompletion(s *sentry.State) any {
	_, rearmed := s.Check(factsSession, factsTarget, []byte(factsRequest))
	return completionProbe{Rearmed: rearmed, Usage: s.Overflow(factsSession, factsTarget, false)}
}

// TestDepartedClientRecordsReceivedFacts checks that what the upstream sent is recorded whether or
// not the client is still there: the Codex rejection's upstream state and its diagnostic line, the
// overflow latch and the learned trigger, and the sentry's and the usage capture's observation of
// a complete JSON body. Each scenario runs for a client that stays, one that leaves when the send
// returns, and one that leaves while the body is being read; the departed runs write nothing to
// the client and record the same facts as the run that stays, except what only a delivered reply
// does: no retry, no rearming as a delivered completion, no line for an empty reply never sent.
func TestDepartedClientRecordsReceivedFacts(t *testing.T) {
	openAI := http.Header{"Content-Type": {"application/json"}, "X-Openai-Request-Id": {"req-1"}, "Retry-After": {"30"}}
	const (
		refused    = `{"error":{"type":"permission_error","message":"request refused"}}`
		upgrade    = `{"error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`
		genuine    = `{"type":"error","error":{"type":"request_too_large","message":"upstream wording (3100 tokens > 3000 tokens)"}}`
		engineOwn  = `{"error":{"type":"exceed_context_size_error","n_prompt_tokens":3100,"n_ctx":3000,"message":"backend detail"}}`
		engineBare = `{"error":{"type":"exceed_context_size_error","n_prompt_tokens":3100,"message":"backend detail"}}`
		legacy     = `{"error":{"message":"backend detail: context length exceeded"}}`
		completed  = `{"type":"message","model":"engine","stop_reason":"end_turn","content":[],"usage":{"input_tokens":2950,"output_tokens":4}}`
		empty      = `{"message":"Codex completed without producing output"}`
	)
	latchedAndLearned := overflowProbe{Latched: true, Learned: 2400}
	tests := map[string]struct {
		provider    string
		headWritten bool
		sentryOn    bool
		unwritable  bool
		up          heldReply
		prime       func(*sentry.State)
		probe       func(*sentry.State) any
		// want holds the facts of the client that stays, apart from Lines and Usage.
		want      receivedFacts
		wantLines int
		wantCode  int
		wantUsage map[string]any
		// departed adjusts the facts of the client that stays into those of a departed client.
		departed func(*receivedFacts)
	}{
		"error: attributed Codex forbidden": {
			provider: "codex", up: heldReply{http.StatusForbidden, openAI, refused},
			want:      receivedFacts{Blocked: &upstreamstate.Blocked{StatusCode: http.StatusForbidden}, Attempts: 1},
			wantLines: 1, wantCode: http.StatusForbidden,
		},
		"error: attributed Codex unauthorized": {
			provider: "codex", up: heldReply{http.StatusUnauthorized, openAI, refused},
			want:      receivedFacts{Blocked: &upstreamstate.Blocked{StatusCode: http.StatusUnauthorized}, Attempts: 1},
			wantLines: 1, wantCode: http.StatusUnauthorized,
		},
		"error: attributed Codex rate limit": {
			provider: "codex", up: heldReply{http.StatusTooManyRequests, openAI, refused},
			want:      receivedFacts{Blocked: &upstreamstate.Blocked{StatusCode: http.StatusTooManyRequests, ExpiresAtMs: new(float64(1_030_000))}, Attempts: 1},
			wantLines: 1, wantCode: http.StatusTooManyRequests,
		},
		"error: attributed Codex rate limit in a committed compaction": {
			provider: "codex", headWritten: true, up: heldReply{http.StatusTooManyRequests, openAI, refused},
			want:      receivedFacts{Blocked: &upstreamstate.Blocked{StatusCode: http.StatusTooManyRequests, ExpiresAtMs: new(float64(1_030_000))}, Attempts: 1},
			wantLines: 1,
		},
		"error: attributed Codex forbidden with unwritable state": {
			provider: "codex", unwritable: true, up: heldReply{http.StatusForbidden, openAI, refused},
			want:      receivedFacts{Attempts: 1},
			wantLines: 1, wantCode: http.StatusForbidden,
		},
		"error: attributed upgrade rejection is no block": {
			provider: "codex", up: heldReply{http.StatusForbidden, openAI, upgrade},
			want:      receivedFacts{Attempts: 3},
			wantLines: 0, wantCode: http.StatusServiceUnavailable,
			departed: func(f *receivedFacts) { f.Attempts = 1 },
		},
		"error: genuine overflow with counts": {
			provider: "local", sentryOn: true, up: heldReply{http.StatusRequestEntityTooLarge, http.Header{"Content-Type": {"application/json"}}, genuine},
			prime: primeOverflow, probe: probeOverflow,
			want:     receivedFacts{Attempts: 1, Sentry: latchedAndLearned},
			wantCode: http.StatusRequestEntityTooLarge,
		},
		"error: engine overflow with its own limit": {
			provider: "local", sentryOn: true, up: heldReply{http.StatusBadRequest, http.Header{"Content-Type": {"application/json"}}, engineOwn},
			prime: primeOverflow, probe: probeOverflow,
			want:     receivedFacts{Attempts: 1, Sentry: overflowProbe{Latched: true}},
			wantCode: http.StatusRequestEntityTooLarge,
		},
		"error: engine overflow without its limit": {
			provider: "local", sentryOn: true, up: heldReply{http.StatusInternalServerError, http.Header{"Content-Type": {"application/json"}}, engineBare},
			prime: primeOverflow, probe: probeOverflow,
			want:     receivedFacts{Attempts: 1, Sentry: latchedAndLearned},
			wantCode: http.StatusRequestEntityTooLarge,
		},
		"error: legacy overflow": {
			provider: "local", sentryOn: true, up: heldReply{http.StatusBadGateway, http.Header{"Content-Type": {"application/json"}}, legacy},
			prime: primeOverflow, probe: probeOverflow,
			want:     receivedFacts{Attempts: 1, Sentry: latchedAndLearned},
			wantCode: http.StatusRequestEntityTooLarge,
		},
		"error: genuine overflow in a committed compaction": {
			provider: "local", sentryOn: true, headWritten: true, up: heldReply{http.StatusRequestEntityTooLarge, http.Header{"Content-Type": {"application/json"}}, genuine},
			prime: primeOverflow, probe: probeOverflow,
			want: receivedFacts{Attempts: 1, Sentry: latchedAndLearned},
		},
		"success: completed JSON is observed": {
			provider: "codex", sentryOn: true, up: heldReply{http.StatusOK, http.Header{"Content-Type": {"application/json"}}, completed},
			prime: primeCompletion, probe: probeCompletion,
			want:      receivedFacts{Attempts: 1, Sentry: completionProbe{Rearmed: true, Usage: 2950}},
			wantCode:  http.StatusOK,
			wantUsage: map[string]any{"input_tokens": float64(2950), "output_tokens": float64(4)},
			departed:  func(f *receivedFacts) { f.Sentry = completionProbe{Rearmed: false, Usage: 2950} },
		},
		"success: empty Codex completion": {
			provider: "codex", up: heldReply{http.StatusServiceUnavailable, http.Header{"Content-Type": {"application/json"}}, empty},
			want:      receivedFacts{Attempts: 1},
			wantLines: 1, wantCode: http.StatusOK,
			departed: func(f *receivedFacts) { f.Lines = "" },
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			type run struct {
				facts    receivedFacts
				w        *countedWriter
				pre      []int
				result   reply.Result
				err      error
				complete []bool
				left     bool
			}
			do := func(t *testing.T, mode departure) run {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				dir := t.TempDir()
				if tc.unwritable {
					blocker := filepath.Join(dir, "state-is-a-file")
					if err := os.WriteFile(blocker, nil, 0o600); err != nil {
						t.Fatal(err)
					}
					dir = filepath.Join(blocker, "codex")
				}
				store := &upstreamstate.Store{Dir: dir, Now: func() time.Time { return time.Unix(1000, 0) }}
				state := new(sentry.State)
				if tc.prime != nil {
					tc.prime(state)
				}
				capture := &recordingCapture{Capture: newCapture()}
				var sink strings.Builder
				req := reply.Request{
					Provider: tc.provider, AdvertisedModel: "shown", BackendModel: "engine", Session: factsSession, Body: []byte(factsRequest),
					Compaction: tc.headWritten, HeadWritten: tc.headWritten, SentryEnabled: tc.sentryOn, Window: 3000,
					Target: factsTarget, Sentry: state, Upstream: store, RetryDelay: new(time.Duration(0)), Usage: capture,
				}
				req = withDiagnostics(req, &sink)
				held := newHeldUpstream(t, tc.up)
				w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
				var pre []int
				if tc.headWritten {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					pre = []int{http.StatusOK}
				}
				result, err := reply.Handle(ctx, w, req, held.send(req.Body, mode, cancel))
				if result.Stream != nil {
					_ = result.Stream.Body.Close()
				}
				facts := receivedFacts{Blocked: store.ReadBlocked(), Lines: sink.String(), Attempts: held.attempts.Load(), Usage: capture.Snapshot()}
				if tc.probe != nil {
					facts.Sentry = tc.probe(state)
				}
				return run{facts: facts, w: w, pre: pre, result: result, err: err, complete: capture.complete, left: ctx.Err() != nil}
			}

			live := do(t, stays)
			if live.err != nil {
				t.Fatalf("Handle for a client that stays: %v", live.err)
			}
			if live.result.Stream != nil {
				t.Fatal("a buffered reply was handed on as a stream")
			}
			if tc.headWritten {
				equal(t, live.pre, live.w.heads)
				if !strings.HasPrefix(live.w.Body.String(), "event: error\n") {
					t.Errorf("committed reply for a client that stays = %q, want an in-band error event", live.w.Body.String())
				}
			} else {
				equal(t, []int{tc.wantCode}, live.w.heads)
			}
			// The client that stays is the control: its facts must be the expected ones, so that
			// matching them is evidence.
			gotBlock := live.facts.Blocked
			if (gotBlock != nil) != (tc.want.Blocked != nil) {
				t.Fatalf("upstream state for a client that stays = %+v, want present=%v", gotBlock, tc.want.Blocked != nil)
			}
			if gotBlock != nil {
				equal(t, tc.want.Blocked.StatusCode, gotBlock.StatusCode)
				equal(t, tc.want.Blocked.ExpiresAtMs, gotBlock.ExpiresAtMs)
			}
			equal(t, tc.want.Attempts, live.facts.Attempts)
			equal(t, tc.want.Sentry, live.facts.Sentry)
			equal(t, tc.wantLines, strings.Count(live.facts.Lines, "\n"))
			if tc.wantLines == 1 {
				checkOneLine(t, live.facts.Lines)
			}
			for key, value := range tc.wantUsage {
				equal(t, value, live.facts.Usage[key])
			}

			for mode, label := range map[departure]string{leavesAtSend: "error: client leaves when the send returns", leavesInRead: "error: client leaves during the read"} {
				t.Run(label, func(t *testing.T) {
					got := do(t, mode)
					if !got.left {
						t.Fatal("the client did not leave")
					}
					checkNothingWritten(t, got.w, got.pre, got.result, got.err)
					want := live.facts
					if tc.departed != nil {
						tc.departed(&want)
					}
					if diff := gocmp.Diff(want, got.facts); diff != "" {
						t.Errorf("received facts of a departed client (-want +got):\n%s", diff)
					}
					if slices.Contains(got.complete, true) {
						t.Errorf("a departed client's reply was marked complete: SetResponseComplete calls %v", got.complete)
					}
				})
			}
		})
	}
}

// leavingWriter is a diagnostics sink that ends the client context while it is written. It has
// only a Write method, so every line reaches it through Write.
type leavingWriter struct {
	leave func()
	lines []string
}

func (w *leavingWriter) Write(p []byte) (int, error) {
	w.leave()
	w.lines = append(w.lines, string(p))
	return len(p), nil
}

// TestCallbackEndsClientContext checks that a client which leaves while a callback of the reply
// runs (the diagnostics writer, the upstream state store's clock, or a usage capture
// observation) gets no client write: the context is checked after every callback and immediately
// before the first byte of the response.
func TestCallbackEndsClientContext(t *testing.T) {
	openAI := http.Header{"Content-Type": {"application/json"}, "X-Openai-Request-Id": {"req-1"}}
	const (
		refused   = `{"error":{"type":"permission_error","message":"request refused"}}`
		upgrade   = `{"error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`
		completed = `{"type":"message","model":"engine","stop_reason":"end_turn","content":[],"usage":{"input_tokens":3,"output_tokens":1}}`
		empty     = `{"message":"Codex completed without producing output"}`
		failed    = `{"error":{"type":"api_error","message":"upstream failed"}}`
	)
	tests := map[string]struct {
		// leaveIn names the callback that ends the client context: "diagnostics", "clock", or
		// a method of the usage capture.
		leaveIn     string
		unwritable  bool
		headWritten bool
		retries     *int
		status      int
		header      http.Header
		body        string
	}{
		"error: diagnostics writer during the rejection line":         {leaveIn: "diagnostics", status: http.StatusForbidden, header: openAI, body: refused},
		"error: diagnostics writer during the state failure line":     {leaveIn: "diagnostics", unwritable: true, status: http.StatusUnauthorized, header: openAI, body: refused},
		"error: diagnostics writer during the empty completion line":  {leaveIn: "diagnostics", status: http.StatusServiceUnavailable, body: empty},
		"error: diagnostics writer during a committed rejection line": {leaveIn: "diagnostics", headWritten: true, status: http.StatusTooManyRequests, header: openAI, body: refused},
		"error: state clock during the rate limit write":              {leaveIn: "clock", status: http.StatusTooManyRequests, header: openAI, body: refused},
		"error: state clock during a committed rejection write":       {leaveIn: "clock", headWritten: true, status: http.StatusForbidden, header: openAI, body: refused},
		"error: capture observing the JSON body":                      {leaveIn: "ObserveJSON", status: http.StatusOK, body: completed},
		"error: capture recording a forbidden response":               {leaveIn: "SetResponse", status: http.StatusForbidden, body: refused},
		"error: capture recording an exhausted upgrade":               {leaveIn: "SetResponse", retries: new(0), status: http.StatusForbidden, body: upgrade},
		"error: capture marking a committed reply incomplete":         {leaveIn: "SetResponseComplete", headWritten: true, status: http.StatusInternalServerError, body: failed},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var clockCalls atomic.Int32
			dir := t.TempDir()
			if tc.unwritable {
				blocker := filepath.Join(dir, "state-is-a-file")
				if err := os.WriteFile(blocker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(blocker, "codex")
			}
			store := &upstreamstate.Store{Dir: dir, Now: func() time.Time {
				if tc.leaveIn == "clock" {
					clockCalls.Add(1)
					cancel()
				}
				return time.Unix(1000, 0)
			}}
			sink := &leavingWriter{leave: func() {}}
			if tc.leaveIn == "diagnostics" {
				sink.leave = cancel
			}
			capture := &recordingCapture{Capture: newCapture(), leaveAt: tc.leaveIn, leave: cancel}
			req := reply.Request{
				Provider: "codex", AdvertisedModel: "shown", BackendModel: "engine", Body: []byte(`{"messages":[]}`),
				Compaction: tc.headWritten, HeadWritten: tc.headWritten, Upstream: store, Retries: tc.retries, Usage: capture,
			}
			req = withDiagnostics(req, sink)
			fixture := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				maps.Copy(w.Header(), tc.header)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
			var pre []int
			if tc.headWritten {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				pre = []int{http.StatusOK}
			}
			result, err := reply.Handle(ctx, w, req, fixture.forward(req.Body))
			if ctx.Err() == nil {
				t.Fatalf("the %s callback never ran, so the client never left", tc.leaveIn)
			}
			if tc.leaveIn == "clock" && clockCalls.Load() == 0 {
				t.Fatal("the upstream state was never written")
			}
			checkNothingWritten(t, w, pre, result, err)
		})
	}
}
