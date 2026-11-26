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
	"bytes"
	"cmp"
	"context"
	"math"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Emitter sends counts-only records to the configured loopback collector.
type Emitter struct{}

// NewEmitter resolves log settings and shares one builder across captures.
func NewEmitter(lookup config.LookupEnv, builder *Builder) *Emitter {
	return newEmitter(lookup, builder, nil)
}

// A nil dial function uses the standard network dialer.
func newEmitter(lookup config.LookupEnv, builder *Builder, dialContext func(context.Context, string, string) (net.Conn, error)) *Emitter {
	return &Emitter{}
}

// Enabled reports whether records have a local export destination.
func (e *Emitter) Enabled() bool { return false }

// Start allocates a request sequence in arrival order, not completion order.
func (e *Emitter) Start(req Request) *Capture { return &Capture{} }

// Wait waits for scheduled exports after the last request has finished.
func (e *Emitter) Wait() {}

// defaultMaxResponseBytes bounds a buffered JSON response when the caller supplies no limit.
const defaultMaxResponseBytes = 4 << 20

// Capture observes one request without modifying its response. Its methods may be called
// from different goroutines.
type Capture struct {
	mu            sync.Mutex
	builder       *Builder
	req           Request
	maxBytes      int
	started       time.Time
	emit          func(Record)
	usage         jsonx.WireValue
	id            string
	model         string
	mode          string
	status        int
	headers       http.Header
	bytes         int64
	retained      [][]byte
	retainedBytes int
	overflowed    bool
	incomplete    bool
	finished      bool
}

// NewCapture observes a standalone request and emits its primary record to emit, if non-nil.
// A zero maxResponseBytes selects the default bound; any bound below 1024 bytes is raised to 1024.
// The response fields of req are replaced by the observations.
func NewCapture(builder *Builder, req Request, maxResponseBytes int, emit func(Record)) *Capture {
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	return &Capture{builder: builder, req: req, maxBytes: max(1024, maxResponseBytes), started: time.Now(), emit: emit, usage: jsonx.WireValue{Kind: '{'}}
}

// SetResponse saves the response status and headers for the final observation.
func (c *Capture) SetResponse(status int, headers http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status, c.headers = status, headers
}

// ObserveJSON observes and counts a complete JSON response.
func (c *Capture) ObserveJSON(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes += int64(len(body))
	if len(body) > c.maxBytes {
		c.overflowed = true
		return
	}
	c.observeDocument(body)
}

// observeDocument merges a parsed response document's identity and usage. A body that is not
// JSON, or is JSON null, is ignored and leaves the response mode unset.
func (c *Capture) observeDocument(body []byte) {
	v, err := jsonx.DecodeWire(body)
	if err != nil || v.Kind == 'n' {
		return
	}
	c.observeIdentity(v)
	c.usage = MergeWireUsage(c.usage, field(v, "usage"))
	c.mode = cmp.Or(c.mode, "json")
}

// observeIdentity keeps the first safe response ID and model seen since the last reset.
func (c *Capture) observeIdentity(message jsonx.WireValue) {
	if id := field(message, "id"); c.id == "" && id.Kind == '"' {
		c.id = safeID(string(utf16.Decode(id.Text)))
	}
	if model := field(message, "model"); c.model == "" && model.Kind == '"' {
		c.model = safeID(string(utf16.Decode(model.Text)))
	}
}

// ObserveChunk observes and counts a piece of a buffered JSON response.
func (c *Capture) ObserveChunk(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes += int64(len(chunk))
	if c.overflowed {
		return
	}
	if c.retainedBytes+len(chunk) > c.maxBytes {
		c.markOverflow()
		return
	}
	// The caller may reuse its buffer after this call returns.
	c.retained = append(c.retained, bytes.Clone(chunk))
	c.retainedBytes += len(chunk)
}

// ObserveEvent merges usage from a decoded SSE event without counting wire bytes.
func (c *Capture) ObserveEvent(event jsonx.WireValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.overflowed || event.Kind != '{' && event.Kind != '[' {
		return
	}
	c.mode = "sse"
	switch typ := field(event, "type"); {
	case typ.Kind != '"':
	case slices.Equal(typ.Text, messageStart):
		message := field(event, "message")
		c.observeIdentity(message)
		c.usage = MergeWireUsage(c.usage, field(message, "usage"))
	case slices.Equal(typ.Text, messageDelta):
		c.usage = MergeWireUsage(c.usage, field(event, "usage"))
	}
}

var (
	messageStart = utf16.Encode([]rune("message_start"))
	messageDelta = utf16.Encode([]rune("message_delta"))
)

// NoteResponseBytes accounts for streamed bytes independently of decoded events.
// A negative count is ignored.
func (c *Capture) NoteResponseBytes(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes += max(0, n)
}

// MarkOverflow discards buffered content and suppresses successful usage export.
func (c *Capture) MarkOverflow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markOverflow()
}

func (c *Capture) markOverflow() {
	c.overflowed = true
	c.retained = nil
	c.retainedBytes = 0
}

// ResetUsage discards the previous attempt's identity, response mode and usage.
func (c *Capture) ResetUsage() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usage = jsonx.WireValue{Kind: '{'}
	c.id, c.model, c.mode = "", "", ""
}

// Snapshot returns the cumulative exact usage measurements for this attempt.
func (c *Capture) Snapshot() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return exactWireUsage(c.usage)
}

// SetResponseComplete records whether the complete upstream response was observed.
func (c *Capture) SetResponseComplete(complete bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.incomplete = !complete
}

// Finish emits at most once and returns the primary record, or nil if none was emitted.
// Derived records built for the request are not emitted.
func (c *Capture) Finish() *Record {
	c.mu.Lock()
	if c.finished || c.builder == nil {
		c.mu.Unlock()
		return nil
	}
	c.finished = true
	if len(c.retained) > 0 && !c.overflowed {
		body := bytes.Join(c.retained, nil)
		c.retained, c.retainedBytes = nil, 0
		c.observeDocument(body)
	}
	req := c.req
	req.StatusCode, req.ResponseHeaders = c.status, c.headers
	req.Usage, req.WireUsage = nil, c.usage
	req.MessageID, req.ResponseModel, req.ResponseMode = c.id, c.model, c.mode
	req.ResponseBytes, req.Elapsed = c.bytes, time.Since(c.started)
	req.Incomplete, req.Overflowed = c.incomplete, c.overflowed
	if req.Sequence == nil {
		// A capture started outside an emitter has no arrival sequence and reports none.
		// Build reports no sequence above the exact-integer range instead of numbering it itself.
		unsequenced := uint64(math.MaxUint64)
		req.Sequence = &unsequenced
	}
	c.mu.Unlock()
	records := c.builder.Build(req)
	if len(records) == 0 {
		return nil
	}
	record := records[0]
	if c.emit != nil {
		c.emit(record)
	}
	return &record
}
