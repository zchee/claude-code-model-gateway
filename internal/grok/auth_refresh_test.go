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
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
)

// refreshFixtureNow is the fixed clock of the refresh tests, on a whole millisecond.
var refreshFixtureNow = time.UnixMilli(1_700_000_000_000)

// refreshFixtureIssuer is the issuer every refresh fixture names; only the test's own
// RoundTripper ever answers it.
const refreshFixtureIssuer = "http://auth.fixture.test"

// refreshSecrets are the credential values the refresh fixtures write; none may reach an error.
var refreshSecrets = []string{
	"fixture-expired-token",
	"fixture-refresh-token",
	"fixture-fresh-token",
	"fixture-external-key",
	"fixture-external-refresh",
	"fixture-exchanged-key",
	"fixture-issued-key",
	"fixture-bounded-key",
	"fixture-fallback-key",
	"fixture-rotated-key",
	"fixture-rotated-refresh",
}

// tokenCall is one of the two AuthStore entry points that may start a shared refresh.
type tokenCall func(*grok.AuthStore, context.Context) (string, error)

// refreshEntryPoints names both entry points that may start or join the shared refresh.
var refreshEntryPoints = map[string]tokenCall{
	"AccessToken": (*grok.AuthStore).AccessToken,
	"Refresh":     (*grok.AuthStore).Refresh,
}

type tokenResult struct {
	token string
	err   error
}

// expiredAuthJSON is an auth file whose key has expired and whose refresh goes to the fixture issuer.
func expiredAuthJSON() string {
	return `{"https://auth.x.ai::fixture-client":{"key":"fixture-expired-token","refresh_token":"fixture-refresh-token","expires_at":0,"oidc_client_id":"fixture-client","oidc_issuer":"` + refreshFixtureIssuer + `"}}`
}

func readFileString(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read auth fixture: %v", err)
	}
	return string(raw)
}

// assertFileUnchanged reports a refresh that rewrote the auth file although it must not have.
func assertFileUnchanged(t *testing.T, file, want string) {
	t.Helper()
	if got := readFileString(t, file); got != want {
		t.Errorf("auth file was rewritten (got len=%d, want len=%d)", len(got), len(want))
	}
}

// assertExactErr checks the sentinel and its exact text, and that the text carries no credential.
func assertExactErr(t *testing.T, label string, err, want error) {
	t.Helper()
	if err != nil {
		assertNoSecrets(t, label+" error", err.Error(), refreshSecrets...)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s error = %v, want %v", label, err, want)
	}
	if err.Error() != want.Error() {
		t.Errorf("%s error text = %q, want exact %q", label, err.Error(), want.Error())
	}
}

// startTokenCall runs call on its own goroutine and delivers its result on the returned channel.
func startTokenCall(store *grok.AuthStore, ctx context.Context, call tokenCall) <-chan tokenResult {
	ch := make(chan tokenResult, 1)
	go func() {
		token, err := call(store, ctx)
		ch <- tokenResult{token: token, err: err}
	}()
	return ch
}

// awaitTokenCall waits in virtual time for a call started with startTokenCall. When the budget
// passes, it reports the call as stuck, releases every fixture stall and joins the call, so the
// bubble never deadlocks and the package's other results survive.
func awaitTokenCall(t *testing.T, label string, ch <-chan tokenResult, budget time.Duration, release func()) tokenResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(budget):
		t.Errorf("%s was still pending after %v", label, budget)
		release()
		return <-ch
	}
}

// freshAuthJSON is an auth file whose key stays valid for another hour.
func freshAuthJSON() string {
	return fmt.Sprintf(`{"https://auth.x.ai::fixture-client":{"key":"fixture-fresh-token","refresh_token":"fixture-refresh-token","expires_at":%d,"oidc_client_id":"fixture-client","oidc_issuer":"%s"}}`, refreshFixtureNow.Add(time.Hour).UnixMilli(), refreshFixtureIssuer)
}

// discoveryResponse answers OIDC discovery with the fixture issuer's token endpoint.
func discoveryResponse(req *http.Request) *http.Response {
	return jsonHTTPResponse(req, http.StatusOK, `{"token_endpoint":"`+refreshFixtureIssuer+`/discovered/token"}`)
}

// rotatingIssuer is a fake token endpoint that honours only the refresh token it issued last. An
// exchange that presents that token supersedes it on receipt, before any reply is sent, as an
// issuer that rotates refresh tokens does; an exchange that presents any other token is refused.
type rotatingIssuer struct {
	mu       sync.Mutex
	valid    string
	accepted int
	refused  int
}

func newRotatingIssuer(valid string) *rotatingIssuer { return &rotatingIssuer{valid: valid} }

// exchange checks an exchange request and returns the status and body of its reply. The n-th
// accepted exchange issues fixture-rotated-key-n and fixture-rotated-refresh-n.
func (i *rotatingIssuer) exchange(req *http.Request) (int, string) {
	raw, readErr := io.ReadAll(req.Body)
	form, parseErr := url.ParseQuery(string(raw))
	i.mu.Lock()
	defer i.mu.Unlock()
	if readErr != nil || parseErr != nil || form.Get("grant_type") != "refresh_token" || form.Get("client_id") != "fixture-client" || form.Get("refresh_token") != i.valid {
		i.refused++
		return http.StatusBadRequest, `{"error":"invalid_grant"}`
	}
	i.accepted++
	i.valid = fmt.Sprintf("fixture-rotated-refresh-%d", i.accepted)
	return http.StatusOK, fmt.Sprintf(`{"access_token":"fixture-rotated-key-%d","refresh_token":%q,"expires_in":3600}`, i.accepted, i.valid)
}

// rotate records a rotation made by another client of the issuer: only next is honoured from now on.
func (i *rotatingIssuer) rotate(next string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.valid = next
}

func (i *rotatingIssuer) counts() (accepted, refused int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.accepted, i.refused
}

// gatedBody is a response body that delivers content once gate is closed. When the request's
// context ends first, the read fails with the context's error and cancelled counts it, as a
// transport that honours cancellation behaves.
type gatedBody struct {
	ctx       context.Context
	gate      <-chan struct{}
	content   io.Reader
	cancelled *atomic.Int32
}

func (b *gatedBody) Read(p []byte) (int, error) {
	select {
	case <-b.gate:
		return b.content.Read(p)
	default:
	}
	select {
	case <-b.gate:
		return b.content.Read(p)
	case <-b.ctx.Done():
		b.cancelled.Add(1)
		return 0, b.ctx.Err()
	}
}

func (b *gatedBody) Close() error { return nil }

// stalledBody is a body whose bytes never arrive before the request's context ends or teardown.
func stalledBody(req *http.Request, teardown <-chan struct{}, cancelled *atomic.Int32) io.ReadCloser {
	return &gatedBody{ctx: req.Context(), gate: teardown, content: strings.NewReader(""), cancelled: cancelled}
}

