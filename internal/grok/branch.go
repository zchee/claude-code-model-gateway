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

package grok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const (
	// threadRefusalBody is the source's stateless-backend thread refusal (lib/request-worker.js:241-253).
	threadRefusalBody = `{"type":"error","error":{"type":"invalid_request_error","message":"capability_rejected: beta_header:message-threads-2026-08-12; the model-gateway proxy and Grok backends hold no conversation state. Resend this turn with the full message history.","details":{"error_code":"thread_unsupported_request"}}}`

	outdatedVersionMessage   = "model-gateway: Grok CLI version header is outdated. Update the grok CLI, then retry."
	untranslatableResponse   = "model-gateway: Grok response could not be translated."
	untranslatableRequest    = "model-gateway: Grok request could not be translated."
	upstreamFailedPrefix     = "model-gateway: Grok upstream failed: "
	upstreamFailedNoResponse = "no response was received"
	upstreamFailedTimeout    = "Grok upstream timeout"

	// readChunk is the size of one upstream body read.
	readChunk = 32 << 10
)

// errUpstreamBroke reports an upstream response body that ended before its end; the worker root
// turns it into an aborted client response after its bookkeeping.
var errUpstreamBroke = fmt.Errorf("grok: upstream response ended early: %w", http.ErrAbortHandler)

// UsageObserver records upstream response metadata, decoded events, byte counts,
// and translated JSON bodies for one Grok request.
// The Grok branch observes usage but does not finish or emit a usage record.
type UsageObserver interface {
	SetResponse(status int, headers http.Header)
	ObserveEvent(event jsonx.WireValue)
	NoteResponseBytes(n int64)
	ObserveJSON(body []byte)
}

// BranchOptions configures a Grok HTTP branch handler without ambient reads.
type BranchOptions struct {
	// Endpoint is the Grok Responses URL. Empty uses DefaultEndpoint.
	Endpoint string
	// VersionFile is the path to version.json. Empty uses VersionFallback.
	VersionFile string
	// Auth supplies and refreshes Grok CLI access tokens.
	Auth *AuthStore
	// Client executes upstream inference HTTP requests to Endpoint.
	// Callers must supply a non-nil Client; Handle never falls back to http.DefaultTransport.
	Client *upstream.Client
	// Sentry holds session latches and learned ceilings for response-side observation.
	// When nil or SentryEnabled is false, response-side context-sentry observation is skipped.
	Sentry        *sentry.State
	SentryEnabled bool
	// Now returns the current time for fallback message and tool IDs. If nil, time.Now is used.
	Now func() time.Time
}

// Branch handles HTTP requests routed to the native Grok backend.
type Branch struct {
	endpoint      string
	versionFile   string
	auth          *AuthStore
	client        *upstream.Client
	sentry        *sentry.State
	sentryEnabled bool
	now           func() time.Time
}

