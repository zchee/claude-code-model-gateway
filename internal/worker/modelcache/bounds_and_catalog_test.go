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
	json "encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	// cacheFileBound is the largest models.json, Claude global config or discovery cache read as present,
	// and the largest proxy listing body read as a listing.
	cacheFileBound = 16 << 20
	// staleCatalogNow is a clock reading more than an hour after the seeded catalog's update time.
	staleCatalogNow = 1_800_000_000_000
	// seededCatalog is a valid saved Anthropic catalog that is stale at staleCatalogNow.
	seededCatalog = `{"data":[{"id":"claude-cached-1","display_name":"Cached One"}],"updatedAt":1}`
)

// padJSON returns doc followed by spaces up to size bytes. Trailing whitespace keeps the document
// valid JSON, so only a size bound can refuse the result.
func padJSON(t *testing.T, doc string, size int) []byte {
	t.Helper()
	if len(doc) > size {
		t.Fatalf("test precondition: a %d-byte document does not fit in %d bytes", len(doc), size)
	}
	out := make([]byte, size)
	copy(out, doc)
	for i := len(doc); i < size; i++ {
		out[i] = ' '
	}
	return out
}

// sequencedListing serves the proxy's GET /v1/models from memory: the n-th request gets bodies[n],
// and requests past the end get the last body. delivered[n] counts the body bytes the n-th reader read.
type sequencedListing struct {
	bodies [][]byte

	mu        sync.Mutex
	delivered []*atomic.Int64
}

func (s *sequencedListing) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path != "/v1/models" {
		return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: req}, nil
	}
	s.mu.Lock()
	n := min(len(s.delivered), len(s.bodies)-1)
	counter := &atomic.Int64{}
	s.delivered = append(s.delivered, counter)
	s.mu.Unlock()
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		ContentLength: -1,
		Body:          &countedListingBody{rest: s.bodies[n], delivered: counter},
		Request:       req,
	}, nil
}

func (s *sequencedListing) deliveredBytes() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.delivered))
	for _, c := range s.delivered {
		out = append(out, c.Load())
	}
	return out
}

type countedListingBody struct {
	rest      []byte
	delivered *atomic.Int64
}

