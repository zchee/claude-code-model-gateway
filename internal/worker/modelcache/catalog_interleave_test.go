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
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/anthropiccatalog"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// catalogTestBase is an Anthropic base URL the in-memory transports answer.
const catalogTestBase = "http://anthropic.test"

// savedCatalog is the part of anthropic-models.json these tests compare.
type savedCatalog struct {
	IDs   []string
	Error string
}

func readSavedCatalog(t *testing.T, path string) savedCatalog {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read anthropic-models.json: %v", err)
	}
	var doc struct {
		Data  []idDisplay `json:"data"`
		Error *string     `json:"error"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal anthropic-models.json: %v", err)
	}
	saved := savedCatalog{IDs: []string{}}
	for _, row := range doc.Data {
		saved.IDs = append(saved.IDs, row.ID)
	}
	if doc.Error != nil {
		saved.Error = *doc.Error
	}
	return saved
}

// TestStaleCatalogReadNeverOverwritesNewerRows parks a caller right after it read a stale catalog,
// lets another caller refresh the catalog to newer rows or wait behind the parked one, then releases
// the first caller while every further catalog fetch fails. The newer rows stay on disk and no
// redundant fetch starts.
func TestStaleCatalogReadNeverOverwritesNewerRows(t *testing.T) {
	tests := map[string]struct {
		newerRows string
		wantIDs   []string
	}{
		"success: failed fetch after a newer refresh keeps the newer rows": {
			newerRows: `{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}],"has_more":false}`,
			wantIDs:   []string{"claude-opus-5-5"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, lookup, resolver := newTestPaths(t, nil)
				stateDir, err := resolver.StateDir()
				if err != nil {
					t.Fatalf("StateDir: %v", err)
				}
				path := filepath.Join(stateDir, "anthropic-models.json")
				now := time.UnixMilli(1_900_000_000_000)
				stale := []byte(`[{"id":"claude-sonnet-4-0","display_name":"Claude Sonnet 4"}]`)
				if err := anthropiccatalog.WriteSuccess(path, stale, now.Add(-2*time.Hour)); err != nil {
					t.Fatalf("seed the stale catalog: %v", err)
				}

				var parkNext atomic.Bool
				parked := make(chan struct{})
				releaseClock := make(chan struct{})
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(releaseClock) }) }
				defer release()
				clock := func() time.Time {
					if parkNext.CompareAndSwap(true, false) {
						close(parked)
						select {
						case <-releaseClock:
						case <-time.After(time.Minute):
							t.Error("the parked clock read was never released")
						}
					}
					return now
				}
				var pages atomic.Int32
				handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if pages.Add(1) > 1 {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tt.newerRows))
				})
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://127.0.0.1:19991",
					AnthropicBaseURL: catalogTestBase,
					DefaultBaseURL:   "http://127.0.0.1:19999",
					Client:           &http.Client{Transport: handlerTransport{handler: handler}},
					Clock:            clock,
					Routes:           &route.Store{},
					Sentry:           &sentry.State{},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				credentials := http.Header{"X-Api-Key": {apiKeyMarker}}

				parkNext.Store(true)
				staleDone := make(chan struct{})
				go func() {
					defer close(staleDone)
					cache.TriggerAnthropicRefresh(credentials)
				}()
				awaitSignal(t, parked, "the first caller to park in its clock read")

				newerDone := make(chan struct{})
				go func() {
					defer close(newerDone)
					triggerBehindParkedCaller(cache, credentials)
				}()
				// The second caller either finishes its own refresh, or waits on the lock the parked caller
				// holds; only then is the parked caller released.
				newerReturned, ok := doneOrLockWait(newerDone, "modelcache_test.triggerBehindParkedCaller")
				if !ok {
					t.Fatal("the second caller neither returned nor waited behind the parked caller")
				}
				if newerReturned {
					cache.Wait()
				}

				release()
				awaitSignal(t, staleDone, "the parked caller to return")
				cache.Wait()
				if !newerReturned {
					awaitSignal(t, newerDone, "the second caller to return")
					cache.Wait()
				}

				got := readSavedCatalog(t, path)
				if diff := gocmp.Diff(savedCatalog{IDs: tt.wantIDs}, got); diff != "" {
					t.Errorf("catalog on disk mismatch (-want +got):\n%s", diff)
				}
				if n := pages.Load(); n != 1 {
					t.Errorf("catalog pages fetched = %d, want 1: a caller holding a stale read started a redundant refresh after a newer one finished", n)
				}
			})
		})
	}
}

// triggerBehindParkedCaller is the second caller's entry point; its frame names that goroutine in a
// goroutine dump.
//
//go:noinline
func triggerBehindParkedCaller(cache *modelcache.Cache, credentials http.Header) {
	cache.TriggerAnthropicRefresh(credentials)
}

// TestPreStartCatalogRefreshEndsWithServiceContext starts a catalog refresh before Start, parks its
// page request, then starts and cancels the cache. Wait returns without waiting for the page timeout,
// and the catalog file is not written then or later.
func TestPreStartCatalogRefreshEndsWithServiceContext(t *testing.T) {
	tests := map[string]struct {
		seed []byte
	}{
		"success: cold catalog": {},
		"success: stale catalog": {
			seed: []byte(`[{"id":"claude-sonnet-4-0","display_name":"Claude Sonnet 4"}]`),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, lookup, resolver := newTestPaths(t, map[string]string{"CODEX_GATEWAY_MODELS_REFRESH_MS": "600000"})
				stateDir, err := resolver.StateDir()
				if err != nil {
					t.Fatalf("StateDir: %v", err)
				}
				path := filepath.Join(stateDir, "anthropic-models.json")
				if tt.seed != nil {
					if err := anthropiccatalog.WriteSuccess(path, tt.seed, time.Now().Add(-2*time.Hour)); err != nil {
						t.Fatalf("seed the stale catalog: %v", err)
					}
				}
				pageArrived := make(chan struct{})
				var arriveOnce sync.Once
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Host != "anthropic.test" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					arriveOnce.Do(func() { close(pageArrived) })
					select {
					case <-r.Context().Done():
					case <-time.After(time.Hour):
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}],"has_more":false}`))
					}
				})
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://127.0.0.1:19991",
					AnthropicBaseURL: catalogTestBase,
					DefaultBaseURL:   "http://127.0.0.1:19999",
					Client:           &http.Client{Transport: handlerTransport{handler: handler}},
					Clock:            time.Now,
					Routes:           &route.Store{},
					Sentry:           &sentry.State{},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}

				cache.TriggerAnthropicRefresh(http.Header{"X-Api-Key": {apiKeyMarker}})
				awaitSignal(t, pageArrived, "the pre-Start catalog page request")
				before := readFileOrNil(t, path)

				ctx, cancel := context.WithCancel(t.Context())
				cache.Start(ctx)
				synctest.Wait()
				cancel()
				start := time.Now()
				cache.Wait()
				if elapsed := time.Since(start); elapsed >= time.Second {
					t.Errorf("Wait after cancelling the service context took %v of virtual time, want it bounded by the cancellation (page timeout %v)", elapsed, modelcache.AnthropicPageTimeout)
				}
				if after := readFileOrNil(t, path); !bytes.Equal(before, after) {
					t.Errorf("catalog file changed by the cancelled pre-Start refresh:\nbefore: %s\nafter:  %s", before, after)
				}
				time.Sleep(time.Hour)
				synctest.Wait()
				if later := readFileOrNil(t, path); !bytes.Equal(before, later) {
					t.Errorf("catalog file written after Wait returned:\nbefore: %s\nlater:  %s", before, later)
				}
			})
		})
	}
}

