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
	"bufio"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func lmStudioOverflowFrame(promptTokens, contextTokens int) string {
	engine := fmt.Sprintf(`{"error":{"code":400,"message":"request (%d tokens) exceeds the available context size (%d tokens), try increasing it","type":"exceed_context_size_error","n_prompt_tokens":%d,"n_ctx":%d}}`,
		promptTokens, contextTokens, promptTokens, contextTokens)
	escapedEngine, _ := json.Marshal(fmt.Sprintf("Engine protocol predict request returned 400: %s", engine))
	return fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%s}}\n\n", escapedEngine)
}

func lmStudioWordedOverflowFrame(promptTokens, contextTokens int) string {
	msg := fmt.Sprintf("Engine protocol predict request returned 400: request (%d tokens) exceeds the available context size (%d tokens), try increasing it [exceed_context_size_error]",
		promptTokens, contextTokens)
	escaped, _ := json.Marshal(msg)
	return fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":%s}}\n\n", escaped)
}

func lmStudioOrdinaryStream(promptTokens, cachedTokens int) string {
	return strings.Join([]string{
		fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"qwen3-coder-30b\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":%d,\"output_tokens\":0,\"cache_read_input_tokens\":%d}}}\n\n", promptTokens, cachedTokens),
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":%d,\"output_tokens\":2,\"cache_read_input_tokens\":%d}}\n\n", promptTokens, cachedTokens),
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")
}

func TestForwardMalformedUTF8Unchanged(t *testing.T) {
	emojiPayload := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"keep \xf0\x9f\x8e\xb5\"}}\n\n"
	emojiSplit := strings.Index(emojiPayload, "\xf0\x9f\x8e\xb5") + 2

	tests := map[string]struct {
		passthrough bool
		chunks      [][]byte
		want        []byte
	}{
		"success: untouched proxy frames preserve ill-formed UTF-8 bytes": {
			chunks: [][]byte{
				[]byte(": bad\xffcomment\r\n\r\n"),
				[]byte(": \xfe\r\ndata:\r\n\r\n"),
				[]byte(": \x80\r\ndata: [DONE]\n\n"),
				[]byte("event: custom\r\ndata: {bad\xff\xfe\r\n\r\n"),
				[]byte("data: partial\xc0\xaf"),
			},
			want: []byte(": bad\xffcomment\r\n\r\n: \xfe\r\ndata:\r\n\r\n: \x80\r\ndata: [DONE]\n\nevent: custom\r\ndata: {bad\xff\xfe\r\n\r\ndata: partial\xc0\xaf"),
		},
		"success: rewritten proxy frame decodes whole frame as Node UTF-8 including comments and metadata": {
			chunks: [][]byte{
				[]byte(": bad\xff\xfecomment\r\nevent: msg\x80\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"gpt-5.6-sol\",\"note\":\"a\xffb\"}}\r\n\r\n"),
			},
			want: []byte(": bad��comment\nevent: msg�\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-gpt-5.6-sol\",\"note\":\"a�b\"}}\r\n\r\n"),
		},
		"success: valid rewritten unterminated proxy tail preserves absence of delimiter": {
			chunks: [][]byte{
				[]byte("event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"gpt-5.6-sol\"}}"),
			},
			want: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-gpt-5.6-sol\"}}"),
		},
		"success: Anthropic passthrough preserves ill-formed UTF-8 bytes unchanged": {
			passthrough: true,
			chunks: [][]byte{
				[]byte(": raw\xff\r\ndata: {\"type\":\"message_start\",\"text\":\"a\xffb\"}\r\n\r\n"),
				[]byte("data: tail\xfe"),
			},
			want: []byte(": raw\xff\r\ndata: {\"type\":\"message_start\",\"text\":\"a\xffb\"}\r\n\r\ndata: tail\xfe"),
		},
		"success: multi-byte UTF-8 split across chunks survives proxy rewrite": {
			chunks: [][]byte{
				[]byte(emojiPayload[:emojiSplit]),
				[]byte(emojiPayload[emojiSplit:]),
			},
			want: []byte(emojiPayload),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				rc := http.NewResponseController(w)
				for _, chunk := range tt.chunks {
					_, _ = w.Write(chunk)
					_ = rc.Flush()
				}
			}))
			t.Cleanup(upSrv.Close)

			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
				if tt.passthrough {
					return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{
						ClientDone: r.Context().Done(),
						Usage:      &usageSpy{},
					})
				}
				return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
					Provider:          "codex",
					AdvertisedModel:   "claude-gpt-5.6-sol",
					BackendModel:      "gpt-5.6-sol",
					HeartbeatInterval: -1,
					ClientDone:        r.Context().Done(),
				})
			})

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
			}
			res, handlerErr := h.waitResult(t)
			if handlerErr != nil {
				t.Fatalf("relay returned error %v, want nil", handlerErr)
			}
			if res.Status != http.StatusOK {
				t.Errorf("outcome.Status = %d, want 200", res.Status)
			}
			if diff := gocmp.Diff(tt.want, body); diff != "" {
				t.Errorf("wire body mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLocalNonStudioNoPeek(t *testing.T) {
	overflowFrame := lmStudioOverflowFrame(6017, 4096)

	tests := map[string]struct {
		provider        string
		advertisedModel string
		backendModel    string
		wantStatus      int
		wantPeekCalls   int64
		wantOverflow    int
	}{
		"success: kimi route bypasses peek and keeps overflow frame in-band": {
			provider:        "kimi",
			advertisedModel: "claude-kimi-k3",
			backendModel:    "kimi-k3",
			wantStatus:      http.StatusOK,
			wantPeekCalls:   0,
			wantOverflow:    0,
		},
		"success: codex route bypasses peek and keeps overflow frame in-band": {
			provider:        "codex",
			advertisedModel: "claude-gpt-5.6-sol",
			backendModel:    "gpt-5.6-sol",
			wantStatus:      http.StatusOK,
			wantPeekCalls:   0,
			wantOverflow:    0,
		},
		"success: local llama route bypasses peek and keeps overflow frame in-band": {
			provider:        "local",
			advertisedModel: "claude-local/gguf",
			backendModel:    "local/gguf",
			wantStatus:      http.StatusOK,
			wantPeekCalls:   0,
			wantOverflow:    0,
		},
		"success: lmstudio control invokes peek once and normalizes initial overflow to 413": {
			provider:        sse.LMStudioProvider,
			advertisedModel: "claude-lmstudio/qwen3-coder-30b",
			backendModel:    "lmstudio/qwen3-coder-30b",
			wantStatus:      http.StatusRequestEntityTooLarge,
			wantPeekCalls:   1,
			wantOverflow:    1,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, overflowFrame)
			}))
			t.Cleanup(upSrv.Close)

			var peekCalls atomic.Int64
			var mu sync.Mutex
			var overflows []recordedOverflow
			var wantOverflowRes outcome.Result

			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
				return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
					Provider:          tt.provider,
					AdvertisedModel:   tt.advertisedModel,
					BackendModel:      tt.backendModel,
					HeartbeatInterval: -1,
					FirstEventWait:    10 * time.Second,
					ClientDone:        r.Context().Done(),
					OnPeek:            func() { peekCalls.Add(1) },
					OnOverflow: func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
						outRes, outErr := testEngineOverflowResponder(w, upstreamStatus, counts)
						mu.Lock()
						overflows = append(overflows, recordedOverflow{Counts: counts, UpstreamStatus: upstreamStatus})
						wantOverflowRes = outRes
						mu.Unlock()
						return outRes, outErr
					},
				})
			})

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d (%q), want %d", resp.StatusCode, string(body), tt.wantStatus)
			}
			res, handlerErr := h.waitResult(t)
			if handlerErr != nil {
				t.Fatalf("RelayProxy error = %v, want nil", handlerErr)
			}
			if res.Status != tt.wantStatus {
				t.Errorf("outcome.Status = %d, want %d", res.Status, tt.wantStatus)
			}
			if got := peekCalls.Load(); got != tt.wantPeekCalls {
				t.Errorf("peek calls = %d, want %d", got, tt.wantPeekCalls)
			}
			mu.Lock()
			gotOverflows := append([]recordedOverflow(nil), overflows...)
			gotWantRes := wantOverflowRes
			mu.Unlock()
			if len(gotOverflows) != tt.wantOverflow {
				t.Fatalf("overflow callbacks = %d, want %d", len(gotOverflows), tt.wantOverflow)
			}
			if tt.wantStatus == http.StatusOK {
				if diff := gocmp.Diff(overflowFrame, string(body)); diff != "" {
					t.Errorf("in-band overflow frame mismatch (-want +got):\n%s", diff)
				}
			} else {
				want := recordedOverflow{
					Counts:         sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))},
					UpstreamStatus: http.StatusOK,
				}
				if diff := gocmp.Diff(want, gotOverflows[0]); diff != "" {
					t.Errorf("overflow details mismatch (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(gotWantRes, res); diff != "" {
					t.Errorf("outcome.Result propagated from OnOverflow (-want +got):\n%s", diff)
				}
			}
		})
	}

	// Verify the non-peek path commits status without flushing headers prematurely before first output.
	t.Run("success: non-studio route commits status without peeking and flushes only on output", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        src,
			}
			var peekCalls atomic.Int64
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(t.Context(), rec, up, sse.ProxyOptions{
					Provider:          "codex",
					AdvertisedModel:   "claude-gpt-5.6-sol",
					BackendModel:      "gpt-5.6-sol",
					HeartbeatInterval: -1,
					OnPeek:            func() { peekCalls.Add(1) },
				})
				done <- handlerOutcome{res: res, err: err, bodyClosed: src.closed.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					_ = pw.Close()
					_ = pr.Close()
					<-done
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				t.Fatalf("RelayProxy returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			status, wroteHeader, _, _, flushes := rec.snapshot()
			if !wroteHeader || status != http.StatusOK {
				t.Fatalf("before output: wroteHeader=%t status=%d, want true and 200", wroteHeader, status)
			}
			if flushes != 0 {
				t.Errorf("flushes before any body output = %d, want 0 (commit status without premature flush)", flushes)
			}
			if got := peekCalls.Load(); got != 0 {
				t.Errorf("peek calls = %d, want 0", got)
			}

			if _, err := io.WriteString(pw, overflowFrame); err != nil {
				t.Fatalf("write overflowFrame: %v", err)
			}
			synctest.Wait()
			_, _, body, _, flushesAfter := rec.snapshot()
			if flushesAfter < 1 {
				t.Errorf("flushes after output = %d, want >= 1", flushesAfter)
			}
			if diff := gocmp.Diff(overflowFrame, body); diff != "" {
				t.Errorf("body mismatch (-want +got):\n%s", diff)
			}

			_ = pw.Close()
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after upstream EOF")
			}
			if out.err != nil {
				t.Fatalf("RelayProxy returned %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayProxy returned without closing owned resp.Body")
			}
		})
	})
}

