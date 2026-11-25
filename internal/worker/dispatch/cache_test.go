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

func checkGet(t *testing.T, cache *dispatch.Cache, key string, want *dispatch.Route) {
	t.Helper()
	if diff := gocmp.Diff(want, cache.Get(key)); diff != "" {
		t.Errorf("Get(%q) (-want +got):\n%s", key, diff)
	}
}

func TestCacheIdleTTL(t *testing.T) {
	var ms int64
	cache := dispatch.NewCache("", 100*time.Millisecond, 2, func() time.Time { return time.UnixMilli(ms) })
	route := dispatch.Route{Model: "gpt-5.6-sol", Effort: new("xhigh")}
	cache.Set("session-a", route)
	ms = 50
	checkGet(t, cache, "session-a", &route)
	ms = 149
	checkGet(t, cache, "session-a", &route)
	ms = 249
	checkGet(t, cache, "session-a", nil)
}

func TestCacheEviction(t *testing.T) {
	cache := dispatch.NewCache("", time.Second, 2, func() time.Time { return time.UnixMilli(0) })
	a := dispatch.Route{Model: "gpt-5.6-sol", Effort: new("xhigh")}
	b := dispatch.Route{Model: "gpt-5.6-terra", Effort: new("high")}
	c := dispatch.Route{Model: "gpt-5.6-luna", Effort: new("low")}
	cache.Set("session-a", a)
	cache.Set("session-b", b)
	checkGet(t, cache, "session-a", &a)
	cache.Set("session-c", c)
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
	cache := dispatch.NewCache(path, 100*time.Millisecond, 2, now)
	a := dispatch.Route{Model: "example", Effort: new("high")}
	b := dispatch.Route{Model: "b", Ticket: new("B_1")}
	cache.Set("identity", a)
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
	cache = dispatch.NewCache(path, 100*time.Millisecond, 2, now)
	ms = 101
	checkGet(t, cache, "other", nil)
	checkFile(t, path, `{"version":1,"routes":[["identity",{"model":"example","effort":"high","ticket":null,"lastUsedAt":99}]]}`)
	checkGet(t, cache, "identity", &a)
}

func TestCacheLoadPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch-routes.json")
	const stored = `{"version":1,"routes":[["expired",{"model":"old","lastUsedAt":0}],["lru",{"model":"unused","lastUsedAt":20}],["keep",{"model":"a","effort":"low","lastUsedAt":10}],["bad-model",{"model":"A","lastUsedAt":25}],["bad-time",{"model":"x","lastUsedAt":"90"}],["bad-effort",{"model":"x","effort":"","lastUsedAt":80}],["bad-ticket",{"model":"x","ticket":"-","lastUsedAt":88}],null,["newest",{"model":"b","lastUsedAt":12}]]}`
	if err := os.WriteFile(path, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := dispatch.NewCache(path, 100*time.Millisecond, 2, func() time.Time { return time.UnixMilli(100) })
	checkFile(t, path, `{"version":1,"routes":[["keep",{"model":"a","effort":"low","ticket":null,"lastUsedAt":10}],["newest",{"model":"b","ticket":null,"lastUsedAt":12}]]}`)
	checkGet(t, cache, "keep", &dispatch.Route{Model: "a", Effort: new("low")})
	checkGet(t, cache, "newest", &dispatch.Route{Model: "b"})
}

func TestCacheRejectsInvalidSets(t *testing.T) {
	tests := map[string]struct {
		key   string
		route dispatch.Route
	}{
		"error: empty key":      {"", dispatch.Route{Model: "valid"}},
		"error: invalid model":  {"session", dispatch.Route{Model: "UPPER"}},
		"error: invalid effort": {"session", dispatch.Route{Model: "valid", Effort: new("")}},
		"error: invalid ticket": {"session", dispatch.Route{Model: "valid", Ticket: new("1bad")}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cache := dispatch.NewCache("", time.Second, 1, func() time.Time { return time.UnixMilli(0) })
			original := dispatch.Route{Model: "original"}
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
	cache := dispatch.NewCache(filepath.Join(blocker, "routes.json"), time.Second, 1, func() time.Time { return time.UnixMilli(0) })
	route := dispatch.Route{Model: "example"}
	cache.Set("session", route)
	checkGet(t, cache, "session", &route)
}
