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
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"go.uber.org/goleak"

	"github.com/zchee/claude-code-model-gateway/internal/venue"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

func TestMain(m *testing.M) {
	venue.Require()
	goleak.VerifyTestMain(m)
}

type recordedTelemetryCall struct {
	Status   int
	Override string
}

type recordingTelemetry struct {
	mu    sync.Mutex
	calls []recordedTelemetryCall
}

func (r *recordingTelemetry) Finish(status int, override string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedTelemetryCall{Status: status, Override: override})
}

func (r *recordingTelemetry) snapshot() []recordedTelemetryCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

type recordingUsage struct {
	mu        sync.Mutex
	events    []string
	chunks    []byte
	bytes     int
	overflows int
}

func (u *recordingUsage) ObserveEvent(event jsontext.Value) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.events = append(u.events, string(event))
}

func (u *recordingUsage) ObserveChunk(chunk []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.chunks = append(u.chunks, chunk...)
}

func (u *recordingUsage) NoteResponseBytes(n int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.bytes += n
}

func (u *recordingUsage) MarkOverflow() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.overflows++
}

func (u *recordingUsage) snapshot() (events []string, chunks []byte, totalBytes, overflows int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.events), slices.Clone(u.chunks), u.bytes, u.overflows
}

type recordedOverflow struct {
	Counts         sentry.Counts
	UpstreamStatus int
}

// streamingRecorder is a concurrency-safe http.ResponseWriter and http.Flusher
// usable inside testing/synctest bubbles without OS sockets.
type streamingRecorder struct {
	mu          sync.Mutex
	header      http.Header
	status      int
	wroteHeader bool
	buf         bytes.Buffer
	writes      []string
	flushes     int
	writeErr    error
	onWrite     func(chunk string)
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
	if r.writeErr != nil {
		err := r.writeErr
		r.mu.Unlock()
		return 0, err
	}
	s := string(p)
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
	defer r.mu.Unlock()
	if !r.wroteHeader {
		r.wroteHeader = true
		r.status = http.StatusOK
	}
	r.flushes++
}

func (r *streamingRecorder) setWriteError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeErr = err
}

func (r *streamingRecorder) snapshot() (status int, wroteHeader bool, body string, writes []string, flushes int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.wroteHeader, r.buf.String(), slices.Clone(r.writes), r.flushes
}

// startRelayServer wraps a relay function behind a real httptest server so tests exercise
// HTTP/1.1 framing, flushing, and http.ErrAbortHandler connection aborts end to end.
func startRelayServer(t *testing.T, upstreamURL string, relay func(w http.ResponseWriter, r *http.Request, up *http.Response) error) (*httptest.Server, *http.Client) {
	t.Helper()
	upTransport := &http.Transport{DisableCompression: true}
	upClient := &http.Client{Transport: upTransport}
	t.Cleanup(upTransport.CloseIdleConnections)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL+r.URL.RequestURI(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		upResp, err := upClient.Do(upReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = upResp.Body.Close() }()

		err = relay(w, r, upResp)
		if errors.Is(err, http.ErrAbortHandler) {
			panic(http.ErrAbortHandler)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	clientTransport := &http.Transport{DisableCompression: true}
	client := &http.Client{Transport: clientTransport}
	t.Cleanup(clientTransport.CloseIdleConnections)
	return srv, client
}

func writeNormalized413(w http.ResponseWriter, counts sentry.Counts, upstreamStatus int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Model-Gateway-Upstream-Status", fmt.Sprintf("%d", upstreamStatus))
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	tokens, limit := 0, 0
	if counts.Tokens != nil {
		tokens = int(*counts.Tokens)
	}
	if counts.Limit != nil {
		limit = int(*counts.Limit)
	}
	_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"request_too_large","message":"Input exceeds the model context window; compact and retry. (%d tokens > %d tokens)","overflow":{"tokens":%d,"limit_tokens":%d,"tokens_from":"upstream","refused_by":"upstream"}}}`,
		tokens, limit, tokens, limit)
}