// NewBranch constructs a Grok branch handler. It returns an error if opts.Client is nil.
func NewBranch(opts BranchOptions) (*Branch, error) {
	if opts.Client == nil {
		return nil, errors.New("grok: branch requires an explicit inference client")
	}
	b := &Branch{
		endpoint:      opts.Endpoint,
		versionFile:   opts.VersionFile,
		auth:          opts.Auth,
		client:        opts.Client,
		sentry:        opts.Sentry,
		sentryEnabled: opts.SentryEnabled,
		now:           opts.Now,
	}
	if b.endpoint == "" {
		b.endpoint = DefaultEndpoint
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b, nil
}

// Request carries one classified Grok inference request into Branch.Prepare and Branch.Handle.
type Request struct {
	// RawBody is the original client request body bytes before any translation.
	RawBody []byte
	// Path is the request URL path without query string (for example "/v1/messages" or "/v1/messages/count_tokens").
	Path string
	// AdvertisedModel is the picker model ID requested by the client (for example "claude-grok-4.7[1m]").
	AdvertisedModel string
	// BackendModel is the resolved Grok upstream model ID (for example "grok-4.7").
	BackendModel string
	// SessionKey is the context-sentry session/agent key computed by the worker from the incoming headers.
	SessionKey string
	// SentryTarget identifies the model key and effective sentry policy for this request.
	SentryTarget sentry.Target
	// Usage optionally records response metadata, byte counts, and translated usage.
	Usage UsageObserver
	// ServiceContext, when non-nil, is the worker-lifetime context: Handle waits for the access token
	// and runs the upstream exchange under it, so the end of the worker ends both. It never decides
	// what is written to the client. When nil, both run on the client context without its
	// cancellation.
	ServiceContext context.Context
}

// route is the route the source publishes for a Grok request (lib/request-worker.js:2604-2629).
func route(payload jsonx.WireValue, req Request) outcome.Route {
	r := outcome.Route{SelectedModel: req.AdvertisedModel, EffectiveModel: req.BackendModel, Backend: "grok", Via: "direct"}
	if effort := get(get(payload, "output_config"), "effort"); effort.Kind == '"' {
		r.Effort = string(utf16.Decode(effort.Text))
	}
	return r
}

// threadRefused follows statelessBackendThreadRefusal (lib/request-worker.js:241-253): a present
// thread member is refused unless it is an object whose type is "create".
func threadRefused(payload jsonx.WireValue) bool {
	thread := get(payload, "thread")
	if thread.Kind == 0 {
		return false
	}
	return thread.Kind != '{' || !isString(get(thread, "type"), "create")
}

// Prepare validates a Grok request before admission, auth, or upstream contact.
// On exact path "/v1/messages", an unsupported thread field is answered immediately with HTTP 400
// and Prepare returns (result, true) with resolved route metadata, Status 400, and StatusOverride
// "thread_unsupported". Otherwise Prepare writes nothing and returns (result, false) with the
// resolved route metadata populated.
func (b *Branch) Prepare(w http.ResponseWriter, r *http.Request, req Request) (outcome.Result, bool) {
	payload, err := jsonx.DecodeWire(req.RawBody)
	if err != nil {
		payload = jsonx.WireValue{}
	}
	res := outcome.Result{Route: route(payload, req)}
	if req.Path != "/v1/messages" || !threadRefused(payload) {
		return res, false
	}
	res.Status, res.StatusOverride = http.StatusBadRequest, "thread_unsupported"
	writeJSON(r.Context(), w, http.StatusBadRequest, []byte(threadRefusalBody))
	return res, true
}

// Handle serves one accepted Grok request, performing authentication, request and response
// translation, SSE streaming, response-side context-sentry observation, and usage observation.
// Admission and request-side context-sentry checks are performed by the caller before Handle;
// Handle never runs an admission check and never finishes a usage record.
//
// A Grok response that cannot be translated or encoded before headers are committed is answered
// locally with HTTP 502, error type "api_error", fixed message
// "model-gateway: Grok response could not be translated.", and StatusOverride "upstream_error".
//
// An upstream response body that stops at the upstream inactivity timeout before any client header
// was written is answered with HTTP 502, error type "api_error", message
// "model-gateway: Grok upstream failed: Grok upstream timeout", and StatusOverride
// "upstream_error". This holds for an event stream too when its headers were not yet written
// because the client had already gone; the outcome is then the same and nothing is written. Any
// other break of the body before the client headers, and every break after them, aborts the
// client response: Handle returns the outcome facts alongside an error wrapping
// http.ErrAbortHandler.
//
// Handle asks the auth store for the access token on req.ServiceContext, or, when that is nil, on a
// context without r.Context()'s cancellation, so a stalled token issuer is bounded only by the end
// of the service and the store's two 30-second request deadlines (discovery and exchange), and
// those hold only with an auth transport that honours request cancellation. When the service ends
// first, Handle stops waiting and answers as for a failed refresh; an exchange already sent still
// completes inside the store (see Wait).
//
// After r.Context() is canceled, Handle writes nothing more to w but continues consuming
// the upstream stream; the configured Client and req.ServiceContext govern its lifetime.
// outcome.Result.Cancelled is always false here and is decided by the worker root. Handle never
// panics with http.ErrAbortHandler itself.
func (b *Branch) Handle(w http.ResponseWriter, r *http.Request, req Request) (outcome.Result, error) {
	payload, decodeErr := jsonx.DecodeWire(req.RawBody)
	if decodeErr != nil {
		payload = jsonx.WireValue{}
	}
	res := outcome.Result{Route: route(payload, req)}
	client := r.Context()
	// The client's departure decides only whether anything more is written to w. Credential
	// refresh and the upstream exchange run on the service's and the Client's lifetime, as the
	// source keeps them running after the client left.
	service := req.ServiceContext
	if service == nil {
		service = context.WithoutCancel(client)
	}

	token, err := b.accessToken(service)
	if err != nil {
		res.Status, res.StatusOverride = http.StatusUnauthorized, "authentication_error"
		writeJSON(client, w, http.StatusUnauthorized, errorBody("authentication_error", units("model-gateway: "+authMessage(err))))
		return res, nil
	}

	// The source translates after reading the token and has no answer when translation throws
	// (the rejection of its async forwardGrok is unhandled); the port refuses the request locally.
	// An undecodable body is refused without translating it.
	var body []byte
	if err = decodeErr; err == nil {
		var translated jsonx.WireValue
		if translated, err = TranslateRequest(payload, req.BackendModel); err == nil {
			body, err = jsonx.EncodeWire(translated)
		}
	}
	if err != nil {
		res.Status, res.StatusOverride = http.StatusBadRequest, "invalid_request"
		writeJSON(client, w, http.StatusBadRequest, errorBody("invalid_request_error", units(untranslatableRequest)))
		return res, nil
	}

	version := VersionFallback
	if b.versionFile != "" {
		version = ReadVersion(b.versionFile)
	}
	res.Contacted = true
	resp, err := b.client.Do(service, upstream.Request{
		Target:      upstream.TargetGrok,
		BaseURL:     b.endpoint,
		GrokHeaders: Headers(token, req.BackendModel, version),
		Body:        body,
	})
	if err != nil {
		reason := upstreamFailedNoResponse
		if errors.Is(err, upstream.ErrTimeout) {
			reason = upstreamFailedTimeout
		}
		res.Status, res.StatusOverride = http.StatusBadGateway, "upstream_error"
		writeJSON(client, w, http.StatusBadGateway, errorBody("api_error", units(upstreamFailedPrefix+reason)))
		return res, nil
	}
	defer func() { _ = resp.Body.Close() }()

	res.Status = resp.StatusCode
	if req.Usage != nil {
		req.Usage.SetResponse(resp.StatusCode, resp.Header)
	}
	h := &handling{branch: b, w: w, client: client, req: req, res: res}
	switch {
	case resp.StatusCode == http.StatusUpgradeRequired:
		return h.outdated(resp)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return h.failed(resp)
	case strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream"):
		return h.stream(resp)
	default:
		return h.json(resp)
	}
}

// Wait blocks until the credential refresh in flight, if any, has ended and released its own
// context. A refresh whose token
// exchange was already sent finishes under its own deadline and stores what the issuer returned; one
// that has not reached its exchange ends once every caller waiting for it has left, which a caller
// whose ServiceContext ended has done. The worker calls Wait at shutdown, after its request
// handlers have returned, so no credential write outlives the worker.
func (b *Branch) Wait() {
	if b.auth != nil {
		b.auth.waitIdle()
	}
}

func (b *Branch) accessToken(ctx context.Context) (string, error) {
	if b.auth == nil {
		return "", ErrAuthMissing
	}
	return b.auth.AccessToken(ctx)
}

// authMessage is the sentinel text of an auth failure; the store returns nothing else, and any
// other error is reported as a failed refresh rather than by its own text.
func authMessage(err error) string {
	for _, sentinel := range [...]error{ErrAuthMissing, ErrAuthInvalid, ErrAuthCannotRefresh, ErrAuthRefreshFailed} {
		if errors.Is(err, sentinel) {
			return sentinel.Error()
		}
	}
	return ErrAuthRefreshFailed.Error()
}

// handling is the state of one upstream response being answered.
type handling struct {
	branch *Branch
	w      http.ResponseWriter
	client context.Context
	req    Request
	res    outcome.Result

	gone          bool
	headersSent   bool // the event stream's status and headers were written to the client
	compaction    bool
	compactionSet bool
}

// brokeBeforeHeaders handles an upstream body that failed before any client header was written:
// the inactivity timeout gets the source's upstream-timeout 502 (lib/request-worker.js:2350-2357),
// any other failure aborts as broke does.
func (h *handling) brokeBeforeHeaders(err error) (outcome.Result, error) {
	if !errors.Is(err, upstream.ErrTimeout) {
		return h.broke(err)
	}
	h.res.Status, h.res.StatusOverride = http.StatusBadGateway, "upstream_error"
	writeJSON(h.client, h.w, http.StatusBadGateway, errorBody("api_error", units(upstreamFailedPrefix+upstreamFailedTimeout)))
	return h.res, nil
}

// broke reports the facts of an upstream body that failed before its end, with the error the
// worker root turns into an aborted client response.
func (h *handling) broke(err error) (outcome.Result, error) {
	h.res.StatusOverride = "upstream_error"
	if errors.Is(err, io.ErrUnexpectedEOF) {
		h.res.StatusOverride = "upstream_aborted"
	}
	return h.res, errUpstreamBroke
}

// outdated answers the backend's refusal of the client version (lib/request-worker.js:2272-2281).
func (h *handling) outdated(resp *upstream.Response) (outcome.Result, error) {
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return h.brokeBeforeHeaders(err)
	}
	writeJSON(h.client, h.w, resp.StatusCode, errorBody("api_error", units(outdatedVersionMessage)))
	return h.res, nil
}

