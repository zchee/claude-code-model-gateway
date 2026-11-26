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

// Package proxyreq prepares classified proxy requests and answers local refusals.
package proxyreq

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/policy"
	"github.com/zchee/claude-code-model-gateway/internal/toolschema"
	"github.com/zchee/claude-code-model-gateway/internal/worker/dispatch"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

// Options supplies worker-lifetime dependencies; relative dispatch paths resolve against StartupDir.
type Options struct {
	StartupDir, DispatchPath string
	Lookup                   config.LookupEnv
	Now                      func() time.Time
	Windows                  policy.Windows
}

// Result describes the effective route and either the serialized upstream body or a local response.
type Result struct {
	// Original retains the client's exact bytes for the post-admission sentry check.
	Original []byte
	Body     []byte
	Route    outcome.Route
	Audit    *outcome.Audit
	// Ticket attributes usage to a dispatch ticket, except on the turn inheriting another agent's route.
	Ticket      *string
	SessionKey  string
	Target      sentry.Target
	LocalStatus int
	Reason      string
}

// Handler owns one worker's dispatch cache and shared request settings.
// A Handler must not be copied after New.
type Handler struct {
	// routesMu makes each request's dispatch decision (its explicit binding, or its own lookup, parent lookup
	// and inherited write) one step, so concurrent turns of a session end as some serial order would leave them.
	routesMu      sync.Mutex
	routes        *dispatch.Cache
	windows       policy.Windows
	sentryEnabled bool
	keepPlanTools bool
	envTrigger    float64
}