// storedCredential returns the key and refresh token the auth file holds now.
func storedCredential(t *testing.T, file string) (key, refreshToken string) {
	t.Helper()
	var doc map[string]struct {
		Key          string `json:"key"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(readFileString(t, file)), &doc); err != nil {
		t.Fatal("auth file is not a JSON object of credential entries")
	}
	entry := doc["https://auth.x.ai::fixture-client"]
	return entry.Key, entry.RefreshToken
}

// receiveNow returns the result of a call that must already have finished, at the current instant
// of virtual time. When it has not, it reports that, releases every fixture stall and joins it.
func receiveNow(t *testing.T, label string, ch <-chan tokenResult, release func()) tokenResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	default:
		t.Errorf("%s was still waiting after it left, with no virtual time passed", label)
		release()
		return <-ch
	}
}

// TestRefreshWithEndedContext verifies that a caller whose context has already ended gets its
// context's error and touches nothing: the answer does not depend on the auth file, no request
// reaches the issuer, no refresh starts and the file stays as it was. That holds when the stored key
// is fresh and in shadow mode too, where the context's error comes before the shadow refusal. A
// context that ends after the entry check, during the caller's expiry check, still joins no refresh.
//
// The file's part in the answer shows in the result: a missing file would answer ErrAuthMissing and
// a fresh stored key would be returned, so either means the file was read for an ended context.
func TestRefreshWithEndedContext(t *testing.T) {
	expired, fresh := expiredAuthJSON(), freshAuthJSON()

	tests := map[string]struct {
		call tokenCall
		// content is the auth file; nil leaves it missing.
		content *string
		shadow  bool
		// deadline ends the context by a deadline already passed instead of by cancellation.
		deadline bool
		// endInClock starts with a live context and ends it at the caller's first clock read.
		endInClock bool
	}{
		"error: AccessToken with an ended context starts no refresh": {
			call:    refreshEntryPoints["AccessToken"],
			content: &expired,
		},
		"error: Refresh with an ended context starts no refresh": {
			call:    refreshEntryPoints["Refresh"],
			content: &expired,
		},
		"error: AccessToken with an ended context returns no stored fresh key": {
			call:    refreshEntryPoints["AccessToken"],
			content: &fresh,
		},
		"error: AccessToken with a passed deadline returns no stored fresh key": {
			call:     refreshEntryPoints["AccessToken"],
			content:  &fresh,
			deadline: true,
		},
		"error: AccessToken with an ended context reports the context rather than a missing auth file": {
			call: refreshEntryPoints["AccessToken"],
		},
		"error: Refresh with an ended context reports the context rather than a missing auth file": {
			call: refreshEntryPoints["Refresh"],
		},
		"error: shadow AccessToken with an ended context returns no stored fresh key": {
			call:    refreshEntryPoints["AccessToken"],
			content: &fresh,
			shadow:  true,
		},
		"error: shadow AccessToken with an ended context reports the context rather than refusing an expired key": {
			call:    refreshEntryPoints["AccessToken"],
			content: &expired,
			shadow:  true,
		},
		"error: shadow Refresh with an ended context reports the context before the shadow refusal": {
			call:    refreshEntryPoints["Refresh"],
			content: &expired,
			shadow:  true,
		},
		"error: shadow Refresh with an ended context reports the context rather than a missing auth file": {
			call:   refreshEntryPoints["Refresh"],
			shadow: true,
		},
		"error: AccessToken whose context ends during its expiry check joins no refresh": {
			call:       refreshEntryPoints["AccessToken"],
			content:    &expired,
			endInClock: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				file := filepath.Join(dir, "auth.json")
				if tt.content != nil {
					writeFixtureFile(t, file, *tt.content)
				}

				var requests atomic.Int32
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					requests.Add(1)
					if req.URL.Path == "/.well-known/openid-configuration" {
						return discoveryResponse(req), nil
					}
					return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-exchanged-key","expires_in":3600}`), nil
				})

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				wantErr := context.Canceled
				switch {
				case tt.deadline:
					ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
					defer cancel()
					wantErr = context.DeadlineExceeded
				case !tt.endInClock:
					cancel()
				}
				endOnce := sync.OnceFunc(cancel)
				clock := func() time.Time {
					if tt.endInClock {
						endOnce()
					}
					return refreshFixtureNow
				}
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: clock, Shadow: tt.shadow})

				token, err := tt.call(store, ctx)
				// Anything the call left running finishes or blocks before the assertions.
				synctest.Wait()

				if err != nil {
					assertNoSecrets(t, "ended-context error", err.Error(), refreshSecrets...)
				}
				if !errors.Is(err, wantErr) {
					t.Errorf("error = %v, want the context's error %v", err, wantErr)
				}
				if token != "" {
					t.Errorf("returned a token with an ended context (len=%d)", len(token))
				}
				if got := requests.Load(); got != 0 {
					t.Errorf("issuer requests = %d, want 0 for a caller whose context had ended", got)
				}
				entries, readErr := os.ReadDir(dir)
				if readErr != nil {
					t.Fatalf("read fixture directory: %v", readErr)
				}
				wantEntries := 0
				if tt.content != nil {
					wantEntries = 1
					assertFileUnchanged(t, file, *tt.content)
				}
				if len(entries) != wantEntries {
					t.Errorf("fixture directory entries = %d, want %d", len(entries), wantEntries)
				}
			})
		})
	}
}

// TestRefreshLastWaiterLeaving verifies that the last waiter leaving during discovery stops the
// shared refresh at once: the caller gets its context's error before any virtual time passes, the
// stalled discovery request sees its context end at that same instant, and no exchange is ever sent,
// not even after every request deadline would have passed. The auth file stays as it was.
func TestRefreshLastWaiterLeaving(t *testing.T) {
	tests := map[string]struct {
		call tokenCall
		// stallBody answers discovery's headers and stalls its body instead of stalling the headers.
		stallBody bool
	}{
		"success: the sole AccessToken caller leaving during discovery ends the shared refresh": {
			call: refreshEntryPoints["AccessToken"],
		},
		"success: the sole Refresh caller leaving during discovery ends the shared refresh": {
			call: refreshEntryPoints["Refresh"],
		},
		"success: the sole AccessToken caller leaving while the discovery body arrives ends the shared refresh": {
			call:      refreshEntryPoints["AccessToken"],
			stallBody: true,
		},
		"success: the sole Refresh caller leaving while the discovery body arrives ends the shared refresh": {
			call:      refreshEntryPoints["Refresh"],
			stallBody: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "auth.json")
				content := expiredAuthJSON()
				writeFixtureFile(t, file, content)

				teardown := make(chan struct{})
				release := sync.OnceFunc(func() { close(teardown) })
				defer release()

				var discoveries, cancelled, exchanges atomic.Int32
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodPost {
						exchanges.Add(1)
						return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-exchanged-key","expires_in":3600}`), nil
					}
					discoveries.Add(1)
					if tt.stallBody {
						resp := discoveryResponse(req)
						resp.Body = stalledBody(req, teardown, &cancelled)
						return resp, nil
					}
					select {
					case <-req.Context().Done():
						cancelled.Add(1)
						return nil, req.Context().Err()
					case <-teardown:
						return nil, errors.New("fixture issuer released at teardown")
					}
				})
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

				ctx, cancel := context.WithCancel(t.Context())
				done := startTokenCall(store, ctx, tt.call)
				synctest.Wait()
				if got := discoveries.Load(); got != 1 {
					release()
					t.Fatalf("stalled discovery requests before the caller left = %d, want 1", got)
				}

				start := time.Now()
				cancel()
				synctest.Wait()
				res := receiveNow(t, "departing caller", done, release)
				if elapsed := time.Since(start); elapsed != 0 {
					t.Errorf("departing caller got its answer after %v of virtual time, want at once", elapsed)
				}
				if res.err != nil {
					assertNoSecrets(t, "departing caller error", res.err.Error(), refreshSecrets...)
				}
				if !errors.Is(res.err, context.Canceled) {
					t.Errorf("departing caller error = %v, want %v", res.err, context.Canceled)
				}
				if res.token != "" {
					t.Errorf("departing caller returned a token (len=%d)", len(res.token))
				}
				if got := cancelled.Load(); got == 0 {
					t.Error("the stalled discovery request did not see its context end at the instant the last waiter left")
				}

				// Let every request deadline pass: a stopped refresh sends no late exchange.
				time.Sleep(time.Minute)
				synctest.Wait()
				if got := exchanges.Load(); got != 0 {
					t.Errorf("exchanges after the last waiter left during discovery = %d, want 0", got)
				}
				assertFileUnchanged(t, file, content)
			})
		})
	}
}

