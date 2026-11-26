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

package grok_test

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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// upstreamTimeoutBody is the fixed answer the source sends when the Grok upstream times out.
const upstreamTimeoutBody = `{"type":"error","error":{"type":"api_error","message":"model-gateway: Grok upstream failed: Grok upstream timeout"}}`

// headerProbe is a ResponseWriter that records whether and how often a status line was written,
// which httptest.ResponseRecorder cannot tell apart from an implicit 200.
type headerProbe struct {
	mu           sync.Mutex
	header       http.Header
	status       int
	headerWrites int
	body         bytes.Buffer
}

func newHeaderProbe() *headerProbe { return &headerProbe{header: make(http.Header)} }

func (p *headerProbe) Header() http.Header { return p.header }

func (p *headerProbe) WriteHeader(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.headerWrites++
	if p.status == 0 {
		p.status = status
	}
}

func (p *headerProbe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == 0 {
		p.headerWrites++
		p.status = http.StatusOK
	}
	return p.body.Write(b)
}

func (p *headerProbe) snapshot() (status, headerWrites int, body string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status, p.headerWrites, p.body.String()
}

// serveUpstreamHead answers one request on conn with head (a status line, headers and the start of
// a body shorter than its Content-Length). With stall it then sends nothing more and waits until
// the client side closes the connection; without, it closes the connection at once.
func serveUpstreamHead(conn net.Conn, head string, stall bool, done chan<- struct{}) {
	defer close(done)
	defer func() { _ = conn.Close() }()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		return
	}
	if _, err := io.WriteString(conn, head); err != nil {
		return
	}
	if stall {
		_, _ = io.Copy(io.Discard, conn)
	}
}

