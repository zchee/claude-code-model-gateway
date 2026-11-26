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
	"os"
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
			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
				return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{
					MaxObserveBytes: tt.maxObserve,
					ClientDone:      r.Context().Done(),
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
			// Successful non-SSE JSON accounts bytes through ObserveChunk alone; calling NoteResponseBytes would double-count.
			wantBytes := int64(len(wire))
			if tt.wantChunks != nil {
				wantBytes = 0
			}
			if totalBytes != wantBytes {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, wantBytes)
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

			h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
				return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{
					ProxyProvider:     tt.proxyProvider,
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

		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{
				ClientDone: r.Context().Done(),
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
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			usage := &usageSpy{}
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        src,
			}
			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayPassthrough(t.Context(), rec, up, sse.PassthroughOptions{
					Usage: usage,
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
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayPassthrough did not return after upstream EOF")
			}
			if out.err != nil {
				t.Fatalf("RelayPassthrough error = %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayPassthrough returned without closing owned resp.Body")
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
		const literalHeartbeat = ": ping\n\n"
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
				res, err := sse.RelayPassthrough(t.Context(), rec, up, sse.PassthroughOptions{
					// Even with a positive HeartbeatInterval configured, empty ProxyProvider means Anthropic passthrough.
					HeartbeatInterval: 5 * time.Second,
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
			if strings.Contains(body, literalHeartbeat) {
				t.Fatalf("Anthropic passthrough emitted heartbeat: %q", body)
			}

			_ = pw.Close()
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayPassthrough did not return after upstream EOF")
			}
			if out.err != nil {
				t.Fatalf("RelayPassthrough returned %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayPassthrough returned without closing owned resp.Body")
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
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					buf := make([]byte, 4096)
					_, _ = conn.Read(buf)
					// Valid first chunk, then hold open until the client confirms receipt, then abruptly close without 0\r\n\r\n.
					_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(frame), frame)
					_ = conn.SetDeadline(time.Time{})
					<-breakUpstream
				})

				h := startRelayServer(t, "http://"+ln.Addr().String(), func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
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
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			usage := &usageSpy{}
			up := &upstream.Response{
				StatusCode:  http.StatusOK,
				Header:      http.Header{"Content-Type": []string{"text/event-stream"}},
				RelayHeader: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:        src,
			}
			clientDone := make(chan struct{})

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayProxy(t.Context(), rec, up, sse.ProxyOptions{
					Provider:        "codex",
					AdvertisedModel: "claude-gpt-5.6-sol",
					BackendModel:    "gpt-5.6-sol",
					ClientDone:      clientDone,
					Usage:           usage,
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

			// Signal client disconnect via ClientDone with NO write error configured on rec:
			// heartbeats and all subsequent client writes must stop immediately without needing a failed write.
			close(clientDone)
			synctest.Wait()
			attemptsAtDisconnect := rec.attemptCount()

			time.Sleep(40 * time.Second)
			synctest.Wait()
			if got := rec.attemptCount(); got != attemptsAtDisconnect {
				t.Fatalf("heartbeat write attempted after ClientDone: attempts=%d (before=%d) writes=%q",
					got, attemptsAtDisconnect, rec.attemptedSnapshot())
			}
			if src.closed.Load() {
				t.Fatal("proxy relay closed upstream body early after ClientDone")
			}

			// Finite upstream tail still drains to completion while observing usage and writing nothing more to w.
			tailDelta := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":12,\"output_tokens\":4}}\n\n"
			tail := tailDelta + literalMessageStop
			if _, err := io.WriteString(pw, tail); err != nil {
				t.Fatalf("proxy upstream tail was closed early after ClientDone: %v", err)
			}
			_ = pw.Close()
			synctest.Wait()

			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after draining finite upstream tail")
			}
			if out.err != nil {
				t.Fatalf("RelayProxy returned %v after clean upstream drain, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayProxy returned without closing owned resp.Body")
			}
			if out.res.Status != http.StatusOK {
				t.Errorf("outcome.Status = %d, want 200", out.res.Status)
			}
			if out.res.Cancelled {
				t.Errorf("outcome.Cancelled = true, want false (relays never set Cancelled)")
			}
			if got := rec.attemptCount(); got != attemptsAtDisconnect {
				t.Errorf("client writes attempted during tail drain after ClientDone: attempts=%d (before=%d) writes=%q",
					got, attemptsAtDisconnect, rec.attemptedSnapshot())
			}
			events, _, totalBytes, _ := usage.snapshot(t)
			if totalBytes != int64(len(first)+len(tail)) {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, len(first)+len(tail))
			}
			wantEvents := []string{
				`{"type":"message_start"}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"output_tokens":4}}`,
				`{"type":"message_stop"}`,
			}
			if diff := gocmp.Diff(wantEvents, events); diff != "" {
				t.Errorf("usage observed events during tail drain (-want +got):\n%s", diff)
			}
		})
	})

	t.Run("success: proxy client write failure stops further heartbeats and drains finite tail cleanly", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			src := &trackedReadCloser{ReadCloser: pr}
			rec := newStreamingRecorder()
			usage := &usageSpy{}
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
					AdvertisedModel: "claude-gpt-5.6-sol",
					BackendModel:    "gpt-5.6-sol",
					Usage:           usage,
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

			// With nil ClientDone, a failing heartbeat write at 20s marks the client gone;
			// no further heartbeats or tail writes may be attempted afterward.
			attemptsBeforeFail := rec.attemptCount()
			rec.setWriteError(errors.New("broken pipe"))
			time.Sleep(20 * time.Second)
			synctest.Wait()
			attemptsAfterFail := rec.attemptCount()
			if attemptsAfterFail != attemptsBeforeFail+1 {
				t.Fatalf("expected exactly one failing heartbeat write at 20s: before=%d after=%d writes=%q",
					attemptsBeforeFail, attemptsAfterFail, rec.attemptedSnapshot())
			}

			time.Sleep(40 * time.Second)
			synctest.Wait()
			if got := rec.attemptCount(); got != attemptsAfterFail {
				t.Fatalf("additional heartbeat write attempted after write failure: attempts=%d (afterFirstFail=%d) writes=%q",
					got, attemptsAfterFail, rec.attemptedSnapshot())
			}

			tail := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":5}}\n\n" + literalMessageStop
			if _, err := io.WriteString(pw, tail); err != nil {
				t.Fatalf("proxy upstream tail closed early after write failure: %v", err)
			}
			_ = pw.Close()
			synctest.Wait()

			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayProxy did not return after draining finite upstream tail")
			}
			if out.err != nil {
				t.Fatalf("RelayProxy returned %v, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayProxy returned without closing owned resp.Body")
			}
			if out.res.Cancelled {
				t.Errorf("outcome.Cancelled = true, want false (relays never set Cancelled)")
			}
			if got := rec.attemptCount(); got != attemptsAfterFail {
				t.Errorf("tail writes attempted after write failure: attempts=%d (afterFirstFail=%d)", got, attemptsAfterFail)
			}
			_, _, totalBytes, _ := usage.snapshot(t)
			if totalBytes != int64(len(first)+len(tail)) {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, len(first)+len(tail))
			}
		})
	})

	t.Run("success: proxy passthrough ClientDone stops heartbeats without a failed write", func(t *testing.T) {
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
			serviceCtx, cancelService := context.WithCancel(t.Context())
			defer cancelService()
			clientDone := make(chan struct{})

			done := make(chan handlerOutcome, 1)
			go func() {
				res, err := sse.RelayPassthrough(serviceCtx, rec, up, sse.PassthroughOptions{
					ProxyProvider: "codex",
					ClientDone:    clientDone,
				})
				done <- handlerOutcome{res: res, err: err, bodyClosed: src.closed.Load()}
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

			first := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
			if _, err := io.WriteString(pw, first); err != nil {
				t.Fatalf("write first: %v", err)
			}
			synctest.Wait()

			close(clientDone)
			synctest.Wait()
			attemptsAtDisconnect := rec.attemptCount()

			time.Sleep(40 * time.Second)
			synctest.Wait()
			if got := rec.attemptCount(); got != attemptsAtDisconnect {
				t.Fatalf("proxy passthrough attempted heartbeat after ClientDone: attempts=%d (before=%d) writes=%q",
					got, attemptsAtDisconnect, rec.attemptedSnapshot())
			}
			if src.closed.Load() {
				t.Fatal("proxy passthrough eagerly closed upstream body after ClientDone")
			}

			cancelService()
			synctest.Wait()
			var out handlerOutcome
			select {
			case out = <-done:
				joined = true
			default:
				t.Fatal("RelayPassthrough did not return after service context cancellation")
			}
			_ = pw.Close()
			if out.err != nil {
				t.Fatalf("RelayPassthrough returned %v after service shutdown, want nil", out.err)
			}
			if !out.bodyClosed {
				t.Errorf("RelayPassthrough returned without closing owned resp.Body")
			}
			if out.res.Cancelled {
				t.Errorf("outcome.Cancelled = true, want false (relays never set Cancelled)")
			}
		})
	})

	t.Run("success: loopback proxy client disconnect drains finite provider tail without eager close", func(t *testing.T) {
		firstFrame := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
		// 512 x 64 KiB = 32 MiB finite tail: exceeds loopback kernel socket buffers so the provider
		// cannot finish unless the relay actively drains resp.Body, verified also by usage byte count.
		const tailBlockCount = 512
		commentBlock := ": " + strings.Repeat("a", 64*1024-4) + "\n\n"
		tailDelta := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":42,\"output_tokens\":7}}\n\n"
		tailFinal := tailDelta + literalMessageStop
		wantTailLen := tailBlockCount*len(commentBlock) + len(tailFinal)

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
			for range tailBlockCount {
				n, err := io.WriteString(w, commentBlock)
				total += n
				if err != nil {
					providerDone <- providerOutcome{tailErr: err, written: total}
					return
				}
				_ = rc.Flush()
			}
			n, err := io.WriteString(w, tailFinal)
			total += n
			_ = rc.Flush()
			providerDone <- providerOutcome{tailErr: err, written: total}
		}))
		// LIFO cleanup: release blocked upstream handler before closing upSrv.
		t.Cleanup(upSrv.Close)
		t.Cleanup(markClientGone)

		usage := &usageSpy{}
		handlerClientDone := make(chan struct{})
		h := startRelayServer(t, upSrv.URL, func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			go func() {
				<-r.Context().Done()
				close(handlerClientDone)
			}()
			return sse.RelayProxy(serviceCtx, w, up, sse.ProxyOptions{
				Provider:          "codex",
				AdvertisedModel:   "claude-gpt-5.6-sol",
				BackendModel:      "gpt-5.6-sol",
				HeartbeatInterval: -1,
				ClientDone:        r.Context().Done(),
				Usage:             usage,
			})
		})

		// Use a raw TCP connection with an explicit setup deadline so closing it immediately severs the client.
		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", h.server.Listener.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
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
			if po.tailErr != nil || po.written != wantTailLen {
				t.Fatalf("provider tail failed: err=%v written=%d want=%d", po.tailErr, po.written, wantTailLen)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for proxy relay to drain 32 MiB finite provider tail")
		}
		res, handlerErr := h.waitResult(t)
		if handlerErr != nil {
			t.Fatalf("RelayProxy error = %v, want nil", handlerErr)
		}
		if res.Status != http.StatusOK {
			t.Errorf("outcome.Status = %d, want 200", res.Status)
		}
		if res.Cancelled {
			t.Errorf("outcome.Cancelled = true, want false (relays never set Cancelled)")
		}
		events, _, totalBytes, _ := usage.snapshot(t)
		wantTotalBytes := int64(len(firstFrame) + wantTailLen)
		if totalBytes != wantTotalBytes {
			t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, wantTotalBytes)
		}
		wantEvents := []string{
			`{"type":"message_start"}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":42,"output_tokens":7}}`,
			`{"type":"message_stop"}`,
		}
		if diff := gocmp.Diff(wantEvents, events); diff != "" {
			t.Errorf("usage observed events (-want +got):\n%s", diff)
		}
	})

	t.Run("success: default-upstream passthrough disconnect applies back-pressure without eager close until service shutdown", func(t *testing.T) {
		triggers := map[string]struct {
			useClientDone bool
		}{
			"success: ClientDone stops client writes and holds back-pressure without failed write": {useClientDone: true},
			"success: client write error stops reading and holds back-pressure until shutdown":     {useClientDone: false},
		}
		for name, tt := range triggers {
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					pr, pw := io.Pipe()
					src := &trackedReadCloser{ReadCloser: pr}
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
						Body: src,
					}
					serviceCtx, cancelService := context.WithCancel(t.Context())
					defer cancelService()

					var clientDone chan struct{}
					if tt.useClientDone {
						clientDone = make(chan struct{})
					}

					done := make(chan handlerOutcome, 1)
					go func() {
						res, err := sse.RelayPassthrough(serviceCtx, rec, up, sse.PassthroughOptions{
							ClientDone: clientDone,
							Usage:      &usageSpy{},
						})
						done <- handlerOutcome{res: res, err: err, bodyClosed: src.closed.Load()}
					}()
					joined := false
					feederDone := make(chan error, 1)
					feederStarted := false
					feederJoined := true
					defer func() {
						cancelService()
						_ = pw.Close()
						_ = pr.Close()
						if feederStarted && !feederJoined {
							<-feederDone
						}
						if !joined {
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

					// Produce an open, flushed gzip stream so the side-branch gzip decoder stays live
					// across disconnect and must be cleanly torn down on service shutdown.
					var gzBuf bytes.Buffer
					gz := gzip.NewWriter(&gzBuf)
					if _, err := gz.Write([]byte("data: {\"type\":\"message_start\"}\n\n")); err != nil {
						t.Fatalf("gzip write: %v", err)
					}
					if err := gz.Flush(); err != nil {
						t.Fatalf("gzip flush: %v", err)
					}
					if _, err := pw.Write(gzBuf.Bytes()); err != nil {
						t.Fatalf("write chunk1: %v", err)
					}
					synctest.Wait()

					attemptsBefore := rec.attemptCount()
					if tt.useClientDone {
						close(clientDone)
						synctest.Wait()
					} else {
						rec.setWriteError(errors.New("broken pipe"))
					}

					// Offer multiple subsequent chunks: the relay must stop reading resp.Body so the feeder
					// blocks on back-pressure (neither draining all chunks nor hitting eager io.ErrClosedPipe).
					feederStarted = true
					feederJoined = false
					go func() {
						for range 8 {
							if _, err := pw.Write([]byte("backpressured-chunk")); err != nil {
								feederDone <- err
								return
							}
						}
						feederDone <- nil
					}()
					synctest.Wait()

					select {
					case err := <-feederDone:
						feederJoined = true
						t.Fatalf("default-upstream passthrough did not hold back-pressure after disconnect (feeder returned %v)", err)
					case out := <-done:
						joined = true
						t.Fatalf("default-upstream passthrough exited eagerly before service shutdown: res=%+v err=%v", out.res, out.err)
					default:
					}
					if src.closed.Load() {
						t.Fatal("default-upstream passthrough eagerly closed resp.Body before service shutdown")
					}
					if tt.useClientDone {
						if got := rec.attemptCount(); got != attemptsBefore {
							t.Fatalf("client write attempted after ClientDone: attempts=%d (before=%d) writes=%q",
								got, attemptsBefore, rec.attemptedSnapshot())
						}
					} else if got := rec.attemptCount(); got != attemptsBefore+1 {
						t.Fatalf("expected exactly one failed client write to detect disconnect: attempts=%d (before=%d) writes=%q",
							got, attemptsBefore, rec.attemptedSnapshot())
					}

					// Service shutdown bounds the paused stream, closes resp.Body, joins the live gzip observer, and returns cleanly.
					cancelService()
					synctest.Wait()

					select {
					case feedErr := <-feederDone:
						feederJoined = true
						if !errors.Is(feedErr, io.ErrClosedPipe) {
							t.Errorf("feeder error after service shutdown = %v, want io.ErrClosedPipe", feedErr)
						}
					default:
						t.Fatal("feeder did not unblock with io.ErrClosedPipe when service shutdown closed resp.Body")
					}
					_ = pw.Close()
					var out handlerOutcome
					select {
					case out = <-done:
						joined = true
					default:
						t.Fatal("RelayPassthrough did not return after service shutdown")
					}
					if out.err != nil {
						t.Fatalf("RelayPassthrough after service shutdown err = %v, want nil", out.err)
					}
					if !out.bodyClosed {
						t.Errorf("RelayPassthrough returned without closing owned resp.Body")
					}
					if out.res.Cancelled {
						t.Errorf("outcome.Cancelled = true, want false (relays never set Cancelled)")
					}
				})
			})
		}
	})

	t.Run("success: loopback default-upstream passthrough disconnect stalls provider under back-pressure until service shutdown", func(t *testing.T) {
		firstFrame := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
		payload := bytes.Repeat([]byte("a"), 64*1024)
		chunkedBlock := fmt.Appendf(nil, "%x\r\n%s\r\n", len(payload), payload)

		var lc net.ListenConfig
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}

		clientDisconnected := make(chan struct{})
		var discOnce sync.Once
		markDisconnected := func() { discOnce.Do(func() { close(clientDisconnected) }) }

		providerStalled := make(chan struct{})
		providerDone := make(chan struct{})
		var providerClosedEagerly atomic.Bool
		var providerDrainedAll atomic.Bool
		var providerCloseErrMu sync.Mutex
		var providerCloseErr error

		var wg sync.WaitGroup
		// LIFO cleanup: markDisconnected -> ln.Close -> wg.Wait (registered before startRelayServer so
		// relayHarness cleanup cancels serviceCtx and closes sockets first, unblocking the provider conn).
		t.Cleanup(wg.Wait)
		t.Cleanup(func() { _ = ln.Close() })
		t.Cleanup(markDisconnected)

		wg.Go(func() {
			defer close(providerDone)
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			defer func() { _ = conn.Close() }()

			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			req, readReqErr := http.ReadRequest(bufio.NewReader(conn))
			if readReqErr != nil {
				return
			}
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()

			_, writeErr := fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(firstFrame), firstFrame)
			if writeErr != nil {
				return
			}
			_ = conn.SetDeadline(time.Time{})

			<-clientDisconnected

			// Write 64 KiB chunked blocks with a 250ms per-write deadline until kernel TCP buffers
			// saturate and conn.Write returns os.ErrDeadlineExceeded, proving a pending provider write
			// on an open socket rather than relying on a passive silence sleep.
			stalled := false
			for range 2048 {
				_ = conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
				_, wErr := conn.Write(chunkedBlock)
				if wErr == nil {
					continue
				}
				if errors.Is(wErr, os.ErrDeadlineExceeded) {
					stalled = true
					break
				}
				providerClosedEagerly.Store(true)
				return
			}
			if !stalled {
				providerDrainedAll.Store(true)
				return
			}
			_ = conn.SetWriteDeadline(time.Time{})
			close(providerStalled)

			// Wait for the relay's service shutdown to close the upstream TCP connection.
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var one [1]byte
			_, rErr := conn.Read(one[:])
			providerCloseErrMu.Lock()
			providerCloseErr = rErr
			providerCloseErrMu.Unlock()
		})

		handlerClientDone := make(chan struct{})
		h := startRelayServer(t, "http://"+ln.Addr().String(), func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error) {
			go func() {
				<-r.Context().Done()
				close(handlerClientDone)
			}()
			return sse.RelayPassthrough(serviceCtx, w, up, sse.PassthroughOptions{
				ClientDone: r.Context().Done(),
			})
		})

		conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", h.server.Listener.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
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

		// Disconnect client mid-stream, wait for the relay handler to see ClientDone, then let provider write.
		_ = conn.Close()
		select {
		case <-handlerClientDone:
		case <-time.After(2 * time.Second):
			t.Fatal("relay handler did not observe client disconnect")
		}
		markDisconnected()

		// Require positive kernel-level proof of a pending provider write under back-pressure.
		select {
		case <-providerStalled:
		case <-providerDone:
			if providerClosedEagerly.Load() {
				t.Fatal("default-upstream passthrough eagerly closed the provider connection on client disconnect")
			}
			if providerDrainedAll.Load() {
				t.Fatal("default-upstream passthrough drained 128 MiB instead of applying back-pressure")
			}
			t.Fatal("provider exited unexpectedly before stalling under back-pressure")
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for provider write to stall under back-pressure")
		}
		select {
		case <-providerDone:
			t.Fatal("provider connection closed before service shutdown")
		default:
		}

		// Trigger bounded service shutdown and verify both relay and provider exit cleanly.
		h.cancelService()
		select {
		case <-providerDone:
		case <-time.After(5 * time.Second):
			t.Fatal("provider did not observe peer close after service shutdown")
		}
		providerCloseErrMu.Lock()
		gotCloseErr := providerCloseErr
		providerCloseErrMu.Unlock()
		if gotCloseErr == nil || errors.Is(gotCloseErr, os.ErrDeadlineExceeded) {
			t.Errorf("provider read after service shutdown = %v, want peer close error/EOF", gotCloseErr)
		}

		res, handlerErr := h.waitResult(t)
		if handlerErr != nil {
			t.Fatalf("RelayPassthrough error after service shutdown = %v, want nil", handlerErr)
		}
		if res.Cancelled {
			t.Errorf("outcome.Cancelled = true, want false (relays never set Cancelled)")
		}
	})
}
