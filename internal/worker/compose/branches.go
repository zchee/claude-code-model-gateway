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

package compose

import (
	"context"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/grok"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
	"github.com/zchee/claude-code-model-gateway/internal/usage"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/reply"
	"github.com/zchee/claude-code-model-gateway/internal/worker/route"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sse"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const (
	messagesPath  = "/v1/messages"
	sessionHeader = "X-Claude-Code-Session-Id"
	agentHeader   = "X-Claude-Code-Agent-Id"
)

// admitFunc is the worker root's request-bound admission callback.
type admitFunc = func(outcome.Route, *outcome.Audit)

// proxy prepares a proxy request, admits it, applies the request-side sentry to the original bytes
// and forwards the prepared bytes, through the compaction guard when the request is eligible
// (lib/request-worker.js:2430-2588).
func (c *composer) proxy(client http.ResponseWriter, r *http.Request, snap route.Snapshot, d route.Decision, original []byte, admit admitFunc) (outcome.Result, error) {
	w := newClientWriter(client, r.Context().Done())
	prep, err := c.opts.ProxyReq.Prepare(w, r, snap, d, original)
	if err != nil {
		return outcome.Result{}, err
	}
	if prep.LocalStatus != 0 {
		// A local refusal is neither counted nor admitted; an unbound dispatch carries its log-only audit.
		return outcome.Result{Route: prep.Route, Status: prep.LocalStatus, StatusOverride: prep.Reason, Audit: prep.Audit}, nil
	}
	admit(prep.Route, prep.Audit)

	path := pathOnly(r)
	provider := prep.Route.Backend
	session := prep.SessionKey
	if !c.opts.SentryEnabled {
		session = ""
	}
	if session != "" && path == messagesPath {
		if cross, ok := c.opts.Sentry.Check(session, prep.Target, prep.Original); ok {
			res, err := reply.WriteSentry(w, prep.Original, backendOf(snap, prep.Route.EffectiveModel, provider), cross)
			return settle(prep.Route, res), err
		}
	}
	c.recordHighWater(r, len(prep.Body))

	var capture *usage.Capture
	// Route tracing alone also needs the capture: the guard reads its token counts for the span.
	if path == messagesPath && (c.opts.Usage.Enabled() || c.opts.Telemetry.Enabled()) {
		payload, _ := jsonx.DecodeWire(prep.Body)
		req := usage.Request{
			Body:           payload,
			BodyBytes:      int64(len(prep.Body)),
			Headers:        r.Header,
			RequestedModel: prep.Route.SelectedModel,
			EffectiveModel: prep.Route.EffectiveModel,
			Backend:        provider,
			Effort:         prep.Route.Effort,
			Via:            prep.Route.Via,
		}
		if prep.Ticket != nil {
			req.TicketRef = *prep.Ticket
		}
		capture = c.opts.Usage.Start(req)
	}

	model := prep.Route.EffectiveModel
	send := c.send(upstream.Request{
		Target:        upstream.TargetProxy,
		BaseURL:       c.opts.ProxyBaseURL,
		Method:        r.Method,
		RequestURI:    requestTarget(r),
		ClientHeaders: r.Header,
		Body:          prep.Body,
	})
	req := reply.Request{
		Provider:        provider,
		AdvertisedModel: prep.Route.SelectedModel,
		BackendModel:    model,
		Session:         session,
		CommandPath:     c.opts.CommandPath,
		Origin:          c.proxyOrigin,
		Body:            prep.Body,
		FilterPlanTools: c.filterPlanTools,
		SentryEnabled:   c.opts.SentryEnabled,
		Window:          snap.Resolver().CodexWindow(model),
		Target:          prep.Target,
		Sentry:          c.opts.Sentry,
		Upstream:        c.opts.UpstreamState,
		Retries:         c.retries,
		RetryDelay:      c.retryDelay,
		Usage:           optional[reply.UsageCapture](capture),
		Diagnostics:     c.diagnostics,
	}
	// The prepared body is classified once; the guard's eligibility reuses the classification.
	compaction := compactguard.IsCompactionRequest(prep.Body)
	stream := sse.ProxyOptions{
		Provider:          provider,
		AdvertisedModel:   prep.Route.SelectedModel,
		BackendModel:      model,
		FilterPlanTools:   c.filterPlanTools,
		IsCompaction:      compaction,
		HeartbeatInterval: c.heartbeat,
		FirstEventWait:    c.firstEventWait,
		ClientDone:        r.Context().Done(),
		SessionKey:        session,
		Sentry:            c.opts.Sentry,
		SentryTarget:      prep.Target,
		Usage:             optional[sse.UsageCapture](capture),
	}
	pass := sse.PassthroughOptions{
		ProxyProvider: provider,
		SessionKey:    session,
		Sentry:        c.opts.Sentry,
		SentryTarget:  prep.Target,
	}

	var res outcome.Result
	// The conditions of compactguard.ShouldGuard, over the classification above.
	if compaction && c.opts.CompactGuard.Settings.StreamGuard && provider != "" && path == messagesPath {
		// Every attempt of a guarded request reports its failures as compaction outcomes.
		req.Compaction = true
		stream.OnOverflow = overflowResponder(req)
		res, err = c.opts.CompactGuard.Serve(r.Context(), w, compactguard.Request{
			ProxyProvider:  provider,
			PathOnly:       path,
			UpstreamOrigin: c.proxyOrigin,
			Body:           prep.Body,
			Next: func(ctx context.Context, headWritten bool) (*upstream.Response, outcome.Result, error) {
				attempt := req
				attempt.HeadWritten = headWritten
				out, err := reply.Handle(ctx, w, attempt, send)
				// An open response that is not an event stream, a redirect in practice, is no stream
				// attempt: it is relayed once, untouched, as the unguarded path relays it, and the
				// guard returns that settled verdict as it is. Only a first attempt gets here, because
				// the reply handler answers anything but an event stream in band once a head is written.
				if out.Stream != nil && err == nil && !eventStream(out.Stream) {
					res, err := c.passthrough(w, r, attempt, out, pass)
					return nil, res, err
				}
				return out.Stream, out.Result, err
			},
			Stream: stream,
			Usage:  optional[compactguard.UsageCapture](capture),
		})
	} else {
		stream.OnOverflow = overflowResponder(req)
		res, err = c.reply(w, r, req, send, stream, pass)
	}
	c.delivered(w, err, capture)
	return settle(prep.Route, res), err
}

