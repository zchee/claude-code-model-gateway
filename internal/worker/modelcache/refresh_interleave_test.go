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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// interleaveLlamaPort is the llama-server port named in the lookup; the in-memory transport answers
// every host, so nothing listens on it.
const interleaveLlamaPort = 18911

// parkGate holds one request inside the fake engine until the test releases it.
type parkGate struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
	free    sync.Once
}

func newParkGate() *parkGate {
	return &parkGate{arrived: make(chan struct{}), release: make(chan struct{})}
}

// open lets the parked request answer; it is safe to call more than once.
func (g *parkGate) open() { g.free.Do(func() { close(g.release) }) }

// engineReply is one scripted answer of the fake proxy or llama-server.
type engineReply struct {
	status int
	body   string
	gate   *parkGate
}

// scriptedEngine answers /v1/models and /props from per-path scripts in arrival order. Once a script
// is used up its last reply repeats without its gate, so a refresh that re-selects or re-probes
// never parks a second time.
type scriptedEngine struct {
	mu       sync.Mutex
	listings []engineReply
	props    []engineReply
	hits     map[string]int
}

func (e *scriptedEngine) next(path string) (engineReply, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var script []engineReply
	switch path {
	case "/v1/models":
		script = e.listings
	case "/props":
		script = e.props
	}
	if len(script) == 0 {
		return engineReply{}, false
	}
	if e.hits == nil {
		e.hits = map[string]int{}
	}
	n := e.hits[path]
	e.hits[path]++
	if n >= len(script) {
		last := script[len(script)-1]
		last.gate = nil
		return last, true
	}
	return script[n], true
}

func (e *scriptedEngine) hitCount(path string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hits[path]
}

func (e *scriptedEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply, ok := e.next(r.URL.Path)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if g := reply.gate; g != nil {
		g.once.Do(func() { close(g.arrived) })
		select {
		case <-g.release:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(reply.status)
	_, _ = w.Write([]byte(reply.body))
}

func listingReply(body string) engineReply { return engineReply{status: http.StatusOK, body: body} }

func propsReply(nCtx int64) engineReply {
	return engineReply{status: http.StatusOK, body: `{"default_generation_settings":{"n_ctx":` + strconv.FormatInt(nCtx, 10) + `}}`}
}

// awaitSignal waits for ch, failing the test when it does not close within a minute (virtual time
// inside a bubble, so a missing signal fails instead of deadlocking the bubble).
func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Minute):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// awaitResult waits for one Refresh result with the same bound as awaitSignal.
func awaitResult(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Minute):
		t.Fatalf("timed out waiting for %s to return", what)
		return nil
	}
}

// routesToProvider reports whether snap sends a Messages request for the advertised model to provider
// through the proxy.
func routesToProvider(snap route.Snapshot, advertised, provider string) bool {
	d := snap.Classify(http.MethodPost, "/v1/messages", []byte(`{"model":"`+advertised+`"}`))
	return d.Branch == "proxy" && d.Provider == provider
}

// catalogRowIDs returns the IDs of advertised catalog rows in their order.
func catalogRowIDs(t *testing.T, rows jsontext.Value) []string {
	t.Helper()
	var decoded []idDisplay
	if err := json.Unmarshal(rows, &decoded); err != nil {
		t.Fatalf("unmarshal advertised rows: %v", err)
	}
	ids := make([]string, 0, len(decoded))
	for _, row := range decoded {
		ids = append(ids, row.ID)
	}
	return ids
}

