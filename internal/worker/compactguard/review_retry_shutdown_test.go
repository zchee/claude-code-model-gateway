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

package compactguard_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// TestGuardServiceEndsDuringCommittedRetryWait ends the service, not the client, while the guard
// waits to retry a discarded attempt whose response head the client already received. The guard
// starts no second attempt, keeps the earlier contact, reports no status, and returns an abort that
// wraps the service's cause, so a client still holding the committed response is not given a clean
// end.
func TestGuardServiceEndsDuringCommittedRetryWait(t *testing.T) {
	const (
		retryDelay  = 250 * time.Millisecond
		serviceEnds = 100 * time.Millisecond
	)
	errWorkerStopped := errors.New("worker stopped for replacement")
	tests := map[string]struct {
		cause     error
		wantCause error
	}{
		"error: service cancelled with a cause": {
			cause:     errWorkerStopped,
			wantCause: errWorkerStopped,
		},
		"error: service cancelled without a cause": {
			wantCause: context.Canceled,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, stop := context.WithCancelCause(t.Context())
				defer stop(nil)
				r := guardRunner(service, 1)
				r.Settings.RetryDelayMillis = float64(retryDelay / time.Millisecond)
				req := guardRequest()
				var heads []bool
				var bodies []*countedBody
				req.Next = func(_ context.Context, head bool) (*upstream.Response, outcome.Result, error) {
					heads = append(heads, head)
					body := &countedBody{ReadCloser: io.NopCloser(strings.NewReader(truncatedStream()))}
					bodies = append(bodies, body)
					return sseResponse(body), outcome.Result{Contacted: true}, nil
				}
				timer := time.AfterFunc(serviceEnds, func() { stop(tt.cause) })
				defer timer.Stop()

				w := &headRecorder{rec: httptest.NewRecorder()}
				start := time.Now()
				got, err := r.Serve(t.Context(), w, req)
				elapsed := time.Since(start)

				if !errors.Is(err, http.ErrAbortHandler) {
					t.Errorf("Serve error = %v, want an abort of the committed response", err)
				}
				if !errors.Is(err, tt.wantCause) {
					t.Errorf("Serve error = %v, want it to wrap the service cause %v", err, tt.wantCause)
				}
				if diff := gocmp.Diff(outcome.Result{Contacted: true}, got); diff != "" {
					t.Errorf("result after shutdown in the retry wait mismatch (-want +got):\n%s", diff)
				}
				if diff := gocmp.Diff([]bool{false}, heads); diff != "" {
					t.Errorf("attempts started (head committed per attempt) mismatch (-want +got):\n%s", diff)
				}
				for i, body := range bodies {
					if n := body.closes.Load(); n != 1 {
						t.Errorf("attempt %d body closed %d times, want 1", i+1, n)
					}
				}
				if diff := gocmp.Diff([]int{http.StatusOK}, w.heads); diff != "" {
					t.Errorf("response heads written mismatch (-want +got):\n%s", diff)
				}
				if body := w.bodyString(); body != "" {
					t.Errorf("guard wrote the discarded attempt or a reply after shutdown: %q", body)
				}
				if elapsed != serviceEnds {
					t.Errorf("Serve returned after %v, want %v when the service ended, not the retry delay %v", elapsed, serviceEnds, retryDelay)
				}
			})
		})
	}
}
