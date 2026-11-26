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
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
)

// slowRedirectTransport answers each request after hopDelay of bubble time: the first redirects
// requests get a 302 to the next hop, the request after them gets a 200 carrying body. With trickle
// set, that body never ends: after body it yields one space every 70 ms of bubble time until the
// request's context ends. Nothing is dialled. Hop and byte times never coincide with a deadline, so
// no outcome depends on the order of timers that fire at the same instant.
type slowRedirectTransport struct {
	hopDelay  time.Duration
	redirects int
	body      []byte
	trickle   bool

	mu   sync.Mutex
	seen []string
}

func (s *slowRedirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.seen = append(s.seen, req.URL.String())
	n := len(s.seen)
	s.mu.Unlock()
	select {
	case <-time.After(s.hopDelay):
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	if n <= s.redirects {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"/hop-" + strconv.Itoa(n)}},
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		ContentLength: int64(len(s.body)),
		Body:          io.NopCloser(bytes.NewReader(s.body)),
		Request:       req,
	}
	if s.trickle {
		resp.ContentLength = -1
		resp.Body = &tricklingBody{ctx: req.Context(), rest: s.body}
	}
	return resp, nil
}

func (s *slowRedirectTransport) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// tricklingBody yields rest, then one space every 70 ms of bubble time, until ctx ends.
type tricklingBody struct {
	ctx  context.Context
	rest []byte
}

