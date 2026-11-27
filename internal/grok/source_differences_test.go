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
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// The tests in this file pin the answers where the port deliberately differs from the source:
// requests the source cannot translate, the text of a failed send, upstream error messages whose
// conversion would throw, stored expiries outside the explicit date grammar or whose numeric
// conversion would throw, an issuer lifetime whose conversion would throw, a refreshed
// credential that cannot be stored, the body of an outdated-version reply, which is drained rather
// than kept, and an explicit refresh skew of zero or below, which is replaced by the default.

const (
	// pipeEndpoint is the upstream URL whose connections handleOverPipe answers in memory.
	pipeEndpoint = "http://grok.fixture.test/v1/responses"
	// pipeAddr is the address the client dials for pipeEndpoint.
	pipeAddr = "grok.fixture.test:80"
)

// pipeRoute is the route Handle publishes for a request without an effort.
var pipeRoute = outcome.Route{SelectedModel: "claude-grok-4.7[1m]", EffectiveModel: "grok-4.7", Backend: "grok", Via: "direct"}

// errorEnvelope is the exact error body the branch writes; the messages used here are ASCII
// without quotes or backslashes, so Go quoting and JSON quoting agree.
func errorEnvelope(typ, message string) string {
	return fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, typ, message)
}

// pipedOutcome is everything one Branch.Handle call produced.
type pipedOutcome struct {
	status  int
	header  http.Header
	body    string
	res     outcome.Result
	err     error
	dials   int32
	elapsed time.Duration
}

// handleOverPipe runs Branch.Handle for body inside the caller's synctest bubble. Each upstream
// dial gets an in-memory connection whose far end serve answers; with a nil serve every dial is
// refused as a closed port refuses it. A dial to any other address fails the test.
func handleOverPipe(t *testing.T, store *grok.AuthStore, body string, serve func(net.Conn)) pipedOutcome {
	t.Helper()
	now := time.UnixMilli(1_700_000_000_000)
	clock := func() time.Time { return now }
	var (
		dials   atomic.Int32
		strayed atomic.Bool
		served  sync.WaitGroup
	)
	client := upstream.NewClient(upstream.Options{
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			dials.Add(1)
			if addr != pipeAddr {
				strayed.Store(true)
				return nil, errors.New("blocked an unapproved inference dial")
			}
			if serve == nil {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
			}
			clientEnd, serverEnd := net.Pipe()
			served.Go(func() {
				defer func() { _ = serverEnd.Close() }()
				serve(serverEnd)
			})
			return clientEnd, nil
		},
	})
	branch := mustNewBranch(t, grok.BranchOptions{Endpoint: pipeEndpoint, Auth: store, Client: client, Now: clock})

	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	var out pipedOutcome
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		out.res, out.err = branch.Handle(w, r, grok.Request{
			RawBody:         []byte(body),
			Path:            "/v1/messages",
			AdvertisedModel: "claude-grok-4.7[1m]",
			BackendModel:    "grok-4.7",
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Hour):
		_ = client.Close()
		<-done
		t.Error("Handle did not return within two hours of virtual time")
	}
	out.elapsed = time.Since(start)
	_ = client.Close()
	served.Wait()

	if strayed.Load() {
		t.Error("Handle dialed an address other than the fixture upstream")
	}
	out.dials = dials.Load()
	out.status, out.header, out.body = w.Code, w.Header().Clone(), w.Body.String()
	return out
}

// readPipedRequest reads one request and its body from conn and reports whether both arrived.
func readPipedRequest(conn net.Conn) bool {
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return false
	}
	_, err = io.Copy(io.Discard, req.Body)
	return err == nil
}

// answerPiped answers one request with the raw response bytes.
func answerPiped(raw string) func(net.Conn) {
	return func(conn net.Conn) {
		if readPipedRequest(conn) {
			_, _ = io.WriteString(conn, raw)
		}
	}
}

// pipedJSONResponse is a complete HTTP/1.1 JSON response that closes its connection.
func pipedJSONResponse(status int, body string) string {
	return fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, http.StatusText(status), len(body), body)
}

