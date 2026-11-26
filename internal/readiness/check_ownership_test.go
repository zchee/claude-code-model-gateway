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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
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

// paddedModelsBody is a valid /v1/models document of exactly size bytes.
func paddedModelsBody(t *testing.T, size int) []byte {
	t.Helper()
	const doc = `{"data":[]}`
	if size < len(doc) {
		t.Fatalf("test precondition: size %d is below the document's own %d bytes", size, len(doc))
	}
	return append([]byte(doc), bytes.Repeat([]byte(" "), size-len(doc))...)
}

// TestProxyCheckRequiresCompleteBody answers /v1/models with HTTP 200 and a body that cannot be read
// to its end, or one at or over the size bound. Only a body read completely within the bound counts
// as an answering proxy. The stalled body is served in memory, so the test also sees that the check
// had the headers and was reading the body when it gave up at its own deadline. The size-bound bodies
// are served in memory with no declared length, so the bound decides, never the transport or the
// check's deadline, and the test sees how many body bytes the check read.
func TestProxyCheckRequiresCompleteBody(t *testing.T) {
	const (
		declaredLength = "1000"
		// proxyModelsBodyLimit is the largest /v1/models body the check reads as an answer.
		proxyModelsBodyLimit = 1 << 20
	)
	tests := map[string]struct {
		// handler serves the body over a loopback socket when neither body nor stallAfter is set.
		handler func(w http.ResponseWriter, r *http.Request)
		// body, when set, is served in memory with no declared length; wantDelivered is how many of its
		// bytes the check reads.
		body          []byte
		wantDelivered int64
		// stallAfter, when set, is the part of the body served before it stalls.
		stallAfter string
		want       bool
	}{
		"success: complete body is an answering proxy": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[]}`))
			},
			want: true,
		},
		"success: body exactly at the size bound is an answering proxy": {
			body:          paddedModelsBody(t, proxyModelsBodyLimit),
			want:          true,
			wantDelivered: proxyModelsBodyLimit,
		},
		"error: body one byte over the size bound": {
			body:          paddedModelsBody(t, proxyModelsBodyLimit+1),
			wantDelivered: proxyModelsBodyLimit + 1,
		},
		"error: body far over the size bound is read only one byte past it": {
			body:          paddedModelsBody(t, 4*proxyModelsBodyLimit),
			wantDelivered: proxyModelsBodyLimit + 1,
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
			stallAfter: `{"data":[`,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.body != nil {
				var delivered atomic.Int64
				got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
					ProxyBinary:    fakeProxyBinary(t),
					ProxyModelsURL: "http://proxy-models.test/v1/models",
					Client:         &http.Client{Transport: memoryTransport{body: tt.body, delivered: &delivered}},
				})
				want := readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: tt.want}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("ProbeProxyAndAuth mismatch (-want +got):\n%s", diff)
				}
				if n := delivered.Load(); n != tt.wantDelivered {
					t.Errorf("the check read %d bytes of a %d-byte body, want %d", n, len(tt.body), tt.wantDelivered)
				}
				return
			}
			if tt.stallAfter != "" {
				synctest.Test(t, func(t *testing.T) {
					entered := make(chan struct{})
					got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
						ProxyBinary:    fakeProxyBinary(t),
						ProxyModelsURL: "http://proxy-models.test/v1/models",
						Client:         &http.Client{Transport: stallingTransport{prefix: []byte(tt.stallAfter), entered: entered}},
					})
					select {
					case <-entered:
					default:
						t.Fatal("test precondition: the check never read past the delivered part of the body")
					}
					want := readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: tt.want}
					if diff := gocmp.Diff(want, got); diff != "" {
						t.Errorf("ProbeProxyAndAuth mismatch (-want +got):\n%s", diff)
					}
				})
				return
			}
			srv := httptest.NewServer(http.HandlerFunc(tt.handler))
			t.Cleanup(srv.Close)
			got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
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

// memoryTransport answers every request with HTTP 200 headers, no declared length and body, served
// from memory, and adds every body byte the caller reads to delivered.
type memoryTransport struct {
	body      []byte
	delivered *atomic.Int64
}

func (m memoryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		ContentLength: -1,
		Body:          &countingBody{rest: m.body, delivered: m.delivered},
		Request:       req,
	}, nil
}

type countingBody struct {
	rest      []byte
	delivered *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	if len(b.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	b.delivered.Add(int64(n))
	return n, nil
}

func (b *countingBody) Close() error { return nil }

// stallingTransport answers every request with HTTP 200 headers and a body that yields prefix, then
// closes entered and blocks until the request's context ends.
type stallingTransport struct {
	prefix  []byte
	entered chan struct{}
}

func (s stallingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		ContentLength: -1,
		Body:          &stallingBody{ctx: req.Context(), rest: s.prefix, entered: s.entered},
		Request:       req,
	}, nil
}

type stallingBody struct {
	ctx     context.Context
	rest    []byte
	entered chan struct{}
	once    sync.Once
}

func (b *stallingBody) Read(p []byte) (int, error) {
	if len(b.rest) > 0 {
		n := copy(p, b.rest)
		b.rest = b.rest[n:]
		return n, nil
	}
	b.once.Do(func() { close(b.entered) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *stallingBody) Close() error { return nil }

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
