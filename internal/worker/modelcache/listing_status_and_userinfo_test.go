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

package modelcache_test

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// stalledListing answers the proxy listing with status and a body that delivers prefix and then
// blocks until the request's context ends.
type stalledListing struct {
	status int
	prefix string
}

func (s stalledListing) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode:    s.status,
		Header:        http.Header{"Content-Type": {"application/json"}},
		ContentLength: -1,
		Body:          &stalledListingBody{ctx: req.Context(), rest: []byte(s.prefix)},
		Request:       req,
	}, nil
}

type stalledListingBody struct {
	ctx  context.Context
	rest []byte
}

func (b *stalledListingBody) Read(p []byte) (int, error) {
	if len(b.rest) > 0 {
		n := copy(p, b.rest)
		b.rest = b.rest[n:]
		return n, nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *stalledListingBody) Close() error { return nil }

// TestListingReturnsOnNon200BeforeBodyEnds answers the proxy listing with a status other than 200 and
// a body that never ends. The refresh treats the listing as unread as soon as the status arrives, in
// no virtual time, rather than waiting for the body until the listing deadline. A 200 whose body
// stalls the same way is read until that deadline, which shows the body really stalls.
func TestListingReturnsOnNon200BeforeBodyEnds(t *testing.T) {
	// listingDeadline restates modelcache.ProxyListingTimeout: how long a listing is read before it
	// counts as unread.
	const listingDeadline = 2500 * time.Millisecond
	tests := map[string]struct {
		status      int
		wantElapsed time.Duration
	}{
		"success: a 503 listing is unread without waiting for its body": {
			status: http.StatusServiceUnavailable,
		},
		"success: a 404 listing is unread without waiting for its body": {
			status: http.StatusNotFound,
		},
		"error: a 200 listing whose body stalls is read until the listing deadline": {
			status:      http.StatusOK,
			wantElapsed: listingDeadline,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, lookup, resolver := newTestPaths(t, nil)
				cache, err := modelcache.New(modelcache.Options{
					Lookup:         lookup,
					Paths:          resolver,
					ProxyBaseURL:   "http://127.0.0.1:19991",
					DefaultBaseURL: discoveryGatewayURL,
					Client:         &http.Client{Transport: stalledListing{status: tt.status, prefix: `{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}`}},
					Clock:          time.Now,
					Routes:         &route.Store{},
					Sentry:         &sentry.State{},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				defer cache.Wait()
				start := time.Now()
				if err := cache.Refresh(t.Context()); err != nil {
					t.Fatalf("Refresh: %v", err)
				}
				if elapsed := time.Since(start); elapsed != tt.wantElapsed {
					t.Errorf("refresh took %v of virtual time, want %v", elapsed, tt.wantElapsed)
				}
				if source, _ := cache.HealthCatalog(); source != "fallback" {
					t.Errorf("catalog source after the refresh = %q, want %q", source, "fallback")
				}
			})
		})
	}
}

// TestListingRefusesRedirectWithUserinfo sends the proxy listing through redirects whose target
// carries userinfo, on the first redirect or a later one, to the same host or another. The listing
// never requests such a target, which would turn the userinfo into an Authorization header, so the
// refresh keeps the built-in catalog although the target would answer with a listing. The same target
// without userinfo is followed and read.
func TestListingRefusesRedirectWithUserinfo(t *testing.T) {
	tests := map[string]struct {
		// first is where /v1/models redirects; /hop redirects to final.
		first, final string
		wantPaths    []string
		wantSource   string
	}{
		"success: the same target without userinfo is followed": {
			first:      "http://127.0.0.1:19991/final",
			wantPaths:  []string{"/v1/models", "/final"},
			wantSource: "proxy",
		},
		"error: a first redirect to the same host with userinfo is refused": {
			first:      "http://listing-user:listing-pass@127.0.0.1:19991/final",
			wantPaths:  []string{"/v1/models"},
			wantSource: "fallback",
		},
		"error: a first redirect to another host with userinfo is refused": {
			first:      "http://listing-user:listing-pass@listing-elsewhere.test/final",
			wantPaths:  []string{"/v1/models"},
			wantSource: "fallback",
		},
		"error: a later redirect with userinfo is refused": {
			first:      "/hop",
			final:      "http://listing-user:listing-pass@127.0.0.1:19991/final",
			wantPaths:  []string{"/v1/models", "/hop"},
			wantSource: "fallback",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var paths, authorized []string
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				if _, ok := r.Header["Authorization"]; ok {
					authorized = append(authorized, r.URL.Path)
				}
				mu.Unlock()
				switch r.URL.Path {
				case "/v1/models":
					w.Header().Set("Location", tt.first)
					w.WriteHeader(http.StatusFound)
				case "/hop":
					w.Header().Set("Location", tt.final)
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "/final":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			_, lookup, resolver := newTestPaths(t, nil)
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   "http://127.0.0.1:19991",
				DefaultBaseURL: discoveryGatewayURL,
				Client:         &http.Client{Transport: handlerTransport{handler: handler}},
				Clock:          time.Now,
				Routes:         &route.Store{},
				Sentry:         &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer cache.Wait()
			if err := cache.Refresh(t.Context()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
			if source, _ := cache.HealthCatalog(); source != tt.wantSource {
				t.Errorf("catalog source after the refresh = %q, want %q", source, tt.wantSource)
			}
			mu.Lock()
			gotPaths, gotAuthorized := slices.Clone(paths), slices.Clone(authorized)
			mu.Unlock()
			if diff := gocmp.Diff(tt.wantPaths, gotPaths); diff != "" {
				t.Errorf("listing request paths mismatch (-want +got):\n%s", diff)
			}
			if len(gotAuthorized) != 0 {
				t.Errorf("listing requests carrying Authorization = %q, want none", gotAuthorized)
			}
		})
	}
}
