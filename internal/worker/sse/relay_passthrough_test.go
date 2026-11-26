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

	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
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
		"error: pending SSE frame over MaxObserveBytes marks overflow while forwarding untouched bytes": {
			status:        http.StatusOK,
			contentType:   "text/event-stream",
			rawBody:       []byte("data: {\"type\":\"huge\",\"pad\":\"" + strings.Repeat("x", 200) + "\"}\n\n" + plainSSE),
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
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				if tt.encoding != "" {
					w.Header().Set("Content-Encoding", tt.encoding)
				}
				w.Header().Set("Request-Id", "anthropic-req-1")
				w.Header().Set("Keep-Alive", "timeout=5")
				w.WriteHeader(tt.status)
				_, _ = w.Write(wire)
			}))
			t.Cleanup(upstream.Close)

			usage := &recordingUsage{}
			telemetry := &recordingTelemetry{}
			relaySrv, client := startRelayServer(t, upstream.URL, func(w http.ResponseWriter, r *http.Request, up *http.Response) error {
				return sse.RelayPassthrough(r.Context(), w, up, sse.PassthroughOptions{
					MaxObserveBytes: tt.maxObserve,
					Usage:           usage,
					Telemetry:       telemetry,
				})
			})

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, relaySrv.URL+"/v1/messages", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext: %v", err)
			}
			resp, err := client.Do(req)
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

			events, chunks, totalBytes, overflows := usage.snapshot()
			if diff := gocmp.Diff(tt.wantEvents, events); diff != "" {
				t.Errorf("observed events (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantChunks, chunks); diff != "" {
				t.Errorf("observed chunks (-want +got):\n%s", diff)
			}
			if overflows != tt.wantOverflows {
				t.Errorf("overflow count = %d, want %d", overflows, tt.wantOverflows)
			}
			if tt.wantChunks == nil && totalBytes != len(wire) {
				t.Errorf("NoteResponseBytes = %d, want %d", totalBytes, len(wire))
			}
			wantTelemetry := []recordedTelemetryCall{{Status: tt.status, Override: ""}}
			if diff := gocmp.Diff(wantTelemetry, telemetry.snapshot()); diff != "" {
				t.Errorf("telemetry (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPassthroughFlushAndNoHeartbeat(t *testing.T) {
	t.Run("success: passthrough SSE flushes each chunk before next chunk arrives", func(t *testing.T) {
		first := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
		second := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		firstSeen := make(chan struct{})

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		t.Cleanup(upstream.Close)

		relaySrv, client := startRelayServer(t, upstream.URL, func(w http.ResponseWriter, r *http.Request, up *http.Response) error {
			return sse.RelayPassthrough(r.Context(), w, up, sse.PassthroughOptions{})
		})

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, relaySrv.URL+"/v1/messages", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		resp, err := client.Do(req)
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
		close(firstSeen)

		rest, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading second frame: %v", err)
		}
		if diff := gocmp.Diff(second, string(rest)); diff != "" {
			t.Errorf("second frame (-want +got):\n%s", diff)
		}
	})

	t.Run("success: Anthropic passthrough never sends heartbeat comments during 60s silence", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			rec := newStreamingRecorder()
			up := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       pr,
			}
			done := make(chan error, 1)
			go func() {
				err := sse.RelayPassthrough(t.Context(), rec, up, sse.PassthroughOptions{
					// Even with a positive HeartbeatInterval configured, empty ProxyProvider means Anthropic passthrough.
					HeartbeatInterval: 5 * time.Second,
				})
				_ = pr.Close()
				done <- err
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
			case err := <-done:
				joined = true
				t.Fatalf("RelayPassthrough returned early: %v", err)
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
			err := <-done
			joined = true
			if err != nil {
				t.Fatalf("RelayPassthrough returned %v, want nil", err)
			}
			_, _, finalBody, _, _ := rec.snapshot()
			if diff := gocmp.Diff(first, finalBody); diff != "" {
				t.Errorf("final body (-want +got):\n%s", diff)
			}
		})
	})
}

