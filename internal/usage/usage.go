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
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"io"
	"maps"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Request contains the inputs needed to attribute a completed response.
type Request struct {
	Body            jsonx.WireValue
	BodyBytes       int64
	Headers         http.Header
	StatusCode      int
	ResponseHeaders http.Header
	Usage           map[string]any
	TicketRef       string
	MessageID       string
	ResponseModel   string
	RouteModel      string
	EffectiveModel  string
	RequestedModel  string
	SelectedModel   string
	Backend         string
	Effort          string
	Via             string
	ResponseMode    string
	ResponseBytes   int64
	Elapsed         time.Duration
	Incomplete      bool
	Overflowed      bool
	// Sequence, when provided by response capture, preserves request start order across overlapping responses.
	Sequence *uint64
}

// Record is one local observation before conversion to OTLP.
type Record struct {
	EventName  string
	ObservedAt time.Time
	TraceID    string
	SpanID     string
	Attributes map[string]any
}

// Builder retains bounded counts-only snapshots across requests.
type Builder struct {
	mu        sync.Mutex
	options   Options
	sequence  uint64
	snapshots recent[identity, snapshot]
	projects  recent[string, string]
}

// Options configures snapshot bounds, transcript lookup, clock and ID generation.
type Options struct {
	MaxIdentities int
	SnapshotTTL   time.Duration
	ProjectsDir   string
	Now           func() time.Time
	Random        io.Reader
}

// NewBuilder creates an independent record builder without a network transport.
func NewBuilder(options Options) *Builder {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MaxIdentities <= 0 {
		options.MaxIdentities = 500
	}
	if options.SnapshotTTL <= 0 {
		options.SnapshotTTL = 4 * time.Hour
	}
	return &Builder{options: options}
}

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@\[\]-]{0,254}$`)
	tracePattern      = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}(?:-[0-9a-f-]+)?$`)
)

func safeID(s string) string {
	if identifierPattern.MatchString(s) {
		return s
	}
	return ""
}

func nullable(s string) any {
	if s != "" {
		return s
	}
	return nil
}

func identifierHeader(h http.Header, name string) string { v, _ := header(h, name); return safeID(v) }

func (b *Builder) id(prefix string) string {
	if b.options.Random == nil {
		return prefix + uuid.NewV4().String()
	}
	var id uuid.UUID
	if _, e := io.ReadFull(b.options.Random, id[:]); e != nil {
		return ""
	}
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return prefix + id.String()
}

func statusClass(code int) string {
	switch {
	case code == 0:
		return "unknown"
	case code >= 500:
		return "server_error"
	case code == 429:
		return "throttled"
	case code >= 400:
		return "client_error"
	case code >= 200 && code < 300:
		return "ok"
	default:
		return "other"
	}
}

