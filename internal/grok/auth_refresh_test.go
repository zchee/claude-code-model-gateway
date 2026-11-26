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
	"fixture-trap-key",
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

// TestRefreshWithEndedContext verifies that a caller whose context has already ended starts and
// joins no refresh: it gets its context's error, no request reaches the issuer and the auth file
// stays as it was.
func TestRefreshWithEndedContext(t *testing.T) {
	tests := map[string]struct {
		call tokenCall
	}{
		"error: AccessToken with an ended context starts no refresh": {call: refreshEntryPoints["AccessToken"]},
		"error: Refresh with an ended context starts no refresh":     {call: refreshEntryPoints["Refresh"]},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })
			synctest.Test(t, func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "auth.json")
				content := expiredAuthJSON()
				writeFixtureFile(t, file, content)

				var requests atomic.Int32
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					requests.Add(1)
					if req.URL.Path == "/.well-known/openid-configuration" {
						return jsonHTTPResponse(req, http.StatusOK, `{"token_endpoint":"`+refreshFixtureIssuer+`/discovered/token"}`), nil
					}
					return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-fresh-token","expires_in":3600}`), nil
				})
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				token, err := tt.call(store, ctx)
				// Anything the call left running finishes or blocks before the assertions.
				synctest.Wait()

				if err != nil {
					assertNoSecrets(t, "ended-context error", err.Error(), refreshSecrets...)
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("error = %v, want the context's error %v", err, context.Canceled)
				}
				if token != "" {
					t.Errorf("returned a token with an ended context (len=%d)", len(token))
				}
				if got := requests.Load(); got != 0 {
					t.Errorf("issuer requests = %d, want 0 for a caller whose context had already ended", got)
				}
				assertFileUnchanged(t, file, content)
			})
		})
	}
}

// TestRefreshLastWaiterLeaving verifies that the shared refresh's own context ends when its last
// waiter has left: the stalled issuer request observes the cancellation, no exchange is sent on a
// live context and the auth file stays as it was.
func TestRefreshLastWaiterLeaving(t *testing.T) {
	tests := map[string]struct {
		call tokenCall
	}{
		"success: the sole AccessToken caller leaving ends the shared refresh": {call: refreshEntryPoints["AccessToken"]},
		"success: the sole Refresh caller leaving ends the shared refresh":     {call: refreshEntryPoints["Refresh"]},
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

				var stalls, cancelled, liveExchanges atomic.Int32
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Context().Err() != nil {
						return nil, req.Context().Err()
					}
					if req.Method == http.MethodPost {
						liveExchanges.Add(1)
					}
					stalls.Add(1)
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
				if got := stalls.Load(); got != 1 {
					t.Fatalf("stalled issuer requests before the caller left = %d, want 1", got)
				}

				cancel()
				res := awaitTokenCall(t, "departing caller", done, time.Minute, release)
				if res.err != nil {
					assertNoSecrets(t, "departing caller error", res.err.Error(), refreshSecrets...)
				}
				if !errors.Is(res.err, context.Canceled) {
					t.Errorf("departing caller error = %v, want %v", res.err, context.Canceled)
				}
				if res.token != "" {
					t.Errorf("departing caller returned a token (len=%d)", len(res.token))
				}

				// With no waiter left, the shared refresh must end its own requests.
				synctest.Wait()
				if got := cancelled.Load(); got < 1 {
					t.Errorf("stalled issuer requests that saw their context end after the last waiter left = %d, want at least 1", got)
				}
				if got := liveExchanges.Load(); got != 0 {
					t.Errorf("exchanges sent on a live context after the last waiter left = %d, want 0", got)
				}
				assertFileUnchanged(t, file, content)

				release()
				synctest.Wait()
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

// TestRefreshStalledIssuerDeadline verifies that every discovery and exchange request has a finite
// deadline: an issuer that accepts the request and never answers fails the refresh with the fixed
// refresh failure within 60 seconds of virtual time (two requests of at most 30 seconds each), and
// the auth file stays as it was.
func TestRefreshStalledIssuerDeadline(t *testing.T) {
	tests := map[string]struct {
		call tokenCall
	}{
		"error: AccessToken against a stalled issuer fails within the request deadlines": {call: refreshEntryPoints["AccessToken"]},
		"error: Refresh against a stalled issuer fails within the request deadlines":     {call: refreshEntryPoints["Refresh"]},
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

				var requests, deadlines atomic.Int32
				tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					requests.Add(1)
					if _, ok := req.Context().Deadline(); ok {
						deadlines.Add(1)
					}
					select {
					case <-req.Context().Done():
						return nil, req.Context().Err()
					case <-teardown:
						return nil, errors.New("fixture issuer released at teardown")
					}
				})
				store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

				start := time.Now()
				done := startTokenCall(store, t.Context(), tt.call)
				res := awaitTokenCall(t, "refresh against a stalled issuer", done, 10*time.Minute, release)
				elapsed := time.Since(start)

				if elapsed > time.Minute {
					t.Errorf("refresh against a stalled issuer took %v of virtual time, want at most 1m0s", elapsed)
				}
				if res.token != "" {
					t.Errorf("returned a token from a stalled issuer (len=%d)", len(res.token))
				}
				assertExactErr(t, "stalled issuer", res.err, grok.ErrAuthRefreshFailed)
				if got := requests.Load(); got < 1 {
					t.Errorf("issuer requests = %d, want at least 1", got)
				}
				t.Logf("issuer requests=%d, with a deadline=%d, virtual elapsed=%v", requests.Load(), deadlines.Load(), elapsed)
				assertFileUnchanged(t, file, content)
				release()
				synctest.Wait()
			})
		})
	}
}

