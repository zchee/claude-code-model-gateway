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
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

const (
	// firstListingProxyHost is the proxy the model cache lists; firstListingGate answers it itself.
	firstListingProxyHost = "127.0.0.1:19991"
	// firstListingRows lists one model that the built-in list does not hold.
	firstListingRows = `{"data":[{"id":"kimi-k3","display_name":"kimi-k3 (kimi)"}]}`
	// firstListingCatalogHost is the Anthropic API the model cache fetches its catalog from;
	// firstListingGate answers it itself.
	firstListingCatalogHost = "anthropic.test"
	// firstListingCatalogRows is one Anthropic catalog page with one model.
	firstListingCatalogRows = `{"data":[{"id":"claude-overlap-1","display_name":"Overlap One"}],"has_more":false}`
)

// listingMode is what firstListingGate does with one proxy listing request.
type listingMode int

const (
	// listingAnswer answers the listing at once.
	listingAnswer listingMode = iota
	// listingFail answers the listing at once with a server error.
	listingFail
	// listingHold holds the listing until release, then answers it.
	listingHold
	// listingHoldFail holds the listing until release, then answers it with a server error.
	listingHoldFail
	// listingHoldTimeout holds the listing until its own context ends.
	listingHoldTimeout
	// listingHoldUntilCleanup holds the listing until release and ignores its context, so it stays
	// held past its own timeout until the test cleans up.
	listingHoldUntilCleanup
)

// firstListingGate is the model cache's whole network: it answers the proxy listing requests in
// turn by modes (later requests are answered at once) and every other request, the local engine
// probes and the Anthropic catalog, with 404. A held listing also ends with its request's context,
// except one held until cleanup.
type firstListingGate struct {
	mu    sync.Mutex // guards modes and calls
	modes []listingMode
	calls int
	// holding counts the listing requests held at the gate now.
	holding atomic.Int32
	// rows holds the listing body of a request by its index; others answer firstListingRows.
	rows map[int]string

	// holdCatalog makes the Anthropic catalog page answer one row once releaseCatalog closes, or
	// end with its request's context; otherwise the catalog answers 404.
	holdCatalog  bool
	catalogCalls atomic.Int32
	// catalogHolding counts the catalog requests held at the gate now.
	catalogHolding atomic.Int32
	catalogEntered chan struct{}
	releaseCatalog chan struct{}
	unholdCatalog  func()
	entered        chan struct{}
	release        chan struct{}
	unhold         func()
}

func newFirstListingGate(modes ...listingMode) *firstListingGate {
	g := &firstListingGate{modes: modes, entered: make(chan struct{}, 8), release: make(chan struct{}), catalogEntered: make(chan struct{}, 8), releaseCatalog: make(chan struct{})}
	g.unhold = sync.OnceFunc(func() { close(g.release) })
	g.unholdCatalog = sync.OnceFunc(func() { close(g.releaseCatalog) })
	return g
}

// arm sets the mode of the next listing request, counting from the first.
func (g *firstListingGate) arm(call int, mode listingMode) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for len(g.modes) <= call {
		g.modes = append(g.modes, listingAnswer)
	}
	g.modes[call] = mode
}

// setRows sets the listing body of the listing request with index call, counting from the first.
func (g *firstListingGate) setRows(call int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.rows == nil {
		g.rows = map[int]string{}
	}
	g.rows[call] = body
}

func (g *firstListingGate) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == firstListingCatalogHost && r.URL.Path == "/v1/models" && g.holdCatalog {
		g.catalogCalls.Add(1)
		g.catalogHolding.Add(1)
		defer g.catalogHolding.Add(-1)
		g.catalogEntered <- struct{}{}
		select {
		case <-g.releaseCatalog:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return gateResponse(r, http.StatusOK, firstListingCatalogRows), nil
	}
	if r.URL.Host != firstListingProxyHost || r.URL.Path != "/v1/models" {
		return gateResponse(r, http.StatusNotFound, ""), nil
	}
	g.mu.Lock()
	mode := listingAnswer
	if g.calls < len(g.modes) {
		mode = g.modes[g.calls]
	}
	body := firstListingRows
	if rows, ok := g.rows[g.calls]; ok {
		body = rows
	}
	g.calls++
	g.mu.Unlock()
	switch mode {
	case listingHold, listingHoldFail, listingHoldTimeout, listingHoldUntilCleanup:
		g.holding.Add(1)
		defer g.holding.Add(-1)
		g.entered <- struct{}{}
		release, done := g.release, r.Context().Done()
		switch mode {
		case listingHoldTimeout:
			release = nil
		case listingHoldUntilCleanup:
			done = nil
		}
		select {
		case <-release:
		case <-done:
			return nil, r.Context().Err()
		}
	}
	if mode == listingFail || mode == listingHoldFail {
		return gateResponse(r, http.StatusServiceUnavailable, `{"error":"unavailable"}`), nil
	}
	return gateResponse(r, http.StatusOK, body), nil
}

func gateResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r,
	}
}

// modelsAnswer is one GET /v1/models answer of the composed worker, with the virtual instant it
// completed.
type modelsAnswer struct {
	// aborted reports a handler that ended the request with http.ErrAbortHandler, as the worker
	// does for a request whose client went away.
	aborted bool
	status  int
	ids     []string
	err     error
	at      time.Time
}

// source names which list an answer advertises: "proxy" when it holds the proxy's model and none of
// the built-in list, "fallback" for the reverse, and "mixed" or "none" otherwise.
func (a modelsAnswer) source() string {
	proxy := slices.ContainsFunc(a.ids, func(id string) bool { return strings.Contains(id, "kimi-k3") })
	builtIn := slices.ContainsFunc(a.ids, func(id string) bool { return strings.Contains(id, "gpt-6.1-sol") })
	switch {
	case proxy && !builtIn:
		return "proxy"
	case builtIn && !proxy:
		return "fallback"
	case proxy:
		return "mixed"
	default:
		return "none"
	}
}

func (a modelsAnswer) String() string {
	return fmt.Sprintf("aborted %t, status %d, %s list %q, err %v", a.aborted, a.status, a.source(), a.ids, a.err)
}

// getModels sends GET /v1/models to wh from its own goroutine and delivers the answer.
func getModels(ctx context.Context, wh http.Handler) <-chan modelsAnswer {
	return getModelsWith(ctx, wh, nil)
}

// getModelsWith is getModels with the request headers header.
func getModelsWith(ctx context.Context, wh http.Handler, header http.Header) <-chan modelsAnswer {
	answers := make(chan modelsAnswer, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/models", nil)
		maps.Copy(req.Header, header)
		var aborted bool
		func() {
			// The HTTP server recovers this panic, which ends a request without an answer; the
			// test calls the handler itself and recovers it in the server's stead.
			defer func() {
				if r := recover(); r != nil {
					if err, _ := r.(error); !errors.Is(err, http.ErrAbortHandler) {
						panic(r)
					}
					aborted = true
				}
			}()
			wh.ServeHTTP(rec, req)
		}()
		answer := modelsAnswer{aborted: aborted, status: rec.Code}
		var doc struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if answer.err = json.Unmarshal(rec.Body.Bytes(), &doc); answer.err == nil {
			for _, row := range doc.Data {
				answer.ids = append(answer.ids, row.ID)
			}
		}
		answer.at = time.Now()
		answers <- answer
	}()
	return answers
}

// answered returns the answer if it has arrived, without waiting.
func answered(answers <-chan modelsAnswer) (modelsAnswer, bool) {
	select {
	case a := <-answers:
		return a, true
	default:
		return modelsAnswer{}, false
	}
}

// awaitAnswer waits for the answer within a minute of virtual time, which only aborts a worker that
// never answers.
func awaitAnswer(t *testing.T, answers <-chan modelsAnswer, what string) (modelsAnswer, bool) {
	t.Helper()
	select {
	case a := <-answers:
		return a, true
	case <-time.After(time.Minute):
		t.Errorf("no answer to %s within a minute", what)
		return modelsAnswer{}, false
	}
}

// awaitEntered waits until a held listing request has reached the gate.
func awaitEntered(t *testing.T, g *firstListingGate, what string) bool {
	t.Helper()
	select {
	case <-g.entered:
		return true
	case <-time.After(time.Minute):
		t.Errorf("%s never reached the proxy", what)
		return false
	}
}