func TestProxyRelayFlushAndHeaders(t *testing.T) {
	firstFrame := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"gpt-5.6-sol\"}}\n\n"
	wantFirst := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-gpt-5.6-sol\"}}\n\n"
	secondFrame := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	t.Run("success: loopback smoke strips content-length and keep-alive, flushes frames, and reports usage and outcome", func(t *testing.T) {
		firstFrameSeen := make(chan struct{})
		var releaseOnce sync.Once
		releaseSecond := func() { releaseOnce.Do(func() { close(firstFrameSeen) }) }

		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Request-Id", "req-proxy-1")
			w.Header().Set("Keep-Alive", "timeout=5")
			w.Header().Set("Content-Length", strconv.Itoa(len(firstFrame)+len(secondFrame)))
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			_, _ = io.WriteString(w, firstFrame)
			_ = rc.Flush()
			select {
			case <-firstFrameSeen:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, secondFrame)
			_ = rc.Flush()
		}))
		// LIFO cleanup: release blocked upstream handler before closing upSrv.
		t.Cleanup(upSrv.Close)
		t.Cleanup(releaseSecond)

		usage := &usageSpy{}
		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
				Provider:          "codex",
				AdvertisedModel:   "claude-gpt-5.6-sol",
				BackendModel:      "gpt-5.6-sol",
				HeartbeatInterval: -1,
				ClientDone:        r.Context().Done(),
				Usage:             usage,
			})
		})

		reqCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.server.URL+"/v1/messages", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
		}
		if resp.Header.Get("Cache-Control") != "no-cache" || resp.Header.Get("Request-Id") != "req-proxy-1" {
			t.Errorf("unexpected preserved headers: %v", resp.Header)
		}
		if resp.Header.Get("Content-Length") != "" || resp.Header.Get("Keep-Alive") != "" {
			t.Errorf("content-length or keep-alive leaked into relayed SSE headers: %v", resp.Header)
		}

		reader := bufio.NewReader(resp.Body)
		var first strings.Builder
		for !strings.HasSuffix(first.String(), "\n\n") {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				t.Fatalf("reading first flushed frame: %v (partial=%q)", readErr, first.String())
			}
			first.WriteString(line)
		}
		if diff := gocmp.Diff(wantFirst, first.String()); diff != "" {
			t.Errorf("first flushed frame (-want +got):\n%s", diff)
		}
		releaseSecond()

		rest, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading remaining stream: %v", err)
		}
		if diff := gocmp.Diff(secondFrame, string(rest)); diff != "" {
			t.Errorf("second frame (-want +got):\n%s", diff)
		}

		res, handlerErr := h.waitResult(t)
		if handlerErr != nil {
			t.Fatalf("RelayProxy error = %v, want nil", handlerErr)
		}
		if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, res); diff != "" {
			t.Errorf("outcome.Result (-want +got):\n%s", diff)
		}

		events, _, totalBytes, overflows := usage.snapshot(t)
		if totalBytes != int64(len(firstFrame)+len(secondFrame)) {
			t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, len(firstFrame)+len(secondFrame))
		}
		if overflows != 0 {
			t.Errorf("unexpected usage overflow count %d", overflows)
		}
		wantEvents := []string{
			`{"type":"message_start","message":{"model":"gpt-5.6-sol"}}`,
			`{"type":"message_stop"}`,
		}
		if diff := gocmp.Diff(wantEvents, events); diff != "" {
			t.Errorf("usage observed events (-want +got):\n%s", diff)
		}
	})

	t.Run("success: deterministic pipe proves each completed proxy frame is flushed before next arrives", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}, "Content-Length": []string{strconv.Itoa(len(firstFrame) + len(secondFrame))}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}, "Content-Length": []string{strconv.Itoa(len(firstFrame) + len(secondFrame))}},
				Body:        src,
			}
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(t.Context(), rec, up, sse.ProxyOptions{
					Provider:          "codex",
					AdvertisedModel:   "claude-gpt-5.6-sol",
					BackendModel:      "gpt-5.6-sol",
					HeartbeatInterval: -1,
				})
				done <- handlerOutcome{res: res, err: err, bodyClosed: src.closed.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					_ = pw.Close()
					_ = pr.Close()
					<-done
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				t.Fatalf("RelayProxy returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			if _, err := io.WriteString(pw, firstFrame); err != nil {
				t.Fatalf("write firstFrame: %v", err)
			}
			synctest.Wait()
			_, _, body1, _, flushes1 := rec.snapshot()
			if diff := gocmp.Diff(wantFirst, body1); diff != "" {
				t.Fatalf("first frame not written before second (-want +got):\n%s", diff)
			}
			if flushes1 < 1 {
				t.Fatalf("flushes after first frame = %d, want >= 1", flushes1)
			}
			if got := rec.Header().Get("Content-Length"); got != "" {
				t.Errorf("Content-Length leaked to writer header: %q", got)
			}

			if _, err := io.WriteString(pw, secondFrame); err != nil {
				t.Fatalf("write secondFrame: %v", err)
			}
			synctest.Wait()
			_, _, body2, _, flushes2 := rec.snapshot()
			if diff := gocmp.Diff(wantFirst+secondFrame, body2); diff != "" {
				t.Fatalf("second frame not written (-want +got):\n%s", diff)
			}
			if flushes2 <= flushes1 {
				t.Fatalf("flushes did not advance on second frame: before=%d after=%d", flushes1, flushes2)
			}

			_ = pw.Close()
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after upstream EOF")
			}
			if out.err != nil {
				t.Fatalf("RelayProxy error = %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayProxy returned without closing owned resp.Body")
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, out.res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
		})
	})
}