// TestBranchUpstreamBodyBreakBeforeHeaders verifies the answer to an upstream body that ends
// before any client header was written. When the body stalls until the upstream connection's
// one-hour inactivity timeout, the client gets the fixed 502 the source sends for an upstream
// timeout; any other break keeps the aborted response. The upstream is an in-memory connection
// and the hour passes in virtual time.
func TestBranchUpstreamBodyBreakBeforeHeaders(t *testing.T) {
	const (
		partialJSON   = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"id\":\"resp_partial\""
		partialError  = "HTTP/1.1 500 Internal Server Error\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"error\":{\"message\""
		partialUpdate = "HTTP/1.1 426 Upgrade Required\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"error\""
	)
	route := outcome.Route{SelectedModel: "claude-grok-4.7[1m]", EffectiveModel: "grok-4.7", Backend: "grok", Via: "direct"}

	tests := map[string]struct {
		head        string
		stall       bool
		wantTimeout bool
	}{
		"error: buffered JSON body stalled until the inactivity timeout answers the fixed 502": {
			head:        partialJSON,
			stall:       true,
			wantTimeout: true,
		},
		"error: upstream error body stalled until the inactivity timeout answers the fixed 502": {
			head:        partialError,
			stall:       true,
			wantTimeout: true,
		},
		"error: outdated version body stalled until the inactivity timeout answers the fixed 502": {
			head:        partialUpdate,
			stall:       true,
			wantTimeout: true,
		},
		"error: buffered JSON body cut short keeps the aborted response": {
			head: partialJSON,
		},
		"error: upstream error body cut short keeps the aborted response": {
			head: partialError,
		},
		"error: outdated version body cut short keeps the aborted response": {
			head: partialUpdate,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				now := time.UnixMilli(1_700_000_000_000)
				clock := func() time.Time { return now }

				served := make(chan struct{})
				var dials atomic.Int32
				var blockedDial atomic.Bool
				client := upstream.NewClient(upstream.Options{
					DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
						if addr != "grok.fixture.test:80" || dials.Add(1) != 1 {
							blockedDial.Store(true)
							return nil, errors.New("blocked an unapproved inference dial")
						}
						clientEnd, serverEnd := net.Pipe()
						go serveUpstreamHead(serverEnd, tt.head, tt.stall, served)
						return clientEnd, nil
					},
				})
				defer func() { _ = client.Close() }()

				store := mustNewAuthStore(t, grok.AuthOptions{File: validAuthFixture(t, now), Transport: denyNetworkTransport(t), Now: clock})
				branch := mustNewBranch(t, grok.BranchOptions{Endpoint: "http://grok.fixture.test/v1/responses", Auth: store, Client: client, Now: clock})

				body := `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`
				r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
				w := newHeaderProbe()
				start := time.Now()
				res, err := branch.Handle(w, r, grok.Request{
					RawBody:         []byte(body),
					Path:            "/v1/messages",
					AdvertisedModel: "claude-grok-4.7[1m]",
					BackendModel:    "grok-4.7",
				})
				elapsed := time.Since(start)
				_ = client.Close()
				select {
				case <-served:
				case <-time.After(time.Minute):
					t.Errorf("the fixture upstream connection was not closed after Handle returned (dials=%d)", dials.Load())
				}

				assertBranchOutcome(t, res, err)
				if blockedDial.Load() {
					t.Error("Handle dialed something other than the one fixture upstream connection")
				}
				status, headerWrites, got := w.snapshot()
				assertBranchText(t, "client body", got)

				if !tt.wantTimeout {
					if !errors.Is(err, http.ErrAbortHandler) {
						t.Errorf("Handle() error = %v, want an error wrapping http.ErrAbortHandler", err)
					}
					if headerWrites != 0 || got != "" {
						t.Errorf("client received status %d (%d header writes) and %d body bytes, want nothing before the abort", status, headerWrites, len(got))
					}
					if !res.Contacted {
						t.Error("outcome.Result.Contacted = false, want true")
					}
					if elapsed >= time.Hour {
						t.Errorf("a cut-short body took %v of virtual time, want less than the inactivity timeout", elapsed)
					}
					return
				}

				if elapsed < time.Hour {
					t.Errorf("stalled body ended after %v of virtual time, want the one-hour inactivity timeout", elapsed)
				}
				if err != nil {
					t.Errorf("Handle() error = %v, want nil after answering the timeout locally", err)
				}
				want := outcome.Result{Route: route, Status: http.StatusBadGateway, StatusOverride: "upstream_error", Contacted: true}
				if diff := gocmp.Diff(want, res); diff != "" {
					t.Errorf("outcome.Result (-want +got):\n%s", diff)
				}
				if status != http.StatusBadGateway || headerWrites != 1 {
					t.Errorf("client status = %d after %d header writes, want one 502", status, headerWrites)
				}
				if diff := gocmp.Diff(upstreamTimeoutBody, got); diff != "" {
					t.Errorf("client body (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("application/json", w.Header().Get("Content-Type")); diff != "" {
					t.Errorf("client Content-Type (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff(strconv.Itoa(len(upstreamTimeoutBody)), w.Header().Get("Content-Length")); diff != "" {
					t.Errorf("client Content-Length (-want +got):\n%s", diff)
				}
			})
		})
	}
}

// encodeOverflowReply builds a buffered Grok reply carrying usage whose translation succeeds and
// whose encoding fails: the tool arguments nest just under the decoder's depth limit, and the
// translated message wraps them deeper still.
func encodeOverflowReply(t *testing.T, now time.Time, usage string) string {
	t.Helper()
	const depth = 9999
	nested := strings.Repeat("[", depth) + strings.Repeat("]", depth)
	body := fmt.Sprintf(`{"id":"resp_deep","status":"completed","output":[{"type":"function_call","call_id":"call_deep","name":"deep_tool","arguments":%q}],"usage":%s}`, nested, usage)

	wire, err := jsonx.DecodeWire([]byte(body))
	if err != nil {
		t.Fatalf("encode-overflow reply does not decode: %v", err)
	}
	translated, err := grok.TranslateResponseChecked(wire, "claude-grok-4.5[1m]", func() time.Time { return now })
	if err != nil {
		t.Fatalf("encode-overflow reply fails translation, want it to fail only at encoding: %v", err)
	}
	if _, err := jsonx.EncodeWire(translated); err == nil {
		t.Fatal("encode-overflow reply encodes, want an encoding failure")
	}
	return body
}

