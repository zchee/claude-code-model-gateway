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

//go:build process

package supervisor_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
	"github.com/zchee/claude-code-model-gateway/internal/supervisor"
)

func TestListenerControlRequests(t *testing.T) {
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	var calls atomic.Int32
	env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
	base := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("cd", 32)
	h, err := supervisor.NewHandler(listener.Addr().(*net.TCPAddr).Port, func() int { return port }, token, "1.2.3", 17)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := supervisor.Serve(ctx, listener, h)
		if ctx.Err() == nil {
			t.Errorf("public listener stopped before cancellation: %v", err)
		}
		_ = listener.Close()
	}()
	defer func() {
		cancel()
		_ = listener.Close()
		<-done
	}()
	public := listener.Addr().String()
	client := http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	tests := map[string]struct {
		host, path, media string
		origins           []string
		status            int
	}{
		"success: control headers with mixed casing": {public, "/drain", "application/json", nil, http.StatusNoContent},
		"error: repeated Origin on drain":            {public, "/drain", "application/json", []string{"", "null"}, http.StatusForbidden},
		"error: repeated Origin on restart":          {public, "/restart", "application/json", []string{"", "null"}, http.StatusForbidden},
		"error: media type with parameters":          {public, "/drain", "application/json; charset=utf-8", nil, http.StatusForbidden},
		"error: incorrect port":                      {"127.0.0.1:0", "/drain", "application/json", nil, http.StatusForbidden},
		"error: foreign Host":                        {"outside.invalid", "/sample", "", nil, http.StatusForbidden},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+public+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = tc.host
			if tc.media != "" {
				req.Header["cOnTeNt-TyPe"] = []string{tc.media}
				req.Header["x-model-gateway-control-token"] = []string{token}
			}
			if tc.origins != nil {
				req.Header["oRiGiN"] = tc.origins
			}
			before := calls.Load()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			var wantCalls int32
			if tc.status == http.StatusNoContent {
				wantCalls = 1
			}
			if resp.StatusCode != tc.status || len(b) != 0 || calls.Load()-before != wantCalls {
				t.Errorf("status=%d body bytes=%d worker calls=%d, want %d/0/%d", resp.StatusCode, len(b), calls.Load()-before, tc.status, wantCalls)
			}
		})
	}
}

func TestRelayBoundary(t *testing.T) {
	const public = "127.0.0.1:40001"
	const body = "\x00\xffpayload\n"
	const reply = "\xff\x00reply"
	token := strings.Repeat("ab", 32)
	tests := map[string]struct {
		host, path string
		origin     []string
		media      string
		credential string
		status     int
	}{
		"success: preserve bytes headers and query":   {host: public, path: "/sample?q=%2F&q=b", status: 201},
		"success: localhost can relay":                {host: "localhost:40001", path: "/sample", status: 201},
		"success: authenticated drain reaches worker": {host: public, path: "/drain?detail=1", media: "application/json", credential: token, status: 201},
		"error: foreign parsed Host":                  {host: "outside.invalid", path: "http://" + public + "/sample", status: 403},
		"error: missing parsed Host":                  {path: "/sample", status: 403},
		"error: empty Origin":                         {host: public, path: "/sample", origin: []string{""}, status: 403},
		"error: multiple Origin fields":               {host: public, path: "/sample", origin: []string{"", "null"}, status: 403},
		"error: wrong public port":                    {host: "127.0.0.1:40002", path: "/drain", media: "application/json", credential: token, status: 403},
		"error: control requires numeric loopback":    {host: "localhost:40001", path: "/drain", media: "application/json", credential: token, status: 403},
		"error: media type has parameters":            {host: public, path: "/drain", media: "application/json; charset=utf-8", credential: token, status: 403},
		"error: restart requires credential":          {host: public, path: "/restart", media: "application/json", status: 403},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			type observed struct{ Method, URI, Host, Marker, Body, Credential string }
			seen := make(chan observed, 1)
			env := gatewaytest.NewEnv(t.TempDir(), nil, nil, nil)
			base := env.StartUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if calls.Add(1) == 1 {
					seen <- observed{r.Method, r.RequestURI, r.Host, r.Header.Get("X-Fixture"), string(b), r.Header.Get("X-Model-Gateway-Control-Token")}
				}
				w.Header().Set("X-Result", "worker")
				w.WriteHeader(201)
				_, _ = io.WriteString(w, reply)
			}))
			u, err := url.Parse(base)
			if err != nil {
				t.Fatal(err)
			}
			p, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatal(err)
			}
			h, err := supervisor.NewHandler(40001, func() int { return p }, token, "1.2.3", 17)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, strings.NewReader(body))
			req.Host = tc.host
			for _, o := range tc.origin {
				req.Header.Add("Origin", o)
			}
			req.Header.Set("X-Fixture", "marker")
			if tc.media != "" {
				req.Header.Set("content-type", tc.media)
			}
			if tc.credential != "" {
				req.Header.Set("x-model-gateway-control-token", tc.credential)
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, req)
			if out.Code != tc.status {
				t.Fatalf("status %d, want %d", out.Code, tc.status)
			}
			if tc.status == 403 {
				if calls.Load() != 0 || out.Body.Len() != 0 {
					t.Errorf("refusal reached worker %d times, returned %d bytes", calls.Load(), out.Body.Len())
				}
				return
			}
			if calls.Load() != 1 {
				t.Fatalf("worker calls=%d, want one", calls.Load())
			}
			if diff := gocmp.Diff(observed{http.MethodPost, tc.path, u.Host, "marker", body, tc.credential}, <-seen); diff != "" {
				t.Error(diff)
			}
			if diff := gocmp.Diff([]string{"worker", reply}, []string{out.Header().Get("X-Result"), out.Body.String()}); diff != "" {
				t.Error(diff)
			}
		})
	}
}
