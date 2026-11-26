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
	"bytes"
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
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func denyNetworkTransport(t *testing.T) http.RoundTripper {
	t.Helper()
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Error("unexpected outbound auth HTTP request")
		return nil, errors.New("network disabled in test")
	})
}

func newAuthTransport(t *testing.T, opts upstream.Options) *http.Transport {
	t.Helper()
	tr := upstream.NewTransport(opts)
	if tr == nil {
		t.Fatal("upstream.NewTransport returned nil")
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func mustNewAuthStore(t *testing.T, opts grok.AuthOptions) *grok.AuthStore {
	t.Helper()
	store, err := grok.NewAuthStore(opts)
	if err != nil {
		t.Fatal("NewAuthStore returned an unexpected error")
	}
	if store == nil {
		t.Fatal("NewAuthStore() returned nil store")
	}
	return store
}

func assertNoSecrets(t *testing.T, label, text string, secrets ...string) {
	t.Helper()
	for i, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("%s leaked fixture secret #%d", label, i+1)
		}
	}
}

func freezeAuthFixture(t *testing.T, dir, file string) os.FileInfo {
	t.Helper()
	if err := os.Chmod(file, 0o400); err != nil {
		t.Fatalf("chmod fixture file read-only: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod fixture dir read-only: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o700)
		_ = os.Chmod(file, 0o600)
	})
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat frozen auth fixture: %v", err)
	}
	return info
}

func assertAuthFixtureUntouched(t *testing.T, dir, file string, before os.FileInfo, wantContent string) {
	t.Helper()
	after, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat auth fixture after call: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("auth fixture file identity changed (file was replaced)")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("auth fixture mtime changed from %v to %v", before.ModTime(), after.ModTime())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read auth fixture dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(file) {
		t.Errorf("auth fixture directory entries changed (found %d; want only the fixture file)", len(entries))
	}
	gotBytes, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read auth fixture after call: %v", err)
	}
	if !bytes.Equal(gotBytes, []byte(wantContent)) {
		t.Errorf("auth fixture bytes changed (got len=%d, want len=%d)", len(gotBytes), len(wantContent))
	}
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

type recordedAuthRequest struct {
	method      string
	url         string
	contentType string
	body        string
	headers     http.Header
}

