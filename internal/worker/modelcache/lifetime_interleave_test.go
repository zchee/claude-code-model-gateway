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

// catalogRequests counts the Anthropic catalog page requests of one test and parks each until its
// context ends, so a refresh that started stays in flight until shutdown reaches it.
type catalogRequests struct {
	active       atomic.Int32
	started      atomic.Int32
	afterWait    atomic.Int32
	waitReturned atomic.Bool
	arrived      chan struct{}
}

func (c *catalogRequests) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if c.started.Add(1) == 1 {
		close(c.arrived)
	}
	select {
	case <-r.Context().Done():
	case <-time.After(time.Minute):
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

// waitForShutdown is the shutdown goroutine's entry point; its frame names that goroutine in a
// goroutine dump. It records how many catalog requests were still running when Wait returned.
//
//go:noinline
func waitForShutdown(cache *modelcache.Cache, requests *catalogRequests, inFlight *atomic.Int32, done chan<- struct{}) {
	cache.Wait()
	requests.waitReturned.Store(true)
	inFlight.Store(requests.active.Load())
	close(done)
}

// TestCatalogRefreshAdmittedDuringShutdownIsJoined starts the cache, then asks for a catalog refresh
// and ends the service context right after the refresh's admission checked the context, before the
// refresh can register; shutdown then runs to the end of Wait. After Wait returns no catalog request
// starts, and none is still running.
//
// A cache that does not consult the service context when it admits a refresh registers it normally;
// shutdown then has to end the refresh in flight, and the same two facts must hold.
func TestCatalogRefreshAdmittedDuringShutdownIsJoined(t *testing.T) {
	tests := map[string]struct {
		credentials http.Header
	}{
		"success: refresh admitted while shutdown begins": {
			credentials: http.Header{"X-Api-Key": {apiKeyMarker}},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, lookup, resolver := newTestPaths(t, map[string]string{"CODEX_GATEWAY_MODELS_REFRESH_MS": "600000"})
			requests := &catalogRequests{arrived: make(chan struct{})}
			routes := &proxyCommitSignal{store: &route.Store{}, committed: make(chan struct{})}
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
			inner, cancel := context.WithCancel(t.Context())
			defer cancel()
			worker := &checkpointContext{Context: inner}
			cache.Start(worker)
			// The first refresh's listing has rows, so the periodic loop then idles until the
			// ten-minute interval and makes no further check of the service context.
			awaitSignal(t, routes.committed, "the first refresh to commit")

			var inFlight atomic.Int32
			waitDone := make(chan struct{})
			startShutdown := func() {
				cancel()
				go waitForShutdown(cache, requests, &inFlight, waitDone)
			}
			var gateReached, gateOK bool
			var onAdmission func()
			onAdmission = func() {
				// Only the admission of the requested refresh is interrupted; a check made by any other
				// goroutine passes and the hook stays armed.
				if !onCurrentStack("modelcache.(*Cache).TriggerAnthropicRefresh") {
					worker.arm(onAdmission)
					return
				}
				gateReached = true
				startShutdown()
				// Shutdown either returns from Wait, or waits on a gate the admission holds while it
				// checks the context; the admission then continues with what it observed.
				_, gateOK = doneOrLockWait(waitDone, "modelcache_test.waitForShutdown")
			}
			worker.arm(onAdmission)
			cache.TriggerAnthropicRefresh(tt.credentials)
			switch {
			case gateReached && !gateOK:
				t.Fatal("shutdown neither returned from Wait nor waited on the admission")
			case !gateReached:
				t.Log("the admission did not check the service context; shutting down a registered refresh instead")
				awaitSignal(t, requests.arrived, "the registered refresh to request its first page")
				startShutdown()
			}
			awaitSignal(t, waitDone, "Wait to return after the service context ended")
			// Anything registered after Wait returned is joined here, so no goroutine outlives the test.
			cache.Wait()

			if n := requests.afterWait.Load(); n != 0 {
				t.Errorf("%d catalog request(s) started after Wait returned, want none", n)
			}
			if n := inFlight.Load(); n != 0 {
				t.Errorf("%d catalog request(s) still running when Wait returned, want none", n)
			}
		})
	}
}