// TestRefreshLastWaiterLeavingDuringExchange verifies that once the exchange request has reached the
// issuer, its last waiter leaving no longer ends the refresh. The waiter gets its context's error at
// once; the refresh reads the reply and stores the rotated credential. The issuer superseded the
// presented refresh token when the request arrived, so the next refresh succeeds only by presenting
// the rotated one the refresh stored.
func TestRefreshLastWaiterLeavingDuringExchange(t *testing.T) {
	tests := map[string]struct {
		call tokenCall
		// stallBody answers the exchange's headers and holds its body instead of holding the headers.
		stallBody bool
	}{
		"success: AccessToken caller leaving before the exchange reply arrives keeps the rotated credential": {
			call: refreshEntryPoints["AccessToken"],
		},
		"success: Refresh caller leaving before the exchange reply arrives keeps the rotated credential": {
			call: refreshEntryPoints["Refresh"],
		},
		"success: AccessToken caller leaving while the exchange reply body arrives keeps the rotated credential": {
			call:      refreshEntryPoints["AccessToken"],
			stallBody: true,
		},
		"success: Refresh caller leaving while the exchange reply body arrives keeps the rotated credential": {
			call:      refreshEntryPoints["Refresh"],
			stallBody: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "auth.json")
				writeFixtureFile(t, file, expiredAuthJSON())

				issuer := newRotatingIssuer("fixture-refresh-token")
				gate := make(chan struct{})
				release := sync.OnceFunc(func() { close(gate) })
				defer release()

				var held atomic.Bool
				var cancelled atomic.Int32
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/.well-known/openid-configuration" {
						return discoveryResponse(req), nil
					}
					status, body := issuer.exchange(req)
					// Only the first exchange is held; the next refresh is answered at once.
					if !held.CompareAndSwap(false, true) {
						return jsonHTTPResponse(req, status, body), nil
					}
					if tt.stallBody {
						resp := jsonHTTPResponse(req, status, "")
						resp.Body = &gatedBody{ctx: req.Context(), gate: gate, content: strings.NewReader(body), cancelled: &cancelled}
						return resp, nil
					}
					select {
					case <-gate:
						return jsonHTTPResponse(req, status, body), nil
					case <-req.Context().Done():
						cancelled.Add(1)
						return nil, req.Context().Err()
					}
				})
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

				ctx, cancel := context.WithCancel(t.Context())
				done := startTokenCall(store, ctx, tt.call)
				synctest.Wait()
				if accepted, refused := issuer.counts(); accepted != 1 || refused != 0 {
					release()
					t.Fatalf("issuer accepted %d and refused %d exchanges before the caller left, want 1 accepted", accepted, refused)
				}

				start := time.Now()
				cancel()
				synctest.Wait()
				res := receiveNow(t, "departing caller", done, release)
				if elapsed := time.Since(start); elapsed != 0 {
					t.Errorf("departing caller got its answer after %v of virtual time, want at once", elapsed)
				}
				if res.err != nil {
					assertNoSecrets(t, "departing caller error", res.err.Error(), refreshSecrets...)
				}
				if !errors.Is(res.err, context.Canceled) {
					t.Errorf("departing caller error = %v, want %v", res.err, context.Canceled)
				}
				if res.token != "" {
					t.Errorf("departing caller returned a token (len=%d)", len(res.token))
				}

				// The reply completes after its last waiter left.
				release()
				synctest.Wait()
				if got := cancelled.Load(); got != 0 {
					t.Errorf("the exchange saw its context end %d times after its last waiter left, although the issuer had already rotated the refresh token", got)
				}
				if key, refreshToken := storedCredential(t, file); key != "fixture-rotated-key-1" || refreshToken != "fixture-rotated-refresh-1" {
					t.Errorf("auth file does not hold the rotated credential of the exchange its last waiter left (key len=%d, refresh token len=%d)", len(key), len(refreshToken))
				}

				token, err := store.Refresh(t.Context())
				if err != nil {
					assertNoSecrets(t, "next Refresh error", err.Error(), refreshSecrets...)
					t.Errorf("next Refresh error = %v, want the key of an exchange that presents the rotated refresh token", err)
				} else if token != "fixture-rotated-key-2" {
					t.Errorf("next Refresh returned an unexpected token (len=%d)", len(token))
				}
				if accepted, refused := issuer.counts(); accepted != 2 || refused != 0 {
					t.Errorf("issuer accepted %d and refused %d exchanges, want 2 accepted and none presenting a superseded refresh token", accepted, refused)
				}
			})
		})
	}
}

// flightStage is the state of the shared refresh when a late caller arrives.
type flightStage int

const (
	// noFlight: no refresh is in flight.
	noFlight flightStage = iota
	// discovering: a refresh is waiting for its discovery reply and has sent no exchange.
	discovering
	// exchangeHeadersHeld: the exchange reached the issuer and its reply headers are held.
	exchangeHeadersHeld
	// exchangeBodyHeld: the exchange reached the issuer and its reply body is held.
	exchangeBodyHeld
)

