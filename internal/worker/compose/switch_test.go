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

package compose_test

import (
	json "encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// switchedReply is a protocol switch as net/http recognises one: status 101 with the upgrade headers.
const switchedReply = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

// heldUpstream is a provider fixture that takes over each connection it answers. In switch mode it
// writes a protocol switch and keeps the connection open and silent; otherwise it closes the
// connection without answering, as an upstream that could not be reached.
type heldUpstream struct {
	switched bool

	mu    sync.Mutex
	conns []net.Conn
}

func (u *heldUpstream) handle(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	if !u.switched {
		_ = conn.Close()
		return
	}
	// Recorded before the switch is written, so a worker that answers at once still finds it held.
	u.mu.Lock()
	u.conns = append(u.conns, conn)
	u.mu.Unlock()
	_, _ = io.WriteString(conn, switchedReply)
}

func (u *heldUpstream) held() []net.Conn {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]net.Conn(nil), u.conns...)
}

// closeAll closes every held connection. It is deferred in the test body, so it runs before the
// harness cleanups that wait for the request handlers a held connection may still block.
func (u *heldUpstream) closeAll() {
	for _, c := range u.held() {
		_ = c.Close()
	}
}

// gatewayAnswer is the part of an error answer that does not depend on the failure's own text.
type gatewayAnswer struct {
	Status      int
	ContentType string
	ErrorType   string
	Message     string
}

// answerOf reduces an answer to its status, content type, error type and message. The proxy and
// native paths end their message with the transport's error after "failed: "; only the text up to
// there is kept, since what the error says is the failing layer's choice.
func answerOf(r httpResult) gatewayAnswer {
	a := gatewayAnswer{Status: r.status, ContentType: r.header.Get("Content-Type")}
	var doc map[string]any
	if err := json.Unmarshal(r.body, &doc); err != nil {
		// A body that is not a JSON object is reported by the status and content type alone.
		a.Message = "(no JSON body: " + strconv.Quote(string(r.body)) + ")"
		return a
	}
	e, _ := doc["error"].(map[string]any)
	a.ErrorType, _ = e["type"].(string)
	a.Message, _ = e["message"].(string)
	if head, _, ok := strings.Cut(a.Message, " failed: "); ok && strings.HasPrefix(a.Message, "model-gateway shim: upstream ") {
		a.Message = head + " failed: "
	}
	return a
}

// TestSwitchedUpstreamAnswersAsUnreachable decides what the client of the composed handler gets when
// its provider answers with a protocol switch and then stays silent: the same answer as when the
// provider could not be reached, on the proxy path, the native passthrough path and the Grok path,
// and the worker closes the switched connection.
func TestSwitchedUpstreamAnswersAsUnreachable(t *testing.T) {
	type path struct {
		model  string
		stream bool
		set    func(cfg *harnessConfig, handler http.HandlerFunc)
		want   gatewayAnswer
	}
	proxy := path{
		model: "claude-gpt-6.1-sol[1m]",
		set:   func(cfg *harnessConfig, handler http.HandlerFunc) { cfg.proxyHandler = handler },
		want:  gatewayAnswer{Status: http.StatusBadGateway, ContentType: "application/json", ErrorType: "api_error"},
	}
	native := path{
		model:  "claude-sonnet-4-6",
		stream: true,
		set:    func(cfg *harnessConfig, handler http.HandlerFunc) { cfg.anthropicHandler = handler },
		want:   gatewayAnswer{Status: http.StatusBadGateway, ContentType: "application/json", ErrorType: "api_error"},
	}
	grokPath := path{
		model: "claude-grok-4.7[1m]",
		set:   func(cfg *harnessConfig, handler http.HandlerFunc) { cfg.grokHandler = handler },
		want: gatewayAnswer{
			Status:      http.StatusBadGateway,
			ContentType: "application/json",
			ErrorType:   "api_error",
			Message:     "model-gateway: Grok upstream failed: no response was received",
		},
	}
	tests := map[string]struct {
		path     path
		switched bool
	}{
		"error: a proxy upstream that closes the connection without answering is unreachable":     {path: proxy},
		"error: a proxy upstream that switches protocols is answered as unreachable":              {path: proxy, switched: true},
		"error: a native upstream that closes the connection without answering is unreachable":    {path: native},
		"error: a native passthrough upstream that switches protocols is answered as unreachable": {path: native, switched: true},
		"error: a Grok upstream that closes the connection without answering is unreachable":      {path: grokPath},
		"error: a Grok upstream that switches protocols is answered as unreachable":               {path: grokPath, switched: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &heldUpstream{switched: tt.switched}
			defer fake.closeAll()
			cfg := harnessConfig{disableTrace: true, disableUsage: true}
			tt.path.set(&cfg, fake.handle)
			h := newHarness(t, cfg)

			r := h.request(http.MethodPost, "/v1/messages", messageRequestJSON(tt.path.model, tt.path.stream), nil)
			if r.err != nil {
				t.Errorf("the client got no answer: %v", r.err)
			}
			assertNoSecrets(t, "response body", r.body)
			if r.err == nil {
				// The proxy and native answers name the provider's origin before "failed: ".
				want := tt.path.want
				if want.Message == "" {
					origin := h.proxySrv.URL
					if tt.path.model == native.model {
						origin = h.anthropicSrv.URL
					}
					want.Message = "model-gateway shim: upstream " + origin + " failed: "
				}
				mustDiff(t, "answer (status, content type, error type, message before the failure's own text)", want, answerOf(r))
			}
			if !tt.switched {
				return
			}
			held := fake.held()
			if len(held) != 1 {
				t.Fatalf("switched connections = %d, want 1 (the request carries a body and is never replayed)", len(held))
			}
			// The deadline only aborts a wait for a close that should already have happened.
			if err := held[0].SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			n, err := held[0].Read(make([]byte, 1))
			switch {
			case errors.Is(err, io.EOF):
			case errors.Is(err, os.ErrDeadlineExceeded):
				t.Error("the worker left the switched upstream connection open")
			default:
				t.Errorf("read of the switched connection after the answer: %d bytes, %v; want the worker's close", n, err)
			}
		})
	}
}