func TestStreamAbortAndDisconnect(t *testing.T) {
	t.Run("error: mid-stream upstream abort returns status 200, earlier data, and transport error within 1s", func(t *testing.T) {
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
				t.Cleanup(func() { _ = ln.Close() })

				var wg sync.WaitGroup
				wg.Go(func() {
					conn, acceptErr := ln.Accept()
					if acceptErr != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					buf := make([]byte, 4096)
					_, _ = conn.Read(buf)
					// Valid first chunk followed by abrupt close with no terminating 0\r\n\r\n chunk.
					_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(frame), frame)
				})
				t.Cleanup(wg.Wait)

				telemetry := &recordingTelemetry{}
				relaySrv, client := startRelayServer(t, "http://"+ln.Addr().String(), func(w http.ResponseWriter, r *http.Request, up *http.Response) error {
					if tt.passthrough {
						return sse.RelayPassthrough(r.Context(), w, up, sse.PassthroughOptions{
							Usage:     &recordingUsage{},
							Telemetry: telemetry,
						})
					}
					return sse.RelayProxy(r.Context(), w, up, sse.ProxyOptions{
						Provider:          "codex",
						AdvertisedModel:   "claude-gpt-5.6-sol",
						BackendModel:      "gpt-5.6-sol",
						HeartbeatInterval: -1,
						Usage:             &recordingUsage{},
						Telemetry:         telemetry,
					})
				})

				reqCtx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, relaySrv.URL+"/v1/messages", nil)
				if err != nil {
					t.Fatalf("NewRequestWithContext: %v", err)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("Do: %v", err)
				}
				defer func() { _ = resp.Body.Close() }()

				if resp.StatusCode != http.StatusOK {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("status = %d (%q), want 200", resp.StatusCode, string(body))
				}
				partial, readErr := io.ReadAll(resp.Body)
				if readErr == nil {
					t.Fatalf("expected transport error on truncated upstream stream, got clean EOF with body=%q", string(partial))
				}
				if errors.Is(readErr, context.DeadlineExceeded) {
					t.Fatalf("client response hung past 1s instead of aborting: %v", readErr)
				}
				if !strings.Contains(string(partial), "message_start") {
					t.Errorf("earlier message_start frame missing from partial body: %q", string(partial))
				}
				calls := telemetry.snapshot()
				if len(calls) != 1 || calls[0].Status != http.StatusOK || (calls[0].Override != "upstream_error" && calls[0].Override != "upstream_aborted") {
					t.Errorf("telemetry = %+v, want status 200 with upstream_error or upstream_aborted", calls)
				}
			})
		}
	})

	t.Run("success: proxy client disconnect stops heartbeats and drains upstream cleanly without leaking", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			rec := newStreamingRecorder()
			up := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       pr,
			}
			clientCtx, cancelClient := context.WithCancel(t.Context())
			defer cancelClient()

			done := make(chan error, 1)
			go func() {
				err := sse.RelayProxy(clientCtx, rec, up, sse.ProxyOptions{
					Provider:        "codex",
					AdvertisedModel: "claude-gpt-5.6-sol",
					BackendModel:    "gpt-5.6-sol",
				})
				_ = pr.Close()
				done <- err
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
			case err := <-done:
				joined = true
				t.Fatalf("RelayProxy returned early: %v", err)
			default:
			}

			first := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
			if _, err := io.WriteString(pw, first); err != nil {
				t.Fatalf("write first: %v", err)
			}
			synctest.Wait()

			// Simulate client disconnecting mid-stream.
			rec.setWriteError(errors.New("broken pipe"))
			cancelClient()
			synctest.Wait()

			// Heartbeat must not fire after client disconnect.
			time.Sleep(40 * time.Second)
			synctest.Wait()
			_, _, bodyAfterDisconnect, _, _ := rec.snapshot()
			if strings.Contains(bodyAfterDisconnect, sse.HeartbeatComment) {
				t.Fatalf("heartbeat fired after client disconnect: %q", bodyAfterDisconnect)
			}

			// Matching the measured Node proxy behavior, the upstream response tail still drains cleanly to completion.
			tail := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"tail\"}}\n\n" + sse.SynthesizedMessageStop
			var providerFinished atomic.Bool
			if _, err := io.WriteString(pw, tail); err != nil {
				t.Fatalf("proxy upstream tail was closed early after client disconnect: %v", err)
			}
			providerFinished.Store(true)
			_ = pw.Close()

			err := <-done
			joined = true
			if err != nil {
				t.Fatalf("RelayProxy returned %v after clean upstream drain, want nil", err)
			}
			if !providerFinished.Load() {
				t.Fatal("expected upstream tail to finish draining")
			}
		})
	})

	t.Run("error: passthrough client write error stops relay and closes side-branch observer cleanly", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			pr, pw := io.Pipe()
			rec := newStreamingRecorder()
			up := &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":     []string{"text/event-stream"},
					"Content-Encoding": []string{"gzip"},
				},
				Body: pr,
			}
			usage := &recordingUsage{}
			clientCtx, cancelClient := context.WithCancel(t.Context())
			defer cancelClient()

			done := make(chan error, 1)
			go func() {
				err := sse.RelayPassthrough(clientCtx, rec, up, sse.PassthroughOptions{
					Usage: usage,
				})
				_ = pr.Close()
				done <- err
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
			case err := <-done:
				joined = true
				t.Fatalf("RelayPassthrough returned early: %v", err)
			default:
			}

			writeErr := errors.New("client reset")
			rec.setWriteError(writeErr)
			chunk := compressBytes(t, "gzip", []byte("data: {\"type\":\"message_start\"}\n\n"))
			// Ignore closed-pipe error on pw.Write in case the relay returns immediately after the write error.
			_, _ = pw.Write(chunk)
			_ = pw.Close()

			err := <-done
			joined = true
			if err == nil || !errors.Is(err, writeErr) {
				t.Fatalf("RelayPassthrough error = %v, want wrapped %v", err, writeErr)
			}
		})
	})
}