// readFileOrNil returns the file's bytes, or nil when it does not exist.
func readFileOrNil(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// pageTransport answers every catalog page with body; when stall is set it then blocks until the
// request's context ends instead of finishing the body.
type pageTransport struct {
	body  []byte
	stall bool
}

func (p pageTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       &pageBody{ctx: req.Context(), rest: p.body, stall: p.stall},
		Request:    req,
	}, nil
}

type pageBody struct {
	ctx   context.Context
	rest  []byte
	stall bool
}

func (b *pageBody) Read(p []byte) (int, error) {
	if len(b.rest) > 0 {
		n := copy(p, b.rest)
		b.rest = b.rest[n:]
		return n, nil
	}
	if b.stall {
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	return 0, io.EOF
}

func (b *pageBody) Close() error { return nil }

// TestCatalogPageBoundCountsDecodedUnits checks the page bound against the UTF-16 length of the page
// as the decoder decodes it, one replacement per maximal ill-formed subpart, and that a page already
// over the bound is too large even when its body then stalls.
func TestCatalogPageBoundCountsDecodedUnits(t *testing.T) {
	const (
		bound  = modelcache.MaxAnthropicPageUTF16Units
		prefix = `{"data":[{"id":"claude-opus-5-5","display_name":"`
		suffix = `"}],"has_more":false}`
		// truncated is a three-byte sequence cut after two bytes; before an ASCII letter it decodes to
		// one U+FFFD, so each group below is two decoded code units in three bytes.
		truncated = "\xE2\x82"
		groups    = 4096
	)
	malformed := strings.Repeat(truncated+"a", groups)
	filler := bound - len(prefix) - len(suffix) - 2*groups
	tests := map[string]struct {
		body      string
		stall     bool
		wantIDs   []string
		wantError string
	}{
		"success: malformed UTF-8 page whose decoded length is exactly the bound": {
			body:    prefix + malformed + strings.Repeat("b", filler) + suffix,
			wantIDs: []string{"claude-opus-5-5"},
		},
		"error: malformed UTF-8 page one decoded unit over the bound": {
			body:      prefix + malformed + strings.Repeat("b", filler+1) + suffix,
			wantIDs:   []string{},
			wantError: "catalog too large",
		},
		"error: page over the bound that then stalls": {
			body:      prefix + strings.Repeat("b", bound),
			stall:     true,
			wantIDs:   []string{},
			wantError: "catalog too large",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, lookup, resolver := newTestPaths(t, nil)
				stateDir, err := resolver.StateDir()
				if err != nil {
					t.Fatalf("StateDir: %v", err)
				}
				cache, err := modelcache.New(modelcache.Options{
					Lookup:           lookup,
					Paths:            resolver,
					ProxyBaseURL:     "http://127.0.0.1:19991",
					AnthropicBaseURL: catalogTestBase,
					DefaultBaseURL:   "http://127.0.0.1:19999",
					Client:           &http.Client{Transport: pageTransport{body: []byte(tt.body), stall: tt.stall}},
					Clock:            time.Now,
					Routes:           &route.Store{},
					Sentry:           &sentry.State{},
				})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if _, err := cache.ModelsResponse(t.Context(), http.Header{"X-Api-Key": {apiKeyMarker}}); err != nil {
					t.Fatalf("ModelsResponse: %s", redactTestMarkers(err.Error()))
				}
				cache.Wait()
				got := readSavedCatalog(t, filepath.Join(stateDir, "anthropic-models.json"))
				if diff := gocmp.Diff(savedCatalog{IDs: tt.wantIDs, Error: tt.wantError}, got); diff != "" {
					t.Errorf("saved catalog mismatch (-want +got):\n%s", diff)
				}
			})
		})
	}
}
