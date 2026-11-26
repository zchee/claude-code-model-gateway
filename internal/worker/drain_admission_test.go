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

package worker_test

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

func drainRequest(ctx context.Context, port int) *http.Request {
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	r := httptest.NewRequestWithContext(context.WithValue(ctx, http.LocalAddrContextKey, addr), http.MethodPost, "/drain", strings.NewReader("{}"))
	r.Host = addr.String()
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Model-Gateway-Control-Token", fixtureToken)
	return r
}

func TestDrainAdmissionCount(t *testing.T) {
	tests := map[string]struct {
		method, target string
		concurrent     bool
	}{
		"success: post-drain message is never forwarded":         {method: http.MethodPost, target: "/v1/messages"},
		"error: concurrent messages never exceed drain count":    {method: http.MethodPost, target: "/v1/messages", concurrent: true},
		"error: concurrent models hook never exceed drain count": {method: http.MethodGet, target: "/v1/models", concurrent: true},
	}
	const rounds = 2000
	const workers = 8
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if !tt.concurrent {
				var forwarded atomic.Int64
				h := handler(t, worker.Options{
					Anthropic: func(w http.ResponseWriter, _ *http.Request, _ route.Snapshot, _ route.Decision, _ []byte, admit admission) (outcome.Result, error) {
						forwarded.Add(1)
						selected := outcome.Route{Backend: "anthropic"}
						admit(selected, nil)
						w.WriteHeader(http.StatusNoContent)
						return outcome.Result{Route: selected, Status: http.StatusNoContent, Contacted: true}, nil
					},
				})
				dw := httptest.NewRecorder()
				h.ServeHTTP(dw, drainRequest(t.Context(), 41234))
				equal(t, http.StatusAccepted, dw.Code)
				var drained struct {
					ActiveRequests int64 `json:"activeRequests"`
				}
				if err := json.Unmarshal(dw.Body.Bytes(), &drained); err != nil {
					t.Fatal(err)
				}
				equal(t, int64(0), drained.ActiveRequests)
				mw := httptest.NewRecorder()
				h.ServeHTTP(mw, httptest.NewRequestWithContext(t.Context(), tt.method, tt.target, strings.NewReader("plain")))
				equal(t, http.StatusServiceUnavailable, mw.Code)
				equal(t, int64(0), forwarded.Load())
				return
			}

			for round := range rounds {
				release := make(chan struct{})
				var forwarded atomic.Int64
				h := handler(t, worker.Options{
					Anthropic: func(w http.ResponseWriter, _ *http.Request, _ route.Snapshot, _ route.Decision, _ []byte, admit admission) (outcome.Result, error) {
						forwarded.Add(1)
						<-release
						selected := outcome.Route{Backend: "anthropic"}
						admit(selected, nil)
						w.WriteHeader(http.StatusNoContent)
						return outcome.Result{Route: selected, Status: http.StatusNoContent, Contacted: true}, nil
					},
					Models: func(context.Context, http.Header) (jsontext.Value, error) {
						forwarded.Add(1)
						<-release
						return jsontext.Value(`{"data":[],"has_more":false}`), nil
					},
				})
				start := make(chan struct{})
				var wg sync.WaitGroup
				for i := range workers {
					wg.Go(func() {
						<-start
						for range i % 3 {
							runtime.Gosched()
						}
						req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.target, strings.NewReader("plain"))
						rec := httptest.NewRecorder()
						h.ServeHTTP(rec, req)
					})
				}
				dw := httptest.NewRecorder()
				drainDone := make(chan struct{})
				go func() {
					defer close(drainDone)
					<-start
					for range round % 4 {
						runtime.Gosched()
					}
					h.ServeHTTP(dw, drainRequest(t.Context(), 41234))
				}()
				close(start)
				<-drainDone
				var drained struct {
					ActiveRequests int64 `json:"activeRequests"`
				}
				err := json.Unmarshal(dw.Body.Bytes(), &drained)
				close(release)
				wg.Wait()
				if err != nil {
					t.Fatalf("round %d: decode drain response: %v", round, err)
				}
				if dw.Code != http.StatusAccepted {
					t.Fatalf("round %d: drain status=%d, want 202", round, dw.Code)
				}
				if got := forwarded.Load(); got > drained.ActiveRequests {
					t.Fatalf("round %d: forwarded %d requests after drain reported activeRequests=%d", round, got, drained.ActiveRequests)
				}
			}
		})
	}
}
