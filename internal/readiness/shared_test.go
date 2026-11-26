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

package readiness_test

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
)

func TestSharedCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		nowMs := int64(1000)
		clock := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return time.UnixMilli(nowMs)
		}
		advance := func(ms int64) {
			mu.Lock()
			nowMs += ms
			mu.Unlock()
		}

		var runs atomic.Int32
		release := make(chan readiness.ProxyAuthResult)
		check := readiness.NewSharedCheck(func(context.Context) readiness.ProxyAuthResult {
			runs.Add(1)
			return <-release
		}, clock)
		defer check.Wait()

		if got := check.Latest(t.Context()); got != nil {
			t.Fatalf("first Latest = %+v, want nil while check is pending", got)
		}
		if got := check.Latest(t.Context()); got != nil {
			t.Fatalf("second Latest = %+v, want nil while check is pending", got)
		}
		synctest.Wait()
		if got := runs.Load(); got != 1 {
			t.Fatalf("check runs while first check is in flight = %d, want 1", got)
		}

		release <- readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: true}
		check.Wait()

		wantFirst := &readiness.ProxyAuthResult{
			ProxyBinary: true,
			ProxyModels: true,
			CodexAuth:   true,
			CheckedAt:   time.UnixMilli(1000),
		}
		if diff := gocmp.Diff(wantFirst, check.Latest(t.Context())); diff != "" {
			t.Fatalf("Latest after first completion mismatch (-want +got):\n%s", diff)
		}

		advance(14999)
		_ = check.Latest(t.Context())
		synctest.Wait()
		if got := runs.Load(); got != 1 {
			t.Fatalf("check runs on fresh result = %d, want 1", got)
		}

		advance(1)
		stale := check.Latest(t.Context())
		if stale == nil || stale.CheckedAt.UnixMilli() != 1000 {
			t.Fatalf("stale Latest = %+v, want CheckedAt 1000 while refresh runs", stale)
		}
		_ = check.Latest(t.Context())
		synctest.Wait()
		if got := runs.Load(); got != 2 {
			t.Fatalf("check runs while stale refresh is in flight = %d, want 2", got)
		}

		release <- readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: false}
		check.Wait()

		refreshed := check.Latest(t.Context())
		if refreshed == nil {
			t.Fatal("Latest after refresh returned nil")
		}
		if refreshed.CodexAuth {
			t.Errorf("refreshed CodexAuth = true, want false")
		}
	})
}

