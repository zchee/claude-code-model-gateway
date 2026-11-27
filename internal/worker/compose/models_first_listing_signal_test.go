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

package compose_test

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// firstListingWorker is the composed worker of these tests with its model cache, built inside a
// synctest bubble with gate as the cache's whole network.
type firstListingWorker struct {
	cache   *modelcache.Cache
	handler http.Handler
}

// newFirstListingWorker composes a worker whose model cache lists through gate. shadow and
// effective are the cache's options of those names. The returned cleanup releases the gate, ends
// the cache and closes the upstream client; the caller defers it inside the bubble, after
// releasing anything else it holds, so the bubble never ends with a request held.
func newFirstListingWorker(t *testing.T, ctx context.Context, gate *firstListingGate, shadow bool, effective func() (string, error)) (firstListingWorker, func(), bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	client := upstream.NewClient(upstream.Options{})
	dir := t.TempDir()
	lookup := mapLookup(map[string]string{
		"CODEX_GATEWAY_USAGE_ENDPOINT":     "0",
		"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
	})
	routes := &route.Store{}
	state := &sentry.State{}
	mc, err := modelcache.New(modelcache.Options{
		Lookup:           lookup,
		Paths:            paths.New(lookup, func() (string, error) { return dir, nil }, func() (string, error) { return dir, nil }),
		ProxyBaseURL:     "http://" + firstListingProxyHost,
		AnthropicBaseURL: "http://" + firstListingCatalogHost,
		DefaultBaseURL:   "http://127.0.0.1:18764",
		Shadow:           shadow,
		Client:           &http.Client{Transport: gate},
		Clock:            time.Now,
		Routes:           routes,
		Sentry:           state,
		GrokIDs:          compose.NewGrokModelIDs(filepath.Join(dir, "models_cache.json")),
		EffectiveBaseURL: effective,
	})
	if err != nil {
		cancel()
		_ = client.Close()
		t.Errorf("modelcache.New: %v", err)
		return firstListingWorker{}, func() {}, false
	}
	cleanup := func() {
		gate.unhold()
		gate.unholdCatalog()
		cancel()
		mc.Wait()
		_ = client.Close()
	}
	opts := minimalOptions(t, ctx, client)
	opts.ProxyBaseURL = "http://" + firstListingProxyHost
	opts.AnthropicBaseURL = "http://" + firstListingCatalogHost
	opts.Routes = routes
	opts.ModelCache = mc
	opts.Sentry = state
	workerOpts, err := compose.New(opts)
	if err != nil {
		t.Errorf("compose.New: %v", err)
		return firstListingWorker{}, cleanup, false
	}
	wh, err := worker.NewHandler(workerOpts)
	if err != nil {
		t.Errorf("worker.NewHandler: %v", err)
		return firstListingWorker{}, cleanup, false
	}
	mc.Start(ctx)
	return firstListingWorker{cache: mc, handler: wh}, cleanup, true
}

// heldPersistence is an EffectiveBaseURL that holds every discovery sync of the cache, which runs
// after a proxy listing was published, until release; then it reports the gateway unwired, so the
// sync writes nothing.
type heldPersistence struct {
	entered chan struct{}
	release chan struct{}
	unhold  func()
}

func newHeldPersistence() *heldPersistence {
	h := &heldPersistence{entered: make(chan struct{}, 8), release: make(chan struct{})}
	h.unhold = sync.OnceFunc(func() { close(h.release) })
	return h
}

func (h *heldPersistence) effectiveBaseURL() (string, error) {
	h.entered <- struct{}{}
	<-h.release
	return "", nil
}

// TestModelsFirstAnswerFollowsCommit decides what releases a first GET /v1/models that is already
// waiting after startup: the first proxy listing being committed, not the refresh that committed it
// returning. The refresh publishes the snapshot and then persists the discovery cache before it
// returns; with that persistence held, the snapshot is already what every reader sees, so the
// waiting request answers at once with the proxy rows. When a newer refresh commits while the first
// listing is still held, its commit is what releases the waiting request, with its rows, although
// the first refresh never commits. The request is sent while the first listing is held and shown
// to wait, so a fast path for requests that arrive after a commit cannot pass for the wake-up.
// Persistence runs only outside shadow mode.
func TestModelsFirstAnswerFollowsCommit(t *testing.T) {
	const newerRows = `{"data":[{"id":"kimi-k4","display_name":"kimi-k4 (kimi)"}]}`
	tests := map[string]struct {
		// newer lets a newer refresh, whose listing answers newerRows, commit while the first
		// listing stays held; otherwise the first listing is released.
		newer bool
		// wantID is a model the answer must list.
		wantID string
	}{
		"success: the first request answers at the commit while its discovery persistence is held": {
			wantID: "kimi-k3",
		},
		"success: the first request answers at a newer refresh's commit while the first listing is held": {
			newer: true, wantID: "kimi-k4",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				persistence := newHeldPersistence()
				gate := newFirstListingGate(listingHold)
				gate.setRows(1, newerRows)
				w, cleanup, ok := newFirstListingWorker(t, t.Context(), gate, false, persistence.effectiveBaseURL)
				defer cleanup()
				// Deferred after cleanup, so it runs first: the cache's Wait in cleanup needs every
				// held persistence released.
				defer persistence.unhold()
				if !ok {
					return
				}
				if !awaitEntered(t, gate, "the first listing") {
					return
				}
				answers := getModels(t.Context(), w.handler)
				synctest.Wait()
				if a, ok := answered(answers); ok {
					t.Errorf("test precondition: the first GET /v1/models answered (%v) while the first listing was held, want it waiting", a)
					return
				}
				if tt.newer {
					go func() { _ = w.cache.Refresh(t.Context()) }()
				} else {
					gate.unhold()
				}
				select {
				case <-persistence.entered:
				case <-time.After(time.Minute):
					t.Error("no refresh reached its discovery persistence after committing its listing")
					return
				}
				synctest.Wait()
				a, ok := answered(answers)
				if !ok {
					t.Error("the waiting first GET /v1/models was not released although a proxy listing was committed and only its discovery persistence was pending")
					persistence.unhold()
					gate.unhold()
					if a, ok = awaitAnswer(t, answers, "the first GET /v1/models"); ok {
						t.Logf("once the test released the persistence and the gate, it answered: %v", a)
					}
					return
				}
				if a.status != http.StatusOK || a.err != nil || !slices.ContainsFunc(a.ids, func(id string) bool { return strings.Contains(id, tt.wantID) }) {
					t.Errorf("the first GET /v1/models after the commit: %v, want status 200 listing %s", a, tt.wantID)
				}
				if tt.newer && gate.holding.Load() != 1 {
					t.Errorf("test precondition: %d listing(s) held when the answer arrived, want the first one still held", gate.holding.Load())
				}
			})
		})
	}
}

