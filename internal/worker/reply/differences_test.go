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
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// loopbackSend returns a Send that posts body to the proxy origin baseURL with a client the test
// closes at cleanup.
func loopbackSend(t *testing.T, baseURL string, body []byte) upstream.Send {
	t.Helper()
	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })
	req := upstream.Request{Target: upstream.TargetProxy, BaseURL: baseURL, Method: http.MethodPost, RequestURI: "/v1/messages", Body: body}
	return func(ctx context.Context) (*upstream.Response, error) { return client.Do(ctx, req) }
}

// TestSuccessfulJSONNullWithPlanFilter checks that a successful proxy reply whose body is the
// JSON literal null is relayed as null with plan-tool filtering enabled, where the source throws
// reading the filter's content member, exactly as with filtering disabled.
func TestSuccessfulJSONNullWithPlanFilter(t *testing.T) {
	tests := map[string]struct {
		provider string
		filter   bool
	}{
		"success: Codex null with plan-tool filtering":    {provider: "codex", filter: true},
		"success: Codex null without plan-tool filtering": {provider: "codex", filter: false},
		"success: Kimi null with plan-tool filtering":     {provider: "kimi", filter: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "null")
			}))
			t.Cleanup(server.Close)
			req := reply.Request{Provider: tt.provider, AdvertisedModel: "shown", BackendModel: "engine", FilterPlanTools: tt.filter, Body: []byte(`{"messages":[]}`)}
			w := httptest.NewRecorder()
			result, err := reply.Handle(t.Context(), w, req, loopbackSend(t, server.URL, req.Body))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if result.Stream != nil {
				_ = result.Stream.Body.Close()
				t.Fatal("a buffered JSON reply was handed over as a stream")
			}
			equal(t, true, result.Contacted)
			equal(t, http.StatusOK, result.Status)
			equal(t, http.StatusOK, w.Code)
			equal(t, "null", w.Body.String())
			equal(t, "4", w.Header().Get("Content-Length"))
			equal(t, "application/json", w.Header().Get("Content-Type"))
		})
	}
}

// rawUpstream serves one HTTP/1.1 response per connection, written byte for byte from response,
// after reading the request; it lets a test fix the order and spelling of header lines, which
// net/http's server sorts and canonicalizes.
func rawUpstream(t *testing.T, response string) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() { _ = conn.Close() }()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					t.Errorf("upstream read request: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				_, _ = io.WriteString(conn, response)
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

// closeCountingBody counts its closes.
type closeCountingBody struct {
	io.Reader
	closes atomic.Int32
}

func (b *closeCountingBody) Close() error { b.closes.Add(1); return nil }

// TestRejectionEvidenceNameOrder checks that the saved evidence of an attributed Codex rejection
// lists the matching header names lowercased, sorted and without duplicates, whatever their
// arrival order and spelling, where the source lists them in arrival order; no value is saved.
func TestRejectionEvidenceNameOrder(t *testing.T) {
	const body = `{"error":{"message":"provider rejection"}}`
	tests := map[string]struct {
		status int
		// wire, when set, is the raw response head the loopback upstream sends before body.
		wire string
		// header, when wire is empty, is the parsed header map of a response handed to Handle.
		header       http.Header
		wantEvidence string
	}{
		"error: wire names in reverse lexical order with case duplicates": {
			status: http.StatusTooManyRequests,
			wire: "HTTP/1.1 429 Too Many Requests\r\n" +
				"X-OpenAI-Version: value-version\r\n" +
				"x-openai-request-id: value-lower\r\n" +
				"X-OPENAI-REQUEST-ID: value-upper\r\n" +
				"X-OpenAI-Organization: value-organization\r\n" +
				"OpenAI-Processing-Ms: 4711\r\n" +
				"Content-Type: application/json\r\n" +
				"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
				"Connection: close\r\n\r\n",
			wantEvidence: "headers:content-type,openai-processing-ms,x-openai-organization,x-openai-request-id,x-openai-version",
		},
		"error: wire names in reverse lexical order on a forbidden reply": {
			status: http.StatusForbidden,
			wire: "HTTP/1.1 403 Forbidden\r\n" +
				"X-Openai-Zeta: value-zeta\r\n" +
				"x-openai-alpha: value-alpha\r\n" +
				"X-OPENAI-ALPHA: value-alpha-upper\r\n" +
				"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
				"Connection: close\r\n\r\n",
			wantEvidence: "headers:x-openai-alpha,x-openai-zeta",
		},
		"error: header map keys that differ only in case": {
			status: http.StatusTooManyRequests,
			header: http.Header{
				"x-openai-beta":        {"value-1"},
				"X-OPENAI-BETA":        {"value-2"},
				"X-Openai-Alpha":       {"value-3"},
				"OPENAI-PROCESSING-MS": {"value-4"},
				"openai-processing-ms": {"value-5"},
				"Content-Type":         {"application/json"},
			},
			wantEvidence: "headers:content-type,openai-processing-ms,x-openai-alpha,x-openai-beta",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store := &upstreamstate.Store{Dir: t.TempDir(), Now: func() time.Time { return time.Unix(1000, 0) }}
			req := reply.Request{Provider: "codex", AdvertisedModel: "shown", Body: []byte(`{}`), Upstream: store}
			var send upstream.Send
			var direct *closeCountingBody
			if tt.wire != "" {
				send = loopbackSend(t, rawUpstream(t, tt.wire+body), req.Body)
			} else {
				direct = &closeCountingBody{Reader: strings.NewReader(body)}
				send = func(context.Context) (*upstream.Response, error) {
					return &upstream.Response{StatusCode: tt.status, Header: tt.header, RelayHeader: tt.header.Clone(), Body: direct}, nil
				}
			}
			w := httptest.NewRecorder()
			result, err := reply.Handle(t.Context(), w, req, send)
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if result.Stream != nil {
				_ = result.Stream.Body.Close()
				t.Fatal("a rejection was handed over as a stream")
			}
			if direct != nil {
				equal(t, int32(1), direct.closes.Load())
			}
			equal(t, tt.status, w.Code)
			equal(t, body, w.Body.String())
			block := store.ReadBlocked()
			if block == nil {
				t.Fatal("ReadBlocked = nil, want the attributed rejection")
			}
			equal(t, tt.status, block.StatusCode)
			equal(t, tt.wantEvidence, block.Evidence)
			if strings.Contains(block.Evidence, "value-") || strings.Contains(block.Evidence, "4711") {
				t.Errorf("evidence %q contains a header value", block.Evidence)
			}
		})
	}
}

