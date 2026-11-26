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
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func sseFrame(eventType, dataJSON string) string {
	return "event: " + eventType + "\ndata: " + dataJSON + "\n\n"
}

var (
	frameMessageStart = sseFrame("message_start", `{"type":"message_start","message":{"id":"msg_compact","type":"message","role":"assistant","model":"gpt-5.6-sol","content":[],"stop_reason":null}}`)
	frameBlockStart   = sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	frameBlockStop    = sseFrame("content_block_stop", `{"type":"content_block_stop","index":0}`)
	frameMessageDelta = sseFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}`)
	frameMessageStop  = sseFrame("message_stop", `{"type":"message_stop"}`)
)

func frameMessageStartUsage(inputTokens, cacheRead int) string {
	return sseFrame("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_compact","type":"message","role":"assistant","model":"gpt-5.6-sol","content":[],"stop_reason":null,"usage":{"input_tokens":%d,"cache_read_input_tokens":%d}}}`,
		inputTokens, cacheRead,
	))
}

func frameMessageDeltaUsage(inputTokens, outputTokens int) string {
	return sseFrame("message_delta", fmt.Sprintf(
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		inputTokens, outputTokens,
	))
}

func frameTextDelta(text string) string {
	encoded, _ := json.Marshal(text)
	return sseFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(encoded)+`}}`)
}

func completeStream(summary string) string {
	return frameMessageStart + frameBlockStart + frameTextDelta(summary) + frameBlockStop + frameMessageDelta + frameMessageStop
}

func lostTerminalStream(summary string) string {
	return frameMessageStart + frameBlockStart + frameTextDelta(summary) + frameBlockStop + frameMessageDelta
}

func missingTerminalStream() string {
	return frameMessageStart + frameBlockStart + frameTextDelta("partial ") +
		sseFrame("error", `{"type":"error","error":{"type":"api_error","message":"websocket_missing_terminal"}}`)
}

func truncatedStream() string {
	return frameMessageStart + frameBlockStart + frameTextDelta("partial ")
}

func compactRequestBody(model, userContent string) []byte {
	encodedModel, _ := json.Marshal(model)
	encodedPrompt, _ := json.Marshal(compactguard.CompactionSystemPrompt)
	encodedUser, _ := json.Marshal(userContent)
	return []byte(`{"model":` + string(encodedModel) + `,"max_tokens":512,"stream":true,"system":[{"type":"text","text":` + string(encodedPrompt) + `}],"messages":[{"role":"user","content":` + string(encodedUser) + `}]}`)
}

func defaultZeroDelaySettings(retries int) compactguard.Settings {
	return compactguard.Settings{
		StreamGuard:      true,
		Retries:          retries,
		MaxBufferedBytes: 16 * 1024 * 1024,
		RetryDelay:       0,
	}
}

func httpUpstream(client *http.Client, targetURL string, body []byte) compactguard.Upstream {
	return compactguard.UpstreamFunc(func(ctx context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return client.Do(req)
	})
}

type recordingUsage struct {
	mu               sync.Mutex
	responseStatuses []int
	responseBytes    int64
	resets           int
	completeCalls    []bool
	current          compactguard.Usage
}

func (u *recordingUsage) SetResponse(statusCode int, _ http.Header) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.responseStatuses = append(u.responseStatuses, statusCode)
}

func (u *recordingUsage) NoteResponseBytes(n int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.responseBytes += n
}

func (u *recordingUsage) ObserveEvent(event jsontext.Value) {
	var parsed struct {
		Type    string `json:"type"`
		Message struct {
			Usage *struct {
				InputTokens         *float64 `json:"input_tokens"`
				OutputTokens        *float64 `json:"output_tokens"`
				CacheReadTokens     *float64 `json:"cache_read_input_tokens"`
				CacheCreationTokens *float64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage *struct {
			InputTokens         *float64 `json:"input_tokens"`
			OutputTokens        *float64 `json:"output_tokens"`
			CacheReadTokens     *float64 `json:"cache_read_input_tokens"`
			CacheCreationTokens *float64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(event, &parsed); err != nil {
		return
	}
	src := parsed.Usage
	if parsed.Type == "message_start" {
		src = parsed.Message.Usage
	}
	if src == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if src.InputTokens != nil {
		u.current.InputTokens = new(*src.InputTokens)
	}
	if src.OutputTokens != nil {
		u.current.OutputTokens = new(*src.OutputTokens)
	}
	if src.CacheReadTokens != nil {
		u.current.CacheReadTokens = new(*src.CacheReadTokens)
	}
	if src.CacheCreationTokens != nil {
		u.current.CacheCreationTokens = new(*src.CacheCreationTokens)
	}
}

func (u *recordingUsage) Snapshot() compactguard.Usage {
	u.mu.Lock()
	defer u.mu.Unlock()
	return compactguard.SnapshotUsage(u.current)
}

func (u *recordingUsage) ResetUsage() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.resets++
	u.current = compactguard.Usage{}
}

func (u *recordingUsage) SetResponseComplete(complete bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.completeCalls = append(u.completeCalls, complete)
}

type finishCall struct {
	statusCode     int
	statusOverride string
}

type recordingTelemetry struct {
	mu          sync.Mutex
	compactions []compactguard.Diagnostics
	finishes    []finishCall
}

func (rt *recordingTelemetry) SetCompaction(diag compactguard.Diagnostics) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.compactions = append(rt.compactions, diag)
}

func (rt *recordingTelemetry) Finish(statusCode int, statusOverride string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.finishes = append(rt.finishes, finishCall{statusCode: statusCode, statusOverride: statusOverride})
}

func (rt *recordingTelemetry) snapshot() ([]compactguard.Diagnostics, []finishCall) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]compactguard.Diagnostics(nil), rt.compactions...), append([]finishCall(nil), rt.finishes...)
}

type countingPeeker struct {
	calls atomic.Int32
	fn    func(ctx context.Context, body io.Reader) (compactguard.PeekResult, error)
}

func (p *countingPeeker) Peek(ctx context.Context, body io.Reader) (compactguard.PeekResult, error) {
	p.calls.Add(1)
	if p.fn != nil {
		return p.fn(ctx, body)
	}
	return compactguard.PeekResult{Stream: body}, nil
}