// TestBranchJSONSentryAfterEncode verifies that the context sentry observes a buffered JSON reply
// only after the translated reply was encoded: a reply that cannot be encoded leaves an unfired
// session without usage and a fired latch fired, while an encoded reply records its usage and an
// ordinary completed turn rearms the latch.
func TestBranchJSONSentryAfterEncode(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	const (
		usageJSON   = `{"input_tokens":150,"output_tokens":1}`
		requestBody = `{"model":"claude-grok-4.5[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`
	)
	encodedReply := `{"id":"resp_ok","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":` + usageJSON + `}`
	failingReply := encodeOverflowReply(t, now, usageJSON)

	tests := map[string]struct {
		fired        bool
		reply        string
		wantStatus   int
		wantCrossing bool
	}{
		"error: unencodable reply records no usage for an unfired session": {
			reply:      failingReply,
			wantStatus: http.StatusBadGateway,
		},
		"error: unencodable reply leaves a fired latch fired": {
			fired:      true,
			reply:      failingReply,
			wantStatus: http.StatusBadGateway,
		},
		"success: encoded reply records usage for an unfired session": {
			reply:        encodedReply,
			wantStatus:   http.StatusOK,
			wantCrossing: true,
		},
		"success: encoded completed reply rearms a fired latch": {
			fired:        true,
			reply:        encodedReply,
			wantStatus:   http.StatusOK,
			wantCrossing: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.reply)
			}))
			t.Cleanup(upSrv.Close)
			t.Cleanup(upSrv.Client().CloseIdleConnections)

			state := &sentry.State{}
			target := grokSentryTarget(t, "grok-4.5", 100)
			key := sentry.SessionKey(true, "grok-encode-session", nil)
			if tt.fired {
				state.Observe(key, target, []byte(`{"type":"message_delta","usage":{"input_tokens":101}}`), false)
				cross, ok := state.Check(key, target, []byte(requestBody))
				if diff := gocmp.Diff(sentry.Crossing{Usage: 101, Trigger: 100}, cross); !ok || diff != "" {
					t.Fatalf("pre-arming the latch did not fire it (ok=%v, -want +got):\n%s", ok, diff)
				}
			}

			client := newLoopbackClient(t, upSrv.Listener.Addr().String())
			store := mustNewAuthStore(t, grok.AuthOptions{File: validAuthFixture(t, now), Transport: denyNetworkTransport(t), Now: func() time.Time { return now }})
			branch := mustNewBranch(t, grok.BranchOptions{
				Endpoint:      upSrv.URL + "/v1/responses",
				Auth:          store,
				Client:        client,
				Sentry:        state,
				SentryEnabled: true,
				Now:           func() time.Time { return now },
			})

			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
			rec := httptest.NewRecorder()
			res, err := branch.Handle(rec, r, grok.Request{
				RawBody:         []byte(requestBody),
				Path:            "/v1/messages",
				AdvertisedModel: "claude-grok-4.5[1m]",
				BackendModel:    "grok-4.5",
				SessionKey:      key,
				SentryTarget:    target,
			})
			assertBranchOutcome(t, res, err)
			if err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			if rec.Code != tt.wantStatus {
				t.Errorf("client status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusBadGateway {
				decoded := decodeFullJSONObject(t, rec.Body.String())
				want := map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": sanitizedTranslationErrMessage}}
				if diff := gocmp.Diff(want, decoded); diff != "" {
					t.Errorf("client body (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff("upstream_error", res.StatusOverride); diff != "" {
					t.Errorf("outcome.Result.StatusOverride (-want +got):\n%s", diff)
				}
			}

			cross, crossed := state.Check(key, target, []byte(requestBody))
			if !tt.wantCrossing {
				if crossed {
					t.Errorf("the sentry observed a reply the client never received: the next request crosses with %+v", cross)
				}
				return
			}
			if diff := gocmp.Diff(sentry.Crossing{Usage: 150, Trigger: 100}, cross); !crossed || diff != "" {
				t.Errorf("the sentry did not record the delivered reply (crossed=%v, -want +got):\n%s", crossed, diff)
			}
		})
	}
}
