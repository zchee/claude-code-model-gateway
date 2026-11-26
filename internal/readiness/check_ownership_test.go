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

package readiness_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

// statusRunnerDeadline is the source's limit on one status subprocess.
const statusRunnerDeadline = 15 * time.Second

// fakeProxyBinary creates a file standing in for the claude-code-proxy binary.
func fakeProxyBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-code-proxy")
	if err := os.WriteFile(path, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// TestProxyCheckNeverUsesInjectedCookieJar hands the proxy check a client with a cookie jar holding
// a cookie for the proxy: the /v1/models request carries no Cookie header, whether or not the client
// names a transport.
func TestProxyCheckNeverUsesInjectedCookieJar(t *testing.T) {
	tests := map[string]struct {
		ownTransport bool
	}{
		"success: client without a transport": {},
		"success: client with its own transport": {
			ownTransport: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var requests, withCookie atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if len(r.Header.Values("Cookie")) > 0 {
					withCookie.Add(1)
				}
				_, _ = w.Write([]byte(`{"data":[]}`))
			}))
			t.Cleanup(srv.Close)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatalf("cookiejar.New: %v", err)
			}
			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "readiness-jar-cookie-marker", Path: "/"}})
			client := &http.Client{Jar: jar}
			if tt.ownTransport {
				tr := &http.Transport{Proxy: nil}
				t.Cleanup(tr.CloseIdleConnections)
				client.Transport = tr
			} else {
				t.Cleanup(http.DefaultTransport.(*http.Transport).CloseIdleConnections)
			}

			got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
				ProxyBinary:    fakeProxyBinary(t),
				ProxyModelsURL: srv.URL + "/v1/models",
				Client:         client,
			})
			if !got.ProxyModels || requests.Load() == 0 {
				t.Fatalf("test precondition: the proxy check did not succeed against the fake proxy: %+v, requests %d", got, requests.Load())
			}
			if n := withCookie.Load(); n != 0 {
				t.Errorf("proxy check sent %d request(s) with a Cookie header from the injected client's cookie jar", n)
			}
		})
	}
}

// TestProxyCheckRequiresCompleteBody answers /v1/models with HTTP 200 and a body that cannot be read
// to its end. Only a body read completely counts as an answering proxy.
func TestProxyCheckRequiresCompleteBody(t *testing.T) {
	const declaredLength = "1000"
	tests := map[string]struct {
		handler func(w http.ResponseWriter, r *http.Request)
		timeout time.Duration
		want    bool
	}{
		"success: complete body is an answering proxy": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[]}`))
			},
			want: true,
		},
		"error: body that breaks off before its declared length": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", declaredLength)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":[`))
				rc := http.NewResponseController(w)
				_ = rc.Flush()
				conn, _, err := rc.Hijack()
				if err != nil {
					return
				}
				_ = conn.Close()
			},
		},
		"error: body that stalls until the check gives up": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", declaredLength)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":[`))
				_ = http.NewResponseController(w).Flush()
				<-r.Context().Done()
			},
			timeout: 500 * time.Millisecond,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tt.handler))
			t.Cleanup(srv.Close)
			ctx := t.Context()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}
			got := readiness.ProbeProxyAndAuth(ctx, readiness.ProxyAuthProbe{
				ProxyBinary:    fakeProxyBinary(t),
				ProxyModelsURL: srv.URL + "/v1/models",
			})
			want := readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: tt.want}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("ProbeProxyAndAuth mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestStatusRunnersGetSourceDeadline gives each injected status runner a command that honours only
// its context: the runner's context carries a deadline and ends exactly 15 s after the runner starts,
// in virtual time.
func TestStatusRunnersGetSourceDeadline(t *testing.T) {
	type observation struct {
		elapsed     time.Duration
		hasDeadline bool
	}
	errGaveUp := errors.New("runner gave up waiting for its context")
	// waitForContext blocks like a status command that ends only when its context does; it gives up
	// after a minute so a context without a deadline fails the test instead of deadlocking the bubble.
	waitForContext := func(ctx context.Context) (observation, error) {
		start := time.Now()
		_, hasDeadline := ctx.Deadline()
		select {
		case <-ctx.Done():
			return observation{time.Since(start), hasDeadline}, ctx.Err()
		case <-time.After(time.Minute):
			return observation{time.Since(start), hasDeadline}, errGaveUp
		}
	}
	tests := map[string]struct {
		run       func(t *testing.T, observed *observation) string
		wantState string
	}{
		"success: Codex auth status runner": {
			run: func(t *testing.T, observed *observation) string {
				got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
					ProxyBinary: fakeProxyBinary(t),
					RunAuthStatus: func(ctx context.Context, _ string, _ []string) ([]byte, []byte, error) {
						obs, err := waitForContext(ctx)
						*observed = obs
						return nil, nil, err
					},
				})
				if got.CodexAuth {
					return "authenticated"
				}
				return "unauthenticated"
			},
			wantState: "unauthenticated",
		},
		"success: provider auth status runner": {
			run: func(t *testing.T, observed *observation) string {
				return readiness.ProviderAuthState(t.Context(), "kimi", readiness.ProviderAuthOptions{
					Lookup:      lookupMap(nil),
					ConfigDir:   t.TempDir(),
					ProxyBinary: fakeProxyBinary(t),
					RunStatus: func(ctx context.Context, _ string, _ []string) (int, error) {
						obs, err := waitForContext(ctx)
						*observed = obs
						return 0, err
					},
				})
			},
			wantState: "auth-unknown",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var observed observation
				state := tt.run(t, &observed)
				if state != tt.wantState {
					t.Errorf("state = %q, want %q", state, tt.wantState)
				}
				if !observed.hasDeadline {
					t.Error("status runner context carries no deadline")
				}
				if observed.elapsed != statusRunnerDeadline {
					t.Errorf("status runner context ended after %v, want %v", observed.elapsed, statusRunnerDeadline)
				}
			})
		})
	}
}