// TestModelsColdCatalogOverlapsFirstListing decides when a first GET /v1/models with Anthropic
// credentials starts the cold Anthropic catalog fetch: at once, while the first proxy listing is
// still pending, as the source starts it on the request (lib/request-worker.js:2409-2413), not
// after the wait for the listing. The answer still comes only once both the listing and the
// catalog landed, at the instant the later one does, and lists the rows of both. A request whose
// own context ends returns at that instant, with both still held.
func TestModelsColdCatalogOverlapsFirstListing(t *testing.T) {
	tests := map[string]struct {
		// catalogFirst releases the catalog before the listing; otherwise the listing goes first.
		catalogFirst bool
		// cancel ends the request's context while both are held, instead of releasing them.
		cancel bool
	}{
		"success: the catalog fetch starts during the first listing, and the answer waits for the catalog landing last": {},
		"success: the catalog fetch starts during the first listing, and the answer waits for the listing landing last": {
			catalogFirst: true,
		},
		"success: a request whose context ends while both are held returns at once": {
			cancel: true,
		},
	}
	credentials := http.Header{"X-Api-Key": {"sk-ant-test-overlap"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := newFirstListingGate(listingHold)
				gate.holdCatalog = true
				w, cleanup, ok := newFirstListingWorker(t, t.Context(), gate, true, nil)
				defer cleanup()
				if !ok {
					return
				}
				if !awaitEntered(t, gate, "the first listing") {
					return
				}
				requestCtx, cancelRequest := context.WithCancel(t.Context())
				defer cancelRequest()
				answers := getModelsWith(requestCtx, w.handler, credentials)
				synctest.Wait()
				if a, ok := answered(answers); ok {
					t.Errorf("the first GET /v1/models answered (%v) while the first listing and the catalog were pending", a)
					return
				}
				if n := gate.catalogCalls.Load(); n != 1 {
					t.Errorf("%d Anthropic catalog request(s) reached the upstream while the first listing was pending, want the cold catalog fetch started at once", n)
				}
				if tt.cancel {
					canceled := time.Now()
					cancelRequest()
					if a, ok := awaitAnswer(t, answers, "the canceled GET /v1/models"); ok && !a.at.Equal(canceled) {
						t.Errorf("the canceled GET /v1/models returned %s after its context ended, want at once", a.at.Sub(canceled))
					}
					// The catalog fetch is shared and runs under the worker's context, so the
					// caller's cancellation ends neither it nor the listing.
					synctest.Wait()
					if listing, catalog := gate.holding.Load(), gate.catalogHolding.Load(); listing != 1 || catalog != 1 {
						t.Errorf("after the canceled request returned: %d listing(s) and %d catalog request(s) held, want both still held", listing, catalog)
					}
					return
				}
				first, second := gate.unhold, gate.unholdCatalog
				firstName, secondName := "listing", "catalog"
				if tt.catalogFirst {
					first, second = second, first
					firstName, secondName = secondName, firstName
				}
				first()
				synctest.Wait()
				if a, ok := answered(answers); ok {
					t.Errorf("the first GET /v1/models answered (%v) once the %s landed, while the %s was still pending", a, firstName, secondName)
					return
				}
				landed := time.Now()
				second()
				a, ok := awaitAnswer(t, answers, "the first GET /v1/models")
				if !ok {
					return
				}
				if !a.at.Equal(landed) {
					t.Errorf("the first GET /v1/models answered %s after the %s landed, want at that instant", a.at.Sub(landed), secondName)
				}
				hasProxy := slices.ContainsFunc(a.ids, func(id string) bool { return strings.Contains(id, "kimi-k3") })
				hasCatalog := slices.ContainsFunc(a.ids, func(id string) bool { return strings.Contains(id, "claude-overlap-1") })
				if a.status != http.StatusOK || a.err != nil || !hasProxy || !hasCatalog {
					t.Errorf("the first GET /v1/models once both landed: %v, want status 200 listing the proxy's kimi-k3 and the catalog's claude-overlap-1", a)
				}
			})
		})
	}
}
