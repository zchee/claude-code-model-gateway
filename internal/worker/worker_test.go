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
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
)

type admission = func(outcome.Route, *outcome.Audit)

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
	const prefix = `{"model":"claude-gpt-private-model-marker","note":"private-body-marker","messages":`
	const depthMessage = "model-gateway: request exceeds the supported JSON nesting depth"
	const modelMessage = "model-gateway: model identifier contains an unpaired surrogate"
	tests := map[string]struct {
		body, branch, model, reason, message string
		local, sentry                        bool
	}{
		"error: nesting exceeds decoder limit":              {body: prefix + strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000) + "}", reason: "nesting-depth", message: depthMessage},
		"error: depth exceeded immediately before EOF":      {body: prefix + strings.Repeat("[", 10000), reason: "nesting-depth", message: depthMessage},
		"error: unpaired model surrogate":                   {body: `{"note":"private-body-marker","model":"claude-grok-private-model-marker\ud800"}`, reason: "ill-formed-model", message: modelMessage},
		"error: branch refuses before forwarding":           {body: `{"model":"claude-gpt-9"}`, branch: "proxy", local: true},
		"error: request sentry refuses after admission":     {body: `{"model":"claude-gpt-9"}`, branch: "proxy", sentry: true},
		"success: malformed name at deepest object":         {body: prefix + strings.Repeat("[", 9998) + "{[]:0}" + strings.Repeat("]", 9998) + "}", branch: "anthropic"},
		"success: non-JSON keeps its original bytes":        {body: " \xffnot JSON\r\n", branch: "anthropic"},
		"success: Claude model selects Anthropic":           {body: `{"model":"claude-opus-5-5"}`, branch: "anthropic"},
		"success: Codex uses the proxy":                     {body: `{"model":"claude-gpt-9"}`, branch: "proxy", model: "gpt-9"},
		"success: Grok uses its backend":                    {body: `{"model":"claude-grok-4.7"}`, branch: "grok", model: "grok-4.7"},
		"success: invalid UTF-8 is replaced before routing": {body: "{\"model\":\"claude-gpt-\xff\"}", branch: "proxy", model: "gpt-�"},
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
				branches[branch] = func(w http.ResponseWriter, r *http.Request, _ route.Snapshot, d route.Decision, raw []byte, admit admission) (outcome.Result, error) {
					backend := d.Provider
					if backend == "" {
						backend = branch
					}
					selected := outcome.Route{Backend: backend, EffectiveModel: d.Model, Via: "direct"}
					if tt.local {
						w.Header().Set("X-Fixture-Refused", branch)
						w.WriteHeader(http.StatusBadRequest)
						return outcome.Result{Route: selected, Status: http.StatusBadRequest}, nil
					}
					admit(selected, nil)
					if tt.sentry {
						w.WriteHeader(http.StatusRequestEntityTooLarge)
						return outcome.Result{Route: selected, Status: http.StatusRequestEntityTooLarge}, nil
					}
					out := r.Clone(r.Context())
					out.Body = io.NopCloser(bytes.NewReader(raw))
					out.ContentLength = int64(len(raw))
					out.Header.Set("X-Fixture-Decision", d.Model)
					proxy.ServeHTTP(w, out)
					return outcome.Result{Route: selected, Contacted: true, Status: http.StatusOK}, nil
				}
			}
			var diagnostics bytes.Buffer
			var probes atomic.Int64
			var terminal []outcome.Result
			h := handler(t, worker.Options{
				Diagnostics: &diagnostics, Anthropic: branches["anthropic"], Proxy: branches["proxy"], Grok: branches["grok"],
				BeforeBody: func(*http.Request) func(outcome.Result) {
					return func(result outcome.Result) { terminal = append(terminal, result) }
				},
				Models: func(context.Context, http.Header) (jsontext.Value, error) {
					probes.Add(1)
					return jsontext.Value(`{"data":[],"has_more":false}`), nil
				},
				RefreshAnthropic: func(context.Context, http.Header) { probes.Add(1) },
			})
			srv := httptest.NewServer(h)
			defer srv.Close()
			secrets := http.Header{"Authorization": {"Bearer private-auth-marker"}, "X-Api-Key": {"private-key-marker"}, "X-Model-Gateway-Control-Token": {"private-control-marker"}}
			status, headers, data := request(t, srv.Client(), "POST", srv.URL+"/v1/messages?private-query-marker", tt.body, secrets)
			healthStatus, _, health := request(t, srv.Client(), "GET", srv.URL+"/healthz", "", nil)
			srv.Close() // All handlers have returned before inspecting their diagnostics and counts.
			equal(t, 200, healthStatus)
			equal(t, 1, len(terminal))
			equal(t, status, terminal[0].Status)
			equal(t, tt.branch != "" && !tt.local && !tt.sentry, terminal[0].Contacted)
			if tt.reason != "" {
				equal(t, int64(0), probes.Load())
			}
			served := map[string]any{"models": float64(0), "codex": float64(0), "grok": float64(0), "anthropic": float64(0)}
			for branch, count := range counts {
				var want int64
				if branch == tt.branch && !tt.local {
					if !tt.sentry {
						want = 1
					}
					provider := branch
					if provider == "proxy" {
						provider = "codex"
					}
					served[provider] = float64(1)
				}
				if got := count.Load(); got != want {
					t.Errorf("%s contacts: got %d want %d", branch, got, want)
				}
			}
			equal(t, any(served), jsonObject(t, health)["served"])
			switch {
			case tt.reason != "":
				equal(t, 400, status)
				equal(t, "application/json", headers.Get("Content-Type"))
				equal(t, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": tt.message}}, jsonObject(t, data))
				log := diagnostics.String()
				if !strings.Contains(log, tt.reason) || !strings.Contains(log, "400") {
					t.Errorf("missing refusal reason or status in %q", log)
				}
				for _, forbidden := range []string{"private-body-marker", "private-model-marker", "private-query-marker", "private-auth-marker", "private-key-marker", "private-control-marker", "authorization", "x-api-key", "x-model-gateway-control-token"} {
					if strings.Contains(strings.ToLower(log), forbidden) {
						t.Errorf("refusal log contains %q", forbidden)
					}
				}
			case tt.sentry:
				equal(t, http.StatusRequestEntityTooLarge, status)
			case tt.local:
				equal(t, 400, status)
				equal(t, tt.branch, headers.Get("X-Fixture-Refused"))
			default:
				equal(t, 200, status)
				equal(t, tt.body, data)
				if tt.model != "" {
					equal(t, tt.model, headers.Get("X-Fixture-Model"))
				}
			}
		})
	}
}