func newInterleaveCache(t *testing.T, engine *scriptedEngine, routes *route.Store) *modelcache.Cache {
	t.Helper()
	_, lookup, resolver := newTestPaths(t, map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(interleaveLlamaPort)})
	cache, err := modelcache.New(modelcache.Options{
		Lookup:         lookup,
		Paths:          resolver,
		ProxyBaseURL:   "http://127.0.0.1:19991",
		DefaultBaseURL: "http://127.0.0.1:19999",
		Client:         &http.Client{Transport: handlerTransport{handler: engine}},
		Clock:          time.Now,
		Routes:         routes,
		Sentry:         &sentry.State{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cache
}

const (
	// tableWithLocal lists a Kimi row and a llama-server row, so a refresh that keeps it probes llama-server.
	tableWithLocal = `{"data":[{"id":"kimi-earlier","display_name":"kimi-earlier (kimi)"},{"id":"local/gguf","display_name":"local/gguf (local)"}]}`
	// tableNewer lists only a different Kimi row and no local-engine row.
	tableNewer = `{"data":[{"id":"kimi-newer","display_name":"kimi-newer (kimi)"}]}`
)

// TestEmptyListingKeepsLatestCommittedTable starts a refresh, then a later one whose listing comes
// back empty and which parks in its llama-server probe while the earlier refresh commits a newer proxy
// table. When the later refresh finishes, the routes and the reported source are still the newer table.
func TestEmptyListingKeepsLatestCommittedTable(t *testing.T) {
	tests := map[string]struct {
		laterListing engineReply
	}{
		"success: later listing answers HTTP 503": {
			laterListing: engineReply{status: http.StatusServiceUnavailable, body: `{}`},
		},
		"success: later listing answers HTTP 200 with no rows": {
			laterListing: listingReply(`{"data":[]}`),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				earlierListing := newParkGate()
				laterProbe := newParkGate()
				defer earlierListing.open()
				defer laterProbe.open()
				engine := &scriptedEngine{
					listings: []engineReply{
						listingReply(tableWithLocal),
						{status: http.StatusOK, body: tableNewer, gate: earlierListing},
						tt.laterListing,
					},
					props: []engineReply{
						propsReply(65536),
						{status: http.StatusOK, body: `{"default_generation_settings":{"n_ctx":65536}}`, gate: laterProbe},
					},
				}
				routes := &route.Store{}
				cache := newInterleaveCache(t, engine, routes)

				if err := cache.Refresh(t.Context()); err != nil {
					t.Fatalf("seeding Refresh: %v", err)
				}
				seeded, _ := cache.Snapshot()
				if !routesToProvider(seeded, "claude-kimi-earlier", "kimi") {
					t.Fatal("test precondition: the seeding refresh did not commit the table with the llama-server row")
				}

				earlierDone := make(chan error, 1)
				go func() { earlierDone <- cache.Refresh(t.Context()) }()
				awaitSignal(t, earlierListing.arrived, "the earlier refresh to reach its listing request")

				laterDone := make(chan error, 1)
				go func() { laterDone <- cache.Refresh(t.Context()) }()
				awaitSignal(t, laterProbe.arrived, "the later refresh to park in its llama-server probe")

				earlierListing.open()
				if err := awaitResult(t, earlierDone, "the earlier refresh"); err != nil {
					t.Fatalf("earlier Refresh: %v", err)
				}
				committed, committedGen := cache.Snapshot()
				if !routesToProvider(committed, "claude-kimi-newer", "kimi") {
					t.Fatal("test precondition: the earlier refresh did not commit the newer table while the later refresh was parked")
				}
				if cache.LlamaState() != nil {
					t.Fatal("test precondition: the newer table lists no llama-server row, but the committed generation keeps llama-server state")
				}
				_, committedRows := committed.Catalog()
				if ids := catalogRowIDs(t, committedRows); !slices.Contains(ids, "claude-kimi-newer") || slices.Contains(ids, "claude-local/gguf") {
					t.Fatalf("test precondition: rows of the newer table = %q, want claude-kimi-newer and no claude-local/gguf", ids)
				}

				laterProbe.open()
				if err := awaitResult(t, laterDone, "the later refresh"); err != nil {
					t.Fatalf("later Refresh: %v", err)
				}

				snap, gen := cache.Snapshot()
				if gen != committedGen+1 {
					t.Errorf("generation after the later refresh = %d, want %d: the later refresh must commit once, rebuilt on the newer table", gen, committedGen+1)
				}
				if got := cache.LlamaState(); got != nil {
					t.Errorf("llama-server state after the later refresh = %+v, want none: the kept table lists no llama-server row", got)
				}
				_, rows := snap.Catalog()
				if diff := gocmp.Diff(catalogRowIDs(t, committedRows), catalogRowIDs(t, rows)); diff != "" {
					t.Errorf("advertised rows after the later refresh differ from those of the kept table (-kept +got):\n%s", diff)
				}
				stored, ok := routes.Load()
				if !ok {
					t.Fatal("route store is empty")
				}
				for surface, s := range map[string]route.Snapshot{"Cache.Snapshot": snap, "route store": stored} {
					if !routesToProvider(s, "claude-kimi-newer", "kimi") {
						t.Errorf("%s no longer routes claude-kimi-newer to kimi after the later empty-listing refresh: the newer committed table was replaced", surface)
					}
					if routesToProvider(s, "claude-kimi-earlier", "kimi") {
						t.Errorf("%s routes claude-kimi-earlier again: the later refresh published the table it sampled before the newer commit", surface)
					}
					if source, _ := s.Catalog(); source != "proxy" {
						t.Errorf("%s catalog source = %q, want proxy", surface, source)
					}
				}
				if source, _ := cache.HealthCatalog(); source != "proxy" {
					t.Errorf("HealthCatalog source = %q, want proxy", source)
				}
			})
		})
	}
}