// checkPipedAnswer compares a complete JSON answer with its framing, its body and its outcome.
func checkPipedAnswer(t *testing.T, got pipedOutcome, wantStatus int, wantBody string, wantResult outcome.Result) {
	t.Helper()
	assertBranchOutcome(t, got.res, got.err)
	if got.err != nil {
		t.Fatalf("Handle() error = %v, want nil", got.err)
	}
	if diff := gocmp.Diff(wantResult, got.res); diff != "" {
		t.Errorf("Handle() outcome.Result (-want +got):\n%s", diff)
	}
	if got.status != wantStatus {
		t.Errorf("status = %d, want %d", got.status, wantStatus)
	}
	if diff := gocmp.Diff("application/json", got.header.Get("Content-Type")); diff != "" {
		t.Errorf("Content-Type (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(strconv.Itoa(len(got.body)), got.header.Get("Content-Length")); diff != "" {
		t.Errorf("Content-Length (-want +got):\n%s", diff)
	}
	assertBranchText(t, "client body", got.body)
	if diff := gocmp.Diff(wantBody, got.body); diff != "" {
		t.Errorf("client body (-want +got):\n%s", diff)
	}
}

// TestBranchRefusesUntranslatableRequest verifies that a request the source could not translate
// (its handler rejected after reading the credential, without any reply) is refused locally once
// the credential has been read: a fixed 400 invalid_request_error that repeats none of the
// request, the invalid_request outcome, and no upstream dial. A request whose credential is
// missing still gets the authentication answer first.
func TestBranchRefusesUntranslatableRequest(t *testing.T) {
	const prompt = "SENTINEL_PROMPT_TEXT"
	refusal := errorEnvelope("invalid_request_error", "model-gateway: Grok request could not be translated.")
	effortRoute := pipeRoute
	effortRoute.Effort = "high"

	tests := map[string]struct {
		body        string
		missingAuth bool
		wantStatus  int
		wantBody    string
		wantResult  outcome.Result
	}{
		"error: a null message is refused with 400 before any upstream dial": {
			body:       `{"model":"claude-grok-4.7[1m]","max_tokens":10,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"` + prompt + `"},null]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   refusal,
			wantResult: outcome.Result{Route: effortRoute, Status: http.StatusBadRequest, StatusOverride: "invalid_request"},
		},
		"error: user text whose String() throws is refused with 400 before any upstream dial": {
			body:       `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"` + prompt + `"},{"role":"user","content":[{"type":"text","text":{"toString":"` + prompt + `"}}]}]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   refusal,
			wantResult: outcome.Result{Route: pipeRoute, Status: http.StatusBadRequest, StatusOverride: "invalid_request"},
		},
		"error: assistant text block whose String() throws is refused with 400 before any upstream dial": {
			body:       `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"` + prompt + `"},{"role":"assistant","content":{"type":"text","text":[{"toString":1}]}}]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   refusal,
			wantResult: outcome.Result{Route: pipeRoute, Status: http.StatusBadRequest, StatusOverride: "invalid_request"},
		},
		"error: system text whose String() throws is refused with 400 before any upstream dial": {
			body:       `{"model":"claude-grok-4.7[1m]","max_tokens":10,"system":[{"type":"text","text":[[{"toString":null}]]}],"messages":[{"role":"user","content":"` + prompt + `"}]}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   refusal,
			wantResult: outcome.Result{Route: pipeRoute, Status: http.StatusBadRequest, StatusOverride: "invalid_request"},
		},
		"error: a truncated JSON body is refused with 400 before any upstream dial": {
			body:       `{"model":"claude-grok-4.7[1m]","output_config":{"effort":"high"},"messages":[{"role":"user","content":"` + prompt + `"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   refusal,
			wantResult: outcome.Result{Route: pipeRoute, Status: http.StatusBadRequest, StatusOverride: "invalid_request"},
		},
		"error: a body nested beyond the decoder's limit is refused with 400 before any upstream dial": {
			body:       strings.Repeat("[", 10001) + `"` + prompt + `"` + strings.Repeat("]", 10001),
			wantStatus: http.StatusBadRequest,
			wantBody:   refusal,
			wantResult: outcome.Result{Route: pipeRoute, Status: http.StatusBadRequest, StatusOverride: "invalid_request"},
		},
		"error: a missing credential is answered with 401 before the request is translated": {
			body:        `{"model":"claude-grok-4.7[1m]","max_tokens":10,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"` + prompt + `"},null]}`,
			missingAuth: true,
			wantStatus:  http.StatusUnauthorized,
			wantBody:    errorEnvelope("authentication_error", "model-gateway: Grok CLI auth is missing. Run `grok` and log in again."),
			wantResult:  outcome.Result{Route: effortRoute, Status: http.StatusUnauthorized, StatusOverride: "authentication_error"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				now := time.UnixMilli(1_700_000_000_000)
				authFile := validAuthFixture(t, now)
				if tt.missingAuth {
					authFile = filepath.Join(t.TempDir(), "absent", "auth.json")
				}
				store := mustNewAuthStore(t, grok.AuthOptions{File: authFile, Transport: denyNetworkTransport(t), Now: func() time.Time { return now }})

				got := handleOverPipe(t, store, tt.body, nil)

				if got.dials != 0 {
					t.Errorf("upstream dials = %d, want 0 (the request must not be sent)", got.dials)
				}
				if strings.Contains(got.body, prompt) {
					t.Error("the refusal repeats request content")
				}
				checkPipedAnswer(t, got, tt.wantStatus, tt.wantBody, tt.wantResult)
			})
		})
	}
}

// TestBranchSendFailureText verifies the complete 502 message of a send that got no response:
// the source appended the transport's error text, the port appends the fixed "no response was
// received", and keeps "Grok upstream timeout" for the socket inactivity timeout. Both record a
// contacted upstream_error. The upstream is an in-memory connection and the hour passes in virtual
// time.
func TestBranchSendFailureText(t *testing.T) {
	const prefix = "model-gateway: Grok upstream failed: "
	failed := outcome.Result{Route: pipeRoute, Status: http.StatusBadGateway, StatusOverride: "upstream_error", Contacted: true}

	tests := map[string]struct {
		serve       func(net.Conn)
		wantMessage string
		wantTimeout bool
	}{
		"error: a refused connection answers no response was received": {
			serve:       nil,
			wantMessage: prefix + "no response was received",
		},
		"error: a connection closed after the request answers no response was received": {
			serve:       func(conn net.Conn) { readPipedRequest(conn) },
			wantMessage: prefix + "no response was received",
		},
		"error: a malformed status line answers no response was received": {
			serve:       answerPiped("HTTP/1.1 2x0 SENTINEL_TRANSPORT_DETAIL\r\n\r\n"),
			wantMessage: prefix + "no response was received",
		},
		"error: an upstream silent for the inactivity timeout answers Grok upstream timeout": {
			serve: func(conn net.Conn) {
				if readPipedRequest(conn) {
					_, _ = io.Copy(io.Discard, conn)
				}
			},
			wantMessage: prefix + "Grok upstream timeout",
			wantTimeout: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				now := time.UnixMilli(1_700_000_000_000)
				store := mustNewAuthStore(t, grok.AuthOptions{File: validAuthFixture(t, now), Transport: denyNetworkTransport(t), Now: func() time.Time { return now }})
				body := `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`

				got := handleOverPipe(t, store, body, tt.serve)

				if got.dials < 1 {
					t.Errorf("upstream dials = %d, want at least 1", got.dials)
				}
				if tt.wantTimeout && got.elapsed < time.Hour {
					t.Errorf("the silent upstream was answered after %v of virtual time, want the one-hour inactivity timeout", got.elapsed)
				}
				if !tt.wantTimeout && got.elapsed >= time.Hour {
					t.Errorf("the failed send took %v of virtual time, want less than the inactivity timeout", got.elapsed)
				}
				for _, detail := range []string{"refused", "EOF", "malformed", "SENTINEL_TRANSPORT_DETAIL", "grok.fixture.test"} {
					if strings.Contains(got.body, detail) {
						t.Errorf("the 502 body carries transport detail %q", detail)
					}
				}
				checkPipedAnswer(t, got, http.StatusBadGateway, errorEnvelope("api_error", tt.wantMessage), failed)
			})
		})
	}
}

// TestBranchUncoercibleUpstreamErrorMessage verifies the reply to an upstream error status whose
// selected message would make the source's template literal throw: the port keeps the status text
// "Grok upstream returned N" with the upstream status and the status's error type. A message that
// converts without throwing keeps its converted text, as in the source.
func TestBranchUncoercibleUpstreamErrorMessage(t *testing.T) {
	tests := map[string]struct {
		status      int
		upstream    string
		wantType    string
		wantMessage string
	}{
		"error: an object message with its own toString keeps the 500 status text": {
			status:      http.StatusInternalServerError,
			upstream:    `{"error":{"message":{"toString":"SENTINEL_UPSTREAM_DETAIL"}}}`,
			wantType:    "api_error",
			wantMessage: "model-gateway: Grok upstream returned 500",
		},
		"error: an array message holding such an object keeps the 401 status text and authentication_error": {
			status:      http.StatusUnauthorized,
			upstream:    `{"error":{"message":["SENTINEL_UPSTREAM_DETAIL",{"toString":1}]}}`,
			wantType:    "authentication_error",
			wantMessage: "model-gateway: Grok upstream returned 401",
		},
		"error: a nested array message with a null toString keeps the 429 status text": {
			status:      http.StatusTooManyRequests,
			upstream:    `{"error":{"message":[["SENTINEL_UPSTREAM_DETAIL"],[[{"toString":null}]]],"type":"rate_limit_error"}}`,
			wantType:    "api_error",
			wantMessage: "model-gateway: Grok upstream returned 429",
		},
		"success: an object message without toString keeps its converted text": {
			status:      http.StatusInternalServerError,
			upstream:    `{"error":{"message":{"code":"overloaded"}}}`,
			wantType:    "api_error",
			wantMessage: "model-gateway: [object Object]",
		},
		"success: an array message joins its elements as String() does": {
			status:      http.StatusServiceUnavailable,
			upstream:    `{"error":{"message":["busy",null,["retry",2]]}}`,
			wantType:    "api_error",
			wantMessage: "model-gateway: busy,,retry,2",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				now := time.UnixMilli(1_700_000_000_000)
				store := mustNewAuthStore(t, grok.AuthOptions{File: validAuthFixture(t, now), Transport: denyNetworkTransport(t), Now: func() time.Time { return now }})
				body := `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`

				got := handleOverPipe(t, store, body, answerPiped(pipedJSONResponse(tt.status, tt.upstream)))

				if got.dials != 1 {
					t.Errorf("upstream dials = %d, want 1", got.dials)
				}
				if strings.Contains(got.body, "SENTINEL_UPSTREAM_DETAIL") {
					t.Error("the reply carries part of a message whose conversion throws")
				}
				checkPipedAnswer(t, got, tt.status, errorEnvelope(tt.wantType, tt.wantMessage), outcome.Result{Route: pipeRoute, Status: tt.status, Contacted: true})
			})
		})
	}
}

// fixtureIssuer is an OIDC issuer on a loopback listener the test owns.
type fixtureIssuer struct {
	url         string
	transport   http.RoundTripper
	discoveries atomic.Int32
	exchanges   atomic.Int32
}

// startFixtureIssuer serves discovery and a refresh-token exchange that accepts only
// fixture-refresh-token for fixture-client. An accepted exchange runs beforeAnswer, when set, and
// then answers tokenBody. The returned transport dials only this issuer.
func startFixtureIssuer(t *testing.T, tokenBody string, beforeAnswer func()) *fixtureIssuer {
	t.Helper()
	iss := &fixtureIssuer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			iss.discoveries.Add(1)
			_, _ = fmt.Fprintf(w, `{"token_endpoint":"http://%s/token"}`, r.Host)
		case "/token":
			raw, _ := io.ReadAll(r.Body)
			form, err := url.ParseQuery(string(raw))
			if err != nil || r.Method != http.MethodPost || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "fixture-refresh-token" || form.Get("client_id") != "fixture-client" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
			iss.exchanges.Add(1)
			if beforeAnswer != nil {
				beforeAnswer()
			}
			_, _ = io.WriteString(w, tokenBody)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(srv.Client().CloseIdleConnections)

	var dialer net.Dialer
	addr := srv.Listener.Addr().String()
	iss.url = srv.URL
	iss.transport = newAuthTransport(t, upstream.Options{
		DialContext: func(ctx context.Context, network, target string) (net.Conn, error) {
			if target != addr {
				t.Error("blocked an unapproved auth dial")
				return nil, fmt.Errorf("blocked unapproved dial to %q", target)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	})
	return iss
}

// storedEntry reads the x.ai entry of the auth file.
func storedEntry(t *testing.T, file string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read persisted auth file: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal("persisted auth file is not a JSON object")
	}
	entry, ok := root["https://auth.x.ai::fixture-client"].(map[string]any)
	if !ok {
		t.Fatal("persisted auth file lost its x.ai entry")
	}
	return entry
}

// TestAccessTokenStoredExpiryGrammar verifies how a stored expiry is read. The source read a string
// with V8's Date.parse, which also accepts legacy and local-time spellings, and a Number() that
// throws rejected the call. The port reads strings with the explicit date grammar; a spelling
// outside it, or a value whose numeric conversion would throw, leaves the expiry unverifiable: it
// is refreshed, or refused in shadow mode without network or disk writes. Spellings the grammar
// accepts, and values Number() converts, keep their expiry. Every expiry here lies a day ahead of
// the fixed clock, so V8 would have read each string spelling as fresh in any time zone.
func TestAccessTokenStoredExpiryGrammar(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		// expiresAt is the raw JSON value stored as expires_at.
		expiresAt  string
		verifiable bool
	}{
		"error: an ISO date-time without a zone is unverifiable": {
			expiresAt: `"2026-11-27T13:00:00"`,
		},
		"error: an ISO date-time with a space separator is unverifiable": {
			expiresAt: `"2026-11-27 13:00:00Z"`,
		},
		"error: an ISO date-time with an expanded year is unverifiable": {
			expiresAt: `"+002026-11-27T13:00:00Z"`,
		},
		"error: a slashed date is unverifiable": {
			expiresAt: `"2026/11/27 13:00:00 UTC"`,
		},
		"error: the Date.prototype.toString spelling is unverifiable": {
			expiresAt: `"Fri Nov 27 2026 13:00:00 GMT+0000"`,
		},
		"error: an IMF date without its weekday is unverifiable": {
			expiresAt: `"27 Nov 2026 13:00:00 GMT"`,
		},
		"error: a month-name date is unverifiable": {
			expiresAt: `"November 27, 2026 13:00:00 UTC"`,
		},
		"error: an object whose own toString makes Number() throw is unverifiable": {
			expiresAt: `{"toString":1795784400}`,
		},
		"error: an array holding such an object is unverifiable": {
			expiresAt: `[{"toString":"1795784400"}]`,
		},
		"error: an object with a null toString and a valueOf is unverifiable": {
			expiresAt: `{"toString":null,"valueOf":1795784400}`,
		},
		"success: an IMF-fixdate is read and fresh": {
			expiresAt:  `"Fri, 27 Nov 2026 13:00:00 GMT"`,
			verifiable: true,
		},
		"success: an ISO date-time with a zone is read and fresh": {
			expiresAt:  `"2026-11-27T13:00:00Z"`,
			verifiable: true,
		},
		"success: a one-element array converts to its number of seconds and is fresh": {
			expiresAt:  `[1795784400]`,
			verifiable: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, shadow := range []bool{false, true} {
				t.Run("shadow="+strconv.FormatBool(shadow), func(t *testing.T) {
					t.Cleanup(func() { goleak.VerifyNone(t) })
					refreshes := !tt.verifiable && !shadow

					var (
						iss       *fixtureIssuer
						transport = denyNetworkTransport(t)
						issuerURL = "http://127.0.0.1:1"
					)
					if refreshes {
						iss = startFixtureIssuer(t, `{"access_token":"fixture-fresh-token","expires_in":3600}`, nil)
						transport, issuerURL = iss.transport, iss.url
					}
					dir := t.TempDir()
					file := filepath.Join(dir, "auth.json")
					initial := `{"https://auth.x.ai::fixture-client":{"key":"fixture-current-token","refresh_token":"fixture-refresh-token","expires_at":` + tt.expiresAt + `,"oidc_client_id":"fixture-client","oidc_issuer":"` + issuerURL + `"}}`
					writeFixtureFile(t, file, initial)
					var before os.FileInfo
					if !refreshes {
						before = freezeAuthFixture(t, dir, file)
					}
					store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: transport, Now: func() time.Time { return now }, Shadow: shadow})

					got, err := store.AccessToken(t.Context())
					if err != nil {
						assertNoSecrets(t, "AccessToken() error", err.Error(), "fixture-current-token", "fixture-refresh-token", "fixture-fresh-token")
					}
					switch {
					case refreshes:
						if err != nil {
							t.Fatalf("AccessToken() error = %v, want a refreshed token", err)
						}
						if got != "fixture-fresh-token" {
							t.Errorf("AccessToken() returned an unexpected token (len=%d), want the refreshed one", len(got))
						}
						if d, x := iss.discoveries.Load(), iss.exchanges.Load(); d != 1 || x != 1 {
							t.Errorf("issuer saw %d discoveries and %d accepted exchanges, want 1 and 1", d, x)
						}
						entry := storedEntry(t, file)
						if entry["key"] != "fixture-fresh-token" {
							t.Error("the refreshed key was not stored")
						}
						if diff := gocmp.Diff(float64(now.UnixMilli()+3600*1000), entry["expires_at"]); diff != "" {
							t.Errorf("stored expires_at (-want +got):\n%s", diff)
						}
					case !tt.verifiable:
						if !errors.Is(err, grok.ErrAuthCannotRefresh) || err.Error() != grok.ErrAuthCannotRefresh.Error() {
							t.Fatalf("AccessToken() error = %v, want exactly %v", err, grok.ErrAuthCannotRefresh)
						}
						if got != "" {
							t.Errorf("AccessToken() returned a token (len=%d) with its error", len(got))
						}
						assertAuthFixtureUntouched(t, dir, file, before, initial)
					default:
						if err != nil {
							t.Fatalf("AccessToken() error = %v, want the stored token", err)
						}
						if got != "fixture-current-token" {
							t.Errorf("AccessToken() returned an unexpected token (len=%d), want the stored one", len(got))
						}
						assertAuthFixtureUntouched(t, dir, file, before, initial)
					}
				})
			}
		})
	}
}

// TestRefreshIssuerLifetimeCoercion verifies the issuer's expires_in after a successful exchange.
// Where the source's Number() would throw and discard the rotation, the port stores the rotated key
// and refresh token and leaves expires_at as it was; a lifetime Number() converts sets it. The
// stored expiry starts as a string, so an unchanged value cannot be mistaken for a computed one.
func TestRefreshIssuerLifetimeCoercion(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const storedExpiry = "2026-11-26T11:00:00Z"

	tests := map[string]struct {
		// expiresIn is the raw JSON value of the issuer's expires_in.
		expiresIn     string
		wantExpiresAt any
	}{
		"success: an object lifetime with its own toString leaves expires_at unchanged": {
			expiresIn:     `{"toString":3600}`,
			wantExpiresAt: storedExpiry,
		},
		"success: an array lifetime holding such an object leaves expires_at unchanged": {
			expiresIn:     `[{"toString":"3600"}]`,
			wantExpiresAt: storedExpiry,
		},
		"success: a nested array lifetime with a null toString leaves expires_at unchanged": {
			expiresIn:     `[["3600",{"toString":null}]]`,
			wantExpiresAt: storedExpiry,
		},
		"success: a numeric string lifetime sets expires_at": {
			expiresIn:     `"3600"`,
			wantExpiresAt: float64(now.UnixMilli() + 3600*1000),
		},
		"success: a one-element array lifetime sets expires_at": {
			expiresIn:     `[1200]`,
			wantExpiresAt: float64(now.UnixMilli() + 1200*1000),
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			iss := startFixtureIssuer(t, `{"access_token":"fixture-fresh-token","refresh_token":"fixture-rotated-refresh","expires_in":`+tt.expiresIn+`}`, nil)
			file := filepath.Join(t.TempDir(), "auth.json")
			writeFixtureFile(t, file, `{"https://auth.x.ai::fixture-client":{"key":"fixture-current-token","refresh_token":"fixture-refresh-token","expires_at":"`+storedExpiry+`","oidc_client_id":"fixture-client","oidc_issuer":"`+iss.url+`"}}`)
			store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: iss.transport, Now: func() time.Time { return now }})

			got, err := store.Refresh(t.Context())
			if err != nil {
				assertNoSecrets(t, "Refresh() error", err.Error(), "fixture-current-token", "fixture-refresh-token", "fixture-fresh-token", "fixture-rotated-refresh")
				t.Fatalf("Refresh() error = %v, want the rotated key", err)
			}
			if got != "fixture-fresh-token" {
				t.Errorf("Refresh() returned an unexpected token (len=%d), want the rotated one", len(got))
			}
			if d, x := iss.discoveries.Load(), iss.exchanges.Load(); d != 1 || x != 1 {
				t.Errorf("issuer saw %d discoveries and %d accepted exchanges, want 1 and 1", d, x)
			}
			info, err := os.Stat(file)
			if err != nil {
				t.Fatalf("stat persisted auth file: %v", err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("persisted auth file mode = %04o, want 0600", info.Mode().Perm())
			}
			entry := storedEntry(t, file)
			if entry["key"] != "fixture-fresh-token" {
				t.Error("the rotated key was not stored")
			}
			if entry["refresh_token"] != "fixture-rotated-refresh" {
				t.Error("the rotated refresh token was not stored")
			}
			delete(entry, "key")
			delete(entry, "refresh_token")
			want := map[string]any{"expires_at": tt.wantExpiresAt, "oidc_client_id": "fixture-client", "oidc_issuer": iss.url}
			if diff := gocmp.Diff(want, entry); diff != "" {
				t.Errorf("stored entry without its credentials (-want +got):\n%s", diff)
			}
		})
	}
}

// TestRefreshStoreReplacementFailure verifies a successful exchange whose rotated credential cannot
// be stored. The source let the filesystem error escape and reach the client; the port returns
// exactly ErrAuthRefreshFailed, neither the new key nor the error's path, and the branch answers
// the fixed refresh failure in its 401 without contacting the upstream. The fake issuer breaks the
// store while it answers the exchange, after the file was read and before it is replaced; the
// breakage does not depend on file permissions, so it holds for any user.
func TestRefreshStoreReplacementFailure(t *testing.T) {
	const (
		unsavedKey     = "fixture-unsaved-token"
		unsavedRefresh = "fixture-unsaved-refresh"
	)
	tokenBody := `{"access_token":"` + unsavedKey + `","refresh_token":"` + unsavedRefresh + `","expires_in":3600}`

	// A breakage moves the auth file's directory or the file itself to held and puts something in
	// its place; it returns the path where the original file now lies and a check of what took
	// the original's place. It runs on the issuer's handler goroutine, so it reports with Errorf.
	type breakage func(t *testing.T, authDir, held string) (original string, check func(t *testing.T))
	parentBecomesFile := func(t *testing.T, authDir, held string) (string, func(t *testing.T)) {
		if err := os.Rename(authDir, held); err != nil {
			t.Errorf("move the auth directory away: %v", err)
		}
		if err := os.WriteFile(authDir, []byte("occupant"), 0o600); err != nil {
			t.Errorf("put a file at the auth directory's path: %v", err)
		}
		return filepath.Join(held, "auth.json"), func(t *testing.T) {
			t.Helper()
			raw, err := os.ReadFile(authDir)
			if err != nil || string(raw) != "occupant" {
				t.Errorf("the file put in the auth directory's place changed (err=%v, len=%d)", err, len(raw))
			}
		}
	}
	leafBecomesDirectory := func(t *testing.T, authDir, held string) (string, func(t *testing.T)) {
		file := filepath.Join(authDir, "auth.json")
		original := filepath.Join(held, "auth.json")
		if err := os.Mkdir(held, 0o700); err != nil {
			t.Errorf("create the holding directory: %v", err)
		}
		if err := os.Rename(file, original); err != nil {
			t.Errorf("move the auth file away: %v", err)
		}
		if err := os.Mkdir(file, 0o700); err != nil {
			t.Errorf("put a directory at the auth file's path: %v", err)
		}
		if err := os.WriteFile(filepath.Join(file, "occupant"), []byte("occupant"), 0o600); err != nil {
			t.Errorf("fill the directory at the auth file's path: %v", err)
		}
		return original, func(t *testing.T) {
			t.Helper()
			entries, err := os.ReadDir(authDir)
			if err != nil {
				t.Fatalf("read the auth directory: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != "auth.json" || !entries[0].IsDir() {
				t.Errorf("the auth directory holds %d entries after the failed replacement, want only the directory put at auth.json", len(entries))
			}
			inner, err := os.ReadDir(file)
			if err != nil || len(inner) != 1 || inner[0].Name() != "occupant" {
				t.Errorf("the directory put at auth.json changed (err=%v, entries=%d)", err, len(inner))
			}
		}
	}

	tests := map[string]struct {
		breakStore breakage
		viaBranch  bool
	}{
		"error: Refresh reports the refresh failure when the auth directory became a file": {
			breakStore: parentBecomesFile,
		},
		"error: Refresh reports the refresh failure when the auth file became a directory": {
			breakStore: leafBecomesDirectory,
		},
		"error: the branch answers the refresh failure in its 401 when the auth directory became a file": {
			breakStore: parentBecomesFile,
			viaBranch:  true,
		},
		"error: the branch answers the refresh failure in its 401 when the auth file became a directory": {
			breakStore: leafBecomesDirectory,
			viaBranch:  true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
			base := t.TempDir()
			authDir := filepath.Join(base, "grok")
			held := filepath.Join(base, "held")
			file := filepath.Join(authDir, "auth.json")
			if err := os.Mkdir(authDir, 0o700); err != nil {
				t.Fatalf("create the auth directory: %v", err)
			}

			// The breakage runs on the issuer's handler goroutine; mu hands its results to this one.
			var (
				mu         sync.Mutex
				original   string
				checkPlace func(t *testing.T)
			)
			iss := startFixtureIssuer(t, tokenBody, func() {
				o, check := tt.breakStore(t, authDir, held)
				mu.Lock()
				original, checkPlace = o, check
				mu.Unlock()
			})
			initial := `{"https://auth.x.ai::fixture-client":{"key":"fixture-expired-token","refresh_token":"fixture-refresh-token","expires_at":0,"oidc_client_id":"fixture-client","oidc_issuer":"` + iss.url + `"}}`
			writeFixtureFile(t, file, initial)
			store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: iss.transport, Now: func() time.Time { return now }})
			leaks := []string{unsavedKey, unsavedRefresh, base, "fixture-expired-token", "fixture-refresh-token"}

			if tt.viaBranch {
				var dials atomic.Int32
				client := upstream.NewClient(upstream.Options{
					DialContext: func(context.Context, string, string) (net.Conn, error) {
						dials.Add(1)
						return nil, errors.New("blocked an inference dial")
					},
				})
				t.Cleanup(func() { _ = client.Close() })
				branch := mustNewBranch(t, grok.BranchOptions{Endpoint: pipeEndpoint, Auth: store, Client: client, Now: func() time.Time { return now }})
				body := `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`
				r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/messages", strings.NewReader(body))
				w := httptest.NewRecorder()

				res, err := branch.Handle(w, r, grok.Request{RawBody: []byte(body), Path: "/v1/messages", AdvertisedModel: "claude-grok-4.7[1m]", BackendModel: "grok-4.7"})

				got := pipedOutcome{status: w.Code, header: w.Header().Clone(), body: w.Body.String(), res: res, err: err}
				assertNoSecrets(t, "401 body", got.body, leaks...)
				if n := dials.Load(); n != 0 {
					t.Errorf("upstream dials = %d, want 0", n)
				}
				checkPipedAnswer(t, got, http.StatusUnauthorized,
					errorEnvelope("authentication_error", "model-gateway: Grok CLI auth refresh failed. Run `grok` and log in again."),
					outcome.Result{Route: pipeRoute, Status: http.StatusUnauthorized, StatusOverride: "authentication_error"})
			} else {
				got, err := store.Refresh(t.Context())
				if err != nil {
					assertNoSecrets(t, "Refresh() error", err.Error(), leaks...)
				}
				if !errors.Is(err, grok.ErrAuthRefreshFailed) || err.Error() != grok.ErrAuthRefreshFailed.Error() {
					t.Fatalf("Refresh() error = %v, want exactly %v", err, grok.ErrAuthRefreshFailed)
				}
				if got != "" {
					t.Errorf("Refresh() returned a token (len=%d) with its error", len(got))
				}
			}

			if d, x := iss.discoveries.Load(), iss.exchanges.Load(); d != 1 || x != 1 {
				t.Fatalf("issuer saw %d discoveries and %d accepted exchanges, want 1 and 1", d, x)
			}
			mu.Lock()
			heldFile, check := original, checkPlace
			mu.Unlock()
			raw, err := os.ReadFile(heldFile)
			if err != nil {
				t.Fatalf("read the original auth file: %v", err)
			}
			if string(raw) != initial {
				t.Errorf("the original auth file changed (len=%d, want len=%d)", len(raw), len(initial))
			}
			entries, err := os.ReadDir(held)
			if err != nil {
				t.Fatalf("read the holding directory: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != "auth.json" {
				t.Errorf("the holding directory has %d entries, want only auth.json (no staged replacement)", len(entries))
			}
			check(t)
		})
	}
}

// TestBranchOutdatedVersionDrainsBody verifies that the body of an upstream 426 is read to its end
// and discarded rather than kept: the source collects every chunk of that body and never uses
// them, while the port's memory stays bounded whatever its size. The fixture upstream sends a
// 32 MiB body from one small reused buffer. The allocation count is process-wide, so the fixture
// runs several times and the smallest count is compared: more than a quarter of the body in every
// run can only come from a reader that keeps it. In each run the whole body is read and the client
// gets the fixed outdated-version answer; the test does not observe which of the two comes first.
func TestBranchOutdatedVersionDrainsBody(t *testing.T) {
	const (
		bodySize  = 32 << 20
		chunkSize = 64 << 10
		// allocLimit is a quarter of the body: far above what draining the body allocates, far
		// below what keeping it would.
		allocLimit = bodySize / 4
		// fixtureRuns is how many times the fixture runs; the smallest allocation count of the runs
		// is compared with allocLimit.
		fixtureRuns = 3
		// outdatedMessage restates the production text outdatedVersionMessage.
		outdatedMessage = "model-gateway: Grok CLI version header is outdated. Update the grok CLI, then retry."
	)
	head := fmt.Sprintf("HTTP/1.1 426 Upgrade Required\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", bodySize)
	chunk := []byte(strings.Repeat("x", chunkSize))

	tests := map[string]struct {
		wantResult outcome.Result
	}{
		"success: a 32 MiB outdated-version body is drained and the fixed 426 answered": {
			wantResult: outcome.Result{Route: pipeRoute, Status: http.StatusUpgradeRequired, Contacted: true},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				now := time.UnixMilli(1_700_000_000_000)
				body := `{"model":"claude-grok-4.7[1m]","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`

				var allocations []uint64
				for run := range fixtureRuns {
					store := mustNewAuthStore(t, grok.AuthOptions{File: validAuthFixture(t, now), Transport: denyNetworkTransport(t), Now: func() time.Time { return now }})
					var written atomic.Int64
					serve := func(conn net.Conn) {
						if !readPipedRequest(conn) {
							return
						}
						if _, err := io.WriteString(conn, head); err != nil {
							return
						}
						for written.Load() < bodySize {
							n, err := conn.Write(chunk)
							written.Add(int64(n))
							if err != nil {
								return
							}
						}
					}

					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					got := handleOverPipe(t, store, body, serve)
					runtime.ReadMemStats(&after)
					allocations = append(allocations, after.TotalAlloc-before.TotalAlloc)

					if got.dials != 1 {
						t.Errorf("run %d: upstream dials = %d, want 1", run, got.dials)
					}
					if n := written.Load(); n != bodySize {
						t.Errorf("run %d: the client read %d bytes of the %d byte outdated-version body, want all of it", run, n, bodySize)
					}
					if got.elapsed >= time.Hour {
						t.Errorf("run %d: the drain took %v of virtual time, want less than the inactivity timeout", run, got.elapsed)
					}
					checkPipedAnswer(t, got, http.StatusUpgradeRequired, errorEnvelope("api_error", outdatedMessage), tt.wantResult)
				}
				if allocated := slices.Min(allocations); allocated >= allocLimit {
					t.Errorf("answering a %d byte outdated-version body allocated at least %d bytes in each of %d runs (%v), want less than %d in one (the body is kept, not drained)", bodySize, allocated, fixtureRuns, allocations, allocLimit)
				}
			})
		})
	}
}