// New constructs the worker's request handler without consulting ambient paths or configuration.
func New(options Options) (*Handler, error) {
	path := options.DispatchPath
	if path != "" && !filepath.IsAbs(path) {
		// The dispatch cache keeps the directory it first opens; a path relative to the process working
		// directory would make that directory depend on where the worker happened to start.
		if !filepath.IsAbs(options.StartupDir) {
			return nil, errors.New("proxyreq: a relative dispatch state path needs an absolute startup directory")
		}
		// Lexical cleaning would let a symlink followed by ".." name a different file than the kernel opens
		// for the same spelling, so only a spelling that needs no cleaning is anchored, by concatenation.
		if !cleanRelative(path) {
			return nil, errors.New("proxyreq: a relative dispatch state path must not have a volume, an empty, \".\" or \"..\" component, or a trailing separator")
		}
		separator := string(filepath.Separator)
		if os.IsPathSeparator(options.StartupDir[len(options.StartupDir)-1]) {
			separator = ""
		}
		path = options.StartupDir + separator + path
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	lookup := options.Lookup
	// The configured maximum is a positive integer. Saturate only where it exceeds the native int: the float
	// nearest math.MaxInt on 64-bit builds is 2^63, which an int conversion cannot represent.
	maxSessions := math.MaxInt
	if limit := config.DispatchMax.Get(lookup); limit < float64(math.MaxInt) {
		maxSessions = int(limit)
	}
	return &Handler{
		routes:        dispatch.NewCache(path, config.DispatchTTL.Get(lookup), maxSessions, now),
		windows:       maps.Clone(options.Windows),
		sentryEnabled: config.Sentry.Get(lookup),
		keepPlanTools: config.KeepPlanTools.Get(lookup),
		envTrigger:    config.CompactTrigger.Get(lookup),
	}, nil
}

const (
	messagesPath = "/v1/messages"

	sessionHeader     = "X-Claude-Code-Session-Id"
	agentHeader       = "X-Claude-Code-Agent-Id"
	parentAgentHeader = "X-Claude-Code-Parent-Agent-Id"

	threadRefusal  = `{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: beta_header:message-threads-2026-08-12; the model-gateway proxy and Grok backends hold no conversation state. Resend this turn with the full message history.","details":{"error_code":"thread_unsupported_request"}}}`
	unboundRefusal = `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway: dispatch model requires exactly one [sidequest-route model=... effort=... ticket=...] marker in the conversation; redispatch the ticket"}}`
)

// Prepare writes a local 400 or returns a forwardable result with zero LocalStatus.
// The caller admits the prepared route before consulting the sentry.
// The caller supplies the original bytes and the same snapshot used to classify them.
func (h *Handler) Prepare(w http.ResponseWriter, r *http.Request, snapshot route.Snapshot, decision route.Decision, original []byte) (Result, error) {
	if decision.Branch != "proxy" || decision.Model == "" {
		return Result{}, errors.New("proxyreq: the decision does not select the proxy branch")
	}
	// The tree is private to this request, so the rewrites below edit it in place.
	payload, err := jsonx.DecodeWire(original)
	if err != nil || payload.Kind != '{' {
		return Result{}, errors.New("proxyreq: the request body is not the JSON object it was classified from")
	}
	modelAt := memberIndex(payload, "model")
	if modelAt < 0 || payload.Members[modelAt].Value.Kind != '"' {
		return Result{}, errors.New("proxyreq: the request body has no model string")
	}
	advertised := string(utf16.Decode(payload.Members[modelAt].Value.Text))
	requestedEffort, _ := effortOf(payload)
	provider, base := decision.Provider, decision.Model
	dispatched := base == "auto"
	res := Result{Original: original, Route: outcome.Route{SelectedModel: advertised, Backend: provider}}

	// Local paths are matched on the raw request target cut at '?'; only the exact messages path is a turn.
	target := r.RequestURI
	if target == "" {
		target = r.URL.RequestURI()
	}
	pathOnly, _, _ := strings.Cut(target, "?")
	if pathOnly == messagesPath && threadRefused(payload) {
		res.Route.Effort, res.Route.Via = requestedEffort, "direct"
		if dispatched {
			res.Route.Via = "dispatch"
		} else {
			res.Route.EffectiveModel = base
		}
		return refuse(w, res, "thread_unsupported", []byte(threadRefusal)), nil
	}

	model := base
	var (
		chosen      *dispatch.CachedRoute
		via         string
		id          identity
		markers     int
		inheritedID string
	)
	if dispatched {
		id = requestIdentity(r.Header, payload)
		var marked *dispatch.Route
		marked, markers = dispatch.FromWireMessages(memberValue(payload, "messages"))
		chosen, via, inheritedID = h.decideDispatch(id, marked, markers)
		if chosen == nil {
			res.Route.Via = "dispatch"
			res.Audit = id.audit(outcome.Audit{Backend: provider, Model: new(advertised), Via: "dispatch-unbound", MarkersLength: new(markers)})
			return refuse(w, res, "invalid_route", []byte(unboundRefusal)), nil
		}
		model = chosen.Model
	}

	payload.Members[modelAt].Value = wireText(model)
	if chosen != nil && chosen.Effort != nil && *chosen.Effort != "" {
		overrideEffort(&payload, *chosen.Effort)
	}
	resolveDeferredTools(&payload)
	routeVia := cmp.Or(via, "direct")
	if provider == "codex" {
		if i := memberIndex(payload, "tools"); i >= 0 {
			adapted, _, err := toolschema.Adapt(payload.Members[i].Value)
			if err != nil {
				res.Route = outcome.Route{SelectedModel: advertised, EffectiveModel: model, Backend: provider, Effort: effectiveEffort(payload, requestedEffort), Via: routeVia}
				return refuse(w, res, "invalid_tool_schema", schemaRefusal(err)), nil
			}
			payload.Members[i].Value = adapted
		}
	}
	if !h.keepPlanTools {
		removePlanTools(&payload)
	}
	body, err := jsonx.EncodeWire(payload)
	if err != nil {
		return Result{}, fmt.Errorf("proxyreq: encode prepared request: %w", err)
	}

	effort := effectiveEffort(payload, requestedEffort)
	res.Body = body
	res.Route = outcome.Route{SelectedModel: advertised, EffectiveModel: model, Backend: provider, Effort: effort, Via: routeVia}
	audit := outcome.Audit{Backend: provider, Model: new(model), Via: via, Effort: effort}
	if dispatched {
		audit.MarkersLength, audit.InheritedFromAgentID = new(markers), inheritedID
		audit = *id.audit(audit)
		if via != "dispatch-inherited" && chosen.Ticket != nil {
			res.Ticket = new(*chosen.Ticket)
		}
	}
	res.Audit = &audit
	res.SessionKey = sentry.SessionKey(h.sentryEnabled, headerValue(r.Header, sessionHeader), optionalHeader(r.Header, agentHeader))
	res.Target = h.target(snapshot.Resolver(), model)
	return res, nil
}

// decideDispatch binds or looks up a session route, returning the chosen route, its via label and any
// inherited parent agent ID. A request with more than one marker does not depend on the cache and takes no lock.
func (h *Handler) decideDispatch(id identity, marked *dispatch.Route, markers int) (*dispatch.CachedRoute, string, string) {
	if markers > 1 {
		return nil, "", ""
	}
	h.routesMu.Lock()
	defer h.routesMu.Unlock()
	switch {
	case markers == 1 && marked != nil:
		chosen := &dispatch.CachedRoute{Route: *marked}
		h.routes.Set(id.key, *chosen)
		return chosen, "dispatch", ""
	case markers == 0:
		if chosen := h.routes.Get(id.key); chosen != nil {
			return chosen, "dispatch-cached", ""
		}
		if chosen := h.routes.Get(id.parentKey); chosen != nil {
			h.routes.Set(id.key, *chosen)
			return chosen, "dispatch-inherited", id.parentAgentID
		}
	}
	return nil, "", ""
}

// target resolves the sentry policy of the effective model with the backend's saved window settings.
// A row whose backend window cannot carry its reserve yields an inert target: the request is forwarded
// to the proxy without a request-side refusal rather than failing or leaving the proxy branch.
func (h *Handler) target(resolver policy.Resolver, model string) sentry.Target {
	var backend string
	if m := resolver.Resolve(model); m != nil {
		backend = m.Backend
	}
	t, err := sentry.ResolveTarget(resolver, model, h.envTrigger, h.windows.Cap(backend), h.windows[backend].CompactAt)
	if err != nil {
		return sentry.Target{}
	}
	return t
}

// refuse answers a branch-local 400 and records it in the result; nothing is forwarded.
func refuse(w http.ResponseWriter, res Result, reason string, body []byte) Result {
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusBadRequest)
	// A failed write means the client left; the refusal is still the request's outcome.
	_, _ = w.Write(body)
	res.LocalStatus, res.Reason = http.StatusBadRequest, reason
	return res
}