func TestProxyRelayCompletionAndMessageStop(t *testing.T) {
	const literalMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	finishedWithoutStop := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"gpt-5.6-sol\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hand-back report\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n",
	}, "")
	cutOffTurn := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"gpt-5.6-sol\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hand-back report\"}}\n\n",
	}, "")
	explicitStop := finishedWithoutStop + literalMessageStop

	tests := map[string]struct {
		stream           string
		wantEndWithStop  bool
		wantMessageStops int
		wantOutcome      outcome.Result
	}{
		"success: finished turn without message_stop synthesizes terminal event at EOF": {
			stream:           finishedWithoutStop,
			wantEndWithStop:  true,
			wantMessageStops: 1,
			wantOutcome:      outcome.Result{Status: http.StatusOK},
		},
		"error: cut-off turn without stop reason and open block does not synthesize message_stop": {
			stream:           cutOffTurn,
			wantEndWithStop:  false,
			wantMessageStops: 0,
			wantOutcome:      outcome.Result{Status: http.StatusOK, StatusOverride: "upstream_error"},
		},
		"success: explicit message_stop is not duplicated": {
			stream:           explicitStop,
			wantEndWithStop:  true,
			wantMessageStops: 1,
			wantOutcome:      outcome.Result{Status: http.StatusOK},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tt.stream)
			}))
			t.Cleanup(upSrv.Close)

			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
				return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
					Provider:          "codex",
					AdvertisedModel:   "claude-gpt-5.6-sol",
					BackendModel:      "gpt-5.6-sol",
					HeartbeatInterval: -1,
					ClientDone:        r.Context().Done(),
				})
			})

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
			}
			bodyStr := string(body)
			if !strings.Contains(bodyStr, "hand-back report") {
				t.Errorf("preceding answer text missing from body: %q", bodyStr)
			}
			if tt.wantEndWithStop {
				if !strings.HasSuffix(bodyStr, literalMessageStop) {
					t.Errorf("body did not end with literal message_stop frame; body=%q", bodyStr)
				}
				if got := strings.Count(bodyStr, literalMessageStop); got != tt.wantMessageStops {
					t.Errorf("literal message_stop count = %d, want %d; body=%q", got, tt.wantMessageStops, bodyStr)
				}
			} else if strings.Contains(bodyStr, "message_stop") {
				t.Errorf("unexpected message_stop in cut-off turn; body=%q", bodyStr)
			}

			res, handlerErr := h.waitResult(t)
			if handlerErr != nil {
				t.Fatalf("RelayProxy error = %v, want nil", handlerErr)
			}
			if diff := gocmp.Diff(tt.wantOutcome, res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
		})
	}
}

