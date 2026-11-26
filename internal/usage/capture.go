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

package usage

import (
	"net/http"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Emitter sends counts-only records to the configured loopback collector.
type Emitter struct{}

// NewEmitter resolves log settings and shares one builder across captures.
func NewEmitter(lookup config.LookupEnv, builder *Builder) *Emitter { return &Emitter{} }

// Enabled reports whether records have a local export destination.
func (e *Emitter) Enabled() bool { return false }

// Start allocates a request sequence in arrival order, not completion order.
func (e *Emitter) Start(req Request) *Capture { return &Capture{} }

// Wait waits for scheduled exports after the last request has finished.
func (e *Emitter) Wait() {}

// Capture observes one request without modifying its response.
type Capture struct{}

// NewCapture observes a standalone request and emits its primary record to emit, if non-nil.
func NewCapture(builder *Builder, req Request, maxResponseBytes int, emit func(Record)) *Capture {
	return &Capture{}
}

// SetResponse saves the response status and headers for the final observation.
func (c *Capture) SetResponse(status int, headers http.Header) {}

// ObserveJSON observes and counts a complete JSON response.
func (c *Capture) ObserveJSON(body []byte) {}

// ObserveChunk observes and counts a piece of a buffered JSON response.
func (c *Capture) ObserveChunk(chunk []byte) {}

// ObserveEvent merges usage from a decoded SSE event without counting wire bytes.
func (c *Capture) ObserveEvent(event jsonx.WireValue) {}

// NoteResponseBytes accounts for streamed bytes independently of decoded events.
func (c *Capture) NoteResponseBytes(n int64) {}

// MarkOverflow discards buffered content and suppresses successful usage export.
func (c *Capture) MarkOverflow() {}

// ResetUsage discards the previous attempt's identity, response mode and usage.
func (c *Capture) ResetUsage() {}

// Snapshot returns the cumulative exact usage measurements for this attempt.
func (c *Capture) Snapshot() map[string]any { return nil }

// SetResponseComplete records whether the complete upstream response was observed.
func (c *Capture) SetResponseComplete(complete bool) {}

// Finish emits at most once and returns the primary record, or nil if none was emitted.
func (c *Capture) Finish() *Record { return nil }