func TestLocalEndpoints(t *testing.T) {
	srv := httptest.NewServer(handler(t, worker.Options{BeforeBody: func(*http.Request) func(outcome.Result) {
		t.Error("local endpoint started request observation")
		return nil
	}}))
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
	equal(t, len(wantIDs), len(listing.Data))
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

func TestPublishedCatalog(t *testing.T) {
	const rows = `[{"type":"model","id":"claude-grok-private-2","display_name":"Private"}]`
	var store route.Store
	called := false
	h := handler(t, worker.Options{Routes: &store, Grok: func(w http.ResponseWriter, _ *http.Request, snap route.Snapshot, d route.Decision, _ []byte, admit admission) (outcome.Result, error) {
		called = true
		source, catalog := snap.Catalog()
		equal(t, "proxy", source)
		equal(t, rows, string(catalog))
		equal(t, route.Decision{Branch: "grok", Provider: "grok", Model: "grok-private.2"}, d)
		selected := outcome.Route{Backend: d.Provider, EffectiveModel: d.Model}
		admit(selected, nil)
		w.WriteHeader(http.StatusNoContent)
		return outcome.Result{Route: selected, Status: http.StatusNoContent, Contacted: true}, nil
	}})
	store.Publish(route.NewSnapshot(policy.Resolver{}, "proxy", jsontext.Value(rows), []string{"grok-private.2"}))
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body)))
		return r
	}
	listing := serve("GET", "/v1/models", "")
	equal(t, 200, listing.Code)
	equal(t, jsonObject(t, `{"has_more":false,"data":`+rows+`}`), jsonObject(t, listing.Body.String()))
	health := serve("GET", "/healthz", "")
	equal(t, 200, health.Code)
	data := jsonObject(t, health.Body.String())
	equal(t, any("proxy"), data["catalog"])
	equal(t, any(float64(1)), data["models"])
	equal(t, 204, serve("POST", "/v1/messages/count_tokens", `{"model":"claude-grok-private-2"}`).Code)
	equal(t, true, called)
}

