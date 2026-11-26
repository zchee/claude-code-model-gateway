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
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/paths"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// deliveryWriter is a client connection whose body writes and flushes can fail independently, and
// which can report the client's departure right after an operation that succeeded.
type deliveryWriter struct {
	mu        sync.Mutex
	header    http.Header
	code      int
	body      bytes.Buffer
	failWrite bool
	failFlush bool
	// afterHeader, afterWrite and afterFlush run once an operation succeeded, with everything
	// written so far.
	afterHeader func()
	afterWrite  func(written string)
	afterFlush  func(written string)
}

func newDeliveryWriter() *deliveryWriter { return &deliveryWriter{header: make(http.Header)} }

func (w *deliveryWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.header
}

func (w *deliveryWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.code == 0 {
		w.code = code
	}
	after := w.afterHeader
	w.mu.Unlock()
	if after != nil {
		after()
	}
}

// declaredLength returns the Content-Length the response head declares, or -1 without one.
func (w *deliveryWriter) declaredLength() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := strconv.Atoi(w.header.Get("Content-Length"))
	if err != nil {
		return -1
	}
	return n
}

func (w *deliveryWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.code == 0 {
		w.code = http.StatusOK
	}
	if w.failWrite {
		w.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	w.body.Write(p)
	written, after := w.body.String(), w.afterWrite
	w.mu.Unlock()
	if after != nil {
		after(written)
	}
	return len(p), nil
}

// FlushError is what http.NewResponseController prefers, so the relays see its failure.
func (w *deliveryWriter) FlushError() error {
	w.mu.Lock()
	if w.failFlush {
		w.mu.Unlock()
		return io.ErrClosedPipe
	}
	written, after := w.body.String(), w.afterFlush
	w.mu.Unlock()
	if after != nil {
		after(written)
	}
	return nil
}

func (w *deliveryWriter) Flush() { _ = w.FlushError() }

func (w *deliveryWriter) snapshot() (int, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.code, w.body.String()
}

// serveDirect runs one request through the composed handler on w, with a request context the
// caller can end, and returns once the handler has returned.
func serveDirect(ctx context.Context, t *testing.T, h *harness, w http.ResponseWriter, body string, header http.Header) {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/messages", strings.NewReader(body))
	for k, vals := range header {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	func() {
		defer func() {
			if p := recover(); p != nil {
				if err, ok := p.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
					panic(p)
				}
			}
		}()
		h.workerHandler.ServeHTTP(w, req)
	}()
}

const (
	proxyStreamDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"
	proxyStreamStop  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

func TestUsageFinishesOnlyForDeliveredResponses(t *testing.T) {
	type writerSetup func(w *deliveryWriter, depart context.CancelFunc)
	tests := map[string]struct {
		stream       bool
		setup        writerSetup
		wantDeclared bool
		wantEmpty    bool
		wantUsage    int
	}{
		"success: delivered buffered reply exports one usage record": {
			setup:     func(*deliveryWriter, context.CancelFunc) {},
			wantUsage: 1,
		},
		"success: delivered proxy stream exports one usage record": {
			stream:    true,
			setup:     func(*deliveryWriter, context.CancelFunc) {},
			wantUsage: 1,
		},
		"error: failed buffered body write with a live client context exports no usage record": {
			setup:     func(w *deliveryWriter, _ context.CancelFunc) { w.failWrite = true },
			wantUsage: 0,
		},
		"error: failed stream write with a live client context exports no usage record": {
			stream:    true,
			setup:     func(w *deliveryWriter, _ context.CancelFunc) { w.failWrite = true },
			wantUsage: 0,
		},
		"error: failed stream flush with a live client context exports no usage record": {
			stream:    true,
			setup:     func(w *deliveryWriter, _ context.CancelFunc) { w.failFlush = true },
			wantUsage: 0,
		},
		// A response with a declared length is complete once exactly that many body bytes were
		// accepted; a client that leaves afterwards does not take the record back.
		"success: buffered reply whose declared length was accepted before the client left exports one usage record": {
			setup: func(w *deliveryWriter, depart context.CancelFunc) {
				w.afterWrite = func(written string) {
					if n := w.declaredLength(); n >= 0 && len(written) >= n {
						depart()
					}
				}
			},
			wantDeclared: true,
			wantUsage:    1,
		},
		"error: buffered reply whose client left before the body write exports no usage record": {
			setup:     func(w *deliveryWriter, depart context.CancelFunc) { w.afterHeader = depart },
			wantEmpty: true,
			wantUsage: 0,
		},
		// A stream declares no length: it is delivered only when its handler returns with the client
		// still there, so a client that leaves inside the last successful flush has no delivered stream.
		"error: stream whose client left inside its last successful flush before the handler returned exports no usage record": {
			stream: true,
			setup: func(w *deliveryWriter, depart context.CancelFunc) {
				w.afterFlush = func(written string) {
					if strings.Contains(written, `"message_stop"`) {
						depart()
					}
				}
			},
			wantUsage: 0,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := harnessConfig{disableTrace: true, heartbeatInterval: -1}
			if tt.stream {
				cfg.proxyHandler = func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, proxyStart+proxyStreamDelta+proxyStreamStop)
				}
			}
			h := newHarness(t, cfg)
			ctx, depart := context.WithTimeout(t.Context(), 10*time.Second)
			defer depart()
			w := newDeliveryWriter()
			tt.setup(w, depart)
			serveDirect(ctx, t, h, w, messageRequestJSON("claude-gpt-6.1-sol[1m]", tt.stream), nil)
			code, body := w.snapshot()
			assertNoSecrets(t, "response body", []byte(body))
			mustDiff(t, "response status", http.StatusOK, code)
			mustDiff(t, "provider contacts", int64(1), h.proxyHits.Load())
			if tt.wantDeclared {
				// Without a declared length this row would test a different case; it must fail loudly.
				mustDiff(t, "declared Content-Length against the accepted body", len(body), w.declaredLength())
			}
			if tt.wantEmpty {
				mustDiff(t, "body bytes after the departure", "", body)
			}
			h.usageEmitter.Wait()
			got := len(decodeOTLPLogs(t, h.usageCollector.snapshot()))
			if got != tt.wantUsage {
				t.Errorf("usage records = %d, want %d (response %d, %d bytes delivered)", got, tt.wantUsage, code, len(body))
			}
		})
	}
}

