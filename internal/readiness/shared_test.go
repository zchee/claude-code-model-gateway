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

var (
	_ func(context.Context, func(context.Context) readiness.ProxyAuthResult, func() time.Time) *readiness.SharedCheck             = readiness.NewSharedCheck
	_ func(*readiness.SharedCheck) *readiness.ProxyAuthResult                                                                     = (*readiness.SharedCheck).Latest
	_ func(*readiness.ProxyAuthResult, readiness.HealthInfo, time.Time, readiness.CodexReadinessOptions) readiness.CodexReadiness = readiness.HealthzCodexReadiness
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
		var closeOnce sync.Once
		closeRelease := func() { closeOnce.Do(func() { close(release) }) }
		workerCtx, workerCancel := context.WithCancel(t.Context())
		check := readiness.NewSharedCheck(workerCtx, func(runCtx context.Context) readiness.ProxyAuthResult {
			runs.Add(1)
			select {
			case res, ok := <-release:
				if !ok {
					return readiness.ProxyAuthResult{}
				}
				return res
			case <-runCtx.Done():
				return readiness.ProxyAuthResult{}
			}
		}, clock)
		defer func() {
			closeRelease()
			workerCancel()
			check.Wait()
		}()

		if got := check.Latest(); got != nil {
			t.Fatalf("first Latest = %+v, want nil while check is pending", got)
		}
		if got := check.Latest(); got != nil {
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
		if diff := gocmp.Diff(wantFirst, check.Latest()); diff != "" {
			t.Fatalf("Latest after first completion mismatch (-want +got):\n%s", diff)
		}

		advance(14999)
		_ = check.Latest()
		synctest.Wait()
		if got := runs.Load(); got != 1 {
			t.Fatalf("check runs on fresh result = %d, want 1", got)
		}

		advance(1)
		stale := check.Latest()
		if stale == nil || stale.CheckedAt.UnixMilli() != 1000 {
			t.Fatalf("stale Latest = %+v, want CheckedAt 1000 while refresh runs", stale)
		}
		_ = check.Latest()
		synctest.Wait()
		if got := runs.Load(); got != 2 {
			t.Fatalf("check runs while stale refresh is in flight = %d, want 2", got)
		}

		release <- readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: false}
		check.Wait()

		refreshed := check.Latest()
		if refreshed == nil {
			t.Fatal("Latest after refresh returned nil")
		}
		if refreshed.CodexAuth {
			t.Errorf("refreshed CodexAuth = true, want false")
		}
	})
}

func TestSharedCheckWorkerLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		nowMs := int64(1000)
		clock := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return time.UnixMilli(nowMs)
		}

		workerCtx, workerCancel := context.WithCancel(t.Context())
		firstRelease := make(chan struct{})
		var closeFirstOnce sync.Once
		closeFirst := func() { closeFirstOnce.Do(func() { close(firstRelease) }) }

		var calls atomic.Int32
		var firstCanceled atomic.Bool
		var secondCanceled atomic.Bool

		check := readiness.NewSharedCheck(workerCtx, func(runCtx context.Context) readiness.ProxyAuthResult {
			call := calls.Add(1)
			if call == 1 {
				select {
				case <-firstRelease:
					firstCanceled.Store(runCtx.Err() != nil)
					return readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: true}
				case <-runCtx.Done():
					firstCanceled.Store(true)
					return readiness.ProxyAuthResult{}
				}
			}
			<-runCtx.Done()
			secondCanceled.Store(true)
			return readiness.ProxyAuthResult{}
		}, clock)
		defer func() {
			closeFirst()
			workerCancel()
			check.Wait()
		}()

		// Trigger from a request scope that ends immediately; the probe must keep running and publish.
		reqCtx, reqCancel := context.WithCancel(t.Context())
		_ = check.Latest()
		reqCancel()
		_ = reqCtx.Err()
		synctest.Wait()
		if got := calls.Load(); got != 1 {
			t.Fatalf("first probe calls = %d, want 1", got)
		}
		closeFirst()
		check.Wait()
		if firstCanceled.Load() {
			t.Error("background probe was canceled before completion after initiating request ended")
		}
		if got := check.Latest(); got == nil || !got.CodexAuth {
			t.Fatalf("Latest after background completion = %+v, want CodexAuth=true", got)
		}

		// Expire the result, start a second probe, and verify worker shutdown cancels it cleanly.
		mu.Lock()
		nowMs += readiness.FreshnessTTL.Milliseconds()
		mu.Unlock()
		_ = check.Latest()
		synctest.Wait()
		if got := calls.Load(); got != 2 {
			t.Fatalf("second probe calls = %d, want 2", got)
		}
		workerCancel()
		check.Wait()
		if !secondCanceled.Load() {
			t.Error("in-flight probe did not observe worker context cancellation on shutdown")
		}
	})
}

