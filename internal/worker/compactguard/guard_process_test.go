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

package compactguard_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const (
	missingTerminal = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"websocket_missing_terminal\"}}\n\n"
	engineOverflow  = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":6017,\"n_ctx\":4096}}\n\n"
	fatalFrame      = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages: required\"}}\n\n"
)

func incompleteFrame(provider string, n int) string {
	return fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"model-gateway: the %s compaction stream ended without a completed generation after %d attempt(s); the summary above is incomplete\"}}\n\n", provider, n)
}

// Count only calls at the capture boundary; the real capture interprets the events.
type observedCapture struct {
	*usage.Capture
	raw      int64
	resets   int
	complete []bool
}

func (c *observedCapture) NoteResponseBytes(n int64) { c.raw += n; c.Capture.NoteResponseBytes(n) }
func (c *observedCapture) ResetUsage()               { c.resets++; c.Capture.ResetUsage() }
func (c *observedCapture) SetResponseComplete(b bool) {
	c.complete = append(c.complete, b)
	c.Capture.SetResponseComplete(b)
}

type countedBody struct {
	io.ReadCloser
	closes atomic.Int32
}

func (b *countedBody) Close() error { b.closes.Add(1); return b.ReadCloser.Close() }

func bindLoopback(t *testing.T, service context.Context, origin string, req *compactguard.Request, before func(bool)) {
	t.Helper()
	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })
	prepared := upstream.Request{Target: upstream.TargetProxy, Method: http.MethodPost, BaseURL: origin, RequestURI: req.PathOnly, Body: req.Body}
	req.UpstreamOrigin = origin
	var bodies []*countedBody
	t.Cleanup(func() {
		for i, b := range bodies {
			if n := b.closes.Load(); n != 1 {
				t.Errorf("attempt %d body closed %d times, want once", i, n)
				if n == 0 {
					_ = b.Close()
				}
			}
		}
	})
	req.Next = func(_ context.Context, head bool) (*upstream.Response, outcome.Result, error) {
		if before != nil {
			before(head)
		}
		resp, err := client.Do(service, prepared)
		if err == nil {
			b := &countedBody{ReadCloser: resp.Body}
			resp.Body = b
			bodies = append(bodies, b)
		}
		return resp, outcome.Result{Contacted: true}, err
	}
}