func (b *countedListingBody) Read(p []byte) (int, error) {
	if len(b.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	b.delivered.Add(int64(n))
	return n, nil
}

func (b *countedListingBody) Close() error { return nil }

// responseRowIDs returns the IDs of a /v1/models response body in order.
func responseRowIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var listing struct {
		Data []idDisplay `json:"data"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		t.Fatalf("unmarshal the model listing: %v", err)
	}
	ids := make([]string, 0, len(listing.Data))
	for _, row := range listing.Data {
		ids = append(ids, row.ID)
	}
	return ids
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

func stateDirOf(t *testing.T, resolver paths.Resolver) string {
	t.Helper()
	dir, err := resolver.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	return dir
}

// TestListingSizeBound answers the proxy listing with a valid listing padded to the size bound or past
// it. A listing at the bound is read; a larger one is unreadable, so the refresh keeps the last table it
// read, or before any table falls back to models.json and then the built-in list. The reader stops one
// byte past the bound.
func TestListingSizeBound(t *testing.T) {
	const (
		kimiK3 = `{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`
		kimiK4 = `{"data":[{"id":"kimi-k4","display_name":"kimi-k4 (kimi)"}]}`
	)
	tests := map[string]struct {
		bodies        func(t *testing.T) [][]byte
		modelsJSON    string
		wantSource    string
		wantIDs       []string
		wantAbsent    []string
		wantDelivered []int64
	}{
		"success: listing exactly at the bound is read": {
			bodies:        func(t *testing.T) [][]byte { return [][]byte{padJSON(t, kimiK3, cacheFileBound)} },
			wantSource:    "proxy",
			wantIDs:       []string{"claude-kimi-k3"},
			wantDelivered: []int64{cacheFileBound},
		},
		"error: listing one byte over the bound before any table falls back to models.json": {
			bodies:        func(t *testing.T) [][]byte { return [][]byte{padJSON(t, kimiK3, cacheFileBound+1)} },
			modelsJSON:    `["gpt-6.1-sol"]`,
			wantSource:    "models.json",
			wantIDs:       []string{"claude-gpt-6.1-sol[1m]"},
			wantAbsent:    []string{"claude-kimi-k3"},
			wantDelivered: []int64{cacheFileBound + 1},
		},
		"error: listing one byte over the bound with no models.json falls back to the built-in list": {
			bodies:        func(t *testing.T) [][]byte { return [][]byte{padJSON(t, kimiK3, cacheFileBound+1)} },
			wantSource:    "fallback",
			wantAbsent:    []string{"claude-kimi-k3"},
			wantDelivered: []int64{cacheFileBound + 1},
		},
		"error: listing far over the bound is read only one byte past it": {
			bodies:        func(t *testing.T) [][]byte { return [][]byte{padJSON(t, kimiK3, 2*cacheFileBound)} },
			wantSource:    "fallback",
			wantAbsent:    []string{"claude-kimi-k3"},
			wantDelivered: []int64{cacheFileBound + 1},
		},
		"error: listing over the bound after a readable one keeps the last table": {
			bodies: func(t *testing.T) [][]byte {
				return [][]byte{[]byte(kimiK3), padJSON(t, kimiK4, cacheFileBound+1)}
			},
			wantSource:    "proxy",
			wantIDs:       []string{"claude-kimi-k3"},
			wantAbsent:    []string{"claude-kimi-k4"},
			wantDelivered: []int64{int64(len(kimiK3)), cacheFileBound + 1},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, nil)
			if tt.modelsJSON != "" {
				writeTestFile(t, filepath.Join(stateDirOf(t, resolver), "models.json"), []byte(tt.modelsJSON))
			}
			listing := &sequencedListing{bodies: tt.bodies(t)}
			cache, err := modelcache.New(modelcache.Options{
				Lookup:         lookup,
				Paths:          resolver,
				ProxyBaseURL:   "http://proxy-listing.test",
				DefaultBaseURL: "http://127.0.0.1:19999",
				Client:         &http.Client{Transport: listing},
				Routes:         &route.Store{},
				Sentry:         &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for range len(listing.bodies) {
				if err := cache.Refresh(t.Context()); err != nil {
					t.Fatalf("Refresh: %v", err)
				}
			}
			snap, _ := cache.Snapshot()
			source, raw := snap.Catalog()
			if source != tt.wantSource {
				t.Errorf("catalog source = %q, want %q", source, tt.wantSource)
			}
			ids := catalogRowIDs(t, raw)
			for _, id := range tt.wantIDs {
				if !slices.Contains(ids, id) {
					t.Errorf("advertised IDs %q lack %q", ids, id)
				}
			}
			for _, id := range tt.wantAbsent {
				if slices.Contains(ids, id) {
					t.Errorf("advertised IDs %q include %q from a listing over the bound", ids, id)
				}
			}
			if diff := gocmp.Diff(tt.wantDelivered, listing.deliveredBytes()); diff != "" {
				t.Errorf("listing body bytes read per request mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBoundedReadsSizeBoundary pads each of the three bounded files with trailing whitespace to the
// bound or one byte past it. A file at the bound is read; a larger regular file is treated as absent.
func TestBoundedReadsSizeBoundary(t *testing.T) {
	tests := map[string]struct {
		run  func(t *testing.T, size int) string
		size int
		want string
	}{
		"success: models.json at the bound stands in for the listing": {
			run:  modelsJSONAtSize,
			size: cacheFileBound,
			want: "models.json",
		},
		"error: models.json one byte over the bound is absent": {
			run:  modelsJSONAtSize,
			size: cacheFileBound + 1,
			want: "fallback",
		},
		"success: Claude global config at the bound supplies its older rows": {
			run:  globalConfigAtSize,
			size: cacheFileBound,
			want: "previous row listed",
		},
		"error: Claude global config one byte over the bound is absent": {
			run:  globalConfigAtSize,
			size: cacheFileBound + 1,
			want: "previous row not listed",
		},
		"success: discovery cache at the bound is compared and left unchanged": {
			run:  discoveryCacheAtSize,
			size: cacheFileBound,
			want: "unchanged",
		},
		"error: discovery cache one byte over the bound is absent and rewritten": {
			run:  discoveryCacheAtSize,
			size: cacheFileBound + 1,
			want: "wrote",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.run(t, tt.size); got != tt.want {
				t.Errorf("outcome with a %d-byte file = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}

// newBoundedReadCache builds a cache whose proxy is unreachable and whose discovery cache is wired.
func newBoundedReadCache(t *testing.T, resolver paths.Resolver, opts modelcache.Options) *modelcache.Cache {
	t.Helper()
	opts.Paths = resolver
	opts.ProxyBaseURL = "http://127.0.0.1:19991"
	opts.DefaultBaseURL = discoveryGatewayURL
	opts.EffectiveBaseURL = baseURLFunc(discoveryGatewayURL)
	opts.Client = newFailingClient()
	opts.Clock = func() time.Time { return time.UnixMilli(staleCatalogNow) }
	opts.Routes = &route.Store{}
	opts.Sentry = &sentry.State{}
	cache, err := modelcache.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cache
}

func modelsJSONAtSize(t *testing.T, size int) string {
	_, lookup, resolver := newTestPaths(t, nil)
	writeTestFile(t, filepath.Join(stateDirOf(t, resolver), "models.json"), padJSON(t, `["gpt-6.1-sol"]`, size))
	cache := newBoundedReadCache(t, resolver, modelcache.Options{Lookup: lookup})
	if err := cache.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	source, _ := cache.HealthCatalog()
	return source
}

func globalConfigAtSize(t *testing.T, size int) string {
	const previousID = "claude-opus-4"
	_, lookup, resolver := newTestPaths(t, nil)
	configPath, err := resolver.GlobalConfigPath()
	if err != nil {
		t.Fatalf("GlobalConfigPath: %v", err)
	}
	writeTestFile(t, configPath, padJSON(t, `{"additionalModelOptionsCache":[{"value":"`+previousID+`"}]}`, size))
	cache := newBoundedReadCache(t, resolver, modelcache.Options{
		Lookup:      lookup,
		CurrentPins: pinsFunc(map[string]string{"opus": "claude-opus-5-5[1m]"}),
	})
	body, err := cache.ModelsResponse(t.Context(), http.Header{})
	if err != nil {
		t.Fatalf("ModelsResponse: %v", err)
	}
	if slices.Contains(responseRowIDs(t, body), previousID) {
		return "previous row listed"
	}
	return "previous row not listed"
}

func discoveryCacheAtSize(t *testing.T, size int) string {
	_, lookup, resolver := newTestPaths(t, nil)
	cache := newBoundedReadCache(t, resolver, modelcache.Options{Lookup: lookup})
	first, err := cache.SyncDiscoveryCache(true)
	if err != nil || first.State != "wrote" {
		t.Fatalf("test precondition: first discovery sync = (%+v, %v), want wrote", first, err)
	}
	written, err := os.ReadFile(first.CachePath)
	if err != nil {
		t.Fatalf("read the discovery cache: %v", err)
	}
	writeTestFile(t, first.CachePath, padJSON(t, string(written), size))
	second, err := cache.SyncDiscoveryCache(false)
	if err != nil {
		t.Fatalf("SyncDiscoveryCache(false): %v", err)
	}
	if second.State == "wrote" {
		if after, err := os.ReadFile(first.CachePath); err != nil || len(after) != len(written) {
			t.Errorf("rewritten discovery cache = %d bytes (err %v), want the %d-byte document", len(after), err, len(written))
		}
	}
	return second.State
}

// TestCatalogRefreshFailureMessages seeds a stale saved catalog and answers its refresh with a page
// that has no data array or with a transport failure. The listing serves the cached rows, and the
// saved catalog keeps them with the failure's classified message. The refused connection comes from
// a dialer that answers ECONNREFUSED inside a real HTTP transport, so no port is dialled.
func TestCatalogRefreshFailureMessages(t *testing.T) {
	tests := map[string]struct {
		page      string
		transport http.RoundTripper
		refuse    bool
		want      savedCatalog
	}{
		"error: a JSON null page is invalid catalog data": {
			page: `null`,
			want: savedCatalog{IDs: []string{"claude-cached-1"}, Error: "invalid catalog data"},
		},
		"error: a page whose data is null is invalid catalog data": {
			page: `{"data":null,"has_more":false}`,
			want: savedCatalog{IDs: []string{"claude-cached-1"}, Error: "invalid catalog data"},
		},
		"error: a page with no data member is invalid catalog data": {
			page: `{"has_more":false}`,
			want: savedCatalog{IDs: []string{"claude-cached-1"}, Error: "invalid catalog data"},
		},
		"error: a transport failure is a catalog request failure": {
			transport: failingTransport{err: io.ErrUnexpectedEOF},
			want:      savedCatalog{IDs: []string{"claude-cached-1"}, Error: "catalog request failed"},
		},
		"error: a refused connection is a catalog request failure": {
			refuse: true,
			want:   savedCatalog{IDs: []string{"claude-cached-1"}, Error: "catalog request failed"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var pageHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				pageHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.page))
			}))
			t.Cleanup(upstream.Close)
			client := newTestClient(t)
			if tt.transport != nil {
				client = &http.Client{Transport: tt.transport}
			}
			var refusedDials atomic.Int32
			if tt.refuse {
				tr := &http.Transport{
					Proxy: nil,
					DialContext: func(context.Context, string, string) (net.Conn, error) {
						refusedDials.Add(1)
						return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
					},
				}
				t.Cleanup(tr.CloseIdleConnections)
				client = &http.Client{Transport: tr}
			}
			baseURL := upstream.URL

			_, lookup, resolver := newTestPaths(t, nil)
			catalogPath := filepath.Join(stateDirOf(t, resolver), "anthropic-models.json")
			writeTestFile(t, catalogPath, []byte(seededCatalog))
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: baseURL,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           client,
				Clock:            func() time.Time { return time.UnixMilli(staleCatalogNow) },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			body, err := cache.ModelsResponse(t.Context(), http.Header{"Authorization": {authBearerHeader}})
			cache.Wait()
			if err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			if ids := responseRowIDs(t, body); !slices.Contains(ids, "claude-cached-1") {
				t.Errorf("listing during the refresh = %q, want the cached row claude-cached-1", ids)
			}
			if diff := gocmp.Diff(tt.want, readSavedCatalog(t, catalogPath)); diff != "" {
				t.Errorf("saved catalog after the failed refresh mismatch (-want +got):\n%s", diff)
			}
			if tt.page != "" && pageHits.Load() != 1 {
				t.Errorf("test precondition: the upstream answered %d page request(s), want 1", pageHits.Load())
			}
			if tt.refuse {
				if got := refusedDials.Load(); got < 1 {
					t.Errorf("test precondition: the refusing dialer ran %d time(s), want at least 1", got)
				}
				if got := pageHits.Load(); got != 0 {
					t.Errorf("the upstream answered %d page request(s) through a refused connection, want 0", got)
				}
			}
		})
	}
}

// TestCatalogRequestHeaderMerge hands the catalog refresh caller-built header maps with repeated
// values and with one name in several spellings. Names are sorted before lowercasing and the last
// spelling in that order wins; within it authorization and user-agent keep their first value and the
// other allowed names join theirs with ", ". The default API version applies only without an override.
func TestCatalogRequestHeaderMerge(t *testing.T) {
	allowed := []string{"Authorization", "X-Api-Key", "Anthropic-Version", "Anthropic-Beta", "Anthropic-Workspace-Id", "User-Agent"}
	tests := map[string]struct {
		incoming http.Header
		want     map[string][]string
	}{
		"success: repeated values merge into one line, authorization and user-agent keep the first": {
			incoming: http.Header{
				"Authorization":          {"Bearer first-merge-marker", "Bearer second-merge-marker"},
				"Anthropic-Beta":         {"beta-a", "beta-b", "beta-c"},
				"Anthropic-Workspace-Id": {"workspace-a", "workspace-b"},
				"X-Api-Key":              {"key-a", "key-b"},
				"User-Agent":             {"agent-first", "agent-second"},
			},
			want: map[string][]string{
				"Authorization":          {"Bearer first-merge-marker"},
				"X-Api-Key":              {"key-a, key-b"},
				"Anthropic-Version":      {"2023-06-01"},
				"Anthropic-Beta":         {"beta-a, beta-b, beta-c"},
				"Anthropic-Workspace-Id": {"workspace-a, workspace-b"},
				"User-Agent":             {"agent-first"},
			},
		},
		"success: of several spellings of one name the last in byte order wins": {
			incoming: http.Header{
				"AUTHORIZATION":  {"Bearer upper-merge-marker"},
				"Authorization":  {"Bearer canonical-merge-marker"},
				"authorization":  {"Bearer lower-merge-marker"},
				"ANTHROPIC-BETA": {"beta-upper"},
				"Anthropic-Beta": {"beta-canonical"},
				"anthropic-beta": {"beta-lower"},
				"user-agent":     {"agent-lower"},
				"User-Agent":     {"agent-canonical"},
			},
			want: map[string][]string{
				"Authorization":     {"Bearer lower-merge-marker"},
				"Anthropic-Version": {"2023-06-01"},
				"Anthropic-Beta":    {"beta-lower"},
				"User-Agent":        {"agent-lower"},
			},
		},
		"success: of spellings that are not all lowercase the last in byte order wins": {
			incoming: http.Header{
				"AUTHORIZATION":  {"Bearer upper-merge-marker"},
				"Authorization":  {"Bearer canonical-merge-marker"},
				"Anthropic-Beta": {"beta-canonical"},
				"Anthropic-beta": {"beta-mixed"},
				"User-Agent":     {"agent-canonical"},
				"User-agent":     {"agent-mixed"},
				"X-API-KEY":      {"key-upper"},
				"X-Api-KEY":      {"key-mixed"},
			},
			want: map[string][]string{
				"Authorization":     {"Bearer canonical-merge-marker"},
				"X-Api-Key":         {"key-mixed"},
				"Anthropic-Version": {"2023-06-01"},
				"Anthropic-Beta":    {"beta-mixed"},
				"User-Agent":        {"agent-mixed"},
			},
		},
		"success: the winning spelling's repeated values merge without the other spellings' values": {
			incoming: http.Header{
				"Authorization":  {"Bearer canonical-merge-marker"},
				"authorization":  {"Bearer lower-first-marker", "Bearer lower-second-marker"},
				"Anthropic-Beta": {"beta-canonical-a", "beta-canonical-b"},
				"anthropic-beta": {"beta-lower-a", "beta-lower-b"},
			},
			want: map[string][]string{
				"Authorization":     {"Bearer lower-first-marker"},
				"Anthropic-Version": {"2023-06-01"},
				"Anthropic-Beta":    {"beta-lower-a, beta-lower-b"},
			},
		},
		"success: an incoming version overrides the default and its spellings collide like any name": {
			incoming: http.Header{
				"x-api-key":         {"key-lower"},
				"Anthropic-Version": {"2000-01-01"},
				"anthropic-version": {"2099-12-31", "2098-01-01"},
			},
			want: map[string][]string{
				"X-Api-Key":         {"key-lower"},
				"Anthropic-Version": {"2099-12-31, 2098-01-01"},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []http.Header
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				mu.Lock()
				seen = append(seen, r.Header.Clone())
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-merge-1","display_name":"Merge One"}],"has_more":false}`))
			}))
			t.Cleanup(upstream.Close)

			_, lookup, resolver := newTestPaths(t, nil)
			cache, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            resolver,
				ProxyBaseURL:     "http://127.0.0.1:19991",
				AnthropicBaseURL: upstream.URL,
				DefaultBaseURL:   "http://127.0.0.1:19999",
				Client:           newTestClient(t),
				Clock:            func() time.Time { return time.UnixMilli(staleCatalogNow) },
				Routes:           &route.Store{},
				Sentry:           &sentry.State{},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = cache.ModelsResponse(t.Context(), tt.incoming)
			cache.Wait()
			if err != nil {
				t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
			}
			mu.Lock()
			requests := slices.Clone(seen)
			mu.Unlock()
			if len(requests) != 1 {
				t.Fatalf("the upstream received %d catalog request(s), want 1", len(requests))
			}
			got := map[string][]string{}
			for _, name := range allowed {
				if values, ok := requests[0][name]; ok {
					got[name] = values
				}
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("catalog request headers mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDiscoveryCacheReplacementIsPrivate writes the discovery cache over no file and over files with
// wider or equal permissions: the replaced file is private to its owner. It checks the plain mode under
// the test process's own creation mask, which it does not change: the expected mode is the one a new
// file created with 0600 receives in the cache's directory, which is 0600 under any mask that leaves the
// owner's bits alone. Under such a mask a final 0600 cannot tell whether the destination name was also
// changed with chmod. TestDiscoveryCacheModeFollowsCreationMask in the discoverymodetest package decides
// that, by writing and replacing the cache in child processes under masks that remove owner bits.
func TestDiscoveryCacheReplacementIsPrivate(t *testing.T) {
	tests := map[string]struct {
		// prior is the mode of a stale file present before the write; zero means no file.
		prior os.FileMode
	}{
		"success: a new discovery cache is private": {},
		"success: replacing a world-readable discovery cache leaves it private": {
			prior: 0o644,
		},
		"success: replacing a world-writable discovery cache leaves it private": {
			prior: 0o666,
		},
		"success: replacing a private discovery cache keeps it private": {
			prior: 0o600,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, nil)
			cachePath, err := resolver.DiscoveryCachePath()
			if err != nil {
				t.Fatalf("DiscoveryCachePath: %v", err)
			}
			if tt.prior != 0 {
				writeTestFile(t, cachePath, []byte(`{"baseUrl":"stale","fetchedAt":0,"models":[]}`))
				// An explicit chmod sets bits the process umask would clear at creation.
				if err := os.Chmod(cachePath, tt.prior); err != nil {
					t.Fatalf("chmod the stale discovery cache: %v", err)
				}
				info, err := os.Stat(cachePath)
				if err != nil {
					t.Fatalf("test precondition: stat the stale discovery cache: %v", err)
				}
				if info.Mode().Perm() != tt.prior {
					t.Fatalf("test precondition: stale discovery cache mode = %v, want %v", info.Mode().Perm(), tt.prior)
				}
			}
			cache := newBoundedReadCache(t, resolver, modelcache.Options{Lookup: lookup})
			result, err := cache.SyncDiscoveryCache(true)
			if err != nil || result.State != "wrote" {
				t.Fatalf("SyncDiscoveryCache(true) = (%+v, %v), want wrote", result, err)
			}
			info, err := os.Lstat(cachePath)
			if err != nil {
				t.Fatalf("stat the discovery cache: %v", err)
			}
			want := privateCreateMode(t, filepath.Dir(cachePath))
			if !info.Mode().IsRegular() || info.Mode().Perm() != want {
				t.Errorf("discovery cache after replacement = %v, want a regular file with mode %v", info.Mode(), want)
			}
			if want&0o400 == 0 {
				// The ambient mask removes the owner's read bit, so the content cannot be read back.
				return
			}
			var doc struct {
				BaseURL string `json:"baseUrl"`
			}
			raw, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatalf("read the discovery cache: %v", err)
			}
			if err := json.Unmarshal(raw, &doc); err != nil || doc.BaseURL != discoveryGatewayURL {
				t.Errorf("discovery cache baseUrl = %q (err %v), want %q", doc.BaseURL, err, discoveryGatewayURL)
			}
		})
	}
}

// privateCreateMode returns the permission bits a new file created with mode 0600 in dir receives
// under the process's current creation mask. The probe file is removed before it returns.
func privateCreateMode(t *testing.T, dir string) os.FileMode {
	t.Helper()
	probe, err := os.CreateTemp(dir, "mode-probe-*")
	if err != nil {
		t.Fatalf("create a mode probe in %s: %v", dir, err)
	}
	name := probe.Name()
	defer func() {
		if err := os.Remove(name); err != nil {
			t.Errorf("remove the mode probe: %v", err)
		}
	}()
	if err := probe.Close(); err != nil {
		t.Fatalf("close the mode probe: %v", err)
	}
	info, err := os.Lstat(name)
	if err != nil {
		t.Fatalf("stat the mode probe: %v", err)
	}
	return info.Mode().Perm()
}
