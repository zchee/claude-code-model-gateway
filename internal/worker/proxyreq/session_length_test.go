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
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
)

func metadataBody(content, userID string) string {
	return fmt.Sprintf(`{"model":"claude-codex-auto","messages":[{"role":"user","content":%q}],"metadata":{"user_id":%q}}`, content, userID)
}

// TestMetadataSessionLength checks the 128-character limit of a session taken from metadata.user_id, in both the
// JSON spelling and the text spelling whose session follows the last "_session_".
func TestMetadataSessionLength(t *testing.T) {
	longest := "s" + strings.Repeat("a", 127)
	tooLong := longest + "a"
	tests := map[string]struct {
		userID, session string
	}{
		"success: JSON session of 128 characters":  {fmt.Sprintf(`{"session_id":%q}`, longest), longest},
		"success: text session of 128 characters":  {"user_session_" + longest, longest},
		"error: JSON session of 129 characters":    {fmt.Sprintf(`{"session_id":%q}`, tooLong), ""},
		"error: text session of 129 characters":    {"user_session_" + tooLong, ""},
		"error: text session after a short prefix": {"_session_" + tooLong, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h, err := proxyreq.New(proxyreq.Options{Now: func() time.Time { return time.UnixMilli(1234) }})
			if err != nil {
				t.Fatal(err)
			}
			res, err := newPendingTurn(t, "/v1/messages", metadataBody("[sidequest-route model=gpt-5.6-sol]", tc.userID), nil).prepare(h)
			if err != nil || res.Audit == nil || res.LocalStatus != 0 {
				t.Fatalf("briefing: error %v, local status %d, audit %+v", err, res.LocalStatus, res.Audit)
			}
			source := ""
			if tc.session != "" {
				source = "metadata"
			}
			if res.Audit.SessionID != tc.session || res.Audit.SessionSource != source {
				t.Errorf("briefing audit session %q from %q, want %q from %q", res.Audit.SessionID, res.Audit.SessionSource, tc.session, source)
			}
			want := turnOutcome{Via: "dispatch-cached", Model: "gpt-5.6-sol", Ticket: "<none>"}
			if tc.session == "" {
				want = turnOutcome{Status: http.StatusBadRequest, Via: "dispatch", Ticket: "<none>"}
			}
			if diff := gocmp.Diff(want, outcomeOf(newPendingTurn(t, "/v1/messages", metadataBody("follow up", tc.userID), nil).prepare(h))); diff != "" {
				t.Errorf("follow-up (-want +got): %s", diff)
			}
		})
	}
}

// TestOverlongMetadataSessionAllocation checks that a metadata session longer than the 128-character limit is
// refused before anything is allocated for it. Each pair of bodies has the same length and differs only in whether
// the candidate session is about a mebibyte long or empty; both are refused the same way, so the extra bytes the
// long candidate costs are what preparing it allocates for the candidate itself.
func TestOverlongMetadataSessionAllocation(t *testing.T) {
	const n = 1 << 20
	filler := strings.Repeat("a", n)
	tests := map[string]struct {
		long, empty string
	}{
		"success: text spelling": {"u_session_" + filler, "u" + filler + "_session_"},
		"success: JSON spelling": {`{"session_id":"` + filler + `"}`, `{"pad":"` + filler + `","session_id":""}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h, err := proxyreq.New(proxyreq.Options{Now: func() time.Time { return time.UnixMilli(1234) }})
			if err != nil {
				t.Fatal(err)
			}
			allocated := func(userID string) uint64 {
				t.Helper()
				least := uint64(math.MaxUint64)
				for range 5 {
					turn := newPendingTurn(t, "/v1/messages", metadataBody("follow up", userID), nil)
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					res, err := turn.prepare(h)
					runtime.ReadMemStats(&after)
					if err != nil || res.LocalStatus != http.StatusBadRequest || res.Audit == nil || res.Audit.SessionID != "" {
						t.Fatalf("turn without a usable session: error %v, local status %d, audit %+v", err, res.LocalStatus, res.Audit)
					}
					least = min(least, after.TotalAlloc-before.TotalAlloc)
				}
				return least
			}
			long, empty := allocated(tc.long), allocated(tc.empty)
			t.Logf("bytes allocated by Prepare: %d with a %d-character candidate, %d with an empty one", long, n, empty)
			if long > empty && long-empty >= n/2 {
				t.Errorf("an overlong session candidate cost %d more bytes than an empty one; want less than %d", long-empty, n/2)
			}
		})
	}
}
