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

package compactguard

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

var errNotImplemented = errors.New("not implemented")

// Upstream sends one upstream HTTP attempt for a request.
// Expected to be satisfied by internal/worker/upstream.
type Upstream interface {
	Do(ctx context.Context) (*http.Response, error)
}

// UpstreamFunc adapts a function to the Upstream interface.
type UpstreamFunc func(ctx context.Context) (*http.Response, error)

// Do calls f(ctx).
func (f UpstreamFunc) Do(ctx context.Context) (*http.Response, error) {
	return f(ctx)
}

// StreamTransformer reads one upstream SSE stream, calls observe on each parsed JSON event
// before rewriting, and passes transformed wire chunks to emit.
// Expected to be satisfied by internal/worker/sse.
type StreamTransformer interface {
	Transform(ctx context.Context, src io.Reader, emit func([]byte) error, observe func(jsontext.Value)) error
}

// PeekResult describes the result of inspecting the first SSE data event on an LM Studio stream.
type PeekResult struct {
	Stream           io.Reader
	Overflow         *sentry.Counts
	EarlyEndOverride string
}

// FirstEventPeeker inspects the first SSE data event before response headers are committed.
// Expected to be satisfied by internal/worker/sse.
type FirstEventPeeker interface {
	Peek(ctx context.Context, body io.Reader) (PeekResult, error)
}

// UsageCapture records response metadata and token counts across guarded compaction attempts.
// Expected to be satisfied by internal/usage.
type UsageCapture interface {
	SetResponse(statusCode int, headers http.Header)
	NoteResponseBytes(n int64)
	ObserveEvent(event jsontext.Value)
	Snapshot() Usage
	ResetUsage()
	SetResponseComplete(complete bool)
}

// Telemetry records compaction diagnostics and completes the route span once per request.
// Expected to be satisfied by internal/telemetry.
type Telemetry interface {
	SetCompaction(diag Diagnostics)
	Finish(statusCode int, statusOverride string)
}

// Runner executes requests through the compaction stream guard using its configured settings.
type Runner struct {
	Settings          Settings
	HeartbeatInterval time.Duration
	Stderr            io.Writer
}

// Request carries the per-request routing metadata and callbacks needed to serve or guard a stream.
type Request struct {
	ProxyProvider          string
	PathOnly               string
	UpstreamOrigin         string
	Body                   []byte
	Upstream               Upstream
	Transformer            StreamTransformer
	Peeker                 FirstEventPeeker
	Usage                  UsageCapture
	Telemetry              Telemetry
	Sentry                 *sentry.State
	SentryEnabled          bool
	SentrySession          string
	SentryTarget           sentry.Target
	HandleNonStream        func(ctx context.Context, w http.ResponseWriter, resp *http.Response) error
	HandleLMStudioOverflow func(ctx context.Context, w http.ResponseWriter, upstreamStatus int, counts sentry.Counts) error
	HandleUnguarded        func(ctx context.Context, w http.ResponseWriter, resp *http.Response) error
	OnCodexRejection       func(statusCode int, headers http.Header, body []byte)
	FormatCodexAuthFailure func(body []byte, statusCode int) (string, bool)
}

// Serve handles req, guarding eligible proxy compaction streams and streaming ineligible requests live.
// It returns http.ErrAbortHandler when an already-committed response must be aborted after a retry transport error.
func (r Runner) Serve(ctx context.Context, w http.ResponseWriter, req Request) error {
	return errNotImplemented
}