func TestProxyRelaySentryAccounting(t *testing.T) {
	const literalMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	ordinaryReq := []byte(`{"model":"claude-gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	session := sentry.SessionKey(true, "session-1", nil)

	t.Run("success: sums all input usage fields from SSE message_delta frames", func(t *testing.T) {
		resolver := policy.NewResolver(policy.ResolverInputs{})
		target, err := sentry.ResolveTarget(resolver, "claude-gpt-5.6-sol", 100, 0, 0)
		if err != nil {
			t.Fatalf("ResolveTarget: %v", err)
		}
		var state sentry.State

		runTurn := func(input, cacheRead, cacheCreate int) {
			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":%d,\"cache_read_input_tokens\":%d,\"cache_creation_input_tokens\":%d}}\n\n%s",
					input, cacheRead, cacheCreate, literalMessageStop)
			}))
			defer upSrv.Close()
			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
				return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
					Provider:          "codex",
					AdvertisedModel:   "claude-gpt-5.6-sol",
					BackendModel:      "gpt-5.6-sol",
					HeartbeatInterval: -1,
					ClientDone:        r.Context().Done(),
					SessionKey:        session,
					Sentry:            &state,
					SentryTarget:      target,
				})
			})
			req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
			if reqErr != nil {
				t.Fatalf("NewRequestWithContext: %v", reqErr)
			}
			resp, doErr := h.client.Do(req)
			if doErr != nil {
				t.Fatalf("Do: %v", doErr)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
			}
			if _, handlerErr := h.waitResult(t); handlerErr != nil {
				t.Fatalf("RelayProxy error = %v", handlerErr)
			}
		}

		// 30 + 50 + 19 = 99 <= 100: must not arm.
		runTurn(30, 50, 19)
		if crossing, fired := state.Check(session, target, ordinaryReq); fired {
			t.Fatalf("unexpected crossing at 99 <= 100: %+v", crossing)
		}
		// 30 + 50 + 21 = 101 > 100: must arm.
		runTurn(30, 50, 21)
		crossing, fired := state.Check(session, target, ordinaryReq)
		if !fired {
			t.Fatal("expected crossing at 101 > 100")
		}
		if diff := gocmp.Diff(sentry.Crossing{Usage: 101, Trigger: 100}, crossing); diff != "" {
			t.Errorf("crossing (-want +got):\n%s", diff)
		}
	})

	t.Run("success: llama-server stream reporting input only in message_start merges and arms sentry", func(t *testing.T) {
		const nCtx = 40960
		const trigger = nCtx - nCtx/3 // 27307
		table := proxymodels.NewTable([]proxymodels.Route{{ID: "local/gguf", Provider: "local"}})
		resolver := policy.NewResolver(policy.ResolverInputs{Routes: table, LocalWindow: nCtx, LocalPort: 8080})
		target, err := sentry.ResolveTarget(resolver, "claude-local/gguf", 0, 0, 0)
		if err != nil {
			t.Fatalf("ResolveTarget: %v", err)
		}
		var state sentry.State

		llamaStream := strings.Join([]string{
			fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"gguf\",\"usage\":{\"cache_read_input_tokens\":%d,\"input_tokens\":22,\"output_tokens\":0}}}\n\n", trigger-21),
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n",
			literalMessageStop,
		}, "")

		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, llamaStream)
		}))
		t.Cleanup(upSrv.Close)

		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
				Provider:          "local",
				AdvertisedModel:   "claude-local/gguf",
				BackendModel:      "local/gguf",
				HeartbeatInterval: -1,
				ClientDone:        r.Context().Done(),
				SessionKey:        session,
				Sentry:            &state,
				SentryTarget:      target,
			})
		})

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
		}
		if _, handlerErr := h.waitResult(t); handlerErr != nil {
			t.Fatalf("RelayProxy error = %v", handlerErr)
		}

		crossing, fired := state.Check(session, target, ordinaryReq)
		if !fired {
			t.Fatalf("expected merged message_start usage (%d) to cross trigger (%d)", trigger+1, trigger)
		}
		if diff := gocmp.Diff(sentry.Crossing{Usage: float64(trigger + 1), Trigger: float64(trigger)}, crossing); diff != "" {
			t.Errorf("crossing (-want +got):\n%s", diff)
		}
	})

	t.Run("success: LM Studio cache accounting subtracts before sentry and client output", func(t *testing.T) {
		const ctxLen = 65536 // trigger = 65536 - 21845 = 43691
		table := proxymodels.NewTable([]proxymodels.Route{{ID: "lmstudio/qwen3-coder-30b", Provider: sse.LMStudioProvider}})
		resolver := policy.NewResolver(policy.ResolverInputs{
			Routes:          table,
			LMStudioURL:     "http://127.0.0.1:1234",
			LMStudioWindows: map[string]int64{"qwen3-coder-30b": ctxLen},
		})
		target, err := sentry.ResolveTarget(resolver, "claude-lmstudio/qwen3-coder-30b", 0, 0, 0)
		if err != nil {
			t.Fatalf("ResolveTarget: %v", err)
		}
		var state sentry.State

		// Inclusive 30000 with 29990 cached would double-count to 59990 (> 43691) if uncorrected.
		stream := lmStudioOrdinaryStream(30000, 29990)
		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, stream)
		}))
		t.Cleanup(upSrv.Close)

		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
				Provider:          sse.LMStudioProvider,
				AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
				BackendModel:      "lmstudio/qwen3-coder-30b",
				HeartbeatInterval: -1,
				FirstEventWait:    5 * time.Second,
				ClientDone:        r.Context().Done(),
				SessionKey:        session,
				Sentry:            &state,
				SentryTarget:      target,
			})
		})

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
		}
		if _, handlerErr := h.waitResult(t); handlerErr != nil {
			t.Fatalf("RelayProxy error = %v", handlerErr)
		}
		if !strings.Contains(string(body), `"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":29990`) ||
			!strings.Contains(string(body), `"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":29990`) {
			t.Errorf("client did not receive exclusive LM Studio usage; body=%q", string(body))
		}
		if crossing, fired := state.Check(session, target, ordinaryReq); fired {
			t.Fatalf("corrected 30000-token prompt should not cross 43691 trigger: %+v", crossing)
		}
	})

	t.Run("success: sentry waits for ordinary stream to finish before rearming latched refusal", func(t *testing.T) {
		resolver := policy.NewResolver(policy.ResolverInputs{})
		target, err := sentry.ResolveTarget(resolver, "claude-gpt-6.1-sol[1m]", 100, 0, 0)
		if err != nil {
			t.Fatalf("ResolveTarget: %v", err)
		}
		var state sentry.State
		// Arm and latch the session at 101 > 100.
		state.Observe(session, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":101}}`), true)
		if _, fired := state.Check(session, target, ordinaryReq); !fired {
			t.Fatal("precondition: expected initial crossing to latch")
		}

		releaseStop := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseStop) }) }

		deltaFrame := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"input_tokens\":105}}\n\n"
		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			_, _ = io.WriteString(w, deltaFrame)
			_ = rc.Flush()
			select {
			case <-releaseStop:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, literalMessageStop)
			_ = rc.Flush()
		}))
		// LIFO cleanup: release blocked upstream handler before closing upSrv.
		t.Cleanup(upSrv.Close)
		t.Cleanup(release)

		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
				Provider:          "codex",
				AdvertisedModel:   "claude-gpt-6.1-sol[1m]",
				BackendModel:      "gpt-6.1-sol",
				HeartbeatInterval: -1,
				ClientDone:        r.Context().Done(),
				SessionKey:        session,
				Sentry:            &state,
				SentryTarget:      target,
			})
		})

		reqCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.server.URL+"/v1/messages", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
		}
		reader := bufio.NewReader(resp.Body)
		var first strings.Builder
		for !strings.HasSuffix(first.String(), "\n\n") {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				t.Fatalf("reading mid-stream delta: %v", readErr)
			}
			first.WriteString(line)
		}
		// While the stream is still open, high usage must not rearm the latched refusal.
		if crossing, fired := state.Check(session, target, ordinaryReq); fired {
			t.Fatalf("stream chunk rearmed sentry before turn finished: %+v", crossing)
		}
		release()
		if _, err := io.ReadAll(reader); err != nil {
			t.Fatalf("draining rest of stream: %v", err)
		}
		if _, handlerErr := h.waitResult(t); handlerErr != nil {
			t.Fatalf("RelayProxy error = %v", handlerErr)
		}
		crossing, fired := state.Check(session, target, ordinaryReq)
		if !fired {
			t.Fatal("completed ordinary turn did not rearm latched sentry")
		}
		if diff := gocmp.Diff(sentry.Crossing{Usage: 105, Trigger: 100}, crossing); diff != "" {
			t.Errorf("rearmed crossing (-want +got):\n%s", diff)
		}
	})

	t.Run("success: compaction stream never rearms latched high-usage refusal on completion", func(t *testing.T) {
		resolver := policy.NewResolver(policy.ResolverInputs{})
		target, err := sentry.ResolveTarget(resolver, "claude-gpt-6.1-sol[1m]", 100, 0, 0)
		if err != nil {
			t.Fatalf("ResolveTarget: %v", err)
		}
		var state sentry.State
		state.Observe(session, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":101}}`), true)
		if _, fired := state.Check(session, target, ordinaryReq); !fired {
			t.Fatal("precondition: expected initial crossing to latch")
		}

		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":105}}\n\n"+literalMessageStop)
		}))
		t.Cleanup(upSrv.Close)

		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
				Provider:          "codex",
				AdvertisedModel:   "claude-gpt-6.1-sol[1m]",
				BackendModel:      "gpt-6.1-sol",
				IsCompaction:      true,
				HeartbeatInterval: -1,
				ClientDone:        r.Context().Done(),
				SessionKey:        session,
				Sentry:            &state,
				SentryTarget:      target,
			})
		})

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
		}
		if _, handlerErr := h.waitResult(t); handlerErr != nil {
			t.Fatalf("RelayProxy error = %v", handlerErr)
		}
		if crossing, fired := state.Check(session, target, ordinaryReq); fired {
			t.Fatalf("compaction stream rearmed latched refusal: %+v", crossing)
		}
	})

	// Non-deliverable ordinary turns ending cleanly at transport EOF must not rearm a latched sentry.
	nonDeliverableCases := map[string]struct {
		stream string
	}{
		"error: ordinary stream with in-band error before clean EOF does not rearm latched sentry": {
			stream: strings.Join([]string{
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":105}}\n\n",
				"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"transient failure\"}}\n\n",
				literalMessageStop,
			}, ""),
		},
		"error: incomplete ordinary stream with open block before clean EOF does not rearm latched sentry": {
			stream: strings.Join([]string{
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":105}}\n\n",
			}, ""),
		},
	}
	for name, tt := range nonDeliverableCases {
		t.Run(name, func(t *testing.T) {
			resolver := policy.NewResolver(policy.ResolverInputs{})
			target, err := sentry.ResolveTarget(resolver, "claude-gpt-6.1-sol[1m]", 100, 0, 0)
			if err != nil {
				t.Fatalf("ResolveTarget: %v", err)
			}
			var state sentry.State
			state.Observe(session, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":101}}`), true)
			if _, fired := state.Check(session, target, ordinaryReq); !fired {
				t.Fatal("precondition: expected initial crossing to latch")
			}

			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tt.stream)
			}))
			t.Cleanup(upSrv.Close)

			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
				return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
					Provider:          "codex",
					AdvertisedModel:   "claude-gpt-6.1-sol[1m]",
					BackendModel:      "gpt-6.1-sol",
					HeartbeatInterval: -1,
					ClientDone:        r.Context().Done(),
					SessionKey:        session,
					Sentry:            &state,
					SentryTarget:      target,
				})
			})

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
			}
			res, handlerErr := h.waitResult(t)
			if handlerErr != nil {
				t.Fatalf("RelayProxy error = %v", handlerErr)
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK, StatusOverride: "upstream_error"}, res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
			if crossing, fired := state.Check(session, target, ordinaryReq); fired {
				t.Fatalf("non-deliverable ordinary turn rearmed latched sentry: %+v", crossing)
			}
		})
	}
}