// schemaRefusal formats a Codex tool schema refusal, keeping the tool name and pointer as UTF-16 units so an
// unpaired surrogate is escaped as JSON.stringify escapes it.
func schemaRefusal(err error) []byte {
	name, pointer, reason := utf16.Encode([]rune("<unnamed tool>")), utf16.Encode([]rune("/tools")), "schema-classifier-failure"
	if c, ok := errors.AsType[*toolschema.CompatibilityError](err); ok {
		if c.ToolName != nil {
			name = c.ToolName
		}
		if c.Pointer != nil {
			pointer = c.Pointer
		}
		reason = cmp.Or(c.ReasonCode, reason)
	}
	message := utf16.Encode([]rune("model-gateway: Codex tool schema compatibility refused tool "))
	message = append(message, name...)
	message = append(message, utf16.Encode([]rune(" at "))...)
	message = append(message, pointer...)
	message = append(message, utf16.Encode([]rune("; reason="+reason+". Nothing was forwarded or rerouted."))...)
	body, err := jsonx.EncodeWire(object(
		jsonx.Member{Name: units("type"), Value: wireText("error")},
		jsonx.Member{Name: units("error"), Value: object(
			jsonx.Member{Name: units("type"), Value: wireText("invalid_request_error")},
			jsonx.Member{Name: units("message"), Value: jsonx.WireValue{Kind: '"', Text: message}},
		)},
	))
	if err != nil {
		// A two-level object of strings always encodes; keep the refusal well-formed regardless.
		return []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway: Codex tool schema compatibility refused the request. Nothing was forwarded or rerouted."}}`)
	}
	return body
}

// identity is the dispatch session and agent lineage; empty keys mean the route cache is not consulted.
type identity struct {
	key, parentKey                    string
	sessionID, agentID, parentAgentID string
	source                            string
}

