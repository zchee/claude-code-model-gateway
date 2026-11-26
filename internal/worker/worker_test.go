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
	"bytes"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

func TestMain(m *testing.M) {
	venue.Require()
	goleak.VerifyTestMain(m)
}

const fixtureToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func equal[T any](t *testing.T, want, got T) {
	t.Helper()
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func handler(t *testing.T, options worker.Options) http.Handler {
	t.Helper()
	options.Version = "fixture-build"
	options.ControlToken = fixtureToken
	options.Lookup = func(k string) (string, bool) {
		if k == "CODEX_GATEWAY_USAGE_ENDPOINT" {
			return "0", true
		}
		return "", false
	}
	h, err := worker.NewHandler(options)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	if h == nil {
		t.Fatal("NewHandler returned a nil handler")
	}
	return h
}

func request(t *testing.T, client *http.Client, method, target, body string, headers http.Header) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(data)
}

func jsonObject(t *testing.T, data string) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal([]byte(data), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestClassificationRefusalHTTP(t *testing.T) {
	tests := map[string]struct {
		body, branch, model string
		status              int
	}{
		"error: nesting exceeds decoder limit":              {`{"model":"claude-gpt-9","messages":` + strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001) + "}", "", "", 400},
		"error: unpaired model surrogate":                   {`{"model":"claude-grok-\ud800"}`, "", "", 400},
		"success: non-JSON keeps its original bytes":        {" \xffnot JSON\r\n", "anthropic", "", 200},
		"success: Claude model selects Anthropic":           {`{"model":"claude-opus-5-5"}`, "anthropic", "", 200},
		"success: Codex uses the proxy":                     {`{"model":"claude-gpt-9"}`, "proxy", "gpt-9", 200},
		"success: Grok uses its backend":                    {`{"model":"claude-grok-4.7"}`, "grok", "grok-4.7", 200},
		"success: invalid UTF-8 is replaced before routing": {"{\"model\":\"claude-gpt-\xff\"}", "proxy", "gpt-�", 200},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			transport := &http.Transport{}
			defer transport.CloseIdleConnections()
			counts := map[string]*atomic.Int64{}
			branches := map[string]worker.Forward{}
			for _, branch := range []string{"anthropic", "proxy", "grok"} {
				count := new(atomic.Int64)
				counts[branch] = count
				endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					count.Add(1)
					data, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
					w.Header().Set("X-Fixture-Model", r.Header.Get("X-Fixture-Decision"))
					_, _ = w.Write(data)
				}))
				t.Cleanup(endpoint.Close)
				u, err := url.Parse(endpoint.URL)
				if err != nil {
					t.Fatal(err)
				}
				proxy := &httputil.ReverseProxy{Transport: transport, Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(u) }}
				branches[branch] = func(w http.ResponseWriter, r *http.Request, _ route.Snapshot, d route.Decision, raw []byte) {
					out := r.Clone(r.Context())
					out.Body = io.NopCloser(bytes.NewReader(raw))
					out.ContentLength = int64(len(raw))
					out.Header.Set("X-Fixture-Decision", d.Model)
					proxy.ServeHTTP(w, out)
				}
			}
			h := handler(t, worker.Options{Anthropic: branches["anthropic"], Proxy: branches["proxy"], Grok: branches["grok"]})
			srv := httptest.NewServer(h)
			defer srv.Close()
			status, headers, data := request(t, srv.Client(), "POST", srv.URL+"/v1/messages?beta=true", tt.body, nil)
			equal(t, tt.status, status)
			for branch, count := range counts {
				var want int64
				if branch == tt.branch {
					want = 1
				}
				if got := count.Load(); got != want {
					t.Errorf("%s contacts: got %d want %d", branch, got, want)
				}
			}
			if tt.status == 200 {
				equal(t, tt.body, data)
				if tt.model != "" {
					equal(t, tt.model, headers.Get("X-Fixture-Model"))
				}
			}
		})
	}
}