type readFunc func([]byte) (int, error)

func (read readFunc) Read(p []byte) (int, error) { return read(p) }

func TestRequestStartBeforeBody(t *testing.T) {
	tests := map[string]struct{ cancelRead, abort bool }{
		"success: branch returns terminal facts":             {},
		"error: client cancels during first read":            {cancelRead: true},
		"error: committed stream aborts at handler boundary": {abort: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			const body = "request bytes"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var events []string
			var results []outcome.Result
			first := sync.OnceFunc(func() { events = append(events, "first read") })
			reader := strings.NewReader(body)
			r := httptest.NewRequestWithContext(ctx, "POST", "/v1/messages", readFunc(func(p []byte) (int, error) {
				first()
				if tt.cancelRead {
					cancel()
					return 0, ctx.Err()
				}
				return reader.Read(p)
			}))
			r.Header.Set("X-Fixture-Request", "body observation")
			facts := outcome.Result{Route: outcome.Route{Backend: "anthropic", Via: "direct"}, Status: 200, Contacted: true}
			h := handler(t, worker.Options{BeforeBody: func(req *http.Request) func(outcome.Result) {
				equal(t, "body observation", req.Header.Get("X-Fixture-Request"))
				events = append(events, "start")
				return func(result outcome.Result) {
					events = append(events, "finalize")
					results = append(results, result)
				}
			}, Anthropic: func(w http.ResponseWriter, _ *http.Request, _ route.Snapshot, _ route.Decision, raw []byte, admit admission) (outcome.Result, error) {
				events = append(events, "forward")
				equal(t, body, string(raw))
				admit(facts.Route, nil)
				_, _ = io.WriteString(w, "committed")
				if tt.abort {
					facts.StatusOverride = "upstream_aborted"
					return facts, http.ErrAbortHandler
				}
				return facts, nil
			}})
			w := httptest.NewRecorder()
			aborted := func() (p any) {
				defer func() { p = recover() }()
				h.ServeHTTP(w, r)
				return nil
			}()
			isAbort := aborted == http.ErrAbortHandler //nolint:errorlint // net/http requires the exact panic sentinel, not a wrapped error.
			equal(t, 1, len(results))
			if tt.cancelRead {
				equal(t, []string{"start", "first read", "finalize"}, events)
				equal(t, true, results[0].Cancelled)
				equal(t, false, results[0].Contacted)
				if results[0].Status == 499 || w.Code == 499 || (aborted != nil && !isAbort) {
					t.Fatalf("cancelled read: result=%+v HTTP=%d panic=%v", results[0], w.Code, aborted)
				}
				return
			}
			equal(t, []string{"start", "first read", "forward", "finalize"}, events)
			equal(t, facts, results[0])
			equal(t, "committed", w.Body.String())
			if (tt.abort && !isAbort) || (!tt.abort && aborted != nil) {
				t.Fatalf("branch abort=%t, handler panic=%v", tt.abort, aborted)
			}
		})
	}
}

func TestAbsoluteRequestTarget(t *testing.T) {
	tests := map[string]struct{ method, path string }{
		"success: drain is not local":           {"POST", "/drain"},
		"success: health is not local":          {"GET", "/healthz"},
		"success: catalog is not local":         {"GET", "/v1/models"},
		"success: messages prefix is not local": {"POST", "/v1/messages/count_tokens"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			target := "http://fixture.invalid" + tt.path + "?detail=1"
			h := handler(t, worker.Options{BeginDrain: func() { t.Error("unexpected drain") }, Anthropic: func(w http.ResponseWriter, r *http.Request, _ route.Snapshot, d route.Decision, _ []byte, admit admission) (outcome.Result, error) {
				equal(t, target, r.RequestURI)
				equal(t, route.Decision{Branch: "anthropic"}, d)
				selected := outcome.Route{Backend: "anthropic"}
				admit(selected, nil)
				w.WriteHeader(http.StatusNoContent)
				return outcome.Result{Route: selected, Status: http.StatusNoContent, Contacted: true}, nil
			}})
			r := httptest.NewRequestWithContext(t.Context(), tt.method, target, strings.NewReader(`{"model":"claude-gpt-9"}`))
			r.Host = "other.invalid"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			equal(t, http.StatusNoContent, w.Code)
		})
	}
}

