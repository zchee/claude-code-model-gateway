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

package route_test

import (
	"encoding/json/jsontext"
	"fmt"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

func TestSnapshotOwnership(t *testing.T) {
	ids := []string{"grok-private.2"}
	catalog := jsontext.Value(`[{"id":"original"}]`)
	windows := policy.Windows{"codex": {Value: config.ContextWindow{Tokens: 272000}}}
	live := map[string]int64{"local": 65536}
	routes := []proxymodels.Route{{ID: "lmstudio/local", Provider: "lmstudio"}}
	snapshot := route.NewSnapshot(resolverFor(routes, windows, 0, live), "proxy", catalog, ids)
	windows["codex"] = policy.Setting{Value: config.ContextWindow{Tokens: 500000}}
	live["local"] = 32768
	ids[0] = "other"
	catalog[2] = 'x'
	_, out := snapshot.Catalog()
	clear(out)
	source, out := snapshot.Catalog()
	r := snapshot.Resolver()
	codex, lmStudio := r.AdvertisedWindow("gpt-5.6-sol"), r.AdvertisedWindow("lmstudio/local")
	if source != "proxy" || string(out) != `[{"id":"original"}]` || codex != 272000 || lmStudio != 65536 {
		t.Errorf("snapshot changed: source=%q catalog=%q Codex window=%d LM Studio window=%d", source, out, codex, lmStudio)
	}
	want := route.Decision{Branch: "grok", Provider: "grok", Model: "grok-private.2"}
	if diff := gocmp.Diff(want, snapshot.Classify("POST", "/v1/messages", []byte(`{"model":"claude-grok-private-2"}`))); diff != "" {
		t.Error(diff)
	}
}

func TestLoadedValueCannotReplacePublishedSnapshot(t *testing.T) {
	var store route.Store
	if _, ok := store.Load(); ok {
		t.Fatal("empty store has a generation")
	}
	const catalog = `[{"id":"kept"}]`
	store.Publish(route.NewSnapshot(resolverFor(nil, nil, 0, nil), "proxy", jsontext.Value(catalog), nil))
	loaded, ok := store.Load()
	if !ok {
		t.Fatal("published generation missing")
	}
	retained := loaded
	_, mutable := loaded.Catalog()
	clear(mutable)
	m := loaded.Resolver().Resolve("claude-gpt-5.6-sol")
	if m == nil {
		t.Fatal("published Codex policy missing")
	}
	m.BackendWindow = 1
	discard := func(s *route.Snapshot) { *s = route.Snapshot{} }
	discard(&loaded)
	latest, ok := store.Load()
	if !ok {
		t.Fatal("discarding the handle removed the generation")
	}
	tests := map[string]struct{ snapshot route.Snapshot }{"success: retained": {retained}, "success: store": {latest}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			source, rows := tt.snapshot.Catalog()
			policy := tt.snapshot.Resolver().Resolve("claude-gpt-5.6-sol")
			if source != "proxy" || string(rows) != catalog || policy == nil || policy.BackendWindow != 920012 {
				t.Errorf("published snapshot changed: source=%q catalog=%q policy=%+v", source, rows, policy)
			}
		})
	}
}

func TestGrokIDsFollowPublishedSnapshot(t *testing.T) {
	var store route.Store
	publish := func(id string) route.Snapshot {
		store.Publish(route.NewSnapshot(policy.Resolver{}, "proxy", nil, []string{id}))
		got, ok := store.Load()
		if !ok {
			t.Fatal("published Grok snapshot missing")
		}
		return got
	}
	old := publish("grok-preview.2-1")
	fresh := publish("grok-preview-2.1")
	tests := map[string]struct {
		s  route.Snapshot
		id string
	}{"success: old request": {old, "grok-preview.2-1"}, "success: new request": {fresh, "grok-preview-2.1"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := tt.s.Classify("POST", "/v1/messages", []byte(`{"model":"claude-grok-preview-2-1"}`))
			if diff := gocmp.Diff(route.Decision{Branch: "grok", Provider: "grok", Model: tt.id}, got); diff != "" {
				t.Error(diff)
			}
		})
	}
}

func TestSnapshotPublication(t *testing.T) {
	type generation struct {
		source, catalog, provider, grok string
		local, lmStudio                 int64
	}
	old := generation{"models.json", `[{"id":"old"}]`, "kimi", "grok-preview.2-1", 64000, 81920}
	fresh := generation{"proxy", `[{"id":"new"}]`, "cursor", "grok-preview-2.1", 128000, 163840}
	build := func(g generation) route.Snapshot {
		routes := []proxymodels.Route{{ID: "shared-model", Provider: g.provider}, {ID: "local/gguf", Provider: "local"}, {ID: "lmstudio/shared", Provider: "lmstudio"}}
		r := resolverFor(routes, nil, g.local, map[string]int64{"shared": g.lmStudio})
		return route.NewSnapshot(r, g.source, jsontext.Value(g.catalog), []string{g.grok})
	}
	observe := func(s route.Snapshot, ok bool) (generation, error) {
		if !ok {
			return generation{}, fmt.Errorf("no published snapshot")
		}
		source, data := s.Catalog()
		d := s.Classify("POST", "/v1/messages", []byte(`{"model":"claude-shared-model"}`))
		if d.Branch != "proxy" || d.Model != "shared-model" {
			return generation{}, fmt.Errorf("wrong classification: %+v", d)
		}
		grok := s.Classify("POST", "/v1/messages", []byte(`{"model":"claude-grok-preview-2-1"}`))
		if grok.Branch != "grok" || grok.Provider != "grok" {
			return generation{}, fmt.Errorf("wrong Grok classification: %+v", grok)
		}
		r := s.Resolver()
		return generation{source, string(data), d.Provider, grok.Model, r.AdvertisedWindow("local/gguf"), r.AdvertisedWindow("lmstudio/shared")}, nil
	}
	first, second := build(old), build(fresh)
	var store route.Store
	store.Publish(first)
	held, heldOK := store.Load()
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Go(func() {
		<-start
		for range 512 {
			store.Publish(second)
			store.Publish(first)
		}
		store.Publish(second)
	})
	for range 4 {
		group.Go(func() {
			<-start
			for range 512 {
				got, err := observe(store.Load())
				if err != nil || got != old && got != fresh {
					t.Errorf("partial generation: %+v, err=%v", got, err)
					return
				}
			}
		})
	}
	close(start)
	group.Wait()
	latest, latestOK := store.Load()
	tests := map[string]struct {
		s    route.Snapshot
		ok   bool
		want generation
	}{
		"success: retained request": {held, heldOK, old},
		"success: latest request":   {latest, latestOK, fresh},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := observe(tt.s, tt.ok)
			if err != nil || got != tt.want {
				t.Errorf("got %+v, err=%v; want %+v", got, err, tt.want)
			}
		})
	}
}