// TestAccessTokenJoinsCommittedExchange verifies that AccessToken joins an exchange already sent to
// the issuer even when the stored key is still fresh. An explicit Refresh starts with a fresh key,
// its exchange reaches the issuer, which supersedes the stored refresh token on receipt, and its
// only waiter leaves while the reply is held. A late AccessToken must not return the stored key,
// whose refresh token the issuer no longer honours: it waits for that exchange and returns the key
// it produced, and exactly one exchange runs. Without a refresh in flight, or with one still waiting
// for discovery, a fresh stored key is returned at once and no exchange is sent; a caller whose
// context has ended gets its context's error at once even while an exchange is in flight.
func TestAccessTokenJoinsCommittedExchange(t *testing.T) {
	tests := map[string]struct {
		stage flightStage
		// ended ends the late caller's context before it calls.
		ended bool
	}{
		"success: AccessToken with a fresh stored key joins an exchange whose reply headers are held after its waiter left": {
			stage: exchangeHeadersHeld,
		},
		"success: AccessToken with a fresh stored key joins an exchange whose reply body is held after its waiter left": {
			stage: exchangeBodyHeld,
		},
		"success: AccessToken with a fresh stored key and no refresh in flight returns it at once without a request": {
			stage: noFlight,
		},
		"success: AccessToken with a fresh stored key returns it at once while the shared refresh is still discovering": {
			stage: discovering,
		},
		"error: AccessToken with an ended context returns its context's error at once while an exchange is in flight": {
			stage: exchangeHeadersHeld,
			ended: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "auth.json")
				content := freshAuthJSON()
				writeFixtureFile(t, file, content)

				issuer := newRotatingIssuer("fixture-refresh-token")
				gate := make(chan struct{})
				release := sync.OnceFunc(func() { close(gate) })
				defer release()

				var requests, discoveries, cancelled atomic.Int32
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					requests.Add(1)
					if req.URL.Path == "/.well-known/openid-configuration" {
						discoveries.Add(1)
						if tt.stage == discovering {
							select {
							case <-gate:
							case <-req.Context().Done():
								cancelled.Add(1)
								return nil, req.Context().Err()
							}
						}
						return discoveryResponse(req), nil
					}
					status, body := issuer.exchange(req)
					if tt.stage == exchangeBodyHeld {
						resp := jsonHTTPResponse(req, status, "")
						resp.Body = &gatedBody{ctx: req.Context(), gate: gate, content: strings.NewReader(body), cancelled: &cancelled}
						return resp, nil
					}
					if tt.stage == exchangeHeadersHeld {
						select {
						case <-gate:
						case <-req.Context().Done():
							cancelled.Add(1)
							return nil, req.Context().Err()
						}
					}
					return jsonHTTPResponse(req, status, body), nil
				})
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

				committed := tt.stage == exchangeHeadersHeld || tt.stage == exchangeBodyHeld
				var (
					refreshing <-chan tokenResult
					leave      context.CancelFunc
				)
				if tt.stage != noFlight {
					var refreshCtx context.Context
					refreshCtx, leave = context.WithCancel(t.Context())
					defer leave()
					refreshing = startTokenCall(store, refreshCtx, refreshEntryPoints["Refresh"])
					synctest.Wait()
					accepted, refused := issuer.counts()
					switch {
					case committed && (accepted != 1 || refused != 0):
						release()
						t.Fatalf("issuer accepted %d and refused %d exchanges of the explicit Refresh, want 1 accepted and its reply held", accepted, refused)
					case !committed && (discoveries.Load() != 1 || accepted+refused != 0):
						release()
						t.Fatalf("explicit Refresh made %d discoveries and %d exchanges, want 1 discovery held and no exchange", discoveries.Load(), accepted+refused)
					}
				}
				if committed {
					// The only waiter of the committed exchange leaves; the exchange keeps running.
					leave()
					synctest.Wait()
					if res := receiveNow(t, "explicit Refresh", refreshing, release); !errors.Is(res.err, context.Canceled) {
						t.Errorf("explicit Refresh that left got %v, want %v", res.err, context.Canceled)
					}
				}

				lateCtx := t.Context()
				if tt.ended {
					var end context.CancelFunc
					lateCtx, end = context.WithCancel(t.Context())
					end()
				}
				start := time.Now()
				late := startTokenCall(store, lateCtx, refreshEntryPoints["AccessToken"])
				synctest.Wait()

				var res tokenResult
				switch {
				case committed && !tt.ended:
					select {
					case res = <-late:
						if res.err != nil {
							assertNoSecrets(t, "late AccessToken error", res.err.Error(), refreshSecrets...)
						}
						t.Errorf("late AccessToken answered while the committed exchange's reply was held (err=%v, returned the stored key=%v); it must wait for that exchange", res.err, res.token == "fixture-fresh-token")
						release()
					default:
						release()
						synctest.Wait()
						res = receiveNow(t, "late AccessToken after the exchange reply arrived", late, release)
					}
				default:
					select {
					case res = <-late:
					default:
						t.Error("late AccessToken was still waiting, with no virtual time passed; it must answer at once")
						release()
						res = <-late
					}
				}
				if elapsed := time.Since(start); elapsed != 0 {
					t.Errorf("late AccessToken answered after %v of virtual time, want no virtual time", elapsed)
				}
				if res.err != nil {
					assertNoSecrets(t, "late AccessToken error", res.err.Error(), refreshSecrets...)
				}

				switch {
				case tt.ended:
					if !errors.Is(res.err, context.Canceled) {
						t.Errorf("late AccessToken error = %v, want its context's error %v", res.err, context.Canceled)
					}
					if res.token != "" {
						t.Errorf("late AccessToken with an ended context returned a token (len=%d)", len(res.token))
					}
				case committed:
					if res.err != nil {
						t.Errorf("late AccessToken error = %v, want the key of the committed exchange", res.err)
					} else if res.token != "fixture-rotated-key-1" {
						t.Errorf("late AccessToken did not return the key of the committed exchange (stored key=%v, len=%d)", res.token == "fixture-fresh-token", len(res.token))
					}
				default:
					if res.err != nil {
						t.Errorf("late AccessToken error = %v, want the fresh stored key", res.err)
					} else if res.token != "fixture-fresh-token" {
						t.Errorf("late AccessToken did not return the fresh stored key (len=%d)", len(res.token))
					}
				}
				if tt.stage == noFlight {
					if got := requests.Load(); got != 0 {
						t.Errorf("issuer requests = %d, want 0 for a fresh stored key with no refresh in flight", got)
					}
					assertFileUnchanged(t, file, content)
					return
				}
				if tt.stage == discovering {
					if accepted, refused := issuer.counts(); accepted+refused != 0 {
						t.Errorf("exchanges while the shared refresh was discovering: accepted=%d refused=%d, want none", accepted, refused)
					}
					leave()
					synctest.Wait()
					if res := receiveNow(t, "explicit Refresh", refreshing, release); !errors.Is(res.err, context.Canceled) {
						t.Errorf("explicit Refresh that left got %v, want %v", res.err, context.Canceled)
					}
					assertFileUnchanged(t, file, content)
					return
				}

				// The committed exchange finishes whoever waits for it.
				release()
				synctest.Wait()
				if accepted, refused := issuer.counts(); accepted != 1 || refused != 0 {
					t.Errorf("issuer accepted %d and refused %d exchanges, want exactly 1 accepted", accepted, refused)
				}
				if got := cancelled.Load(); got != 0 {
					t.Errorf("the committed exchange saw its context end %d times, want 0", got)
				}
				if key, refreshToken := storedCredential(t, file); key != "fixture-rotated-key-1" || refreshToken != "fixture-rotated-refresh-1" {
					t.Errorf("auth file does not hold the credential of the committed exchange (key len=%d, refresh token len=%d)", len(key), len(refreshToken))
				}
			})
		})
	}
}

// endAtDoneCtx is a caller context that ends at the first evaluation of its Done method, after
// onDone has run there. Its error stays nil until then. A waiter evaluates Done when it starts to
// wait for the shared refresh, so onDone can let the refresh finish first: the waiter then finds its
// context ended and the refresh's result ready at the same time.
type endAtDoneCtx struct {
	context.Context
	once   sync.Once
	onDone func()
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (c *endAtDoneCtx) Done() <-chan struct{} {
	c.once.Do(func() {
		c.fired.Store(true)
		c.onDone()
		c.cancel()
	})
	return c.Context.Done()
}

// TestRefreshWaiterEndedBeforeResult verifies that a waiter whose context has ended by the time it
// consumes a finished refresh gets its context's error and not the refresh's key. A witness caller
// starts the refresh and an exchange is held; the waiter joins it. When the waiter starts to wait,
// its context lets the exchange reply, waits until the witness has its key (so the refresh's result
// is set), and only then ends. Both the result and the end of the context are then ready at once,
// and which one a wait picks first is not fixed, so the scenario is repeated in many rounds and
// every round must answer the context's error.
func TestRefreshWaiterEndedBeforeResult(t *testing.T) {
	const rounds = 64

	tests := map[string]struct {
		call tokenCall
	}{
		"error: AccessToken waiter whose context ended before it consumed the result gets its context's error": {
			call: refreshEntryPoints["AccessToken"],
		},
		"error: Refresh waiter whose context ended before it consumed the result gets its context's error": {
			call: refreshEntryPoints["Refresh"],
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			// A real-time escape, created outside the bubble: if the waiter evaluated its Done while
			// the refresh could not finish (for example under the store's lock), the wait in onDone
			// gives up instead of hanging.
			escape := make(chan struct{})
			timer := time.AfterFunc(time.Minute, func() { close(escape) })
			defer timer.Stop()

			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				var tookResult, exercised int
				for round := range rounds {
					file := filepath.Join(dir, fmt.Sprintf("auth-%d.json", round))
					writeFixtureFile(t, file, expiredAuthJSON())

					gate := make(chan struct{})
					release := sync.OnceFunc(func() { close(gate) })
					var exchanges atomic.Int32
					tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
						if req.URL.Path == "/.well-known/openid-configuration" {
							return discoveryResponse(req), nil
						}
						exchanges.Add(1)
						select {
						case <-gate:
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
						return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-exchanged-key","expires_in":3600}`), nil
					})
					store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

					returned := make(chan struct{})
					var witness tokenResult
					go func() {
						defer close(returned)
						witness.token, witness.err = store.AccessToken(t.Context())
					}()
					synctest.Wait()

					inner, cancel := context.WithCancel(t.Context())
					var witnessFirst atomic.Bool
					ctx := &endAtDoneCtx{Context: inner, cancel: cancel, onDone: func() {
						release()
						select {
						case <-returned:
							witnessFirst.Store(true)
						case <-escape:
							t.Errorf("round %d: the waiter's Done was evaluated while the shared refresh could not finish", round)
						}
					}}
					waiter := startTokenCall(store, ctx, tt.call)
					res := awaitTokenCall(t, fmt.Sprintf("round %d waiter", round), waiter, time.Minute, release)
					release()
					<-returned
					cancel()

					if ctx.fired.Load() && witnessFirst.Load() {
						exercised++
					}
					switch {
					case errors.Is(res.err, context.Canceled) && res.token == "":
					case res.err == nil && res.token == "fixture-exchanged-key":
						tookResult++
					default:
						if res.err != nil {
							assertNoSecrets(t, "waiter error", res.err.Error(), refreshSecrets...)
						}
						t.Errorf("round %d: waiter got err=%v and a token of len %d, want its context's error %v", round, res.err, len(res.token), context.Canceled)
					}
					if witness.err != nil || witness.token != "fixture-exchanged-key" {
						t.Errorf("round %d: witness got err=%v and a token of len %d, want the exchanged key", round, witness.err, len(witness.token))
					}
					if got := exchanges.Load(); got != 1 {
						t.Errorf("round %d: exchanges = %d, want 1 shared by the witness and the waiter", round, got)
					}
				}
				if tookResult != 0 {
					t.Errorf("in %d of %d rounds the waiter whose context had ended took the refresh's key instead of its context's error", tookResult, rounds)
				}
				if exercised != rounds {
					t.Errorf("only %d of %d rounds reached the wait with the result ready and the context ended", exercised, rounds)
				}
			})
		})
	}
}

