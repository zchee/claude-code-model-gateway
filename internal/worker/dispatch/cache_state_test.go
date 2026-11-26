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
		"success: lone surrogate and replacement character remain distinct": {
			stored:   `{"version":1,"routes":[["\ud800",{"model":"lone","lastUsedAt":1}],["�",{"model":"replacement","lastUsedAt":2}]]}`,
			want:     `{"version":1,"routes":[["\ud800",{"model":"lone","ticket":null,"lastUsedAt":1}],["�",{"model":"replacement","ticket":null,"lastUsedAt":2}]]}`,
			capacity: 2,
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