// TestUncommittedRefreshLeavesLocalEngineState checks that a refresh which loses to a later-started
// committed refresh changes nothing a still later refresh inherits: during a llama-server outage on
// the same port, the inherited window is the last committed measurement.
func TestUncommittedRefreshLeavesLocalEngineState(t *testing.T) {
	const outageError = "GET /props answered HTTP 500"
	tests := map[string]struct {
		// olderListing and olderProbe script the older refresh, one of which parks on gate.
		olderListing func(gate *parkGate) engineReply
		olderProbe   func(gate *parkGate) []engineReply
		where        string
	}{
		"success: a slow older probe answering after the newer commit is not inherited": {
			olderListing: func(*parkGate) engineReply { return listingReply(tableWithLocal) },
			olderProbe: func(gate *parkGate) []engineReply {
				return []engineReply{{status: http.StatusOK, body: `{"default_generation_settings":{"n_ctx":100000}}`, gate: gate}}
			},
			where: "its llama-server probe",
		},
		"success: a dropped older refresh whose listing has no local row keeps outage preservation": {
			olderListing: func(gate *parkGate) engineReply {
				return engineReply{status: http.StatusOK, body: tableNewer, gate: gate}
			},
			olderProbe: func(*parkGate) []engineReply { return nil },
			where:      "its listing request",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				older := newParkGate()
				defer older.open()
				props := []engineReply{propsReply(65536)}
				props = append(props, tt.olderProbe(older)...)
				props = append(props, propsReply(40960), engineReply{status: http.StatusInternalServerError, body: `{}`})
				engine := &scriptedEngine{
					listings: []engineReply{
						listingReply(tableWithLocal),
						tt.olderListing(older),
						listingReply(tableWithLocal),
						listingReply(tableWithLocal),
					},
					props: props,
				}
				cache := newInterleaveCache(t, engine, &route.Store{})

				if err := cache.Refresh(t.Context()); err != nil {
					t.Fatalf("seeding Refresh: %v", err)
				}
				if got := cache.LlamaState(); got == nil || got.ContextWindow != 65536 {
					t.Fatalf("test precondition: seeded llama-server state = %+v, want window 65536", got)
				}

				olderDone := make(chan error, 1)
				go func() { olderDone <- cache.Refresh(t.Context()) }()
				awaitSignal(t, older.arrived, "the older refresh to park in "+tt.where)

				if err := cache.Refresh(t.Context()); err != nil {
					t.Fatalf("newer Refresh: %v", err)
				}
				if got := cache.LlamaState(); got == nil || !got.Reachable || got.ContextWindow != 40960 {
					t.Fatalf("test precondition: llama-server state after the newer commit = %+v, want reachable window 40960", got)
				}

				older.open()
				if err := awaitResult(t, olderDone, "the older refresh"); err != nil {
					t.Fatalf("older Refresh: %v", err)
				}
				if got := cache.LlamaState(); got == nil || got.ContextWindow != 40960 {
					t.Errorf("llama-server state after the dropped older refresh = %+v, want the newer committed window 40960", got)
				}

				if err := cache.Refresh(t.Context()); err != nil {
					t.Fatalf("outage Refresh: %v", err)
				}
				if got := engine.hitCount("/props"); got < len(props) {
					t.Fatalf("test precondition: llama-server saw %d probes, want %d (the outage refresh must have probed)", got, len(props))
				}
				want := &localprov.LlamaState{Port: interleaveLlamaPort, Configured: true, ContextWindow: 40960, Error: outageError}
				if diff := gocmp.Diff(want, cache.LlamaState()); diff != "" {
					t.Errorf("llama-server state inherited during the outage mismatch (-want +got):\n%s", diff)
				}
				snap, _ := cache.Snapshot()
				if got := snap.Resolver().AdvertisedWindow("claude-local/gguf"); got != 40960 {
					t.Errorf("advertised window of claude-local/gguf during the outage = %d, want the last committed measurement 40960", got)
				}
				if t.Failed() {
					t.Logf("probes answered in order: 65536, older %s, newer 40960, outage HTTP 500", tt.where)
				}
			})
		})
	}
}
