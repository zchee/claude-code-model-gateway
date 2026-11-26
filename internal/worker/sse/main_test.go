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

package sse_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// Compile-time check that *usage.Capture directly satisfies sse.UsageCapture with no adapter.
var _ sse.UsageCapture = (*usage.Capture)(nil)

func TestMain(m *testing.M) {
	venue.Require()
	goleak.VerifyTestMain(m)
}

func mustDecodeWire(t *testing.T, raw string) jsonx.WireValue {
	t.Helper()
	if raw == "" {
		return jsonx.WireValue{}
	}
	v, err := jsonx.DecodeWire([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeWire(%q): %v", raw, err)
	}
	return v
}

func mustEncodeWire(t *testing.T, v jsonx.WireValue) string {
	t.Helper()
	if v.Kind == 0 {
		return ""
	}
	b, err := jsonx.EncodeWire(v)
	if err != nil {
		t.Fatalf("EncodeWire: %v", err)
	}
	return string(b)
}

type usageSpy struct {
	mu        sync.Mutex
	events    []jsonx.WireValue
	chunks    []byte
	bytes     int64
	overflows int
}

func (u *usageSpy) ObserveEvent(event jsonx.WireValue) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.events = append(u.events, event)
}

func (u *usageSpy) ObserveChunk(chunk []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.chunks = append(u.chunks, chunk...)
}

func (u *usageSpy) NoteResponseBytes(n int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bytes += n
}

func (u *usageSpy) MarkOverflow() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.overflows++
}

func (u *usageSpy) snapshot(t *testing.T) (events []string, chunks []byte, totalBytes int64, overflows int) {
	t.Helper()
	u.mu.Lock()
	rawEvents := slices.Clone(u.events)
	chunks = slices.Clone(u.chunks)
	totalBytes = u.bytes
	overflows = u.overflows
	u.mu.Unlock()
	if len(rawEvents) > 0 {
		events = make([]string, len(rawEvents))
		for i, ev := range rawEvents {
			events[i] = mustEncodeWire(t, ev)
		}
	}
	return events, chunks, totalBytes, overflows
}

type recordedOverflow struct {
	Counts         sentry.Counts
	UpstreamStatus int
}

// streamingRecorder is a concurrency-safe http.ResponseWriter and http.Flusher
// usable inside testing/synctest bubbles without OS sockets.
type streamingRecorder struct {
	mu              sync.Mutex
	header          http.Header
	status          int
	wroteHeader     bool
	buf             bytes.Buffer
	writes          []string
	attemptedWrites []string
	flushes         int
	writeErr        error
	onWrite         func(chunk string)
	onFlush         func(flushes int)
}

func newStreamingRecorder() *streamingRecorder {
	return &streamingRecorder{header: make(http.Header)}
}

func (r *streamingRecorder) Header() http.Header { return r.header }

func (r *streamingRecorder) WriteHeader(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
}

func (r *streamingRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	if !r.wroteHeader {
		r.wroteHeader = true
		r.status = http.StatusOK
	}
	s := string(p)
	r.attemptedWrites = append(r.attemptedWrites, s)
	if r.writeErr != nil {
		err := r.writeErr
		r.mu.Unlock()
		return 0, err
	}
	r.buf.Write(p)
	r.writes = append(r.writes, s)
	cb := r.onWrite
	r.mu.Unlock()
	if cb != nil {
		cb(s)
	}
	return len(p), nil
}

func (r *streamingRecorder) Flush() {
	r.mu.Lock()
	if !r.wroteHeader {
		r.wroteHeader = true
		r.status = http.StatusOK
	}
	r.flushes++
	n := r.flushes
	cb := r.onFlush
	r.mu.Unlock()
	if cb != nil {
		cb(n)
	}
}

func (r *streamingRecorder) setWriteError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeErr = err
}

func (r *streamingRecorder) attemptCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attemptedWrites)
}

func (r *streamingRecorder) attemptedSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.attemptedWrites)
}

func (r *streamingRecorder) snapshot() (status int, wroteHeader bool, body string, writes []string, flushes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.wroteHeader, r.buf.String(), slices.Clone(r.writes), r.flushes
}

type handlerOutcome struct {
	res outcome.Result
	err error
}

