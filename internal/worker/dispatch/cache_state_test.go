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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/dispatch"
)

func TestCacheStateRoundTrip(t *testing.T) {
	tests := map[string]struct {
		stored, want string
		capacity     int
	}{
		"success: duplicate keeps its first LRU position": {
			stored:   `{"version":1,"routes":[["a",{"model":"old","lastUsedAt":1}],["b",{"model":"kept","lastUsedAt":2}],["a",{"model":"new","lastUsedAt":3}]]}`,
			want:     `{"version":1,"routes":[["b",{"model":"kept","ticket":null,"lastUsedAt":2}]]}`,
			capacity: 1,
		},
		"success: duplicate replaces expired value in place": {
			stored:   `{"version":1,"routes":[["a",{"model":"old","lastUsedAt":-100}],["b",{"model":"kept","lastUsedAt":2}],["a",{"model":"new","lastUsedAt":3}]]}`,
			want:     `{"version":1,"routes":[["a",{"model":"new","ticket":null,"lastUsedAt":3}],["b",{"model":"kept","ticket":null,"lastUsedAt":2}]]}`,
			capacity: 2,
		},
		"success: last valid duplicate wins": {
			stored:   `{"version":1,"routes":[["a",{"model":"old","lastUsedAt":1}],["b",{"model":"kept","lastUsedAt":2}],["a",{"model":"new","lastUsedAt":3}],["a",{"model":"UPPER","lastUsedAt":4}]]}`,
			want:     `{"version":1,"routes":[["a",{"model":"new","ticket":null,"lastUsedAt":3}],["b",{"model":"kept","ticket":null,"lastUsedAt":2}]]}`,
			capacity: 2,
		},
		"success: lone surrogate and replacement character remain distinct": {
			stored:   `{"version":1,"routes":[["\ud800",{"model":"lone","lastUsedAt":1}],["�",{"model":"replacement","lastUsedAt":2}]]}`,
			want:     `{"version":1,"routes":[["\ud800",{"model":"lone","ticket":null,"lastUsedAt":1}],["�",{"model":"replacement","ticket":null,"lastUsedAt":2}]]}`,
			capacity: 2,
		},
		"success: empty stored key is retained": {
			stored:   `{"version":1,"routes":[["",{"model":"empty","lastUsedAt":1}]]}`,
			want:     `{"version":1,"routes":[["",{"model":"empty","ticket":null,"lastUsedAt":1}]]}`,
			capacity: 1,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := storedCache(t, tt.stored)
			dispatch.NewCache(path, 100, tt.capacity, func() time.Time { return time.UnixMilli(4) })
			checkFile(t, path, tt.want)
		})
	}
}

func TestCacheGetEffects(t *testing.T) {
	const stored = `{ "version":1,"routes":[["first",{"model":"a","lastUsedAt":1}],["second",{"model":"b","lastUsedAt":1}],["",{"model":"empty","lastUsedAt":2}],["�",{"model":"replacement","lastUsedAt":2}]] }`
	const expiredFirst = `{"version":1,"routes":[["second",{"model":"b","ticket":null,"lastUsedAt":1}],["",{"model":"empty","ticket":null,"lastUsedAt":2}],["�",{"model":"replacement","ticket":null,"lastUsedAt":2}]]}`
	tests := map[string]struct {
		key, wantFile  string
		wantClockCalls int
		rejectSet      bool
	}{
		"success: miss does not read clock or write": {key: "absent", wantFile: stored},
		"success: empty stored key cannot be read":   {key: "", wantFile: stored},
		"success: expiry removes only requested row": {
			key: "first", wantFile: expiredFirst, wantClockCalls: 1,
		},
		"error: invalid UTF-8 cannot alias replacement character": {
			key: "\xff", wantFile: stored, rejectSet: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := storedCache(t, stored)
			ms := int64(2)
			clockCalls := 0
			cache := dispatch.NewCache(path, 100, 4, func() time.Time {
				clockCalls++
				return time.UnixMilli(ms)
			})
			// An accidental write would replace these noncanonical bytes.
			if err := os.WriteFile(path, []byte(stored+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ms = 101
			clockCalls = 0
			checkGet(t, cache, tt.key, nil)
			if tt.rejectSet {
				cache.Set(tt.key, dispatch.CachedRoute{Model: "wrong"})
			}
			checkFile(t, path, tt.wantFile)
			if clockCalls != tt.wantClockCalls {
				t.Errorf("clock calls = %d, want %d", clockCalls, tt.wantClockCalls)
			}
			if tt.rejectSet {
				checkGet(t, cache, "�", &dispatch.CachedRoute{Model: "replacement", EffortMissing: true})
			}
		})
	}
}

func TestCacheEnvelopeRewrite(t *testing.T) {
	tests := map[string]struct{ stored, want string }{
		"success: valid empty envelope is normalized": {
			stored: ` {"routes": [ ], "version": 1.0 } `,
			want:   `{"version":1,"routes":[]}`,
		},
		"error: invalid final routes field is untouched": {
			stored: ` {"version": 1, "routes": [], "routes": null} `,
			want:   ` {"version": 1, "routes": [], "routes": null} `,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			path := storedCache(t, tt.stored+"\n")
			dispatch.NewCache(path, 100, 1, func() time.Time { return time.UnixMilli(4) })
			checkFile(t, path, tt.want)
		})
	}
}

func TestCachePersistenceDirectory(t *testing.T) {
	tests := map[string]struct {
		blockFirstOpen bool
		renameParent   bool
	}{
		"success: construction leaves a missing parent alone": {},
		"success: later writes follow the original directory": {renameParent: true},
		"success: failed first open is retried":               {blockFirstOpen: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			parent := filepath.Join(root, "state")
			path := filepath.Join(parent, "routes.json")
			if tt.blockFirstOpen {
				if err := os.WriteFile(parent, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cache := dispatch.NewCache(path, 100, 2, func() time.Time { return time.UnixMilli(1) })
			if !tt.blockFirstOpen {
				if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stat parent after construction: %v, want not exist", err)
				}
			}
			cache.Set("first", dispatch.CachedRoute{Model: "first"})
			if tt.blockFirstOpen {
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
			} else {
				checkFile(t, path, `{"version":1,"routes":[["first",{"model":"first","effort":null,"ticket":null,"lastUsedAt":1}]]}`)
			}
			wantPath := path
			if tt.renameParent {
				moved := filepath.Join(root, "moved")
				if err := os.Rename(parent, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				wantPath = filepath.Join(moved, "routes.json")
			}
			cache.Set("second", dispatch.CachedRoute{Model: "second"})
			checkFile(t, wantPath, `{"version":1,"routes":[["first",{"model":"first","effort":null,"ticket":null,"lastUsedAt":1}],["second",{"model":"second","effort":null,"ticket":null,"lastUsedAt":1}]]}`)
			if tt.renameParent {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("stat route file in replacement parent: %v, want not exist", err)
				}
			}
		})
	}
}
