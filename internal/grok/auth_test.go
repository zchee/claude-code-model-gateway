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
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/catalog"
	"github.com/zchee/claude-code-model-gateway/internal/grok"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonHTTPResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// redirectTransport forwards every request to srv while recording each original URL and body.
func redirectTransport(srv *httptest.Server, record func(origURL, method, contentType, body string)) http.RoundTripper {
	target, err := url.Parse(srv.URL)
	if err != nil {
		panic(err)
	}
	base := srv.Client().Transport
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		origURL := req.URL.String()
		var bodyStr string
		if req.Body != nil {
			b, err := io.ReadAll(req.Body)
			_ = req.Body.Close()
			if err != nil {
				return nil, err
			}
			bodyStr = string(b)
			req = req.Clone(req.Context())
			req.Body = io.NopCloser(strings.NewReader(bodyStr))
			req.ContentLength = int64(len(b))
		} else {
			req = req.Clone(req.Context())
		}
		if record != nil {
			record(origURL, req.Method, req.Header.Get("Content-Type"), bodyStr)
		}
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		return base.RoundTrip(req)
	})
}

func TestReadiness(t *testing.T) {
	tests := map[string]struct {
		check func(t *testing.T) func() error
		want  catalog.Readiness
	}{
		"success: auth present": {
			check: func(*testing.T) func() error {
				return func() error { return nil }
			},
			want: catalog.Readiness{
				Ready:   true,
				State:   "ready",
				Message: "Grok CLI auth is present.",
			},
		},
		"error: auth missing": {
			check: func(*testing.T) func() error {
				return func() error { return grok.ErrAuthMissing }
			},
			want: catalog.Readiness{
				Ready:   false,
				State:   "auth-missing",
				Message: "Grok CLI auth is missing. Run `grok` and log in again.",
			},
		},
		"error: auth invalid": {
			check: func(*testing.T) func() error {
				return func() error { return grok.ErrAuthInvalid }
			},
			want: catalog.Readiness{
				Ready:   false,
				State:   "auth-invalid",
				Message: "Grok CLI auth is invalid. Run `grok` and log in again.",
			},
		},
		"error: auth invalid mixed case": {
			check: func(*testing.T) func() error {
				return func() error { return errors.New("token payload INVALID") }
			},
			want: catalog.Readiness{
				Ready:   false,
				State:   "auth-invalid",
				Message: "token payload INVALID",
			},
		},
		"error: nil check reports unavailable fallback": {
			check: func(*testing.T) func() error { return nil },
			want: catalog.Readiness{
				Ready:   false,
				State:   "auth-missing",
				Message: "Grok CLI auth is unavailable. Run `grok` and log in again.",
			},
		},
		"success: CheckAuth accepts valid auth file": {
			check: func(t *testing.T) func() error {
				file := filepath.Join(t.TempDir(), "auth.json")
				writeFixtureFile(t, file, `{"other":{"key":"ignored"},"https://auth.x.ai::fixture-client":{"key":"fixture-token"}}`)
				return func() error { return grok.CheckAuth(file) }
			},
			want: catalog.Readiness{
				Ready:   true,
				State:   "ready",
				Message: "Grok CLI auth is present.",
			},
		},
		"error: CheckAuth rejects missing file": {
			check: func(t *testing.T) func() error {
				file := filepath.Join(t.TempDir(), "missing.json")
				return func() error { return grok.CheckAuth(file) }
			},
			want: catalog.Readiness{
				Ready:   false,
				State:   "auth-missing",
				Message: "Grok CLI auth is missing. Run `grok` and log in again.",
			},
		},
		"error: CheckAuth rejects empty key": {
			check: func(t *testing.T) func() error {
				file := filepath.Join(t.TempDir(), "auth.json")
				writeFixtureFile(t, file, `{"https://auth.x.ai::fixture-client":{"key":""}}`)
				return func() error { return grok.CheckAuth(file) }
			},
			want: catalog.Readiness{
				Ready:   false,
				State:   "auth-invalid",
				Message: "Grok CLI auth is invalid. Run `grok` and log in again.",
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := grok.Readiness(tt.check(t))
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Readiness() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestRefreshAuth(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)

	tests := map[string]struct {
		authJSON            string
		missingFile         bool
		shadow              bool
		discoveryStatus     int
		discoveryBody       string
		tokenStatus         int
		tokenBody           string
		wantToken           string
		wantErr             error
		wantTokenURL        string
		wantRefreshRotated  string
		wantExpiresAt       int64
		wantZeroNetworkCall bool
		wantFileUnchanged   bool
	}{
		"success: refreshes expired fixture auth": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token",
					"refresh_token": "fixture-refresh-token",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				},
				"unrelated": {"keep": true}
			}`,
			discoveryStatus:    http.StatusOK,
			discoveryBody:      `{"token_endpoint":"https://auth.x.ai/oauth/token"}`,
			tokenStatus:        http.StatusOK,
			tokenBody:          `{"access_token":"fixture-fresh-token","expires_in":3600}`,
			wantToken:          "fixture-fresh-token",
			wantTokenURL:       "https://auth.x.ai/oauth/token",
			wantRefreshRotated: "fixture-refresh-token",
			wantExpiresAt:      now.UnixMilli() + 3600*1000,
		},
		"success: rotates refresh token and falls back when discovery fails": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token",
					"refresh_token": "fixture-old-refresh",
					"expires_at": 0,
					"oidc_client_id": "fixture-client",
					"oidc_issuer": "https://issuer.example.test/"
				}
			}`,
			discoveryStatus:    http.StatusInternalServerError,
			discoveryBody:      `{"error":"discovery unavailable"}`,
			tokenStatus:        http.StatusOK,
			tokenBody:          `{"access_token":"fixture-rotated-token","refresh_token":"fixture-new-refresh","expires_in":1200}`,
			wantToken:          "fixture-rotated-token",
			wantTokenURL:       "https://issuer.example.test/oauth/token",
			wantRefreshRotated: "fixture-new-refresh",
			wantExpiresAt:      now.UnixMilli() + 1200*1000,
		},
		"error: missing refresh token cannot refresh": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				}
			}`,
			wantErr:             grok.ErrAuthCannotRefresh,
			wantZeroNetworkCall: true,
			wantFileUnchanged:   true,
		},
		"error: missing client id cannot refresh": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token",
					"refresh_token": "fixture-refresh-token",
					"expires_at": 0
				}
			}`,
			wantErr:             grok.ErrAuthCannotRefresh,
			wantZeroNetworkCall: true,
			wantFileUnchanged:   true,
		},
		"error: non-2xx token response fails without modifying file": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token",
					"refresh_token": "fixture-secret-refresh-sentinel",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				}
			}`,
			discoveryStatus:   http.StatusOK,
			discoveryBody:     `{"token_endpoint":"https://auth.x.ai/oauth/token"}`,
			tokenStatus:       http.StatusBadGateway,
			tokenBody:         `{"error":"invalid_grant"}`,
			wantErr:           grok.ErrAuthRefreshFailed,
			wantTokenURL:      "https://auth.x.ai/oauth/token",
			wantFileUnchanged: true,
		},
		"error: missing access token in 200 response fails": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token",
					"refresh_token": "fixture-refresh-token",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				}
			}`,
			discoveryStatus:   http.StatusOK,
			discoveryBody:     `{"token_endpoint":"https://auth.x.ai/oauth/token"}`,
			tokenStatus:       http.StatusOK,
			tokenBody:         `{"access_token":""}`,
			wantErr:           grok.ErrAuthRefreshFailed,
			wantTokenURL:      "https://auth.x.ai/oauth/token",
			wantFileUnchanged: true,
		},
		"error: missing auth file fails": {
			missingFile:         true,
			wantErr:             grok.ErrAuthMissing,
			wantZeroNetworkCall: true,
		},
		"error: invalid auth file without x.ai entry fails": {
			authJSON:            `{"other":{"key":"x"}}`,
			wantErr:             grok.ErrAuthInvalid,
			wantZeroNetworkCall: true,
			wantFileUnchanged:   true,
		},
		"error: shadow mode disables refresh without network or disk writes": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token",
					"refresh_token": "fixture-refresh-token",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				}
			}`,
			shadow:              true,
			wantErr:             grok.ErrAuthCannotRefresh,
			wantZeroNetworkCall: true,
			wantFileUnchanged:   true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			file := filepath.Join(t.TempDir(), "auth.json")
			if !tt.missingFile {
				writeFixtureFile(t, file, tt.authJSON)
			}

			var mu sync.Mutex
			var seenURLs []string
			var tokenURL, tokenContentType, tokenBody string
			var totalCalls atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				totalCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					w.WriteHeader(tt.discoveryStatus)
					_, _ = io.WriteString(w, tt.discoveryBody)
				case "/oauth/token":
					w.WriteHeader(tt.tokenStatus)
					_, _ = io.WriteString(w, tt.tokenBody)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(srv.Client().CloseIdleConnections)

			tr := redirectTransport(srv, func(origURL, method, contentType, body string) {
				mu.Lock()
				defer mu.Unlock()
				seenURLs = append(seenURLs, origURL)
				if method == http.MethodPost {
					tokenURL = origURL
					tokenContentType = contentType
					tokenBody = body
				}
			})

			store := grok.NewAuthStore(grok.AuthOptions{
				File:      file,
				Transport: tr,
				Now:       func() time.Time { return now },
				Shadow:    tt.shadow,
			})

			gotToken, err := store.Refresh(t.Context())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Refresh() error = %v, want %v", err, tt.wantErr)
				}
				if gotToken != "" {
					t.Errorf("Refresh() returned token %q on error", gotToken)
				}
				if strings.Contains(err.Error(), "fixture-secret-refresh-sentinel") || strings.Contains(err.Error(), "fixture-expired-token") {
					t.Errorf("Refresh() error leaked credential: %q", err.Error())
				}
				if tt.wantZeroNetworkCall && totalCalls.Load() != 0 {
					t.Errorf("network calls = %d, want 0 (URLs: %v)", totalCalls.Load(), seenURLs)
				}
				if tt.wantFileUnchanged {
					after, readErr := os.ReadFile(file)
					if readErr != nil {
						t.Fatalf("read unchanged auth file: %v", readErr)
					}
					if diff := gocmp.Diff(tt.authJSON, string(after)); diff != "" {
						t.Errorf("auth.json changed on failure (-want +got):\n%s", diff)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Refresh() unexpected error: %v", err)
			}

			if diff := gocmp.Diff(tt.wantToken, gotToken); diff != "" {
				t.Errorf("Refresh() token (-want +got):\n%s", diff)
			}
			mu.Lock()
			gotTokenURL, gotContentType, gotBody := tokenURL, tokenContentType, tokenBody
			mu.Unlock()
			if diff := gocmp.Diff(tt.wantTokenURL, gotTokenURL); diff != "" {
				t.Errorf("token endpoint URL (-want +got):\n%s", diff)
			}
			if !strings.HasPrefix(gotContentType, "application/x-www-form-urlencoded") {
				t.Errorf("token request Content-Type = %q, want application/x-www-form-urlencoded", gotContentType)
			}
			if !strings.Contains(gotBody, "grant_type=refresh_token") {
				t.Errorf("token request body = %q, want grant_type=refresh_token", gotBody)
			}
			form, parseErr := url.ParseQuery(gotBody)
			if parseErr != nil {
				t.Fatalf("parse form body %q: %v", gotBody, parseErr)
			}
			if diff := gocmp.Diff("fixture-client", form.Get("client_id")); diff != "" {
				t.Errorf("form client_id (-want +got):\n%s", diff)
			}

			persistedBytes, readErr := os.ReadFile(file)
			if readErr != nil {
				t.Fatalf("read persisted auth file: %v", readErr)
			}
			if !strings.HasSuffix(string(persistedBytes), "\n") {
				t.Errorf("persisted auth.json must end with newline")
			}
			info, statErr := os.Stat(file)
			if statErr != nil {
				t.Fatalf("stat persisted auth file: %v", statErr)
			}
			if info.Mode().Perm()&0o077 != 0 {
				t.Errorf("persisted auth.json mode = %04o, want no group/other permissions", info.Mode().Perm())
			}

			var persisted map[string]any
			if unmarshalErr := json.Unmarshal(persistedBytes, &persisted); unmarshalErr != nil {
				t.Fatalf("unmarshal persisted auth.json: %v", unmarshalErr)
			}
			entry, ok := persisted["https://auth.x.ai::fixture-client"].(map[string]any)
			if !ok {
				t.Fatalf("persisted auth.json missing x.ai entry: %#v", persisted)
			}
			if diff := gocmp.Diff(tt.wantToken, entry["key"]); diff != "" {
				t.Errorf("persisted key (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantRefreshRotated, entry["refresh_token"]); diff != "" {
				t.Errorf("persisted refresh_token (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(float64(tt.wantExpiresAt), entry["expires_at"]); diff != "" {
				t.Errorf("persisted expires_at (-want +got):\n%s", diff)
			}
		})
	}
}

func TestAccessToken(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		expiresAt     string
		shadow        bool
		wantToken     string
		wantRefreshed bool
		wantErr       error
	}{
		"success: fresh ISO timestamp skips refresh": {
			expiresAt:     `"2026-11-26T13:00:00Z"`,
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: fresh epoch seconds skips refresh": {
			expiresAt:     "1795698000", // 2026-11-26T13:00:00Z in seconds (< 1e11)
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: fresh epoch milliseconds skips refresh": {
			expiresAt:     "1795698000000", // 2026-11-26T13:00:00Z in milliseconds (>= 1e11)
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: shadow mode returns valid non-expiring token": {
			expiresAt:     `"2026-11-26T13:00:00Z"`,
			shadow:        true,
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: token within 5 minute skew triggers refresh": {
			expiresAt:     `"2026-11-26T12:04:00Z"`,
			wantToken:     "fixture-fresh-token",
			wantRefreshed: true,
		},
		"success: unparseable expiry string triggers refresh": {
			expiresAt:     `"not-a-timestamp"`,
			wantToken:     "fixture-fresh-token",
			wantRefreshed: true,
		},
		"error: shadow mode refuses expiring token without network or disk writes": {
			expiresAt:     `"2026-11-26T12:04:00Z"`,
			shadow:        true,
			wantRefreshed: false,
			wantErr:       grok.ErrAuthCannotRefresh,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					_, _ = io.WriteString(w, `{"token_endpoint":"http://`+r.Host+`/oauth/token"}`)
				case "/oauth/token":
					_, _ = io.WriteString(w, `{"access_token":"fixture-fresh-token","expires_in":3600}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			t.Cleanup(srv.Client().CloseIdleConnections)

			file := filepath.Join(t.TempDir(), "auth.json")
			initial := `{"https://auth.x.ai::fixture-client":{"key":"fixture-current-token","refresh_token":"fixture-refresh-token","expires_at":` + tt.expiresAt + `,"oidc_client_id":"fixture-client","oidc_issuer":"` + srv.URL + `"}}`
			writeFixtureFile(t, file, initial)

			store := grok.NewAuthStore(grok.AuthOptions{
				File:      file,
				Transport: srv.Client().Transport,
				Now:       func() time.Time { return now },
				Shadow:    tt.shadow,
			})

			got, err := store.AccessToken(t.Context())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("AccessToken() error = %v, want %v", err, tt.wantErr)
				}
				if hits.Load() != 0 {
					t.Errorf("network hits = %d, want 0", hits.Load())
				}
				after, readErr := os.ReadFile(file)
				if readErr != nil {
					t.Fatalf("read auth file: %v", readErr)
				}
				if diff := gocmp.Diff(initial, string(after)); diff != "" {
					t.Errorf("auth.json changed in shadow mode (-want +got):\n%s", diff)
				}
				return
			}
			if err != nil {
				t.Fatalf("AccessToken() unexpected error: %v", err)
			}
			if diff := gocmp.Diff(tt.wantToken, got); diff != "" {
				t.Errorf("AccessToken() (-want +got):\n%s", diff)
			}
			if tt.wantRefreshed && hits.Load() == 0 {
				t.Errorf("expected refresh HTTP call, got 0")
			}
			if !tt.wantRefreshed && hits.Load() != 0 {
				t.Errorf("expected 0 refresh HTTP calls, got %d", hits.Load())
			}
		})
	}
}

