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

package compactguard_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

var _ compactguard.UsageCapture = (*usage.Capture)(nil)

const (
	advertised        = "claude-gpt-5.6-sol"
	backend           = "gpt-5.6-sol"
	frameMessageStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"gpt-5.6-sol\"}}\n\n"
	advertisedStart   = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-gpt-5.6-sol\"}}\n\n"
	frameBlockStart   = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"
	frameBlockStop    = "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"
	frameMessageDelta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"
	frameMessageStop  = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

func frameTextDelta(text string) string {
	return fmt.Sprintf("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
}

func completeStream(text string) string {
	return frameMessageStart + frameBlockStart + frameTextDelta(text) + frameBlockStop + frameMessageDelta + frameMessageStop
}

func truncatedStream() string {
	return frameMessageStart + frameBlockStart + frameTextDelta("discard me")
}

func delivered(stream string) string {
	return strings.Replace(stream, frameMessageStart, advertisedStart, 1)
}

func compactRequestBody(model, text string) []byte {
	return fmt.Appendf(nil, `{"model":%q,"stream":true,"system":"You are a helpful AI assistant tasked with summarizing conversations.","messages":[{"role":"user","content":%q}]}`, model, text)
}

func guardRequest() compactguard.Request {
	return compactguard.Request{ProxyProvider: "codex", PathOnly: "/v1/messages", Body: compactRequestBody(backend, "Summarize."), Stream: sse.ProxyOptions{Provider: "codex", BackendModel: backend, AdvertisedModel: advertised, IsCompaction: true}}
}

func guardRunner(service context.Context, retries float64) compactguard.Runner {
	return compactguard.Runner{ServiceContext: service, Settings: compactguard.Settings{StreamGuard: true, Retries: retries, MaxBufferedBytes: 16 << 20}, Stderr: io.Discard}
}

func sseResponse(body io.ReadCloser) *upstream.Response {
	h := http.Header{"Content-Type": {"text/event-stream"}}
	return &upstream.Response{StatusCode: 200, Header: h, RelayHeader: h.Clone(), Body: body}
}

type countedBody struct {
	io.ReadCloser
	closes atomic.Int32
}

func (b *countedBody) Close() error { b.closes.Add(1); return b.ReadCloser.Close() }

type headRecorder struct {
	rec   *httptest.ResponseRecorder
	heads []int
}

var (
	_ http.ResponseWriter = (*headRecorder)(nil)
	_ http.Flusher        = (*headRecorder)(nil)
	_ io.StringWriter     = (*headRecorder)(nil)
)

func (r *headRecorder) Header() http.Header { return r.rec.Header() }

func (r *headRecorder) WriteHeader(code int) {
	r.heads = append(r.heads, code)
	r.rec.WriteHeader(code)
}

func (r *headRecorder) Write(b []byte) (int, error) {
	if len(r.heads) == 0 {
		r.heads = append(r.heads, http.StatusOK)
	}
	return r.rec.Write(b)
}

func (r *headRecorder) WriteString(s string) (int, error) {
	if len(r.heads) == 0 {
		r.heads = append(r.heads, http.StatusOK)
	}
	return r.rec.WriteString(s)
}

func (r *headRecorder) Flush() {
	if len(r.heads) == 0 {
		r.heads = append(r.heads, http.StatusOK)
	}
	r.rec.Flush()
}

func (r *headRecorder) bodyString() string { return r.rec.Body.String() }

func TestGuardSettledVerdict(t *testing.T) {
	tests := map[string]struct {
		retry  bool
		status int
		err    error
	}{
		"success: initial buffered verdict is unchanged":  {false, 413, nil},
		"error: committed rejection keeps buffered facts": {true, 401, nil},
		"error: transport abort keeps terminal error":     {true, 502, http.ErrAbortHandler},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := guardRequest()
			w := &headRecorder{rec: httptest.NewRecorder()}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cap := usage.NewCapture(usage.NewBuilder(usage.Options{Now: time.Now}), usage.Request{Backend: "codex"}, 0, nil)
			req.Usage = cap
			makeVerdict := func() outcome.Result {
				v := outcome.Result{Status: tt.status, Contacted: true, Route: outcome.Route{Backend: "codex", Via: "proxy", Overflow: &outcome.Overflow{Phase: "upstream"}}}
				if tt.err != nil {
					v.StatusOverride = "upstream_error"
				}
				if tt.retry {
					v.Route.Compaction = &outcome.Compaction{Outcome: "upstream_error", ErrorCode: "upstream_http_error", UpstreamStatus: new(tt.status)}
				}
				return v
			}
			want := makeVerdict()
			var heads []int
			wantCalls := 1
			if tt.retry {
				wantCalls++
				heads = []int{200}
				want.Route.Compaction.InputTokens = new(19.0)
			}
			var firstBody *countedBody
			calls := 0
			req.Next = func(c context.Context, head bool) (*upstream.Response, outcome.Result, error) {
				if c.Done() != ctx.Done() {
					t.Error("Next did not receive client liveness")
				}
				if calls > 1 || head != (calls == 1) || head != (len(w.heads) > 0) {
					t.Errorf("Next call %d head=%v, writes=%v", calls, head, w.heads)
				}
				calls++
				if tt.retry && calls == 1 {
					firstBody = &countedBody{ReadCloser: io.NopCloser(strings.NewReader("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":19}}}\n\n"))}
					return sseResponse(firstBody), outcome.Result{Contacted: true}, nil
				}
				return nil, makeVerdict(), tt.err
			}
			got, err := guardRunner(t.Context(), 1).Serve(ctx, w, req)
			if !errors.Is(err, tt.err) {
				t.Fatalf("Serve: %v, want %v", err, tt.err)
			}
			if tt.retry {
				if firstBody == nil {
					t.Error("attempt 1 body was not created")
				} else if n := firstBody.closes.Load(); n != 1 {
					t.Errorf("attempt 1 body closed %d times, want 1", n)
				}
			}
			if calls != wantCalls {
				t.Errorf("Next called %d times, want %d", calls, wantCalls)
			}
			if diff := gocmp.Diff(want, got); diff != "" {
				t.Error(diff)
			}
			if diff := gocmp.Diff(heads, w.heads); diff != "" {
				t.Error(diff)
			}
			if body := w.bodyString(); body != "" {
				t.Errorf("guard wrote a second response: %q", body)
			}
		})
	}
}
