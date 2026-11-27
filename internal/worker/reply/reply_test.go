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

package reply_test

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

var _ reply.UsageCapture = (*usage.Capture)(nil)

func newCapture() *usage.Capture {
	return usage.NewCapture(usage.NewBuilder(usage.Options{Now: time.Now}), usage.Request{}, 0, nil)
}

func checkCapture(t *testing.T, c *usage.Capture, status, size int, fields map[string]any) {
	t.Helper()
	// A success without token measurements would not produce an inspectable record.
	// Supply one measurement without adding bytes or changing the response identity.
	event, err := jsonx.DecodeWire([]byte(`{"type":"message_delta","usage":{"cache_creation_input_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	c.ObserveEvent(event)
	r := c.Finish()
	if r == nil {
		t.Fatal("usage record missing")
	}
	equal[any](t, status, r.Attributes["status_code"])
	equal[any](t, int64(size), r.Attributes["response_body_bytes"])
	for key, want := range fields {
		if diff := gocmp.Diff(want, r.Attributes[key]); diff != "" {
			t.Errorf("%s (-want +got): %s", key, diff)
		}
	}
}

func checkIncomplete(t *testing.T, c *usage.Capture) {
	t.Helper()
	// Incompleteness suppresses success records, not limit signals. Give this capture
	// otherwise exportable inputs without touching its completeness flag.
	c.SetResponse(200, nil)
	c.ObserveJSON([]byte(`{"usage":{"output_tokens":1}}`))
	if c.Finish() != nil {
		t.Error("incomplete response emitted a success record")
	}
}

type proxyFixture struct {
	server *httptest.Server
	client *upstream.Client
	t      *testing.T
}

type observedBody struct {
	io.ReadCloser
	closes atomic.Int32
}

func (b *observedBody) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}

func (p proxyFixture) forward(body []byte) upstream.Send {
	req := upstream.Request{Target: upstream.TargetProxy, BaseURL: p.server.URL, Method: http.MethodPost, RequestURI: "/v1/messages", Body: body}
	var previous *observedBody
	return func(ctx context.Context) (*upstream.Response, error) {
		if previous != nil && previous.closes.Load() != 1 {
			p.t.Error("previous upstream body not closed before retry")
		}
		if ctx == nil {
			p.t.Error("forward called with nil context")
			ctx = p.t.Context()
		}
		res, err := p.client.Do(ctx, req)
		if err == nil {
			b := &observedBody{ReadCloser: res.Body}
			res.Body = b
			previous = b
			p.t.Cleanup(func() {
				if got := b.closes.Load(); got != 1 {
					p.t.Errorf("upstream body closed %d times, want once", got)
					_ = b.ReadCloser.Close()
				}
			})
		}
		return res, err
	}
}

func newProxy(t *testing.T, serve http.HandlerFunc) proxyFixture {
	t.Helper()
	server := httptest.NewServer(serve)
	t.Cleanup(server.Close)
	client := upstream.NewClient(upstream.Options{})
	t.Cleanup(func() { _ = client.Close() })
	return proxyFixture{server, client, t}
}

func answer(t *testing.T, code int, body string) proxyFixture {
	t.Helper()
	return newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Test", "kept")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
}

func handle(t *testing.T, req reply.Request, fixture proxyFixture) (*httptest.ResponseRecorder, reply.Result) {
	t.Helper()
	w := httptest.NewRecorder()
	result, err := reply.Handle(t.Context(), w, req, fixture.forward(req.Body))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result.Stream != nil {
		_ = result.Stream.Body.Close()
		t.Fatal("terminal response transferred a stream")
	}
	equal(t, true, result.Contacted)
	return w, result
}

func TestCodexAuthFailure(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stable := filepath.Join(t.TempDir(), "stable gateway")
	if err := os.Symlink(executable, stable); err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	const detail = "API Error: 401 Not authenticated. Run: claude-code-proxy codex auth login"
	tests := map[string]struct {
		status int
		stream bool
	}{"error: buffered unauthorized": {401, false}, "error: streaming unauthorized": {401, true}, "error: forbidden auth detail": {403, false}}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			body := fmt.Appendf(nil, `{"stream":%t}`, tc.stream)
			server := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				b, e := io.ReadAll(r.Body)
				if e != nil {
					t.Error(e)
				}
				equal(t, string(body), string(b))
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, detail)
			})
			w, _ := handle(t, reply.Request{Provider: "codex", AdvertisedModel: "claude-gpt-5.6-sol", Body: body, CommandPath: stable}, server)
			equal(t, tc.status, w.Code)
			equal(t, "application/json", w.Header().Get("Content-Type"))
			equal(t, strconv.Itoa(w.Body.Len()), w.Header().Get("Content-Length"))
			var got struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			equal(t, "error", got.Type)
			equal(t, "authentication_error", got.Error.Type)
			match := regexp.MustCompile("`\"([^\"]+)\" login`").FindStringSubmatch(got.Error.Message)
			if len(match) != 2 {
				t.Fatalf("missing quoted stable login command: %q", got.Error.Message)
			}
			equal(t, stable, match[1])
			info, e := os.Stat(match[1])
			if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				t.Errorf("advised path %q is not an executable file: %v", match[1], e)
			}
			if regexp.MustCompile(`(?i)claude-code-proxy\s+codex\s+auth\s+login`).MatchString(got.Error.Message) || !strings.Contains(got.Error.Message, "API Error: 401 Not authenticated.") {
				t.Errorf("upstream detail was not preserved and sanitized: %q", got.Error.Message)
			}
		})
	}
	equal(t, int32(3), attempts.Load())
}