// failed relays an upstream error status with the upstream's message (lib/request-worker.js:2283-2296).
func (h *handling) failed(resp *upstream.Response) (outcome.Result, error) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return h.brokeBeforeHeaders(err)
	}
	detail := units("Grok upstream returned " + strconv.Itoa(resp.StatusCode))
	if parsed, err := jsonx.DecodeWire(raw); err == nil {
		upstreamErr := get(parsed, "error")
		candidate := jsonx.WireValue{}
		if message := get(upstreamErr, "message"); truthy(message) {
			candidate = message
		} else if upstreamErr.Kind == '"' {
			candidate = upstreamErr
		}
		// Where the source's template literal would throw on the message, the status text stays.
		if candidate.Kind != 0 {
			if text, err := toString(candidate); err == nil {
				detail = text
			}
		}
	}
	typ := "api_error"
	if resp.StatusCode == http.StatusUnauthorized {
		typ = "authentication_error"
	}
	writeJSON(h.client, h.w, resp.StatusCode, errorBody(typ, append(units("model-gateway: "), detail...)))
	return h.res, nil
}

// json translates a buffered response before any header is written (lib/request-worker.js:2330-2343).
func (h *handling) json(resp *upstream.Response) (outcome.Result, error) {
	var raw []byte
	buf := make([]byte, readChunk)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if h.req.Usage != nil {
				h.req.Usage.NoteResponseBytes(int64(n))
			}
			raw = append(raw, buf[:n]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return h.brokeBeforeHeaders(err)
		}
	}
	response, err := jsonx.DecodeWire(raw)
	if err != nil {
		response = null()
	}
	translated, err := TranslateResponseChecked(response, h.req.AdvertisedModel, h.branch.now)
	var out []byte
	if err == nil {
		out, err = jsonx.EncodeWire(translated)
	}
	if err != nil {
		h.res.Status, h.res.StatusOverride = http.StatusBadGateway, "upstream_error"
		writeJSON(h.client, h.w, http.StatusBadGateway, errorBody("api_error", units(untranslatableResponse)))
		return h.res, nil
	}
	// The sentry observes only a reply that was translated and encoded; the translation already
	// converted the usage, so it is taken from there.
	if truthy(get(response, "usage")) {
		h.observeSentry(messageDelta(get(translated, "usage")), isString(get(response, "status"), "completed") && !h.isCompaction())
	}
	if h.req.Usage != nil {
		h.req.Usage.ObserveJSON(out)
	}
	if h.client.Err() != nil {
		return h.res, nil
	}
	header := h.relayHeader(resp)
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(out)))
	h.w.WriteHeader(resp.StatusCode)
	_, _ = h.w.Write(out)
	return h.res, nil
}