// TestExplicitRefreshAfterFlightWithoutExchange verifies that an explicit Refresh joining a shared
// refresh that made no exchange runs one exchange of its own afterwards and never returns a key no
// exchange produced.
//
// The AccessToken caller finds the stored key expiring; before its shared refresh re-reads the
// file, another process stores a fresh key, so that refresh returns the fresh key without an
// exchange. The clock hook drives the order: its first call (the caller's expiry check) stores the
// fresh key, and its second call (the expiry check inside the shared refresh, after the re-read)
// holds the shared refresh open until Refresh has joined it.
func TestExplicitRefreshAfterFlightWithoutExchange(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	synctest.Test(t, func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "auth.json")
		writeFixtureFile(t, file, expiredAuthJSON())
		external := fmt.Sprintf(`{"https://auth.x.ai::fixture-client":{"key":"fixture-external-key","refresh_token":"fixture-external-refresh","expires_at":%d,"oidc_client_id":"fixture-client","oidc_issuer":"%s"}}`, refreshFixtureNow.Add(time.Hour).UnixMilli(), refreshFixtureIssuer)

		var exchanges atomic.Int32
		tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/.well-known/openid-configuration" {
				return jsonHTTPResponse(req, http.StatusOK, `{"token_endpoint":"`+refreshFixtureIssuer+`/discovered/token"}`), nil
			}
			raw, _ := io.ReadAll(req.Body)
			form, err := url.ParseQuery(string(raw))
			if err != nil || form.Get("grant_type") != "refresh_token" || form.Get("client_id") != "fixture-client" || form.Get("refresh_token") == "" {
				return jsonHTTPResponse(req, http.StatusBadRequest, `{"error":"invalid_grant"}`), nil
			}
			exchanges.Add(1)
			return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-exchanged-key","refresh_token":"fixture-exchanged-refresh","expires_in":3600}`), nil
		})

		gate := make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		entered := make(chan struct{})
		hookErr := make(chan error, 1)
		var clockCalls atomic.Int32
		clock := func() time.Time {
			switch clockCalls.Add(1) {
			case 1:
				if err := os.WriteFile(file, []byte(external), 0o600); err != nil {
					hookErr <- err
				}
			case 2:
				close(entered)
				<-gate
			}
			return refreshFixtureNow
		}
		store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: clock})

		access := startTokenCall(store, t.Context(), refreshEntryPoints["AccessToken"])
		select {
		case <-entered:
		case res := <-access:
			t.Fatalf("AccessToken finished before its shared refresh checked the clock (err=%v, token len=%d); this test needs the expiry check inside the shared refresh", res.err, len(res.token))
		case <-time.After(time.Minute):
			t.Fatal("AccessToken never reached the expiry check of its shared refresh")
		}
		select {
		case err := <-hookErr:
			t.Fatalf("store the externally rotated key: %v", err)
		default:
		}

		refresh := startTokenCall(store, t.Context(), refreshEntryPoints["Refresh"])
		synctest.Wait()
		release()

		gotAccess := awaitTokenCall(t, "AccessToken", access, time.Minute, release)
		gotRefresh := awaitTokenCall(t, "Refresh", refresh, time.Minute, release)

		if gotAccess.err != nil {
			assertNoSecrets(t, "AccessToken error", gotAccess.err.Error(), refreshSecrets...)
			t.Errorf("AccessToken error = %v, want a valid key", gotAccess.err)
		} else if gotAccess.token != "fixture-external-key" && gotAccess.token != "fixture-exchanged-key" {
			t.Errorf("AccessToken returned neither the stored fresh key nor the exchanged key (len=%d)", len(gotAccess.token))
		}
		if gotRefresh.err != nil {
			assertNoSecrets(t, "Refresh error", gotRefresh.err.Error(), refreshSecrets...)
			t.Fatalf("Refresh error = %v, want the key of its own exchange", gotRefresh.err)
		}
		switch gotRefresh.token {
		case "fixture-exchanged-key":
		case "fixture-external-key":
			t.Error("Refresh returned the stored key that no exchange produced")
		default:
			t.Errorf("Refresh returned an unexpected token (len=%d)", len(gotRefresh.token))
		}
		if got := exchanges.Load(); got != 1 {
			t.Errorf("exchanges = %d, want 1 (the one Refresh runs after the shared refresh made none)", got)
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

// paddedJSON returns head, a pad string member and the closing brace, exactly size bytes long.
func paddedJSON(t *testing.T, head string, size int) string {
	t.Helper()
	open := head + `,"pad":"`
	const closing = `"}`
	n := size - len(open) - len(closing)
	if n < 0 {
		t.Fatalf("padded JSON of %d bytes cannot hold its head", size)
	}
	return open + strings.Repeat("x", n) + closing
}

// TestRefreshBodyBound verifies that discovery and exchange response bodies are read through a
// 1 MiB bound: a body of exactly 1 MiB is accepted, a larger exchange body fails the refresh
// without rewriting the auth file, and no reader consumes much more than the bound of a larger
// body.
func TestRefreshBodyBound(t *testing.T) {
	const (
		mib = 1 << 20
		// readSlack allows a bounded reader to look one buffer past the bound to detect a larger body.
		readSlack = 64 << 10
	)

	tests := map[string]struct {
		discoverySize int
		exchangeSize  int
		// eitherOutcome accepts both the fixed refresh failure and a refresh that fell back to the
		// conventional token endpoint, for an oversized discovery body.
		eitherOutcome bool
		wantToken     string
		wantErr       error
	}{
		"success: exchange body of exactly one MiB is accepted": {
			exchangeSize: mib,
			wantToken:    "fixture-bounded-key",
		},
		"error: exchange body one byte over one MiB fails the refresh": {
			exchangeSize: mib + 1,
			wantErr:      grok.ErrAuthRefreshFailed,
		},
		"error: exchange body of eight MiB fails the refresh after a bounded read": {
			exchangeSize: 8 * mib,
			wantErr:      grok.ErrAuthRefreshFailed,
		},
		"error: discovery body of eight MiB is read through the bound and its endpoint is never used": {
			discoverySize: 8 * mib,
			eitherOutcome: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			dir := t.TempDir()
			file := filepath.Join(dir, "auth.json")
			content := expiredAuthJSON()
			writeFixtureFile(t, file, content)

			var discoveryRead, exchangeRead atomic.Int64
			var trapHits, fallbackHits atomic.Int32
			tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/.well-known/openid-configuration":
					body := `{"token_endpoint":"` + refreshFixtureIssuer + `/discovered/token"}`
					if tt.discoverySize > 0 {
						body = paddedJSON(t, `{"token_endpoint":"`+refreshFixtureIssuer+`/trap/token"`, tt.discoverySize)
					}
					resp := jsonHTTPResponse(req, http.StatusOK, "")
					resp.Body = &countingBody{r: strings.NewReader(body), n: &discoveryRead}
					return resp, nil
				case "/discovered/token":
					body := `{"access_token":"fixture-bounded-key","expires_in":3600}`
					if tt.exchangeSize > 0 {
						body = paddedJSON(t, `{"access_token":"fixture-bounded-key","expires_in":3600`, tt.exchangeSize)
					}
					resp := jsonHTTPResponse(req, http.StatusOK, "")
					resp.Body = &countingBody{r: strings.NewReader(body), n: &exchangeRead}
					return resp, nil
				case "/oauth/token":
					fallbackHits.Add(1)
					return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-fallback-key","expires_in":3600}`), nil
				case "/trap/token":
					trapHits.Add(1)
					return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-trap-key","expires_in":3600}`), nil
				default:
					return jsonHTTPResponse(req, http.StatusNotFound, `{}`), nil
				}
			})
			store := mustNewAuthStore(t, grok.AuthOptions{File: file, Transport: tr, Now: func() time.Time { return refreshFixtureNow }})

			token, err := store.Refresh(t.Context())

			if got := discoveryRead.Load(); got > mib+readSlack {
				t.Errorf("discovery body bytes read = %d, want at most %d (1 MiB bound plus one read buffer)", got, mib+readSlack)
			}
			if got := exchangeRead.Load(); got > mib+readSlack {
				t.Errorf("exchange body bytes read = %d, want at most %d (1 MiB bound plus one read buffer)", got, mib+readSlack)
			}
			if got := trapHits.Load(); got != 0 {
				t.Errorf("token endpoint named by an oversized discovery body was contacted %d times, want 0", got)
			}

			switch {
			case tt.eitherOutcome:
				if err != nil {
					assertExactErr(t, "oversized discovery", err, grok.ErrAuthRefreshFailed)
					if token != "" {
						t.Errorf("returned a token with an error (len=%d)", len(token))
					}
					assertFileUnchanged(t, file, content)
					t.Log("oversized discovery body failed the refresh")
					return
				}
				if token != "fixture-fallback-key" || fallbackHits.Load() != 1 {
					t.Errorf("oversized discovery body neither failed the refresh nor fell back to the conventional endpoint (token len=%d, fallback hits=%d)", len(token), fallbackHits.Load())
					return
				}
				t.Log("oversized discovery body fell back to the conventional token endpoint")
			case tt.wantErr != nil:
				assertExactErr(t, "oversized exchange", err, tt.wantErr)
				if token != "" {
					t.Errorf("returned a token from an oversized exchange body (len=%d)", len(token))
				}
				assertFileUnchanged(t, file, content)
			default:
				if err != nil {
					assertNoSecrets(t, "bounded exchange error", err.Error(), refreshSecrets...)
					t.Fatalf("Refresh() error = %v, want the key of a body within the bound", err)
				}
				if token != tt.wantToken {
					t.Errorf("Refresh() returned an unexpected token (got len=%d, want len=%d)", len(token), len(tt.wantToken))
				}
			}
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
