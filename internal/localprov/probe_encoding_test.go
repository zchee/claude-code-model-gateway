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
	"compress/gzip"
	json "encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
)

// probeBodyBound restates localprov's maxProbeBody: the most bytes one probe response may carry.
const probeBodyBound = 16 << 20

// largeTransferTimeout is the probe timeout of a case that moves more than probeBodyBound bytes over
// loopback. It is far above any transfer time, so a slow runner cannot turn the size outcome into a
// timeout; the probe's own transport is still selected, since only the timeout is set.
const largeTransferTimeout = 2 * time.Minute

// encodedAnswer is a local engine that answers every request with a fixed body and records the
// Accept-Encoding field of each request it receives.
type encodedAnswer struct {
	url  string
	port uint16

	mu sync.Mutex
	// acceptEncoding holds one entry per request: the field's values, or nil when it was absent.
	acceptEncoding [][]string
	present        []bool
}

// startEncodedAnswer serves wire as the body of every answer, with Content-Encoding: gzip when
// gzipped is set, whatever the request asked for.
func startEncodedAnswer(t *testing.T, wire []byte, gzipped bool) *encodedAnswer {
	t.Helper()
	answer := &encodedAnswer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, ok := r.Header["Accept-Encoding"]
		answer.mu.Lock()
		answer.acceptEncoding = append(answer.acceptEncoding, values)
		answer.present = append(answer.present, ok)
		answer.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if gzipped {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
		_, _ = w.Write(wire)
	}))
	t.Cleanup(srv.Close)
	answer.url = srv.URL
	answer.port = uint16(srv.Listener.Addr().(*net.TCPAddr).Port)
	return answer
}

// requests returns, per request received, whether Accept-Encoding was present and its values.
func (a *encodedAnswer) requests() ([]bool, [][]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.present), slices.Clone(a.acceptEncoding)
}

// gzipBytes compresses plain at level.
func gzipBytes(t *testing.T, plain []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatalf("gzip writer: %v", err)
	}
	if _, err := zw.Write(plain); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// llamaPropsBody is a llama-server /props answer reporting window, padded with trailing spaces to
// size bytes when size is positive. Trailing whitespace keeps the document valid JSON.
func llamaPropsBody(t *testing.T, window int64, size int) []byte {
	t.Helper()
	doc, err := json.Marshal(map[string]any{"default_generation_settings": map[string]any{"n_ctx": window}})
	if err != nil {
		t.Fatalf("marshal llama props: %v", err)
	}
	if size <= 0 {
		return doc
	}
	if len(doc) > size {
		t.Fatalf("test precondition: a %d-byte document does not fit in %d bytes", len(doc), size)
	}
	return append(doc, bytes.Repeat([]byte(" "), size-len(doc))...)
}

// TestProbeDefaultTransportAsksForNoEncoding probes llama-server and LM Studio through the probe's own
// transport, which a client without a transport selects. A local engine receives no Accept-Encoding
// field, as the source's request carries none.
func TestProbeDefaultTransportAsksForNoEncoding(t *testing.T) {
	tests := map[string]struct {
		probe func(t *testing.T, answer *encodedAnswer, opts localprov.ProbeOptions)
		body  func(t *testing.T) []byte
	}{
		"success: a llama-server probe without a client asks for no encoding": {
			body: func(t *testing.T) []byte { return llamaPropsBody(t, 40960, 0) },
			probe: func(t *testing.T, answer *encodedAnswer, opts localprov.ProbeOptions) {
				localprov.ProbeLlama(t.Context(), mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(answer.port))}), opts)
			},
		},
		"success: an LM Studio probe without a client asks for no encoding": {
			body: func(t *testing.T) []byte { return studioModelsBody(t, 65536) },
			probe: func(t *testing.T, answer *encodedAnswer, opts localprov.ProbeOptions) {
				localprov.ProbeStudio(t.Context(), mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": answer.url}), t.TempDir(), opts)
			},
		},
		"success: an LM Studio probe whose client names the default transport asks for no encoding": {
			body: func(t *testing.T) []byte { return studioModelsBody(t, 65536) },
			probe: func(t *testing.T, answer *encodedAnswer, _ localprov.ProbeOptions) {
				opts := localprov.ProbeOptions{Client: &http.Client{Transport: http.DefaultTransport}}
				localprov.ProbeStudio(t.Context(), mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": answer.url}), t.TempDir(), opts)
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			answer := startEncodedAnswer(t, tt.body(t), false)
			tt.probe(t, answer, localprov.ProbeOptions{})
			present, values := answer.requests()
			if len(present) != 1 {
				t.Fatalf("test precondition: the engine received %d request(s), want 1", len(present))
			}
			if present[0] {
				t.Errorf("probe request carried Accept-Encoding %q, want no Accept-Encoding field", values[0])
			}
		})
	}
}