func TestNewAuthStore(t *testing.T) {
	file := filepath.Join(t.TempDir(), "auth.json")

	tests := map[string]struct {
		opts    grok.AuthOptions
		wantErr bool
	}{
		"error: nil transport is rejected": {
			opts: grok.AuthOptions{
				File:      file,
				Transport: nil,
			},
			wantErr: true,
		},
		"error: nil transport is rejected in shadow mode": {
			opts: grok.AuthOptions{
				File:      file,
				Transport: nil,
				Shadow:    true,
			},
			wantErr: true,
		},
		"success: explicit non-nil transport is accepted": {
			opts: grok.AuthOptions{
				File:      file,
				Transport: denyNetworkTransport(t),
			},
			wantErr: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			store, err := grok.NewAuthStore(tt.opts)
			if tt.wantErr {
				if err == nil {
					t.Fatal("NewAuthStore() error = nil, want non-nil error for nil Transport")
				}
				if store != nil {
					t.Error("NewAuthStore() returned non-nil store on error")
				}
				return
			}
			if err != nil {
				t.Fatal("NewAuthStore returned an unexpected error")
			}
			if store == nil {
				t.Fatal("NewAuthStore() returned nil store")
			}
		})
	}
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
		"success: CheckAuth and Readiness never refresh or write expired auth": {
			check: func(t *testing.T) func() error {
				t.Cleanup(func() { goleak.VerifyNone(t) })
				var hits atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hits.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				t.Cleanup(srv.Close)
				t.Cleanup(srv.Client().CloseIdleConnections)

				dir := t.TempDir()
				file := filepath.Join(dir, "auth.json")
				content := `{"https://auth.x.ai::fixture-client":{"key":"fixture-expired-key","refresh_token":"fixture-refresh","expires_at":0,"oidc_client_id":"fixture-client","oidc_issuer":"` + srv.URL + `"}}`
				writeFixtureFile(t, file, content)
				before := freezeAuthFixture(t, dir, file)
				return func() error {
					err := grok.CheckAuth(file)
					if hits.Load() != 0 {
						t.Errorf("CheckAuth performed %d network calls, want 0", hits.Load())
					}
					assertAuthFixtureUntouched(t, dir, file, before, content)
					return err
				}
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
				return func() error {
					err := grok.CheckAuth(file)
					if err != nil && !errors.Is(err, grok.ErrAuthMissing) {
						t.Errorf("CheckAuth(missing) error = %v, want %v", err, grok.ErrAuthMissing)
					}
					if err != nil && err.Error() != grok.ErrAuthMissing.Error() {
						t.Errorf("CheckAuth(missing) error message = %q, want exact %q", err.Error(), grok.ErrAuthMissing.Error())
					}
					return err
				}
			},
			want: catalog.Readiness{
				Ready:   false,
				State:   "auth-missing",
				Message: "Grok CLI auth is missing. Run `grok` and log in again.",
			},
		},
		"error: CheckAuth rejects empty key without leaking sibling secrets": {
			check: func(t *testing.T) func() error {
				file := filepath.Join(t.TempDir(), "auth.json")
				writeFixtureFile(t, file, `{"https://auth.x.ai::fixture-client":{"key":"","refresh_token":"fixture-secret-refresh-checkauth"}}`)
				return func() error {
					err := grok.CheckAuth(file)
					if err != nil {
						assertNoSecrets(t, "CheckAuth error", err.Error(), "fixture-secret-refresh-checkauth")
						if !errors.Is(err, grok.ErrAuthInvalid) || err.Error() != grok.ErrAuthInvalid.Error() {
							t.Errorf("CheckAuth(empty key) error = %v, want exact %v", err, grok.ErrAuthInvalid)
						}
					}
					return err
				}
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
			assertNoSecrets(t, "readiness", fmt.Sprint(got), "fixture-token", "fixture-refresh", "fixture-expired-key", "fixture-secret-refresh-checkauth", "ignored")
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
		useDiscoveredServer bool
		tokenStatus         int
		tokenBody           string
		expectedRefresh     string
		wantToken           string
		wantErr             error
		wantDiscoveryURL    string
		wantTokenURL        string // empty means tokenSrv.URL + "/discovered/oauth/token"
		wantRefreshRotated  string
		wantExpiresAt       int64
		wantIssuer          string
		wantPreserveOther   bool
		wantZeroNetworkCall bool
		wantFileUnchanged   bool
		secrets             []string
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
			discoveryStatus:     http.StatusOK,
			useDiscoveredServer: true,
			tokenStatus:         http.StatusOK,
			tokenBody:           `{"access_token":"fixture-fresh-token","expires_in":3600}`,
			expectedRefresh:     "fixture-refresh-token",
			wantToken:           "fixture-fresh-token",
			wantDiscoveryURL:    "https://auth.x.ai/.well-known/openid-configuration",
			wantRefreshRotated:  "fixture-refresh-token",
			wantExpiresAt:       now.UnixMilli() + 3600*1000,
			wantPreserveOther:   true,
			secrets:             []string{"fixture-expired-token", "fixture-refresh-token", "fixture-fresh-token"},
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
			discoveryStatus:     http.StatusInternalServerError,
			useDiscoveredServer: false,
			tokenStatus:         http.StatusOK,
			tokenBody:           `{"access_token":"fixture-rotated-token","refresh_token":"fixture-new-refresh","expires_in":1200}`,
			expectedRefresh:     "fixture-old-refresh",
			wantToken:           "fixture-rotated-token",
			wantDiscoveryURL:    "https://issuer.example.test/.well-known/openid-configuration",
			wantTokenURL:        "https://issuer.example.test/oauth/token",
			wantRefreshRotated:  "fixture-new-refresh",
			wantExpiresAt:       now.UnixMilli() + 1200*1000,
			wantIssuer:          "https://issuer.example.test/",
			secrets:             []string{"fixture-expired-token", "fixture-old-refresh", "fixture-rotated-token", "fixture-new-refresh"},
		},
		"error: missing refresh token cannot refresh": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token-noref",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				}
			}`,
			wantErr:             grok.ErrAuthCannotRefresh,
			wantZeroNetworkCall: true,
			wantFileUnchanged:   true,
			secrets:             []string{"fixture-expired-token-noref"},
		},
		"error: missing client id cannot refresh": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token-noclient",
					"refresh_token": "fixture-refresh-token-noclient",
					"expires_at": 0
				}
			}`,
			wantErr:             grok.ErrAuthCannotRefresh,
			wantZeroNetworkCall: true,
			wantFileUnchanged:   true,
			secrets:             []string{"fixture-expired-token-noclient", "fixture-refresh-token-noclient"},
		},
		"error: non-2xx token response fails without modifying file": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token-502",
					"refresh_token": "fixture-secret-refresh-sentinel",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				}
			}`,
			discoveryStatus:     http.StatusOK,
			useDiscoveredServer: true,
			tokenStatus:         http.StatusBadGateway,
			tokenBody:           `{"error":"invalid_grant","detail":"fixture-upstream-secret-detail"}`,
			expectedRefresh:     "fixture-secret-refresh-sentinel",
			wantErr:             grok.ErrAuthRefreshFailed,
			wantDiscoveryURL:    "https://auth.x.ai/.well-known/openid-configuration",
			wantFileUnchanged:   true,
			secrets:             []string{"fixture-expired-token-502", "fixture-secret-refresh-sentinel", "fixture-upstream-secret-detail"},
		},
		"error: missing access token in 200 response fails": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token-empty",
					"refresh_token": "fixture-refresh-token-empty",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				}
			}`,
			discoveryStatus:     http.StatusOK,
			useDiscoveredServer: true,
			tokenStatus:         http.StatusOK,
			tokenBody:           `{"access_token":""}`,
			expectedRefresh:     "fixture-refresh-token-empty",
			wantErr:             grok.ErrAuthRefreshFailed,
			wantDiscoveryURL:    "https://auth.x.ai/.well-known/openid-configuration",
			wantFileUnchanged:   true,
			secrets:             []string{"fixture-expired-token-empty", "fixture-refresh-token-empty"},
		},
		"error: missing auth file fails": {
			missingFile:         true,
			wantErr:             grok.ErrAuthMissing,
			wantZeroNetworkCall: true,
		},
		"error: invalid auth file without x.ai entry fails": {
			authJSON:            `{"other":{"key":"fixture-unrelated-secret"}}`,
			wantErr:             grok.ErrAuthInvalid,
			wantZeroNetworkCall: true,
			wantFileUnchanged:   true,
			secrets:             []string{"fixture-unrelated-secret"},
		},
		"error: shadow mode disables refresh without network or disk writes": {
			authJSON: `{
				"https://auth.x.ai::fixture-client": {
					"key": "fixture-expired-token-shadow",
					"refresh_token": "fixture-refresh-token-shadow",
					"expires_at": 0,
					"oidc_client_id": "fixture-client"
				}
			}`,
			shadow:              true,
			wantErr:             grok.ErrAuthCannotRefresh,
			wantZeroNetworkCall: true,
			wantFileUnchanged:   true,
			secrets:             []string{"fixture-expired-token-shadow", "fixture-refresh-token-shadow"},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			dir := t.TempDir()
			file := filepath.Join(dir, "auth.json")
			var beforeInfo os.FileInfo
			if !tt.missingFile {
				writeFixtureFile(t, file, tt.authJSON)
				if tt.wantFileUnchanged {
					beforeInfo = freezeAuthFixture(t, dir, file)
				}
			}

			var tr http.RoundTripper
			var mu sync.Mutex
			var requests []recordedAuthRequest
			var expectedTokenURL string

			if tt.wantZeroNetworkCall {
				tr = denyNetworkTransport(t)
			} else {
				recordRequest := func(r *http.Request, body []byte) {
					scheme := "http"
					if r.TLS != nil {
						scheme = "https"
					}
					mu.Lock()
					requests = append(requests, recordedAuthRequest{
						method:      r.Method,
						url:         scheme + "://" + r.Host + r.URL.RequestURI(),
						contentType: r.Header.Get("Content-Type"),
						body:        string(body),
						headers:     r.Header.Clone(),
					})
					mu.Unlock()
				}

				handleTokenExchange := func(w http.ResponseWriter, r *http.Request, raw []byte) {
					w.Header().Set("Content-Type", "application/json")
					rawBody := string(raw)
					form, parseErr := url.ParseQuery(rawBody)
					if parseErr != nil ||
						r.Method != http.MethodPost ||
						!strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") ||
						!strings.Contains(rawBody, "grant_type=refresh_token") ||
						form.Get("grant_type") != "refresh_token" ||
						form.Get("refresh_token") != tt.expectedRefresh ||
						form.Get("client_id") != "fixture-client" {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
						return
					}
					w.WriteHeader(tt.tokenStatus)
					_, _ = io.WriteString(w, tt.tokenBody)
				}

				tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					recordRequest(r, raw)
					if r.URL.Path != "/discovered/oauth/token" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					handleTokenExchange(w, r, raw)
				}))
				t.Cleanup(tokenSrv.Close)
				t.Cleanup(tokenSrv.Client().CloseIdleConnections)

				discoveredEndpoint := tokenSrv.URL + "/discovered/oauth/token"
				expectedTokenURL = tt.wantTokenURL
				if expectedTokenURL == "" {
					expectedTokenURL = discoveredEndpoint
				}

				discoverySrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					raw, _ := io.ReadAll(r.Body)
					recordRequest(r, raw)
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/.well-known/openid-configuration":
						w.WriteHeader(tt.discoveryStatus)
						if tt.useDiscoveredServer {
							_, _ = fmt.Fprintf(w, `{"token_endpoint":%q}`, discoveredEndpoint)
						} else {
							_, _ = io.WriteString(w, `{"error":"discovery unavailable"}`)
						}
					case "/oauth/token":
						if tt.useDiscoveredServer {
							// Falling back when discovery succeeded must fail.
							w.WriteHeader(http.StatusNotFound)
							_, _ = io.WriteString(w, `{"error":"must use discovered token endpoint"}`)
							return
						}
						handleTokenExchange(w, r, raw)
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(discoverySrv.Close)
				t.Cleanup(discoverySrv.Client().CloseIdleConnections)

				tlsCfg := discoverySrv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				tlsCfg.ServerName = "127.0.0.1"

				var dialer net.Dialer
				tokenAddr := tokenSrv.Listener.Addr().String()
				discoveryAddr := discoverySrv.Listener.Addr().String()
				dialCtx := func(ctx context.Context, network, addr string) (net.Conn, error) {
					switch addr {
					case "auth.x.ai:443", "issuer.example.test:443":
						return dialer.DialContext(ctx, network, discoveryAddr)
					case tokenAddr:
						return dialer.DialContext(ctx, network, tokenAddr)
					default:
						t.Error("blocked an unapproved auth dial")
						return nil, fmt.Errorf("blocked unapproved dial to %q", addr)
					}
				}
				tr = newAuthTransport(t, upstream.Options{
					DialContext: dialCtx,
					TLSConfig:   tlsCfg,
				})
			}

			store := mustNewAuthStore(t, grok.AuthOptions{
				File:      file,
				Transport: tr,
				Now:       func() time.Time { return now },
				Shadow:    tt.shadow,
			})

			gotToken, err := store.Refresh(t.Context())
			if err != nil {
				assertNoSecrets(t, "Refresh() error", err.Error(), tt.secrets...)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Refresh() error = %v, want %v", err, tt.wantErr)
				}
				if err.Error() != tt.wantErr.Error() {
					t.Errorf("Refresh() error text = %q, want exact %q", err.Error(), tt.wantErr.Error())
				}
				if gotToken != "" {
					t.Errorf("Refresh() returned non-empty token on error (len=%d)", len(gotToken))
				}
				if tt.wantFileUnchanged {
					assertAuthFixtureUntouched(t, dir, file, beforeInfo, tt.authJSON)
				}
				if !tt.wantZeroNetworkCall {
					mu.Lock()
					gotReqs := append([]recordedAuthRequest(nil), requests...)
					mu.Unlock()
					assertOrderedRefreshRequests(t, gotReqs, tt.wantDiscoveryURL, expectedTokenURL, tt.expectedRefresh, tt.secrets...)
				}
				return
			}
			if err != nil {
				t.Fatalf("Refresh() unexpected error: %v", err)
			}

			if gotToken != tt.wantToken {
				t.Errorf("Refresh() returned unexpected token (got len=%d, want len=%d)", len(gotToken), len(tt.wantToken))
			}

			mu.Lock()
			gotReqs := append([]recordedAuthRequest(nil), requests...)
			mu.Unlock()
			assertOrderedRefreshRequests(t, gotReqs, tt.wantDiscoveryURL, expectedTokenURL, tt.expectedRefresh, tt.secrets...)

			persistedBytes, readErr := os.ReadFile(file)
			if readErr != nil {
				t.Fatalf("read persisted auth file: %v", readErr)
			}
			if !strings.HasSuffix(string(persistedBytes), "\n") {
				t.Error("persisted auth.json must end with newline")
			}
			info, statErr := os.Stat(file)
			if statErr != nil {
				t.Fatalf("stat persisted auth file: %v", statErr)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("persisted auth.json mode = %04o, want exact 0600", info.Mode().Perm())
			}

			var persisted map[string]any
			if unmarshalErr := json.Unmarshal(persistedBytes, &persisted); unmarshalErr != nil {
				t.Fatal("persisted auth.json was not a JSON object")
			}
			wantKeys := 1
			if tt.wantPreserveOther {
				wantKeys = 2
				other, ok := persisted["unrelated"].(map[string]any)
				if !ok {
					t.Fatal("persisted auth.json lost unrelated entry")
				}
				assertNoSecrets(t, "persisted unrelated entry", fmt.Sprint(other), tt.secrets...)
				if diff := gocmp.Diff(map[string]any{"keep": true}, other); diff != "" {
					t.Errorf("persisted unrelated entry (-want +got):\n%s", diff)
				}
			}
			if len(persisted) != wantKeys {
				t.Errorf("persisted auth.json top-level key count = %d, want %d", len(persisted), wantKeys)
			}

			entry, ok := persisted["https://auth.x.ai::fixture-client"].(map[string]any)
			if !ok {
				t.Fatal("persisted auth.json missing x.ai entry")
			}
			wantEntry := map[string]any{
				"key":            tt.wantToken,
				"refresh_token":  tt.wantRefreshRotated,
				"expires_at":     float64(tt.wantExpiresAt),
				"oidc_client_id": "fixture-client",
			}
			if tt.wantIssuer != "" {
				wantEntry["oidc_issuer"] = tt.wantIssuer
			}
			if entry["key"] != tt.wantToken {
				t.Error("persisted key did not match refreshed access token")
			}
			if entry["refresh_token"] != tt.wantRefreshRotated {
				t.Error("persisted refresh_token did not match expected refresh token")
			}
			assertNoSecrets(t, "persisted auth metadata", fmt.Sprint(entry["expires_at"], entry["oidc_client_id"], entry["oidc_issuer"]), tt.secrets...)
			if diff := gocmp.Diff(float64(tt.wantExpiresAt), entry["expires_at"]); diff != "" {
				t.Errorf("persisted expires_at (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff("fixture-client", entry["oidc_client_id"]); diff != "" {
				t.Errorf("persisted oidc_client_id (-want +got):\n%s", diff)
			}
			if tt.wantIssuer != "" {
				if diff := gocmp.Diff(tt.wantIssuer, entry["oidc_issuer"]); diff != "" {
					t.Errorf("persisted oidc_issuer (-want +got):\n%s", diff)
				}
			}
			if !gocmp.Equal(wantEntry, entry) {
				t.Errorf("persisted x.ai entry did not match expected entry (got %d fields, want %d; values redacted)", len(entry), len(wantEntry))
			}
		})
	}
}