func TestGuardRetryAndDelivery(t *testing.T) {
	success := completeStream("summary")
	first := truncatedStream() + ":" + strings.Repeat("x", 1024) + "\n\n"
	unknown := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"private detail < & >\"}}\n\n"
	firstUsage := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":17,\"cache_read_input_tokens\":3}}}\n\n"
	lastUsage := "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\n\n"
	tests := map[string]struct {
		attempts                              []string
		retries                               int
		cap                                   int
		body, label, code, terminal, override string
		input, output, cache                  *float64
		abort, warn                           bool
	}{
		"success: healthy proxy advertises the client model":                  {attempts: []string{success}, retries: 2, body: delivered(success), label: "completed", terminal: "message_stop"},
		"success: inferred terminal appended once":                            {attempts: []string{strings.TrimSuffix(success, frameMessageStop)}, retries: 2, body: delivered(success), label: "completed"},
		"success: empty summary is complete":                                  {attempts: []string{frameMessageStart + frameMessageStop}, body: advertisedStart + frameMessageStop, label: "empty_summary", terminal: "message_stop"},
		"success: discarded error contributes no usage to a successful retry": {attempts: []string{firstUsage + truncatedStream() + missingTerminal, lastUsage + success}, retries: 2, body: lastUsage + delivered(success), label: "completed", terminal: "message_stop", output: new(0.0)},
		"success: transformed ceiling stays buffered and replacement fits":    {attempts: []string{first, success}, retries: 1, cap: len(delivered(first)), body: delivered(success), label: "completed", terminal: "message_stop"},
		"error: one transformed byte over loses retry cover":                  {attempts: []string{first}, retries: 1, cap: len(delivered(first)) - 1, body: delivered(first) + incompleteFrame("Codex", 1), label: "incomplete", override: "upstream_error", warn: true},
		"error: fatal request is never retried":                               {attempts: []string{frameMessageStart + fatalFrame}, retries: 2, body: advertisedStart + fatalFrame, label: "incomplete", code: "invalid_request_error", override: "upstream_error"},
		"error: exhausted attempts merge nullable measurements":               {attempts: []string{firstUsage + truncatedStream(), lastUsage + frameTextDelta("partial two")}, retries: 1, body: lastUsage + frameTextDelta("partial two") + incompleteFrame("Codex", 2), label: "incomplete", override: "upstream_error", input: new(17.0), cache: new(3.0), output: new(0.0)},
		"error: raw upstream message stays on wire only":                      {attempts: []string{unknown}, body: unknown, label: "unknown_error", code: "unknown_error", override: "upstream_error"},
		"error: interrupted connection after terminal preserves summary":      {attempts: []string{success}, body: delivered(success), label: "completed", terminal: "message_stop", override: "upstream_aborted", abort: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := guardRequest()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				idx := int(calls.Add(1)) - 1
				if idx >= len(tt.attempts) {
					http.Error(w, "unexpected retry", 500)
					return
				}
				b, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(req.Body, b) {
					t.Errorf("prepared request changed: %q, %v", b, err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-First-Attempt", strconv.Itoa(idx))
				if tt.abort {
					w.Header().Set("Content-Length", strconv.Itoa(len(tt.attempts[idx])+1))
				}
				_, _ = io.WriteString(w, tt.attempts[idx])
			}))
			defer server.Close()
			service, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			var heads []bool
			w := &headRecorder{ResponseRecorder: httptest.NewRecorder()}
			bindLoopback(t, service, server.URL, &req, func(head bool) {
				if head != (len(w.heads) > 0) {
					t.Errorf("head=%v at Next, writes=%v", head, w.heads)
				}
				heads = append(heads, head)
			})
			obs := &observedCapture{Capture: usage.NewCapture(usage.NewBuilder(usage.Options{Now: time.Now}), usage.Request{Backend: "codex"}, 0, nil)}
			req.Usage = obs
			r := guardRunner(service, float64(tt.retries))
			var log bytes.Buffer
			r.Stderr = &log
			if tt.cap != 0 {
				r.Settings.MaxBufferedBytes = float64(tt.cap)
			}
			res, err := r.Serve(t.Context(), w, req)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			wantHeads := make([]bool, len(tt.attempts))
			var total int64
			for i, b := range tt.attempts {
				total += int64(len(b))
				wantHeads[i] = i > 0
			}
			if diff := gocmp.Diff(wantHeads, heads); diff != "" {
				t.Error(diff)
			}
			if diff := gocmp.Diff([]int{200}, w.heads); diff != "" {
				t.Error(diff)
			}
			if w.Header().Get("X-First-Attempt") != "0" || w.Header().Get("Content-Length") != "" {
				t.Errorf("unexpected relay headers: %v", w.Header())
			}
			if diff := gocmp.Diff(tt.body, w.Body.String()); diff != "" {
				t.Error(diff)
			}
			wantDiag := &outcome.Compaction{Outcome: tt.label, TerminalCode: tt.terminal, ErrorCode: tt.code, UpstreamStatus: new(200), InputTokens: tt.input, OutputTokens: tt.output, CacheReadTokens: tt.cache}
			if diff := gocmp.Diff(wantDiag, res.Route.Compaction); diff != "" {
				t.Error(diff)
			}
			if !res.Contacted || res.Status != 200 || res.StatusOverride != tt.override {
				t.Errorf("terminal result: %+v", res)
			}
			if obs.raw != total || obs.resets != len(tt.attempts)-1 {
				t.Errorf("capture observed bytes=%d resets=%d; want %d/%d", obs.raw, obs.resets, total, len(tt.attempts)-1)
			}
			if diff := gocmp.Diff([]bool{tt.label == "completed" || tt.label == "empty_summary"}, obs.complete); diff != "" {
				t.Error(diff)
			}
			wantWarn := ""
			if tt.warn {
				wantWarn = fmt.Sprintf("model-gateway: compaction response passed %d buffered bytes; streaming the remainder live without retry cover\n", tt.cap)
			}
			if diff := gocmp.Diff(wantWarn, log.String()); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestGuardLMStudioPeek(t *testing.T) {
	success := completeStream("summary")
	tests := map[string]struct {
		provider                 string
		attempts                 []string
		broken                   bool
		status, peeks, overflows int
		body                     string
	}{
		"success: first overflow precedes commitment":         {"lmstudio", []string{engineOverflow}, false, 413, 1, 1, ""},
		"error: retry overflow remains in-band":               {"lmstudio", []string{truncatedStream(), engineOverflow}, false, 200, 1, 0, engineOverflow},
		"success: clean empty EOF allows retry":               {"lmstudio", []string{"", success}, false, 200, 1, 0, delivered(success)},
		"error: truncated first data event before commitment": {"lmstudio", []string{"data: {"}, true, 502, 1, 0, `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`},
		"success: other provider does not peek":               {"codex", []string{success}, false, 200, 0, 0, delivered(success)},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var count atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				i := int(count.Add(1)) - 1
				if i >= len(tt.attempts) {
					http.Error(w, "unexpected retry", 500)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if tt.broken {
					w.Header().Set("Content-Length", "100")
				}
				_, _ = io.WriteString(w, tt.attempts[i])
			}))
			defer srv.Close()
			service, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			req := guardRequest()
			req.ProxyProvider = tt.provider
			req.Stream.Provider = tt.provider
			w := &headRecorder{ResponseRecorder: httptest.NewRecorder()}
			bindLoopback(t, service, srv.URL, &req, nil)
			var peeks, overflows int
			req.Stream.OnPeek = func() {
				peeks++
				if count.Load() != 1 || len(w.heads) != 0 {
					t.Error("peek did not precede first head")
				}
			}
			req.Stream.OnOverflow = func(rw http.ResponseWriter, status int, c sentry.Counts) (outcome.Result, error) {
				overflows++
				if status != 200 || len(w.heads) != 0 {
					t.Errorf("overflow callback status=%d heads=%v", status, w.heads)
				}
				if diff := gocmp.Diff(sentry.Counts{Tokens: new(6017.0), Limit: new(4096.0)}, c); diff != "" {
					t.Error(diff)
				}
				rw.WriteHeader(413)
				return outcome.Result{Status: 413, Contacted: true}, nil
			}
			res, err := guardRunner(service, 1).Serve(t.Context(), w, req)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if tt.broken && (res.Status != 200 || (res.StatusOverride != "upstream_aborted" && res.StatusOverride != "upstream_error")) {
				t.Errorf("broken peek result: %+v", res)
			}
			if tt.overflows > 0 && res.Status != 413 {
				t.Errorf("overflow result lost: %+v", res)
			}
			if diff := gocmp.Diff([]int{tt.status}, w.heads); diff != "" {
				t.Error(diff)
			}
			if peeks != tt.peeks || overflows != tt.overflows || int(count.Load()) != len(tt.attempts) {
				t.Errorf("peeks=%d overflows=%d attempts=%d", peeks, overflows, count.Load())
			}
			if diff := gocmp.Diff(tt.body, w.Body.String()); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestGuardSentryObservation(t *testing.T) {
	tests := map[string]struct {
		levels         []int
		retry, rearmed bool
	}{"success: discarded attempt rearms on intermediate low usage": {[]int{20, 105}, true, true}, "success: completed guarded high usage does not rearm": {[]int{105}, false, false}, "success: hysteresis preserves latch": {[]int{80}, false, false}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var state sentry.State
			key := sentry.SessionKey(true, "compact", nil)
			target := sentry.Target{Key: backend, Policy: &policy.Sentry{BackendWindow: 200000, CompactTrigger: 100, Source: "env"}, Headroom: 40000}
			ordinary := []byte(`{"model":"claude-gpt-5.6-sol","messages":[]}`)
			high := jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":105}}`)
			state.Observe(key, target, high, true)
			if _, fired := state.Check(key, target, ordinary); !fired {
				t.Fatal("could not latch sentry")
			}
			check := func() {
				state.Observe(key, target, high, false)
				_, fired := state.Check(key, target, ordinary)
				if fired != tt.rearmed {
					t.Errorf("sentry rearmed=%v, want %v", fired, tt.rearmed)
				}
			}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					for _, level := range tt.levels {
						_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":%d}}\n\n", level)
					}
					if tt.retry {
						return
					}
				}
				_, _ = io.WriteString(w, frameMessageStop)
			}))
			defer srv.Close()
			service, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			req := guardRequest()
			req.Stream.Sentry = &state
			req.Stream.SessionKey = key
			req.Stream.SentryTarget = target
			checked := false
			bindLoopback(t, service, srv.URL, &req, func(head bool) {
				if head {
					checked = true
					check()
				}
			})
			_, err := guardRunner(service, 1).Serve(t.Context(), httptest.NewRecorder(), req)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if tt.retry != checked {
				t.Errorf("retry observation made=%v", checked)
			}
			if !tt.retry {
				check()
			}
		})
	}
}

