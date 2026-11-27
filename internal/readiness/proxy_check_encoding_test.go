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
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/readiness"
)

// proxyModelsBound restates readiness's maxProxyModelsBody: the largest /v1/models body the check
// reads as an answer.
const proxyModelsBound = 1 << 20

// encodedModels is a proxy whose /v1/models answers with a fixed body and which records whether each
// request carried an Accept-Encoding field.
type encodedModels struct {
	url string

	mu      sync.Mutex
	present []bool
	values  [][]string
}

// startEncodedModels serves wire as every answer's body, with Content-Encoding: gzip when gzipped is
// set, whatever the request asked for.
func startEncodedModels(t *testing.T, wire []byte, gzipped bool) *encodedModels {
	t.Helper()
	models := &encodedModels{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, ok := r.Header["Accept-Encoding"]
		models.mu.Lock()
		models.present = append(models.present, ok)
		models.values = append(models.values, values)
		models.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if gzipped {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
		_, _ = w.Write(wire)
	}))
	t.Cleanup(srv.Close)
	models.url = srv.URL + "/v1/models"
	return models
}

func (m *encodedModels) requests() ([]bool, [][]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.present), slices.Clone(m.values)
}

func gzipModels(t *testing.T, plain []byte, level int) []byte {
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

// TestProxyCheckDefaultTransportAsksForNoEncoding runs the /v1/models check through the check's own
// transport, which a missing client or one naming the default transport selects. The proxy receives
// no Accept-Encoding field, as the source's request carries none.
func TestProxyCheckDefaultTransportAsksForNoEncoding(t *testing.T) {
	tests := map[string]struct {
		client *http.Client
	}{
		"success: a check without a client asks for no encoding": {},
		"success: a check whose client has no transport asks for no encoding": {
			client: &http.Client{},
		},
		"success: a check whose client names the default transport asks for no encoding": {
			client: &http.Client{Transport: http.DefaultTransport},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			models := startEncodedModels(t, []byte(`{"data":[]}`), false)
			got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
				ProxyBinary:    fakeProxyBinary(t),
				ProxyModelsURL: models.url,
				Client:         tt.client,
			})
			present, values := models.requests()
			if len(present) != 1 || !got.ProxyModels {
				t.Fatalf("test precondition: the proxy received %d request(s) and the check = %+v, want 1 request and an answering proxy", len(present), got)
			}
			if present[0] {
				t.Errorf("the /v1/models request carried Accept-Encoding %q, want no Accept-Encoding field", values[0])
			}
		})
	}
}

// TestProxyCheckDefaultTransportCountsWireBytes answers the /v1/models check through the check's own
// transport with bodies marked Content-Encoding: gzip that the request never asked for. The check
// reads the bytes as sent, as the source does, so its size bound counts the compressed bytes: a stored
// gzip body whose decoded text is exactly at the bound is over it on the wire and is no answer, while
// a small compressed body whose decoded text would be far over the bound is a complete answer. An
// identity body at the bound through the same fake proxy is an answer.
func TestProxyCheckDefaultTransportCountsWireBytes(t *testing.T) {
	tests := map[string]struct {
		// plainSize is the decoded body's length; level is the gzip level when gzipped is set.
		plainSize int
		gzipped   bool
		level     int
		// wireOverBound states which side of the bound the sent bytes must fall on.
		wireOverBound bool
		want          bool
	}{
		"success: an identity body at the bound is an answer": {
			plainSize: proxyModelsBound,
			want:      true,
		},
		"error: a stored gzip body at the decoded bound is over the bound on the wire": {
			plainSize:     proxyModelsBound,
			gzipped:       true,
			level:         gzip.NoCompression,
			wireOverBound: true,
		},
		"success: a compressed body far under the bound on the wire is an answer": {
			plainSize: 4 * proxyModelsBound,
			gzipped:   true,
			level:     gzip.BestCompression,
			want:      true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			plain := paddedModelsBody(t, tt.plainSize)
			wire := plain
			if tt.gzipped {
				wire = gzipModels(t, plain, tt.level)
			}
			if (len(wire) > proxyModelsBound) != tt.wireOverBound {
				t.Fatalf("test precondition: %d bytes on the wire for a %d-byte body, want over the %d-byte bound = %v", len(wire), len(plain), proxyModelsBound, tt.wireOverBound)
			}
			models := startEncodedModels(t, wire, tt.gzipped)
			got := readiness.ProbeProxyAndAuth(t.Context(), readiness.ProxyAuthProbe{
				ProxyBinary:    fakeProxyBinary(t),
				ProxyModelsURL: models.url,
			})
			if present, _ := models.requests(); len(present) != 1 {
				t.Fatalf("test precondition: the proxy received %d request(s), want 1", len(present))
			}
			want := readiness.ProxyAuthResult{ProxyBinary: true, ProxyModels: tt.want}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("ProbeProxyAndAuth with %d bytes on the wire for a %d-byte body mismatch (-want +got):\n%s", len(wire), len(plain), diff)
			}
		})
	}
}
