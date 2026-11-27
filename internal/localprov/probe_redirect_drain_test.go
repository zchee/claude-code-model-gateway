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
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
)

// redirectSlurpBound restates maxBodySlurpSize of the Go 1.27.4 net/http client (net/http/client.go):
// the most bytes the client reads from a redirect response's body, when its length is unknown or at
// most this bound, before it closes the body and requests the next hop. This is observed
// implementation behaviour of that release, not a documented guarantee.
const redirectSlurpBound = 2 << 10

// drainedRedirect answers the first request in memory with a 302 to /next whose body is
// instrumented, and the request for /next with the final answer. It records, in order, each request's
// path, the closing of the redirect body with the bytes read from it until then, and any read of the
// redirect body after its closing. A negative contentLength sends no Content-Length field.
type drainedRedirect struct {
	body          []byte
	contentLength int64
	final         string

	mu     sync.Mutex
	events []string
}

func (d *drainedRedirect) record(event string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.events = append(d.events, event)
}

func (d *drainedRedirect) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.events)
}

func (d *drainedRedirect) RoundTrip(req *http.Request) (*http.Response, error) {
	d.record("request " + req.URL.Path)
	if req.URL.Path == "/next" {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": {"application/json"}},
			ContentLength: int64(len(d.final)),
			Body:          io.NopCloser(strings.NewReader(d.final)),
			Request:       req,
		}, nil
	}
	header := http.Header{"Location": {"/next"}}
	if d.contentLength >= 0 {
		header.Set("Content-Length", strconv.FormatInt(d.contentLength, 10))
	}
	return &http.Response{
		StatusCode:    http.StatusFound,
		Header:        header,
		ContentLength: d.contentLength,
		Body:          &redirectBody{owner: d, rest: d.body},
		Request:       req,
	}, nil
}

// redirectBody hands out as much of rest as each read asks for and records its closing. A read after
// the closing is recorded and fails.
type redirectBody struct {
	owner  *drainedRedirect
	rest   []byte
	read   int
	closed bool
}

func (b *redirectBody) Read(p []byte) (int, error) {
	if b.closed {
		b.owner.record("read the redirect body after its closing")
		return 0, errors.New("read of a closed redirect body")
	}
	if len(b.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.rest)
	b.rest = b.rest[n:]
	b.read += n
	return n, nil
}

func (b *redirectBody) Close() error {
	if !b.closed {
		b.closed = true
		b.owner.record(fmt.Sprintf("close the redirect body after %d bytes", b.read))
	}
	return nil
}

// TestProbeRedirectBodyIsDrainedBeforeNextHop sends a llama-server probe through a 302 whose body is
// instrumented. Before it requests the next hop, the probe reads a redirect body of unknown length,
// or of a known length up to 2 KiB, for at most 2 KiB, and closes it; a body of a known length over
// 2 KiB is closed unread. The next hop answers with the props, which are measured.
func TestProbeRedirectBodyIsDrainedBeforeNextHop(t *testing.T) {
	const window = 40960
	tests := map[string]struct {
		size          int
		contentLength int64
		wantRead      int
	}{
		"success: a short redirect body of known length is read to its end": {
			size:          100,
			contentLength: 100,
			wantRead:      100,
		},
		"success: a redirect body of known length exactly at the bound is read to its end": {
			size:          redirectSlurpBound,
			contentLength: redirectSlurpBound,
			wantRead:      redirectSlurpBound,
		},
		"success: a short redirect body of unknown length is read to its end": {
			size:          100,
			contentLength: -1,
			wantRead:      100,
		},
		"success: a long redirect body of unknown length is read up to the bound": {
			size:          5000,
			contentLength: -1,
			wantRead:      redirectSlurpBound,
		},
		"success: a redirect body of known length over the bound is closed unread": {
			size:          5000,
			contentLength: 5000,
			wantRead:      0,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			redirect := &drainedRedirect{
				body:          bytes.Repeat([]byte("r"), tt.size),
				contentLength: tt.contentLength,
				final:         string(llamaPropsBody(t, window, 0)),
			}
			// The port is never dialed: the in-memory transport answers every request.
			const port = 19995
			got := localprov.ProbeLlama(t.Context(), mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(port)}), localprov.ProbeOptions{Client: &http.Client{Transport: redirect}})
			want := []string{
				"request /props",
				fmt.Sprintf("close the redirect body after %d bytes", tt.wantRead),
				"request /next",
			}
			if diff := gocmp.Diff(want, redirect.recorded()); diff != "" {
				t.Errorf("probe redirect events mismatch (-want +got):\n%s", diff)
			}
			wantState := localprov.LlamaState{Port: port, Configured: true, Reachable: true, ContextWindow: window}
			if diff := gocmp.Diff(wantState, got); diff != "" {
				t.Errorf("probe state after the redirect mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
