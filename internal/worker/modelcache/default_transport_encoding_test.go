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
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	// encodingTestListing is a valid proxy listing with one Codex row.
	encodingTestListing = `{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`
	// encodingTestPage is a valid last Anthropic catalog page with one row.
	encodingTestPage = `{"data":[{"id":"claude-encoded-1","display_name":"Encoded One"}],"has_more":false}`
)

// encodedEndpoint answers every request with a fixed body and records the Accept-Encoding field of
// each request it receives.
type encodedEndpoint struct {
	url string

	mu      sync.Mutex
	present []bool
	values  [][]string
}

// startEncodedEndpoint serves wire as every answer's body, with Content-Encoding: gzip when gzipped
// is set, whatever the request asked for.
func startEncodedEndpoint(t *testing.T, wire []byte, gzipped bool) *encodedEndpoint {
	t.Helper()
	endpoint := &encodedEndpoint{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, ok := r.Header["Accept-Encoding"]
		endpoint.mu.Lock()
		endpoint.present = append(endpoint.present, ok)
		endpoint.values = append(endpoint.values, values)
		endpoint.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if gzipped {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
		_, _ = w.Write(wire)
	}))
	t.Cleanup(srv.Close)
	endpoint.url = srv.URL
	return endpoint
}

func (e *encodedEndpoint) requests() ([]bool, [][]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.present), slices.Clone(e.values)
}

func gzipBody(t *testing.T, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(plain)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// encodingOutcome is what one request kind made of the endpoint's answer: the catalog source after a
// listing refresh, or the saved Anthropic catalog after a cold catalog refresh.
type encodingOutcome struct {
	Source string
	Saved  savedCatalog
}

// runEncodingRequest builds a cache with client, points the request kind at endpoint, runs it and
// reports its outcome.
func runEncodingRequest(t *testing.T, kind string, endpoint *encodedEndpoint, client *http.Client) encodingOutcome {
	t.Helper()
	_, lookup, resolver := newTestPaths(t, nil)
	opts := modelcache.Options{
		Lookup:         lookup,
		Paths:          resolver,
		ProxyBaseURL:   "http://127.0.0.1:19991",
		DefaultBaseURL: "http://127.0.0.1:19999",
		Client:         client,
		Clock:          func() time.Time { return time.UnixMilli(staleCatalogNow) },
		Routes:         &route.Store{},
		Sentry:         &sentry.State{},
	}
	if kind == "listing" {
		opts.ProxyBaseURL = endpoint.url
	} else {
		opts.AnthropicBaseURL = endpoint.url
	}
	cache, err := modelcache.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer cache.Wait()
	if kind == "listing" {
		if err := cache.Refresh(t.Context()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		source, _ := cache.HealthCatalog()
		return encodingOutcome{Source: source}
	}
	if _, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}}); err != nil {
		t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
	}
	cache.Wait()
	return encodingOutcome{Saved: readSavedCatalog(t, filepath.Join(stateDirOf(t, resolver), "anthropic-models.json"))}
}

// TestDefaultTransportAsksForNoEncoding sends the proxy listing and an Anthropic catalog page through
// the cache's own transport, which a client without a transport or one naming the default transport
// selects. Neither request carries an Accept-Encoding field, as the source's requests carry none.
func TestDefaultTransportAsksForNoEncoding(t *testing.T) {
	tests := map[string]struct {
		kind   string
		body   string
		client *http.Client
	}{
		"success: a listing through a client without a transport asks for no encoding": {
			kind:   "listing",
			body:   encodingTestListing,
			client: &http.Client{},
		},
		"success: a listing through a client naming the default transport asks for no encoding": {
			kind:   "listing",
			body:   encodingTestListing,
			client: &http.Client{Transport: http.DefaultTransport},
		},
		"success: a catalog page through a client without a transport asks for no encoding": {
			kind:   "catalog",
			body:   encodingTestPage,
			client: &http.Client{},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			endpoint := startEncodedEndpoint(t, []byte(tt.body), false)
			runEncodingRequest(t, tt.kind, endpoint, tt.client)
			present, values := endpoint.requests()
			if len(present) != 1 {
				t.Fatalf("test precondition: the endpoint received %d request(s), want 1", len(present))
			}
			if present[0] {
				t.Errorf("the %s request carried Accept-Encoding %q, want no Accept-Encoding field", tt.kind, values[0])
			}
		})
	}
}

// TestDefaultTransportKeepsCompressedBytes answers the proxy listing and an Anthropic catalog page
// through the cache's own transport with a body marked Content-Encoding: gzip that the request never
// asked for. The cache reads the bytes as sent, as the source does, and a compressed body is not the
// JSON it encodes: the listing is unread, so the built-in catalog stays, and the catalog page is
// invalid JSON. The same answers sent without an encoding through the same fake endpoints are read.
func TestDefaultTransportKeepsCompressedBytes(t *testing.T) {
	tests := map[string]struct {
		kind    string
		body    string
		gzipped bool
		want    encodingOutcome
	}{
		"success: an identity listing is read": {
			kind: "listing",
			body: encodingTestListing,
			want: encodingOutcome{Source: "proxy"},
		},
		"error: a gzip listing is not decoded into a listing": {
			kind:    "listing",
			body:    encodingTestListing,
			gzipped: true,
			want:    encodingOutcome{Source: "fallback"},
		},
		"success: an identity catalog page is saved": {
			kind: "catalog",
			body: encodingTestPage,
			want: encodingOutcome{Saved: savedCatalog{IDs: []string{"claude-encoded-1"}}},
		},
		"error: a gzip catalog page is not decoded into a page": {
			kind:    "catalog",
			body:    encodingTestPage,
			gzipped: true,
			want:    encodingOutcome{Saved: savedCatalog{IDs: []string{}, Error: "invalid catalog JSON"}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			wire := []byte(tt.body)
			if tt.gzipped {
				wire = gzipBody(t, tt.body)
			}
			endpoint := startEncodedEndpoint(t, wire, tt.gzipped)
			got := runEncodingRequest(t, tt.kind, endpoint, &http.Client{})
			if present, _ := endpoint.requests(); len(present) != 1 {
				t.Fatalf("test precondition: the endpoint received %d request(s), want 1", len(present))
			}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("%s outcome mismatch (-want +got):\n%s", tt.kind, diff)
			}
		})
	}
}