// TestRefreshWaiterLeavingKeepsOthers verifies that a waiter leaving the shared refresh while
// another remains fails nobody: the exchange keeps its live context, the remaining caller gets the
// exchanged key and exactly one exchange runs.
func TestRefreshWaiterLeavingKeepsOthers(t *testing.T) {
	tests := map[string]struct {
		first, second tokenCall
	}{
		"success: AccessToken caller leaving keeps a waiting AccessToken caller": {first: refreshEntryPoints["AccessToken"], second: refreshEntryPoints["AccessToken"]},
		"success: Refresh caller leaving keeps a waiting Refresh caller":         {first: refreshEntryPoints["Refresh"], second: refreshEntryPoints["Refresh"]},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "auth.json")
				writeFixtureFile(t, file, expiredAuthJSON())

				gate := make(chan struct{})
				release := sync.OnceFunc(func() { close(gate) })
				defer release()

				var exchanges, cancelled atomic.Int32
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path == "/.well-known/openid-configuration" {
						return jsonHTTPResponse(req, http.StatusOK, `{"token_endpoint":"`+refreshFixtureIssuer+`/discovered/token"}`), nil
					}
					exchanges.Add(1)
					select {
					case <-gate:
						return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-fresh-token","expires_in":3600}`), nil
					case <-req.Context().Done():
						cancelled.Add(1)
						return nil, req.Context().Err()
					}
				})
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

				leaveCtx, leave := context.WithCancel(t.Context())
				leaving := startTokenCall(store, leaveCtx, tt.first)
				synctest.Wait()
				staying := startTokenCall(store, t.Context(), tt.second)
				synctest.Wait()

				leave()
				left := awaitTokenCall(t, "departing caller", leaving, time.Minute, release)
				if !errors.Is(left.err, context.Canceled) {
					t.Errorf("departing caller error = %v, want %v", left.err, context.Canceled)
				}
				synctest.Wait()
				if got := cancelled.Load(); got != 0 {
					t.Errorf("exchange context ended %d times while a waiter remained, want 0", got)
				}

				release()
				stayed := awaitTokenCall(t, "remaining caller", staying, time.Minute, release)
				if stayed.err != nil {
					assertNoSecrets(t, "remaining caller error", stayed.err.Error(), refreshSecrets...)
					t.Fatalf("remaining caller error = %v, want the exchanged key", stayed.err)
				}
				if stayed.token != "fixture-fresh-token" {
					t.Errorf("remaining caller returned an unexpected token (len=%d)", len(stayed.token))
				}
				if got := exchanges.Load(); got != 1 {
					t.Errorf("exchanges = %d, want 1", got)
				}
			})
		})
	}
}

