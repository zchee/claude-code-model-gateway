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
	Usage           map[string]any
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

// Build returns the primary record and any derived records for a completed request.
func (b *Builder) Build(req Request) []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sequence++
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
			return nil
		}
		maps.Copy(a, exact)
		counts, measures := measureInput(req.Body, req.BodyBytes)
		for k, v := range counts {
			a[k] = v
		}
		servers = attribute(a, counts, measures)
		event = "gateway.token.usage"
	} else if len(limits) == 0 {
		return nil
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
		rid = b.id("gateway-request-")
		src = "generated"
	}
	r.add("request_id", nullable(rid))
	r.add("request_id_source", src)
	r.add("client_request_id", nullable(cmp.Or(identifierHeader(req.Headers, "x-claude-code-request-id"), identifierHeader(req.Headers, "x-request-id"))))
	r.add("session_id", nullable(key.session))
	r.add("project_name", nullable(b.projectName(key.session)))
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
	seq := b.sequence
	if req.Sequence != nil {
		seq = *req.Sequence
	}
	if seq <= maxSafeInteger {
		sequence = seq
	}
	r.add("request_sequence", sequence)
	r.add("duration_ms", float64(req.Elapsed)/float64(time.Millisecond))
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
	tp, _ := header(req.Headers, "traceparent")
	if m := tracePattern.FindStringSubmatch(config.JSTrim(tp)); m != nil && m[1] != "ff" && strings.Trim(m[2], "0") != "" && strings.Trim(m[3], "0") != "" {
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
		foot.add("mcp_server", s.name)
		foot.add("input_mcp_tools_tokens", s.tokens)
		records = append(records, foot)
	}
	return append(records, b.toolRecords(r, key, req.Body)...)
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