func TestLMStudioFirstEventPeek(t *testing.T) {
	overflowFrame := lmStudioOverflowFrame(6017, 4096)
	wordedFrame := lmStudioWordedOverflowFrame(6017, 4096)
	multilineOverflow := "event: error\ndata: {\"type\":\"error\",\ndata: \"error\":{\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":6017,\"n_ctx\":4096}}\n\n"
	markerOnlyOverflow := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"exceed_context_size_error\"}}\n\n"
	ordinary := lmStudioOrdinaryStream(19, 0)
	otherError := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"The model has crashed without additional information. (Exit code: 6)\"}}\n\n"
	partialFrame := "event: message_start\ndata: {\"type\":\"mess"
	unparseableFrame := "event: message_start\ndata: {broken-json}\n\n"
	malformedThenOverflow := unparseableFrame + overflowFrame
	nonErrorOverflowShaped := "event: message_start\ndata: {\"type\":\"message_start\",\"error\":{\"type\":\"exceed_context_size_error\",\"n_prompt_tokens\":6017,\"n_ctx\":4096}}\n\n"
	secondEventOverflow := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"qwen3-coder-30b\"}}\n\n" + overflowFrame

	tests := map[string]struct {
		chunks       []string
		headWritten  bool
		wantStatus   int
		wantBody     string
		wantPeek     int64
		wantOverflow *recordedOverflow
	}{
		"success: embedded JSON overflow arrives at once": {
			chunks:       []string{overflowFrame},
			wantStatus:   http.StatusRequestEntityTooLarge,
			wantPeek:     1,
			wantOverflow: &recordedOverflow{Counts: sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))}, UpstreamStatus: http.StatusOK},
		},
		"success: embedded JSON overflow arrives in two writes after a comment frame": {
			chunks:       []string{": ping\n\n" + overflowFrame[:40], overflowFrame[40:]},
			wantStatus:   http.StatusRequestEntityTooLarge,
			wantPeek:     1,
			wantOverflow: &recordedOverflow{Counts: sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))}, UpstreamStatus: http.StatusOK},
		},
		"success: non-JSON worded engine overflow extracts counts": {
			chunks:       []string{wordedFrame},
			wantStatus:   http.StatusRequestEntityTooLarge,
			wantPeek:     1,
			wantOverflow: &recordedOverflow{Counts: sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))}, UpstreamStatus: http.StatusOK},
		},
		"success: multiline data first error event normalizes to 413": {
			chunks:       []string{multilineOverflow},
			wantStatus:   http.StatusRequestEntityTooLarge,
			wantPeek:     1,
			wantOverflow: &recordedOverflow{Counts: sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))}, UpstreamStatus: http.StatusOK},
		},
		"success: marker-only engine overflow without counts still normalizes to 413": {
			chunks:       []string{markerOnlyOverflow},
			wantStatus:   http.StatusRequestEntityTooLarge,
			wantPeek:     1,
			wantOverflow: &recordedOverflow{Counts: sentry.Counts{}, UpstreamStatus: http.StatusOK},
		},
		"success: ordinary answer replays original bytes unchanged as 200": {
			chunks:     []string{ordinary},
			wantStatus: http.StatusOK,
			wantBody:   ordinary,
			wantPeek:   1,
		},
		"success: ordinary answer split inside first frame replays unchanged as 200": {
			chunks:     []string{ordinary[:30], ordinary[30:]},
			wantStatus: http.StatusOK,
			wantBody:   ordinary,
			wantPeek:   1,
		},
		"error: another error event stays in-band on a 200 stream": {
			chunks:     []string{otherError},
			wantStatus: http.StatusOK,
			wantBody:   otherError,
			wantPeek:   1,
		},
		"error: unparseable first data event stays on a 200 stream": {
			chunks:     []string{unparseableFrame},
			wantStatus: http.StatusOK,
			wantBody:   unparseableFrame,
			wantPeek:   1,
		},
		"error: malformed first data frame followed by overflow does not skip and stays 200": {
			chunks:     []string{malformedThenOverflow},
			wantStatus: http.StatusOK,
			wantBody:   malformedThenOverflow,
			wantPeek:   1,
		},
		"success: overflow-shaped fields on a non-error first event stay in-band on 200 stream": {
			chunks:     []string{nonErrorOverflowShaped},
			wantStatus: http.StatusOK,
			wantBody:   nonErrorOverflowShaped,
			wantPeek:   1,
		},
		"error: stream ending cleanly before one frame completes stays 200 with partial bytes": {
			chunks:     []string{partialFrame},
			wantStatus: http.StatusOK,
			wantBody:   partialFrame,
			wantPeek:   1,
		},
		"error: empty stream at clean EOF stays 200 with empty body": {
			chunks:     []string{""},
			wantStatus: http.StatusOK,
			wantBody:   "",
			wantPeek:   1,
		},
		"success: overflow in second event is not scanned and stays in-band on 200 stream": {
			chunks:     []string{secondEventOverflow},
			wantStatus: http.StatusOK,
			wantBody:   secondEventOverflow,
			wantPeek:   1,
		},
		"success: compaction retry with HeadWritten skips peek even on overflow first frame": {
			chunks:      []string{overflowFrame},
			headWritten: true,
			wantStatus:  http.StatusOK,
			wantBody:    overflowFrame,
			wantPeek:    0,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				rc := http.NewResponseController(w)
				for _, chunk := range tt.chunks {
					if chunk != "" {
						_, _ = io.WriteString(w, chunk)
						_ = rc.Flush()
					}
				}
			}))
			t.Cleanup(upSrv.Close)

			var peekCalls atomic.Int64
			var mu sync.Mutex
			var overflows []recordedOverflow
			var wantOverflowRes outcome.Result

			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
				if tt.headWritten {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
				}
				return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
					Provider:          sse.LMStudioProvider,
					AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
					BackendModel:      "lmstudio/qwen3-coder-30b",
					HeadWritten:       tt.headWritten,
					HeartbeatInterval: -1,
					FirstEventWait:    5 * time.Second,
					ClientDone:        r.Context().Done(),
					OnPeek:            func() { peekCalls.Add(1) },
					OnOverflow: func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
						outRes, outErr := testEngineOverflowResponder(w, upstreamStatus, counts)
						mu.Lock()
						overflows = append(overflows, recordedOverflow{Counts: counts, UpstreamStatus: upstreamStatus})
						wantOverflowRes = outRes
						mu.Unlock()
						return outRes, outErr
					},
				})
			})

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d (%q), want %d", resp.StatusCode, string(body), tt.wantStatus)
			}
			res, handlerErr := h.waitResult(t)
			if handlerErr != nil {
				t.Fatalf("RelayProxy error = %v, want nil", handlerErr)
			}
			if res.Status != tt.wantStatus {
				t.Errorf("outcome.Status = %d, want %d", res.Status, tt.wantStatus)
			}
			if got := peekCalls.Load(); got != tt.wantPeek {
				t.Errorf("peek calls = %d, want %d", got, tt.wantPeek)
			}
			mu.Lock()
			gotOverflows := append([]recordedOverflow(nil), overflows...)
			gotWantRes := wantOverflowRes
			mu.Unlock()
			if tt.wantOverflow == nil {
				if len(gotOverflows) != 0 {
					t.Errorf("unexpected overflow callbacks: %+v", gotOverflows)
				}
				if diff := gocmp.Diff(tt.wantBody, string(body)); diff != "" {
					t.Errorf("stream body mismatch (-want +got):\n%s", diff)
				}
			} else {
				if len(gotOverflows) != 1 {
					t.Fatalf("overflow callbacks = %d, want 1", len(gotOverflows))
				}
				if diff := gocmp.Diff(*tt.wantOverflow, gotOverflows[0]); diff != "" {
					t.Errorf("overflow mismatch (-want +got):\n%s", diff)
				}
				if got := resp.Header.Get("X-Model-Gateway-Upstream-Status"); got != "200" {
					t.Errorf("X-Model-Gateway-Upstream-Status = %q, want 200", got)
				}
				if diff := gocmp.Diff(gotWantRes, res); diff != "" {
					t.Errorf("outcome.Result propagated from OnOverflow (-want +got):\n%s", diff)
				}
			}
		})
	}

	t.Run("error: OnOverflow result and error propagate verbatim from RelayProxy", func(t *testing.T) {
		errOverflowWrite := errors.New("overflow responder failed")
		wantRes := outcome.Result{
			Status:         http.StatusRequestEntityTooLarge,
			StatusOverride: "context_overflow",
			Route: outcome.Route{
				Overflow: &outcome.Overflow{
					Phase:        "first_turn",
					RefusedBy:    "upstream",
					TokensFrom:   "upstream",
					RequestBytes: 1234,
					Tokens:       new(float64(6017)),
					LimitTokens:  new(float64(4096)),
				},
			},
		}

		src := &trackedChunkBody{chunks: [][]byte{[]byte(overflowFrame)}}
		rec := newStreamingRecorder()
		up := &upstream.Response{
			StatusCode:  http.StatusOK,
			Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
			RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:        src,
		}
		var calls int
		var gotCounts sentry.Counts
		var gotUpstreamStatus int
		res, err := sse.RelayProxy(t.Context(), rec, up, sse.ProxyOptions{
			Provider:          sse.LMStudioProvider,
			AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
			BackendModel:      "lmstudio/qwen3-coder-30b",
			HeartbeatInterval: -1,
			FirstEventWait:    5 * time.Second,
			OnOverflow: func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
				calls++
				gotUpstreamStatus = upstreamStatus
				gotCounts = counts
				_, _ = testEngineOverflowResponder(w, upstreamStatus, counts)
				return wantRes, errOverflowWrite
			},
		})
		if calls != 1 {
			t.Fatalf("OnOverflow calls = %d, want 1", calls)
		}
		if gotUpstreamStatus != http.StatusOK {
			t.Errorf("OnOverflow upstreamStatus = %d, want 200", gotUpstreamStatus)
		}
		wantCounts := sentry.Counts{Tokens: new(float64(6017)), Limit: new(float64(4096))}
		if diff := gocmp.Diff(wantCounts, gotCounts); diff != "" {
			t.Errorf("OnOverflow counts (-want +got):\n%s", diff)
		}
		if !errors.Is(err, errOverflowWrite) {
			t.Errorf("RelayProxy error = %v, want %v", err, errOverflowWrite)
		}
		if diff := gocmp.Diff(wantRes, res); diff != "" {
			t.Errorf("RelayProxy result (-want +got):\n%s", diff)
		}
		if !src.closed.Load() {
			t.Errorf("RelayProxy returned without closing owned resp.Body")
		}
	})

	t.Run("error: upstream socket broken before first complete frame returns 502 early-end JSON", func(t *testing.T) {
		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}

		var wg sync.WaitGroup
		// LIFO cleanup: close listener first so Accept unblocks, then wait for goroutine exit.
		t.Cleanup(wg.Wait)
		t.Cleanup(func() { _ = ln.Close() })

		wg.Go(func() {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			// Truncated chunked stream before any complete frame.
			_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n5\r\n: hi\n\r\n")
		})

		h := startRelayServer(t, "http://"+ln.Addr().String(), func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
				Provider:          sse.LMStudioProvider,
				AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
				BackendModel:      "lmstudio/qwen3-coder-30b",
				HeartbeatInterval: -1,
				FirstEventWait:    5 * time.Second,
				ClientDone:        r.Context().Done(),
			})
		})

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/messages", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d (%q), want 502", resp.StatusCode, string(body))
		}
		wantBody := `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`
		if diff := gocmp.Diff(wantBody, string(body)); diff != "" {
			t.Errorf("502 body mismatch (-want +got):\n%s", diff)
		}
		res, handlerErr := h.waitResult(t)
		if handlerErr != nil {
			t.Fatalf("RelayProxy error = %v, want nil", handlerErr)
		}
		if res.Status != http.StatusOK || (res.StatusOverride != "upstream_error" && res.StatusOverride != "upstream_aborted") {
			t.Errorf("outcome.Result = %+v, want status 200 with upstream_error or upstream_aborted", res)
		}
	})
}