// stream translates an event stream frame by frame as it arrives (lib/request-worker.js:2299-2328).
// After the client has gone, the stream is still read to its end and observed, as the source
// keeps consuming it, but nothing more is written.
func (h *handling) stream(resp *upstream.Response) (outcome.Result, error) {
	if h.client.Err() != nil {
		h.gone = true
	} else {
		h.relayHeader(resp).Set("Content-Type", "text/event-stream")
		h.w.WriteHeader(resp.StatusCode)
		h.headersSent = true
		h.flush()
	}
	translator := NewStream(h.req.AdvertisedModel, h.branch.now)
	var (
		scanner            sse.Scanner
		terminal, sawError bool
		// lastUsage is the latest usage a frame reported; the zero value means none.
		lastUsage jsonx.WireValue
	)
	buf := make([]byte, readChunk)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if h.req.Usage != nil {
				h.req.Usage.NoteResponseBytes(int64(n))
			}
			for _, frame := range scanner.Feed(buf[:n]) {
				event, ok := frameEvent(frame)
				if !ok {
					continue
				}
				// The source handles each event in one try block: when the translator throws,
				// the event marks nothing and reports no usage (lib/request-worker.js:363-377).
				failed := translator.event(event) != nil
				h.write(translator.flush())
				if failed {
					continue
				}
				typ := get(event, "type")
				terminal = terminal || isString(typ, "response.completed")
				sawError = sawError || isString(typ, "error") || isString(typ, "response.failed") || isString(typ, "response.incomplete")
				if u := get(get(event, "response"), "usage"); truthy(u) {
					converted, err := usage(u)
					if err != nil {
						continue
					}
					lastUsage = converted
					delta := messageDelta(converted)
					if h.req.Usage != nil {
						h.req.Usage.ObserveEvent(delta)
					}
					h.observeSentry(delta, false)
				}
			}
			h.flush()
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The translator's end-of-stream frames belong to an orderly end only. A client that
			// left before the headers were written is answered as the buffered paths answer.
			if !h.headersSent {
				return h.brokeBeforeHeaders(err)
			}
			return h.broke(err)
		}
	}
	h.write(translator.End())
	// Only a provider-completed stream without an error event completes the turn; the stop the
	// translator synthesizes at an end without response.completed does not.
	if terminal && !sawError {
		h.observeSentry(messageDelta(lastUsage), !h.isCompaction())
	}
	h.flush()
	return h.res, nil
}

