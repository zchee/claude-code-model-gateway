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

func TestSnapshotPublication(t *testing.T) {
	type generation struct {
		source, catalog, provider string
		window                    int64
	}
	old := generation{"models.json", `[{"id":"old"}]`, "kimi", 64000}
	fresh := generation{"proxy", `[{"id":"new"}]`, "cursor", 128000}
	build := func(g generation) route.Snapshot {
		routes := []proxymodels.Route{{ID: "shared-model", Provider: g.provider}, {ID: "local/gguf", Provider: "local"}}
		r := resolverFor(routes, nil, g.window, nil)
		return route.NewSnapshot(r, g.source, jsontext.Value(g.catalog), nil)
	}
	observe := func(s *route.Snapshot) (generation, error) {
		if s == nil {
			return generation{}, fmt.Errorf("no published snapshot")
		}
		source, data := s.Catalog()
		d := s.Classify("POST", "/v1/messages", []byte(`{"model":"claude-shared-model"}`))
		if d.Branch != "proxy" || d.Model != "shared-model" {
			return generation{}, fmt.Errorf("wrong classification: %+v", d)
		}
		return generation{source, string(data), d.Provider, s.Resolver().AdvertisedWindow("local/gguf")}, nil
	}
	first, second := build(old), build(fresh)
	var store route.Store
	store.Publish(first)
	held := store.Load()
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
	tests := map[string]struct {
		s    *route.Snapshot
		want generation
	}{"success: retained request": {held, old}, "success: latest request": {store.Load(), fresh}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := observe(tt.s)
			if err != nil || got != tt.want {
				t.Errorf("got %+v, err=%v; want %+v", got, err, tt.want)
			}
		})
	}
}