func TestGuardTwentyConcurrentCompactionStreams(t *testing.T) {
	const total = 20
	var seen [total]atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.UnmarshalRead(r.Body, &payload); err != nil || len(payload.Messages) != 1 {
			http.Error(w, "invalid request", 400)
			return
		}
		id, err := strconv.Atoi(strings.TrimPrefix(payload.Messages[0].Content, "compact-"))
		if err != nil || id < 0 || id >= total {
			http.Error(w, "invalid key", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if seen[id].Add(1) == 1 {
			_, _ = io.WriteString(w, truncatedStream())
			if id%2 == 0 {
				_, _ = io.WriteString(w, missingTerminal)
			}
			return
		}
		_, _ = io.WriteString(w, completeStream("summary for "+payload.Messages[0].Content))
	}))
	defer srv.Close()
	service, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	runner := guardRunner(service, 2)
	var wg sync.WaitGroup
	for i := range total {
		key := fmt.Sprintf("compact-%d", i)
		req := guardRequest()
		req.Body = compactRequestBody(backend, key)
		bindLoopback(t, service, srv.URL, &req, nil)
		wg.Go(func() {
			w := httptest.NewRecorder()
			_, err := runner.Serve(t.Context(), w, req)
			if err != nil {
				t.Errorf("%s: %v", key, err)
				return
			}
			if diff := gocmp.Diff(delivered(completeStream("summary for "+key)), w.Body.String()); diff != "" {
				t.Errorf("%s: %s", key, diff)
			}
			var texts []string
			for line := range strings.SplitSeq(w.Body.String(), "\n") {
				if data, ok := strings.CutPrefix(line, "data: "); ok {
					var event struct {
						Type  string `json:"type"`
						Delta struct {
							Text string `json:"text"`
						} `json:"delta"`
					}
					if err := json.Unmarshal([]byte(data), &event); err != nil {
						t.Errorf("%s: %v", key, err)
						return
					}
					if event.Type == "content_block_delta" {
						texts = append(texts, event.Delta.Text)
					}
				}
			}
			if diff := gocmp.Diff([]string{"summary for " + key}, texts); diff != "" {
				t.Errorf("%s: %s", key, diff)
			}
		})
	}
	wg.Wait()
	for i := range seen {
		if n := seen[i].Load(); n != 2 {
			t.Errorf("compact-%d: %d attempts, want 2", i, n)
		}
	}
}