func serveGuardOverHTTP(t *testing.T, runner compactguard.Runner, buildReq func(r *http.Request, body []byte) compactguard.Request, onErr func(error)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = runner.Serve(r.Context(), w, buildReq(r, body))
		if onErr != nil {
			onErr(err)
		}
		if errors.Is(err, http.ErrAbortHandler) {
			panic(http.ErrAbortHandler)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotImplemented)
		}
	}))
	t.Cleanup(func() {
		srv.Close()
		srv.Client().CloseIdleConnections()
	})
	return srv
}

func TestSentryNativeUnbuffered(t *testing.T) {
	t.Parallel()

	firstChunk := frameMessageStart + frameBlockStart + frameTextDelta("native first chunk ")
	tailChunk := frameBlockStop + frameMessageDelta + frameMessageStop

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseTail := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseTail)

	var attempts atomic.Int32
	var mu sync.Mutex
	var upstreamReceivedBody []byte

	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		body, err := io.ReadAll(r.Body)
		if err == nil {
			mu.Lock()
			upstreamReceivedBody = body
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, firstChunk)
		_ = http.NewResponseController(w).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, tailChunk)
		_ = http.NewResponseController(w).Flush()
	}))
	t.Cleanup(func() {
		releaseTail()
		anthropic.Close()
		anthropic.Client().CloseIdleConnections()
	})

	runner := compactguard.Runner{Settings: defaultZeroDelaySettings(2)}
	gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
		return compactguard.Request{
			ProxyProvider:  "",
			PathOnly:       r.URL.Path,
			UpstreamOrigin: anthropic.URL,
			Body:           body,
			Upstream:       httpUpstream(anthropic.Client(), anthropic.URL+r.URL.Path, body),
		}
	}, nil)

	reqBody := compactRequestBody("claude-opus-5", "Summarize the conversation.")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("client do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	var received strings.Builder
	for !strings.Contains(received.String(), "native first chunk ") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("failed waiting for first chunk while terminal frame was withheld (got %q): %v", received.String(), err)
		}
		received.WriteString(line)
	}

	// Only release the terminal frame after the client already observed the first chunk.
	releaseTail()

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading remainder: %v", err)
	}
	received.Write(rest)

	if diff := gocmp.Diff(int32(1), attempts.Load()); diff != "" {
		t.Fatalf("upstream attempts mismatch (-want +got):\n%s", diff)
	}
	mu.Lock()
	gotUpstreamBody := append([]byte(nil), upstreamReceivedBody...)
	mu.Unlock()
	if diff := gocmp.Diff(string(reqBody), string(gotUpstreamBody)); diff != "" {
		t.Fatalf("forwarded body mismatch (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(firstChunk+tailChunk, received.String()); diff != "" {
		t.Fatalf("native compaction stream bytes mismatch (-want +got):\n%s", diff)
	}
}