func (b *tricklingBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.rest) > 0 {
		n := copy(p, b.rest)
		b.rest = b.rest[n:]
		return n, nil
	}
	select {
	case <-time.After(70 * time.Millisecond):
		p[0] = ' '
		return 1, nil
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (b *tricklingBody) Close() error { return nil }

// deadlineProbeOutcome is what one probe reported and how long it took in bubble time.
type deadlineProbeOutcome struct {
	Reachable bool
	Error     string
	Elapsed   time.Duration
}

// TestProbeDeadlineCoversRedirectChain gives each local probe a redirect chain whose every hop
// answers well inside the probe deadline, so only a deadline over the whole chain and the body can
// end the probe. A probe that runs out of time names the first URL it requested, never a later hop.
func TestProbeDeadlineCoversRedirectChain(t *testing.T) {
	const window = 65536
	llamaBody := []byte(`{"default_generation_settings":{"n_ctx":65536}}`)
	studioBody := studioModelsBody(t, window)
	port := closedLoopbackPort(t)
	llamaURL := "http://127.0.0.1:" + strconv.Itoa(int(port)) + "/props"
	studioBase := "http://127.0.0.1:" + strconv.Itoa(int(port))
	studioURL := studioBase + "/api/v1/models"

	probeLlama := func(t *testing.T, opts localprov.ProbeOptions) deadlineProbeOutcome {
		state := localprov.ProbeLlama(t.Context(), mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(port))}), opts)
		if state.Reachable && state.ContextWindow != window {
			t.Errorf("reachable llama-server window = %d, want %d", state.ContextWindow, window)
		}
		return deadlineProbeOutcome{Reachable: state.Reachable, Error: state.Error}
	}
	probeStudio := func(t *testing.T, opts localprov.ProbeOptions) deadlineProbeOutcome {
		state := localprov.ProbeStudio(t.Context(), mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": studioBase}), t.TempDir(), opts)
		if state.BaseURL != studioBase {
			t.Errorf("test precondition: LM Studio base URL = %q, want %q", state.BaseURL, studioBase)
		}
		if state.Reachable && state.Windows()["qwen3-coder-30b"] != window {
			t.Errorf("reachable LM Studio windows = %v, want qwen3-coder-30b at %d", state.Windows(), window)
		}
		return deadlineProbeOutcome{Reachable: state.Reachable, Error: state.Error}
	}

	tests := map[string]struct {
		probe        func(t *testing.T, opts localprov.ProbeOptions) deadlineProbeOutcome
		firstURL     string
		timeout      time.Duration
		redirects    int
		hopDelay     time.Duration
		body         []byte
		trickle      bool
		wantRequests int
		want         deadlineProbeOutcome
	}{
		"success: llama-server chain that ends inside the deadline is answered": {
			probe:        probeLlama,
			firstURL:     llamaURL,
			redirects:    1,
			hopDelay:     600 * time.Millisecond,
			body:         llamaBody,
			wantRequests: 2,
			want:         deadlineProbeOutcome{Reachable: true, Elapsed: 1200 * time.Millisecond},
		},
		"error: llama-server chain longer than the deadline times out naming the first URL": {
			probe:        probeLlama,
			firstURL:     llamaURL,
			redirects:    2,
			hopDelay:     600 * time.Millisecond,
			body:         llamaBody,
			wantRequests: 3,
			want:         deadlineProbeOutcome{Error: "timeout: " + llamaURL, Elapsed: localprov.DefaultProbeTimeout},
		},
		"error: llama-server body that outlasts the deadline after redirects times out naming the first URL": {
			probe:        probeLlama,
			firstURL:     llamaURL,
			redirects:    2,
			hopDelay:     100 * time.Millisecond,
			body:         llamaBody,
			trickle:      true,
			wantRequests: 3,
			want:         deadlineProbeOutcome{Error: "timeout: " + llamaURL, Elapsed: localprov.DefaultProbeTimeout},
		},
		"error: llama-server chain under a configured deadline times out at that deadline": {
			probe:        probeLlama,
			firstURL:     llamaURL,
			timeout:      4 * time.Second,
			redirects:    5,
			hopDelay:     1500 * time.Millisecond,
			body:         llamaBody,
			wantRequests: 3,
			want:         deadlineProbeOutcome{Error: "timeout: " + llamaURL, Elapsed: 4 * time.Second},
		},
		"success: LM Studio chain that ends inside the deadline is answered": {
			probe:        probeStudio,
			firstURL:     studioURL,
			redirects:    1,
			hopDelay:     600 * time.Millisecond,
			body:         studioBody,
			wantRequests: 2,
			want:         deadlineProbeOutcome{Reachable: true, Elapsed: 1200 * time.Millisecond},
		},
		"error: LM Studio chain longer than the deadline times out naming the first URL": {
			probe:        probeStudio,
			firstURL:     studioURL,
			redirects:    2,
			hopDelay:     600 * time.Millisecond,
			body:         studioBody,
			wantRequests: 3,
			want:         deadlineProbeOutcome{Error: "timeout: " + studioURL, Elapsed: localprov.DefaultProbeTimeout},
		},
		"error: LM Studio body that outlasts the deadline after redirects times out naming the first URL": {
			probe:        probeStudio,
			firstURL:     studioURL,
			redirects:    2,
			hopDelay:     100 * time.Millisecond,
			body:         studioBody,
			trickle:      true,
			wantRequests: 3,
			want:         deadlineProbeOutcome{Error: "timeout: " + studioURL, Elapsed: localprov.DefaultProbeTimeout},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := &slowRedirectTransport{hopDelay: tt.hopDelay, redirects: tt.redirects, body: tt.body, trickle: tt.trickle}
				opts := localprov.ProbeOptions{Client: &http.Client{Transport: fake}, Timeout: tt.timeout}
				start := time.Now()
				got := tt.probe(t, opts)
				got.Elapsed = time.Since(start)
				if diff := gocmp.Diff(tt.want, got); diff != "" {
					t.Errorf("probe outcome mismatch (-want +got):\n%s", diff)
				}
				requests := fake.requests()
				if len(requests) != tt.wantRequests {
					t.Errorf("the probe made %d request(s) %q, want %d", len(requests), requests, tt.wantRequests)
				}
				if len(requests) > 0 && requests[0] != tt.firstURL {
					t.Errorf("test precondition: first request = %q, want %q", requests[0], tt.firstURL)
				}
				if strings.Contains(got.Error, "/hop-") {
					t.Errorf("probe error %q names a redirect hop, want only the first URL", got.Error)
				}
			})
		})
	}
}