// TestModelsFirstAnswerWaitsForFirstListing decides, through the composed worker, when GET
// /v1/models answers after the worker started its model cache. The first answer waits for the first
// proxy listing: while that listing is pending the request does not answer from the built-in list,
// and it answers with the proxy's rows the moment the listing lands. A first listing that fails, or
// that reaches its own timeout, ends the wait at that instant with the built-in list. A later request
// never waits for a later listing: it answers at once from what is committed. The source answers at
// once from the built-in list even before its first listing (lib/request-worker.js:1211-1214,
// 1535); waiting is a deliberate difference that removes a startup race. Virtual time pins the
// instants, and synctest.Wait shows a request still pending once nothing else can run.
func TestModelsFirstAnswerWaitsForFirstListing(t *testing.T) {
	tests := map[string]struct {
		// first is the mode of the first proxy listing, the one the started cache makes.
		first listingMode
		// later makes the request under test a later one: the first listing ends before any
		// request, and the request is sent while a later listing is held.
		later bool
		// pastBound sends the first request partway through a first listing that stays held past
		// its timeout, until cleanup.
		pastBound bool
		// cancelRequest ends the first request's own context while the first listing is held.
		cancelRequest bool
		// want is the list the request under test advertises.
		want string
	}{
		"success: a first request sent partway through a listing held past its bound answers from the built-in list at the startup deadline": {
			first: listingHoldUntilCleanup, pastBound: true, want: "fallback",
		},
		"success: a first request whose context ends returns at once while the first listing is held, and the listing still lands": {
			first: listingHold, cancelRequest: true, want: "proxy",
		},
		"success: the first request waits for a held first listing and lists the proxy rows": {
			first: listingHold, want: "proxy",
		},
		"success: a first listing that fails while the first request waits answers from the built-in list": {
			first: listingHoldFail, want: "fallback",
		},
		"success: a first listing that reaches its timeout while the first request waits answers from the built-in list": {
			first: listingHoldTimeout, want: "fallback",
		},
		"success: a later request does not wait for a later listing after a listed start": {
			first: listingAnswer, later: true, want: "proxy",
		},
		"success: a later request does not wait for a later listing after a failed first listing": {
			first: listingFail, later: true, want: "fallback",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := newFirstListingGate(tt.first)
				ctx, cancel := context.WithCancel(t.Context())
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
					AnthropicBaseURL: "http://anthropic.test",
					DefaultBaseURL:   "http://127.0.0.1:18764",
					Shadow:           true,
					Client:           &http.Client{Transport: gate},
					Clock:            time.Now,
					Routes:           routes,
					Sentry:           state,
					GrokIDs:          compose.NewGrokModelIDs(filepath.Join(dir, "models_cache.json")),
				})
				if err != nil {
					t.Fatalf("modelcache.New: %v", err)
				}
				// Every path releases the gate and ends the cache, so the bubble never ends with a
				// listing held.
				defer func() {
					gate.unhold()
					cancel()
					mc.Wait()
					_ = client.Close()
				}()
				opts := minimalOptions(t, ctx, client)
				opts.ProxyBaseURL = "http://" + firstListingProxyHost
				opts.Routes = routes
				opts.ModelCache = mc
				opts.Sentry = state
				workerOpts, err := compose.New(opts)
				if err != nil {
					t.Errorf("compose.New: %v", err)
					return
				}
				wh, err := worker.NewHandler(workerOpts)
				if err != nil {
					t.Errorf("worker.NewHandler: %v", err)
					return
				}

				mc.Start(ctx)
				switch {
				case tt.later:
					runLaterRequest(t, ctx, gate, mc, wh, tt.want)
					return
				case tt.pastBound:
					runPastBound(t, ctx, gate, wh, tt.want)
					return
				case tt.cancelRequest:
					runCanceledRequest(t, ctx, gate, wh, tt.want)
					return
				}
				if !awaitEntered(t, gate, "the first listing") {
					return
				}
				entered := time.Now()
				answers := getModels(ctx, wh)
				synctest.Wait()
				if a, ok := answered(answers); ok {
					t.Errorf("the first GET /v1/models answered (%v) while the first listing was pending, want it to wait for the listing", a)
					return
				}
				var ended time.Time
				if tt.first == listingHoldTimeout {
					time.Sleep(modelcache.ProxyListingTimeout - time.Millisecond)
					synctest.Wait()
					if a, ok := answered(answers); ok {
						t.Errorf("the first GET /v1/models answered (%v) %s after the first listing started, before the listing's %s timeout", a, time.Since(entered), modelcache.ProxyListingTimeout)
						return
					}
					ended = entered.Add(modelcache.ProxyListingTimeout)
				} else {
					ended = time.Now()
					gate.unhold()
				}
				a, ok := awaitAnswer(t, answers, "the first GET /v1/models")
				if !ok {
					return
				}
				if a.status != http.StatusOK || a.err != nil || a.source() != tt.want {
					t.Errorf("the first GET /v1/models after the first listing ended: %v, want status 200 and the %s list", a, tt.want)
				}
				if !a.at.Equal(ended) {
					t.Errorf("the first GET /v1/models answered %s after the first listing ended, want at that instant", a.at.Sub(ended))
				}
			})
		})
	}
}