func TestLocalEndpoints(t *testing.T) {
	srv := httptest.NewServer(handler(t, worker.Options{}))
	defer srv.Close()
	status, h, body := request(t, srv.Client(), "GET", srv.URL+"/v1/models?limit=1", "", nil)
	equal(t, 200, status)
	equal(t, "application/json", h.Get("Content-Type"))
	var listing struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Type        string `json:"type"`
			Window      int    `json:"max_input_tokens"`
		} `json:"data"`
		HasMore *bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatal(err)
	}
	equal(t, new(false), listing.HasMore)
	wantIDs := []string{"gpt-6.1-sol", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"}
	if len(listing.Data) < len(wantIDs) {
		t.Fatalf("incomplete fallback catalog: %s", body)
	}
	for i, id := range wantIDs {
		row := listing.Data[i]
		equal(t, "claude-"+id+"[1m]", row.ID)
		equal(t, "GPT-"+strings.TrimPrefix(id, "gpt-")+" (Codex)", row.DisplayName)
		equal(t, "model", row.Type)
		equal(t, 272000, row.Window)
	}
	status, _, body = request(t, srv.Client(), "HEAD", srv.URL+"/v1/messages", "", nil)
	equal(t, 200, status)
	equal(t, "", body)
	status, h, body = request(t, srv.Client(), "POST", srv.URL+"/healthz?detail=1", "", nil)
	equal(t, 200, status)
	equal(t, "application/json", h.Get("Content-Type"))
	expected := fmt.Sprintf(`{"ok":true,"version":"fixture-build","models":%d,"catalog":"fallback","served":{"models":1,"codex":0,"grok":0,"anthropic":0},"draining":false,"activeRequests":0,"compaction":{"streamGuard":true,"retries":2,"maxBufferedBytes":16777216},"usage":{"enabled":false,"endpoint":null,"maxResponseBytes":4194304,"settingsLevelBaseUrl":false},"codexReadiness":{"ready":false,"state":"checking","message":"Codex readiness is still being checked (proxy /v1/models and codex auth status); ask /healthz again in a few seconds.","checks":null,"upstreamBlocked":null,"upstreamUnavailable":null,"checkedAt":null,"ageMs":null,"stale":true}}`, len(listing.Data))
	equal(t, jsonObject(t, expected), jsonObject(t, body))
}

func TestRejectMissingControlToken(t *testing.T) {
	h, err := worker.NewHandler(worker.Options{Version: "fixture-build", Lookup: func(string) (string, bool) { return "", false }})
	if err == nil || h != nil {
		t.Fatalf("NewHandler without a control token returned handler=%T err=%v", h, err)
	}
}

func TestDrainWhileForwarding(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := handler(t, worker.Options{Anthropic: func(w http.ResponseWriter, r *http.Request, _ route.Snapshot, _ route.Decision, _ []byte) {
		close(entered)
		select {
		case <-release:
			_, _ = io.WriteString(w, "finished")
		case <-r.Context().Done():
		}
	}, BeginDrain: func() {}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer close(release)
	headers := http.Header{"Content-Type": {"application/json"}, "X-Model-Gateway-Control-Token": {fixtureToken}}
	tests := map[string]struct{ header, value string }{"error: token mismatch": {"X-Model-Gateway-Control-Token", strings.Repeat("a", 64)}, "error: origin present": {"Origin", "http://localhost"}, "error: JSON media type must be exact": {"Content-Type", "application/json; charset=utf-8"}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bad := headers.Clone()
			bad.Set(tt.header, tt.value)
			s, _, b := request(t, srv.Client(), "POST", srv.URL+"/drain", "{}", bad)
			equal(t, 403, s)
			equal(t, "", b)
		})
	}
	t.Run("error: Host port differs from the listener", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), "POST", srv.URL+"/drain", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1:1"
		req.Header = headers.Clone()
		r, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		equal(t, 403, r.StatusCode)
	})
	// No network operation in the blocked request needs a timer: either the forwarder enters or the handler returns.
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/messages", strings.NewReader("plain"))
	result := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(result, req) }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("request finished without reaching Anthropic")
	}
	s, _, b := request(t, srv.Client(), "POST", srv.URL+"/drain?restart=1", "{}", headers)
	equal(t, 202, s)
	equal(t, jsonObject(t, `{"ok":true,"draining":true,"activeRequests":1}`), jsonObject(t, b))
	s, _, b = request(t, srv.Client(), "GET", srv.URL+"/healthz", "", nil)
	equal(t, 200, s)
	health := jsonObject(t, b)
	equal(t, any(true), health["draining"])
	equal(t, any(float64(1)), health["activeRequests"])
	s, _, b = request(t, srv.Client(), "GET", srv.URL+"/v1/models", "", nil)
	equal(t, 503, s)
	equal(t, jsonObject(t, `{"type":"error","error":{"type":"api_error","message":"model-gateway shim is restarting; retry this request shortly"}}`), jsonObject(t, b))
	release <- struct{}{}
	<-done
	equal(t, "finished", result.Body.String())
	_, _, b = request(t, srv.Client(), "GET", srv.URL+"/healthz", "", nil)
	equal(t, any(float64(0)), jsonObject(t, b)["activeRequests"])
}
