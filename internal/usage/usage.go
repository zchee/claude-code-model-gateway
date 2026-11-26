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
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Request contains the inputs needed to attribute a completed response.
type Request struct {
	Body            jsonx.WireValue
	BodyBytes       int64
	Headers         http.Header
	StatusCode      int
	ResponseHeaders http.Header
	// Usage is a legacy input that loses JSON object insertion order. Response capture must use WireUsage.
	Usage map[string]any
	// WireUsage takes precedence over Usage and preserves member order across response observations.
	WireUsage      jsonx.WireValue
	TicketRef      string
	MessageID      string
	ResponseModel  string
	RouteModel     string
	EffectiveModel string
	RequestedModel string
	SelectedModel  string
	Backend        string
	Effort         string
	Via            string
	ResponseMode   string
	ResponseBytes  int64
	Elapsed        time.Duration
	Incomplete     bool
	Overflowed     bool
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
	order      []string
}

type recordField struct {
	key   string
	value any
}

func (r *Record) add(key string, value any) {
	r.order = append(r.order, key)
	r.Attributes[key] = value
}

func (r *Record) take(source map[string]any, keys ...string) {
	for _, key := range keys {
		r.add(key, source[key])
	}
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
// It panics if Options.Now is nil. Project lookup requires a clean absolute ProjectsDir;
// transcript symlinks leaving that directory are ignored, leaving the project name unset.
// Unlike the source's unbounded Git pointer reads, this lookup accepts .git and commondir files of at most 64 KiB.
func NewBuilder(options Options) *Builder {
	if options.Now == nil {
		panic("usage: missing observation clock")
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

// requestEvidence holds the request facts a capture fixes when it starts, as the source does: the input
// composition, the incoming trace with a new span ID, the generated fallback request ID, the arrival sequence
// and the start time. A direct Build leaves fixed unset and sets only sequence; the request is then measured
// while the record is built and random identifiers are drawn only when the record uses them.
type requestEvidence struct {
	fixed      bool
	counts     map[string]int64
	servers    []serverMeasure
	traceID    string
	spanID     string
	fallbackID string
	// sequence is the arrival order, or nil when the request has none.
	sequence *uint64
	started  time.Time
}

// start fixes the evidence of a request whose capture began at started. The body is measured without
// the builder lock; the trace span and then the fallback request ID are drawn, in the source's order.
func (b *Builder) start(req Request, started time.Time) requestEvidence {
	ev := requestEvidence{fixed: true, started: started}
	if req.Sequence != nil {
		ev.sequence = new(*req.Sequence)
	}
	ev.counts, ev.servers = measureInput(req.Body, req.BodyBytes)
	b.mu.Lock()
	defer b.mu.Unlock()
	ev.traceID, ev.spanID = b.trace(req.Headers)
	ev.fallbackID = b.id("gateway-request-")
	return ev
}

// trace returns the trace ID of a valid incoming traceparent header with a new span ID, or two empty
// strings when the header is absent or invalid or no span ID can be drawn. The caller holds b.mu.
func (b *Builder) trace(h http.Header) (traceID, spanID string) {
	tp, _ := header(h, "traceparent")
	m := tracePattern.FindStringSubmatch(config.JSTrim(tp))
	if m == nil || m[1] == "ff" || strings.Trim(m[2], "0") == "" || strings.Trim(m[3], "0") == "" {
		return "", ""
	}
	var span [8]byte
	reader := b.options.Random
	if reader == nil {
		reader = rand.Reader
	}
	if _, e := io.ReadFull(reader, span[:]); e != nil {
		return "", ""
	}
	return m[2], hex.EncodeToString(span[:])
}

// Build returns the primary record and any derived records for a completed request.
// Every call advances the builder's own request numbering, which a request without a Sequence reports.
func (b *Builder) Build(req Request) []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sequence++
	seq := b.sequence
	if req.Sequence != nil {
		seq = *req.Sequence
	}
	r, key, servers, ok := b.primary(req, &requestEvidence{sequence: &seq})
	if !ok {
		return nil
	}
	records := []Record{r}
	if r.EventName != tokenUsageEvent {
		return records
	}
	records = append(records, b.footprints(r, servers)...)
	return append(records, b.toolRecords(r, key, req.Body)...)
}

const tokenUsageEvent = "gateway.token.usage"

// primary builds the primary record of a completed request. It does not number the request, derive
// records or update the tool-result baseline. It reports false when a successful response lacks complete
// usage or a failed one carries no limit evidence. The identity and the MCP servers it returns are what the
// derived records need; servers is empty unless the record reports token usage. The caller holds b.mu.
func (b *Builder) primary(req Request, ev *requestEvidence) (Record, identity, []serverMeasure, bool) {
	now := b.options.Now()
	limits := limitEvidence(req.ResponseHeaders, now)
	a := make(map[string]any)
	event := "gateway.limit.signal"
	success := req.StatusCode >= 200 && req.StatusCode < 300
	var servers []serverMeasure
	if success {
		raw := req.WireUsage
		if raw.Kind == 0 {
			raw = legacyWire(req.Usage)
		}
		exact := exactWireUsage(raw)
		if req.Incomplete || req.Overflowed || exact["input_tokens"] == nil && exact["output_tokens"] == nil && exact["cache_read_tokens"] == nil && exact["cache_creation_tokens"] == nil {
			return Record{}, identity{}, nil, false
		}
		maps.Copy(a, exact)
		counts, measures := ev.counts, slices.Clone(ev.servers) // attribute fills and reorders the servers in place.
		if !ev.fixed {
			counts, measures = measureInput(req.Body, req.BodyBytes)
		}
		for k, v := range counts {
			a[k] = v
		}
		servers = attribute(a, counts, measures)
		event = tokenUsageEvent
	} else if len(limits) == 0 {
		return Record{}, identity{}, nil, false
	}
	key := identity{identifierHeader(req.Headers, "x-claude-code-session-id"), identifierHeader(req.Headers, "x-claude-code-agent-id"), identifierHeader(req.Headers, "x-claude-code-parent-agent-id")}
	r := b.record(event, "gateway-usage-", now)
	rid := cmp.Or(identifierHeader(req.ResponseHeaders, "request-id"), identifierHeader(req.ResponseHeaders, "x-request-id"), identifierHeader(req.ResponseHeaders, "x-codex-request-id"))
	src := "response_header"
	if rid == "" {
		rid = safeID(req.MessageID)
		src = "message_id"
	}
	if rid == "" {
		if !ev.fixed {
			ev.fallbackID = b.id("gateway-request-")
		}
		rid = ev.fallbackID
		src = "generated"
	}
	r.add("request_id", nullable(rid))
	r.add("request_id_source", src)
	r.add("client_request_id", nullable(cmp.Or(identifierHeader(req.Headers, "x-claude-code-request-id"), identifierHeader(req.Headers, "x-request-id"))))
	r.add("session_id", nullable(key.session))
	r.add("project_name", nullable(b.projectName(key.session)))
	elapsed := req.Elapsed
	if ev.fixed {
		// The source measures the duration after the project lookup.
		elapsed = time.Since(ev.started)
	}
	r.add("agent_id", nullable(key.agent))
	r.add("parent_agent_id", nullable(key.parent))
	role := "orchestrator"
	if key.agent != "" {
		role = "executor"
	}
	r.add("agent_role", role)
	route, response := safeID(cmp.Or(req.EffectiveModel, req.RouteModel)), safeID(req.ResponseModel)
	if req.Backend == "anthropic" {
		route, response = response, route
	}
	r.add("model", nullable(cmp.Or(route, response)))
	r.add("requested_model", nullable(safeID(cmp.Or(req.RequestedModel, req.SelectedModel))))
	r.add("backend", nullable(safeID(req.Backend)))
	var effort any
	switch req.Effort {
	case "low", "medium", "high", "xhigh", "max":
		effort = req.Effort
	}
	r.add("effort", effort)
	r.add("via", nullable(safeID(req.Via)))
	r.add("ticket_ref", nullable(safeID(req.TicketRef)))
	r.add("status", statusClass(req.StatusCode))
	var code, mode, estimator, cache, sequence any
	if req.StatusCode != 0 {
		code = req.StatusCode
	}
	r.add("status_code", code)
	if req.ResponseMode == "json" || req.ResponseMode == "sse" {
		mode = req.ResponseMode
	}
	r.add("response_mode", mode)
	if success {
		estimator = "utf8_bytes_div_4_normalized"
		cache = "prefix_order_estimate"
	}
	r.add("token_estimator", estimator)
	r.add("cache_attribution", cache)
	if ev.sequence != nil && *ev.sequence <= maxSafeInteger {
		sequence = *ev.sequence
	}
	r.add("request_sequence", sequence)
	r.add("duration_ms", float64(elapsed)/float64(time.Millisecond))
	r.add("response_body_bytes", req.ResponseBytes)
	for _, limit := range limits {
		r.add(limit.key, limit.value)
	}
	if success {
		r.take(a, "request_body_bytes", "input_system_bytes", "input_tools_bytes", "input_native_tools_bytes", "input_mcp_tools_bytes", "input_messages_bytes", "input_first_message_bytes", "input_history_bytes", "input_tool_results_bytes")
		r.take(a, "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "cache_creation_5m_tokens", "cache_creation_1h_tokens", "thinking_tokens", "server_tool_use_count", "web_search_requests", "web_fetch_requests", "code_execution_requests", "tool_search_requests")
		r.take(a, "context_tokens", "input_tools_tokens", "input_native_tools_tokens", "input_mcp_tools_tokens")
		for _, s := range servers {
			r.add("input_mcp_tools_"+s.name+"_tokens", s.tokens)
		}
		r.take(a, "input_system_tokens", "input_first_message_tokens", "input_history_tokens", "input_tool_results_tokens", "cache_read_tools_tokens", "cache_read_system_tokens", "cache_read_first_message_tokens", "cache_read_history_tokens", "fresh_tools_tokens", "fresh_system_tokens", "fresh_first_message_tokens", "fresh_history_tokens")
	}
	if !ev.fixed {
		ev.traceID, ev.spanID = b.trace(req.Headers)
	}
	r.TraceID, r.SpanID = ev.traceID, ev.spanID
	return r, key, servers, true
}

// footprints derives one record for each measured MCP server of a token usage record.
func (b *Builder) footprints(primary Record, servers []serverMeasure) []Record {
	var records []Record
	for _, s := range servers {
		if len(s.name) > 64 {
			continue
		}
		foot := b.derived(primary, "gateway.mcp.footprint", "gateway-mcp-footprint-")
		foot.add("mcp_server", s.name)
		foot.add("input_mcp_tools_tokens", s.tokens)
		records = append(records, foot)
	}
	return records
}

func (b *Builder) record(event, prefix string, now time.Time) Record {
	r := Record{EventName: event, ObservedAt: now, Attributes: make(map[string]any)}
	r.add("source", "codex_gateway")
	r.add("source_event_id", b.id(prefix))
	r.add("source_schema", "gateway-usage-v1")
	r.add("event.name", event)
	r.add("event_name", event)
	return r
}

func (b *Builder) derived(primary Record, event, prefix string) Record {
	r := b.record(event, prefix, primary.ObservedAt)
	r.TraceID, r.SpanID = primary.TraceID, primary.SpanID
	r.take(primary.Attributes, "request_id", "client_request_id", "session_id", "project_name", "agent_id", "parent_agent_id", "agent_role", "model", "requested_model", "backend", "effort", "via", "ticket_ref", "request_sequence")
	return r
}