func TestHealthzCodexReadiness(t *testing.T) {
	health := readiness.HealthInfo{OK: true, Version: "0.0.0"}
	baseDir := t.TempDir()
	commandPath := filepath.Join(baseDir, "my gateway", "bin", "model-gateway")
	logsDir := filepath.Join(baseDir, "logs")
	opts := readiness.CodexReadinessOptions{
		InstalledVersion: "0.0.0",
		CommandPath:      commandPath,
		LogsDir:          logsDir,
		UpstreamStore: upstreamstate.Store{
			Dir:         filepath.Join(baseDir, "state"),
			Now:         func() time.Time { return time.UnixMilli(5000) },
			CommandPath: commandPath,
		},
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

	t.Run("readiness_messages", func(t *testing.T) {
		quoted := `"` + commandPath + `"`
		const expiresAt = "1970-01-01T00:01:05.000Z"
		tests := map[string]struct {
			latest    *readiness.ProxyAuthResult
			health    readiness.HealthInfo
			seedBlock func(t *testing.T, store upstreamstate.Store)
			wantState string
			wantMsg   string
		}{
			"error: binary missing names quoted stable setup": {
				latest:    &readiness.ProxyAuthResult{ProxyBinary: false, ProxyModels: false, CodexAuth: false, CheckedAt: time.UnixMilli(5000)},
				health:    health,
				wantState: "binary-missing",
				wantMsg:   "Codex dispatch refused: claude-code-proxy is missing. Run `" + quoted + " setup`, then retry. No Anthropic fallback was used.",
			},
			"error: auth missing names quoted stable login then setup": {
				latest:    &readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: false, CheckedAt: time.UnixMilli(5000)},
				health:    health,
				wantState: "auth-missing",
				wantMsg:   "Codex dispatch refused: ChatGPT sign-in is required. Run `" + quoted + " login`, finish browser OAuth, then run `" + quoted + " setup` and retry. Credentials live in `~/.config/claude-code-proxy/`.",
			},
			"error: proxy down keeps guardian log text": {
				latest:    &readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: false, CodexAuth: true, CheckedAt: time.UnixMilli(5000)},
				health:    health,
				wantState: "proxy-down",
				wantMsg:   "Codex dispatch refused: claude-code-proxy is not answering on /v1/models. The running shim supervisor retries recovery with bounded backoff; check " + filepath.Join(logsDir, "guardian.log") + " if it does not recover. No Anthropic fallback was used.",
			},
			"error: shim down names quoted stable ensure": {
				latest:    check,
				health:    readiness.HealthInfo{OK: false},
				wantState: "shim-down",
				wantMsg:   "Codex dispatch refused: the model-gateway shim is down. Run `" + quoted + " ensure`, then retry. No Anthropic fallback was used.",
			},
			"error: serving version mismatch names quoted stable ensure": {
				latest:    check,
				health:    readiness.HealthInfo{OK: true, Version: "0.0.0"},
				wantState: "serving-version-mismatch",
				wantMsg:   "Codex dispatch refused: model-gateway is serving a stale shim version. Run `" + quoted + " ensure`, then retry. No Anthropic fallback was used.",
			},
			"error: upstream credential block names quoted stable setup": {
				latest: check,
				health: health,
				seedBlock: func(t *testing.T, store upstreamstate.Store) {
					t.Helper()
					if _, err := store.SetBlocked(http.StatusUnauthorized, "openai rejection", nil); err != nil {
						t.Fatalf("SetBlocked 401: %v", err)
					}
				},
				wantState: "upstream-blocked",
				wantMsg:   "Codex is blocked by an OpenAI rejection. Run `" + quoted + " setup`; if it persists, wait for a claude-code-proxy update or explicitly re-route this ticket. Codex tickets remain blocked.",
			},
			"error: expiring 429 keeps rate-limit text unchanged": {
				latest: check,
				health: health,
				seedBlock: func(t *testing.T, store upstreamstate.Store) {
					t.Helper()
					if _, err := store.SetBlocked(http.StatusTooManyRequests, "rate limit", http.Header{"Retry-After": {"60"}}); err != nil {
						t.Fatalf("SetBlocked 429: %v", err)
					}
				},
				wantState: "upstream-blocked",
				wantMsg:   "Codex is rate-limited by OpenAI (429) until " + expiresAt + ". The block lifts by itself then, or sooner when a Codex request succeeds; wait, or explicitly re-route this ticket.",
			},
		}

		for name, tt := range tests {
			t.Run(name, func(t *testing.T) {
				caseOpts := opts
				caseOpts.UpstreamStore = upstreamstate.Store{
					Dir:         t.TempDir(),
					Now:         func() time.Time { return time.UnixMilli(5000) },
					CommandPath: commandPath,
				}
				if tt.wantState == "serving-version-mismatch" {
					caseOpts.InstalledVersion = "0.57.0"
				}
				if tt.seedBlock != nil {
					tt.seedBlock(t, caseOpts.UpstreamStore)
				}
				got := readiness.HealthzCodexReadiness(tt.latest, tt.health, time.UnixMilli(6000), caseOpts)
				if got.Ready {
					t.Error("Ready = true, want false")
				}
				if got.State != tt.wantState {
					t.Errorf("State = %q, want %q", got.State, tt.wantState)
				}
				if got.Message != tt.wantMsg {
					t.Errorf("Message mismatch:\n got: %q\nwant: %q", got.Message, tt.wantMsg)
				}
			})
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
