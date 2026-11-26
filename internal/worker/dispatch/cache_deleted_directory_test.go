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

func TestCacheDeletedDirectory(t *testing.T) {
	tests := map[string]struct {
		tryBeforeRecreation bool
	}{
		"success: update does not recreate the deleted directory":   {tryBeforeRecreation: true},
		"success: replacement directory is present on first update": {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			parent := filepath.Join(t.TempDir(), "state")
			path := filepath.Join(parent, "routes.json")
			clock := func() time.Time { return time.UnixMilli(1) }
			cache := dispatch.NewCache(path, 100, 2, clock)
			first := dispatch.CachedRoute{Model: "first"}
			cache.Set("first", first)
			checkFile(t, path, `{"version":1,"routes":[["first",{"model":"first","effort":null,"ticket":null,"lastUsedAt":1}]]}`)

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(parent); err != nil {
				t.Fatal(err)
			}
			if tt.tryBeforeRecreation {
				checkGet(t, cache, "first", &first)
				if _, err := os.Lstat(parent); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("cache update recreated the deleted directory: Lstat(%q) = %v", parent, err)
				}
			}
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}

			second := dispatch.CachedRoute{Model: "second"}
			cache.Set("second", second)
			checkGet(t, cache, "first", &first)
			checkGet(t, cache, "second", &second)
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cache wrote to the replacement directory before restart: Lstat(%q) = %v", path, err)
			}

			restarted := dispatch.NewCache(path, 100, 2, clock)
			restarted.Set("second", second)
			checkFile(t, path, `{"version":1,"routes":[["second",{"model":"second","effort":null,"ticket":null,"lastUsedAt":1}]]}`)
		})
	}
}
