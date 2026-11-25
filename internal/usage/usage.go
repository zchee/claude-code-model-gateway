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

// Package usage builds counts-only gateway observations without exporting them.
package usage

import (
	"encoding/json/jsontext"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Request contains the inputs needed to attribute a completed response.
// Response capture will supply the message ID and model, route model and backend,
// elapsed time, response byte count, and completion and overflow state.
type Request struct {
	Body            jsonx.WireValue
	BodyBytes       int64
	Headers         http.Header
	StatusCode      int
	ResponseHeaders http.Header
	Usage           map[string]any
	TicketRef       string
}

// Record is one local observation before conversion to OTLP.
type Record struct {
	EventName  string
	ObservedAt time.Time
	TraceID    string
	SpanID     string
	Attributes map[string]any
}

// OTLP encodes this record as an OTLP/HTTP JSON log export payload.
func (r Record) OTLP() (jsontext.Value, error) { return nil, errors.New("usage: OTLP not implemented") }

// Builder retains bounded counts-only snapshots across requests.
type Builder struct{}

// Options configures snapshot bounds, transcript lookup, clock and ID generation.
type Options struct {
	MaxIdentities int
	SnapshotTTL   time.Duration
	ProjectsDir   string
	Now           func() time.Time
	Random        io.Reader
}

// NewBuilder creates an independent record builder without a network transport.
func NewBuilder(options Options) *Builder { return &Builder{} }

// Build returns the primary record and any derived records for a completed request.
func (b *Builder) Build(request Request) []Record { return nil }

// MergeUsage merges numeric usage trees, giving later observations precedence.
func MergeUsage(current, next map[string]any) map[string]any { return nil }

func exactUsage(raw map[string]any) map[string]any { return nil }

func inputComposition(body jsonx.WireValue, bodyBytes int64) map[string]int64 { return nil }

func allocateLargestRemainder(total int64, weights []int64) []int64 { return nil }

// HighWater tracks the largest request body for each session in a state directory.
type HighWater struct{}

// NewHighWater creates a tracker for the supplied directory using the supplied clock.
func NewHighWater(directory string, now func() time.Time) *HighWater { return &HighWater{} }

// Record persists a new maximum and returns the known value, or nil if unavailable.
func (h *HighWater) Record(session string, size int64) *int64 { return nil }