// anthropic forwards the original bytes to the default upstream untouched and relays its reply as
// it arrives (lib/request-worker.js:2649-2677).
func (c *composer) anthropic(client http.ResponseWriter, r *http.Request, _ route.Snapshot, _ route.Decision, original []byte, admit admitFunc) (outcome.Result, error) {
	w := newClientWriter(client, r.Context().Done())
	path := pathOnly(r)
	var (
		payload  jsonx.WireValue
		parsed   bool
		model    *string
		selected string
		effort   string
	)
	if len(original) > 0 && strings.HasPrefix(path, messagesPath) {
		if v, err := jsonx.DecodeWire(original); err == nil {
			payload, parsed = v, true
			if m := member(v, "model"); m.Kind == '"' {
				selected = text(m)
				model = &selected
			}
			if e := member(member(v, "output_config"), "effort"); e.Kind == '"' {
				effort = text(e)
			}
		}
	}
	rt := outcome.Route{SelectedModel: selected, EffectiveModel: selected, Backend: "anthropic", Effort: effort, Via: "direct"}
	// The source logs the native route without its via and effort.
	admit(rt, &outcome.Audit{Backend: "anthropic", Model: model})
	c.recordHighWater(r, len(original))

	var capture *usage.Capture
	if path == messagesPath && c.opts.Usage.Enabled() && parsed && truthy(payload) {
		capture = c.opts.Usage.Start(usage.Request{
			Body:           payload,
			BodyBytes:      int64(len(original)),
			Headers:        r.Header,
			RequestedModel: selected,
			EffectiveModel: selected,
			Backend:        "anthropic",
			Effort:         effort,
			Via:            "direct",
		})
	}
	send := c.send(upstream.Request{
		Target:        upstream.TargetAnthropic,
		BaseURL:       c.opts.AnthropicBaseURL,
		Method:        r.Method,
		RequestURI:    requestTarget(r),
		ClientHeaders: r.Header,
		Body:          original,
	})
	req := reply.Request{
		CommandPath: c.opts.CommandPath,
		Origin:      c.anthropicOrigin,
		Body:        original,
		Upstream:    c.opts.UpstreamState,
		Retries:     c.retries,
		RetryDelay:  c.retryDelay,
		Usage:       optional[reply.UsageCapture](capture),
		Diagnostics: c.diagnostics,
	}
	res, err := c.reply(w, r, req, send, sse.ProxyOptions{}, sse.PassthroughOptions{})
	c.delivered(w, err, capture)
	return settle(rt, res), err
}

