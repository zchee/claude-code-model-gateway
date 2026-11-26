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

package compactguard_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

func TestSentryNativeUnbuffered(t *testing.T)      { checkLive(t, "", true) }
func TestGuardDisabledStreamsLive(t *testing.T)    { checkLive(t, "codex", false) }
func TestGuardBufferOverflowDegrades(t *testing.T) { checkLive(t, "codex", true) }

// The provider cannot end its response until the client has read the prefix.
func checkLive(t *testing.T, provider string, enabled bool) {
	t.Helper()
	service, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	releaseCh := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseCh) })
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frameMessageStart)
		w.(http.Flusher).Flush()
		select {
		case <-releaseCh:
		case <-r.Context().Done():
			return
		case <-service.Done():
			return
		}
		_, _ = io.WriteString(w, frameMessageStop)
	}))
	defer srv.Close()
	client := upstream.NewClient(upstream.Options{})
	defer func() { _ = client.Close() }()
	runner := guardRunner(service, 2)
	runner.Settings.StreamGuard = enabled
	runner.Settings.MaxBufferedBytes = 1
	done := make(chan error, 1)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := guardRequest()
		req.ProxyProvider = provider
		req.Stream.Provider = provider
		req.UpstreamOrigin = srv.URL
		target := upstream.TargetProxy
		if provider == "" {
			target = upstream.TargetAnthropic
			req.Body = compactRequestBody("claude-native", "Summarize.")
		}
		send := func() (*upstream.Response, error) {
			return client.Do(service, upstream.Request{Target: target, BaseURL: srv.URL, RequestURI: req.PathOnly, Method: http.MethodPost, Body: req.Body})
		}
		var err error
		if compactguard.ShouldGuard(runner.Settings, provider, req.PathOnly, req.Body) {
			req.Next = func(context.Context, bool) (*upstream.Response, outcome.Result, error) {
				resp, err := send()
				return resp, outcome.Result{Contacted: true}, err
			}
			_, err = runner.Serve(r.Context(), w, req)
		} else {
			var resp *upstream.Response
			resp, err = send()
			if err == nil {
				if provider == "" {
					_, err = sse.RelayPassthrough(service, w, resp, sse.PassthroughOptions{})
				} else {
					_, err = sse.RelayProxy(service, w, resp, req.Stream)
				}
			}
		}
		done <- err
		if err != nil && !errors.Is(err, http.ErrAbortHandler) {
			http.Error(w, "relay failed", http.StatusBadGateway)
		}
	}))
	defer gw.Close()
	defer release()
	req, err := http.NewRequestWithContext(service, http.MethodPost, gw.URL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("first response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d relay error=%v", resp.StatusCode, <-done)
	}
	prefix := advertisedStart
	if provider == "" {
		prefix = frameMessageStart
	}
	got := make([]byte, len(prefix))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatalf("first frame not visible while provider withheld terminal: %v", err)
	}
	if diff := gocmp.Diff(prefix, string(got)); diff != "" {
		t.Fatal(diff)
	}
	release()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if diff := gocmp.Diff(frameMessageStop, string(rest)); diff != "" {
		t.Error(diff)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-service.Done():
		t.Fatal(service.Err())
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("provider attempts=%d", n)
	}
}

func TestGuardClientDisconnectStopsRetry(t *testing.T) {
	service, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	client, disconnect := context.WithCancel(t.Context())
	defer disconnect()
	releaseCh := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseCh) })
	var sends atomic.Int32
	waiting := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sends.Add(1) > 1 {
			http.Error(w, "unexpected retry", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, truncatedStream())
		w.(http.Flusher).Flush()
		close(waiting)
		select {
		case <-releaseCh:
		case <-service.Done():
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer release()
	req := guardRequest()
	bindLoopback(t, service, srv.URL, &req, nil)
	done := make(chan error, 1)
	go func() { _, err := guardRunner(service, 2).Serve(client, httptest.NewRecorder(), req); done <- err }()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("Serve ended before provider streamed: %v", err)
	case <-service.Done():
		t.Fatal("provider did not stream")
	}
	disconnect()
	release()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	case <-service.Done():
		t.Fatal("Serve did not return after provider ended")
	}
	if sends.Load() != 1 {
		t.Errorf("unexpected retry after disconnect: %d", sends.Load())
	}
}
