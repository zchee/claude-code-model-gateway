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
	"math"
	"net/http"
	"slices"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// defaultMaxResponseBytes bounds a buffered JSON response when the caller supplies no limit.
const defaultMaxResponseBytes = 4 << 20

// Capture observes one request without modifying its response. Its methods may be called
// from different goroutines.
type Capture struct {
	mu            sync.Mutex
	builder       *Builder
	req           Request
	maxBytes      int
	evidence      requestEvidence
	emit          func(Record) // caller-supplied; a panic in it is recovered
	handle        func(Record) // an Emitter's own handler; a panic in it reaches the caller of Finish
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
// The response fields of req are replaced by the observations. The request evidence is fixed here:
// changing the body, the headers or the sequence of req afterwards does not change the record.
// The capture reports no sequence unless req supplies one, as an Emitter's captures do. It derives
// no footprint or tool-result records, which an Emitter derives in its own handler, and it leaves
// the builder's request numbering and tool-result baseline unchanged. It shares the builder's
// project lookup cache and random source, as the source's captures share a project resolver and
// the process's randomness: a project name it resolves is cached for later Build calls, and its
// identifiers are read from Options.Random when that is set. The identifiers fixed here are drawn
// even when Finish builds no record.
func NewCapture(builder *Builder, req Request, maxResponseBytes int, emit func(Record)) *Capture {
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	c := &Capture{builder: builder, maxBytes: max(1024, maxResponseBytes), emit: emit, usage: jsonx.WireValue{Kind: '{'}}
	if builder != nil {
		c.evidence = builder.start(req, time.Now())
	}
	// The evidence holds what the record needs from the body and the sequence.
	req.Body, req.Sequence, req.Headers = jsonx.WireValue{}, nil, req.Headers.Clone()
	c.req = req
	return c
}

// SetResponse saves the response status and a copy of the headers for the final observation.
// The status is a valid HTTP status, 100 to 599, or zero when none was supplied.
func (c *Capture) SetResponse(status int, headers http.Header) {
	headers = headers.Clone()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status, c.headers = status, headers
}

// ObserveJSON observes and counts a complete JSON response. The caller may reuse body after
// this call returns.
func (c *Capture) ObserveJSON(body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes = addBytes(c.bytes, int64(len(body)))
	if len(body) > c.maxBytes {
		c.overflowed = true
		return
	}
	c.observeDocument(body)
}

// observeDocument merges a parsed response document's identity and usage. A body that is not
// JSON, or is JSON null, is ignored and leaves the response mode unset. So is the usage of a
// document whose numeric conversion fails in the source, after its identity has been kept.
func (c *Capture) observeDocument(body []byte) {
	v, err := jsonx.DecodeWire(body)
	if err != nil || v.Kind == 'n' {
		return
	}
	c.observeIdentity(v)
	usage := field(v, "usage")
	if numericConversionFails(usage) {
		return
	}
	c.usage = MergeWireUsage(c.usage, usage)
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

// ObserveChunk observes and counts a piece of a buffered JSON response. An empty chunk is ignored.
func (c *Capture) ObserveChunk(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes = addBytes(c.bytes, int64(len(chunk)))
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

// ObserveEvent merges usage from a decoded SSE event without counting wire bytes. The capture
// keeps no storage of event: the caller may change or reuse it, including its member names,
// after this call returns.
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
	c.bytes = addBytes(c.bytes, max(0, n))
}

// addBytes adds a non-negative count to a non-negative byte total, saturating at the largest
// int64 instead of wrapping to a negative total.
func addBytes(total, n int64) int64 {
	if n > math.MaxInt64-total {
		return math.MaxInt64
	}
	return total + n
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

// Finish emits at most once and returns the primary record, or nil if none was built or Finish
// was called before. A panic in a callback supplied to NewCapture is recovered and the record is
// still returned. A capture from Emitter.Start has no such callback: a panic in the emitter's own
// handling of the record reaches the caller, and a later Finish returns nil.
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
	req.ResponseBytes = c.bytes
	req.Incomplete, req.Overflowed = c.incomplete, c.overflowed
	c.mu.Unlock()
	evidence := c.evidence
	// The deferred unlock keeps the builder usable after a panic in its clock or random source.
	record, ok := func() (Record, bool) {
		c.builder.mu.Lock()
		defer c.builder.mu.Unlock()
		r, _, _, ok := c.builder.primary(req, &evidence)
		return r, ok
	}()
	if !ok {
		return nil
	}
	if c.handle != nil {
		c.handle(record)
	}
	if c.emit != nil {
		func() {
			// The source ignores a failing callback; the caller still receives the record.
			defer func() { _ = recover() }()
			c.emit(record)
		}()
	}
	return &record
}
