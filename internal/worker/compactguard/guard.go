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
	"errors"
	"io"
	"net/http"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// UsageCapture observes raw response bytes and corrected events, independently of delivery.
type UsageCapture interface {
	NoteResponseBytes(int64)
	ObserveEvent(jsonx.WireValue)
	Snapshot() map[string]any
	ResetUsage()
	SetResponseComplete(bool)
}

// Runner buffers eligible compaction streams and reports degradation to Stderr.
type Runner struct {
	// ServiceContext is set once when the worker builds the runner, not per request.
	// It bounds peeking and relay work; a nil context is rejected before dispatch.
	ServiceContext context.Context
	Settings       Settings
	Stderr         io.Writer
}

// Request describes a prepared proxy request. Next invokes the bound reply handler;
// its bool argument says whether the client has already received response headers.
// A nil response is a settled verdict; a non-nil response transfers body ownership.
type Request struct {
	ProxyProvider  string
	PathOnly       string
	UpstreamOrigin string
	Body           []byte
	Next           func(context.Context, bool) (*upstream.Response, outcome.Result, error)
	Stream         sse.ProxyOptions
	Usage          UsageCapture
}

// Serve consumes one stream per attempt and returns the terminal response facts.
// The caller dispatches unguarded requests directly to their ordinary relay.
//
// The ctx parameter tracks client liveness. Once ctx is done, Serve starts no
// new attempt (before any attempt or during a retry wait), stops heartbeats,
// and writes nothing more to w; it does not end a peek or read already in
// flight. Once in-flight upstream work finishes on its own (or if no attempt
// was in flight), Serve returns with a nil error and Result.Cancelled false.
//
// Runner.ServiceContext bounds upstream peeking and reads. When it is done,
// Serve closes any in-flight peek or stream body exactly once, writes nothing
// more to w, and returns promptly with an error matching context.Canceled (or
// the service context's cause).
func (r Runner) Serve(ctx context.Context, w http.ResponseWriter, req Request) (outcome.Result, error) {
	return outcome.Result{}, errors.New("not implemented")
}