type relayHarness struct {
	server        *httptest.Server
	client        *http.Client
	serviceCtx    context.Context
	cancelService context.CancelFunc
	results       chan handlerOutcome
}

func (h *relayHarness) waitResult(t *testing.T) (outcome.Result, error) {
	t.Helper()
	select {
	case out := <-h.results:
		return out.res, out.err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for relay handler completion")
		return outcome.Result{}, nil
	}
}

func relayHeadersFrom(in http.Header) http.Header {
	out := in.Clone()
	out.Del("Transfer-Encoding")
	out.Del("Connection")
	out.Del("Keep-Alive")
	return out
}

// startRelayServer wraps a relay function behind a real httptest server with an independent
// service-owned upstream context so client disconnects do not implicitly cancel the provider.
func startRelayServer(t *testing.T, upstreamURL string, relay func(serviceCtx context.Context, w http.ResponseWriter, r *http.Request, up *upstream.Response) (outcome.Result, error)) *relayHarness {
	t.Helper()
	serviceCtx, cancelService := context.WithCancel(t.Context())
	upTransport := &http.Transport{DisableCompression: true}
	upClient := &http.Client{Transport: upTransport}
	results := make(chan handlerOutcome, 16)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upReq, err := http.NewRequestWithContext(serviceCtx, r.Method, upstreamURL+r.URL.RequestURI(), r.Body)
		if err != nil {
			results <- handlerOutcome{err: err}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		upResp, err := upClient.Do(upReq)
		if err != nil {
			results <- handlerOutcome{err: err}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = upResp.Body.Close() }()
		up := &upstream.Response{
			StatusCode:  upResp.StatusCode,
			Header:      upResp.Header.Clone(),
			RelayHeader: relayHeadersFrom(upResp.Header),
			Body:        upResp.Body,
		}

		res, err := relay(serviceCtx, w, r, up)
		results <- handlerOutcome{res: res, err: err}
		if errors.Is(err, http.ErrAbortHandler) {
			panic(http.ErrAbortHandler)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))

	clientTransport := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: clientTransport}

	// LIFO cleanup order: close client idle conns -> cancel serviceCtx (unblocking paused relays)
	// -> close relay server -> close upstream idle conns.
	t.Cleanup(upTransport.CloseIdleConnections)
	t.Cleanup(srv.Close)
	t.Cleanup(cancelService)
	t.Cleanup(clientTransport.CloseIdleConnections)

	return &relayHarness{
		server:        srv,
		client:        client,
		serviceCtx:    serviceCtx,
		cancelService: cancelService,
		results:       results,
	}
}

// testEngineOverflowResponder stands in for reply.WriteEngineOverflow (bound by the worker)
// and writes the documented normalized 413 response with outcome facts.
func testEngineOverflowResponder(w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) (outcome.Result, error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Model-Gateway-Upstream-Status", fmt.Sprintf("%d", upstreamStatus))
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	tokensFrom := ""
	tokensJSON := "null"
	limitJSON := "null"
	msg := "Input exceeds the model context window; compact and retry."
	if counts.Tokens != nil {
		tokensFrom = "upstream"
		tokensJSON = fmt.Sprintf("%d", int(*counts.Tokens))
	}
	if counts.Limit != nil {
		limitJSON = fmt.Sprintf("%d", int(*counts.Limit))
	}
	if counts.Tokens != nil && counts.Limit != nil {
		msg = fmt.Sprintf("Input exceeds the model context window; compact and retry. (%d tokens > %d tokens)", int(*counts.Tokens), int(*counts.Limit))
	}
	tokensFromJSON := "null"
	if tokensFrom != "" {
		tokensFromJSON = fmt.Sprintf("%q", tokensFrom)
	}
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"request_too_large","message":%q,"overflow":{"tokens":%s,"limit_tokens":%s,"tokens_from":%s,"refused_by":"upstream"}}}`,
		msg, tokensJSON, limitJSON, tokensFromJSON)
	return outcome.Result{
		Status: http.StatusRequestEntityTooLarge,
		Route: outcome.Route{
			Overflow: &outcome.Overflow{
				Phase:       "first_turn",
				RefusedBy:   "upstream",
				TokensFrom:  tokensFrom,
				Tokens:      counts.Tokens,
				LimitTokens: counts.Limit,
			},
		},
	}, nil
}
