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

package compose_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// syncCaptureWriter synchronizes concurrent header/body inspection while enforcing no late writes after client cancel.
type syncCaptureWriter struct {
	mu           sync.Mutex
	rec          *httptest.ResponseRecorder
	disconnected atomic.Bool
	lateWrites   atomic.Int64
	firstWrite   chan struct{}
	markFirst    func()
}

func newSyncCaptureWriter() *syncCaptureWriter {
	ch := make(chan struct{})
	return &syncCaptureWriter{
		rec:        httptest.NewRecorder(),
		firstWrite: ch,
		markFirst:  sync.OnceFunc(func() { close(ch) }),
	}
}

func (w *syncCaptureWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rec.Header()
}

func (w *syncCaptureWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disconnected.Load() {
		w.lateWrites.Add(1)
	}
	w.rec.WriteHeader(code)
}

func (w *syncCaptureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disconnected.Load() {
		w.lateWrites.Add(1)
		return 0, io.ErrClosedPipe
	}
	w.markFirst()
	return w.rec.Write(p)
}

func (w *syncCaptureWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rec.Flush()
}

func TestClientDisconnectPreservesUpstreamWork(t *testing.T) {
	tests := map[string]struct {
		kind string
	}{
		"success: client leaves during buffered retry wait":        {kind: "buffered-retry"},
		"success: client leaves during sse relay":                  {kind: "sse-relay"},
		"success: client leaves during guarded compaction attempt": {kind: "guarded-compaction"},
		"success: client leaves during grok inference":             {kind: "grok-inference"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			markEntered := sync.OnceFunc(func() { close(entered) })
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)

			var upstreamCancelled atomic.Bool
			var proxyAttempts atomic.Int32
			var grokAttempts atomic.Int32
			retryDelay := 500 * time.Millisecond
			retries := 2

			cfg := harnessConfig{
				replyRetries:      &retries,
				replyRetryDelay:   &retryDelay,
				heartbeatInterval: -1,
			}

			switch tt.kind {
			case "buffered-retry":
				cfg.proxyHandler = func(w http.ResponseWriter, r *http.Request) {
					n := proxyAttempts.Add(1)
					markEntered()
					if n == 1 {
						select {
						case <-release:
						case <-r.Context().Done():
							upstreamCancelled.Store(true)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"failed to connect to websocket: HTTP 403 Forbidden"}}`)
						return
					}
					w.WriteHeader(http.StatusInternalServerError)
				}
			case "sse-relay":
				cfg.proxyHandler = func(w http.ResponseWriter, r *http.Request) {
					proxyAttempts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_rel\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
					w.(http.Flusher).Flush()
					markEntered()
					select {
					case <-release:
					case <-r.Context().Done():
						upstreamCancelled.Store(true)
						return
					}
					_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
					_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			case "guarded-compaction":
				cfg.proxyHandler = func(w http.ResponseWriter, r *http.Request) {
					n := proxyAttempts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_g\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
					w.(http.Flusher).Flush()
					markEntered()
					if n > 1 {
						return
					}
					select {
					case <-release:
					case <-r.Context().Done():
						upstreamCancelled.Store(true)
						return
					}
				}
			case "grok-inference":
				cfg.grokHandler = func(w http.ResponseWriter, r *http.Request) {
					grokAttempts.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_gd\",\"model\":\"grok-4.7\"}}\n\n")
					w.(http.Flusher).Flush()
					markEntered()
					select {
					case <-release:
					case <-r.Context().Done():
						upstreamCancelled.Store(true)
						return
					}
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_gd\",\"status\":\"completed\",\"usage\":{\"input_tokens\":11,\"output_tokens\":4}}}\n\n")
				}
			}

			h := newHarness(t, cfg)

			var reqBody string
			switch tt.kind {
			case "buffered-retry":
				reqBody = messageRequestJSON("claude-gpt-6.1-sol[1m]", false)
			case "sse-relay":
				reqBody = messageRequestJSON("claude-gpt-6.1-sol[1m]", true)
			case "guarded-compaction":
				reqBody = compactRequestJSON("claude-gpt-6.1-sol[1m]")
			case "grok-inference":
				reqBody = messageRequestJSON("claude-grok-4.7[1m]", true)
			}

			clientCtx, cancelClient := context.WithCancel(t.Context())
			defer cancelClient()

			w := newSyncCaptureWriter()
			req := httptest.NewRequestWithContext(clientCtx, http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
			req.RequestURI = "/v1/messages"

			done := make(chan struct{})
			go func() {
				defer close(done)
				h.workerHandler.ServeHTTP(w, req)
			}()
			t.Cleanup(func() {
				cancelClient()
				unblock()
				h.cancelService()
				_ = h.upstreamClient.Close()
				timer := time.NewTimer(3 * time.Second)
				defer timer.Stop()
				select {
				case <-done:
				case <-timer.C:
					t.Error("ServeHTTP did not exit during cleanup")
				}
			})

			waitSignal(t, "provider entry", entered, 3*time.Second)
			if tt.kind == "sse-relay" || tt.kind == "grok-inference" {
				waitSignal(t, "first streamed write to client", w.firstWrite, 3*time.Second)
			}

			// Client leaves while upstream work is in flight.
			w.disconnected.Store(true)
			cancelClient()

			// Release the provider so the in-flight attempt completes normally on its own.
			unblock()
			waitSignal(t, "ServeHTTP return after provider finished", done, 3*time.Second)

			if upstreamCancelled.Load() {
				t.Error("client departure cancelled in-flight upstream work; upstream must use ServiceContext")
			}
			if got := w.lateWrites.Load(); got != 0 {
				t.Errorf("writes to client after departure = %d, want 0", got)
			}

			switch tt.kind {
			case "buffered-retry", "guarded-compaction":
				mustDiff(t, "no retry started after client departure", int32(1), proxyAttempts.Load())
			case "grok-inference":
				mustDiff(t, "grok attempts", int32(1), grokAttempts.Load())
			}

			// Root decides Cancelled from client context -> zero route spans exported (no synthetic 499) and no Codex state mutation.
			h.telemetryRec.Wait()
			h.usageEmitter.Wait()
			mustDiff(t, "cancelled client must not emit route spans", 0, len(decodeOTLPSpans(t, h.traceCollector.snapshot())))
			if h.upstreamState.ReadUnavailable() != nil {
				t.Error("cancelled client must not mutate Codex upstream unavailable state")
			}
		})
	}
}

func TestRetryWaitAndHeartbeatVirtualClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serviceCtx, cancelService := context.WithCancel(t.Context())
		defer cancelService()

		proxyLn := newPipeListener()
		defer func() { _ = proxyLn.Close() }()

		var attempts atomic.Int32
		proxySrv := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, defaultProxyListing())
					return
				}
				n := attempts.Add(1)
				if n == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"failed to connect to websocket: HTTP 403 Forbidden"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_vt","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":2}}`)
			}),
		}
		srvDone := make(chan struct{})
		go func() {
			defer close(srvDone)
			_ = proxySrv.Serve(proxyLn)
		}()
		defer func() {
			_ = proxySrv.Close()
			<-srvDone
		}()

		dir := t.TempDir()
		env := map[string]string{
			"CODEX_GATEWAY_USAGE_ENDPOINT":     "0",
			"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "",
		}
		lookup := mapLookup(env)
		upOpts := upstream.Options{DialContext: proxyLn.DialContext}
		upClient := upstream.NewClient(upOpts)
		defer func() { _ = upClient.Close() }()

		authTr := upstream.NewTransport(upOpts)
		if authTr == nil {
			t.Fatal("upstream.NewTransport returned nil (not implemented)")
		}
		defer authTr.CloseIdleConnections()

		routeStore := &route.Store{}
		sentryState := &sentry.State{}
		grokBranch, err := compose.NewGrokBranch(compose.GrokOptions{
			Endpoint:        "http://127.0.0.1:1/v1/responses",
			AuthFile:        writeGrokAuthFile(t, dir, "http://127.0.0.1:1", fixedTime.Add(time.Hour)),
			InferenceClient: upClient,
			AuthTransport:   authTr,
			Sentry:          sentryState,
			SentryEnabled:   true,
			Now:             fixedClock,
		})
		if err != nil {
			t.Fatalf("compose.NewGrokBranch: %v", err)
		}

		mcClient := &http.Client{Transport: &http.Transport{DialContext: proxyLn.DialContext}}
		mc, err := modelcache.New(modelcache.Options{
			Lookup:           lookup,
			Paths:            paths.New(lookup, func() (string, error) { return dir, nil }, func() (string, error) { return dir, nil }),
			ProxyBaseURL:     "http://127.0.0.1:18765",
			AnthropicBaseURL: "http://127.0.0.1:18766",
			DefaultBaseURL:   "http://127.0.0.1:18764",
			Shadow:           true,
			Client:           mcClient,
			Clock:            fixedClock,
			Routes:           routeStore,
			Sentry:           sentryState,
		})
		if err != nil {
			t.Fatalf("modelcache.New: %v", err)
		}
		defer mc.Wait()
		if err := mc.Refresh(serviceCtx); err != nil {
			t.Fatalf("modelcache.Refresh: %v", err)
		}

		pr, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: dir + "/dispatch.json", Lookup: lookup, Now: fixedClock})
		if err != nil {
			t.Fatalf("proxyreq.New: %v", err)
		}
		retries := 2
		retryDelay := 250 * time.Millisecond
		workerOpts, err := compose.New(compose.Options{
			ServiceContext:   serviceCtx,
			Version:          "fixture-build",
			ControlToken:     fixtureControlToken,
			Lookup:           lookup,
			AnthropicBaseURL: "http://127.0.0.1:18766",
			ProxyBaseURL:     "http://127.0.0.1:18765",
			Routes:           routeStore,
			ModelCache:       mc,
			Sentry:           sentryState,
			Upstream:         upClient,
			UpstreamState:    &upstreamstate.Store{Dir: dir + "/state", Now: fixedClock},
			ProxyReq:         pr,
			CompactGuard:     &compactguard.Runner{ServiceContext: serviceCtx, Settings: compactguard.Settings{StreamGuard: true, Retries: 2, MaxBufferedBytes: 1 << 20}},
			Grok:             grokBranch,
			Usage:            usage.NewEmitter(lookup, usage.NewBuilder(usage.Options{Now: fixedClock})),
			Telemetry:        telemetry.NewRecorder(telemetry.Options{Lookup: lookup, Now: fixedClock}),
			Now:              fixedClock,
			ReplyRetries:     &retries,
			ReplyRetryDelay:  &retryDelay,
		})
		if err != nil {
			t.Fatalf("compose.New: %v", err)
		}
		wh, err := worker.NewHandler(workerOpts)
		if err != nil {
			t.Fatalf("worker.NewHandler: %v", err)
		}

		// Cancel client context while reply.Handle is sleeping on the 250ms retry delay in virtual time.
		clientCtx, cancelClient := context.WithCancel(t.Context())
		defer cancelClient()
		w := newSyncCaptureWriter()
		req := httptest.NewRequestWithContext(clientCtx, http.MethodPost, "/v1/messages", strings.NewReader(messageRequestJSON("claude-gpt-6.1-sol[1m]", false)))
		req.RequestURI = "/v1/messages"

		done := make(chan struct{})
		go func() {
			defer close(done)
			wh.ServeHTTP(w, req)
		}()

		synctest.Wait()
		mustDiff(t, "attempt 1 completed before retry delay elapsed", int32(1), attempts.Load())

		w.disconnected.Store(true)
		cancelClient()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("ServeHTTP did not exit promptly after client cancellation during retry wait")
		}
		mustDiff(t, "retry did not fire after client departure during retry wait", int32(1), attempts.Load())
		mustDiff(t, "no writes after client departure", int64(0), w.lateWrites.Load())
	})
}

