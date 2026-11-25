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

package dispatch_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/dispatch"
)

func checkGet(t *testing.T, cache *dispatch.Cache, key string, want *dispatch.CachedRoute) {
	t.Helper()
	if diff := gocmp.Diff(want, cache.Get(key)); diff != "" {
		t.Errorf("Get(%q) (-want +got):\n%s", key, diff)
	}
}

func TestCacheIdleTTL(t *testing.T) {
	var ms int64
	cache := dispatch.NewCache("", 100, 2, func() time.Time { return time.UnixMilli(ms) })
	cache.Set("session-a", dispatch.CachedRoute{Model: "gpt-5.6-sol", Effort: new("xhigh")})
	route := dispatch.CachedRoute{Model: "gpt-5.6-sol", Effort: new("xhigh")}
	ms = 50
	checkGet(t, cache, "session-a", &route)
	ms = 149
	checkGet(t, cache, "session-a", &route)
	ms = 249
	checkGet(t, cache, "session-a", nil)
}

func TestCacheEviction(t *testing.T) {
	cache := dispatch.NewCache("", 1000, 2, func() time.Time { return time.UnixMilli(0) })
	cache.Set("session-a", dispatch.CachedRoute{Model: "gpt-5.6-sol", Effort: new("xhigh")})
	cache.Set("session-b", dispatch.CachedRoute{Model: "gpt-5.6-terra", Effort: new("high")})
	a := dispatch.CachedRoute{Model: "gpt-5.6-sol", Effort: new("xhigh")}
	checkGet(t, cache, "session-a", &a)
	cache.Set("session-c", dispatch.CachedRoute{Model: "gpt-5.6-luna", Effort: new("low")})
	c := dispatch.CachedRoute{Model: "gpt-5.6-luna", Effort: new("low")}
	checkGet(t, cache, "session-b", nil)
	checkGet(t, cache, "session-a", &a)
	checkGet(t, cache, "session-c", &c)
}

func checkFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted routes: %v", err)
	}
	if diff := gocmp.Diff(want+"\n", string(b)); diff != "" {
		t.Errorf("persisted bytes (-want +got):\n%s", diff)
	}
}

func TestCachePersistsReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "dispatch-routes.json")
	ms := int64(1)
	now := func() time.Time { return time.UnixMilli(ms) }
	cache := dispatch.NewCache(path, 100, 2, now)
	a := dispatch.CachedRoute{Model: "example", Effort: new("high")}
	b := dispatch.CachedRoute{Model: "b", Ticket: new("B_1")}
	cache.Set("identity", dispatch.CachedRoute{Model: "example", Effort: new("high")})
	checkFile(t, path, `{"version":1,"routes":[["identity",{"model":"example","effort":"high","ticket":null,"lastUsedAt":1}]]}`)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cache permissions = %o, want 600", info.Mode().Perm())
	}
	cache.Set("other", b)
	ms = 99
	checkGet(t, cache, "identity", &a)
	checkFile(t, path, `{"version":1,"routes":[["other",{"model":"b","effort":null,"ticket":"B_1","lastUsedAt":1}],["identity",{"model":"example","effort":"high","ticket":null,"lastUsedAt":99}]]}`)
	cache = dispatch.NewCache(path, 100, 2, now)
	ms = 101
	checkGet(t, cache, "other", nil)
	checkFile(t, path, `{"version":1,"routes":[["identity",{"model":"example","effort":"high","ticket":null,"lastUsedAt":99}]]}`)
	checkGet(t, cache, "identity", &a)
}

func TestCacheLoadPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch-routes.json")
	const stored = `{"version":1,"routes":[["lru",{"model":"unused","lastUsedAt":20}],["keep",{"model":"a","effort":"low","lastUsedAt":10}],["bad-model",{"model":"A","lastUsedAt":25}],["bad-time",{"model":"x","lastUsedAt":"90"}],["bad-effort",{"model":"x","effort":"","lastUsedAt":80}],["bad-ticket",{"model":"x","ticket":"-","lastUsedAt":88}],null,["newest",{"model":"b","lastUsedAt":12}],["expired",{"model":"old","lastUsedAt":0}]]}`
	if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := dispatch.NewCache(path, 100, 2, func() time.Time { return time.UnixMilli(100) })
	checkFile(t, path, `{"version":1,"routes":[["keep",{"model":"a","effort":"low","ticket":null,"lastUsedAt":10}],["newest",{"model":"b","ticket":null,"lastUsedAt":12}]]}`)
	checkGet(t, cache, "keep", &dispatch.CachedRoute{Model: "a", Effort: new("low")})
	checkGet(t, cache, "newest", &dispatch.CachedRoute{Model: "b", EffortMissing: true})
}

func storedCache(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routes.json")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCacheCopiesEffortPresence(t *testing.T) {
	tests := map[string]struct {
		stored, copied string
		missing        bool
	}{
		"success: omitted effort":       {`{"model":"m","lastUsedAt":1}`, `{"model":"m","ticket":null,"lastUsedAt":2}`, true},
		"success: explicit null effort": {`{"model":"m","effort":null,"lastUsedAt":1}`, `{"model":"m","effort":null,"ticket":null,"lastUsedAt":2}`, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := storedCache(t, `{"version":1,"routes":[["parent",`+tt.stored+`]]}`)
			cache := dispatch.NewCache(path, 100, 2, func() time.Time { return time.UnixMilli(2) })
			got := cache.Get("parent")
			if diff := gocmp.Diff(&dispatch.CachedRoute{Model: "m", EffortMissing: tt.missing}, got); diff != "" {
				t.Fatalf("loaded route (-want +got):\n%s", diff)
			}
			cache.Set("child", *got)
			checkFile(t, path, `{"version":1,"routes":[["parent",`+tt.copied+`],["child",`+tt.copied+`]]}`)
		})
	}
}