func TestGuardRetryAndDelivery(t *testing.T) {
	t.Parallel()

	type scriptEntry struct {
		status      int
		contentType string
		body        string
		abort       bool
	}

	tests := map[string]struct {
		retries            int
		scripts            []scriptEntry
		promptContent      string
		wantStatus         int
		wantAttempts       int32
		wantExactBody      string
		wantContains       []string
		wantAbsent         []string
		wantMessageStarts  int
		wantMessageStops   int
		wantDiag           compactguard.Diagnostics
		wantFinish         finishCall
		wantResets         int
		wantComplete       bool
		wantDiagNoContains []string
	}{
		"success: healthy compaction stream passes through untouched": {
			retries:           2,
			scripts:           []scriptEntry{{status: http.StatusOK, body: completeStream("first try summary")}},
			promptContent:     "Summarize.",
			wantStatus:        http.StatusOK,
			wantAttempts:      1,
			wantExactBody:     completeStream("first try summary"),
			wantMessageStarts: 1,
			wantMessageStops:  1,
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeCompleted,
				UpstreamStatus: new(http.StatusOK),
				TerminalCode:   compactguard.TerminalMessageStop,
				Usage:          compactguard.Usage{OutputTokens: new(12.0)},
			},
			wantFinish:   finishCall{statusCode: http.StatusOK},
			wantComplete: true,
		},
		"success: lost terminal after end_turn synthesizes byte-identical message_stop": {
			retries:           1,
			scripts:           []scriptEntry{{status: http.StatusOK, body: lostTerminalStream("IDENTICAL")}, {status: http.StatusOK, body: completeStream("unused")}},
			promptContent:     "Summarize.",
			wantStatus:        http.StatusOK,
			wantAttempts:      1,
			wantExactBody:     completeStream("IDENTICAL"),
			wantMessageStarts: 1,
			wantMessageStops:  1,
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeCompleted,
				UpstreamStatus: new(http.StatusOK),
				Usage:          compactguard.Usage{OutputTokens: new(12.0)},
			},
			wantFinish:   finishCall{statusCode: http.StatusOK},
			wantComplete: true,
		},
		"success: websocket_missing_terminal retries and reports only completed attempt usage": {
			retries: 2,
			scripts: []scriptEntry{
				{
					status: http.StatusOK,
					body: sseFrame("message_start", `{"type":"message_start","message":{"id":"msg_compact","type":"message","role":"assistant","model":"gpt-5.6-sol","content":[],"stop_reason":null,"usage":{"input_tokens":17,"cache_read_input_tokens":3,"cache_creation_input_tokens":9}}}`) +
						frameBlockStart + frameTextDelta("partial ") +
						sseFrame("error", `{"type":"error","error":{"type":"api_error","message":"websocket_missing_terminal"}}`),
				},
				{
					status: http.StatusOK,
					body:   frameMessageStartUsage(19, 5) + frameBlockStart + frameTextDelta("the real summary") + frameBlockStop + frameMessageDeltaUsage(19, 13) + frameMessageStop,
				},
			},
			promptContent:     "Summarize.",
			wantStatus:        http.StatusOK,
			wantAttempts:      2,
			wantContains:      []string{"the real summary", `"type":"message_stop"`},
			wantAbsent:        []string{"partial ", "websocket_missing_terminal"},
			wantMessageStarts: 1,
			wantMessageStops:  1,
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeCompleted,
				UpstreamStatus: new(http.StatusOK),
				TerminalCode:   compactguard.TerminalMessageStop,
				Usage: compactguard.Usage{
					InputTokens:     new(19.0),
					OutputTokens:    new(13.0),
					CacheReadTokens: new(5.0),
				},
			},
			wantFinish:   finishCall{statusCode: http.StatusOK},
			wantResets:   1,
			wantComplete: true,
		},
		"success: truncated stream without error event retries and completes": {
			retries: 2,
			scripts: []scriptEntry{
				{status: http.StatusOK, body: truncatedStream()},
				{status: http.StatusOK, body: completeStream("recovered summary")},
			},
			promptContent:     "Summarize.",
			wantStatus:        http.StatusOK,
			wantAttempts:      2,
			wantContains:      []string{"recovered summary", `"type":"message_stop"`},
			wantAbsent:        []string{"partial "},
			wantMessageStarts: 1,
			wantMessageStops:  1,
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeCompleted,
				UpstreamStatus: new(http.StatusOK),
				TerminalCode:   compactguard.TerminalMessageStop,
				Usage:          compactguard.Usage{OutputTokens: new(12.0)},
			},
			wantFinish:   finishCall{statusCode: http.StatusOK},
			wantResets:   1,
			wantComplete: true,
		},
		"success: fatal invalid_request_error is not retried and replays verbatim": {
			retries: 2,
			scripts: []scriptEntry{
				{
					status: http.StatusOK,
					body:   frameMessageStart + sseFrame("error", `{"type":"error","error":{"type":"invalid_request_error","message":"messages: required"}}`),
				},
				{status: http.StatusOK, body: completeStream("should not be reached")},
			},
			promptContent:     "Summarize.",
			wantStatus:        http.StatusOK,
			wantAttempts:      1,
			wantContains:      []string{"invalid_request_error", "messages: required"},
			wantAbsent:        []string{"should not be reached", "ended without a completed generation"},
			wantMessageStarts: 1,
			wantMessageStops:  0,
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeIncomplete,
				UpstreamStatus: new(http.StatusOK),
				ErrorCode:      compactguard.ErrorInvalidRequest,
			},
			wantFinish:   finishCall{statusCode: http.StatusOK, statusOverride: "upstream_error"},
			wantComplete: false,
		},
		"error: exhausted retries on truncated stream append truthful error and merge usage": {
			retries: 1,
			scripts: []scriptEntry{
				{status: http.StatusOK, body: frameMessageStartUsage(17, 3) + frameBlockStart + frameTextDelta("partial-one ")},
				{status: http.StatusOK, body: frameMessageStart + frameBlockStart + frameTextDelta("partial-two ") + sseFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":9}}`)},
			},
			promptContent:     "prompt-secret-must-not-leak",
			wantStatus:        http.StatusOK,
			wantAttempts:      2,
			wantContains:      []string{"partial-two ", "ended without a completed generation after 2 attempt(s)", `"type":"error"`},
			wantAbsent:        []string{"partial-one ", `"type":"message_stop"`},
			wantMessageStarts: 1,
			wantMessageStops:  0,
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeIncomplete,
				UpstreamStatus: new(http.StatusOK),
				Usage: compactguard.Usage{
					InputTokens:     new(17.0),
					OutputTokens:    new(9.0),
					CacheReadTokens: new(3.0),
				},
			},
			wantFinish:         finishCall{statusCode: http.StatusOK, statusOverride: "upstream_error"},
			wantResets:         1,
			wantComplete:       false,
			wantDiagNoContains: []string{"prompt-secret-must-not-leak", "partial-one", "partial-two"},
		},
		"error: exhausted retries on unknown error replay verbatim without leaking secret to diagnostics": {
			retries: 0,
			scripts: []scriptEntry{
				{
					status: http.StatusOK,
					body: frameMessageStartUsage(29, 0) + frameBlockStart + frameTextDelta("partial-response-secret") +
						sseFrame("error", `{"type":"error","error":{"type":"api_error","message":"synthetic-error-secret"}}`),
				},
			},
			promptContent:     "prompt-secret-text",
			wantStatus:        http.StatusOK,
			wantAttempts:      1,
			wantContains:      []string{"synthetic-error-secret"},
			wantAbsent:        []string{`"type":"message_stop"`, "ended without a completed generation"},
			wantMessageStarts: 1,
			wantMessageStops:  0,
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeUnknownError,
				UpstreamStatus: new(http.StatusOK),
				ErrorCode:      compactguard.ErrorUnknown,
				Usage: compactguard.Usage{
					InputTokens:     new(29.0),
					CacheReadTokens: new(0.0),
				},
			},
			wantFinish:         finishCall{statusCode: http.StatusOK, statusOverride: "upstream_error"},
			wantComplete:       false,
			wantDiagNoContains: []string{"partial-response-secret", "prompt-secret-text", "synthetic-error-secret"},
		},
		"error: upstream socket abort after message_stop delivers completed outcome": {
			retries: 0,
			scripts: []scriptEntry{
				{status: http.StatusOK, body: completeStream("delivered before socket abort"), abort: true},
			},
			promptContent:     "Summarize.",
			wantStatus:        http.StatusOK,
			wantAttempts:      1,
			wantContains:      []string{"delivered before socket abort", `"type":"message_stop"`},
			wantMessageStarts: 1,
			wantMessageStops:  1,
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeCompleted,
				UpstreamStatus: new(http.StatusOK),
				TerminalCode:   compactguard.TerminalMessageStop,
				Usage:          compactguard.Usage{OutputTokens: new(12.0)},
			},
			wantFinish:   finishCall{statusCode: http.StatusOK, statusOverride: "upstream_aborted"},
			wantComplete: true,
		},
		"error: attempt 0 HTTP 503 records upstream_error diagnostics without secret": {
			retries: 2,
			scripts: []scriptEntry{
				{status: http.StatusServiceUnavailable, contentType: "application/json", body: `{"error":{"message":"upstream-http-secret"}}`},
			},
			promptContent: "prompt-secret-503",
			wantStatus:    http.StatusServiceUnavailable,
			wantAttempts:  1,
			wantContains:  []string{"upstream-http-secret"},
			wantDiag: compactguard.Diagnostics{
				Outcome:        compactguard.OutcomeUpstreamError,
				UpstreamStatus: new(http.StatusServiceUnavailable),
				ErrorCode:      compactguard.ErrorUpstreamHTTP,
			},
			wantFinish:         finishCall{statusCode: http.StatusServiceUnavailable},
			wantComplete:       false,
			wantDiagNoContains: []string{"upstream-http-secret", "prompt-secret-503"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var attempts atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				idx := int(attempts.Add(1)) - 1
				if idx >= len(tt.scripts) {
					idx = len(tt.scripts) - 1
				}
				entry := tt.scripts[idx]
				ct := entry.contentType
				if ct == "" {
					ct = "text/event-stream"
				}
				if entry.abort {
					hj, ok := w.(http.Hijacker)
					if !ok {
						http.Error(w, "hijack unsupported", http.StatusInternalServerError)
						return
					}
					conn, rw, err := hj.Hijack()
					if err != nil {
						return
					}
					_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", ct, len(entry.body), entry.body)
					_ = rw.Flush()
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", ct)
				w.WriteHeader(entry.status)
				_, _ = io.WriteString(w, entry.body)
			}))
			t.Cleanup(func() {
				proxy.Close()
				proxy.Client().CloseIdleConnections()
			})

			usageRec := &recordingUsage{}
			telRec := &recordingTelemetry{}
			runner := compactguard.Runner{Settings: defaultZeroDelaySettings(tt.retries)}
			gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
				return compactguard.Request{
					ProxyProvider:  "codex",
					PathOnly:       r.URL.Path,
					UpstreamOrigin: proxy.URL,
					Body:           body,
					Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
					Usage:          usageRec,
					Telemetry:      telRec,
				}
			}, nil)

			reqBody := compactRequestBody("claude-gpt-5.6-sol", tt.promptContent)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(reqBody))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := gw.Client().Do(req)
			if err != nil {
				t.Fatalf("client do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read response: %v", err)
			}
			body := string(bodyBytes)

			if diff := gocmp.Diff(tt.wantStatus, resp.StatusCode); diff != "" {
				t.Fatalf("status mismatch (-want +got):\n%s\nbody=%q", diff, body)
			}
			if diff := gocmp.Diff(tt.wantAttempts, attempts.Load()); diff != "" {
				t.Fatalf("attempt count mismatch (-want +got):\n%s", diff)
			}
			if tt.wantExactBody != "" {
				if diff := gocmp.Diff(tt.wantExactBody, body); diff != "" {
					t.Fatalf("exact body mismatch (-want +got):\n%s", diff)
				}
			}
			for _, sub := range tt.wantContains {
				if !strings.Contains(body, sub) {
					t.Errorf("response body missing %q; got %q", sub, body)
				}
			}
			for _, sub := range tt.wantAbsent {
				if strings.Contains(body, sub) {
					t.Errorf("response body unexpectedly contained %q; got %q", sub, body)
				}
			}
			if tt.wantMessageStarts > 0 || tt.wantStatus == http.StatusOK {
				if got := strings.Count(body, `"type":"message_start"`); got != tt.wantMessageStarts {
					t.Errorf("message_start count = %d, want %d", got, tt.wantMessageStarts)
				}
				if got := strings.Count(body, `"type":"message_stop"`); got != tt.wantMessageStops {
					t.Errorf("message_stop count = %d, want %d", got, tt.wantMessageStops)
				}
			}

			diags, finishes := telRec.snapshot()
			if len(diags) != 1 {
				t.Fatalf("telemetry compaction calls = %d, want 1 (%+v)", len(diags), diags)
			}
			if diff := gocmp.Diff(tt.wantDiag, diags[0]); diff != "" {
				t.Fatalf("compaction diagnostics mismatch (-want +got):\n%s", diff)
			}
			if len(finishes) != 1 {
				t.Fatalf("telemetry finish calls = %d, want 1 (%+v)", len(finishes), finishes)
			}
			if diff := gocmp.Diff(tt.wantFinish, finishes[0]); diff != "" {
				t.Fatalf("telemetry finish mismatch (-want +got):\n%s", diff)
			}

			usageRec.mu.Lock()
			gotResets := usageRec.resets
			gotCompleteCalls := append([]bool(nil), usageRec.completeCalls...)
			usageRec.mu.Unlock()

			if diff := gocmp.Diff(tt.wantResets, gotResets); diff != "" {
				t.Fatalf("usage reset count mismatch (-want +got):\n%s", diff)
			}
			if len(gotCompleteCalls) == 0 || gotCompleteCalls[len(gotCompleteCalls)-1] != tt.wantComplete {
				t.Fatalf("usage SetResponseComplete calls = %v, want final %t", gotCompleteCalls, tt.wantComplete)
			}

			encodedDiag, err := json.Marshal(diags[0])
			if err != nil {
				t.Fatalf("marshal diagnostics: %v", err)
			}
			for _, secret := range tt.wantDiagNoContains {
				if strings.Contains(string(encodedDiag), secret) {
					t.Errorf("diagnostics leaked %q: %s", secret, string(encodedDiag))
				}
			}
		})
	}
}