// TestTransportFailureWireLength checks on the wire that the small 502 JSON reply to an upstream
// transport failure carries the content type and bytes of the source's reply, with a
// Content-Length the HTTP server supplies instead of the chunked framing the source can get: the
// handler leaves the length unset and the server derives it from the one complete write. The
// refused connection is injected, so no released port is dialled again.
func TestTransportFailureWireLength(t *testing.T) {
	tests := map[string]struct {
		connection string
	}{
		"error: keep-alive HTTP/1.1 client":       {connection: "keep-alive"},
		"error: HTTP/1.1 client closing the link": {connection: "close"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// Nothing dials this origin; it only names the upstream in the reply.
			const origin = "http://127.0.0.1:9"
			refusal := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
			type handled struct {
				refused, err error
				result       reply.Result
				// explicitLength reports whether the handler had set Content-Length when Handle returned.
				explicitLength bool
			}
			req := reply.Request{Origin: origin, Body: []byte(`{}`)}
			outcome := make(chan handled, 1)
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var refused error
				result, err := reply.Handle(r.Context(), w, req, func(context.Context) (*upstream.Response, error) {
					refused = refusal
					return nil, refusal
				})
				_, explicit := w.Header()["Content-Length"]
				outcome <- handled{refused: refused, err: err, result: result, explicitLength: explicit}
			}))
			t.Cleanup(front.Close)

			conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", front.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: gateway.test\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: %s\r\n\r\n{}", tt.connection); err != nil {
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
			h := <-outcome
			if !errors.Is(h.refused, syscall.ECONNREFUSED) {
				t.Fatalf("test precondition: the injected send did not run, got %v (Handle: %v)", h.refused, h.err)
			}
			if h.err != nil {
				t.Fatalf("Handle: %v", h.err)
			}
			equal(t, false, h.explicitLength)
			equal(t, http.StatusBadGateway, h.result.Status)
			equal(t, "upstream_error", h.result.StatusOverride)
			want := `{"type":"error","error":{"type":"api_error","message":` + strconv.Quote("model-gateway shim: upstream "+origin+" failed: "+h.refused.Error()) + `}}`
			equal(t, http.StatusBadGateway, res.StatusCode)
			equal(t, "application/json", res.Header.Get("Content-Type"))
			equal(t, want, string(got))
			equal(t, int64(len(want)), res.ContentLength)
			equal(t, strconv.Itoa(len(want)), res.Header.Get("Content-Length"))
			equal(t, []string(nil), res.TransferEncoding)
		})
	}
}
