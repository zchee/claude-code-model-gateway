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
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/localprov"
	"github.com/zchee/claude-code-model-gateway/internal/proxymodels"
)

const (
	probeJarCookieMarker = "probe-jar-cookie-marker-1d4f"
	probeAuthMarker      = "probe-auth-marker-8e27"
	// probeBodyLimit is the largest probe response body read as complete.
	probeBodyLimit = 16 << 20
)

// probeHop is what a fake server saw of one probe request.
type probeHop struct {
	Server string
	Path   string
	Auth   bool
}

// probeHopLog records the probe requests of every fake server of one test, with their Referer.
type probeHopLog struct {
	mu       sync.Mutex
	hops     []probeHop
	referers []string
	cookies  []bool
}

func (l *probeHopLog) record(server string, r *http.Request) {
	_, auth := r.Header["Authorization"]
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hops = append(l.hops, probeHop{Server: server, Path: r.URL.Path, Auth: auth})
	l.referers = append(l.referers, r.Header.Get("Referer"))
	l.cookies = append(l.cookies, len(r.Header.Values("Cookie")) > 0)
}

func (l *probeHopLog) snapshot() ([]probeHop, []string, []bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.hops), slices.Clone(l.referers), slices.Clone(l.cookies)
}

// probeOutcome is what a tracker refresh reports about one probe.
type probeOutcome struct {
	Reachable bool
	Window    int64
	Error     string
}

func studioModelsBody(t *testing.T, contextLength int64) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"models": []map[string]any{studioCatalogModel("qwen3-coder-30b", []studioInstanceSpec{{"qwen3-coder-30b", contextLength}}, "llm")},
	})
	if err != nil {
		t.Fatalf("marshal studio models: %v", err)
	}
	return data
}

// TestProbesNeverUseInjectedCookieJar hands each probe a client with a cookie jar holding a cookie
// for the probed server: no probe request carries a Cookie header, whether or not the client names a
// transport.
func TestProbesNeverUseInjectedCookieJar(t *testing.T) {
	tests := map[string]struct {
		ownTransport bool
		probe        func(t *testing.T, serverURL string, port uint16, opts localprov.ProbeOptions) bool
	}{
		"success: LM Studio probe with a client without a transport": {
			probe: probeStudioReachable,
		},
		"success: LM Studio probe with a client with its own transport": {
			ownTransport: true,
			probe:        probeStudioReachable,
		},
		"success: llama-server probe with a client without a transport": {
			probe: probeLlamaReachable,
		},
		"success: llama-server probe with a client with its own transport": {
			ownTransport: true,
			probe:        probeLlamaReachable,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var log probeHopLog
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record("engine", r)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/props":
					_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":65536}}`))
				case "/api/v1/models":
					_, _ = w.Write(studioModelsBody(t, 65536))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatalf("cookiejar.New: %v", err)
			}
			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: probeJarCookieMarker, Path: "/"}})
			client := &http.Client{Jar: jar}
			if tt.ownTransport {
				tr := &http.Transport{Proxy: nil}
				t.Cleanup(tr.CloseIdleConnections)
				client.Transport = tr
			} else {
				t.Cleanup(http.DefaultTransport.(*http.Transport).CloseIdleConnections)
			}

			port := uint16(srv.Listener.Addr().(*net.TCPAddr).Port)
			if !tt.probe(t, srv.URL, port, localprov.ProbeOptions{Client: client}) {
				t.Fatal("test precondition: the probe did not reach the fake engine")
			}
			hops, _, cookies := log.snapshot()
			for i, hop := range hops {
				if cookies[i] {
					t.Errorf("probe request to %s carried a Cookie header from the injected client's cookie jar", hop.Path)
				}
			}
		})
	}
}

func probeStudioReachable(t *testing.T, serverURL string, _ uint16, opts localprov.ProbeOptions) bool {
	state := localprov.ProbeStudio(t.Context(), mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": serverURL}), t.TempDir(), opts)
	return state.Reachable
}

func probeLlamaReachable(t *testing.T, _ string, port uint16, opts localprov.ProbeOptions) bool {
	state := localprov.ProbeLlama(t.Context(), mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(port))}), opts)
	return state.Reachable
}

