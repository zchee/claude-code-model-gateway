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
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"uuid"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
)

// exportTimeout bounds one collector post, including draining its reply.
const exportTimeout = 500 * time.Millisecond

// Options supplies the environment, resolved audit path, diagnostics, clock and entropy.
type Options struct {
	Lookup      config.LookupEnv
	LogPath     string
	Diagnostics io.Writer
	Now         func() time.Time
	Random      io.Reader
}

// Recorder owns local route logging and asynchronous trace delivery for one worker.
// Its methods may be called from different goroutines.
type Recorder struct {
	now         func() time.Time
	logEnabled  bool
	logPath     string
	endpoint    *url.URL
	client      *http.Client
	logMu       sync.Mutex // serializes audit appends and their diagnostics
	diagnostics io.Writer
	randomMu    sync.Mutex
	random      io.Reader
	pending     sync.WaitGroup
}

// NewRecorder creates a recorder from the worker's isolated configuration.
// A nil Now uses the wall clock, a nil Random uses crypto/rand, and nil Diagnostics are discarded.
// An empty LogPath writes no audit entries.
func NewRecorder(options Options) *Recorder { return newRecorder(options, nil) }

// A nil dial function uses the standard network dialer.
func newRecorder(options Options, dialContext func(context.Context, string, string) (net.Conn, error)) *Recorder {
	r := &Recorder{
		now:         options.Now,
		logEnabled:  config.RequestLog.Get(options.Lookup),
		logPath:     options.LogPath,
		endpoint:    config.TraceURL(options.Lookup),
		diagnostics: options.Diagnostics,
		random:      options.Random,
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.diagnostics == nil {
		r.diagnostics = io.Discard
	}
	if r.random == nil {
		r.random = rand.Reader
	}
	if r.endpoint != nil {
		if dialContext == nil {
			dialContext = (&net.Dialer{}).DialContext
		}
		// One connection per post, as the source's agentless request: no proxy, no redirects,
		// no compression negotiation and no idle connection left behind.
		r.client = &http.Client{
			Transport:     &http.Transport{DialContext: dialContext, DisableKeepAlives: true, DisableCompression: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return r
}

// LogEntry holds only the allowed route and dispatch metadata for the local audit file.
type LogEntry = outcome.Audit

// LogRoute appends one audit entry using the request path and fallback session header.
// The entry never contains the query, body or other headers. A write failure is reported to
// Diagnostics by its system error name only.
func (r *Recorder) LogRoute(request *http.Request, entry LogEntry) {
	if !r.logEnabled || r.logPath == "" {
		return
	}
	var path string
	sessionID := entry.SessionID
	if request != nil {
		path, _, _ = strings.Cut(cmp.Or(request.RequestURI, request.URL.RequestURI()), "?")
		if sessionID == "" {
			sessionID, _ = headerValue(request.Header, "X-Claude-Code-Session-Id")
		}
	}
	model := jsonx.WireValue{Kind: 'n'}
	if entry.Model != nil {
		model = wireString(*entry.Model)
	}
	row := object(member("at", wireString(r.now().UTC().Format("2006-01-02T15:04:05.000Z"))), member("backend", wireString(entry.Backend)), member("model", model), member("path", wireString(path)))
	for _, field := range [...]struct{ key, value string }{
		{"via", entry.Via},
		{"effort", entry.Effort},
		{"sessionId", sessionID},
		{"agentId", entry.AgentID},
		{"parentAgentId", entry.ParentAgentID},
		{"inheritedFromAgentId", entry.InheritedFromAgentID},
		{"sessionSource", entry.SessionSource},
	} {
		if field.value != "" {
			row.Members = append(row.Members, member(field.key, wireString(field.value)))
		}
	}
	if entry.MarkersLength != nil {
		row.Members = append(row.Members, member("markersLength", jsonx.WireValue{Kind: '0', Number: float64(*entry.MarkersLength)}))
	}
	line, err := jsonx.EncodeWire(row)
	if err != nil {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	if err := appendLine(r.logPath, append(line, '\n')); err != nil {
		_, _ = fmt.Fprintf(r.diagnostics, "model-gateway: could not write request route log: %s\n", errorCode(err))
	}
}

// appendLine creates the log's directory and file if needed and appends one complete record.
func appendLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Enabled reports whether a local route span destination is configured.
func (r *Recorder) Enabled() bool { return r.endpoint != nil }

// Span represents one client request, finished once at terminal completion even across internal retries.
// The zero Span, and a Span started while tracing is disabled, records nothing.
type Span struct {
	recorder     *Recorder
	finished     atomic.Bool
	start        time.Time
	traceID      string
	spanID       string
	parentSpanID string
	routeID      string
	sessionID    string
	linked       bool
	flags        int
}

// Start records the start instant and incoming trace and session identity.
func (r *Recorder) Start(headers http.Header) *Span {
	if r.endpoint == nil {
		return &Span{}
	}
	s := &Span{}
	if trace, parent, flags, ok := parseTraceparent(headers); ok {
		s.traceID, s.parentSpanID, s.flags, s.linked = trace, parent, flags, true
	}
	var traceID [16]byte
	var spanID [8]byte
	var routeID uuid.UUID
	r.randomMu.Lock()
	ok := s.linked || readFull(r.random, traceID[:])
	ok = ok && readFull(r.random, spanID[:]) && readFull(r.random, routeID[:])
	r.randomMu.Unlock()
	if !ok {
		return &Span{}
	}
	if !s.linked {
		s.traceID = hex.EncodeToString(traceID[:])
	}
	s.spanID = hex.EncodeToString(spanID[:])
	routeID[6] = routeID[6]&0x0f | 0x40
	routeID[8] = routeID[8]&0x3f | 0x80
	s.routeID = routeID.String()
	session, _ := headerValue(headers, "X-Claude-Code-Session-Id")
	s.sessionID = safeMetadataID(session)
	s.recorder = r
	s.start = r.now()
	return s
}

func readFull(r io.Reader, b []byte) bool { _, err := io.ReadFull(r, b); return err == nil }

// Route describes the resolved model and classification at response completion.
type Route = outcome.Route

// Compaction contains safe outcome labels and measured token counts; unset counts are omitted.
type Compaction = outcome.Compaction

// Overflow holds only the classifier's diagnostic labels and numeric measurements.
type Overflow = outcome.Overflow

// Finish queues the final span once without waiting for the collector's reply.
// A zero status code denotes the absence of an HTTP response. Later calls are ignored.
func (s *Span) Finish(route Route, statusCode int, statusOverride string) {
	if s == nil || s.recorder == nil || !s.finished.CompareAndSwap(false, true) {
		return
	}
	r := s.recorder
	elapsed := r.now().Sub(s.start)
	startNanos := s.start.UnixMilli() * int64(time.Millisecond)
	status := routeStatus(statusCode, statusOverride)
	attrs := s.attributes(route, status, statusCode, float64(elapsed)/float64(time.Millisecond))
	span := object(member("traceId", wireString(s.traceID)), member("spanId", wireString(s.spanID)))
	if s.parentSpanID != "" {
		span.Members = append(span.Members, member("parentSpanId", wireString(s.parentSpanID)))
	}
	if s.linked {
		span.Members = append(span.Members, member("flags", number(float64(s.flags))))
	}
	end := wireString(strconv.FormatInt(startNanos+elapsed.Nanoseconds(), 10))
	code := 2
	if status == "ok" {
		code = 1
	}
	span.Members = append(span.Members,
		member("name", wireString(spanName)),
		member("kind", number(2)),
		member("startTimeUnixNano", wireString(strconv.FormatInt(startNanos, 10))),
		member("endTimeUnixNano", end),
		member("attributes", attrs),
		member("events", array(object(member("timeUnixNano", end), member("name", wireString(spanName)), member("attributes", attrs)))),
		member("status", object(member("code", number(float64(code))))),
	)
	resource := object(member("attributes", array(attribute("service.name", stringValue("model-gateway")))))
	scope := object(member("scope", object(member("name", wireString("eigenwise.codex-gateway")))), member("spans", array(span)))
	payload, err := jsonx.EncodeWire(object(member("resourceSpans", array(object(member("resource", resource), member("scopeSpans", array(scope)))))))
	if err != nil {
		return
	}
	r.pending.Go(func() { r.post(payload) })
}

// post sends one span; every failure, including a timeout or a refused connection, is silent.
func (r *Recorder) post(payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// An empty value suppresses Go's default User-Agent; the source sends none.
	req.Header.Set("User-Agent", "")
	resp, err := r.client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// Wait waits for pending trace exports after all spans have finished.
func (r *Recorder) Wait() { r.pending.Wait() }

const spanName = "codex_gateway.route"

var (
	tracePattern    = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(-[0-9a-f-]+)?$`)
	metadataPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/\[\]-]*$`)
	backendPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	efforts         = set("low", "medium", "high", "xhigh", "max")
	vias            = set("direct", "dispatch", "dispatch-cached")
	outcomes        = set("completed", "aborted", "incomplete", "empty_summary", "upstream_error", "unknown_error")
	errorCodes      = set("invalid_request_error", "authentication_error", "permission_error", "not_found_error", "request_too_large", "rate_limit_error", "billing_error", "websocket_missing_terminal", "upstream_http_error", "unknown_error")
)

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

// headerValue joins repeated fields with ", ", as Node presents a repeated request header.
func headerValue(h http.Header, name string) (string, bool) {
	values := h.Values(name)
	return strings.Join(values, ", "), len(values) != 0
}

// parseTraceparent accepts a W3C trace context whose version is not ff, whose version 00 carries
// no suffix, and whose trace and parent identifiers are not all zero.
func parseTraceparent(h http.Header) (trace, parent string, flags int, ok bool) {
	value, present := headerValue(h, "Traceparent")
	if !present {
		return "", "", 0, false
	}
	m := tracePattern.FindStringSubmatch(config.JSTrim(value))
	if m == nil || m[1] == "ff" || m[1] == "00" && m[5] != "" || strings.Trim(m[2], "0") == "" || strings.Trim(m[3], "0") == "" {
		return "", "", 0, false
	}
	n, _ := strconv.ParseUint(m[4], 16, 8)
	return m[2], m[3], int(n), true
}

func safeMetadataID(s string) string {
	if len(s) < 1 || len(s) > 128 || !metadataPattern.MatchString(s) {
		return ""
	}
	return s
}

func allowed(value string, values map[string]bool) string {
	if values[value] {
		return value
	}
	return ""
}

func routeStatus(code int, override string) string {
	switch {
	case override != "":
		return override
	case code == 0:
		return "upstream_error"
	case code >= 500:
		return "server_error"
	case code >= 400:
		return "client_error"
	default:
		return "ok"
	}
}

// attributes returns the span attributes in the source's order, omitting absent values.
func (s *Span) attributes(route Route, status string, statusCode int, durationMillis float64) jsonx.WireValue {
	attrs := array()
	str := func(key, value string) {
		if value != "" {
			attrs.Elements = append(attrs.Elements, attribute(key, stringValue(value)))
		}
	}
	num := func(key string, value float64) {
		attrs.Elements = append(attrs.Elements, attribute(key, numeric(value)))
	}
	boolean := func(key string, value bool) {
		kind := jsontext.Kind('f')
		if value {
			kind = 't'
		}
		attrs.Elements = append(attrs.Elements, attribute(key, object(member("boolValue", jsonx.WireValue{Kind: kind}))))
	}
	str("source", "codex-gateway")
	str("source_event_id", s.routeID)
	str("source_schema", "1")
	str("event_name", spanName)
	str("route_id", s.routeID)
	str("trace_id", s.traceID)
	str("span_id", s.spanID)
	str("parent_span_id", s.parentSpanID)
	boolean("trace_linked", s.linked)
	str("session_id", s.sessionID)
	str("selected_model", safeMetadataID(route.SelectedModel))
	str("effective_model", safeMetadataID(route.EffectiveModel))
	if backendPattern.MatchString(route.Backend) {
		str("backend", route.Backend)
	}
	str("effort", allowed(route.Effort, efforts))
	boolean("fallback", route.Fallback)
	str("via", allowed(route.Via, vias))
	str("status", status)
	if statusCode != 0 {
		num("status_code", float64(statusCode))
	}
	if c := route.Compaction; c != nil {
		str("compaction_outcome", cmp.Or(allowed(c.Outcome, outcomes), "unknown_error"))
		if c.UpstreamStatus != nil {
			num("upstream_status_code", float64(*c.UpstreamStatus))
		}
		if c.TerminalCode == "message_stop" {
			str("compaction_terminal_code", c.TerminalCode)
		}
		str("compaction_error_code", allowed(c.ErrorCode, errorCodes))
		for _, count := range [...]struct {
			key   string
			value *float64
		}{{"compaction_input_tokens", c.InputTokens}, {"compaction_output_tokens", c.OutputTokens}, {"compaction_cache_read_tokens", c.CacheReadTokens}, {"compaction_cache_creation_tokens", c.CacheCreationTokens}} {
			if count.value != nil && *count.value >= 0 && !math.IsInf(*count.value, 0) {
				num(count.key, *count.value)
			}
		}
	}
	if o := route.Overflow; o != nil {
		str("overflow_phase", o.Phase)
		str("overflow_refused_by", o.RefusedBy)
		num("overflow_request_bytes", float64(o.RequestBytes))
		if o.Tokens != nil {
			num("overflow_tokens", *o.Tokens)
		}
		str("overflow_tokens_from", o.TokensFrom)
		if o.LimitTokens != nil {
			num("overflow_limit_tokens", *o.LimitTokens)
		}
	}
	num("duration_ms", durationMillis)
	return attrs
}

func wireString(s string) jsonx.WireValue {
	return jsonx.WireValue{Kind: '"', Text: utf16.Encode([]rune(s))}
}

func member(key string, v jsonx.WireValue) jsonx.Member {
	return jsonx.Member{Name: utf16.Encode([]rune(key)), Value: v}
}

func object(m ...jsonx.Member) jsonx.WireValue   { return jsonx.WireValue{Kind: '{', Members: m} }
func array(v ...jsonx.WireValue) jsonx.WireValue { return jsonx.WireValue{Kind: '[', Elements: v} }
func number(n float64) jsonx.WireValue           { return jsonx.WireValue{Kind: '0', Number: n} }

func stringValue(s string) jsonx.WireValue { return object(member("stringValue", wireString(s))) }

// attribute builds an OTLP key-value pair from an already typed OTLP value.
func attribute(key string, value jsonx.WireValue) jsonx.WireValue {
	return object(member("key", wireString(key)), member("value", value))
}

// numeric spells an integral number as an OTLP intValue string and any other number, including a
// non-finite one that JSON encodes as null, as a doubleValue.
func numeric(n float64) jsonx.WireValue {
	if !math.IsInf(n, 0) && n == math.Trunc(n) {
		raw, _ := jsonx.EncodeWire(number(n))
		return object(member("intValue", wireString(string(raw))))
	}
	return object(member("doubleValue", number(n)))
}