func TestHealthzCodexReadiness(t *testing.T) {
	health := readiness.HealthInfo{OK: true, Version: "0.0.0"}
	opts := readiness.CodexReadinessOptions{
		InstalledVersion: "0.0.0",
		CommandPath:      filepath.Join(t.TempDir(), "model-gateway"),
		LogsDir:          filepath.Join(t.TempDir(), "logs"),
		UpstreamStore:    upstreamstate.Store{Dir: t.TempDir(), Now: func() time.Time { return time.UnixMilli(5000) }},
	}

	t.Run("pending", func(t *testing.T) {
		pending := readiness.HealthzCodexReadiness(nil, health, time.UnixMilli(5000), opts)
		if pending.State != "checking" {
			t.Errorf("pending State = %q, want %q", pending.State, "checking")
		}
		if pending.Ready {
			t.Error("pending Ready = true, want false")
		}
		if pending.Checks != nil {
			t.Errorf("pending Checks = %+v, want nil", pending.Checks)
		}
		if pending.CheckedAt != nil {
			t.Errorf("pending CheckedAt = %v, want nil", *pending.CheckedAt)
		}
		if pending.AgeMs != nil {
			t.Errorf("pending AgeMs = %v, want nil", *pending.AgeMs)
		}
		if !pending.Stale {
			t.Error("pending Stale = false, want true")
		}
		raw, err := json.Marshal(pending)
		if err != nil {
			t.Fatalf("marshal pending readiness: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal pending readiness: %v", err)
		}
		for _, field := range []string{"checks", "checkedAt", "ageMs"} {
			val, ok := doc[field]
			if !ok || val != nil {
				t.Errorf("pending JSON %q = (%v, present=%v), want explicit null", field, val, ok)
			}
		}
	})

	check := &readiness.ProxyAuthResult{
		ProxyBinary: true,
		ProxyModels: true,
		CodexAuth:   true,
		CheckedAt:   time.UnixMilli(5000),
	}

	t.Run("confirmed", func(t *testing.T) {
		confirmed := readiness.HealthzCodexReadiness(check, health, time.UnixMilli(6000), opts)
		if confirmed.Checks == nil {
			t.Fatal("confirmed Checks = nil, want populated checks")
		}
		if !confirmed.Checks.CodexAuth {
			t.Error("confirmed Checks.CodexAuth = false, want true")
		}
		if confirmed.CheckedAt == nil {
			t.Fatal("confirmed CheckedAt = nil, want ISO timestamp")
		}
		wantISO := time.UnixMilli(5000).UTC().Format("2006-01-02T15:04:05.000Z")
		if *confirmed.CheckedAt != wantISO {
			t.Errorf("confirmed CheckedAt = %q, want %q", *confirmed.CheckedAt, wantISO)
		}
		if confirmed.AgeMs == nil || *confirmed.AgeMs != 1000 {
			t.Errorf("confirmed AgeMs = %v, want 1000", confirmed.AgeMs)
		}
		if confirmed.Stale {
			t.Error("confirmed Stale = true, want false")
		}
	})

	t.Run("stale", func(t *testing.T) {
		stale := readiness.HealthzCodexReadiness(check, health, time.UnixMilli(20000), opts)
		if stale.AgeMs == nil || *stale.AgeMs != 15000 {
			t.Errorf("stale AgeMs = %v, want 15000", stale.AgeMs)
		}
		if !stale.Stale {
			t.Error("stale Stale = false, want true")
		}
	})
}

func TestProbeProxyAndAuth(t *testing.T) {
	dir := t.TempDir()
	existingBin := filepath.Join(dir, "claude-code-proxy")
	if err := os.WriteFile(existingBin, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	missingBin := filepath.Join(dir, "missing-proxy")

	var modelHits atomic.Int32
	var modelStatus atomic.Int32
	modelStatus.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			modelHits.Add(1)
			w.WriteHeader(int(modelStatus.Load()))
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	tests := map[string]struct {
		binary      string
		status      int32
		authOut     string
		authErrOut  string
		want        readiness.ProxyAuthResult
		wantNoCalls bool
	}{
		"success: binary present, models 200, account on stdout": {
			binary:  existingBin,
			status:  http.StatusOK,
			authOut: "Account: user@example.com\n",
			want:    readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: true},
		},
		"success: account match on stderr is accepted": {
			binary:     existingBin,
			status:     http.StatusOK,
			authErrOut: "logged in account active",
			want:       readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: true},
		},
		"error: missing binary skips both probes": {
			binary:      missingBin,
			status:      http.StatusOK,
			authOut:     "account: present",
			want:        readiness.ProxyAuthResult{ProxyBinary: false, ProxyModels: false, CodexAuth: false},
			wantNoCalls: true,
		},
		"error: non-200 models and unauthenticated status": {
			binary:  existingBin,
			status:  http.StatusBadGateway,
			authOut: "not signed in",
			want:    readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: false, CodexAuth: false},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			modelHits.Store(0)
			modelStatus.Store(tt.status)
			var authCalls atomic.Int32
			got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
				ProxyBinary:    tt.binary,
				ProxyModelsURL: srv.URL + "/v1/models",
				RunAuthStatus: func(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
					authCalls.Add(1)
					if diff := gocmp.Diff([]string{"codex", "auth", "status"}, args); diff != "" {
						t.Errorf("auth status args mismatch (-want +got):\n%s", diff)
					}
					return []byte(tt.authOut), []byte(tt.authErrOut), nil
				},
			})
			got.CheckedAt = time.Time{}
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ProbeProxyAndAuth mismatch (-want +got):\n%s", diff)
			}
			if tt.wantNoCalls && (modelHits.Load() != 0 || authCalls.Load() != 0) {
				t.Errorf("missing binary still ran probes: models=%d auth=%d", modelHits.Load(), authCalls.Load())
			}
		})
	}
}