// Keep the real transport and its idle-connection timers outside the retry's virtual clock.
func outsideClock(t *testing.T, send upstream.Send) upstream.Send {
	t.Helper()
	type result struct {
		response *upstream.Response
		err      error
	}
	call, returned, stopped := make(chan struct{}), make(chan result), make(chan struct{})
	go func() {
		defer close(stopped)
		for range call {
			r, err := send(t.Context())
			returned <- result{r, err}
		}
	}()
	t.Cleanup(func() { close(call); <-stopped })
	return func(ctx context.Context) (*upstream.Response, error) {
		if ctx == nil {
			t.Error("forward called with nil context")
		} else if ctx.Err() != nil {
			t.Errorf("forward called with done context: %v", ctx.Err())
		}
		call <- struct{}{}
		r := <-returned
		return r.response, r.err
	}
}

func TestUpgradeRetry(t *testing.T) {
	const rejection = `{"error":{"type":"permission_error","message":"WebSocket upgrade was rejected"}}`
	tests := map[string]struct {
		provider  string
		retries   *int
		delay     *time.Duration
		recover   bool
		wantCodes []time.Duration
		status    int
		want      string
	}{
		"success: third attempt":           {"codex", nil, nil, true, []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond}, 200, `{"model":"public"}`},
		"error: exhausted non Codex proxy": {"kimi", nil, nil, false, []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond}, 503, `{"type":"error","error":{"type":"api_error","message":"model-gateway: Codex WebSocket upgrade was temporarily rejected after 3 attempts; retry the request."}}`},
		"error: zero retries":              {"codex", new(0), nil, false, []time.Duration{0}, 503, `{"type":"error","error":{"type":"api_error","message":"model-gateway: Codex WebSocket upgrade was temporarily rejected after 1 attempts; retry the request."}}`},
		"success: zero retry delay":        {"codex", nil, new(time.Duration(0)), true, []time.Duration{0, 0, 0}, 200, `{"model":"public"}`},
		"error: configured retry budget":   {"codex", new(1), new(7 * time.Millisecond), false, []time.Duration{0, 7 * time.Millisecond}, 503, `{"type":"error","error":{"type":"api_error","message":"model-gateway: Codex WebSocket upgrade was temporarily rejected after 2 attempts; retry the request."}}`},
		// A positive delay is a Go duration used as given, never truncated to whole milliseconds
		// as Node's timer does: 1.5ms waits 1.5ms and 250µs waits 250µs.
		"success: fractional millisecond retry delay": {"codex", nil, new(1500 * time.Microsecond), true, []time.Duration{0, 1500 * time.Microsecond, 3 * time.Millisecond}, 200, `{"model":"public"}`},
		"success: sub-millisecond retry delay":        {"codex", nil, new(250 * time.Microsecond), true, []time.Duration{0, 250 * time.Microsecond, 500 * time.Microsecond}, 200, `{"model":"public"}`},
		"success: one nanosecond retry delay":         {"codex", nil, new(time.Nanosecond), true, []time.Duration{0, time.Nanosecond, 2 * time.Nanosecond}, 200, `{"model":"public"}`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			const recovered = `{"model":"gpt-test"}`
			capture := newCapture()
			req := reply.Request{Provider: tc.provider, AdvertisedModel: "public", Body: []byte(`{}`), Retries: tc.retries, RetryDelay: tc.delay, Usage: capture}
			var attempts atomic.Int32
			fixture := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
				n := attempts.Add(1)
				w.Header().Set("X-Attempt", "kept")
				w.Header().Set("Retry-After", "2")
				w.Header().Set("Transfer-Encoding", "chunked")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("Keep-Alive", "timeout=5")
				if tc.recover && n == 3 {
					_, _ = io.WriteString(w, recovered)
				} else {
					w.WriteHeader(403)
					_, _ = io.WriteString(w, rejection)
				}
			})
			send := outsideClock(t, fixture.forward(req.Body))
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var calls []time.Duration
				w := httptest.NewRecorder()
				res, err := reply.Handle(t.Context(), w, req, func(ctx context.Context) (*upstream.Response, error) {
					calls = append(calls, time.Since(start))
					if len(calls) > len(tc.wantCodes) {
						return nil, errors.New("unexpected extra attempt")
					}
					return send(ctx)
				})
				if err != nil {
					t.Fatal(err)
				}
				equal(t, true, res.Contacted)
				equal(t, true, res.Stream == nil)
				equal(t, len(tc.wantCodes), int(attempts.Load()))
				equal(t, tc.wantCodes, calls)
				equal(t, tc.status, w.Code)
				equal(t, tc.want, w.Body.String())
				equal(t, "kept", w.Header().Get("X-Attempt"))
				bytes := len(calls) * len(rejection)
				if tc.recover {
					bytes += len(recovered) - len(rejection)
				}
				checkCapture(t, capture, tc.status, bytes, map[string]any{"retry_after_ms": float64(2000)})
				if tc.status == 503 {
					equal(t, 503, res.Status)
					equal(t, "upstream_error", res.StatusOverride)
				}
				for _, key := range []string{"Transfer-Encoding", "Connection", "Keep-Alive"} {
					if _, ok := w.Result().Header[key]; ok {
						t.Errorf("forwarded hop-by-hop header %s", key)
					}
				}
				equal(t, strconv.Itoa(w.Body.Len()), w.Header().Get("Content-Length"))
			})
		})
	}
	t.Run("error: disconnect during retry delay", func(t *testing.T) {
		req := reply.Request{Provider: "codex", Body: []byte(`{}`)}
		var attempts atomic.Int32
		fixture := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.WriteHeader(403)
			_, _ = io.WriteString(w, rejection)
		})
		send := outsideClock(t, fixture.forward(req.Body))
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
			var result reply.Result
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				result, err = reply.Handle(ctx, w, req, send)
			}()
			synctest.Wait()
			select {
			case <-done:
				t.Fatalf("Handle returned before waiting to retry: %v", err)
			default:
			}
			equal(t, 1, int(attempts.Load()))
			cancel()
			<-done
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			equal(t, 0, len(w.heads))
			equal(t, 0, w.Body.Len())
			equal(t, true, result.Stream == nil)
			equal(t, 1, int(attempts.Load()))
			equal(t, true, result.Contacted)
			equal(t, false, result.Cancelled)
			equal(t, 0, result.Status)
		})
	})
	t.Run("error: disconnect during in-flight attempt with zero retry delay", func(t *testing.T) {
		type ctxKey struct{}
		ctx, cancel := context.WithCancel(context.WithValue(t.Context(), ctxKey{}, "client"))
		defer cancel()
		req := reply.Request{Provider: "codex", Body: []byte(`{}`), RetryDelay: new(time.Duration(0))}
		var attempts, forwardCalls atomic.Int32
		inFlight, release := make(chan struct{}), make(chan struct{})
		releaseOnce := sync.OnceFunc(func() { close(release) })
		defer releaseOnce()
		fixture := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
			if attempts.Add(1) == 1 {
				close(inFlight)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			w.WriteHeader(403)
			_, _ = io.WriteString(w, rejection)
		})
		base := fixture.forward(req.Body)
		gotCtx := make(chan context.Context, 1)
		var firstBody *observedBody
		var firstErr error
		var firstStatus int
		send := func(callCtx context.Context) (*upstream.Response, error) {
			n := forwardCalls.Add(1)
			if n == 1 {
				gotCtx <- callCtx
			}
			res, err := base(callCtx)
			if n == 1 {
				firstErr = err
				if err == nil {
					firstStatus = res.StatusCode
					firstBody, _ = res.Body.(*observedBody)
				}
			}
			return res, err
		}
		w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
		var result reply.Result
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			result, err = reply.Handle(ctx, w, req, send)
		}()
		defer func() {
			releaseOnce()
			<-done
		}()
		select {
		case <-inFlight:
		case <-done:
			t.Fatalf("Handle returned before attempt 1 reached upstream: err=%v result=%+v", err, result)
		}
		forwardCtx := <-gotCtx
		if forwardCtx == nil {
			t.Fatal("forward received nil context")
		}
		equal(t, "client", forwardCtx.Value(ctxKey{}))
		if forwardCtx.Err() != nil {
			t.Errorf("forward context done before client cancel: %v", forwardCtx.Err())
		}
		cancel()
		if forwardCtx.Err() != nil {
			t.Errorf("forward context cancelled by client cancel: %v", forwardCtx.Err())
		}
		select {
		case <-forwardCtx.Done():
			t.Error("forward context Done channel closed after client cancel")
		default:
		}
		releaseOnce()
		<-done
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if firstErr != nil {
			t.Fatalf("attempt 1 failed: %v", firstErr)
		}
		equal(t, 403, firstStatus)
		equal(t, 1, int(attempts.Load()))
		equal(t, 0, len(w.heads))
		equal(t, 0, w.Body.Len())
		equal(t, true, result.Stream == nil)
		equal(t, false, result.Cancelled)
		equal(t, true, result.Contacted)
		equal(t, 0, result.Status)
		if firstBody == nil {
			t.Fatal("attempt 1 returned no upstream body")
		}
		equal(t, int32(1), firstBody.closes.Load())
	})
	t.Run("error: already done client context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req := reply.Request{Provider: "codex", Body: []byte(`{}`)}
		var attempts, forwardCalls atomic.Int32
		fixture := newProxy(t, func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.WriteHeader(200)
		})
		base := fixture.forward(req.Body)
		send := func(callCtx context.Context) (*upstream.Response, error) {
			forwardCalls.Add(1)
			return base(callCtx)
		}
		w := &countedWriter{ResponseRecorder: httptest.NewRecorder()}
		result, err := reply.Handle(ctx, w, req, send)
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		equal(t, int32(0), forwardCalls.Load())
		equal(t, int32(0), attempts.Load())
		equal(t, false, result.Contacted)
		equal(t, 0, len(w.heads))
		equal(t, 0, w.Body.Len())
		equal(t, true, result.Stream == nil)
		equal(t, false, result.Cancelled)
		equal(t, 0, result.Status)
	})
}