// grok validates a native Grok request, admits it, applies the request-side sentry to the original
// bytes before any credential or upstream contact, and hands it to the Grok branch. The branch
// observes usage, but no usage record is emitted for Grok, as in the source
// (lib/request-worker.js:2590-2647).
func (c *composer) grok(client http.ResponseWriter, r *http.Request, snap route.Snapshot, d route.Decision, original []byte, admit admitFunc) (outcome.Result, error) {
	w := newClientWriter(client, r.Context().Done())
	payload, _ := jsonx.DecodeWire(original)
	req := grok.Request{
		RawBody:         original,
		Path:            pathOnly(r),
		AdvertisedModel: text(member(payload, "model")),
		BackendModel:    d.Model,
		SessionKey:      sentry.SessionKey(c.opts.SentryEnabled, headerValue(r.Header, sessionHeader), optionalHeader(r.Header, agentHeader)),
		SentryTarget:    c.target(snap, d.Model),
		// The credential wait and the upstream exchange end with the worker.
		ServiceContext: c.opts.ServiceContext,
	}
	prepared, refused := c.opts.Grok.Prepare(w, r, req)
	if refused {
		return prepared, nil
	}
	admit(prepared.Route, &outcome.Audit{Backend: "grok", Model: new(d.Model), Via: "direct", Effort: prepared.Route.Effort})
	if cross, ok := c.opts.Sentry.Check(req.SessionKey, req.SentryTarget, original); ok {
		res, err := reply.WriteSentry(w, original, backendOf(snap, d.Model, "grok"), cross)
		return settle(prepared.Route, res), err
	}
	c.recordHighWater(r, len(original))
	if c.opts.Usage.Enabled() {
		req.Usage = c.opts.Usage.Start(usage.Request{
			Body:           payload,
			BodyBytes:      int64(len(original)),
			Headers:        r.Header,
			RequestedModel: req.AdvertisedModel,
			EffectiveModel: d.Model,
			Backend:        "grok",
			Effort:         prepared.Route.Effort,
			Via:            "direct",
		})
	}
	res, err := c.opts.Grok.Handle(w, r, req)
	return settle(prepared.Route, res), err
}

// reply sends the prepared request through reply.Handle on the client context and hands a stream it
// leaves open to the matching relay on the service context: a successful proxy event stream to the
// proxy relay, anything else to the untouched passthrough.
func (c *composer) reply(w http.ResponseWriter, r *http.Request, req reply.Request, send upstream.Send, stream sse.ProxyOptions, pass sse.PassthroughOptions) (outcome.Result, error) {
	out, err := reply.Handle(r.Context(), w, req, send)
	if out.Stream == nil {
		return out.Result, err
	}
	if err != nil {
		if out.Stream.Body != nil {
			_ = out.Stream.Body.Close()
		}
		return out.Result, err
	}
	if req.Provider == "" || !eventStream(out.Stream) {
		return c.passthrough(w, r, req, out, pass)
	}
	res, err := sse.RelayProxy(c.opts.ServiceContext, w, out.Stream, stream)
	res.Contacted = res.Contacted || out.Contacted
	return res, err
}

// passthrough relays the open response reply.Handle left untouched on the service context, which
// takes over and closes its body. The result is the relay's, with the upstream contact and the
// compaction outcome the reply handler recorded for a status that is no success kept.
func (c *composer) passthrough(w http.ResponseWriter, r *http.Request, req reply.Request, out reply.Result, pass sse.PassthroughOptions) (outcome.Result, error) {
	pass.HeartbeatInterval = c.heartbeat
	pass.MaxObserveBytes = c.maxObserveBytes
	pass.ClientDone = r.Context().Done()
	pass.Usage = optional[sse.UsageCapture](usageOf(req))
	res, err := sse.RelayPassthrough(c.opts.ServiceContext, w, out.Stream, pass)
	res.Contacted = res.Contacted || out.Contacted
	res.Route.Compaction = out.Route.Compaction
	return res, err
}

// usageOf returns the capture a reply request carries, or nil.
func usageOf(req reply.Request) *usage.Capture {
	capture, _ := req.Usage.(*usage.Capture)
	return capture
}

// delivered exports the usage record exactly once, and only when both halves the source waits for
// have happened (lib/request-worker.js:1824-1825,2190-2207): the branch observed the whole upstream
// reply (it returned no error), and the response was delivered to a client that was still there.
func (c *composer) delivered(w *clientWriter, err error, capture *usage.Capture) {
	if capture != nil && err == nil && w.delivered() {
		c.finishUsage(capture)
	}
}

// send binds one immutable upstream request to the service context: an attempt ends with the
// worker, never with the client. The response body is handed on as a serviceBody.
func (c *composer) send(req upstream.Request) upstream.Send {
	return func(context.Context) (*upstream.Response, error) {
		resp, err := c.opts.Upstream.Do(c.opts.ServiceContext, req)
		if err == nil && resp != nil && resp.Body != nil {
			resp.Body = &serviceBody{ReadCloser: resp.Body, service: c.opts.ServiceContext, reading: make(chan struct{}, 1)}
		}
		return resp, err
	}
}

