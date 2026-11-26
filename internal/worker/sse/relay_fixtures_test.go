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
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// eofChunkBody returns scripted chunks and delivers io.EOF together with the last one, the way a
// fixed-length or decompressing body may end, so the relay sees the final bytes and the clean end
// in a single read.
type eofChunkBody struct {
	mu     sync.Mutex
	chunks [][]byte
	reads  atomic.Int32
	closes atomic.Int32
	eofs   atomic.Int32
}

func newEOFChunkBody(chunks ...[]byte) *eofChunkBody {
	return &eofChunkBody{chunks: chunks}
}

func (b *eofChunkBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closes.Load() > 0 {
		return 0, io.ErrClosedPipe
	}
	if len(b.chunks) == 0 {
		b.eofs.Add(1)
		return 0, io.EOF
	}
	n := copy(p, b.chunks[0])
	if n < len(b.chunks[0]) {
		b.chunks[0] = b.chunks[0][n:]
		return n, nil
	}
	b.chunks = b.chunks[1:]
	if len(b.chunks) == 0 {
		b.eofs.Add(1)
		return n, io.EOF
	}
	return n, nil
}

func (b *eofChunkBody) Close() error {
	b.closes.Add(1)
	return nil
}

// endDelivered reports whether a read has returned io.EOF, the clean end of the body.
func (b *eofChunkBody) endDelivered() bool {
	return b.eofs.Load() > 0
}

// remaining reports how many scripted chunks no read has delivered yet.
func (b *eofChunkBody) remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.chunks)
}

// heldRead is one scripted result of a heldBody read.
type heldRead struct {
	data []byte
	err  error
}

// heldBody blocks every Read until the test hands it a scripted result or the owner closes it, so a
// test decides when an upstream read completes and what it returns. Each scripted result must fit
// in one relay read buffer.
type heldBody struct {
	results   chan heldRead
	closed    chan struct{}
	closeOnce sync.Once
	reads     atomic.Int32
	closes    atomic.Int32
}

func newHeldBody() *heldBody {
	return &heldBody{results: make(chan heldRead, 1), closed: make(chan struct{})}
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	select {
	case r := <-b.results:
		return copy(p, r.data), r.err
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}

func (b *heldBody) Close() error {
	b.closes.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

// release queues the result of the next read; the read in flight, or the next one, receives it.
func (b *heldBody) release(data string, err error) {
	b.results <- heldRead{data: []byte(data), err: err}
}

// sseResponse wraps body in a 200 upstream response whose Header and RelayHeader are independent
// copies of header, with text/event-stream as the default Content-Type.
func sseResponse(body io.ReadCloser, header http.Header) *upstream.Response {
	h := header.Clone()
	if h == nil {
		h = make(http.Header)
	}
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "text/event-stream")
	}
	return &upstream.Response{StatusCode: http.StatusOK, Header: h.Clone(), RelayHeader: h.Clone(), Body: body}
}

// assertNothingWritten fails when the recorder saw a status line, a body write attempt, or a flush,
// or when its header map holds a field: every caller expects the relay never to have reached the
// point of copying a head.
func assertNothingWritten(t *testing.T, rec *streamingRecorder) {
	t.Helper()
	status, wroteHeader, _, _, flushes := rec.snapshot()
	if attempts := rec.attemptedSnapshot(); wroteHeader || len(attempts) != 0 || flushes != 0 {
		t.Errorf("client output after it left or before the relay could commit: wroteHeader=%t status=%d flushes=%d attempted writes=%q",
			wroteHeader, status, flushes, attempts)
	}
	if got := rec.Header(); len(got) != 0 {
		t.Errorf("client header fields = %v, want none", got)
	}
}

// formatPeekOutcome describes one PeekFirstEvent result for a failure message.
func formatPeekOutcome(round int, res sse.PeekResult, bodyReturned bool, err error) string {
	return fmt.Sprintf("round %d: Body returned=%t Overflow=%v EarlyEndOverride=%q err=%v", round, bodyReturned, res.Overflow, res.EarlyEndOverride, err)
}

// formatRelayOutcome describes one relay result and what the client saw for a failure message.
func formatRelayOutcome(round int, res outcome.Result, err error, wroteHeader bool, status int, body string) string {
	return fmt.Sprintf("round %d: result=%+v err=%v client wroteHeader=%t status=%d body=%q", round, res, err, wroteHeader, status, body)
}
