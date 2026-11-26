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
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/andybalholm/brotli"
	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func compressBytes(t *testing.T, encoding string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch encoding {
	case "":
		return raw
	case "gzip":
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
	case "deflate":
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			t.Fatalf("zlib write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("zlib close: %v", err)
		}
	case "br":
		w := brotli.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			t.Fatalf("brotli write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("brotli close: %v", err)
		}
	default:
		t.Fatalf("unsupported encoding %q", encoding)
	}
	return buf.Bytes()
}

func TestPassthroughSideBranchObservation(t *testing.T) {
	plainSSE := strings.Join([]string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-anthropic-sse\",\"model\":\"claude-fable-5\",\"usage\":{\"input_tokens\":9,\"output_tokens\":0}}}\n\n",
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n",
	}, "")
	wantEvents := []string{
		`{"type":"message_start","message":{"id":"msg-anthropic-sse","model":"claude-fable-5","usage":{"input_tokens":9,"output_tokens":0}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`,
	}

	tests := map[string]struct {
		status        int
		contentType   string
		encoding      string
		rawBody       []byte
		corruptWire   []byte
		maxObserve    int
		wantEvents    []string
		wantChunks    []byte
		wantOverflows int
	}{
		"success: uncompressed 2xx SSE observes events without altering bytes": {
			status:      http.StatusOK,
			contentType: "text/event-stream",
			rawBody:     []byte(plainSSE),
			wantEvents:  wantEvents,
		},
		"success: gzip 2xx SSE decodes on side branch while forwarding compressed bytes untouched": {
			status:      http.StatusOK,
			contentType: "text/event-stream",
			encoding:    "gzip",
			rawBody:     []byte(plainSSE),
			wantEvents:  wantEvents,
		},
		"success: deflate 2xx SSE decodes on side branch while forwarding compressed bytes untouched": {
			status:      http.StatusOK,
			contentType: "text/event-stream",
			encoding:    "deflate",
			rawBody:     []byte(plainSSE),
			wantEvents:  wantEvents,
		},
		"success: br 2xx SSE decodes on side branch while forwarding compressed bytes untouched": {
			status:      http.StatusOK,
			contentType: "text/event-stream",
			encoding:    "br",
			rawBody:     []byte(plainSSE),
			wantEvents:  wantEvents,
		},
		"error: corrupt gzip stream still forwards raw bytes untouched and marks usage overflow": {
			status:        http.StatusOK,
			contentType:   "text/event-stream",
			encoding:      "gzip",
			corruptWire:   []byte("not-a-valid-gzip-payload-at-all"),
			wantOverflows: 1,
		},
		"error: pending SSE frame over MaxObserveBytes marks overflow once while forwarding untouched bytes": {
			status:        http.StatusOK,
			contentType:   "text/event-stream",
			rawBody:       []byte("data: {\"type\":\"huge\",\"pad\":\"" + strings.Repeat("x", 200) + "\"}\n\ndata: {\"type\":\"huge2\",\"pad\":\"" + strings.Repeat("y", 200) + "\"}\n\n" + plainSSE),
			maxObserve:    128,
			wantOverflows: 1,
		},
		"success: 2xx non-SSE JSON records chunks via ObserveChunk": {
			status:      http.StatusOK,
			contentType: "application/json",
			rawBody:     []byte(`{"id":"msg_1","type":"message","usage":{"input_tokens":10,"output_tokens":4}}`),
			wantChunks:  []byte(`{"id":"msg_1","type":"message","usage":{"input_tokens":10,"output_tokens":4}}`),
		},
		"success: non-2xx overflow-shaped Anthropic error passes through verbatim without rewriting": {
			status:      http.StatusBadRequest,
			contentType: "application/json",
			rawBody:     []byte(`{"error":{"message":"input exceeds the context window of this model"}}`),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			wire := tt.corruptWire
			if wire == nil {
				wire = compressBytes(t, tt.encoding, tt.rawBody)
			}
			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				if tt.encoding != "" {
					w.Header().Set("Content-Encoding", tt.encoding)
				}
				w.Header().Set("Request-Id", "anthropic-req-1")
				w.Header().Set("Keep-Alive", "timeout=5")
				w.WriteHeader(tt.status)
				_, _ = w.Write(wire)
			}))
			t.Cleanup(upSrv.Close)

			usage := &usageSpy{}
			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, _ *http.Request, up *upstream.Response) (outcome.Result, error) {
				return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{
					MaxObserveBytes: tt.maxObserve,
					Usage:           usage,
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
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d (%q), want %d", resp.StatusCode, string(body), tt.status)
			}
			if diff := gocmp.Diff(wire, body); diff != "" {
				t.Errorf("forwarded bytes altered (-want +got):\n%s", diff)
			}
			if resp.Header.Get("Content-Encoding") != tt.encoding {
				t.Errorf("Content-Encoding = %q, want %q", resp.Header.Get("Content-Encoding"), tt.encoding)
			}
			if resp.Header.Get("Request-Id") != "anthropic-req-1" {
				t.Errorf("Request-Id = %q, want anthropic-req-1", resp.Header.Get("Request-Id"))
			}
			if resp.Header.Get("Keep-Alive") != "" {
				t.Errorf("Keep-Alive leaked: %q", resp.Header.Get("Keep-Alive"))
			}

			// Wait for the relay handler to finish synchronous observation before snapshotting.
			res, handlerErr := h.waitResult(t)
			if handlerErr != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil", handlerErr)
			}
			if diff := gocmp.Diff(outcome.Result{Status: tt.status}, res); diff != "" {
				t.Errorf("outcome.Result (-want +got):\n%s", diff)
			}

			events, chunks, totalBytes, overflows := usage.snapshot(t)
			if diff := gocmp.Diff(tt.wantEvents, events); diff != "" {
				t.Errorf("observed events (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantChunks, chunks); diff != "" {
				t.Errorf("observed chunks (-want +got):\n%s", diff)
			}
			if overflows != tt.wantOverflows {
				t.Errorf("overflow count = %d, want %d", overflows, tt.wantOverflows)
			}
			if tt.wantChunks == nil && totalBytes != int64(len(wire)) {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, len(wire))
			}
		})
	}

	// Default upstream (native Anthropic passthrough) must never touch sentry even when Sentry fields are set;
	// eligible 2xx proxy passthrough (ProxyProvider != "") observes SSE events in sentry without rearming a latch.
	sentryTests := map[string]struct {
		proxyProvider string
		status        int
		encoding      string
		preLatched    bool
		wantCrossing  bool
	}{
		"success: default upstream Anthropic passthrough never touches sentry even with high usage": {
			proxyProvider: "",
			status:        http.StatusOK,
			wantCrossing:  false,
		},
		"success: eligible proxy passthrough observes gzip 2xx SSE in sentry and arms unlatched session": {
			proxyProvider: "codex",
			status:        http.StatusOK,
			encoding:      "gzip",
			wantCrossing:  true,
		},
		"success: eligible proxy passthrough observes without ordinary-completed so latched refusal stays latched": {
			proxyProvider: "codex",
			status:        http.StatusOK,
			preLatched:    true,
			wantCrossing:  false,
		},
		"success: non-2xx proxy passthrough does not observe SSE in sentry": {
			proxyProvider: "codex",
			status:        http.StatusBadRequest,
			wantCrossing:  false,
		},
	}
	for name, tt := range sentryTests {
		t.Run(name, func(t *testing.T) {
			resolver := policy.NewResolver(policy.ResolverInputs{})
			target, err := sentry.ResolveTarget(resolver, "claude-gpt-5.6-sol", 100, 0, 0)
			if err != nil {
				t.Fatalf("ResolveTarget: %v", err)
			}
			session := sentry.SessionKey(true, "session-passthrough", nil)
			ordinaryReq := []byte(`{"model":"claude-gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			var state sentry.State
			if tt.preLatched {
				state.Observe(session, target, jsontext.Value(`{"type":"message_delta","usage":{"input_tokens":101}}`), true)
				if _, fired := state.Check(session, target, ordinaryReq); !fired {
					t.Fatal("precondition: expected initial crossing to latch")
				}
			}

			rawSSE := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":105}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			wire := compressBytes(t, tt.encoding, rawSSE)
			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tt.encoding != "" {
					w.Header().Set("Content-Encoding", tt.encoding)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write(wire)
			}))
			t.Cleanup(upSrv.Close)

			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, _ *http.Request, up *upstream.Response) (outcome.Result, error) {
				return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{
					ProxyProvider:     tt.proxyProvider,
					HeartbeatInterval: -1,
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
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d (%q), want %d", resp.StatusCode, string(body), tt.status)
			}
			if diff := gocmp.Diff(wire, body); diff != "" {
				t.Errorf("forwarded bytes altered (-want +got):\n%s", diff)
			}
			if _, handlerErr := h.waitResult(t); handlerErr != nil {
				t.Fatalf("RelayPassthrough error = %v", handlerErr)
			}

			crossing, fired := state.Check(session, target, ordinaryReq)
			if fired != tt.wantCrossing {
				t.Fatalf("sentry Check fired = %t (crossing=%+v), want %t", fired, crossing, tt.wantCrossing)
			}
			if tt.wantCrossing {
				if diff := gocmp.Diff(sentry.Crossing{Usage: 105, Trigger: 100}, crossing); diff != "" {
					t.Errorf("crossing (-want +got):\n%s", diff)
				}
			}
		})
	}
}

func TestPassthroughFlushAndNoHeartbeat(t *testing.T) {
	t.Run("success: loopback passthrough SSE flushes each chunk before next chunk arrives", func(t *testing.T) {
		first := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
		second := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		firstSeen := make(chan struct{})
		var releaseOnce sync.Once
		releaseSecond := func() { releaseOnce.Do(func() { close(firstSeen) }) }

		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			_, _ = io.WriteString(w, first)
			_ = rc.Flush()
			select {
			case <-firstSeen:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, second)
			_ = rc.Flush()
		}))
		// LIFO cleanup: release blocked upstream handler before closing upSrv.
		t.Cleanup(upSrv.Close)
		t.Cleanup(releaseSecond)

		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, _ *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{})
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
		var gotFirst strings.Builder
		for !strings.HasSuffix(gotFirst.String(), "\n\n") {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				t.Fatalf("reading first flushed passthrough frame: %v", readErr)
			}
			gotFirst.WriteString(line)
		}
		if diff := gocmp.Diff(first, gotFirst.String()); diff != "" {
			t.Errorf("first frame (-want +got):\n%s", diff)
		}
		releaseSecond()

		rest, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading second frame: %v", err)
		}
		if diff := gocmp.Diff(second, string(rest)); diff != "" {
			t.Errorf("second frame (-want +got):\n%s", diff)
		}
		if _, handlerErr := h.waitResult(t); handlerErr != nil {
			t.Fatalf("RelayPassthrough error = %v", handlerErr)
		}
	})

	t.Run("success: deterministic pipe proves raw passthrough flushes even a partial SSE frame immediately", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			rec := newStreamingRecorder()
			usage := &usageSpy{}
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        pr,
			}
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayPassthrough(t.Context(), rec, up, sse.PassthroughOptions{
					Usage: usage,
				})
				_ = pr.Close()
				done <- handlerOutcome{res: res, err: err}
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
				t.Fatalf("RelayPassthrough returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			part1 := "event: message_start\ndata: {\"type\":"
			part2 := "\"message_start\"}\n\n"
			if _, err := io.WriteString(pw, part1); err != nil {
				t.Fatalf("write partial chunk: %v", err)
			}
			synctest.Wait()

			_, _, body1, _, flushes1 := rec.snapshot()
			if diff := gocmp.Diff(part1, body1); diff != "" {
				t.Fatalf("partial SSE chunk was buffered instead of written immediately (-want +got):\n%s", diff)
			}
			if flushes1 < 1 {
				t.Fatalf("flushes after partial chunk = %d, want >= 1", flushes1)
			}

			if _, err := io.WriteString(pw, part2); err != nil {
				t.Fatalf("write remainder chunk: %v", err)
			}
			synctest.Wait()
			_, _, body2, _, flushes2 := rec.snapshot()
			if diff := gocmp.Diff(part1+part2, body2); diff != "" {
				t.Fatalf("completed frame mismatch (-want +got):\n%s", diff)
			}
			if flushes2 <= flushes1 {
				t.Fatalf("flushes did not advance after second chunk: before=%d after=%d", flushes1, flushes2)
			}

			_ = pw.Close()
			out := <-done
			joined = true
			if out.err != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil", out.err)
			}
			events, _, totalBytes, _ := usage.snapshot(t)
			if totalBytes != int64(len(part1)+len(part2)) {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, len(part1)+len(part2))
			}
			if diff := gocmp.Diff([]string{`{"type":"message_start"}`}, events); diff != "" {
				t.Errorf("observed events (-want +got):\n%s", diff)
			}
		})
	})

	t.Run("success: Anthropic passthrough never sends heartbeat comments during 60s silence", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        pr,
			}
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayPassthrough(t.Context(), rec, up, sse.PassthroughOptions{
					// Even with a positive HeartbeatInterval configured, empty ProxyProvider means Anthropic passthrough.
					HeartbeatInterval: 5 * time.Second,
				})
				_ = pr.Close()
				done <- handlerOutcome{res: res, err: err}
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
				t.Fatalf("RelayPassthrough returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			first := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
			if _, err := io.WriteString(pw, first); err != nil {
				t.Fatalf("write first: %v", err)
			}
			synctest.Wait()

			time.Sleep(60 * time.Second)
			synctest.Wait()
			_, _, body, _, _ := rec.snapshot()
			if strings.Contains(body, sse.HeartbeatComment) {
				t.Fatalf("Anthropic passthrough emitted heartbeat: %q", body)
			}

			_ = pw.Close()
			out := <-done
			joined = true
			if out.err != nil {
				t.Fatalf("RelayPassthrough returned %v, want nil", out.err)
			}
			_, _, finalBody, _, _ := rec.snapshot()
			if diff := gocmp.Diff(first, finalBody); diff != "" {
				t.Errorf("final body (-want +got):\n%s", diff)
			}
		})
	})
}

func TestStreamAbortAndDisconnect(t *testing.T) {
	const literalMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	t.Run("error: mid-stream upstream abort returns status 200, earlier data, and transport error within 1s of break", func(t *testing.T) {
		modes := map[string]struct {
			passthrough bool
		}{
			"error: proxy SSE upstream closes mid-chunk":       {passthrough: false},
			"error: passthrough SSE upstream closes mid-chunk": {passthrough: true},
		}
		for name, tt := range modes {
			t.Run(name, func(t *testing.T) {
				frame := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
				var lc net.ListenConfig
				ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("Listen: %v", err)
				}

				breakUpstream := make(chan struct{})
				var breakOnce sync.Once
				signalBreak := func() { breakOnce.Do(func() { close(breakUpstream) }) }

				var wg sync.WaitGroup
				// LIFO cleanup: signalBreak -> ln.Close -> wg.Wait so cleanup never hangs.
				t.Cleanup(wg.Wait)
				t.Cleanup(func() { _ = ln.Close() })
				t.Cleanup(signalBreak)

				wg.Go(func() {
					conn, acceptErr := ln.Accept()
					if acceptErr != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					buf := make([]byte, 4096)
					_, _ = conn.Read(buf)
					// Valid first chunk, then hold open until the client confirms receipt, then abruptly close without 0\r\n\r\n.
					_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(frame), frame)
					<-breakUpstream
				})

				h := startRelayServer(t, "http://"+ln.Addr().String(), func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
					if tt.passthrough {
						return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{
							Usage: &usageSpy{},
						})
					}
					return sse.RelayProxy(r.Context(), w, up, sse.ProxyOptions{
						Provider:          "codex",
						AdvertisedModel:   "claude-gpt-5.6-sol",
						BackendModel:      "gpt-5.6-sol",
						HeartbeatInterval: -1,
						Usage:             &usageSpy{},
					})
				})

				// Generous outer watchdog for setup; the 1s acceptance window is measured from signalBreak.
				reqCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
						t.Fatalf("reading initial message_start before break: %v (partial=%q)", readErr, first.String())
					}
					first.WriteString(line)
				}
				if !strings.Contains(first.String(), "message_start") {
					t.Fatalf("earlier message_start frame missing: %q", first.String())
				}

				// Trigger abrupt upstream close and measure the 1s abort window from this moment.
				breakAt := time.Now()
				signalBreak()

				type tailOut struct {
					data []byte
					err  error
				}
				tailCh := make(chan tailOut, 1)
				go func() {
					data, readErr := io.ReadAll(reader)
					tailCh <- tailOut{data: data, err: readErr}
				}()

				select {
				case out := <-tailCh:
					elapsed := time.Since(breakAt)
					if out.err == nil {
						t.Fatalf("expected transport error on truncated upstream stream, got clean EOF after %v (tail=%q)", elapsed, string(out.data))
					}
					if errors.Is(out.err, context.DeadlineExceeded) {
						t.Fatalf("client read hit outer deadline instead of aborting: %v", out.err)
					}
					if elapsed > time.Second {
						t.Errorf("client abort took %v from provider break, want <= 1s", elapsed)
					}
				case <-time.After(time.Second):
					cancel()
					<-tailCh
					t.Fatal("client response hung past 1s after provider break instead of aborting")
				}

				res, handlerErr := h.waitResult(t)
				if !errors.Is(handlerErr, http.ErrAbortHandler) {
					t.Errorf("relay error = %v, want wrapped http.ErrAbortHandler", handlerErr)
				}
				if res.Status != http.StatusOK || (res.StatusOverride != "upstream_error" && res.StatusOverride != "upstream_aborted") {
					t.Errorf("outcome.Result = %+v, want status 200 with upstream_error or upstream_aborted", res)
				}
				if res.Cancelled {
					t.Errorf("outcome.Cancelled = true on server-side upstream abort, want false")
				}
			})
		}
	})

	t.Run("success: proxy client disconnect stops attempted heartbeats and drains finite tail cleanly", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        pr,
			}
			clientCtx, cancelClient := context.WithCancel(t.Context())
			defer cancelClient()

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(clientCtx, rec, up, sse.ProxyOptions{
					Provider:        "codex",
					AdvertisedModel: "claude-gpt-5.6-sol",
					BackendModel:    "gpt-5.6-sol",
				})
				_ = pr.Close()
				done <- handlerOutcome{res: res, err: err}
			}()
			joined := false
			defer func() {
				if !joined {
					cancelClient()
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

			// Simulate client disconnecting mid-stream and record attempted write count before silence.
			rec.setWriteError(errors.New("broken pipe"))
			cancelClient()
			synctest.Wait()
			attemptsAtDisconnect := rec.attemptCount()

			// Heartbeat must not even be attempted after client disconnect.
			time.Sleep(40 * time.Second)
			synctest.Wait()
			if got := rec.attemptCount(); got != attemptsAtDisconnect {
				t.Fatalf("heartbeat write attempted after client disconnect: attempts=%d (before=%d) writes=%q",
					got, attemptsAtDisconnect, rec.attemptedSnapshot())
			}

			// Matching the measured Node proxy behavior, the finite upstream tail still drains to completion.
			tail := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"tail\"}}\n\n" + literalMessageStop
			if _, err := io.WriteString(pw, tail); err != nil {
				t.Fatalf("proxy upstream tail was closed early after client disconnect: %v", err)
			}
			_ = pw.Close()

			out := <-done
			joined = true
			if out.err != nil {
				t.Fatalf("RelayProxy returned %v after clean upstream drain, want nil", out.err)
			}
			if out.res.Status != http.StatusOK {
				t.Errorf("outcome.Status = %d, want 200", out.res.Status)
			}
		})
	})

	t.Run("success: loopback proxy client disconnect drains finite provider tail without eager close", func(t *testing.T) {
		firstFrame := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
		deltaFrame := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("a", 1024) + "\"}}\n\n"
		clientGone := make(chan struct{})
		var goneOnce sync.Once
		markClientGone := func() { goneOnce.Do(func() { close(clientGone) }) }

		type providerOutcome struct {
			tailErr error
			written int
		}
		providerDone := make(chan providerOutcome, 1)

		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			_, _ = io.WriteString(w, firstFrame)
			_ = rc.Flush()
			select {
			case <-clientGone:
			case <-r.Context().Done():
				providerDone <- providerOutcome{tailErr: r.Context().Err()}
				return
			}
			total := 0
			for range 64 {
				n, err := io.WriteString(w, deltaFrame)
				total += n
				if err != nil {
					providerDone <- providerOutcome{tailErr: err, written: total}
					return
				}
			}
			n, err := io.WriteString(w, literalMessageStop)
			total += n
			providerDone <- providerOutcome{tailErr: err, written: total}
		}))
		// LIFO cleanup: release blocked upstream handler before closing upSrv.
		t.Cleanup(upSrv.Close)
		t.Cleanup(markClientGone)

		handlerClientDone := make(chan struct{})
		h := startRelayServer(t, upSrv.URL, func(_ context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			go func() {
				<-r.Context().Done()
				close(handlerClientDone)
			}()
			return sse.RelayProxy(r.Context(), w, up, sse.ProxyOptions{
				Provider:          "codex",
				AdvertisedModel:   "claude-gpt-5.6-sol",
				BackendModel:      "gpt-5.6-sol",
				HeartbeatInterval: -1,
			})
		})

		// Use a raw TCP connection so closing it immediately severs the downstream connection.
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", h.server.Listener.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 0\r\n\r\n"); err != nil {
			t.Fatalf("write request: %v", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
		if err != nil {
			t.Fatalf("ReadResponse: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
		}
		bodyReader := bufio.NewReader(resp.Body)
		var first strings.Builder
		for !strings.HasSuffix(first.String(), "\n\n") {
			line, readErr := bodyReader.ReadString('\n')
			if readErr != nil {
				t.Fatalf("reading first frame: %v", readErr)
			}
			first.WriteString(line)
		}
		// Disconnect client mid-stream.
		_ = conn.Close()
		select {
		case <-handlerClientDone:
		case <-time.After(2 * time.Second):
			t.Fatal("relay handler did not observe client disconnect")
		}
		markClientGone()

		select {
		case po := <-providerDone:
			wantLen := 64*len(deltaFrame) + len(literalMessageStop)
			if po.tailErr != nil || po.written != wantLen {
				t.Fatalf("provider tail failed: err=%v written=%d want=%d", po.tailErr, po.written, wantLen)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for proxy relay to drain finite provider tail")
		}
		res, handlerErr := h.waitResult(t)
		if handlerErr != nil {
			t.Fatalf("RelayProxy error = %v, want nil", handlerErr)
		}
		if res.Status != http.StatusOK {
			t.Errorf("outcome.Status = %d, want 200", res.Status)
		}
	})

	t.Run("success: default-upstream passthrough disconnect applies back-pressure without eager close until service shutdown", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			rec := newStreamingRecorder()
			up := &upstream.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":     []string{"text/event-stream"},
					"Content-Encoding": []string{"gzip"},
				},
				RelayHeader: http.Header{
					"Content-Type":     []string{"text/event-stream"},
					"Content-Encoding": []string{"gzip"},
				},
				Body: pr,
			}
			serviceCtx, cancelService := context.WithCancel(t.Context())
			defer cancelService()

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayPassthrough(serviceCtx, rec, up, sse.PassthroughOptions{
					Usage: &usageSpy{},
				})
				_ = pr.Close()
				done <- handlerOutcome{res: res, err: err}
			}()
			joined := false
			defer func() {
				if !joined {
					cancelService()
					_ = pw.Close()
					_ = pr.Close()
					<-done
				}
			}()

			synctest.Wait()
			select {
			case out := <-done:
				joined = true
				t.Fatalf("RelayPassthrough returned early: res=%+v err=%v", out.res, out.err)
			default:
			}

			// First chunk flows normally to the client.
			chunk1 := compressBytes(t, "gzip", []byte("data: {\"type\":\"message_start\"}\n\n"))
			if _, err := pw.Write(chunk1); err != nil {
				t.Fatalf("write chunk1: %v", err)
			}
			synctest.Wait()

			// Client disconnects; second chunk hits a client write error.
			rec.setWriteError(errors.New("broken pipe"))
			if _, err := pw.Write([]byte("more-upstream-bytes")); err != nil {
				t.Fatalf("write chunk2: %v", err)
			}
			synctest.Wait()

			// A third upstream write must block on back-pressure (neither drained nor eagerly closed).
			write3Done := make(chan error, 1)
			go func() {
				_, err := pw.Write([]byte("backpressured-chunk"))
				write3Done <- err
			}()
			synctest.Wait()

			select {
			case err := <-write3Done:
				t.Fatalf("default-upstream passthrough did not hold back-pressure after client disconnect (write3 returned %v)", err)
			case out := <-done:
				joined = true
				t.Fatalf("default-upstream passthrough exited eagerly before service shutdown: res=%+v err=%v", out.res, out.err)
			default:
			}

			// Service shutdown bounds the paused stream, closes resp.Body, joins the gzip observer, and returns cleanly.
			cancelService()
			synctest.Wait()

			if write3Err := <-write3Done; !errors.Is(write3Err, io.ErrClosedPipe) {
				t.Errorf("write3 after service shutdown err = %v, want io.ErrClosedPipe", write3Err)
			}
			_ = pw.Close()
			out := <-done
			joined = true
			if out.err != nil {
				t.Fatalf("RelayPassthrough after service shutdown err = %v, want nil", out.err)
			}
			if !out.res.Cancelled {
				t.Errorf("outcome.Cancelled = false after client disconnect + shutdown, want true")
			}
		})
	})

	t.Run("success: loopback default-upstream passthrough disconnect stalls provider under back-pressure until service shutdown", func(t *testing.T) {
		firstFrame := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
		block := bytes.Repeat([]byte("a"), 64*1024)
		clientDisconnected := make(chan struct{})
		var discOnce sync.Once
		markDisconnected := func() { discOnce.Do(func() { close(clientDisconnected) }) }

		var providerClosedEagerly atomic.Bool
		var providerDrainedAll atomic.Bool
		providerDone := make(chan struct{})

		upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(providerDone)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			rc := http.NewResponseController(w)
			_, _ = io.WriteString(w, firstFrame)
			_ = rc.Flush()
			select {
			case <-clientDisconnected:
			case <-r.Context().Done():
				return
			}
			// Offer up to 64 MiB in 64 KiB blocks so loopback socket buffers saturate and block on back-pressure.
			for range 1024 {
				if _, err := w.Write(block); err != nil {
					providerClosedEagerly.Store(true)
					return
				}
				_ = rc.Flush()
			}
			providerDrainedAll.Store(true)
		}))
		// LIFO cleanup: release blocked upstream handler before closing upSrv.
		t.Cleanup(upSrv.Close)
		t.Cleanup(markDisconnected)

		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, _ *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{})
		})

		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", h.server.Listener.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 0\r\n\r\n"); err != nil {
			t.Fatalf("write request: %v", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
		if err != nil {
			t.Fatalf("ReadResponse: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
		}
		bodyReader := bufio.NewReader(resp.Body)
		var first strings.Builder
		for !strings.HasSuffix(first.String(), "\n\n") {
			line, readErr := bodyReader.ReadString('\n')
			if readErr != nil {
				t.Fatalf("reading first frame: %v", readErr)
			}
			first.WriteString(line)
		}

		// Disconnect client mid-stream and let the provider push into the stalled connection.
		_ = conn.Close()
		markDisconnected()

		// Observe a short window (no unmeasured timeout): provider must neither receive an eager TCP close
		// nor drain the full 64 MiB payload.
		select {
		case <-providerDone:
			if providerClosedEagerly.Load() {
				t.Fatal("default-upstream passthrough eagerly closed the provider connection on client disconnect")
			}
			if providerDrainedAll.Load() {
				t.Fatal("default-upstream passthrough drained 64 MiB instead of applying back-pressure")
			}
			t.Fatal("provider exited unexpectedly before service shutdown")
		case <-time.After(200 * time.Millisecond):
		}

		// Trigger bounded service shutdown and verify both relay and provider exit cleanly.
		h.cancelService()
		select {
		case <-providerDone:
		case <-time.After(5 * time.Second):
			t.Fatal("provider did not unblock after service shutdown")
		}
		res, handlerErr := h.waitResult(t)
		if handlerErr != nil {
			t.Fatalf("RelayPassthrough error after service shutdown = %v, want nil", handlerErr)
		}
		if !res.Cancelled {
			t.Errorf("outcome.Cancelled = false after client disconnect + service shutdown, want true")
		}
	})
}