// TestProbeRedirectHygiene follows LM Studio probe redirects between two fake servers. A redirected
// request carries no Referer, a redirect target with userinfo is refused on any origin, and the
// configured Authorization never reaches another origin, even when the chain returns to the first one.
func TestProbeRedirectHygiene(t *testing.T) {
	tests := map[string]struct {
		// location is where the first server sends /api/v1/models; first and second are server URLs.
		location func(first, second string) string
		// secondNext, when set, is where the second server sends its request on.
		secondNext func(first string) string
		wantHops   []probeHop
	}{
		"success: same-origin redirect chain sends no Referer": {
			location: func(string, string) string { return "/hop?state=probe-query-marker" },
			wantHops: []probeHop{
				{Server: "first", Path: "/api/v1/models", Auth: true},
				{Server: "first", Path: "/hop", Auth: true},
				{Server: "first", Path: "/final", Auth: true},
			},
		},
		"success: cross-origin redirect sends no Referer and no Authorization": {
			location: func(_, second string) string { return second + "/cross" },
			wantHops: []probeHop{
				{Server: "first", Path: "/api/v1/models", Auth: true},
				{Server: "second", Path: "/cross", Auth: false},
			},
		},
		"success: Authorization stays stripped when the chain returns to the first origin": {
			location:   func(_, second string) string { return second + "/cross" },
			secondNext: func(first string) string { return first + "/back" },
			wantHops: []probeHop{
				{Server: "first", Path: "/api/v1/models", Auth: true},
				{Server: "second", Path: "/cross", Auth: false},
				{Server: "first", Path: "/back", Auth: false},
			},
		},
		"error: redirect to another origin with userinfo is refused": {
			location: func(_, second string) string {
				return strings.Replace(second, "http://", "http://probe-user:probe-pass@", 1) + "/cross"
			},
			wantHops: []probeHop{
				{Server: "first", Path: "/api/v1/models", Auth: true},
			},
		},
		"error: redirect to the same origin with userinfo is refused": {
			location: func(first, _ string) string {
				return strings.Replace(first, "http://", "http://probe-user:probe-pass@", 1) + "/final"
			},
			wantHops: []probeHop{
				{Server: "first", Path: "/api/v1/models", Auth: true},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var log probeHopLog
			var firstURL, secondURL atomic.Value
			answer := func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(studioModelsBody(t, 65536))
			}
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record("first", r)
				switch r.URL.Path {
				case "/api/v1/models":
					w.Header().Set("Location", tt.location(firstURL.Load().(string), secondURL.Load().(string)))
					w.WriteHeader(http.StatusFound)
				case "/hop":
					w.Header().Set("Location", "/final")
					w.WriteHeader(http.StatusFound)
				default:
					answer(w)
				}
			}))
			t.Cleanup(first.Close)
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.record("second", r)
				if tt.secondNext != nil {
					w.Header().Set("Location", tt.secondNext(firstURL.Load().(string)))
					w.WriteHeader(http.StatusFound)
					return
				}
				answer(w)
			}))
			t.Cleanup(second.Close)
			firstURL.Store(first.URL)
			secondURL.Store(second.URL)

			env := mapLookup(map[string]string{
				"CCP_LMSTUDIO_BASE_URL": first.URL,
				"CCP_LMSTUDIO_API_KEY":  probeAuthMarker,
			})
			_ = localprov.ProbeStudio(t.Context(), env, t.TempDir(), localprov.ProbeOptions{})

			hops, referers, _ := log.snapshot()
			if diff := gocmp.Diff(tt.wantHops, hops); diff != "" {
				t.Errorf("probe requests mismatch (-want +got):\n%s", diff)
			}
			for i, referer := range referers {
				if referer != "" {
					t.Errorf("probe request %d to %s %s carried Referer %q", i, hops[i].Server, hops[i].Path, referer)
				}
			}
		})
	}
}

