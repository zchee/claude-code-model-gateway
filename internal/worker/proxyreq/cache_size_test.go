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

package proxyreq_test

import (
	"net/http"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
)

// TestDispatchCacheMaxSessions checks how the configured session limit reaches the dispatch cache. Small limits
// keep exactly that many sessions. A limit beyond what a native int holds, or beyond the 32-bit range, must still
// leave a working cache: converting such a number to an int without saturating it first is undefined in Go and on
// common targets yields a negative limit, which would silently disable dispatch caching.
func TestDispatchCacheMaxSessions(t *testing.T) {
	cached := turnOutcome{Via: "dispatch-cached", Model: "gpt-5.6-sol", Effort: "high", Ticket: "SQ-1"}
	unbound := turnOutcome{Status: http.StatusBadRequest, Via: "dispatch", Ticket: "<none>"}
	tests := map[string]struct {
		limit         string
		first, second turnOutcome
	}{
		"success: a limit of one keeps only the newest session": {"1", unbound, cached},
		"success: a limit of two keeps both sessions":           {"2", cached, cached},
		"success: the largest 32-bit int":                       {"2147483647", cached, cached},
		"success: just beyond the 32-bit range":                 {"2147483648", cached, cached},
		"success: beyond the unsigned 32-bit range":             {"4294967296", cached, cached},
		"success: the largest 64-bit int spelling":              {"9223372036854775807", cached, cached},
		"success: just beyond the 64-bit range":                 {"9223372036854775808", cached, cached},
		"success: far beyond any int":                           {"1e300", cached, cached},
		"success: the largest finite number":                    {"1.7976931348623157e308", cached, cached},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"CODEX_GATEWAY_DISPATCH_CACHE_MAX_SESSIONS": tc.limit}
			h, err := proxyreq.New(proxyreq.Options{Now: func() time.Time { return time.UnixMilli(1234) }, Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
			if err != nil {
				t.Fatal(err)
			}
			first := http.Header{"X-Claude-Code-Session-Id": {"first"}}
			second := http.Header{"X-Claude-Code-Session-Id": {"second"}}
			briefing := autoBody("[sidequest-route model=gpt-5.6-sol effort=high ticket=SQ-1]")
			for _, headers := range []http.Header{first, second} {
				if got, want := outcomeOf(newPendingTurn(t, "/v1/messages", briefing, headers).prepare(h)), (turnOutcome{Via: "dispatch", Model: "gpt-5.6-sol", Effort: "high", Ticket: "SQ-1"}); got != want {
					t.Fatalf("briefing = %+v, want %+v", got, want)
				}
			}
			// The newest session is checked first so that its lookup does not refresh the older one.
			if diff := gocmp.Diff(tc.second, outcomeOf(newPendingTurn(t, "/v1/messages", autoBody("follow up"), second).prepare(h))); diff != "" {
				t.Errorf("newest session follow-up with limit %s (-want +got): %s", tc.limit, diff)
			}
			if diff := gocmp.Diff(tc.first, outcomeOf(newPendingTurn(t, "/v1/messages", autoBody("follow up"), first).prepare(h))); diff != "" {
				t.Errorf("older session follow-up with limit %s (-want +got): %s", tc.limit, diff)
			}
		})
	}
}
