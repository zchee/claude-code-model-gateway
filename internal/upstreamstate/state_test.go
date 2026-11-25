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
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

const (
	blockFile       = "codex-upstream-blocked.json"
	unavailableFile = "codex-upstream-unavailable.json"
	evidence        = "headers:x-openai-request-id"
)

func storeAt(t *testing.T, now *time.Time) upstreamstate.Store {
	t.Helper()
	return upstreamstate.Store{Dir: filepath.Join(t.TempDir(), "state"), Now: func() time.Time { return *now }}
}

func checkFile(t *testing.T, file, want string) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(want+"\n", string(data)); diff != "" {
		t.Fatalf("%s (-want +got):\n%s", file, diff)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if diff := gocmp.Diff(fs.FileMode(0o600), info.Mode().Perm()); diff != "" {
			t.Fatalf("%s mode (-want +got):\n%s", file, diff)
		}
	}
}

func TestRateLimitExpiry(t *testing.T) {
	tests := map[string]struct{ retryAfter, reset, want string }{
		"success: seconds":                             {retryAfter: "30", want: "2026-09-28T10:00:30.000Z"},
		"success: HTTP date":                           {retryAfter: "Mon, 28 Sep 2026 10:05:00 GMT", want: "2026-09-28T10:05:00.000Z"},
		"success: proxy reset":                         {reset: "1790597700", want: "2026-09-28T12:15:00.000Z"},
		"success: fallback":                            {want: "2026-09-28T10:01:00.000Z"},
		"success: retry after takes priority":          {retryAfter: "30", reset: "1790597700", want: "2026-09-28T10:00:30.000Z"},
		"success: invalid retry after uses reset":      {retryAfter: "not a date", reset: "1790597700", want: "2026-09-28T12:15:00.000Z"},
		"success: negative seconds expire immediately": {retryAfter: "-2", want: "2026-09-28T10:00:00.000Z"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
			store := storeAt(t, &now)
			headers := make(http.Header)
			if tt.retryAfter != "" {
				headers.Set("Retry-After", tt.retryAfter)
			}
			if tt.reset != "" {
				headers.Set("Anthropic-Ratelimit-Unified-Reset", tt.reset)
			}
			record, err := store.SetBlocked(429, evidence, headers)
			if err != nil {
				t.Fatal(err)
			}
			if record == nil {
				t.Fatal("SetBlocked returned no record")
			}
			if diff := gocmp.Diff(tt.want, record.ExpiresAt); diff != "" {
				t.Fatalf("expiresAt (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBlockLifetime(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	store := storeAt(t, &now)
	block, err := store.SetBlocked(429, evidence, nil)
	if err != nil {
		t.Fatal(err)
	}
	if block == nil {
		t.Fatal("no rate limit returned")
	}
	checkFile(t, filepath.Join(store.Dir, blockFile), `{"state":"upstream-blocked","observedAt":"2026-09-28T10:00:00.000Z","statusCode":429,"evidence":"headers:x-openai-request-id","expiresAt":"2026-09-28T10:01:00.000Z","expiresAtMs":1790589660000}`)
	if message := block.ReadinessMessage("gw"); !strings.Contains(message, "rate-limited by OpenAI (429) until 2026-09-28T10:01:00.000Z. The block lifts by itself then") {
		t.Fatalf("missing expiry guidance: %q", message)
	}
	now = now.Add(60*time.Second - time.Millisecond)
	if active := store.ReadBlocked(); active == nil || active.State != "upstream-blocked" {
		t.Fatalf("block before expiry = %+v", active)
	}
	now = now.Add(time.Millisecond)
	if active := store.ReadBlocked(); active != nil {
		t.Fatalf("block at expiry = %+v", active)
	}
}

func TestCredentialBlock(t *testing.T) {
	now := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	store := storeAt(t, &now)
	headers := make(http.Header)
	headers.Set("Retry-After", "30")
	block, err := store.SetBlocked(401, evidence, headers)
	if err != nil {
		t.Fatal(err)
	}
	if block == nil {
		t.Fatal("no credential block returned")
	}
	if block.ExpiresAt != "" || block.ExpiresAtMs != nil {
		t.Fatalf("credential block has expiry: %+v", block)
	}
	file := filepath.Join(store.Dir, blockFile)
	checkFile(t, file, `{"state":"upstream-blocked","observedAt":"2026-09-28T10:00:00.000Z","statusCode":401,"evidence":"headers:x-openai-request-id"}`)
	now = now.Add(24 * time.Hour)
	if active := store.ReadBlocked(); active == nil || active.State != "upstream-blocked" {
		t.Fatalf("credential block the next day = %+v", active)
	}
	msg := block.ReadinessMessage("gw")
	if !strings.Contains(msg, "Run `gw setup`") && !strings.Contains(msg, "Run `\"gw\" setup`") {
		t.Fatalf("missing direct stable binary setup guidance: %q", msg)
	}
	unavailable, err := store.SetUnavailable(503)
	if err != nil || unavailable != nil {
		t.Fatalf("SetUnavailable with active block = (%+v, %v)", unavailable, err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, unavailableFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unavailable file with active block: %v", err)
	}
	store.ClearBlocked()
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("block file after clear: %v", err)
	}
}

func TestUnavailableLifetime(t *testing.T) {
	now := time.Date(2026, 9, 23, 15, 5, 55, 807_000_000, time.UTC)
	store := storeAt(t, &now)
	u, err := store.SetUnavailable(503)
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("SetUnavailable returned no record")
	}
	if diff := gocmp.Diff("2026-09-23T15:06:25.807Z", u.ExpiresAt); diff != "" {
		t.Fatalf("expiresAt (-want +got):\n%s", diff)
	}
	file := filepath.Join(store.Dir, unavailableFile)
	checkFile(t, file, `{"state":"upstream-unavailable","observedAt":"2026-09-23T15:05:55.807Z","observedAtMs":1790175955807,"expiresAt":"2026-09-23T15:06:25.807Z","statusCode":503}`)
	if msg := u.ReadinessMessage(); !strings.Contains(msg, "terminal upstream failure (HTTP 503) at 2026-09-23T15:05:55.807Z. Dispatch holds until 2026-09-23T15:06:25.807Z, or until a Codex request succeeds") {
		t.Fatalf("missing failure guidance: %q", msg)
	}
	now = now.Add(30*time.Second - time.Millisecond)
	if active := store.ReadUnavailable(); active == nil || active.State != "upstream-unavailable" {
		t.Fatalf("transient state before expiry = %+v", active)
	}
	now = now.Add(time.Millisecond)
	if active := store.ReadUnavailable(); active != nil {
		t.Fatalf("transient state at expiry = %+v", active)
	}
	store.ClearUnavailable()
	if _, err := os.Stat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient file after clear: %v", err)
	}
}