// TestRefreshStalledIssuerDeadline verifies that each discovery and exchange request has its own
// 30 second deadline, and that the deadline covers the reply body as well as its headers. An issuer
// that stalls one request fails the refresh with the fixed refresh failure exactly 30 seconds of
// virtual time after that request started; a stalled discovery never leads to an exchange, even
// after more time passes; the auth file stays as it was. An issuer that answers each request just
// inside its own deadline refreshes normally, which a single budget for the whole refresh would not
// allow.
func TestRefreshStalledIssuerDeadline(t *testing.T) {
	const (
		requestDeadline = 30 * time.Second
		discoveryPath   = "/.well-known/openid-configuration"
		exchangePath    = "/discovered/token"
	)

	tests := map[string]struct {
		call tokenCall
		// stallPath is the request the issuer never answers; empty answers every request.
		stallPath string
		// stallBody answers the stalled request's headers and never sends its body.
		stallBody bool
		// delay is how long the issuer takes to answer each request it answers.
		delay         time.Duration
		wantElapsed   time.Duration
		wantExchanges int32
		wantToken     string
	}{
		"error: AccessToken with discovery stalled before its headers fails at the request deadline": {
			call:        refreshEntryPoints["AccessToken"],
			stallPath:   discoveryPath,
			wantElapsed: requestDeadline,
		},
		"error: Refresh with discovery stalled before its headers fails at the request deadline": {
			call:        refreshEntryPoints["Refresh"],
			stallPath:   discoveryPath,
			wantElapsed: requestDeadline,
		},
		"error: AccessToken with the discovery body stalled fails at the request deadline": {
			call:        refreshEntryPoints["AccessToken"],
			stallPath:   discoveryPath,
			stallBody:   true,
			wantElapsed: requestDeadline,
		},
		"error: Refresh with the discovery body stalled fails at the request deadline": {
			call:        refreshEntryPoints["Refresh"],
			stallPath:   discoveryPath,
			stallBody:   true,
			wantElapsed: requestDeadline,
		},
		"error: AccessToken with the exchange stalled before its headers fails at the request deadline": {
			call:          refreshEntryPoints["AccessToken"],
			stallPath:     exchangePath,
			wantElapsed:   requestDeadline,
			wantExchanges: 1,
		},
		"error: Refresh with the exchange stalled before its headers fails at the request deadline": {
			call:          refreshEntryPoints["Refresh"],
			stallPath:     exchangePath,
			wantElapsed:   requestDeadline,
			wantExchanges: 1,
		},
		"error: AccessToken with the exchange body stalled fails at the request deadline": {
			call:          refreshEntryPoints["AccessToken"],
			stallPath:     exchangePath,
			stallBody:     true,
			wantElapsed:   requestDeadline,
			wantExchanges: 1,
		},
		"error: Refresh with the exchange body stalled fails at the request deadline": {
			call:          refreshEntryPoints["Refresh"],
			stallPath:     exchangePath,
			stallBody:     true,
			wantElapsed:   requestDeadline,
			wantExchanges: 1,
		},
		"success: AccessToken with each request answered just inside its own deadline refreshes": {
			call:          refreshEntryPoints["AccessToken"],
			delay:         requestDeadline - time.Second,
			wantElapsed:   2 * (requestDeadline - time.Second),
			wantExchanges: 1,
			wantToken:     "fixture-exchanged-key",
		},
		"success: Refresh with each request answered just inside its own deadline refreshes": {
			call:          refreshEntryPoints["Refresh"],
			delay:         requestDeadline - time.Second,
			wantElapsed:   2 * (requestDeadline - time.Second),
			wantExchanges: 1,
			wantToken:     "fixture-exchanged-key",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "auth.json")
				content := expiredAuthJSON()
				writeFixtureFile(t, file, content)

				teardown := make(chan struct{})
				release := sync.OnceFunc(func() { close(teardown) })
				defer release()

				type seenRequest struct {
					path        string
					hasDeadline bool
					remaining   time.Duration
				}
				var (
					mu        sync.Mutex
					seen      []seenRequest
					exchanges atomic.Int32
					cancelled atomic.Int32
				)
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					deadline, ok := req.Context().Deadline()
					mu.Lock()
					seen = append(seen, seenRequest{path: req.URL.Path, hasDeadline: ok, remaining: time.Until(deadline)})
					mu.Unlock()
					if req.Method == http.MethodPost {
						exchanges.Add(1)
					}
					stalled := req.URL.Path == tt.stallPath
					switch {
					case stalled && !tt.stallBody:
						select {
						case <-req.Context().Done():
							return nil, req.Context().Err()
						case <-teardown:
							return nil, errors.New("fixture issuer released at teardown")
						}
					case !stalled && tt.delay > 0:
						select {
						case <-time.After(tt.delay):
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					}
					resp := discoveryResponse(req)
					if req.Method == http.MethodPost {
						resp = jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-exchanged-key","expires_in":3600}`)
					}
					if stalled {
						resp.Body = stalledBody(req, teardown, &cancelled)
					}
					return resp, nil
				})
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

				start := time.Now()
				done := startTokenCall(store, t.Context(), tt.call)
				res := awaitTokenCall(t, "refresh against a stalling issuer", done, 10*time.Minute, release)
				if elapsed := time.Since(start); elapsed != tt.wantElapsed {
					t.Errorf("refresh answered after %v of virtual time, want exactly %v", elapsed, tt.wantElapsed)
				}

				mu.Lock()
				for _, r := range seen {
					switch {
					case !r.hasDeadline:
						t.Errorf("request to %s carried no deadline", r.path)
					case r.remaining != requestDeadline:
						t.Errorf("request to %s started with %v left before its deadline, want %v", r.path, r.remaining, requestDeadline)
					}
				}
				mu.Unlock()

				// Let more time pass: a refresh that failed at discovery sends no late exchange.
				time.Sleep(time.Minute)
				synctest.Wait()
				if got := exchanges.Load(); got != tt.wantExchanges {
					t.Errorf("exchanges = %d, want %d", got, tt.wantExchanges)
				}

				if tt.wantToken != "" {
					if res.err != nil {
						assertNoSecrets(t, "refresh error", res.err.Error(), refreshSecrets...)
						t.Fatalf("refresh error = %v, want the exchanged key", res.err)
					}
					if res.token != tt.wantToken {
						t.Errorf("refresh returned an unexpected token (len=%d)", len(res.token))
					}
					if key, _ := storedCredential(t, file); key != tt.wantToken {
						t.Errorf("auth file does not hold the exchanged key (len=%d)", len(key))
					}
					return
				}
				if res.token != "" {
					t.Errorf("returned a token from a stalling issuer (len=%d)", len(res.token))
				}
				assertExactErr(t, "stalling issuer", res.err, grok.ErrAuthRefreshFailed)
				assertFileUnchanged(t, file, content)
			})
		})
	}
}

// TestExplicitRefreshAfterFlightWithoutExchange verifies that an explicit Refresh that arrives while
// a shared refresh is running, which then finds the stored key fresh and exchanges nothing, runs one
// exchange of its own afterwards and never returns a key no exchange produced. Its exchange presents
// the refresh token the file holds by then; the issuer honours no other. The AccessToken caller that
// started the shared refresh gets exactly the key stored by the other client, and it gets it while
// every exchange the issuer has seen is still held, so that key came from the file and not from an
// exchange the explicit Refresh could share.
//
// The AccessToken caller finds the stored key expired. At the first clock read, which follows the
// caller's own read of the file, another client of the issuer rotates the refresh token and stores a
// fresh key with it. Every later clock read waits at a barrier, which holds the shared refresh open
// at its expiry check. Before the barrier is lifted, the test checks that Refresh is still waiting
// and that no request at all has reached the issuer: Refresh demonstrably joined a refresh in flight
// rather than starting its own, whose discovery would show.
func TestExplicitRefreshAfterFlightWithoutExchange(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	synctest.Test(t, func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "auth.json")
		writeFixtureFile(t, file, expiredAuthJSON())
		external := fmt.Sprintf(`{"https://auth.x.ai::fixture-client":{"key":"fixture-external-key","refresh_token":"fixture-external-refresh","expires_at":%d,"oidc_client_id":"fixture-client","oidc_issuer":"%s"}}`, refreshFixtureNow.Add(time.Hour).UnixMilli(), refreshFixtureIssuer)

		// Every exchange reply is held until replies is closed; the issuer sees the request on receipt.
		replies := make(chan struct{})
		releaseReplies := sync.OnceFunc(func() { close(replies) })
		defer releaseReplies()

		issuer := newRotatingIssuer("fixture-refresh-token")
		var requests atomic.Int32
		tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests.Add(1)
			if req.URL.Path == "/.well-known/openid-configuration" {
				return discoveryResponse(req), nil
			}
			status, body := issuer.exchange(req)
			select {
			case <-replies:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return jsonHTTPResponse(req, status, body), nil
		})

		barrier := make(chan struct{})
		lift := sync.OnceFunc(func() { close(barrier) })
		defer lift()
		releaseAll := func() { lift(); releaseReplies() }
		entered := make(chan struct{})
		enter := sync.OnceFunc(func() { close(entered) })
		hookErr := make(chan error, 1)
		var rotated atomic.Bool
		clock := func() time.Time {
			if rotated.CompareAndSwap(false, true) {
				issuer.rotate("fixture-external-refresh")
				if err := os.WriteFile(file, []byte(external), 0o600); err != nil {
					hookErr <- err
				}
				return refreshFixtureNow
			}
			enter()
			<-barrier
			return refreshFixtureNow
		}
		store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: clock})

		access := startTokenCall(store, t.Context(), refreshEntryPoints["AccessToken"])
		select {
		case <-entered:
		case res := <-access:
			t.Fatalf("AccessToken finished without a clock read after the stored key changed (err=%v, token len=%d); its shared refresh must check the expiry it re-read", res.err, len(res.token))
		case <-time.After(time.Minute):
			releaseAll()
			t.Fatal("AccessToken never read the clock again after the stored key changed")
		}
		select {
		case err := <-hookErr:
			releaseAll()
			t.Fatalf("store the externally rotated key: %v", err)
		default:
		}

		refresh := startTokenCall(store, t.Context(), refreshEntryPoints["Refresh"])
		synctest.Wait()
		var gotRefresh tokenResult
		refreshEnded := false
		select {
		case gotRefresh = <-refresh:
			refreshEnded = true
			t.Errorf("Refresh finished while the shared refresh was held open (issuer requests=%d); it must wait for the refresh in flight", requests.Load())
		default:
			if got := requests.Load(); got != 0 {
				t.Errorf("issuer requests while the shared refresh was held open = %d, want 0 (a discovery means Refresh found no shared refresh open and started its own, or AccessToken read the clock again before starting the shared refresh)", got)
			}
		}
		lift()
		synctest.Wait()

		// The shared refresh found the stored key fresh: AccessToken has its answer although every
		// exchange reply is still held.
		var gotAccess tokenResult
		select {
		case gotAccess = <-access:
		default:
			accepted, refused := issuer.counts()
			t.Errorf("AccessToken was still waiting after the barrier lifted, while the issuer had seen %d accepted and %d refused exchanges whose replies are held; it must return the externally stored key without any exchange", accepted, refused)
			releaseReplies()
			gotAccess = <-access
		}
		if gotAccess.err != nil {
			assertNoSecrets(t, "AccessToken error", gotAccess.err.Error(), refreshSecrets...)
			t.Errorf("AccessToken error = %v, want the externally stored key", gotAccess.err)
		} else if gotAccess.token != "fixture-external-key" {
			t.Errorf("AccessToken did not return exactly the externally stored key (exchanged key=%v, len=%d)", strings.HasPrefix(gotAccess.token, "fixture-rotated-key-"), len(gotAccess.token))
		}

		releaseReplies()
		if !refreshEnded {
			gotRefresh = awaitTokenCall(t, "Refresh", refresh, time.Minute, releaseAll)
		}
		if gotRefresh.err != nil {
			assertNoSecrets(t, "Refresh error", gotRefresh.err.Error(), refreshSecrets...)
			t.Fatalf("Refresh error = %v, want the key of its own exchange presenting the rotated refresh token", gotRefresh.err)
		}
		switch gotRefresh.token {
		case "fixture-rotated-key-1":
		case "fixture-external-key":
			t.Error("Refresh returned the stored key that no exchange produced")
		default:
			t.Errorf("Refresh returned an unexpected token (len=%d)", len(gotRefresh.token))
		}
		if accepted, refused := issuer.counts(); accepted != 1 || refused != 0 {
			t.Errorf("issuer accepted %d and refused %d exchanges, want 1 accepted (the one Refresh runs after the shared refresh made none) and none presenting a superseded refresh token", accepted, refused)
		}
		if key, refreshToken := storedCredential(t, file); key != "fixture-rotated-key-1" || refreshToken != "fixture-rotated-refresh-1" {
			t.Errorf("auth file does not hold the credential of Refresh's exchange (key len=%d, refresh token len=%d)", len(key), len(refreshToken))
		}
	})
}

