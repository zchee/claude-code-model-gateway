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

package telemetry

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"testing"
	"testing/synctest"

	gocmp "github.com/google/go-cmp/cmp"
	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/gatewaytest"
)

// spanGateReply is the collector's answer to every post.
const spanGateReply = "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"

// spanGateRecorder returns a recorder that exports spans through g, with entropy for spans spans.
func spanGateRecorder(t *testing.T, g *gatewaytest.OrderGate, spans int) *Recorder {
	t.Helper()
	opts := recorderOptions(map[string]string{"CODEX_GATEWAY_TELEMETRY_ENDPOINT": "http://127.0.0.1:1/v1/traces", "CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1"})
	// Each unlinked span reads a trace id, a span id and a route id: 40 bytes.
	opts.Random = bytes.NewReader(bytes.Repeat([]byte{0x11}, 40*spans))
	r := newRecorder(opts, func(ctx context.Context, network, addr string) (net.Conn, error) { return g.Dial(ctx, network, addr) })
	if !r.Enabled() {
		t.Fatal("test precondition: the recorder does not export spans")
	}
	return r
}

// spanOrderKey names a posted span by its selected_model attribute. It returns an error instead of
// failing the test, so that the collector can still answer every post.
func spanOrderKey(payload []byte) (string, error) {
	var envelope struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					Attributes []struct {
						Key   string         `json:"key"`
						Value map[string]any `json:"value"`
					} `json:"attributes"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", fmt.Errorf("decode post %s: %w", payload, err)
	}
	if len(envelope.ResourceSpans) != 1 || len(envelope.ResourceSpans[0].ScopeSpans) != 1 || len(envelope.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		return "", fmt.Errorf("post %s does not hold exactly one span", payload)
	}
	for _, attr := range envelope.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes {
		if attr.Key == "selected_model" {
			return fmt.Sprint(attr.Value["stringValue"]), nil
		}
	}
	return "", fmt.Errorf("post %s has no selected_model attribute", payload)
}

// TestTraceSubmissionOrder checks that the route spans the recorder is given reach the collector in
// the order they finished, as in the source: a request's span is posted when its route finishes
// (lib/request-worker.js:1106), and each post starts at once in that call (:984-1008), so spans
// leave in finish order, not start order. The collector (gatewaytest.OrderGate) parks every post until the
// bubble is quiet and records posts that were outstanding together in reverse, so the test does not depend on how
// the goroutines happen to be scheduled.
func TestTraceSubmissionOrder(t *testing.T) {
	tests := map[string]struct {
		// started lists the selected models of the spans in the order they start.
		started []string
		// finished lists indexes into started in the order the spans finish.
		finished []int
		// want is every span key in submission (finish) order.
		want []string
	}{
		"success: spans finished one after another arrive in that order": {
			started:  []string{"model-a", "model-b", "model-c"},
			finished: []int{0, 1, 2},
			want:     []string{"model-a", "model-b", "model-c"},
		},
		"success: spans arrive in the order they finished, not the order they started": {
			started:  []string{"model-a", "model-b", "model-c"},
			finished: []int{2, 0, 1},
			want:     []string{"model-c", "model-a", "model-b"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				g := gatewaytest.NewOrderGate(tt.want, spanOrderKey, spanGateReply)
				r := spanGateRecorder(t, g, len(tt.started))
				spans := make([]*Span, len(tt.started))
				for i := range tt.started {
					spans[i] = r.Start(nil)
				}
				// Finish queues each span without waiting for the collector; TestTraceExportFailure
				// already requires that it returns while the collector never replies.
				for _, index := range tt.finished {
					spans[index].Finish(Route{Backend: "codex", SelectedModel: tt.started[index]}, http.StatusOK, "")
				}
				waited := make(chan struct{})
				go func() {
					defer close(waited)
					r.Wait()
				}()
				g.Serve(t, waited)
				g.Check(t, tt.want)
			})
		})
	}
}

// TestTraceHandOff decides when the recorder moves from one span to the next: it starts the next
// post once the previous request is wholly on its connection, not when only its headers are
// written and not after waiting for the collector's reply, as the source starts each post without
// waiting for an earlier one's reply (lib/request-worker.js:984-1008). Two spans are finished; the
// collector holds back the first post's last body byte and then its reply.
func TestTraceHandOff(t *testing.T) {
	tests := map[string]struct {
		models []string
	}{
		"success: the second span is posted once the first is written and before its reply": {
			models: []string{"model-first", "model-second"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			synctest.Test(t, func(t *testing.T) {
				g := gatewaytest.NewOrderGate(tt.models, spanOrderKey, spanGateReply)
				r := spanGateRecorder(t, g, len(tt.models))
				for _, model := range tt.models {
					r.Start(nil).Finish(Route{Backend: "codex", SelectedModel: model}, http.StatusOK, "")
				}
				findings := g.HandOff()
				waited := make(chan struct{})
				go func() {
					defer close(waited)
					r.Wait()
				}()
				g.Serve(t, waited)
				if d := gocmp.Diff([]string(nil), findings); d != "" {
					t.Errorf("how the recorder hands one post over to the next (-want +got):\n%s", d)
				}
				g.Check(t, tt.models)
			})
		})
	}
}