// serviceBody orders the end of a response body read against its close once the service has
// ended. The guard and the relays close the body from one goroutine while another may still be
// reading it. net/http answers a body closed before its read ended by draining the rest of it,
// bounded only by its own short timer, and only a closed connection ends the read that is in
// flight meanwhile. Once the service context has ended, the transport's cancellation closes the
// connection and ends that read by itself, so a close made then first waits for the read to return
// and net/http never starts the drain. A close made while the service runs is not delayed. Every
// body wrapped here is one the cancellation ends: the upstream client never hands out the
// connection of a response that switched protocols, whose reads no cancellation would end.
type serviceBody struct {
	io.ReadCloser
	service context.Context
	reading chan struct{} // holds a token while a Read runs
}

func (b *serviceBody) Read(p []byte) (int, error) {
	b.reading <- struct{}{}
	defer func() { <-b.reading }()
	return b.ReadCloser.Read(p)
}

func (b *serviceBody) Close() error {
	if b.service.Err() != nil {
		b.reading <- struct{}{}
		defer func() { <-b.reading }()
	}
	return b.ReadCloser.Close()
}

// overflowResponder answers an engine overflow found by the first-event peek of an LM Studio stream.
func overflowResponder(req reply.Request) sse.OverflowResponder {
	return func(w http.ResponseWriter, status int, counts sentry.Counts) (outcome.Result, error) {
		return reply.WriteEngineOverflow(w, req, status, counts)
	}
}

// target resolves the sentry policy of a Grok backend model with the backend's saved window settings.
// A row whose window cannot carry its reserve yields an inert target.
func (c *composer) target(snap route.Snapshot, model string) sentry.Target {
	resolver := snap.Resolver()
	var backend string
	if m := resolver.Resolve(model); m != nil {
		backend = m.Backend
	}
	t, err := sentry.ResolveTarget(resolver, model, c.opts.SentryEnvTrigger, c.opts.Windows.Cap(backend), c.opts.Windows[backend].CompactAt)
	if err != nil {
		return sentry.Target{}
	}
	return t
}

// recordHighWater keeps the largest forwarded request body of the client's session.
func (c *composer) recordHighWater(r *http.Request, size int) {
	if c.opts.HighWater == nil {
		return
	}
	if session := headerValue(r.Header, sessionHeader); session != "" {
		c.opts.HighWater.Record(session, int64(size))
	}
}

// settle attaches a branch's terminal diagnostics to the route the request was admitted with.
func settle(rt outcome.Route, res outcome.Result) outcome.Result {
	rt.Compaction, rt.Overflow = res.Route.Compaction, res.Route.Overflow
	res.Route = rt
	return res
}

// backendOf names the policy backend of a model, which labels a sentry refusal, or fallback when the
// model has no row.
func backendOf(snap route.Snapshot, model, fallback string) string {
	if m := snap.Resolver().Resolve(model); m != nil && m.Backend != "" {
		return m.Backend
	}
	return fallback
}

// optional converts a capture to one of the packages' usage interfaces, keeping an absent capture a
// nil interface rather than a typed nil.
func optional[I any](capture *usage.Capture) I {
	var out I
	if capture != nil {
		out = any(capture).(I)
	}
	return out
}

// eventStream reports a successful upstream response that declares an event stream.
func eventStream(resp *upstream.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300 && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

func requestTarget(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

// pathOnly is the raw request target cut at '?'.
func pathOnly(r *http.Request) string {
	path, _, _ := strings.Cut(requestTarget(r), "?")
	return path
}

// headerValue joins repeated values of a header as the source's parsed header object does.
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

// member returns the named member of an object, or the zero value.
func member(v jsonx.WireValue, name string) jsonx.WireValue {
	if v.Kind != '{' {
		return jsonx.WireValue{}
	}
	want := utf16.Encode([]rune(name))
	for _, m := range slices.Backward(v.Members) {
		if slices.Equal(m.Name, want) {
			return m.Value
		}
	}
	return jsonx.WireValue{}
}

func text(v jsonx.WireValue) string { return string(utf16.Decode(v.Text)) }

// truthy applies JavaScript's truthiness to a parsed document.
func truthy(v jsonx.WireValue) bool {
	switch v.Kind {
	case 'n', 'f', 0:
		return false
	case '"':
		return len(v.Text) > 0
	case '0':
		return v.Number != 0 && !math.IsNaN(v.Number)
	}
	return true
}