// TestRefreshExchangesNeverOverlap verifies that two exchanges never run at once on one refresh
// token, whichever entry points are waiting, and that every explicit Refresh returns a key an
// exchange produced.
func TestRefreshExchangesNeverOverlap(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	synctest.Test(t, func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "auth.json")
		writeFixtureFile(t, file, expiredAuthJSON())

		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		defer release()

		var (
			mu        sync.Mutex
			active    int
			maxActive int
			issued    = map[string]bool{}
		)
		tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/.well-known/openid-configuration" {
				return jsonHTTPResponse(req, http.StatusOK, `{"token_endpoint":"`+refreshFixtureIssuer+`/discovered/token"}`), nil
			}
			mu.Lock()
			active++
			maxActive = max(maxActive, active)
			mu.Unlock()
			select {
			case <-gate:
			case <-req.Context().Done():
			}
			mu.Lock()
			active--
			n := len(issued) + 1
			key := fmt.Sprintf("fixture-issued-key-%d", n)
			issued[key] = true
			mu.Unlock()
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			return jsonHTTPResponse(req, http.StatusOK, fmt.Sprintf(`{"access_token":%q,"refresh_token":"fixture-issued-refresh-%d","expires_in":3600}`, key, n)), nil
		})
		store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

		first := startTokenCall(store, t.Context(), refreshEntryPoints["Refresh"])
		synctest.Wait()
		second := startTokenCall(store, t.Context(), refreshEntryPoints["Refresh"])
		access := startTokenCall(store, t.Context(), refreshEntryPoints["AccessToken"])
		synctest.Wait()
		release()

		results := map[string]tokenResult{
			"first Refresh":  awaitTokenCall(t, "first Refresh", first, time.Minute, release),
			"second Refresh": awaitTokenCall(t, "second Refresh", second, time.Minute, release),
			"AccessToken":    awaitTokenCall(t, "AccessToken", access, time.Minute, release),
		}
		mu.Lock()
		defer mu.Unlock()
		if maxActive != 1 {
			t.Errorf("most exchanges running at once = %d, want 1", maxActive)
		}
		for label, res := range results {
			if res.err != nil {
				assertNoSecrets(t, label+" error", res.err.Error(), refreshSecrets...)
				t.Errorf("%s error = %v, want an exchanged key", label, res.err)
				continue
			}
			if !issued[res.token] {
				t.Errorf("%s returned a key no exchange produced (len=%d)", label, len(res.token))
			}
		}
	})
}

