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

// Package telemetry records local route decisions without retaining message content.
package telemetry

import (
	"io"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

// Options supplies the environment, resolved audit path, diagnostics, clock and entropy.
type Options struct {
	Lookup      config.LookupEnv
	LogPath     string
	Diagnostics io.Writer
	Now         func() time.Time
	Random      io.Reader
}

// Recorder owns local route logging and asynchronous trace delivery for one worker.
type Recorder struct{}

// NewRecorder creates a recorder from the worker's isolated configuration.
func NewRecorder(options Options) *Recorder { return &Recorder{} }

// LogEntry holds only the allowed route and dispatch metadata for the local audit file.
type LogEntry struct {
	Backend                                                                             string
	Model                                                                               *string
	Via, Effort, SessionID, AgentID, ParentAgentID, InheritedFromAgentID, SessionSource string
	MarkersLength                                                                       *int
}

// LogRoute appends one audit entry using the request path and fallback session header.
func (r *Recorder) LogRoute(request *http.Request, entry LogEntry) {}

// Enabled reports whether a local route span destination is configured.
func (r *Recorder) Enabled() bool { return false }

// Span represents one client request, or one independently reported retry outcome.
type Span struct{}

// Start records the start instant and incoming trace and session identity.
func (r *Recorder) Start(headers http.Header) *Span { return &Span{} }

// Route describes the resolved model and classification at response completion.
type Route struct {
	SelectedModel, EffectiveModel, Backend, Effort, Via string
	Fallback                                            bool
	Compaction                                          *Compaction
	Overflow                                            *Overflow
}

// Compaction contains safe outcome labels and measured token counts; unset counts are omitted.
type Compaction struct {
	Outcome, TerminalCode, ErrorCode                                string
	UpstreamStatus                                                  *int
	InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens *float64
}

// Overflow holds only the classifier's diagnostic labels and numeric measurements.
type Overflow struct {
	Phase, RefusedBy, TokensFrom string
	RequestBytes                 int64
	Tokens, LimitTokens          *float64
}

// Finish queues the final span once without waiting for the collector's reply.
// A zero status code denotes the absence of an HTTP response.
func (s *Span) Finish(route Route, statusCode int, statusOverride string) {}

// Wait waits for pending trace exports after all spans have finished.
func (r *Recorder) Wait() {}