// departingWriter reports the client's departure when the response head is first prepared.
type departingWriter struct {
	*departureWriter
	once   sync.Once
	depart func()
}

func (w *departingWriter) Header() http.Header {
	w.once.Do(w.depart)
	return w.departureWriter.Header()
}

func TestLocalRefusalToDepartedClient(t *testing.T) {
	const session = "departed-refusal-session"
	tests := map[string]struct {
		model         string
		body          func(model string) string
		arm           bool
		departAtAudit bool
		servedKey     string
		wantServed    float64
		wantAudits    int
	}{
		"error: proxy sentry refusal after the client left during admission writes nothing": {
			model: "claude-gpt-5.6-sol", arm: true, departAtAudit: true, servedKey: "codex", wantServed: 2, wantAudits: 2,
		},
		"error: grok sentry refusal after the client left during admission writes nothing": {
			model: "claude-grok-4.5[1m]", arm: true, departAtAudit: true, servedKey: "grok", wantServed: 2, wantAudits: 2,
		},
		"error: proxy sentry refusal to a client that leaves while its head is prepared writes nothing": {
			model: "claude-gpt-5.6-sol", arm: true, servedKey: "codex", wantServed: 2, wantAudits: 2,
		},
		"error: request assembly refusal to a client that leaves while its head is prepared writes nothing": {
			model: "claude-gpt-6.1-sol[1m]",
			body: func(model string) string {
				return `{"model":"` + model + `","thread":{"type":"continue","id":"th_1"},"messages":[{"role":"user","content":"hi"}]}`
			},
			servedKey: "codex", wantServed: 0, wantAudits: 0,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var atAudit atomic.Pointer[func()]
			h := newHarness(t, harnessConfig{
				sentryEnvTrigger: 100,
				env:              map[string]string{"CODEX_GATEWAY_COMPACT_TRIGGER": "100"},
				proxyHandler: func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{},\"usage\":{\"input_tokens\":30,\"cache_read_input_tokens\":50,\"cache_creation_input_tokens\":21}}\n\n")
					_, _ = io.WriteString(w, proxyStreamStop)
				},
				grokHandler: func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp_fixture","output":[],"usage":{"input_tokens":101}}`)
				},
				observeOptions: func(o *worker.Options) {
					logRoute := o.LogRoute
					o.LogRoute = func(r *http.Request, a outcome.Audit) {
						logRoute(r, a)
						if hook := atAudit.Load(); hook != nil {
							(*hook)()
						}
					}
				},
			})
			body := `{"model":"` + tt.model + `","max_tokens":1,"messages":[{"role":"user","content":"test"}]}`
			if tt.body != nil {
				body = tt.body(tt.model)
			}
			headers := http.Header{"X-Claude-Code-Session-Id": {session}}
			if tt.arm {
				// The first turn reports more than the trigger, so the next turn is refused locally.
				status, _, _ := h.do(http.MethodPost, "/v1/messages", body, headers)
				mustDiff(t, "arming turn status", http.StatusOK, status)
			}
			hitsBefore := h.proxyHits.Load() + h.grokHits.Load()

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			inner := newDepartureWriter()
			depart := sync.OnceFunc(func() { inner.markDeparted(); cancel() })
			var w http.ResponseWriter = inner
			if tt.departAtAudit {
				atAudit.Store(&depart)
			} else {
				w = &departingWriter{departureWriter: inner, depart: depart}
			}
			serveDirect(ctx, t, h, w, body, headers)
			atAudit.Store(nil)

			writes, late, code, got := inner.snapshot()
			if late != 0 || writes != 0 {
				t.Errorf("writes to the departed client: %d operations after departure, %d in total (status %d, %d body bytes)", late, writes, code, len(got))
			}
			mustDiff(t, "provider contacts on the refused turn", hitsBefore, h.proxyHits.Load()+h.grokHits.Load())
			served, _ := h.healthz()["served"].(map[string]any)
			mustDiff(t, "served."+tt.servedKey+" still counts the admission", any(tt.wantServed), served[tt.servedKey])
			h.telemetryRec.Wait()
			mustDiff(t, "route audits still recorded", tt.wantAudits, len(h.readAuditLines()))
		})
	}
}

