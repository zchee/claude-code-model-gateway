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

package proxymodels_test

import (
	"slices"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

func TestAdvertisedRoutes(t *testing.T) {
	bare := proxymodels.Route{ID: "gemini-4-argon", Provider: "vertex"}
	twin := proxymodels.Route{ID: "vertex/gemini-4-argon", Provider: "vertex"}
	untwinned := []proxymodels.Route{
		{ID: "vertex/gemini-9-neon", Provider: "vertex"},
		{ID: "vertex/anthropic/claude-opus-5-5", Provider: "vertex"},
		{ID: "vertex/endpoint/my-llm", Provider: "vertex"},
	}
	otherBare := proxymodels.Route{ID: bare.ID, Provider: "gemini"}
	otherTwin := proxymodels.Route{ID: twin.ID, Provider: "opencode"}
	tests := map[string]struct{ routes, want []proxymodels.Route }{
		"success: drop Vertex twin":                        {[]proxymodels.Route{bare, twin}, []proxymodels.Route{bare}},
		"success: twin before bare":                        {[]proxymodels.Route{twin, bare}, []proxymodels.Route{bare}},
		"success: vertex routes without twins":             {untwinned, slices.Clone(untwinned)},
		"success: keep repeated bare routes":               {[]proxymodels.Route{bare, bare, twin}, []proxymodels.Route{bare, bare}},
		"success: bare id belongs to another provider":     {[]proxymodels.Route{otherBare, twin}, []proxymodels.Route{otherBare, twin}},
		"success: prefixed id belongs to another provider": {[]proxymodels.Route{bare, otherTwin}, []proxymodels.Route{bare, otherTwin}},
		"success: empty listing":                           {nil, []proxymodels.Route{}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			table := proxymodels.NewTable(tt.routes)
			if diff := gocmp.Diff(tt.want, proxymodels.AdvertisedRoutes(tt.routes)); diff != "" {
				t.Errorf("AdvertisedRoutes (-want +got):\n%s", diff)
			}
			for _, route := range tt.routes {
				if diff := gocmp.Diff(route.Provider, table.Provider(route.ID)); diff != "" {
					t.Errorf("Provider(%q) after advertising (-want +got):\n%s", route.ID, diff)
				}
			}
		})
	}
}

func TestTable(t *testing.T) {
	tests := map[string]struct {
		routes, want []proxymodels.Route
		providers    map[string]string
	}{
		"success: last provider at first position": {
			routes:    []proxymodels.Route{{ID: "k3", Provider: "kimi"}, {ID: "gpt-5.4", Provider: "other"}, {ID: "k3", Provider: "opencode"}},
			want:      []proxymodels.Route{{ID: "k3", Provider: "opencode"}, {ID: "gpt-5.4", Provider: "other"}},
			providers: map[string]string{"k3": "opencode", "gpt-5.4": "codex", "gpt-other": "codex", "missing": ""},
		},
		"success: empty snapshot": {want: []proxymodels.Route{}, providers: map[string]string{"gpt-5.4": "codex", "k3": "", "": ""}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			table := proxymodels.NewTable(tt.routes)
			if diff := gocmp.Diff(tt.want, table.Routes()); diff != "" {
				t.Errorf("Routes (-want +got):\n%s", diff)
			}
			for id, want := range tt.providers {
				if diff := gocmp.Diff(want, table.Provider(id)); diff != "" {
					t.Errorf("Provider(%q) (-want +got):\n%s", id, diff)
				}
			}
		})
	}
}