// TestAccessTokenNonPositiveSkew verifies that an explicit refresh skew of zero or below is
// replaced by the default lead time of five minutes, where the source honours it: with either, a
// key expiring exactly five minutes ahead is refreshed and one expiring a millisecond later is used
// as stored, and an hour of negative skew does not make a key that expired half an hour ago usable.
// A positive skew is used as given.
func TestAccessTokenNonPositiveSkew(t *testing.T) {
	// defaultSkewMillis restates the production default DefaultRefreshSkew (five minutes).
	const defaultSkewMillis = 5 * 60 * 1000

	tests := map[string]struct {
		skew time.Duration
		// expiresIn is the stored expiry in milliseconds after the fixture clock.
		expiresIn   int64
		wantRefresh bool
	}{
		"success: zero skew refreshes a key expiring exactly five minutes ahead": {
			expiresIn:   defaultSkewMillis,
			wantRefresh: true,
		},
		"success: zero skew uses a key expiring a millisecond past five minutes as stored": {
			expiresIn: defaultSkewMillis + 1,
		},
		"success: negative skew refreshes a key expiring exactly five minutes ahead": {
			skew:        -time.Minute,
			expiresIn:   defaultSkewMillis,
			wantRefresh: true,
		},
		"success: negative skew uses a key expiring a millisecond past five minutes as stored": {
			skew:      -time.Minute,
			expiresIn: defaultSkewMillis + 1,
		},
		"success: an hour of negative skew refreshes a key that expired half an hour ago": {
			skew:        -time.Hour,
			expiresIn:   -30 * 60 * 1000,
			wantRefresh: true,
		},
		"success: a positive one-minute skew uses a key expiring in four minutes as stored": {
			skew:      time.Minute,
			expiresIn: 4 * 60 * 1000,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			dir := t.TempDir()
			file := filepath.Join(dir, "auth.json")
			expiresAt := strconv.FormatInt(refreshFixtureNow.UnixMilli()+tt.expiresIn, 10)
			content := `{"https://auth.x.ai::fixture-client":{"key":"fixture-expired-token","refresh_token":"fixture-refresh-token","expires_at":` + expiresAt + `,"oidc_client_id":"fixture-client","oidc_issuer":"` + refreshFixtureIssuer + `"}}`
			writeFixtureFile(t, file, content)

			var tr http.RoundTripper
			var exchanges atomic.Int32
			var before os.FileInfo
			if tt.wantRefresh {
				tr = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/.well-known/openid-configuration" {
						return jsonHTTPResponse(req, http.StatusOK, `{"token_endpoint":"`+refreshFixtureIssuer+`/discovered/token"}`), nil
					}
					exchanges.Add(1)
					return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-fresh-token","expires_in":3600}`), nil
				})
			} else {
				tr = denyNetworkTransport(t)
				before = freezeAuthFixture(t, dir, file)
			}
			store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }, Skew: tt.skew})

			token, err := store.AccessToken(t.Context())
			if err != nil {
				assertNoSecrets(t, "AccessToken error", err.Error(), refreshSecrets...)
				t.Fatalf("AccessToken() error = %v", err)
			}
			if !tt.wantRefresh {
				if token != "fixture-expired-token" {
					t.Errorf("AccessToken() did not return the stored key of an expiry outside the skew (len=%d)", len(token))
				}
				assertAuthFixtureUntouched(t, dir, file, before, content)
				return
			}
			if token != "fixture-fresh-token" {
				t.Errorf("AccessToken() returned the stored key with skew %v instead of refreshing inside the five-minute default (len=%d)", tt.skew, len(token))
			}
			if got := exchanges.Load(); got != 1 {
				t.Errorf("exchanges = %d, want 1", got)
			}
		})
	}
}