func TestAccessTokenSingleFlight(t *testing.T) {
	t.Cleanup(func() { goleak.VerifyNone(t) })
	synctest.Test(t, func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "auth.json")
		writeFixtureFile(t, file, `{"https://auth.x.ai::fixture-client":{"key":"fixture-expired-token","refresh_token":"fixture-refresh-token","expires_at":0,"oidc_client_id":"fixture-client","oidc_issuer":"http://auth.fixture.test"}}`)

		release := make(chan struct{})
		var discoveryHits atomic.Int32
		var tokenHits atomic.Int32
		tr := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/.well-known/openid-configuration":
				discoveryHits.Add(1)
				return jsonHTTPResponse(req, http.StatusOK, `{"token_endpoint":"http://auth.fixture.test/oauth/token"}`), nil
			case "/oauth/token":
				tokenHits.Add(1)
				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"fixture-fresh-token","expires_in":3600}`), nil
			default:
				return jsonHTTPResponse(req, http.StatusNotFound, `{}`), nil
			}
		})

		now := time.UnixMilli(1_700_000_000_000)
		store := grok.NewAuthStore(grok.AuthOptions{
			File:      file,
			Transport: tr,
			Now:       func() time.Time { return now },
		})

		const callers = 5
		tokens := make([]string, callers)
		errs := make([]error, callers)
		var wg sync.WaitGroup

		wg.Go(func() {
			tokens[0], errs[0] = store.AccessToken(t.Context())
		})
		synctest.Wait()

		for i := 1; i < callers; i++ {
			wg.Go(func() {
				tokens[i], errs[i] = store.AccessToken(t.Context())
			})
		}
		synctest.Wait()

		close(release)
		wg.Wait()

		for i := range callers {
			if errs[i] != nil {
				t.Fatalf("caller %d AccessToken() error: %v", i, errs[i])
			}
			if diff := gocmp.Diff("fixture-fresh-token", tokens[i]); diff != "" {
				t.Errorf("caller %d token (-want +got):\n%s", i, diff)
			}
		}
		if got := discoveryHits.Load(); got != 1 {
			t.Errorf("discovery hits = %d, want 1", got)
		}
		if got := tokenHits.Load(); got != 1 {
			t.Errorf("token exchange hits = %d, want 1", got)
		}
	})
}