// countingBody counts the bytes a reader of the issuer's response body consumed.
type countingBody struct {
	r io.Reader
	n *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { return nil }

// paddedJSON returns obj followed by spaces, exactly size bytes long: one complete JSON document, so
// only the size bound can reject it.
func paddedJSON(t *testing.T, obj string, size int) string {
	t.Helper()
	if size < len(obj) {
		t.Fatalf("padded JSON of %d bytes cannot hold its %d byte object", size, len(obj))
	}
	return obj + strings.Repeat(" ", size-len(obj))
}

// breakingBody serves content, and on the read that takes the bytes served past limit it reports a
// read error that is not io.EOF together with those bytes, as a connection reset mid-body may.
type breakingBody struct {
	content string
	off     int
	limit   int
	n       *atomic.Int64
}

func (b *breakingBody) Read(p []byte) (int, error) {
	if b.off >= len(b.content) {
		return 0, io.EOF
	}
	n := copy(p, b.content[b.off:])
	b.off += n
	b.n.Add(int64(n))
	if b.off > b.limit {
		return n, errors.New("fixture connection reset")
	}
	return n, nil
}

func (b *breakingBody) Close() error { return nil }

// TestRefreshBodyBound verifies that discovery and exchange response bodies are read through a
// 1 MiB bound. A complete body of exactly 1 MiB is used; one byte more fails the refresh, as does a
// body past the bound whose last read also reported an error, and no reader consumes much more than
// the bound of a larger body. A discovery body past the bound fails the refresh without any
// exchange: neither the endpoint it names nor the conventional fallback is contacted.
func TestRefreshBodyBound(t *testing.T) {
	const (
		mib = 1 << 20
		// readSlack allows a bounded reader to look one buffer past the bound to detect a larger body.
		readSlack = 64 << 10
	)
	discoveryObject := `{"token_endpoint":"` + refreshFixtureIssuer + `/discovered/token"}`
	exchangeObject := `{"access_token":"fixture-bounded-key","expires_in":3600}`

	tests := map[string]struct {
		// discoverySize and exchangeSize pad the reply to that many bytes; zero sends the bare object.
		discoverySize, exchangeSize int
		// discoveryBreak and exchangeBreak report a read error with the read that passes one MiB.
		discoveryBreak, exchangeBreak bool
		// wantErr nil wants the bounded key from the discovered token endpoint.
		wantErr error
	}{
		"success: discovery body of exactly one MiB names the token endpoint": {
			discoverySize: mib,
		},
		"error: discovery body one byte over one MiB fails the refresh without an exchange": {
			discoverySize: mib + 1,
			wantErr:       grok.ErrAuthRefreshFailed,
		},
		"error: discovery body of eight MiB fails the refresh after a bounded read": {
			discoverySize: 8 * mib,
			wantErr:       grok.ErrAuthRefreshFailed,
		},
		"error: discovery body past one MiB read together with an error fails the refresh without an exchange": {
			discoverySize:  2 * mib,
			discoveryBreak: true,
			wantErr:        grok.ErrAuthRefreshFailed,
		},
		"success: exchange body of exactly one MiB is accepted": {
			exchangeSize: mib,
		},
		"error: exchange body one byte over one MiB fails the refresh": {
			exchangeSize: mib + 1,
			wantErr:      grok.ErrAuthRefreshFailed,
		},
		"error: exchange body of eight MiB fails the refresh after a bounded read": {
			exchangeSize: 8 * mib,
			wantErr:      grok.ErrAuthRefreshFailed,
		},
		"error: exchange body past one MiB read together with an error fails the refresh": {
			exchangeSize:  2 * mib,
			exchangeBreak: true,
			wantErr:       grok.ErrAuthRefreshFailed,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			dir := t.TempDir()
			file := filepath.Join(dir, "auth.json")
			content := expiredAuthJSON()
			writeFixtureFile(t, file, content)

			reply := func(obj string, size int, broken bool, n *atomic.Int64) io.ReadCloser {
				body := obj
				if size > 0 {
					body = paddedJSON(t, obj, size)
				}
				if broken {
					return &breakingBody{content: body, limit: mib, n: n}
				}
				return &countingBody{r: strings.NewReader(body), n: n}
			}

			var discoveryRead, exchangeRead atomic.Int64
			var discoveredHits, otherHits atomic.Int32
			tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				resp := jsonHTTPResponse(req, http.StatusOK, "")
				switch {
				case req.Method == http.MethodGet && req.URL.Path == "/.well-known/openid-configuration":
					resp.Body = reply(discoveryObject, tt.discoverySize, tt.discoveryBreak, &discoveryRead)
				case req.Method == http.MethodPost && req.URL.Path == "/discovered/token":
					discoveredHits.Add(1)
					resp.Body = reply(exchangeObject, tt.exchangeSize, tt.exchangeBreak, &exchangeRead)
				default:
					// The conventional fallback endpoint, or anything else: never wanted here.
					otherHits.Add(1)
					resp.Body = io.NopCloser(strings.NewReader(`{"access_token":"fixture-fallback-key","expires_in":3600}`))
				}
				return resp, nil
			})
			store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

			token, err := store.Refresh(t.Context())

			if got := discoveryRead.Load(); got > mib+readSlack {
				t.Errorf("discovery body bytes read = %d, want at most %d (1 MiB bound plus one read buffer)", got, mib+readSlack)
			}
			if got := exchangeRead.Load(); got > mib+readSlack {
				t.Errorf("exchange body bytes read = %d, want at most %d (1 MiB bound plus one read buffer)", got, mib+readSlack)
			}
			if got := otherHits.Load(); got != 0 {
				t.Errorf("requests to anything but discovery and the discovered token endpoint = %d, want 0 (no fallback exchange)", got)
			}

			if tt.wantErr == nil {
				if err != nil {
					assertNoSecrets(t, "bounded refresh error", err.Error(), refreshSecrets...)
					t.Fatalf("Refresh() error = %v, want the key of bodies within the bound", err)
				}
				if token != "fixture-bounded-key" {
					t.Errorf("Refresh() returned an unexpected token (len=%d)", len(token))
				}
				if got := discoveredHits.Load(); got != 1 {
					t.Errorf("exchanges at the discovered token endpoint = %d, want 1", got)
				}
				return
			}
			assertExactErr(t, "oversized body", err, tt.wantErr)
			if token != "" {
				t.Errorf("returned a token from an oversized body (len=%d)", len(token))
			}
			wantExchanges := int32(1)
			if tt.discoverySize > mib {
				wantExchanges = 0
			}
			if got := discoveredHits.Load(); got != wantExchanges {
				t.Errorf("exchanges at the discovered token endpoint = %d, want %d", got, wantExchanges)
			}
			assertFileUnchanged(t, file, content)
		})
	}
}

// TestShadowRefreshRefusesBeforeReading verifies that in shadow mode Refresh answers
// ErrAuthCannotRefresh before it reads the auth file or contacts anything, so a missing or
// unusable file does not change the answer.
func TestShadowRefreshRefusesBeforeReading(t *testing.T) {
	tests := map[string]struct {
		content *string
	}{
		"error: shadow Refresh with a missing auth file cannot refresh":       {},
		"error: shadow Refresh with a malformed auth file cannot refresh":     {content: new(`{"https://auth.x.ai::fixture-client":`)},
		"error: shadow Refresh with an auth file without x.ai cannot refresh": {content: new(`{"other":{"key":"fixture-expired-token"}}`)},
		"error: shadow Refresh with an expired auth file cannot refresh":      {content: new(expiredAuthJSON())},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			dir := t.TempDir()
			file := filepath.Join(dir, "auth.json")
			if tt.content != nil {
				writeFixtureFile(t, file, *tt.content)
			}
			store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: denyNetworkTransport(t), Now: func() time.Time { return refreshFixtureNow }, Shadow: true})

			token, err := store.Refresh(t.Context())
			if token != "" {
				t.Errorf("shadow Refresh returned a token (len=%d)", len(token))
			}
			assertExactErr(t, "shadow Refresh", err, grok.ErrAuthCannotRefresh)

			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatalf("read fixture directory: %v", readErr)
			}
			wantEntries := 0
			if tt.content != nil {
				wantEntries = 1
				assertFileUnchanged(t, file, *tt.content)
			}
			if len(entries) != wantEntries {
				t.Errorf("fixture directory entries = %d, want %d", len(entries), wantEntries)
			}
		})
	}
}

// TestAccessTokenSubMillisecondSkew verifies that the refresh skew is compared as a duration: a
// positive skew shorter than a millisecond, or with a fraction of one, still refreshes a key that
// expires inside it, and a key that expires after it is used as stored. The clock sits on a whole
// millisecond and the expiry carries the fraction.
func TestAccessTokenSubMillisecondSkew(t *testing.T) {
	tests := map[string]struct {
		skew        time.Duration
		expiresAt   string
		wantRefresh bool
	}{
		"success: key expiring inside a half millisecond skew is refreshed": {
			skew:        500 * time.Microsecond,
			expiresAt:   "1700000000000.3",
			wantRefresh: true,
		},
		"success: key expiring inside a one and a half millisecond skew is refreshed": {
			skew:        1500 * time.Microsecond,
			expiresAt:   "1700000000001.3",
			wantRefresh: true,
		},
		"success: key expiring after a half millisecond skew is used as stored": {
			skew:      500 * time.Microsecond,
			expiresAt: "1700000000001",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			dir := t.TempDir()
			file := filepath.Join(dir, "auth.json")
			content := `{"https://auth.x.ai::fixture-client":{"key":"fixture-expired-token","refresh_token":"fixture-refresh-token","expires_at":` + tt.expiresAt + `,"oidc_client_id":"fixture-client","oidc_issuer":"` + refreshFixtureIssuer + `"}}`
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
					t.Errorf("AccessToken() did not return the stored key (len=%d)", len(token))
				}
				assertAuthFixtureUntouched(t, dir, file, before, content)
				return
			}
			if token != "fixture-fresh-token" {
				t.Errorf("AccessToken() returned the stored key of an expiry inside the %v skew instead of refreshing (len=%d)", tt.skew, len(token))
			}
			if got := exchanges.Load(); got != 1 {
				t.Errorf("exchanges = %d, want 1", got)
			}
		})
	}
}
