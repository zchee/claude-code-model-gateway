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
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// checkpointContext is a context whose Err, once armed, samples the error it returns and only then
// runs the armed callback. The callback therefore acts between a caller's check of the context and
// whatever the caller does next, as a cancellation that lands right after the check would.
type checkpointContext struct {
	context.Context
	armed atomic.Pointer[func()]
}

// arm makes the next Err call run f after sampling its result.
func (c *checkpointContext) arm(f func()) { c.armed.Store(&f) }

// fired reports whether the armed callback has run.
func (c *checkpointContext) fired() bool { return c.armed.Load() == nil }

func (c *checkpointContext) Err() error {
	err := c.Context.Err()
	if f := c.armed.Swap(nil); f != nil {
		(*f)()
	}
	return err
}

// TestCanceledSupersededRefreshReturnsContextError parks a refresh in its listing request while a
// later-started refresh commits, so the parked refresh is superseded. Its context ends either while it
// is parked or right after its last check of the context. Either way Refresh returns the context's
// error, and the later refresh's generation stays published.
func TestCanceledSupersededRefreshReturnsContextError(t *testing.T) {
	const (
		tableEarlier = `{"data":[{"id":"kimi-earlier","display_name":"kimi-earlier (kimi)"}]}`
		tableLater   = `{"data":[{"id":"kimi-later","display_name":"kimi-later (kimi)"}]}`
	)
	tests := map[string]struct {
		// afterCheck ends the context right after the refresh's last check of it; otherwise it ends
		// while the refresh is parked.
		afterCheck bool
	}{
		"error: context ends while the superseded refresh waits for its listing": {},
		"error: context ends right after the superseded refresh last checked it": {
			afterCheck: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := newParkGate()
				defer gate.open()
				engine := &scriptedEngine{
					listings: []engineReply{
						{status: http.StatusOK, body: tableEarlier, gate: gate},
						listingReply(tableLater),
					},
				}
				cache := newInterleaveCache(t, engine, &route.Store{})
				inner, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx := &checkpointContext{Context: inner}

				earlierDone := make(chan error, 1)
				go func() { earlierDone <- cache.Refresh(ctx) }()
				awaitSignal(t, gate.arrived, "the earlier refresh to park in its listing request")

				if err := cache.Refresh(t.Context()); err != nil {
					t.Fatalf("later Refresh: %v", err)
				}
				later, laterGen := cache.Snapshot()
				if !routesToProvider(later, "claude-kimi-later", "kimi") {
					t.Fatal("test precondition: the later refresh did not commit its table")
				}

				if tt.afterCheck {
					ctx.arm(cancel)
				} else {
					cancel()
				}
				gate.open()
				err := awaitResult(t, earlierDone, "the earlier refresh")
				if ctx.Context.Err() == nil || (tt.afterCheck && !ctx.fired()) {
					t.Fatal("test precondition: the earlier refresh returned without its context having ended")
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("superseded Refresh whose context ended returned %v, want %v", err, context.Canceled)
				}
				snap, gen := cache.Snapshot()
				if gen != laterGen || !routesToProvider(snap, "claude-kimi-later", "kimi") {
					t.Errorf("after the superseded refresh the cache publishes generation %d (routes claude-kimi-later: %v), want the later generation %d unchanged",
						gen, routesToProvider(snap, "claude-kimi-later", "kimi"), laterGen)
				}
			})
		})
	}
}

// lifetimeRequests counts the requests of one test: every request, and the Anthropic catalog page
// requests apart. It answers the proxy listing with one row, and parks each catalog page request
// until its context ends, so a catalog refresh that started stays in flight until shutdown reaches it.
type lifetimeRequests struct {
	all          atomic.Int32
	catalog      atomic.Int32
	active       atomic.Int32
	afterWait    atomic.Int32
	waitReturned atomic.Bool
	arrived      chan struct{}
	arriveOnce   sync.Once
}

func newLifetimeRequests() *lifetimeRequests {
	return &lifetimeRequests{arrived: make(chan struct{})}
}

