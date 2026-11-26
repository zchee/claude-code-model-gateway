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

//go:build process

package supervisor_test

import (
	json "encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

func TestRelayMethodAndTarget(t *testing.T) {
	tests := map[string]struct {
		method, target string
		status         int
		healthJSON     bool
	}{
		"success: healthy worker answers HEAD":             {method: http.MethodHead, target: "/healthz", status: http.StatusOK},
		"error: worker failure status survives HEAD":       {method: http.MethodHead, target: "/healthz?detail=1", status: http.StatusServiceUnavailable},
		"success: GET healthz augments supervisor fields":  {method: http.MethodGet, target: "/healthz", status: http.StatusOK, healthJSON: true},
		"success: preserve double slash without query":     {method: http.MethodGet, target: "//v1/messages", status: http.StatusNoContent},
		"success: preserve double slash and escaped query": {method: http.MethodGet, target: "//catalog/%2f?q=a%2Fb&q=+", status: http.StatusNoContent},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			base := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if diff := gocmp.Diff([]string{tt.method, tt.target}, []string{r.Method, r.RequestURI}); diff != "" {
					t.Errorf("worker request (-want +got):\n%s", diff)
				}
				w.Header().Set("X-Worker-Response", "yes")
				if tt.healthJSON {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, `{"workerField":"ok"}`)
					return
				}
				w.WriteHeader(tt.status)
			}))
			u, err := url.Parse(base)
			if err != nil {
				t.Fatal(err)
			}
			p, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatal(err)
			}
			h, err := supervisor.NewHandler(40001, func() int { return p }, strings.Repeat("ab", 32), "test-version", 1)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequestWithContext(t.Context(), tt.method, tt.target, nil)
			r.Host = "127.0.0.1:40001"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if tt.healthJSON {
				if w.Code != tt.status || w.Header().Get("X-Worker-Response") != "yes" {
					t.Fatalf("status=%d, worker response=%q", w.Code, w.Header().Get("X-Worker-Response"))
				}
				var got map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode health JSON: %v", err)
				}
				want := map[string]any{"workerField": "ok", "supervisorVersion": "test-version", "supervisorPid": float64(1), "proxyRecovery": true}
				if diff := gocmp.Diff(want, got); diff != "" {
					t.Errorf("health JSON (-want +got):\n%s", diff)
				}
				return
			}
			if w.Code != tt.status || w.Body.Len() != 0 || w.Header().Get("X-Worker-Response") != "yes" {
				t.Errorf("status=%d, bytes=%d, worker response=%q", w.Code, w.Body.Len(), w.Header().Get("X-Worker-Response"))
			}
		})
	}
}