func TestCacheLoadRejectsMalformedRows(t *testing.T) {
	t.Run("non-string tickets", func(t *testing.T) {
		path := storedCache(t, `{"version":1,"routes":[["bool",{"model":"m","ticket":true,"lastUsedAt":1}],["array",{"model":"m","ticket":["Valid"],"lastUsedAt":1}],["keep",{"model":"m","ticket":"Valid","lastUsedAt":1}]]}`)
		cache := dispatch.NewCache(path, 100, 4, func() time.Time { return time.UnixMilli(2) })
		checkFile(t, path, `{"version":1,"routes":[["keep",{"model":"m","ticket":"Valid","lastUsedAt":1}]]}`)
		checkGet(t, cache, "bool", nil)
		checkGet(t, cache, "array", nil)
		checkGet(t, cache, "keep", &dispatch.CachedRoute{Model: "m", Ticket: new("Valid"), EffortMissing: true})
	})
}

func TestCacheSetSweepsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routes.json")
	var ms int64
	cache := dispatch.NewCache(path, 100, 4, func() time.Time { return time.UnixMilli(ms) })
	cache.Set("old", dispatch.CachedRoute{Model: "old"})
	checkFile(t, path, `{"version":1,"routes":[["old",{"model":"old","effort":null,"ticket":null,"lastUsedAt":0}]]}`)
	ms = 100
	cache.Set("new", dispatch.CachedRoute{Model: "new"})
	checkFile(t, path, `{"version":1,"routes":[["new",{"model":"new","effort":null,"ticket":null,"lastUsedAt":100}]]}`)
}

func TestCacheLoadLeavesInvalidEnvelope(t *testing.T) {
	tests := map[string]struct{ data string }{
		"error: unsupported version": {`{ "version":2, "routes":[["session",{"model":"m","lastUsedAt":1}]] }`},
		"error: routes not array":    {`{ "version":1, "routes":{} }`},
		"error: incomplete document": {`{"version":1,"routes":[`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := storedCache(t, tt.data)
			cache := dispatch.NewCache(path, 100, 2, func() time.Time { return time.UnixMilli(2) })
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(tt.data, string(b)); diff != "" {
				t.Errorf("load changed file (-want +got):\n%s", diff)
			}
			checkGet(t, cache, "session", nil)
		})
	}
}

func TestCacheTTLInMilliseconds(t *testing.T) {
	tests := map[string]struct {
		ttl  float64
		age  int64
		live bool
	}{
		"success: before fractional threshold": {1.25, 1, true},
		"error: beyond fractional threshold":   {1.25, 2, false},
		"success: beyond duration range":       {1e13, 9_999_999_999_999, true},
		"error: at large threshold":            {1e13, 10_000_000_000_000, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var ms int64
			cache := dispatch.NewCache("", tt.ttl, 1, func() time.Time { return time.UnixMilli(ms) })
			cache.Set("session", dispatch.CachedRoute{Model: "m"})
			ms = tt.age
			var want *dispatch.CachedRoute
			if tt.live {
				want = &dispatch.CachedRoute{Model: "m"}
			}
			checkGet(t, cache, "session", want)
		})
	}
}

func TestCacheGetReturnsIndependentRoute(t *testing.T) {
	cache := dispatch.NewCache("", 100, 1, func() time.Time { return time.UnixMilli(0) })
	cache.Set("session", dispatch.CachedRoute{Model: "m", Effort: new("high"), Ticket: new("Ticket")})
	got := cache.Get("session")
	want := &dispatch.CachedRoute{Model: "m", Effort: new("high"), Ticket: new("Ticket")}
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("original route (-want +got):\n%s", diff)
	}
	got.Model = "changed"
	*got.Effort = "low"
	*got.Ticket = "Other"
	checkGet(t, cache, "session", want)
}

func TestCacheRejectsInvalidSets(t *testing.T) {
	tests := map[string]struct {
		key   string
		route dispatch.CachedRoute
	}{
		"error: empty key":      {"", dispatch.CachedRoute{Model: "valid"}},
		"error: invalid model":  {"session", dispatch.CachedRoute{Model: "UPPER"}},
		"error: invalid effort": {"session", dispatch.CachedRoute{Model: "valid", Effort: new("")}},
		"error: invalid ticket": {"session", dispatch.CachedRoute{Model: "valid", Ticket: new("1bad")}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cache := dispatch.NewCache("", 1000, 1, func() time.Time { return time.UnixMilli(0) })
			original := dispatch.CachedRoute{Model: "original"}
			cache.Set("session", original)
			cache.Set(tt.key, tt.route)
			checkGet(t, cache, "", nil)
			checkGet(t, cache, "session", &original)
		})
	}
}

func TestCacheWriteFailureKeepsRoute(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := dispatch.NewCache(filepath.Join(blocker, "routes.json"), 1000, 1, func() time.Time { return time.UnixMilli(0) })
	route := dispatch.CachedRoute{Model: "example"}
	cache.Set("session", route)
	checkGet(t, cache, "session", &route)
}