func assertOrderedRefreshRequests(t *testing.T, reqs []recordedAuthRequest, wantDiscoveryURL, wantTokenURL, wantRefreshToken string, secrets ...string) {
	t.Helper()
	for _, r := range reqs {
		assertNoSecrets(t, "auth request metadata", fmt.Sprint(r.method, r.url, r.contentType, r.headers), secrets...)
	}
	if len(reqs) != 2 {
		t.Fatalf("recorded auth HTTP requests = %d, want 2 (discovery then exchange)", len(reqs))
	}
	disc := reqs[0]
	if diff := gocmp.Diff(http.MethodGet, disc.method); diff != "" {
		t.Errorf("discovery request method (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(wantDiscoveryURL, disc.url); diff != "" {
		t.Errorf("discovery request URL (-want +got):\n%s", diff)
	}

	exch := reqs[1]
	if diff := gocmp.Diff(http.MethodPost, exch.method); diff != "" {
		t.Errorf("exchange request method (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(wantTokenURL, exch.url); diff != "" {
		t.Errorf("exchange request URL (-want +got):\n%s", diff)
	}
	if !strings.HasPrefix(exch.contentType, "application/x-www-form-urlencoded") {
		t.Errorf("exchange Content-Type = %q, want prefix application/x-www-form-urlencoded", exch.contentType)
	}
	if !strings.Contains(exch.body, "grant_type=refresh_token") {
		t.Error("exchange form body missing literal grant_type=refresh_token")
	}
	form, err := url.ParseQuery(exch.body)
	if err != nil {
		t.Fatal("could not parse exchange form body")
	}
	assertNoSecrets(t, "exchange form metadata", fmt.Sprint(form["grant_type"], form["client_id"]), secrets...)
	wantForm := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {wantRefreshToken},
		"client_id":     {"fixture-client"},
	}
	if len(form) != len(wantForm) {
		t.Errorf("exchange form field count = %d, want %d", len(form), len(wantForm))
	}
	if diff := gocmp.Diff(wantForm["grant_type"], form["grant_type"]); diff != "" {
		t.Errorf("exchange form grant_type (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(wantForm["client_id"], form["client_id"]); diff != "" {
		t.Errorf("exchange form client_id (-want +got):\n%s", diff)
	}
	if !gocmp.Equal(wantForm["refresh_token"], form["refresh_token"]) {
		t.Error("exchange form refresh_token did not match fixture refresh token")
	}

	for i, req := range reqs {
		for _, forbidden := range []string{"Authorization", "X-Grok-Client-Version", "X-Grok-Model-Override", "X-Grok-Client-Identifier"} {
			if req.headers.Get(forbidden) != "" {
				t.Errorf("auth request #%d sent forbidden inference header %q", i+1, forbidden)
			}
		}
	}
}

func TestAccessToken(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		expiresAt     *string
		shadow        bool
		wantToken     string
		wantRefreshed bool
		wantErr       error
	}{
		"success: fresh ISO timestamp skips refresh": {
			expiresAt:     new(`"2026-11-26T13:00:00Z"`),
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: fresh epoch seconds skips refresh": {
			expiresAt:     new("1795698000"), // 2026-11-26T13:00:00Z in seconds (< 1e11)
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: fresh epoch milliseconds skips refresh": {
			expiresAt:     new("1795698000000"), // 2026-11-26T13:00:00Z in milliseconds (>= 1e11)
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: shadow mode returns valid non-expiring token": {
			expiresAt:     new(`"2026-11-26T13:00:00Z"`),
			shadow:        true,
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: shadow mode uses valid token inside refresh skew window without refreshing": {
			expiresAt:     new(`"2026-11-26T12:04:00Z"`),
			shadow:        true,
			wantToken:     "fixture-current-token",
			wantRefreshed: false,
		},
		"success: token within 5 minute skew triggers refresh": {
			expiresAt:     new(`"2026-11-26T12:04:00Z"`),
			wantToken:     "fixture-fresh-token",
			wantRefreshed: true,
		},
		"success: unparseable expiry string triggers refresh": {
			expiresAt:     new(`"not-a-timestamp"`),
			wantToken:     "fixture-fresh-token",
			wantRefreshed: true,
		},
		"error: shadow mode refuses token at exact expiry without network or disk writes": {
			expiresAt:     new(`"2026-11-26T12:00:00Z"`),
			shadow:        true,
			wantRefreshed: false,
			wantErr:       grok.ErrAuthCannotRefresh,
		},
		"error: shadow mode refuses expired token without network or disk writes": {
			expiresAt:     new(`"2026-11-26T11:59:00Z"`),
			shadow:        true,
			wantRefreshed: false,
			wantErr:       grok.ErrAuthCannotRefresh,
		},
		"error: shadow mode refuses unverifiable expiry without network or disk writes": {
			expiresAt:     new(`"not-a-timestamp"`),
			shadow:        true,
			wantRefreshed: false,
			wantErr:       grok.ErrAuthCannotRefresh,
		},
		"error: shadow mode refuses missing expiry without network or disk writes": {
			expiresAt:     nil,
			shadow:        true,
			wantRefreshed: false,
			wantErr:       grok.ErrAuthCannotRefresh,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { goleak.VerifyNone(t) })

			var tr http.RoundTripper
			var issuerURL string
			var hits atomic.Int32

			if tt.wantRefreshed {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/.well-known/openid-configuration":
						_, _ = fmt.Fprintf(w, `{"token_endpoint":"http://%s/discovered/token"}`, r.Host)
					case "/discovered/token":
						raw, _ := io.ReadAll(r.Body)
						form, err := url.ParseQuery(string(raw))
						if err != nil || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "fixture-refresh-token" || form.Get("client_id") != "fixture-client" {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
							return
						}
						_, _ = io.WriteString(w, `{"access_token":"fixture-fresh-token","expires_in":3600}`)
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(srv.Close)
				t.Cleanup(srv.Client().CloseIdleConnections)
				issuerURL = srv.URL

				var dialer net.Dialer
				srvAddr := srv.Listener.Addr().String()
				tr = newAuthTransport(t, upstream.Options{
					DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
						if addr != srvAddr {
							t.Error("blocked an unapproved auth dial")
							return nil, fmt.Errorf("blocked unapproved dial to %q", addr)
						}
						return dialer.DialContext(ctx, network, srvAddr)
					},
				})
			} else {
				issuerURL = "http://127.0.0.1:1"
				tr = denyNetworkTransport(t)
			}

			dir := t.TempDir()
			file := filepath.Join(dir, "auth.json")
			var expiresField string
			if tt.expiresAt != nil {
				expiresField = `"expires_at":` + *tt.expiresAt + `,`
			}
			initial := `{"https://auth.x.ai::fixture-client":{"key":"fixture-current-token","refresh_token":"fixture-refresh-token",` + expiresField + `"oidc_client_id":"fixture-client","oidc_issuer":"` + issuerURL + `"}}`
			writeFixtureFile(t, file, initial)

			var beforeInfo os.FileInfo
			if !tt.wantRefreshed {
				beforeInfo = freezeAuthFixture(t, dir, file)
			}

			store := mustNewAuthStore(t, grok.AuthOptions{
				File:      file,
				Transport: tr,
				Now:       func() time.Time { return now },
				Shadow:    tt.shadow,
			})

			got, err := store.AccessToken(t.Context())
			if err != nil {
				assertNoSecrets(t, "AccessToken() error", err.Error(), "fixture-current-token", "fixture-refresh-token", "fixture-fresh-token")
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("AccessToken() error = %v, want %v", err, tt.wantErr)
				}
				if err.Error() != tt.wantErr.Error() {
					t.Errorf("AccessToken() error text = %q, want exact %q", err.Error(), tt.wantErr.Error())
				}
				if got != "" {
					t.Errorf("AccessToken() returned non-empty token on error (len=%d)", len(got))
				}
				assertAuthFixtureUntouched(t, dir, file, beforeInfo, initial)
				return
			}
			if err != nil {
				t.Fatalf("AccessToken() unexpected error: %v", err)
			}
			if got != tt.wantToken {
				t.Errorf("AccessToken() returned unexpected token (got len=%d, want len=%d)", len(got), len(tt.wantToken))
			}
			if tt.wantRefreshed {
				if hits.Load() != 2 {
					t.Errorf("refresh HTTP calls = %d, want 2 (discovery + exchange)", hits.Load())
				}
			} else {
				assertAuthFixtureUntouched(t, dir, file, beforeInfo, initial)
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
				return jsonHTTPResponse(req, http.StatusOK, `{"token_endpoint":"http://auth.fixture.test/discovered/token"}`), nil
			case "/discovered/token":
				tokenHits.Add(1)
				raw, _ := io.ReadAll(req.Body)
				form, err := url.ParseQuery(string(raw))
				if err != nil || form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "fixture-refresh-token" || form.Get("client_id") != "fixture-client" {
					return jsonHTTPResponse(req, http.StatusBadRequest, `{"error":"invalid_grant"}`), nil
				}
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
		store := mustNewAuthStore(t, grok.AuthOptions{
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
				assertNoSecrets(t, "AccessToken() error", errs[i].Error(), "fixture-expired-token", "fixture-refresh-token", "fixture-fresh-token")
				t.Fatalf("caller %d AccessToken() error: %v", i, errs[i])
			}
			if tokens[i] != "fixture-fresh-token" {
				t.Errorf("caller %d returned unexpected token (got len=%d)", i, len(tokens[i]))
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
