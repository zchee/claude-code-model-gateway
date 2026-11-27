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

package localprov_test

import (
	json "encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

// switchableLlama is a llama-server on its own listener that answers /props with the window it was
// started with while status is 200, and with status and no props otherwise. The listener stays open
// for the whole test, so an outage keeps the configured port and no other process can take it.
type switchableLlama struct {
	port   uint16
	status atomic.Int32
}

func startSwitchableLlama(t *testing.T, window int64) *switchableLlama {
	t.Helper()
	llama := &switchableLlama{}
	llama.status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/props" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status := int(llama.status.Load())
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		data, err := json.Marshal(map[string]any{"default_generation_settings": map[string]any{"n_ctx": window}})
		if err != nil {
			t.Errorf("marshal llama props: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	llama.port = uint16(srv.Listener.Addr().(*net.TCPAddr).Port)
	return llama
}

// TestLlamaTrackerHandsOutCopies changes every field of the states the llama-server tracker hands
// out, from Refresh and from State. The tracked state keeps the measured values, and a later probe
// that finds the server failing on the same port still preserves the window that was measured, not
// a value written into a returned copy.
func TestLlamaTrackerHandsOutCopies(t *testing.T) {
	const measuredWindow = 65536
	tests := map[string]struct {
		// mutate changes a state handed out by the tracker.
		mutate func(state *localprov.LlamaState)
	}{
		"success: changing every field of the returned states leaves the tracked state": {
			mutate: func(state *localprov.LlamaState) {
				state.Port++
				state.Configured = false
				state.Reachable = false
				state.ContextWindow = 1
				state.Error = "written through a returned copy"
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			llama := startSwitchableLlama(t, measuredWindow)
			port := llama.port
			routes := []proxymodels.Route{{ID: "local/gguf", Provider: "local"}}
			env := mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(port))})
			want := localprov.LlamaState{Port: port, Configured: true, Reachable: true, ContextWindow: measuredWindow}
			var tracker localprov.LlamaTracker

			refreshed := tracker.Refresh(t.Context(), routes, env, localprov.ProbeOptions{})
			if refreshed == nil {
				t.Fatal("Refresh with a local route returned nil")
			}
			if diff := gocmp.Diff(want, *refreshed); diff != "" {
				t.Fatalf("test precondition: Refresh mismatch (-want +got):\n%s", diff)
			}
			tt.mutate(refreshed)
			first := tracker.State()
			if first == nil {
				t.Fatal("State after Refresh returned nil")
			}
			if diff := gocmp.Diff(want, *first); diff != "" {
				t.Errorf("State after changing the state Refresh returned mismatch (-want +got):\n%s", diff)
			}
			tt.mutate(first)
			second := tracker.State()
			if second == nil {
				t.Fatal("State after changing a State copy returned nil")
			}
			if diff := gocmp.Diff(want, *second); diff != "" {
				t.Errorf("State after changing the state State returned mismatch (-want +got):\n%s", diff)
			}
			if first == second {
				t.Error("two State calls returned the same pointer, want a fresh copy each time")
			}

			// The outage is a failing answer on the same listener, so the configured port is unchanged.
			llama.status.Store(http.StatusServiceUnavailable)
			down := tracker.Refresh(t.Context(), routes, env, localprov.ProbeOptions{})
			if down == nil {
				t.Fatal("Refresh after the server started failing returned nil")
			}
			wantDown := localprov.LlamaState{Port: port, Configured: true, Reachable: false, ContextWindow: measuredWindow, Error: "GET /props answered HTTP 503"}
			if diff := gocmp.Diff(wantDown, *down); diff != "" {
				t.Errorf("Refresh after the server started failing mismatch, want unreachable with the measured window %d preserved (-want +got):\n%s", measuredWindow, diff)
			}
		})
	}
}