// TestProbeDefaultTransportKeepsCompressedBytes answers probes through the probe's own transport with
// a body marked Content-Encoding: gzip that the request never asked for. The probe reads the bytes as
// sent, as the source does: a compressed answer is not the JSON it encodes, so no window or model
// list is measured from it, and the size bound counts the compressed bytes, so a stored gzip answer
// whose decoded text is exactly at the bound is too large. An identity answer through the same fake
// engine is measured.
func TestProbeDefaultTransportKeepsCompressedBytes(t *testing.T) {
	tests := map[string]struct {
		// plain is the decoded answer; wire is what the engine sends.
		plain   func(t *testing.T) []byte
		gzipped bool
		level   int
		// probe runs the probe against answer with opts and returns its state for comparison.
		probe func(t *testing.T, answer *encodedAnswer, opts localprov.ProbeOptions) any
		// timeout is the probe timeout; zero leaves localprov's default.
		timeout time.Duration
		want    func(answer *encodedAnswer) any
		// wireOverBound requires the sent bytes to exceed probeBodyBound while plain does not.
		wireOverBound bool
	}{
		"success: an identity llama-server answer is measured": {
			plain: func(t *testing.T) []byte { return llamaPropsBody(t, 40960, 0) },
			probe: probeLlamaState,
			want: func(a *encodedAnswer) any {
				return localprov.LlamaState{Port: a.port, Configured: true, Reachable: true, ContextWindow: 40960}
			},
		},
		"error: a gzip llama-server answer is not decoded into a window": {
			plain:   func(t *testing.T) []byte { return llamaPropsBody(t, 40960, 0) },
			gzipped: true,
			level:   gzip.BestCompression,
			probe:   probeLlamaState,
			want: func(a *encodedAnswer) any {
				return localprov.LlamaState{Port: a.port, Configured: true, Reachable: true, ContextWindow: 0}
			},
		},
		"error: a stored gzip llama-server answer at the decoded bound is too large on the wire": {
			plain:         func(t *testing.T) []byte { return llamaPropsBody(t, 40960, probeBodyBound) },
			gzipped:       true,
			level:         gzip.NoCompression,
			wireOverBound: true,
			timeout:       largeTransferTimeout,
			probe:         probeLlamaState,
			want: func(a *encodedAnswer) any {
				return localprov.LlamaState{Port: a.port, Configured: true, Reachable: false, Error: "response too large"}
			},
		},
		"success: an identity LM Studio answer is measured": {
			plain: func(t *testing.T) []byte { return studioModelsBody(t, 65536) },
			probe: probeStudioState,
			want: func(a *encodedAnswer) any {
				return localprov.StudioState{Enabled: true, BaseURL: a.url, Reachable: true, Models: []localprov.Model{{Key: "qwen3-coder-30b", Loaded: true, ContextWindow: 65536}}}
			},
		},
		"error: a gzip LM Studio answer is not decoded into a model list": {
			plain:   func(t *testing.T) []byte { return studioModelsBody(t, 65536) },
			gzipped: true,
			level:   gzip.BestCompression,
			probe:   probeStudioState,
			want: func(a *encodedAnswer) any {
				return localprov.StudioState{Enabled: true, BaseURL: a.url, Error: "GET /api/v1/models returned no model list"}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			plain := tt.plain(t)
			wire := plain
			if tt.gzipped {
				wire = gzipBytes(t, plain, tt.level)
				if bytes.Equal(wire, plain) {
					t.Fatal("test precondition: the gzip answer equals its decoded text")
				}
			}
			if tt.wireOverBound && (len(plain) > probeBodyBound || len(wire) <= probeBodyBound) {
				t.Fatalf("test precondition: decoded %d and sent %d bytes, want the decoded text within and the sent bytes over the %d-byte bound", len(plain), len(wire), probeBodyBound)
			}
			answer := startEncodedAnswer(t, wire, tt.gzipped)
			got := tt.probe(t, answer, localprov.ProbeOptions{Timeout: tt.timeout})
			if present, _ := answer.requests(); len(present) != 1 {
				t.Fatalf("test precondition: the engine received %d request(s), want 1", len(present))
			}
			if diff := gocmp.Diff(tt.want(answer), got); diff != "" {
				t.Errorf("probe state mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func probeLlamaState(t *testing.T, answer *encodedAnswer, opts localprov.ProbeOptions) any {
	return localprov.ProbeLlama(t.Context(), mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(answer.port))}), opts)
}

func probeStudioState(t *testing.T, answer *encodedAnswer, opts localprov.ProbeOptions) any {
	return localprov.ProbeStudio(t.Context(), mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": answer.url}), t.TempDir(), opts)
}