// requestIdentity prefers a safe session header, then the session in metadata.user_id. A parent-agent header
// without an agent ID has no own key, so such a turn never stores or reuses a route of its own.
func requestIdentity(header http.Header, payload jsonx.WireValue) identity {
	session, source := safeID(headerValue(header, sessionHeader)), "header"
	if session == "" {
		session, source = metadataSession(payload), "metadata"
	}
	if session == "" {
		return identity{}
	}
	id := identity{sessionID: session, agentID: safeID(headerValue(header, agentHeader)), parentAgentID: safeID(headerValue(header, parentAgentHeader)), source: source}
	if id.agentID != "" || id.parentAgentID == "" {
		id.key = pairKey(session, id.agentID)
	}
	if id.agentID != "" && id.parentAgentID != "" && id.agentID != id.parentAgentID {
		id.parentKey = pairKey(session, id.parentAgentID)
	}
	return id
}

// audit adds the identity's lineage to a route log entry.
func (id identity) audit(a outcome.Audit) *outcome.Audit {
	a.SessionID, a.AgentID, a.ParentAgentID, a.SessionSource = id.sessionID, id.agentID, id.parentAgentID, id.source
	return &a
}

// pairKey spells JSON.stringify([session, agent || null]). Safe IDs hold no character JSON escapes.
func pairKey(session, agent string) string {
	if agent == "" {
		return `["` + session + `",null]`
	}
	return `["` + session + `","` + agent + `"]`
}

// metadataSession reads metadata.user_id as JSON with a session_id, or, only when it is not JSON, as text whose
// session follows the last "_session_".
func metadataSession(payload jsonx.WireValue) string {
	userID := memberValue(memberValue(payload, "metadata"), "user_id")
	if userID.Kind != '"' || len(userID.Text) == 0 {
		return ""
	}
	if parsed, err := jsonx.DecodeWireUnits(userID.Text); err == nil {
		if session := memberValue(parsed, "session_id"); session.Kind == '"' {
			return safeUnits(session.Text)
		}
		return ""
	}
	marker := units("_session_")
	for i := len(userID.Text) - len(marker); i >= 0; i-- {
		if slices.Equal(userID.Text[i:i+len(marker)], marker) {
			return safeUnits(userID.Text[i+len(marker):])
		}
	}
	return ""
}

// safeUnits applies safeID to UTF-16 units; a safe ID is ASCII, so any other unit rejects it.
// The length limit is checked first so an overlong candidate is not copied.
func safeUnits(text []uint16) string {
	if len(text) < 1 || len(text) > 128 {
		return ""
	}
	b := make([]byte, len(text))
	for i, u := range text {
		if u >= 0x80 {
			return ""
		}
		b[i] = byte(u)
	}
	return safeID(string(b))
}

// safeID returns s when it is 1 to 128 characters of [a-zA-Z0-9._:/\[\]-] starting with a letter or digit.
func safeID(s string) string {
	if len(s) < 1 || len(s) > 128 {
		return ""
	}
	for i := range len(s) {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("._:/[]-", rune(c))) {
			return ""
		}
	}
	return s
}

// cleanRelative reports whether a relative path has no volume and only components other than "", "." and
// "..", so it needs no lexical cleaning; a leading or trailing separator makes an empty component.
func cleanRelative(path string) bool {
	if filepath.VolumeName(path) != "" {
		return false
	}
	start := 0
	for i := range len(path) + 1 {
		if i < len(path) && !os.IsPathSeparator(path[i]) {
			continue
		}
		if c := path[start:i]; c == "" || c == "." || c == ".." {
			return false
		}
		start = i + 1
	}
	return true
}

// headerValue joins repeated values as Node joins a repeated custom header; it is empty when absent.
func headerValue(header http.Header, name string) string {
	return strings.Join(header.Values(name), ", ")
}

// optionalHeader distinguishes an absent header (nil) from one present with an empty value.
func optionalHeader(header http.Header, name string) *string {
	if len(header.Values(name)) == 0 {
		return nil
	}
	return new(headerValue(header, name))
}