// TestOverLimitProbeBodyKeepsLastMeasurement answers a second probe with a valid JSON document padded
// with whitespace past 16 MiB, so only the size bound can refuse it. The body is over the limit, so
// the probe is unreadable with a size error and the tracker keeps its last measurement.
func TestOverLimitProbeBodyKeepsLastMeasurement(t *testing.T) {
	tests := map[string]struct {
		path    string
		first   func(t *testing.T) []byte
		second  func(t *testing.T) []byte
		refresh func(t *testing.T, serverURL string, port uint16, opts localprov.ProbeOptions) probeOutcome
	}{
		"success: llama-server keeps the last window": {
			path:   "/props",
			first:  func(*testing.T) []byte { return []byte(`{"default_generation_settings":{"n_ctx":65536}}`) },
			second: func(*testing.T) []byte { return []byte(`{"default_generation_settings":{"n_ctx":40960}}`) },
			refresh: func() func(*testing.T, string, uint16, localprov.ProbeOptions) probeOutcome {
				var tracker localprov.LlamaTracker
				routes := []proxymodels.Route{{ID: "local/gguf", Provider: "local"}}
				return func(t *testing.T, _ string, port uint16, opts localprov.ProbeOptions) probeOutcome {
					state := tracker.Refresh(t.Context(), routes, mapLookup(map[string]string{"CCP_LLAMA_PORT": strconv.Itoa(int(port))}), opts)
					if state == nil {
						return probeOutcome{}
					}
					return probeOutcome{Reachable: state.Reachable, Window: state.ContextWindow, Error: state.Error}
				}
			}(),
		},
		"success: LM Studio keeps the last model windows": {
			path:   "/api/v1/models",
			first:  func(t *testing.T) []byte { return studioModelsBody(t, 65536) },
			second: func(t *testing.T) []byte { return studioModelsBody(t, 40960) },
			refresh: func() func(*testing.T, string, uint16, localprov.ProbeOptions) probeOutcome {
				var tracker localprov.StudioTracker
				routes := []proxymodels.Route{{ID: "lmstudio/qwen3-coder-30b", Provider: "lmstudio"}}
				return func(t *testing.T, serverURL string, _ uint16, opts localprov.ProbeOptions) probeOutcome {
					state := tracker.Refresh(t.Context(), routes, mapLookup(map[string]string{"CCP_LMSTUDIO_BASE_URL": serverURL}), t.TempDir(), opts)
					if state == nil {
						return probeOutcome{}
					}
					return probeOutcome{Reachable: state.Reachable, Window: state.Windows()["qwen3-coder-30b"], Error: state.Error}
				}
			}(),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			second := tt.second(t)
			if !jsontext.Value(second).IsValid() {
				t.Fatalf("test precondition: the second document is not valid JSON: %s", second)
			}
			// Trailing whitespace keeps the document valid, so a reader without the bound would accept
			// the second measurement.
			oversized := make([]byte, 0, probeBodyLimit+16)
			oversized = append(oversized, second...)
			for len(oversized) <= probeBodyLimit+8 {
				oversized = append(oversized, ' ')
			}
			if !jsontext.Value(oversized).IsValid() {
				t.Fatal("test precondition: the padded body is not valid JSON")
			}
			bodies := [][]byte{tt.first(t), oversized}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.path {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				n := min(int(calls.Add(1)), len(bodies)) - 1
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(bodies[n])
			}))
			t.Cleanup(srv.Close)
			port := uint16(srv.Listener.Addr().(*net.TCPAddr).Port)
			// The oversized answer takes longer than the default probe deadline under the race detector.
			opts := localprov.ProbeOptions{Timeout: 30 * time.Second}

			if got := tt.refresh(t, srv.URL, port, opts); !got.Reachable || got.Window != 65536 {
				t.Fatalf("test precondition: first probe = %+v, want reachable with window 65536", got)
			}
			got := tt.refresh(t, srv.URL, port, opts)
			if n := calls.Load(); n != 2 {
				t.Fatalf("test precondition: the engine answered %d probe request(s), want 2", n)
			}
			want := probeOutcome{Window: 65536, Error: "response too large"}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Errorf("probe answered with a body over the limit mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