func TestGuardRetryInBandFailure(t *testing.T) {
	t.Parallel()

	t.Run("error: retry HTTP 500 reported in-band as api_error without crashing", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			if attempts.Add(1) == 1 {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, truncatedStream())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"upstream exploded"}}`)
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		telRec := &recordingTelemetry{}
		usageRec := &recordingUsage{}
		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(1)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
				Usage:          usageRec,
				Telemetry:      telRec,
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		body := string(bodyBytes)

		if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s\nbody=%q", diff, body)
		}
		wantFrame := compactguard.SSEErrorFrame("api_error", "model-gateway: upstream returned 500: upstream exploded")
		if diff := gocmp.Diff(wantFrame, body); diff != "" {
			t.Fatalf("in-band error frame mismatch (-want +got):\n%s", diff)
		}
		diags, finishes := telRec.snapshot()
		if len(diags) != 1 || diags[0].Outcome != compactguard.OutcomeUpstreamError || diags[0].ErrorCode != compactguard.ErrorUpstreamHTTP || diags[0].UpstreamStatus == nil || *diags[0].UpstreamStatus != http.StatusInternalServerError {
			t.Fatalf("unexpected diagnostics: %+v", diags)
		}
		if diff := gocmp.Diff([]finishCall{{statusCode: http.StatusInternalServerError}}, finishes); diff != "" {
			t.Fatalf("finish calls mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("error: retry 200 non-SSE reported in-band", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			if attempts.Add(1) == 1 {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, truncatedStream())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"message":"unexpected json reply"}`)
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(1)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s", diff)
		}
		wantFrame := compactguard.SSEErrorFrame("api_error", "model-gateway: upstream returned 200: unexpected json reply")
		if diff := gocmp.Diff(wantFrame, string(bodyBytes)); diff != "" {
			t.Fatalf("in-band non-SSE frame mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("error: retry Codex 401 records rejection and emits authentication_error", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			if attempts.Add(1) == 1 {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, truncatedStream())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Openai-Request-Id", "req-auth-401")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"not authenticated"}}`)
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		var rejectedStatus int
		var rejectedHeader string
		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(1)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
				OnCodexRejection: func(statusCode int, headers http.Header, _ []byte) {
					rejectedStatus = statusCode
					rejectedHeader = headers.Get("X-Openai-Request-Id")
				},
				FormatCodexAuthFailure: func(_ []byte, statusCode int) (string, bool) {
					if statusCode == http.StatusUnauthorized {
						return "Codex login required. Upstream detail: not authenticated", true
					}
					return "", false
				},
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s", diff)
		}
		wantFrame := compactguard.SSEErrorFrame("authentication_error", "Codex login required. Upstream detail: not authenticated")
		if diff := gocmp.Diff(wantFrame, string(bodyBytes)); diff != "" {
			t.Fatalf("in-band auth frame mismatch (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(http.StatusUnauthorized, rejectedStatus); diff != "" {
			t.Fatalf("rejectedStatus mismatch (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff("req-auth-401", rejectedHeader); diff != "" {
			t.Fatalf("rejectedHeader mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("error: retry 413 learns sentry ceiling only when sentry is enabled", func(t *testing.T) {
		t.Parallel()

		for _, sentryEnabled := range []bool{true, false} {
			name := "enabled"
			if !sentryEnabled {
				name = "disabled"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				var attempts atomic.Int32
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.ReadAll(r.Body)
					if attempts.Add(1) == 1 {
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(http.StatusOK)
						_, _ = io.WriteString(w, truncatedStream())
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					_, _ = io.WriteString(w, `{"error":{"type":"request_too_large","message":"too big"}}`)
				}))
				t.Cleanup(func() {
					proxy.Close()
					proxy.Client().CloseIdleConnections()
				})

				var state sentry.State
				sessionKey := sentry.SessionKey(true, "sess-413", nil)
				target := sentry.Target{
					Key:      "gpt-5.6-sol",
					Policy:   &policy.Sentry{BackendWindow: 200000, CompactTrigger: 160000, Source: "derived"},
					Headroom: 40000,
				}
				// Seed prior usage at 120000 (below initial trigger 160000, above headroom 40000).
				// A learned ceiling lowers the derived trigger from 160000 to 120000 - 40000 = 80000.
				state.Observe(sessionKey, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":120000}}`), true)

				runner := compactguard.Runner{Settings: defaultZeroDelaySettings(1)}
				gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
					return compactguard.Request{
						ProxyProvider:  "codex",
						PathOnly:       r.URL.Path,
						UpstreamOrigin: proxy.URL,
						Body:           body,
						Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
						Sentry:         &state,
						SentryEnabled:  sentryEnabled,
						SentrySession:  sessionKey,
						SentryTarget:   target,
					}
				}, nil)

				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
				if err != nil {
					t.Fatalf("new request: %v", err)
				}
				resp, err := gw.Client().Do(req)
				if err != nil {
					t.Fatalf("client do: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()
				_, _ = io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", resp.StatusCode)
				}

				// Rearm with a low observation, then observe 90000 tokens (above learned 80000, below initial 160000).
				state.Observe(sessionKey, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":1000}}`), true)
				state.Observe(sessionKey, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":90000}}`), true)
				crossing, fired := state.Check(sessionKey, target, []byte(`{"messages":[{"role":"user","content":"next"}]}`))
				if diff := gocmp.Diff(sentryEnabled, fired); diff != "" {
					t.Fatalf("sentry fired after retry 413 mismatch (-want +got):\n%s (crossing=%+v)", diff, crossing)
				}
				if sentryEnabled && crossing.Trigger != 80000 {
					t.Fatalf("learned trigger = %v, want 80000", crossing.Trigger)
				}
			})
		}
	})

	t.Run("error: retry transport error aborts committed response", func(t *testing.T) {
		t.Parallel()

		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, truncatedStream())
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		var attempts atomic.Int32
		telRec := &recordingTelemetry{}
		errCh := make(chan error, 1)
		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(1)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Telemetry:      telRec,
				Upstream: compactguard.UpstreamFunc(func(ctx context.Context) (*http.Response, error) {
					if attempts.Add(1) == 1 {
						return httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body).Do(ctx)
					}
					return nil, errors.New("connection refused")
				}),
			}
		}, func(err error) {
			errCh <- err
		})

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		_, readErr := io.ReadAll(resp.Body)
		serveErr := <-errCh

		if !errors.Is(serveErr, http.ErrAbortHandler) {
			t.Fatalf("Serve error = %v, want http.ErrAbortHandler", serveErr)
		}
		if readErr == nil {
			t.Fatalf("client ReadAll succeeded cleanly on aborted committed response; want transport error")
		}
		diags, finishes := telRec.snapshot()
		if len(diags) != 1 || diags[0].Outcome != compactguard.OutcomeUpstreamError || diags[0].UpstreamStatus == nil || *diags[0].UpstreamStatus != http.StatusBadGateway {
			t.Fatalf("unexpected diagnostics on retry transport error: %+v", diags)
		}
		if diff := gocmp.Diff([]finishCall{{statusCode: http.StatusBadGateway, statusOverride: "upstream_error"}}, finishes); diff != "" {
			t.Fatalf("finish calls mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("error: attempt 0 transport error writes 502 JSON", func(t *testing.T) {
		t.Parallel()

		telRec := &recordingTelemetry{}
		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(2)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: "http://127.0.0.1:19999",
				Body:           body,
				Telemetry:      telRec,
				Upstream: compactguard.UpstreamFunc(func(_ context.Context) (*http.Response, error) {
					return nil, errors.New("connect ECONNREFUSED")
				}),
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if diff := gocmp.Diff(http.StatusBadGateway, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s\nbody=%q", diff, string(bodyBytes))
		}
		wantBody := `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream http://127.0.0.1:19999 failed: connect ECONNREFUSED"}}`
		if diff := gocmp.Diff(wantBody, string(bodyBytes)); diff != "" {
			t.Fatalf("502 JSON mismatch (-want +got):\n%s", diff)
		}
		diags, finishes := telRec.snapshot()
		if len(diags) != 1 || diags[0].Outcome != compactguard.OutcomeUpstreamError || diags[0].ErrorCode != compactguard.ErrorUpstreamHTTP {
			t.Fatalf("unexpected diagnostics: %+v", diags)
		}
		if diff := gocmp.Diff([]finishCall{{statusCode: http.StatusBadGateway, statusOverride: "upstream_error"}}, finishes); diff != "" {
			t.Fatalf("finish calls mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestGuardSentryObservation(t *testing.T) {
	t.Parallel()

	ordinaryBody := []byte(`{"model":"claude-gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"ordinary turn"}]}`)

	tests := map[string]struct {
		compactInputTokens int
		wantRearmed        bool
	}{
		"success: low usage observation during guarded compaction rearms latched sentry": {
			compactInputTokens: 20, // Below low watermark (100 - min(40000, 25) = 75).
			wantRearmed:        true,
		},
		"success: high usage guarded compaction never marks attempt ordinary-completed": {
			compactInputTokens: 105, // Above trigger 100; would rearm only if ordinaryCompleted were true.
			wantRearmed:        false,
		},
		"success: usage between low watermark and trigger preserves hysteresis": {
			compactInputTokens: 80, // Between 75 and 100.
			wantRearmed:        false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, frameMessageStart+frameBlockStart+frameTextDelta("summary")+frameBlockStop+
					frameMessageDeltaUsage(tt.compactInputTokens, 10)+frameMessageStop)
			}))
			t.Cleanup(func() {
				proxy.Close()
				proxy.Client().CloseIdleConnections()
			})

			var state sentry.State
			sessionKey := sentry.SessionKey(true, "sess-compact", nil)
			target := sentry.Target{
				Key:      "gpt-5.6-sol",
				Policy:   &policy.Sentry{BackendWindow: 200000, CompactTrigger: 100, Source: "env"},
				Headroom: 40000,
			}
			// Arm and fire the sentry so state.fired == true.
			state.Observe(sessionKey, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":101}}`), true)
			if _, fired := state.Check(sessionKey, target, ordinaryBody); !fired {
				t.Fatal("precondition failed: sentry did not latch")
			}

			runner := compactguard.Runner{Settings: defaultZeroDelaySettings(0)}
			gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
				return compactguard.Request{
					ProxyProvider:  "codex",
					PathOnly:       r.URL.Path,
					UpstreamOrigin: proxy.URL,
					Body:           body,
					Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
					Sentry:         &state,
					SentryEnabled:  true,
					SentrySession:  sessionKey,
					SentryTarget:   target,
				}
			}, nil)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := gw.Client().Do(req)
			if err != nil {
				t.Fatalf("client do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			// Probe whether the latch was cleared: if rearmed, observing 102 (non-completed)
			// must cause Check to fire; if still latched, observing 102 (non-completed) keeps fired=true
			// so Check returns false.
			state.Observe(sessionKey, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":102}}`), false)
			_, fired := state.Check(sessionKey, target, ordinaryBody)
			if diff := gocmp.Diff(tt.wantRearmed, fired); diff != "" {
				t.Fatalf("sentry rearmed state mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGuardLMStudioPeek(t *testing.T) {
	t.Parallel()

	t.Run("success: peeks once on attempt 0 and never peeks on retry after headers committed", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if attempts.Add(1) == 1 {
				_, _ = io.WriteString(w, truncatedStream())
				return
			}
			_, _ = io.WriteString(w, completeStream("lmstudio retried ok"))
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		peeker := &countingPeeker{}
		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(1)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "lmstudio",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
				Peeker:         peeker,
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-lmstudio-qwen", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s\nbody=%q", diff, string(bodyBytes))
		}
		if !strings.Contains(string(bodyBytes), "lmstudio retried ok") {
			t.Fatalf("expected retried body, got %q", string(bodyBytes))
		}
		if diff := gocmp.Diff(int32(2), attempts.Load()); diff != "" {
			t.Fatalf("attempts mismatch (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(int32(1), peeker.calls.Load()); diff != "" {
			t.Fatalf("peek call count mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("success: attempt 0 overflow handled before committing headers without retry", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			attempts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, sseFrame("error", `{"type":"error","error":{"type":"exceed_context_size_error","n_prompt_tokens":9000,"n_ctx":8192}}`))
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		peeker := &countingPeeker{
			fn: func(_ context.Context, _ io.Reader) (compactguard.PeekResult, error) {
				return compactguard.PeekResult{
					Overflow: &sentry.Counts{Tokens: new(9000.0), Limit: new(8192.0)},
				}, nil
			},
		}

		var gotCounts sentry.Counts
		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(2)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "lmstudio",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
				Peeker:         peeker,
				HandleLMStudioOverflow: func(_ context.Context, w http.ResponseWriter, _ int, counts sentry.Counts) error {
					gotCounts = counts
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"request_too_large"}}`)
					return nil
				},
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-lmstudio-qwen", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)

		if diff := gocmp.Diff(http.StatusRequestEntityTooLarge, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(int32(1), attempts.Load()); diff != "" {
			t.Fatalf("attempts mismatch (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(sentry.Counts{Tokens: new(9000.0), Limit: new(8192.0)}, gotCounts); diff != "" {
			t.Fatalf("overflow counts mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("error: attempt 0 peek early end writes 502 JSON", func(t *testing.T) {
		t.Parallel()

		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		peeker := &countingPeeker{
			fn: func(_ context.Context, _ io.Reader) (compactguard.PeekResult, error) {
				return compactguard.PeekResult{EarlyEndOverride: "upstream_error"}, nil
			},
		}
		telRec := &recordingTelemetry{}
		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(2)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "lmstudio",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
				Peeker:         peeker,
				Telemetry:      telRec,
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-lmstudio-qwen", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		bodyBytes, _ := io.ReadAll(resp.Body)

		if diff := gocmp.Diff(http.StatusBadGateway, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s\nbody=%q", diff, string(bodyBytes))
		}
		wantBody := `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`
		if diff := gocmp.Diff(wantBody, string(bodyBytes)); diff != "" {
			t.Fatalf("early-end 502 body mismatch (-want +got):\n%s", diff)
		}
		_, finishes := telRec.snapshot()
		if diff := gocmp.Diff([]finishCall{{statusCode: http.StatusOK, statusOverride: "upstream_error"}}, finishes); diff != "" {
			t.Fatalf("finish mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("success: non-LM-Studio provider never invokes peeker", func(t *testing.T) {
		t.Parallel()

		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, completeStream("codex ok"))
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		peeker := &countingPeeker{}
		runner := compactguard.Runner{Settings: defaultZeroDelaySettings(1)}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
				Peeker:         peeker,
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.ReadAll(resp.Body)

		if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(int32(0), peeker.calls.Load()); diff != "" {
			t.Fatalf("peek call count mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestGuardBufferOverflowDegrades(t *testing.T) {
	t.Parallel()

	t.Run("success: exact byte ceiling stays buffered and retries", func(t *testing.T) {
		t.Parallel()

		truncated := truncatedStream()
		var attempts atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if attempts.Add(1) == 1 {
				_, _ = io.WriteString(w, truncated)
				return
			}
			_, _ = io.WriteString(w, completeStream("retried at exact limit"))
		}))
		t.Cleanup(func() {
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		var stderr bytes.Buffer
		runner := compactguard.Runner{
			Settings: compactguard.Settings{
				StreamGuard:      true,
				Retries:          1,
				MaxBufferedBytes: float64(len(truncated)),
			},
			Stderr: &stderr,
		}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if diff := gocmp.Diff(http.StatusOK, resp.StatusCode); diff != "" {
			t.Fatalf("status mismatch (-want +got):\n%s", diff)
		}
		if diff := gocmp.Diff(int32(2), attempts.Load()); diff != "" {
			t.Fatalf("attempts mismatch (-want +got):\n%s", diff)
		}
		if !strings.Contains(string(bodyBytes), "retried at exact limit") {
			t.Fatalf("expected retried summary, got %q", string(bodyBytes))
		}
		if stderr.Len() != 0 {
			t.Fatalf("unexpected stderr warning at exact limit: %q", stderr.String())
		}
	})

	t.Run("success: exceeding limit logs warning flushes live and disables retry", func(t *testing.T) {
		t.Parallel()

		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseTail := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseTail)

		bigFirst := frameMessageStart + frameBlockStart + frameTextDelta(strings.Repeat("x", 160))
		var attempts atomic.Int32
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			attempts.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, bigFirst)
			_ = http.NewResponseController(w).Flush()

			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			// End truncated without message_stop after degradation.
		}))
		t.Cleanup(func() {
			releaseTail()
			proxy.Close()
			proxy.Client().CloseIdleConnections()
		})

		var stderrMu sync.Mutex
		var stderr bytes.Buffer
		syncStderr := writerFunc(func(p []byte) (int, error) {
			stderrMu.Lock()
			defer stderrMu.Unlock()
			return stderr.Write(p)
		})

		runner := compactguard.Runner{
			Settings: compactguard.Settings{
				StreamGuard:      true,
				Retries:          2,
				MaxBufferedBytes: 128,
			},
			Stderr: syncStderr,
		}
		gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
			return compactguard.Request{
				ProxyProvider:  "codex",
				PathOnly:       r.URL.Path,
				UpstreamOrigin: proxy.URL,
				Body:           body,
				Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
			}
		}, nil)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := gw.Client().Do(req)
		if err != nil {
			t.Fatalf("client do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, string(body))
		}

		// Confirm flushed chunks reach the client while the upstream is still open.
		reader := bufio.NewReader(resp.Body)
		var received strings.Builder
		for !strings.Contains(received.String(), strings.Repeat("x", 160)) {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("expected live flush after exceeding buffer ceiling (got %q): %v", received.String(), err)
			}
			received.WriteString(line)
		}

		releaseTail()
		rest, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read remainder: %v", err)
		}
		received.Write(rest)

		if diff := gocmp.Diff(int32(1), attempts.Load()); diff != "" {
			t.Fatalf("degraded stream must not retry (-want +got):\n%s", diff)
		}
		if !strings.Contains(received.String(), "ended without a completed generation after 1 attempt(s)") {
			t.Fatalf("expected incomplete error frame after degraded truncation, got %q", received.String())
		}
		stderrMu.Lock()
		stderrText := stderr.String()
		stderrMu.Unlock()
		wantWarn := "model-gateway: compaction response passed 128 buffered bytes; streaming the remainder live without retry cover"
		if !strings.Contains(stderrText, wantWarn) {
			t.Fatalf("stderr = %q, want substring %q", stderrText, wantWarn)
		}
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestGuardClientDisconnectStopsRetry(t *testing.T) {
	t.Parallel()

	allowTruncate := make(chan struct{})
	var closeTruncateOnce sync.Once
	finishTruncate := func() { closeTruncateOnce.Do(func() { close(allowTruncate) }) }
	t.Cleanup(finishTruncate)

	var attempts atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, truncatedStream())
		_ = http.NewResponseController(w).Flush()

		select {
		case <-allowTruncate:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		finishTruncate()
		proxy.Close()
		proxy.Client().CloseIdleConnections()
	})

	handlerCtxCanceled := make(chan struct{})
	handlerDone := make(chan struct{})
	runner := compactguard.Runner{Settings: defaultZeroDelaySettings(2)}
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		stop := context.AfterFunc(r.Context(), func() {
			close(handlerCtxCanceled)
		})
		defer stop()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := runner.Serve(r.Context(), w, compactguard.Request{
			ProxyProvider:  "codex",
			PathOnly:       r.URL.Path,
			UpstreamOrigin: proxy.URL,
			Body:           body,
			// Use a detached upstream context so the first attempt actually completes truncated
			// after the client disconnects, exercising the retry writability check.
			Upstream: compactguard.UpstreamFunc(func(_ context.Context) (*http.Response, error) {
				upReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, proxy.URL+r.URL.Path, bytes.NewReader(body))
				if err != nil {
					return nil, err
				}
				return proxy.Client().Do(upReq)
			}),
		}); err != nil {
			http.Error(w, err.Error(), http.StatusNotImplemented)
		}
	}))
	t.Cleanup(func() {
		finishTruncate()
		gw.Close()
		gw.Client().CloseIdleConnections()
	})

	clientCtx, cancelClient := context.WithCancel(t.Context())
	defer cancelClient()

	req, err := http.NewRequestWithContext(clientCtx, http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("client do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, string(body))
	}

	// Disconnect the client after headers were committed, then let attempt 1 finish truncated.
	cancelClient()
	_ = resp.Body.Close()
	<-handlerCtxCanceled
	finishTruncate()
	<-handlerDone

	if diff := gocmp.Diff(int32(1), attempts.Load()); diff != "" {
		t.Fatalf("disconnected client must prevent retry (-want +got):\n%s", diff)
	}
}

func TestGuardDisabledStreamsLive(t *testing.T) {
	t.Parallel()

	firstChunk := frameMessageStart + frameBlockStart + frameTextDelta("unguarded partial ")
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseEnd := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseEnd)

	var attempts atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, firstChunk)
		_ = http.NewResponseController(w).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
		}
		// Close truncated without message_stop to confirm retries stay disabled.
	}))
	t.Cleanup(func() {
		releaseEnd()
		proxy.Close()
		proxy.Client().CloseIdleConnections()
	})

	runner := compactguard.Runner{
		Settings: compactguard.Settings{
			StreamGuard:      false,
			Retries:          2,
			MaxBufferedBytes: 16 * 1024 * 1024,
		},
	}
	gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
		return compactguard.Request{
			ProxyProvider:  "codex",
			PathOnly:       r.URL.Path,
			UpstreamOrigin: proxy.URL,
			Body:           body,
			Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
		}
	}, nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(compactRequestBody("claude-gpt-5.6-sol", "Summarize.")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("client do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, string(body))
	}

	reader := bufio.NewReader(resp.Body)
	var received strings.Builder
	for !strings.Contains(received.String(), "unguarded partial ") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("expected live delivery with guard off (got %q): %v", received.String(), err)
		}
		received.WriteString(line)
	}
	releaseEnd()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remainder: %v", err)
	}
	received.Write(rest)

	if diff := gocmp.Diff(int32(1), attempts.Load()); diff != "" {
		t.Fatalf("guard disabled must not retry (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(firstChunk, received.String()); diff != "" {
		t.Fatalf("unguarded body mismatch (-want +got):\n%s", diff)
	}
}

func TestGuardTwentyConcurrentCompactionStreams(t *testing.T) {
	t.Parallel()

	const total = 20

	var mu sync.Mutex
	attemptsByKey := make(map[string]int, total)
	var totalAttempts atomic.Int32

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || len(payload.Messages) == 0 {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		key := payload.Messages[0].Content
		mu.Lock()
		seen := attemptsByKey[key] + 1
		attemptsByKey[key] = seen
		mu.Unlock()
		n := totalAttempts.Add(1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if seen == 1 {
			if n%2 == 1 {
				_, _ = io.WriteString(w, missingTerminalStream())
			} else {
				_, _ = io.WriteString(w, truncatedStream())
			}
			return
		}
		_, _ = io.WriteString(w, completeStream("summary for "+key))
	}))
	t.Cleanup(func() {
		proxy.Close()
		proxy.Client().CloseIdleConnections()
	})

	runner := compactguard.Runner{Settings: defaultZeroDelaySettings(2)}
	gw := serveGuardOverHTTP(t, runner, func(r *http.Request, body []byte) compactguard.Request {
		return compactguard.Request{
			ProxyProvider:  "codex",
			PathOnly:       r.URL.Path,
			UpstreamOrigin: proxy.URL,
			Body:           body,
			Upstream:       httpUpstream(proxy.Client(), proxy.URL+r.URL.Path, body),
		}
	}, nil)

	type result struct {
		status int
		body   string
		err    error
	}
	results := make([]result, total)
	var wg sync.WaitGroup
	for i := range total {
		wg.Go(func() {
			reqBody := compactRequestBody("claude-gpt-5.6-sol", fmt.Sprintf("compact-%d", i))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gw.URL+"/v1/messages", bytes.NewReader(reqBody))
			if err != nil {
				results[i] = result{err: err}
				return
			}
			resp, err := gw.Client().Do(req)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			results[i] = result{status: resp.StatusCode, body: string(b), err: err}
		})
	}
	wg.Wait()

	for i, res := range results {
		if res.err != nil {
			t.Fatalf("request %d failed: %v", i, res.err)
		}
		if res.status != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (body=%q)", i, res.status, res.body)
		}
		wantSummary := fmt.Sprintf("summary for compact-%d", i)
		if !strings.Contains(res.body, wantSummary) {
			t.Errorf("request %d missing %q; got %q", i, wantSummary, res.body)
		}
		if got := strings.Count(res.body, `"type":"message_stop"`); got != 1 {
			t.Errorf("request %d message_stop count = %d, want 1", i, got)
		}
		if got := strings.Count(res.body, `"type":"message_start"`); got != 1 {
			t.Errorf("request %d message_start count = %d, want 1", i, got)
		}
		if strings.Contains(res.body, "websocket_missing_terminal") || strings.Contains(res.body, "partial ") {
			t.Errorf("request %d leaked discarded attempt: %q", i, res.body)
		}
	}
	if diff := gocmp.Diff(int32(total*2), totalAttempts.Load()); diff != "" {
		t.Fatalf("total upstream attempts mismatch (-want +got):\n%s", diff)
	}
}