func TestRejectMissingControlToken(t *testing.T) {
	tests := map[string]struct{ token string }{"error: missing token": {}, "error: non-hex token of correct length": {strings.Repeat("g", 64)}}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h, err := worker.NewHandler(worker.Options{Version: "fixture-build", ControlToken: tt.token, Lookup: func(string) (string, bool) { return "", false }})
			if err == nil || h != nil {
				t.Fatalf("NewHandler with invalid token returned handler=%T err=%v", h, err)
			}
		})
	}
}

func TestDrainWhileForwarding(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	drained := make(chan struct{})
	h := handler(t, worker.Options{BeforeBody: func(r *http.Request) func(outcome.Result) {
		if r.RequestURI != "/v1/messages" {
			t.Errorf("unexpected observation for %s", r.RequestURI)
		}
		return nil
	}, Anthropic: func(w http.ResponseWriter, r *http.Request, _ route.Snapshot, _ route.Decision, _ []byte, admit admission) (outcome.Result, error) {
		selected := outcome.Route{Backend: "anthropic", Via: "direct"}
		admit(selected, nil)
		_, _ = io.WriteString(w, "start ")
		close(entered)
		select {
		case <-release:
			_, _ = io.WriteString(w, "finished")
		case <-r.Context().Done():
		}
		return outcome.Result{Route: selected, Status: http.StatusOK, Contacted: true}, nil
	}, BeginDrain: func() {
		if calls.Add(1) == 1 {
			close(drained)
		}
	}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	headers := http.Header{"Content-Type": {"application/json"}, "X-Model-Gateway-Control-Token": {fixtureToken}}
	tests := map[string]struct {
		header string
		values []string
	}{
		"error: token mismatch":                {"X-Model-Gateway-Control-Token", []string{strings.Repeat("a", 64)}},
		"error: origin present":                {"Origin", []string{"http://localhost"}},
		"error: empty origin":                  {"Origin", []string{""}},
		"error: repeated origin":               {"Origin", []string{"", "http://localhost"}},
		"error: JSON media type must be exact": {"Content-Type", []string{"application/json; charset=utf-8"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			bad := headers.Clone()
			bad[tt.header] = tt.values
			s, _, b := request(t, srv.Client(), "POST", srv.URL+"/drain", "{}", bad)
			equal(t, int64(0), calls.Load())
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
		equal(t, int64(0), calls.Load())
	})
	// No network operation in the blocked request needs a timer: either the forwarder enters or the handler returns.
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/messages", strings.NewReader("plain"))
	result := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(result, req) }()
	unblock := sync.OnceFunc(func() { close(release) })
	defer func() { unblock(); <-done }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("request finished without reaching Anthropic")
	}
	s, respHeaders, b := request(t, srv.Client(), "POST", srv.URL+"/drain?restart=1", "{}", headers)
	equal(t, 202, s)
	equal(t, "application/json", respHeaders.Get("Content-Type"))
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted drain did not call BeginDrain")
	}
	equal(t, int64(1), calls.Load())
	equal(t, jsonObject(t, `{"ok":true,"draining":true,"activeRequests":1}`), jsonObject(t, b))
	s, _, b = request(t, srv.Client(), "GET", srv.URL+"/healthz", "", nil)
	equal(t, 200, s)
	health := jsonObject(t, b)
	equal(t, any(true), health["draining"])
	equal(t, any(float64(1)), health["activeRequests"])
	equal(t, any(map[string]any{"anthropic": float64(1), "models": float64(0), "codex": float64(0), "grok": float64(0)}), health["served"])
	s, respHeaders, b = request(t, srv.Client(), "GET", srv.URL+"/v1/models", "", nil)
	equal(t, 503, s)
	equal(t, "application/json", respHeaders.Get("Content-Type"))
	equal(t, jsonObject(t, `{"type":"error","error":{"type":"api_error","message":"model-gateway shim is restarting; retry this request shortly"}}`), jsonObject(t, b))
	unblock()
	<-done
	equal(t, "start finished", result.Body.String())
	_, _, b = request(t, srv.Client(), "GET", srv.URL+"/healthz", "", nil)
	equal(t, any(float64(0)), jsonObject(t, b)["activeRequests"])
}
