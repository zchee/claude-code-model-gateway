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

package upstreamstate_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
)

func TestRateLimitExpiryHeaderForms(t *testing.T) {
	const (
		retry = "Retry-After"
		reset = "Anthropic-Ratelimit-Unified-Reset"
	)
	tests := map[string]struct {
		headers http.Header
		expires string
		active  bool
	}{
		// V8 DateParser::DayComposer::Write maps 00–49 to the 2000s and 50–99 to the 1900s.
		"success: RFC 850 current century":               {http.Header{retry: {"Monday, 28-Sep-26 10:05:00 GMT"}}, "2026-09-28T10:05:00.000Z", true},
		"success: RFC 850 year fifty has already passed": {http.Header{retry: {"Sunday, 01-Jan-50 00:00:00 GMT"}}, "1950-01-01T00:00:00.000Z", false},
		// ECMAScript's Date Time String Format permits HH:mm without seconds.
		"success: ISO time without seconds": {http.Header{retry: {"2026-09-28T10:05Z"}}, "2026-09-28T10:05:00.000Z", true},
		// V8 DateParser::KeywordTable recognizes UTC as a zero-offset zone.
		"success: RFC-style date with UTC zone": {http.Header{retry: {"Mon, 28 Sep 2026 10:05:00 UTC"}}, "2026-09-28T10:05:00.000Z", true},
		// Node discards later Retry-After occurrences but joins custom headers.
		// The resulting reset string is not numeric (lib/codex-upstream-state.js:42-50).
		"success: first retry after wins":          {http.Header{"retry-after": {"30", "45"}}, "2026-09-28T10:00:30.000Z", true},
		"success: uncanonicalized reset":           {http.Header{"anthropic-ratelimit-unified-reset": {"1790597700"}}, "2026-09-28T12:15:00.000Z", true},
		"success: repeated reset uses default":     {http.Header{reset: {"1790597700", "1790597715"}}, "2026-09-28T10:01:00.000Z", true},
		"success: reset keys with different cases": {http.Header{reset: {"1790597700"}, "anthropic-ratelimit-unified-reset": {"1790597715"}}, "2026-09-28T10:01:00.000Z", true},
		"success: unsupported date uses reset":     {http.Header{retry: {"Mon, 28 Sep 2026 10:05:00 XYZ"}, reset: {"1790597700"}}, "2026-09-28T12:15:00.000Z", true},
		// Bare asctime is local in V8; the gateway cannot assign it a portable instant.
		"success: asctime falls to reset":   {http.Header{retry: {"Mon Sep 28 10:05:00 2026"}, reset: {"1790597700"}}, "2026-09-28T12:15:00.000Z", true},
		"success: asctime falls to default": {http.Header{retry: {"Mon Sep 28 10:05:00 2026"}}, "2026-09-28T10:01:00.000Z", true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
			store := storeAt(t, &now)
			block, err := store.SetBlocked(http.StatusTooManyRequests, evidence, tt.headers)
			if err != nil || block == nil {
				t.Fatalf("SetBlocked returned (%v, %v)", block, err)
			}
			if diff := gocmp.Diff(tt.expires, block.ExpiresAt); diff != "" {
				t.Fatalf("expiry (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.active, store.ReadBlocked() != nil); diff != "" {
				t.Fatalf("active block (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStateDecisionMembers(t *testing.T) {
	const observedAtMs = 1790589600000
	// The source checks only state and the relevant timestamp; JSON.parse
	// keeps the last value for a repeated name (lib/codex-upstream-state.js:14-16,28-31,69-72).
	tests := map[string]struct {
		blockJSON, unavailableJSON string
		block                      *upstreamstate.Blocked
		unavailable                *upstreamstate.Unavailable
	}{
		"success: malformed unrelated metadata does not erase a hold": {
			blockJSON:       `{"state":"upstream-blocked","evidence":17,"statusCode":401}`,
			unavailableJSON: `{"state":"upstream-unavailable","observedAtMs":1790589600000,"statusCode":"503"}`,
			block:           &upstreamstate.Blocked{State: "upstream-blocked", StatusCode: 401},
			unavailable:     &upstreamstate.Unavailable{State: "upstream-unavailable", ObservedAtMs: observedAtMs},
		},
		"success: final duplicate wins regardless of earlier types": {
			blockJSON:       `{"state":17,"state":"upstream-blocked","expiresAtMs":0,"expiresAtMs":1790589660000,"evidence":17,"evidence":"last"}`,
			unavailableJSON: `{"state":17,"state":"upstream-unavailable","observedAtMs":"not a number","observedAtMs":1790589600000,"statusCode":"503","statusCode":504}`,
			block:           &upstreamstate.Blocked{State: "upstream-blocked", Evidence: "last", ExpiresAtMs: new(1790589660000.0)},
			unavailable:     &upstreamstate.Unavailable{State: "upstream-unavailable", ObservedAtMs: observedAtMs, StatusCode: 504},
		},
		"error: final decision members have the wrong type": {
			blockJSON:       `{"state":"upstream-blocked","state":true}`,
			unavailableJSON: `{"state":"upstream-unavailable","observedAtMs":1790589600000,"observedAtMs":"1790589600000"}`,
		},
		// JSON.parse produces infinity and the comparison converts singleton arrays
		// to their element's string (lib/codex-upstream-state.js:14-16,28-31).
		"success: negative overflow in an array expires": {
			blockJSON:       `{"state":"upstream-blocked","expiresAtMs":[-1e400]}`,
			unavailableJSON: `null`,
		},
		"success: positive overflow in nested arrays stays active": {
			blockJSON:       `{"state":"upstream-blocked","expiresAtMs":[[1e400]]}`,
			unavailableJSON: `null`,
			block:           &upstreamstate.Blocked{State: "upstream-blocked"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, contents := range map[string]string{blockFile: tt.blockJSON, unavailableFile: tt.unavailableJSON} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(contents+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store := upstreamstate.Store{Dir: dir, Now: func() time.Time { return time.UnixMilli(observedAtMs + 1000) }}
			if diff := gocmp.Diff(tt.block, store.ReadBlocked()); diff != "" {
				t.Fatalf("blocked state (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.unavailable, store.ReadUnavailable()); diff != "" {
				t.Fatalf("unavailable state (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSetUnavailableUsesFreshBlockClock(t *testing.T) {
	const firstMs = 1790589600000
	// setUpstreamUnavailable captures its observation time, then invokes
	// readUpstreamBlocked without a now override (lib/codex-upstream-state.js:28,75-82).
	tests := map[string]struct {
		secondMs int64
		want     bool
	}{
		"success: block expires before the check": {firstMs + 1, true},
		"error: block is still active":            {firstMs, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			const block = `{"state":"upstream-blocked","expiresAtMs":1790589600001}`
			if err := os.WriteFile(filepath.Join(dir, blockFile), []byte(block+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			store := upstreamstate.Store{Dir: dir, Now: func() time.Time {
				calls++
				if calls == 1 {
					return time.UnixMilli(firstMs)
				}
				return time.UnixMilli(tt.secondMs)
			}}
			u, err := store.SetUnavailable(http.StatusServiceUnavailable)
			if err != nil {
				t.Fatal(err)
			}
			if diff := gocmp.Diff(2, calls); diff != "" {
				t.Errorf("clock samples (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.want, u != nil); diff != "" {
				t.Fatalf("wrote unavailable state (-want +got):\n%s", diff)
			}
			if u != nil {
				if diff := gocmp.Diff(int64(firstMs), u.ObservedAtMs); diff != "" {
					t.Errorf("observation time (-want +got):\n%s", diff)
				}
			}
		})
	}
}
