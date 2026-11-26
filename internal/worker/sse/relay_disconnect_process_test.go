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

package sse_test

import (
	"bufio"
	"bytes"
	"context"
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
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func TestRelayLoopbackClientDisconnect(t *testing.T) {
	const literalMessageStop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

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