func TestProxyTimers(t *testing.T) {
	const literalMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const literalHeartbeat = ": ping\n\n"
	if sse.HeartbeatComment != literalHeartbeat {
		t.Errorf("HeartbeatComment = %q, want %q", sse.HeartbeatComment, literalHeartbeat)
	}

	t.Run("success: default 20s heartbeat emits ping while idle and resets on upstream data", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        src,
			}
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(t.Context(), rec, up, sse.ProxyOptions{
					Provider:        "codex",
					AdvertisedModel: "claude-gpt-5.6-terra",
					BackendModel:    "gpt-5.6-terra",
				})
				done <- handlerOutcome{res: res, err: err, bodyClosed: src.closed.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					_ = pw.Close()
					_ = pr.Close()
					<-done
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				t.Fatalf("RelayProxy returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			first := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
			if _, err := io.WriteString(pw, first); err != nil {
				t.Fatalf("write first: %v", err)
			}
			synctest.Wait()

			// 19s silence: no heartbeat yet.
			time.Sleep(19 * time.Second)
			synctest.Wait()
			_, _, body19, _, _ := rec.snapshot()
			if strings.Contains(body19, literalHeartbeat) {
				t.Fatalf("heartbeat fired before 20s: %q", body19)
			}

			// Crossing 20s fires one heartbeat.
			time.Sleep(1 * time.Second)
			synctest.Wait()
			_, _, body20, _, _ := rec.snapshot()
			if got := strings.Count(body20, literalHeartbeat); got != 1 {
				t.Fatalf("heartbeats at 20s = %d, want 1; body=%q", got, body20)
			}

			// Upstream activity resets the 20s idle timer.
			time.Sleep(15 * time.Second)
			synctest.Wait()
			mid := "event: ping\ndata: {\"type\":\"ping\"}\n\n"
			if _, err := io.WriteString(pw, mid); err != nil {
				t.Fatalf("write mid: %v", err)
			}
			synctest.Wait()

			time.Sleep(15 * time.Second)
			synctest.Wait()
			_, _, bodyReset, _, _ := rec.snapshot()
			if got := strings.Count(bodyReset, literalHeartbeat); got != 1 {
				t.Fatalf("heartbeat did not reset on upstream data; count=%d body=%q", got, bodyReset)
			}

			// 5 more seconds reaches 20s since mid -> second heartbeat.
			time.Sleep(5 * time.Second)
			synctest.Wait()
			_, _, bodySecond, _, _ := rec.snapshot()
			if got := strings.Count(bodySecond, literalHeartbeat); got != 2 {
				t.Fatalf("heartbeats after second 20s silence = %d, want 2; body=%q", got, bodySecond)
			}

			if _, err := io.WriteString(pw, literalMessageStop); err != nil {
				t.Fatalf("write stop: %v", err)
			}
			_ = pw.Close()
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after upstream EOF")
			}
			if out.err != nil {
				t.Fatalf("RelayProxy returned %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayProxy returned without closing owned resp.Body")
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, out.res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
			_, _, finalBody, _, _ := rec.snapshot()
			if diff := gocmp.Diff(first+mid+literalMessageStop, strings.ReplaceAll(finalBody, literalHeartbeat, "")); diff != "" {
				t.Errorf("stream without heartbeats (-want +got):\n%s", diff)
			}
		})
	})

	t.Run("success: negative HeartbeatInterval disables heartbeats across 60s of silence", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        src,
			}
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(t.Context(), rec, up, sse.ProxyOptions{
					Provider:          "codex",
					AdvertisedModel:   "claude-gpt-5.6-terra",
					BackendModel:      "gpt-5.6-terra",
					HeartbeatInterval: -1,
				})
				done <- handlerOutcome{res: res, err: err, bodyClosed: src.closed.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					_ = pw.Close()
					_ = pr.Close()
					<-done
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				t.Fatalf("RelayProxy returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			time.Sleep(60 * time.Second)
			synctest.Wait()
			_, _, body, _, _ := rec.snapshot()
			if strings.Contains(body, literalHeartbeat) {
				t.Fatalf("unexpected heartbeat when disabled: %q", body)
			}

			_, _ = io.WriteString(pw, literalMessageStop)
			_ = pw.Close()
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after upstream EOF")
			}
			if out.err != nil {
				t.Fatalf("RelayProxy returned %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayProxy returned without closing owned resp.Body")
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, out.res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
		})
	})

	t.Run("success: LM Studio first-event wait times out at 10s and resumes heartbeat without splitting stalled frame", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ordinary := lmStudioOrdinaryStream(19, 0)
			pr, pw := io.Pipe()
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        src,
			}
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(t.Context(), rec, up, sse.ProxyOptions{
					Provider:        sse.LMStudioProvider,
					AdvertisedModel: "claude-lmstudio/qwen3-coder-30b",
					BackendModel:    "lmstudio/qwen3-coder-30b",
				})
				done <- handlerOutcome{res: res, err: err, bodyClosed: src.closed.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					_ = pw.Close()
					_ = pr.Close()
					<-done
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				t.Fatalf("RelayProxy returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			// Send partial first frame, then stall past the 10s first-event wait and 20s heartbeat.
			if _, err := io.WriteString(pw, ordinary[:30]); err != nil {
				t.Fatalf("write partial first frame: %v", err)
			}
			synctest.Wait()

			time.Sleep(9 * time.Second)
			synctest.Wait()
			_, wroteAt9, _, _, _ := rec.snapshot()
			if wroteAt9 {
				t.Fatal("headers committed before 10s LM Studio first-event wait expired")
			}

			// Expire the 10s peek wait -> headers committed, heartbeat armed.
			time.Sleep(1 * time.Second)
			synctest.Wait()
			statusAt10, wroteAt10, _, _, _ := rec.snapshot()
			if !wroteAt10 || statusAt10 != http.StatusOK {
				t.Fatalf("at 10s wroteHeader=%t status=%d, want true and 200", wroteAt10, statusAt10)
			}

			// 20s of silence after hand-off fires the heartbeat before the stalled frame completes.
			time.Sleep(20 * time.Second)
			synctest.Wait()
			_, _, bodyAt30, _, _ := rec.snapshot()
			if !strings.HasPrefix(bodyAt30, literalHeartbeat) {
				t.Fatalf("expected heartbeat before stalled first frame; body=%q", bodyAt30)
			}

			// Finish sending the stalled stream.
			if _, err := io.WriteString(pw, ordinary[30:]); err != nil {
				t.Fatalf("write rest of stream: %v", err)
			}
			_ = pw.Close()
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after upstream EOF")
			}
			if out.err != nil {
				t.Fatalf("RelayProxy returned %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayProxy returned without closing owned resp.Body")
			}
			if diff := gocmp.Diff(outcome.Result{Status: http.StatusOK}, out.res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}
			_, _, finalBody, _, _ := rec.snapshot()
			if !strings.HasPrefix(finalBody, literalHeartbeat) {
				t.Errorf("final body did not start with heartbeat: %q", finalBody)
			}
			if diff := gocmp.Diff(ordinary, strings.ReplaceAll(finalBody, literalHeartbeat, "")); diff != "" {
				t.Errorf("stream with heartbeats stripped (-want +got):\n%s", diff)
			}
		})
	})

	t.Run("success: LM Studio overflow arriving after wait timeout stays in-band error on 200 stream", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			overflowFrame := lmStudioOverflowFrame(6017, 4096)
			pr, pw := io.Pipe()
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        src,
			}
			overflowCalls := 0
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(t.Context(), rec, up, sse.ProxyOptions{
					Provider:          sse.LMStudioProvider,
					AdvertisedModel:   "claude-lmstudio/qwen3-coder-30b",
					BackendModel:      "lmstudio/qwen3-coder-30b",
					HeartbeatInterval: -1,
					FirstEventWait:    50 * time.Millisecond,
					OnOverflow: func(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
						overflowCalls++
						return testEngineOverflowResponder(w, upstreamStatus, counts)
					},
				})
				done <- handlerOutcome{res: res, err: err, bodyClosed: src.closed.Load()}
			}()
			joined := false
			defer func() {
				if !joined {
					_ = pw.Close()
					_ = pr.Close()
					<-done
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				t.Fatalf("RelayProxy returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			time.Sleep(50 * time.Millisecond)
			synctest.Wait()

			if _, err := io.WriteString(pw, overflowFrame); err != nil {
				t.Fatalf("write late overflow: %v", err)
			}
			_ = pw.Close()
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after upstream EOF")
			}
			if out.err != nil {
				t.Fatalf("RelayProxy returned %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayProxy returned without closing owned resp.Body")
			}
			status, _, finalBody, _, _ := rec.snapshot()
			if status != http.StatusOK {
				t.Errorf("status = %d, want 200", status)
			}
			if overflowCalls != 0 {
				t.Errorf("OnOverflow called %d times after timeout, want 0", overflowCalls)
			}
			if diff := gocmp.Diff(overflowFrame, finalBody); diff != "" {
				t.Errorf("late overflow body (-want +got):\n%s", diff)
			}
		})
	})
}
