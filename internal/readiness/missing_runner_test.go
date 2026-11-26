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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

// TestProviderAuthStateWithoutStatusRunner leaves the status runner unset. The source would start the
// real status command; without one configured, a provider whose state needs that command has no
// evidence and reports auth-unknown, while providers decided without the command keep their answers.
func TestProviderAuthStateWithoutStatusRunner(t *testing.T) {
	tests := map[string]struct {
		provider string
		env      map[string]string
		want     string
	}{
		"error: Kimi sign-in cannot be checked": {
			provider: "kimi",
			want:     "auth-unknown",
		},
		"error: Cursor sign-in cannot be checked": {
			provider: "cursor",
			want:     "auth-unknown",
		},
		"error: Muse sign-in cannot be checked": {
			provider: "muse",
			want:     "auth-unknown",
		},
		"error: Vertex with a project cannot be checked": {
			provider: "vertex",
			env:      map[string]string{"CCP_VERTEX_PROJECT": "runner-absent-project"},
			want:     "auth-unknown",
		},
		"error: Vertex without a project is decided before the command": {
			provider: "vertex",
			env:      map[string]string{},
			want:     "config-invalid",
		},
		"success: OpenCode key presence needs no command": {
			provider: "opencode",
			env:      map[string]string{"OPENCODE_API_KEY": "runner-absent-key"},
			want:     "ready",
		},
		"error: OpenCode without a key needs no command": {
			provider: "opencode",
			env:      map[string]string{},
			want:     "auth-missing",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := readiness.ProviderAuthState(t.Context(), tt.provider, readiness.ProviderAuthOptions{
				Lookup:      lookupMap(tt.env),
				ConfigDir:   t.TempDir(),
				ProxyBinary: filepath.Join(t.TempDir(), "claude-code-proxy"),
			})
			if got != tt.want {
				t.Errorf("ProviderAuthState(%q) without a status runner = %q, want %q", tt.provider, got, tt.want)
			}
		})
	}
}

// TestProbeProxyAndAuthWithoutStatusRunner leaves the Codex auth status runner unset while the proxy
// binary exists and /v1/models answers: the shared check reports the binary and the listing and never
// reports Codex as signed in.
func TestProbeProxyAndAuthWithoutStatusRunner(t *testing.T) {
	tests := map[string]struct {
		status int
		want   readiness.ProxyAuthResult
	}{
		"error: answering proxy without a runner is not signed in": {
			status: http.StatusOK,
			want:   readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: true, CodexAuth: false},
		},
		"error: failing proxy without a runner is not signed in": {
			status: http.StatusBadGateway,
			want:   readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: false, CodexAuth: false},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var modelHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				modelHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"data":[]}`))
			}))
			t.Cleanup(srv.Close)

			got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
				ProxyBinary:    fakeProxyBinary(t),
				ProxyModelsURL: srv.URL + "/v1/models",
			})
			if diff := gocmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ProbeProxyAndAuth without a status runner mismatch (-want +got):\n%s", diff)
			}
			if n := modelHits.Load(); n != 1 {
				t.Errorf("test precondition: /v1/models answered %d request(s), want 1", n)
			}
		})
	}
}