// Build returns the primary record and any derived records for a completed request.
func (b *Builder) Build(req Request) []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sequence++
	now := b.options.Now()
	a := limitEvidence(req.ResponseHeaders, now)
	event := "gateway.limit.signal"
	success := req.StatusCode >= 200 && req.StatusCode < 300
	var servers []serverMeasure
	if success {
		exact := exactUsage(req.Usage)
		if req.Incomplete || req.Overflowed || exact["input_tokens"] == nil && exact["output_tokens"] == nil && exact["cache_read_tokens"] == nil && exact["cache_creation_tokens"] == nil {
			return nil
		}
		maps.Copy(a, exact)
		counts, measures := measureInput(req.Body, req.BodyBytes)
		for k, v := range counts {
			a[k] = v
		}
		servers = attribute(a, counts, measures)
		event = "gateway.token.usage"
	} else if len(a) == 0 {
		return nil
	}
	key := identity{identifierHeader(req.Headers, "x-claude-code-session-id"), identifierHeader(req.Headers, "x-claude-code-agent-id"), identifierHeader(req.Headers, "x-claude-code-parent-agent-id")}
	a["source"] = "codex_gateway"
	a["source_schema"] = "gateway-usage-v1"
	a["source_event_id"] = b.id("gateway-usage-")
	a["event_name"] = event
	a["event.name"] = event
	rid := cmp.Or(identifierHeader(req.ResponseHeaders, "request-id"), identifierHeader(req.ResponseHeaders, "x-request-id"), identifierHeader(req.ResponseHeaders, "x-codex-request-id"))
	src := "response_header"
	if rid == "" {
		rid = safeID(req.MessageID)
		src = "message_id"
	}
	if rid == "" {
		rid = b.id("gateway-request-")
		src = "generated"
	}
	a["request_id"] = nullable(rid)
	a["request_id_source"] = src
	a["client_request_id"] = nullable(cmp.Or(identifierHeader(req.Headers, "x-claude-code-request-id"), identifierHeader(req.Headers, "x-request-id")))
	a["session_id"] = nullable(key.session)
	a["project_name"] = nullable(b.projectName(key.session))
	a["agent_id"] = nullable(key.agent)
	a["parent_agent_id"] = nullable(key.parent)
	a["agent_role"] = "orchestrator"
	if key.agent != "" {
		a["agent_role"] = "executor"
	}
	route, response := safeID(cmp.Or(req.EffectiveModel, req.RouteModel)), safeID(req.ResponseModel)
	if req.Backend == "anthropic" {
		route, response = response, route
	}
	a["model"] = nullable(cmp.Or(route, response))
	a["requested_model"] = nullable(safeID(cmp.Or(req.RequestedModel, req.SelectedModel)))
	a["backend"] = nullable(safeID(req.Backend))
	a["via"] = nullable(safeID(req.Via))
	a["ticket_ref"] = nullable(safeID(req.TicketRef))
	a["effort"] = nil
	switch req.Effort {
	case "low", "medium", "high", "xhigh", "max":
		a["effort"] = req.Effort
	}
	a["status"] = statusClass(req.StatusCode)
	a["status_code"] = nil
	if req.StatusCode != 0 {
		a["status_code"] = req.StatusCode
	}
	seq := b.sequence
	if req.Sequence != nil {
		seq = *req.Sequence
	}
	a["request_sequence"] = nil
	if seq < 1<<53 {
		a["request_sequence"] = seq
	}
	a["response_mode"] = nil
	switch req.ResponseMode {
	case "json", "sse":
		a["response_mode"] = req.ResponseMode
	}
	a["response_body_bytes"] = req.ResponseBytes
	a["duration_ms"] = float64(req.Elapsed) / float64(time.Millisecond)
	a["token_estimator"] = nil
	a["cache_attribution"] = nil
	if success {
		a["token_estimator"] = "utf8_bytes_div_4_normalized"
		a["cache_attribution"] = "prefix_order_estimate"
	}
	r := Record{EventName: event, ObservedAt: now, Attributes: a}
	tp, _ := header(req.Headers, "traceparent")
	if m := tracePattern.FindStringSubmatch(strings.TrimSpace(tp)); m != nil && m[1] != "ff" && strings.Trim(m[2], "0") != "" && strings.Trim(m[3], "0") != "" {
		var span [8]byte
		reader := b.options.Random
		if reader == nil {
			reader = rand.Reader
		}
		if _, e := io.ReadFull(reader, span[:]); e == nil {
			r.TraceID = m[2]
			r.SpanID = hex.EncodeToString(span[:])
		}
	}
	records := []Record{r}
	if !success {
		return records
	}
	for _, s := range servers {
		if len(s.name) > 64 {
			continue
		}
		foot := b.derived(r, "gateway.mcp.footprint", "gateway-mcp-footprint-")
		foot.Attributes["mcp_server"] = s.name
		foot.Attributes["input_mcp_tools_tokens"] = s.tokens
		records = append(records, foot)
	}
	return append(records, b.toolRecords(r, key, req.Body)...)
}

func (b *Builder) derived(primary Record, event, prefix string) Record {
	a := map[string]any{"event.name": event, "event_name": event, "source_event_id": b.id(prefix)}
	for _, k := range []string{"source", "source_schema", "request_id", "client_request_id", "session_id", "project_name", "agent_id", "parent_agent_id", "agent_role", "model", "requested_model", "backend", "effort", "via", "ticket_ref", "request_sequence"} {
		a[k] = primary.Attributes[k]
	}
	primary.EventName = event
	primary.Attributes = a
	return primary
}