func (c *lifetimeRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.all.Add(1)
	if r.URL.Host != "anthropic.test" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`))
		return
	}
	c.active.Add(1)
	defer c.active.Add(-1)
	if c.waitReturned.Load() {
		c.afterWait.Add(1)
	}
	c.catalog.Add(1)
	c.arriveOnce.Do(func() { close(c.arrived) })
	select {
	case <-r.Context().Done():
	case <-time.After(time.Hour):
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// newLifetimeCache builds a cache whose every request goes to requests. The periodic interval is ten
// minutes, so after its first refresh a started cache makes no request on its own for that long.
func newLifetimeCache(t *testing.T, requests *lifetimeRequests, routes modelcache.RoutePublisher) *modelcache.Cache {
	t.Helper()
	_, lookup, resolver := newTestPaths(t, map[string]string{"CODEX_GATEWAY_MODELS_REFRESH_MS": "600000"})
	cache, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            resolver,
		ProxyBaseURL:     "http://127.0.0.1:19991",
		AnthropicBaseURL: catalogTestBase,
		DefaultBaseURL:   "http://127.0.0.1:19999",
		Client:           &http.Client{Transport: handlerTransport{handler: requests}},
		Clock:            time.Now,
		Routes:           routes,
		Sentry:           &sentry.State{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cache
}

// TestCatalogRefreshInFlightAtShutdownIsDrained starts the cache, starts a catalog refresh and lets
// its first page request park, then ends the service context and calls Wait. Wait returns only after
// the refresh ended: no catalog request is still running when it returns and none starts afterwards.
//
// This shows that shutdown drains a refresh that was already registered and running. It says nothing
// about a refresh admitted while shutdown begins: the refresh here was admitted long before.
func TestCatalogRefreshInFlightAtShutdownIsDrained(t *testing.T) {
	tests := map[string]struct {
		credentials http.Header
	}{
		"success: refresh waiting for its first page when the service context ends": {
			credentials: http.Header{"X-Api-Key": {apiKeyMarker}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests := newLifetimeRequests()
				routes := &proxyCommitSignal{store: &route.Store{}, committed: make(chan struct{})}
				cache := newLifetimeCache(t, requests, routes)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				cache.Start(ctx)
				awaitSignal(t, routes.committed, "the first refresh to commit")

				cache.TriggerAnthropicRefresh(tt.credentials)
				awaitSignal(t, requests.arrived, "the catalog refresh to request its first page")
				cancel()
				cache.Wait()
				inFlight := requests.active.Load()
				requests.waitReturned.Store(true)
				time.Sleep(time.Hour)
				synctest.Wait()

				if inFlight != 0 {
					t.Errorf("%d catalog request(s) still running when Wait returned, want none", inFlight)
				}
				if n := requests.afterWait.Load(); n != 0 {
					t.Errorf("%d catalog request(s) started after Wait returned, want none", n)
				}
				if n := requests.catalog.Load(); n != 1 {
					t.Errorf("catalog requests = %d, want the one the drained refresh made", n)
				}
			})
		})
	}
}

// TestNoCatalogRefreshAdmittedAfterShutdown asks for catalog refreshes once shutdown has begun, with a
// cold catalog and credentials that would otherwise start one, and counts the requests that reach the
// transport.
//
// After Wait without any cancellation, neither a refresh request, a model listing, a Start nor a
// refresh request after that Start sends anything: Wait alone closes admission. After the service
// context ended but before Wait, a refresh request sends no catalog request: admission sees the ended
// context.
func TestNoCatalogRefreshAdmittedAfterShutdown(t *testing.T) {
	credentials := http.Header{"X-Api-Key": {apiKeyMarker}}
	tests := map[string]struct {
		// cancelFirst starts the cache and ends its service context before the refresh request;
		// otherwise the cache is never started before Wait.
		cancelFirst bool
	}{
		"success: refresh requested and cache started after Wait without cancellation": {},
		"success: refresh requested after the service context ended": {
			cancelFirst: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				requests := newLifetimeRequests()
				routes := &proxyCommitSignal{store: &route.Store{}, committed: make(chan struct{})}
				cache := newLifetimeCache(t, requests, routes)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				if tt.cancelFirst {
					cache.Start(ctx)
					awaitSignal(t, routes.committed, "the first refresh to commit")
					cancel()
					cache.TriggerAnthropicRefresh(credentials)
					cache.Wait()
				} else {
					cache.Wait()
					cache.TriggerAnthropicRefresh(credentials)
					if _, err := cache.ModelsResponse(t.Context(), credentials); err != nil {
						t.Errorf("ModelsResponse after Wait: %s", redactTestMarkers(err.Error()))
					}
					cache.Start(ctx)
					cache.TriggerAnthropicRefresh(credentials)
				}
				time.Sleep(time.Hour)
				synctest.Wait()
				cancel()
				cache.Wait()

				if n := requests.catalog.Load(); n != 0 {
					t.Errorf("%d catalog request(s) sent for a refresh requested after shutdown began, want none", n)
				}
				if !tt.cancelFirst {
					if n := requests.all.Load(); n != 0 {
						t.Errorf("%d request(s) sent by a cache started and asked for a refresh after Wait, want none", n)
					}
				}
			})
		})
	}
}
