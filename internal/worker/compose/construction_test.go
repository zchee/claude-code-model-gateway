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
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/telemetry"
	"github.com/zchee/claude-code-model-gateway/internal/upstreamstate"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/modelcache"
	"github.com/zchee/claude-code-model-gateway/internal/worker/proxyreq"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// minimalOptions supplies every collaborator compose.New requires, with exports disabled.
func minimalOptions(t *testing.T, service context.Context, client *upstream.Client) compose.Options {
	t.Helper()
	dir := t.TempDir()
	lookup := mapLookup(map[string]string{
		"CODEX_GATEWAY_USAGE_ENDPOINT":     "0",
		"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "0",
	})
	pr, err := proxyreq.New(proxyreq.Options{StartupDir: dir, DispatchPath: filepath.Join(dir, "dispatch.json"), Lookup: lookup, Now: fixedClock})
	if err != nil {
		t.Fatalf("proxyreq.New: %v", err)
	}
	gb, err := grok.NewBranch(grok.BranchOptions{Client: client, Now: fixedClock})
	if err != nil {
		t.Fatalf("grok.NewBranch: %v", err)
	}
	return compose.Options{
		ServiceContext:   service,
		Version:          "fixture-build",
		ControlToken:     fixtureControlToken,
		Lookup:           lookup,
		AnthropicBaseURL: "http://127.0.0.1:9",
		ProxyBaseURL:     "http://127.0.0.1:9",
		Routes:           &route.Store{},
		ModelCache:       &modelcache.Cache{},
		Sentry:           &sentry.State{},
		Upstream:         client,
		UpstreamState:    &upstreamstate.Store{Dir: filepath.Join(dir, "state"), Now: fixedClock},
		ProxyReq:         pr,
		CompactGuard:     &compactguard.Runner{ServiceContext: service},
		Grok:             gb,
		Usage:            usage.NewEmitter(lookup, usage.NewBuilder(usage.Options{Now: fixedClock})),
		Telemetry:        telemetry.NewRecorder(telemetry.Options{Lookup: lookup, Now: fixedClock}),
		Now:              fixedClock,
	}
}

func TestConstructionLeavesUpstreamToItsOwner(t *testing.T) {
	tests := map[string]struct {
		cancelBefore bool
	}{
		"success: an already-cancelled service context at construction schedules no work": {cancelBefore: true},
		"success: a service context that ends after construction leaves the client open":  {},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, cancel := context.WithCancel(t.Context())
				client := upstream.NewClient(upstream.Options{})
				defer func() { _ = client.Close() }()
				if tt.cancelBefore {
					cancel()
				}
				if _, err := compose.New(minimalOptions(t, service, client)); err != nil {
					t.Fatalf("compose.New: %v", err)
				}
				cancel()
				// Anything the constructor left behind runs to completion or blocks here.
				synctest.Wait()

				// An ended request context makes an open client answer with that context's error
				// before any dial; a closed client answers with its own closed error instead.
				ended, stop := context.WithCancel(t.Context())
				stop()
				resp, err := client.Do(ended, upstream.Request{
					Target:        upstream.TargetAnthropic,
					BaseURL:       "http://127.0.0.1:9",
					Method:        http.MethodPost,
					RequestURI:    "/v1/messages",
					ClientHeaders: http.Header{},
					Body:          []byte(`{}`),
				})
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("upstream client after construction: got %v, want the request's own context.Canceled (the client must stay open for its owner)", err)
				}
			})
		})
	}
}

const (
	nativeStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_native\",\"type\":\"message\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":10}}}\n\n"
	nativeDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"
	nativeStop  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	// beyond32Bits is above every 32-bit count and below 2^53, so it is an exact JavaScript number.
	beyond32Bits = "1000000000000000"
)

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, s); err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip fixture: %v", err)
	}
	return buf.Bytes()
}

// TestCountSettingsBeyond32Bits guards the conversion of count and byte settings above 32 bits with
// small bodies: such a setting must be honoured as a large bound, never wrap or disable the feature.
// The saturation point of the conversion itself is tested directly in clamp_internal_test.go.
func TestCountSettingsBeyond32Bits(t *testing.T) {
	encoded := gzipped(t, nativeStart+nativeDelta+nativeStop)
	var attempts atomic.Int32
	tests := map[string]struct {
		cfg   harnessConfig
		check func(t *testing.T, h *harness)
	}{
		"success: an observation ceiling beyond 32 bits still observes a compressed native stream": {
			cfg: harnessConfig{
				disableTrace: true,
				env:          map[string]string{"CODEX_GATEWAY_USAGE_MAX_RESPONSE_BYTES": beyond32Bits},
				anthropicHandler: func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Content-Encoding", "gzip")
					w.Header().Set("Request-Id", "req-native-large-bound")
					_, _ = w.Write(encoded)
				},
			},
			check: func(t *testing.T, h *harness) {
				// The handler is run to its end here: a client can read a sized body before the handler returns.
				w := newDeliveryWriter()
				serveDirect(t.Context(), t, h, w, messageRequestJSON("claude-sonnet-4-6", true), http.Header{"Accept-Encoding": {"gzip"}})
				status, _ := w.snapshot()
				mustDiff(t, "native status", http.StatusOK, status)
				h.usageEmitter.Wait()
				logs := decodeOTLPLogs(t, h.usageCollector.snapshot())
				mustDiff(t, "usage records", 1, len(logs))
				mustDiff(t, "observed input tokens", int64(10), logs[0].Attributes["input_tokens"])
				mustDiff(t, "observed output tokens", int64(3), logs[0].Attributes["output_tokens"])
			},
		},
		"success: a retry count beyond 32 bits still retries rejected upgrades": {
			cfg: harnessConfig{
				disableTrace: true,
				disableUsage: true,
				env: map[string]string{
					"CODEX_GATEWAY_WS_UPGRADE_RETRIES":        beyond32Bits,
					"CODEX_GATEWAY_WS_UPGRADE_RETRY_DELAY_MS": "0",
				},
				proxyHandler: func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					if attempts.Add(1) <= 3 {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, upgradeRejection)
						return
					}
					_, _ = io.WriteString(w, `{"id":"msg_retry","type":"message","model":"gpt-6.1-sol","stop_reason":"end_turn","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
				},
			},
			check: func(t *testing.T, h *harness) {
				status, _, _ := h.do(http.MethodPost, "/v1/messages", messageRequestJSON("claude-gpt-6.1-sol[1m]", false), nil)
				mustDiff(t, "status after three rejected upgrades", http.StatusOK, status)
				mustDiff(t, "upstream attempts", int32(4), attempts.Load())
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			tt.check(t, newHarness(t, tt.cfg))
		})
	}
}
