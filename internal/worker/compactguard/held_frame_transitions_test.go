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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// heldTransitionLimit is the buffered-byte limit of this test's guard: the small summary below
// stays under it, the large one crosses it at its text delta.
const heldTransitionLimit = 1024

// TestGuardHeldFramesAtBothTransitions decides how the guard groups its client writes at the two
// moments it releases held frames. When the attempt is accepted, everything held goes out in one
// write. When the held bytes pass the buffer limit, the held frames, including the one that crossed
// the limit, go out in one write at that moment, and the live output that follows is written on its
// own, never joined to that batch, also when it arrived in the same upstream read as the crossing
// frame. A guard that wrote held frames one by one at either transition, or that folded later live
// output into the released batch, fails here.
func TestGuardHeldFramesAtBothTransitions(t *testing.T) {
	longDelta := frameTextDelta(strings.Repeat("x", 1000))
	tests := map[string]struct {
		// reads are the upstream body's reads, in order; each is returned by one Read call.
		reads []string
		// wantFirst is the first client write, the released batch; wantRest is every later write joined.
		wantFirst, wantRest string
		// single requires the batch to be the only write.
		single bool
	}{
		"success: a summary under the limit is released in one write when the attempt is accepted": {
			reads:     []string{frameMessageStart, frameBlockStart, frameTextDelta("held"), frameBlockStop, frameMessageDelta, frameMessageStop},
			wantFirst: advertisedStart + frameBlockStart + frameTextDelta("held") + frameBlockStop + frameMessageDelta + frameMessageStop,
			single:    true,
		},
		"success: a summary over the limit releases its held frames in one write and streams the rest after it": {
			reads:     []string{frameMessageStart, frameBlockStart, longDelta + frameBlockStop, frameMessageDelta, frameMessageStop},
			wantFirst: advertisedStart + frameBlockStart + longDelta,
			wantRest:  frameBlockStop + frameMessageDelta + frameMessageStop,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			readers := make([]io.Reader, len(tt.reads))
			for i, r := range tt.reads {
				readers[i] = strings.NewReader(r)
			}
			// io.MultiReader returns at most one of its readers' data per read.
			body := &countedBody{ReadCloser: io.NopCloser(io.MultiReader(readers...))}
			req := guardRequest()
			// No heartbeat, so a slow run cannot add a heartbeat write to the ones compared here.
			req.Stream.HeartbeatInterval = -1
			dispatches := 0
			req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
				dispatches++
				return sseResponse(body), outcome.Result{Contacted: true}, nil
			}
			runner := compactguard.Runner{
				ServiceContext: t.Context(),
				Settings:       compactguard.Settings{StreamGuard: true, Retries: 1, MaxBufferedBytes: heldTransitionLimit},
				Stderr:         io.Discard,
			}
			w := &writeLog{rec: httptest.NewRecorder()}

			res, err := runner.Serve(t.Context(), w, req)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			if len(w.writes) == 0 {
				t.Fatal("the guard wrote nothing to the client")
			}
			if diff := gocmp.Diff(tt.wantFirst, w.writes[0]); diff != "" {
				t.Errorf("first client write, the released batch (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.wantRest, strings.Join(w.writes[1:], "")); diff != "" {
				t.Errorf("client writes after the batch, joined (-want +got):\n%s", diff)
			}
			if tt.single {
				if diff := gocmp.Diff(1, len(w.writes)); diff != "" {
					t.Errorf("client writes of an accepted summary (-want +got):\n%s", diff)
				}
			}
			if diff := gocmp.Diff([]int{http.StatusOK}, w.heads); diff != "" {
				t.Errorf("response heads (-want +got):\n%s", diff)
			}
			if res.Status != http.StatusOK || res.StatusOverride != "" {
				t.Errorf("result status=%d override=%q, want 200 and none", res.Status, res.StatusOverride)
			}
			if dispatches != 1 {
				t.Errorf("dispatches = %d, want 1", dispatches)
			}
			if got := body.closes.Load(); got != 1 {
				t.Errorf("body closed %d times, want 1", got)
			}
		})
	}
}
