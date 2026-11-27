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

package reply_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"syscall"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// brokenBody returns its bytes once and then fails every read with err.
type brokenBody struct {
	data   []byte
	err    error
	closes int
}

func (b *brokenBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, b.err
}

func (b *brokenBody) Close() error { b.closes++; return nil }

// headLog records every status line written to a client.
type headLog struct {
	*httptest.ResponseRecorder
	heads []int
}

func (w *headLog) WriteHeader(status int) {
	w.heads = append(w.heads, status)
	w.ResponseRecorder.WriteHeader(status)
}

// TestBufferedReadErrorLabel checks the span status override of a buffered upstream body that
// breaks before its end, on every buffered branch: a plain or wrapped unexpected end of file is
// an abort, and any other read error an upstream error. The turn keeps the upstream status either
// way. Branches that can still answer write the early-end error; the others drop the connection.
func TestBufferedReadErrorLabel(t *testing.T) {
	type branch struct {
		status  int
		enabled bool
		// abort reports that the branch drops the client connection instead of answering.
		abort bool
	}
	branches := map[string]branch{
		"enabled genuine overflow": {status: http.StatusRequestEntityTooLarge, enabled: true},
		"other proxy failure":      {status: http.StatusInternalServerError, enabled: true},
		"forbidden":                {status: http.StatusForbidden, enabled: true, abort: true},
		"overflow with sentry off": {status: http.StatusRequestEntityTooLarge, abort: true},
		"JSON success":             {status: http.StatusOK, enabled: true, abort: true},
	}
	reads := map[string]struct {
		err  error
		want string
	}{
		"error: plain unexpected end of file":   {err: io.ErrUnexpectedEOF, want: "upstream_aborted"},
		"error: wrapped unexpected end of file": {err: fmt.Errorf("read upstream body: %w", io.ErrUnexpectedEOF), want: "upstream_aborted"},
		"error: connection reset":               {err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, want: "upstream_error"},
		"error: read error with no cause":       {err: errors.New("upstream body read failed"), want: "upstream_error"},
	}
	for branchName, br := range branches {
		t.Run(branchName, func(t *testing.T) {
			for name, rd := range reads {
				t.Run(name, func(t *testing.T) {
					body := &brokenBody{data: []byte(`{"type":"message","content":[`), err: rd.err}
					req := reply.Request{Provider: "codex", AdvertisedModel: "shown", BackendModel: "engine", SentryEnabled: br.enabled, Sentry: new(sentry.State), Body: []byte(`{}`)}
					w := &headLog{ResponseRecorder: httptest.NewRecorder()}
					result, err := reply.Handle(t.Context(), w, req, func(context.Context) (*upstream.Response, error) {
						return &upstream.Response{StatusCode: br.status, Header: http.Header{"Content-Type": {"application/json"}}, RelayHeader: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
					})
					equal(t, true, result.Contacted)
					equal(t, true, result.Stream == nil)
					equal(t, 1, body.closes)
					equal(t, br.status, result.Status)
					equal(t, rd.want, result.StatusOverride)
					if br.abort {
						if !errors.Is(err, http.ErrAbortHandler) {
							t.Errorf("Handle error = %v, want one that aborts the client connection", err)
						}
						equal(t, []int(nil), w.heads)
						equal(t, "", w.Body.String())
						return
					}
					if err != nil {
						t.Fatalf("Handle: %v", err)
					}
					equal(t, []int{http.StatusBadGateway}, w.heads)
					equal(t, `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`, w.Body.String())
				})
			}
		})
	}
}

// TestSentryOffOverflowHead checks that a proxy's 413 relayed with the sentry off is answered with
// exactly one head and that the turn reports the 413 with no override and no overflow facts, as
// the same 413 normalized with the sentry on reports the 413 with its overflow facts.
func TestSentryOffOverflowHead(t *testing.T) {
	const refusal = `{"error":{"type":"request_too_large","message":"context window"},"model":"engine"}`
	tests := map[string]struct {
		enabled      bool
		wantBody     string
		wantOverflow bool
	}{
		"success: sentry off relays the refusal with only the model rewritten": {
			wantBody: `{"error":{"type":"request_too_large","message":"context window"},"model":"shown"}`,
		},
		"success: sentry on normalizes the refusal": {
			enabled: true, wantOverflow: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			body := &brokenBody{data: []byte(refusal), err: io.EOF}
			req := reply.Request{Provider: "local", AdvertisedModel: "shown", BackendModel: "engine", Window: 110, SentryEnabled: tt.enabled, Sentry: new(sentry.State), Body: []byte(`{"messages":[]}`)}
			w := &headLog{ResponseRecorder: httptest.NewRecorder()}
			result, err := reply.Handle(t.Context(), w, req, func(context.Context) (*upstream.Response, error) {
				return &upstream.Response{StatusCode: http.StatusRequestEntityTooLarge, Header: http.Header{"Content-Type": {"application/json"}}, RelayHeader: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
			})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			equal(t, true, result.Contacted)
			equal(t, true, result.Stream == nil)
			equal(t, 1, body.closes)
			equal(t, []int{http.StatusRequestEntityTooLarge}, w.heads)
			equal(t, http.StatusRequestEntityTooLarge, result.Status)
			equal(t, "", result.StatusOverride)
			equal(t, tt.wantOverflow, result.Route.Overflow != nil)
			if !tt.wantOverflow {
				equal(t, (*outcome.Overflow)(nil), result.Route.Overflow)
				equal(t, tt.wantBody, w.Body.String())
			}
		})
	}
}

// TestUntypedBufferedReply checks the headers a client receives over a real connection for a
// successful buffered reply: an upstream reply without a Content-Type reaches the client without
// one, as the source writes it and as the stream relay does, so net/http must not sniff a type
// from the body; a typed reply keeps its type.
func TestUntypedBufferedReply(t *testing.T) {
	const message = `{"type":"message","role":"assistant","content":[],"stop_reason":"end_turn"}`
	tests := map[string]struct {
		upstreamType string
		// wantType is the client's Content-Type values; nil means the header is absent.
		wantType []string
	}{
		"success: untyped upstream reply reaches the client untyped": {},
		"success: typed upstream reply keeps its type": {
			upstreamType: "application/json", wantType: []string{"application/json"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			response := "HTTP/1.1 200 OK\r\n"
			if tt.upstreamType != "" {
				response += "Content-Type: " + tt.upstreamType + "\r\n"
			}
			response += "Content-Length: " + strconv.Itoa(len(message)) + "\r\nConnection: close\r\n\r\n" + message
			req := reply.Request{Provider: "kimi", AdvertisedModel: "shown", BackendModel: "engine", Body: []byte(`{"messages":[]}`)}
			send := loopbackSend(t, rawUpstream(t, response), req.Body)
			type handled struct {
				result reply.Result
				err    error
			}
			done := make(chan handled, 1)
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				result, err := reply.Handle(r.Context(), w, req, send)
				if result.Stream != nil {
					_ = result.Stream.Body.Close()
				}
				done <- handled{result: result, err: err}
			}))
			t.Cleanup(front.Close)

			conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", front.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\nHost: gateway.test\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"); err != nil {
				t.Fatal(err)
			}
			res, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			h := <-done
			if h.err != nil {
				t.Fatalf("Handle: %v", h.err)
			}
			if h.result.Stream != nil {
				t.Fatal("a buffered JSON reply was handed over as a stream")
			}
			equal(t, http.StatusOK, h.result.Status)
			equal(t, "", h.result.StatusOverride)
			equal(t, http.StatusOK, res.StatusCode)
			equal(t, message, string(got))
			equal(t, strconv.Itoa(len(message)), res.Header.Get("Content-Length"))
			equal(t, tt.wantType, res.Header["Content-Type"])
		})
	}
}