func TestAdvertisedPart(t *testing.T) {
	const original = "vertex/anthropic/claude-fable-5-2"
	const renamed = "vertex/anthropic/fable-5-2"
	route := proxymodels.Route{ID: original, Provider: "vertex"}
	collision := proxymodels.Route{ID: renamed, Provider: "vertex"}
	tests := map[string]struct {
		routes                  []proxymodels.Route
		id, advertised, reverse string
	}{
		"success: listed Fable":                                 {[]proxymodels.Route{route}, original, renamed, original},
		"success: unlisted Fable spelling has no reverse route": {nil, original, renamed, ""},
		"success: replace every occurrence":                     {[]proxymodels.Route{{ID: "x/claude-fable-1/claude-fable-2", Provider: "x"}}, "x/claude-fable-1/claude-fable-2", "x/fable-1/fable-2", "x/claude-fable-1/claude-fable-2"},
		"success: not a Fable":                                  {nil, "vertex/anthropic/claude-opus-5-5", "vertex/anthropic/claude-opus-5-5", ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			table := proxymodels.NewTable(tt.routes)
			if diff := gocmp.Diff(tt.advertised, table.AdvertisedPart(tt.id)); diff != "" {
				t.Errorf("AdvertisedPart(%q) (-want +got):\n%s", tt.id, diff)
			}
			got, ok := table.RouteIDForAdvertisedPart(tt.advertised)
			if diff := gocmp.Diff(tt.reverse, got); diff != "" || ok != (tt.reverse != "") {
				t.Errorf("RouteIDForAdvertisedPart(%q) = (%q, %t), want (%q, %t)", tt.advertised, got, ok, tt.reverse, tt.reverse != "")
			}
			if _, ok := table.RouteIDForAdvertisedPart(tt.id); ok {
				t.Errorf("backend ID %q unexpectedly resolved as an alias", tt.id)
			}
		})
	}

	collisions := map[string]struct{ routes []proxymodels.Route }{
		"success: collision after original":  {[]proxymodels.Route{route, collision}},
		"success: collision before original": {[]proxymodels.Route{collision, route}},
	}
	for name, tt := range collisions {
		t.Run(name, func(t *testing.T) {
			table := proxymodels.NewTable(tt.routes)
			for _, id := range []string{original, renamed} {
				if diff := gocmp.Diff(id, table.AdvertisedPart(id)); diff != "" {
					t.Errorf("AdvertisedPart(%q) (-want +got):\n%s", id, diff)
				}
				if got, ok := table.RouteIDForAdvertisedPart(id); ok || got != "" {
					t.Errorf("RouteIDForAdvertisedPart(%q) = (%q, %t), want no alias", id, got, ok)
				}
			}
			if diff := gocmp.Diff("vertex", table.Provider(original)); diff != "" {
				t.Errorf("Provider(%q) (-want +got):\n%s", original, diff)
			}
		})
	}
}

func TestSharedAdvertisedPart(t *testing.T) {
	first := proxymodels.Route{ID: "x/claude-fable-a/fable-b", Provider: "x"}
	second := proxymodels.Route{ID: "x/fable-a/claude-fable-b", Provider: "x"}
	const part = "x/fable-a/fable-b"
	tests := map[string]struct {
		routes []proxymodels.Route
		wantID string
	}{
		"success: last distinct id wins":    {[]proxymodels.Route{first, second, first}, second.ID},
		"success: reversed insertion order": {[]proxymodels.Route{second, first, second}, first.ID},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			table := proxymodels.NewTable(tt.routes)
			for _, id := range []string{first.ID, second.ID} {
				if diff := gocmp.Diff(part, table.AdvertisedPart(id)); diff != "" {
					t.Errorf("AdvertisedPart(%q) (-want +got):\n%s", id, diff)
				}
			}
			if id, ok := table.RouteIDForAdvertisedPart(part); id != tt.wantID || !ok {
				t.Errorf("RouteIDForAdvertisedPart(%q) = (%q, %t), want (%q, true)", part, id, ok, tt.wantID)
			}
		})
	}
}

func TestTableSnapshots(t *testing.T) {
	oldRoute := proxymodels.Route{ID: "x/claude-fable-a", Provider: "x"}
	newRoute := proxymodels.Route{ID: "k3", Provider: "kimi"}
	const alias = "x/fable-a"
	input := []proxymodels.Route{oldRoute}
	first := proxymodels.NewTable(input)
	input[0] = newRoute
	second := proxymodels.NewTable(input)
	input[0] = proxymodels.Route{}

	tests := map[string]struct {
		table              proxymodels.Table
		want               proxymodels.Route
		missingID, aliasID string
	}{
		"success: old snapshot unchanged":                     {first, oldRoute, newRoute.ID, oldRoute.ID},
		"success: replacement has no stale routes or aliases": {second, newRoute, oldRoute.ID, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.table.Provider(tt.missingID); got != "" {
				t.Errorf("Provider(%q) = %q, want no provider", tt.missingID, got)
			}
			if diff := gocmp.Diff(tt.want.Provider, tt.table.Provider(tt.want.ID)); diff != "" {
				t.Errorf("Provider(%q) (-want +got):\n%s", tt.want.ID, diff)
			}
			if id, ok := tt.table.RouteIDForAdvertisedPart(alias); id != tt.aliasID || ok != (tt.aliasID != "") {
				t.Errorf("RouteIDForAdvertisedPart(%q) = (%q, %t), want (%q, %t)", alias, id, ok, tt.aliasID, tt.aliasID != "")
			}
			routes := tt.table.Routes()
			if diff := gocmp.Diff([]proxymodels.Route{tt.want}, routes); diff != "" {
				t.Fatalf("Routes after changing input (-want +got):\n%s", diff)
			}
			routes[0] = proxymodels.Route{}
			if diff := gocmp.Diff([]proxymodels.Route{tt.want}, tt.table.Routes()); diff != "" {
				t.Errorf("Routes after changing result (-want +got):\n%s", diff)
			}
		})
	}
}
