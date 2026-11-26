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
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// startupPolicyLines starts a cache against a healthy proxy listing with the default windows and
// returns the sentry policy lines it logged after its first refresh.
func startupPolicyLines(t *testing.T) []string {
	t.Helper()
	var logs logCapture
	synctest.Test(t, func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-6-astra","display_name":"gpt-6-astra (codex)"}]}`))
		})
		_, lookup, resolver := newTestPaths(t, nil)
		cache, err := modelcache.New(modelcache.Options{
			Lookup:         lookup,
			Paths:          resolver,
			ProxyBaseURL:   "http://127.0.0.1:19991",
			DefaultBaseURL: "http://127.0.0.1:19999",
			Client:         &http.Client{Transport: handlerTransport{handler: handler}},
			Clock:          time.Now,
			Routes:         &route.Store{},
			Sentry:         &sentry.State{},
			Logf:           logs.logf,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer func() {
			cancel()
			cache.Wait()
		}()
		cache.Start(ctx)
		synctest.Wait()
	})
	var lines []string
	for line := range strings.SplitSeq(string(logs.bytes()), "\n") {
		if strings.HasPrefix(line, "model-gateway: sentry policy ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// policyLinesFor returns the logged policy lines of one row. The trailing space keeps a row id from
// matching a longer id that contains it.
func policyLinesFor(lines []string, backendID string) []string {
	id := "sentry policy id=" + backendID + " "
	return slices.DeleteFunc(slices.Clone(lines), func(s string) bool { return !strings.Contains(s, id) })
}

// TestStartupPolicyLineWindow checks the backend window the startup policy lines print. A row with a
// window prints its token count, with or without a sentry; a row without one prints null, as the
// source prints JavaScript's null window, never zero.
func TestStartupPolicyLineWindow(t *testing.T) {
	lines := startupPolicyLines(t)
	if len(lines) == 0 {
		t.Fatal("the cache logged no sentry policy line after its first refresh")
	}

	tests := map[string]struct {
		backendID string
		want      string
	}{
		"success: native Anthropic row without a backend window prints null": {
			backendID: "anthropic",
			want:      "model-gateway: sentry policy id=anthropic backendWindow=null sentry=none",
		},
		"success: grok row prints its window and a derived trigger": {
			backendID: "grok-4.7",
			want:      "model-gateway: sentry policy id=grok-4.7 backendWindow=500000 sentry=synthetic-413 effectiveTrigger=460000 source=derived",
		},
		"success: codex default row prints its window and the default cap trigger": {
			backendID: "default",
			want:      "model-gateway: sentry policy id=default backendWindow=920000 sentry=synthetic-413 effectiveTrigger=187000 source=cap",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if diff := gocmp.Diff([]string{tt.want}, policyLinesFor(lines, tt.backendID)); diff != "" {
				t.Errorf("policy lines for %s mismatch (-want +got):\n%s", tt.backendID, diff)
			}
		})
	}

	t.Run("success: every table row prints its window or null", func(t *testing.T) {
		for _, row := range policy.Rows() {
			got := policyLinesFor(lines, row.BackendID)
			if len(got) != 1 {
				t.Errorf("%s: got %d policy lines %q, want 1", row.BackendID, len(got), got)
				continue
			}
			window := "null"
			if row.BackendWindow != 0 {
				window = strconv.FormatInt(row.BackendWindow, 10)
			}
			want := "sentry policy id=" + row.BackendID + " backendWindow=" + window + " sentry=" + row.Sentry
			if !strings.Contains(got[0], want) {
				t.Errorf("%s: policy line %q does not contain %q", row.BackendID, got[0], want)
			}
		}
	})
}