func TestShutdownCancelsInFlightWork(t *testing.T) {
	tests := map[string]struct {
		kind             string
		disconnectClient bool
	}{
		"success: default upstream passthrough ends on service shutdown":              {kind: "anthropic"},
		"success: disconnected default upstream passthrough ends on service shutdown": {kind: "anthropic", disconnectClient: true},
		"success: proxy streaming relay ends on service shutdown":                     {kind: "proxy-stream"},
		"success: compaction-guarded stream ends on service shutdown":                 {kind: "compaction-guard"},
		"success: grok streaming inference ends on service shutdown":                  {kind: "grok-stream"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			markEntered := sync.OnceFunc(func() { close(entered) })
			providerDone := make(chan struct{})
			markProviderDone := sync.OnceFunc(func() { close(providerDone) })

			holdStream := func(firstFrame string) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					defer markProviderDone()
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, firstFrame)
					w.(http.Flusher).Flush()
					markEntered()
					<-r.Context().Done()
				}
			}

			cfg := harnessConfig{heartbeatInterval: -1}
			var body string
			switch tt.kind {
			case "anthropic":
				cfg.anthropicHandler = holdStream("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"type\":\"message\",\"model\":\"claude-sonnet-4-6\"}}\n\n")
				body = messageRequestJSON("claude-sonnet-4-6", true)
			case "proxy-stream":
				cfg.proxyHandler = holdStream("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_p\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\"}}\n\n")
				body = messageRequestJSON("claude-gpt-6.1-sol[1m]", true)
			case "compaction-guard":
				cfg.proxyHandler = holdStream("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_c\",\"type\":\"message\",\"model\":\"gpt-6.1-sol\"}}\n\n")
				body = compactRequestJSON("claude-gpt-6.1-sol[1m]")
			case "grok-stream":
				cfg.grokHandler = holdStream("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_s\",\"model\":\"grok-4.7\"}}\n\n")
				body = messageRequestJSON("claude-grok-4.7[1m]", true)
			}

			h := newHarness(t, cfg)

			clientCtx, cancelClient := context.WithCancel(t.Context())
			defer cancelClient()
			w := newSyncCaptureWriter()
			req := httptest.NewRequestWithContext(clientCtx, http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.RequestURI = "/v1/messages"

			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					// Recover expected http.ErrAbortHandler if a committed stream aborts at the net/http boundary.
					if r := recover(); r != nil {
						if err, ok := r.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
							panic(r)
						}
					}
				}()
				h.workerHandler.ServeHTTP(w, req)
			}()
			t.Cleanup(func() {
				cancelClient()
				h.cancelService()
				_ = h.upstreamClient.Close()
				timer := time.NewTimer(3 * time.Second)
				defer timer.Stop()
				select {
				case <-done:
				case <-timer.C:
					t.Error("ServeHTTP did not return during cleanup")
				}
			})

			waitSignal(t, "upstream stream entry", entered, 3*time.Second)

			if tt.disconnectClient {
				waitSignal(t, "first frame written before client disconnect", w.firstWrite, 3*time.Second)
				w.disconnected.Store(true)
				cancelClient()
				// On disconnected Anthropic passthrough, back-pressure keeps the provider open until service shutdown.
				select {
				case <-providerDone:
					t.Fatal("client disconnect eagerly closed default upstream provider before service shutdown")
				default:
				}
			}

			// Ending ServiceContext must cancel in-flight upstream work and make the handler return promptly.
			h.cancelService()
			waitSignal(t, "provider cancellation on service shutdown", providerDone, 3*time.Second)
			waitSignal(t, "handler return on service shutdown", done, 3*time.Second)

			if tt.disconnectClient && w.lateWrites.Load() != 0 {
				t.Errorf("writes after client departure = %d, want 0", w.lateWrites.Load())
			}
		})
	}
}