// frameEvent returns the event of a frame as the source reads it (lib/request-worker.js:363-367):
// the first data line, trimmed; empty data, [DONE] and data JSON.parse rejects carry no event.
func frameEvent(frame sse.Frame) (jsonx.WireValue, bool) {
	data, ok := frame.Data()
	if !ok {
		return jsonx.WireValue{}, false
	}
	data = strings.TrimFunc(data, isSpace)
	if data == "" || data == "[DONE]" {
		return jsonx.WireValue{}, false
	}
	event, err := jsonx.DecodeWire([]byte(data))
	if err != nil {
		return jsonx.WireValue{}, false
	}
	return event, true
}

// messageDelta is the source's { type: 'message_delta', usage }; an undefined usage is left out.
func messageDelta(u jsonx.WireValue) jsonx.WireValue {
	return obj(member("type", str("message_delta")), member("usage", u))
}

func (h *handling) observeSentry(event jsonx.WireValue, ordinaryCompleted bool) {
	if h.branch.sentry != nil && h.branch.sentryEnabled {
		h.branch.sentry.ObserveWire(h.req.SessionKey, h.req.SentryTarget, event, ordinaryCompleted)
	}
}

// isCompaction is the source's isCompactionRequest on the original request.
func (h *handling) isCompaction() bool {
	if !h.compactionSet {
		h.compaction = sentry.RequestPhase(h.req.RawBody) == "compaction"
		h.compactionSet = true
	}
	return h.compaction
}

// relayHeader copies the upstream's relayable headers into the response, without the upstream's
// length (lib/request-worker.js:2265-2266,2298).
func (h *handling) relayHeader(resp *upstream.Response) http.Header {
	header := h.w.Header()
	for name, values := range resp.RelayHeader {
		header[name] = slices.Clone(values)
	}
	header.Del("Content-Length")
	return header
}

// write sends translated frames unless the client has gone; a failed write means it has.
func (h *handling) write(frames string) {
	if frames == "" || h.gone {
		return
	}
	if h.client.Err() != nil {
		h.gone = true
		return
	}
	if _, err := io.WriteString(h.w, frames); err != nil {
		h.gone = true
	}
}

func (h *handling) flush() {
	if h.gone {
		return
	}
	if h.client.Err() != nil {
		h.gone = true
		return
	}
	if err := http.NewResponseController(h.w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.gone = true
	}
}

// errorBody is the source's { type: 'error', error: { type, message } } with message held as
// UTF-16 units, so upstream text keeps its exact spelling.
func errorBody(typ string, message []uint16) []byte {
	// A two-level object of strings is far below the nesting limit, so encoding cannot fail.
	body, _ := jsonx.EncodeWire(obj(
		member("type", str("error")),
		member("error", obj(member("type", str(typ)), member("message", strUnits(message)))),
	))
	return body
}

// writeJSON answers with a complete JSON body unless the client has gone.
func writeJSON(client context.Context, w http.ResponseWriter, status int, body []byte) {
	if client.Err() != nil {
		return
	}
	header := w.Header()
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