// runLaterRequest lets the started cache's first listing end, answers one request, then holds a
// later listing and requires a request sent meanwhile to answer at once with want.
func runLaterRequest(t *testing.T, ctx context.Context, gate *firstListingGate, mc *modelcache.Cache, wh http.Handler, want string) {
	t.Helper()
	// The first listing is answered at once; once nothing else can run, the first refresh ended.
	synctest.Wait()
	if a, ok := awaitAnswer(t, getModels(ctx, wh), "the first GET /v1/models"); ok && a.source() != want {
		t.Errorf("test precondition: the first GET /v1/models after the first listing ended: %v, want the %s list", a, want)
	}
	gate.arm(1, listingHold)
	refreshed := make(chan error, 1)
	go func() { refreshed <- mc.Refresh(ctx) }()
	if !awaitEntered(t, gate, "the later listing") {
		return
	}
	answers := getModels(ctx, wh)
	synctest.Wait()
	a, ok := answered(answers)
	if !ok {
		t.Error("a later GET /v1/models waited for a later listing, want it to answer at once")
	} else if a.status != http.StatusOK || a.err != nil || a.source() != want {
		t.Errorf("a later GET /v1/models while a later listing was held: %v, want status 200 and the %s list", a, want)
	}
	gate.unhold()
	if !ok {
		_, _ = awaitAnswer(t, answers, "the later GET /v1/models")
	}
	select {
	case <-refreshed:
	case <-time.After(time.Minute):
		t.Error("the later refresh never ended")
	}
}

// runPastBound sends the first request halfway through a first listing that stays held past its
// timeout, and requires the request to wait and then answer with want at the startup deadline, the
// listing's timeout after the cache started, while the listing is still held.
func runPastBound(t *testing.T, ctx context.Context, gate *firstListingGate, wh http.Handler, want string) {
	t.Helper()
	if !awaitEntered(t, gate, "the first listing") {
		return
	}
	// The cache starts its first listing at once, so this is also the instant it started.
	started := time.Now()
	time.Sleep(modelcache.ProxyListingTimeout / 2)
	answers := getModels(ctx, wh)
	synctest.Wait()
	if a, ok := answered(answers); ok {
		t.Errorf("the first GET /v1/models answered (%v) %s after the cache started, while the first listing was pending and before the startup deadline", a, time.Since(started))
		return
	}
	a, ok := awaitAnswer(t, answers, "the first GET /v1/models")
	if !ok {
		return
	}
	deadline := started.Add(modelcache.ProxyListingTimeout)
	if a.status != http.StatusOK || a.err != nil || a.source() != want {
		t.Errorf("the first GET /v1/models at the startup deadline: %v, want status 200 and the %s list", a, want)
	}
	if !a.at.Equal(deadline) {
		t.Errorf("the first GET /v1/models answered %s after the cache started, want at the startup deadline %s", a.at.Sub(started), modelcache.ProxyListingTimeout)
	}
	if n := gate.holding.Load(); n != 1 {
		t.Errorf("test precondition: %d listing(s) held when the answer arrived, want the first one still held", n)
	}
}

// runCanceledRequest sends the first request while the first listing is held, ends the request's
// own context, and requires the request to return at that instant while the listing is still held;
// the listing, released afterwards, still lands, and a new request lists want.
func runCanceledRequest(t *testing.T, ctx context.Context, gate *firstListingGate, wh http.Handler, want string) {
	t.Helper()
	if !awaitEntered(t, gate, "the first listing") {
		return
	}
	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	answers := getModels(requestCtx, wh)
	time.Sleep(modelcache.ProxyListingTimeout / 2)
	synctest.Wait()
	if a, ok := answered(answers); ok {
		t.Errorf("the first GET /v1/models answered (%v) while the first listing was pending, before its context ended", a)
		return
	}
	canceled := time.Now()
	cancelRequest()
	a, ok := awaitAnswer(t, answers, "the canceled GET /v1/models")
	if !ok {
		return
	}
	if !a.at.Equal(canceled) {
		t.Errorf("the canceled GET /v1/models returned %s after its context ended, want at once", a.at.Sub(canceled))
	}
	if n := gate.holding.Load(); n != 1 {
		t.Errorf("test precondition: %d listing(s) held when the canceled request returned, want the first one still held", n)
	}
	gate.unhold()
	synctest.Wait()
	if b, ok := awaitAnswer(t, getModels(ctx, wh), "a GET /v1/models after the first listing landed"); ok && (b.status != http.StatusOK || b.err != nil || b.source() != want) {
		t.Errorf("a GET /v1/models after the released first listing: %v, want status 200 and the %s list", b, want)
	}
}