// lockedBuffer is a diagnostic sink that is safe for concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestUsageFinishPanicLeavesResponseIntact(t *testing.T) {
	tests := map[string]struct {
		requests int
	}{
		"success: one delivered reply whose usage record cannot be derived":        {requests: 1},
		"success: each of two delivered replies reports its own failed derivation": {requests: 2},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			dir := t.TempDir()
			var proxyHits atomic.Int64
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
					_, _ = io.WriteString(w, defaultProxyListing())
					return
				}
				proxyHits.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, `{"id":"msg_panic","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"intact"}],"usage":{"input_tokens":12,"output_tokens":4}}`)
			}))
			defer proxy.Close()
			native := httptest.NewServer(http.NotFoundHandler())
			defer native.Close()
			collector := newOTLPCollector(t, httptest.NewServer)
			lookup := mapLookup(map[string]string{
				"CCP_LMSTUDIO_BASE_URL":            native.URL,
				"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
				"CODEX_GATEWAY_USAGE_ENDPOINT":     collector.srv.URL + "/v1/logs",
			})
			routes, state := &route.Store{}, &sentry.State{}
			mc, err := modelcache.New(modelcache.Options{
				Lookup:           lookup,
				Paths:            paths.New(lookup, func() (string, error) { return dir, nil }, func() (string, error) { return dir, nil }),
				ProxyBaseURL:     proxy.URL,
				AnthropicBaseURL: native.URL,
				DefaultBaseURL:   "http://127.0.0.1:18764",
				Shadow:           true,
				Client:           proxy.Client(),
				Clock:            fixedClock,
				Routes:           routes,
				Sentry:           state,
			})
			if err != nil {
				t.Fatalf("modelcache.New: %v", err)
			}
			if err := mc.Refresh(ctx); err != nil {
				t.Fatalf("modelcache.Refresh: %v", err)
			}
			client := upstream.NewClient(upstream.Options{})
			authTr := upstream.NewTransport(upstream.Options{})
			emitter := usage.NewEmitter(lookup, usage.NewBuilder(usage.Options{Now: func() time.Time { panic("fixture clock refuses to tell the time") }}))
			recorder := telemetry.NewRecorder(telemetry.Options{Lookup: lookup, Now: fixedClock})
			defer func() {
				cancel()
				_ = client.Close()
				authTr.CloseIdleConnections()
				mc.Wait()
				emitter.Wait()
				recorder.Wait()
			}()
			gb, err := compose.NewGrokBranch(compose.GrokOptions{
				Endpoint:        "http://127.0.0.1:9/v1/responses",
				AuthFile:        writeGrokAuthFile(t, dir, "http://127.0.0.1:9", fixedClock().Add(time.Hour)),
				InferenceClient: client,
				AuthTransport:   authTr,
				Sentry:          state,
				Now:             fixedClock,
			})
			if err != nil {
				t.Fatalf("compose.NewGrokBranch: %v", err)
			}
			pr, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: filepath.Join(dir, "dispatch.json"), Lookup: lookup, Now: fixedClock})
			if err != nil {
				t.Fatalf("proxyreq.New: %v", err)
			}
			var diagnostics lockedBuffer
			opts, err := compose.New(compose.Options{
				ServiceContext:   ctx,
				Version:          "fixture-build",
				ControlToken:     fixtureControlToken,
				Lookup:           lookup,
				AnthropicBaseURL: native.URL,
				ProxyBaseURL:     proxy.URL,
				Routes:           routes,
				ModelCache:       mc,
				Sentry:           state,
				Upstream:         client,
				UpstreamState:    &upstreamstate.Store{Dir: filepath.Join(dir, "state"), Now: fixedClock},
				ProxyReq:         pr,
				CompactGuard:     &compactguard.Runner{ServiceContext: ctx, Stderr: io.Discard},
				Grok:             gb,
				Usage:            emitter,
				Telemetry:        recorder,
				Now:              fixedClock,
				Diagnostics:      &diagnostics,
			})
			if err != nil {
				t.Fatalf("compose.New: %v", err)
			}
			handler, err := worker.NewHandler(opts)
			if err != nil {
				t.Fatalf("worker.NewHandler: %v", err)
			}
			want := decodeJSONObject(t, []byte(`{"id":"msg_panic","type":"message","model":"claude-gpt-6.1-sol[1m]","stop_reason":"end_turn","content":[{"type":"text","text":"intact"}],"usage":{"input_tokens":12,"output_tokens":4}}`))
			for i := range tt.requests {
				rec := httptest.NewRecorder()
				reqCtx, done := context.WithTimeout(t.Context(), 10*time.Second)
				handler.ServeHTTP(rec, httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/v1/messages", strings.NewReader(messageRequestJSON("claude-gpt-6.1-sol[1m]", false))))
				done()
				mustDiff(t, "response status", http.StatusOK, rec.Code)
				mustDiff(t, "response body", want, decodeJSONObject(t, rec.Body.Bytes()))
				mustDiff(t, "provider contacts", int64(i+1), proxyHits.Load())
			}
			emitter.Wait()
			lines := strings.Split(strings.TrimSuffix(diagnostics.String(), "\n"), "\n")
			if diagnostics.String() == "" {
				lines = nil
			}
			mustDiff(t, "diagnostic lines", tt.requests, len(lines))
			for _, line := range lines {
				if !strings.HasPrefix(line, "model-gateway: ") || strings.Contains(line, "fixture clock") {
					t.Errorf("diagnostic line %q is not a fixed gateway line", line)
				}
			}
			mustDiff(t, "exported usage records", 0, collector.count())
		})
	}
}
