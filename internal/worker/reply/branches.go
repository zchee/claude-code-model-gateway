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

package reply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/worker/outcome"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

const lmStudioProvider = "lmstudio"

// errAborted reports that the client connection must be dropped without a clean reply.
var errAborted = fmt.Errorf("reply: upstream response failed after it could no longer be answered cleanly: %w", http.ErrAbortHandler)

// errDeparted reports that the client left before its reply was written; Handle turns it into a
// result with nothing written.
var errDeparted = errors.New("reply: client left before the upstream response was answered")

const earlyEnd = `{"type":"error","error":{"type":"api_error","message":"model-gateway shim: upstream response ended early"}}`

type noUsage struct{}

func (noUsage) SetResponse(int, http.Header) {}
func (noUsage) NoteResponseBytes(int64)      {}
func (noUsage) ObserveJSON([]byte)           {}
func (noUsage) SetResponseComplete(bool)     {}

// turn holds one upstream response and the facts reported for it.
type turn struct {
	ctx    context.Context
	w      http.ResponseWriter
	req    Request
	res    *upstream.Response
	result outcome.Result
}

// gone reports that the client has left. It is checked after every callback and rewrite, before
// each output operation toward the client.
func (t *turn) gone() bool { return t.ctx.Err() != nil }

// put writes one client response: set prepares the header map, then the status and body follow.
// Each output operation has its own check of the client context, because the writer itself may
// end it: before the header map is touched, after it was prepared and immediately before
// WriteHeader, and before the body Write. head reports whether WriteHeader was called; delivered
// reports that the body Write ran and returned no error.
func put(ctx context.Context, w http.ResponseWriter, status int, body []byte, set func(http.Header)) (head, delivered bool) {
	if ctx.Err() != nil {
		return false, false
	}
	set(w.Header())
	if ctx.Err() != nil {
		return false, false
	}
	w.WriteHeader(status)
	if ctx.Err() != nil {
		return true, false
	}
	_, err := w.Write(body)
	return true, err == nil
}

// send writes one client response with put. It reports errDeparted when the client left before
// the head was committed. A committed head stands with the turn's facts whether or not its body
// followed; delivered says whether it did.
func (t *turn) send(status int, body []byte, set func(http.Header)) (delivered bool, err error) {
	head, delivered := put(t.ctx, t.w, status, body, set)
	if !head {
		return false, errDeparted
	}
	return delivered, nil
}

func setJSON(h http.Header) { h.Set("Content-Type", "application/json") }

// abort drops the client connection after a buffered body broke, unless the client already left.
func (t *turn) abort() error {
	if t.gone() {
		return errDeparted
	}
	return errAborted
}

// readBody buffers the whole upstream body and closes it. With note, every read is counted as
// response bytes as it arrives, so a failed read still accounts for what was received. The read
// is not interrupted when the client leaves: what the upstream sent is recorded either way.
func (t *turn) readBody(note bool) ([]byte, error) {
	defer func() { _ = t.res.Body.Close() }()
	var body []byte
	buf := make([]byte, 32<<10)
	for {
		n, err := t.res.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
			if note {
				t.req.Usage.NoteResponseBytes(int64(n))
			}
		}
		if errors.Is(err, io.EOF) {
			return body, nil
		}
		if err != nil {
			return body, err
		}
	}
}

// readFailure labels a body that broke before its end: a connection closed early is an abort.
func readFailure(err error) string {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "upstream_aborted"
	}
	return "upstream_error"
}

func (t *turn) fail(status int, err error) {
	t.result.Status, t.result.StatusOverride = status, readFailure(err)
}

func compactionFailure(status int) *outcome.Compaction {
	return &outcome.Compaction{Outcome: "upstream_error", ErrorCode: "upstream_http_error", UpstreamStatus: &status}
}

// writeRelay answers with the upstream's relay headers, an optional content type and the length of
// body, or reports errDeparted when the client left before the head was committed. With neither an
// upstream nor a given content type, no type is sniffed from the body; a content type the writer
// already holds is kept.
func (t *turn) writeRelay(status int, body []byte, contentType string) error {
	_, err := t.send(status, body, func(h http.Header) {
		maps.Copy(h, t.res.RelayHeader)
		if contentType != "" {
			h.Set("Content-Type", contentType)
		}
		// Without a content type from the upstream, the argument or the writer, the present but
		// empty entry keeps net/http from sniffing a type from the body.
		if _, ok := h["Content-Type"]; !ok {
			h["Content-Type"] = nil
		}
		h.Set("Content-Length", strconv.Itoa(len(body)))
	})
	return err
}

// writeEarlyEnd answers a buffered body that broke before its end, unless the client has left.
func (t *turn) writeEarlyEnd() error {
	_, err := t.send(http.StatusBadGateway, []byte(earlyEnd), setJSON)
	return err
}

// transportFailure answers an attempt that produced no response. The client context is checked
// after the capture callback, before the abort and before each output operation of the reply; a
// client that left before a head was committed gets nothing and the result carries only the
// facts it was given.
func transportFailure(ctx context.Context, w http.ResponseWriter, req Request, cause error, facts outcome.Result) (Result, error) {
	result := facts
	if req.Compaction {
		req.Usage.SetResponseComplete(false)
		result.Route.Compaction = compactionFailure(http.StatusBadGateway)
	}
	result.Status, result.StatusOverride = http.StatusBadGateway, "upstream_error"
	var body []byte
	if !req.HeadWritten {
		var err error
		body, err = errorDocument("api_error", units("model-gateway shim: upstream "+req.Origin+" failed: "+cause.Error()))
		if err != nil {
			return Result{Result: result}, err
		}
	}
	if req.HeadWritten {
		if ctx.Err() != nil {
			return Result{Result: facts}, nil
		}
		return Result{Result: result}, errAborted
	}
	if head, _ := put(ctx, w, http.StatusBadGateway, body, setJSON); !head {
		return Result{Result: facts}, nil
	}
	return Result{Result: result}, nil
}

// respond handles one upstream response. retry reports a rejected WebSocket upgrade that the
// caller may send again; its body is already closed and nothing has been written. errDeparted
// reports that the response was recorded but the client had left before anything was written.
func (t *turn) respond(attempt, retries int) (out Result, retry bool, err error) {
	status := t.res.StatusCode
	successful := status >= 200 && status < 300
	stream := successful && strings.Contains(strings.ToLower(t.res.Header.Get("Content-Type")), "text/event-stream")
	proxy, advertised := t.req.Provider != "", t.req.AdvertisedModel != ""
	switch {
	case t.req.HeadWritten && !stream:
		err = t.committed()
	case proxy && status == http.StatusForbidden:
		return t.forbidden(attempt, retries)
	default:
		t.req.Usage.SetResponse(status, t.res.Header)
		if t.req.Compaction && !successful {
			t.req.Usage.SetResponseComplete(false)
			t.result.Route.Compaction = compactionFailure(status)
		}
		switch {
		case proxy && t.req.SentryEnabled && status == http.StatusRequestEntityTooLarge:
			err = t.genuineOverflow()
		case proxy && status >= 400 && status != http.StatusRequestEntityTooLarge:
			err = t.proxyFailure()
		case advertised && status >= 400:
			err = t.passFailure()
		case advertised && successful && !stream:
			err = t.successJSON()
		default:
			// Streams, redirects and native Anthropic replies belong to the relay, unread and open,
			// also for a client that has left: the relay learns of the departure and owns the body.
			return Result{Result: t.result, Stream: t.res}, false, nil
		}
	}
	return Result{Result: t.result}, false, err
}

// committed reports a failed compaction retry inside the stream whose status line was already
// sent: one in-band error event, never a second header. The facts are recorded from whatever was
// read, also when the read broke.
func (t *turn) committed() error {
	status := t.res.StatusCode
	t.req.Usage.SetResponse(status, t.res.Header)
	body, err := t.readBody(true)
	if err != nil {
		t.fail(status, err)
	} else {
		t.result.Status = status
	}
	t.req.Usage.SetResponseComplete(false)
	t.result.Route.Compaction = compactionFailure(status)
	// The status line is gone but the learned ceiling is not.
	if t.req.Provider != "" && t.req.SentryEnabled && status == http.StatusRequestEntityTooLarge && t.req.Sentry != nil {
		t.req.Sentry.Overflow(t.req.Session, t.req.Target, true)
	}
	kind, message := "api_error", upstreamErrorMessage(body, status)
	if t.req.Provider == "codex" {
		noteCodexRejection(t.req, status, t.res.Header, body)
		if auth, ok := codexAuthFailure(body, status, t.req.CommandPath); ok {
			kind, message = "authentication_error", auth
		}
	}
	frame, err := sseErrorFrame(kind, message)
	if err != nil {
		return err
	}
	if t.gone() {
		return errDeparted
	}
	_, _ = t.w.Write(frame)
	return nil
}

// forbidden handles a proxy 403: a rejected WebSocket upgrade is retried, any other 403 is answered
// once. A rejection is recorded only from a body that was read to its end.
func (t *turn) forbidden(attempt, retries int) (Result, bool, error) {
	status := t.res.StatusCode
	body, err := t.readBody(true)
	if err != nil {
		t.fail(status, err)
		return Result{Result: t.result}, false, t.abort()
	}
	if !isUpgradeRejection(status, body) {
		codex := t.req.Provider == "codex"
		if codex {
			noteCodexRejection(t.req, status, t.res.Header, body)
		}
		t.req.Usage.SetResponse(status, t.res.Header)
		t.result.Status = status
		return Result{Result: t.result}, false, t.answerFailure(status, body, rewriteCodexJSON(body, t.req.AdvertisedModel, false, t.req.BackendModel))
	}
	if attempt < retries {
		return Result{}, true, nil
	}
	exhausted, err := upgradeExhausted(attempt + 1)
	if err != nil {
		return Result{Result: t.result}, false, err
	}
	t.req.Usage.SetResponse(http.StatusServiceUnavailable, t.res.RelayHeader)
	t.result.Status, t.result.StatusOverride = http.StatusServiceUnavailable, "upstream_error"
	return Result{Result: t.result}, false, t.writeRelay(http.StatusServiceUnavailable, exhausted, "")
}

// genuineOverflow answers a proxy's own 413 with the gateway's diagnostics appended. The ceiling
// is learned from a body read to its end, before the client is checked.
func (t *turn) genuineOverflow() error {
	body, err := t.readBody(true)
	if err != nil {
		t.fail(http.StatusRequestEntityTooLarge, err)
		return t.writeEarlyEnd()
	}
	normalized, overflow := normalizeGenuine(body, t.req)
	t.result.Status, t.result.Route.Overflow = http.StatusRequestEntityTooLarge, overflow
	return t.writeRelay(http.StatusRequestEntityTooLarge, rewriteCodexJSON(normalized, t.req.AdvertisedModel, false, t.req.BackendModel), "")
}

// proxyFailure handles a proxy error other than 413, in the source's order: the empty Codex
// completion, rejection evidence, an engine or legacy context error, then the error as sent.
func (t *turn) proxyFailure() error {
	status := t.res.StatusCode
	raw, err := t.readBody(true)
	if err != nil {
		t.fail(status, err)
		return t.writeEarlyEnd()
	}
	body := rewriteCodexJSON(raw, t.req.AdvertisedModel, false, t.req.BackendModel)
	codex := t.req.Provider == "codex"
	if codex && isEmptyCompletion(status, body) {
		contentType, answer, err := emptyEndTurn(t.req.AdvertisedModel, newMessageID(t.req.NewMessageID), requestedStream(t.req.Body))
		if err != nil {
			return err
		}
		t.result.Status = http.StatusOK
		delivered, err := t.send(http.StatusOK, answer, func(h http.Header) {
			h.Set("Content-Type", contentType)
			h.Set("Content-Length", strconv.Itoa(len(answer)))
		})
		// The line reports an empty reply that was written, so a client that left before it or a
		// failed write gets none.
		if delivered {
			diagnose(t.req.Diagnostics, emptyCompletionLine)
		}
		return err
	}
	if codex {
		noteCodexRejection(t.req, status, t.res.Header, body)
	}
	if counts, ok := sentry.ParseOverflow(status, body); ok {
		overflow, answer, err := engineOverflow(t.req, status, counts)
		if err != nil {
			return err
		}
		t.result.Status, t.result.Route.Overflow = http.StatusRequestEntityTooLarge, overflow
		_, err = t.send(http.StatusRequestEntityTooLarge, answer, overflowHeader(answer, &status))
		return err
	}
	t.result.Status = status
	return t.answerFailure(status, body, body)
}

// answerFailure relays an upstream error with its status, replacing a Codex sign-in failure
// read from detail with the sign-in advice.
func (t *turn) answerFailure(status int, detail, body []byte) error {
	if t.req.Provider == "codex" {
		if auth, ok := codexAuthFailure(detail, status, t.req.CommandPath); ok {
			doc, err := errorDocument("authentication_error", auth)
			if err != nil {
				return err
			}
			return t.writeRelay(status, doc, "application/json")
		}
	}
	return t.writeRelay(status, body, "")
}

// passFailure relays a proxy error the earlier branches left alone (a 413 with the sentry off)
// with only the model rewritten.
func (t *turn) passFailure() error {
	status := t.res.StatusCode
	body, err := t.readBody(true)
	if err != nil {
		t.fail(status, err)
		return t.abort()
	}
	t.result.Status = status
	return t.writeRelay(status, rewriteCodexJSON(body, t.req.AdvertisedModel, false, t.req.BackendModel), "")
}

// successJSON delivers a successful proxy JSON reply. LM Studio's usage is corrected before
// usage capture and the sentry read it, and before the model rewrite and plan-tool filter. Both
// observe a complete body once, whether or not the client is still there and whether or not the
// reply is then written; the sentry rearms a fired refusal for an ordinary completed turn.
func (t *turn) successJSON() error {
	status := t.res.StatusCode
	body, err := t.readBody(false)
	if err != nil {
		t.fail(status, err)
		return t.abort()
	}
	if t.req.Provider == lmStudioProvider {
		body = exclusiveLMStudioJSON(body)
	}
	t.req.Usage.ObserveJSON(body)
	completed := completedJSON(body)
	if completed && t.req.Sentry != nil {
		t.req.Sentry.ObserveJSON(t.req.Session, t.req.Target, t.req.Body, body)
	}
	t.result.Status = status
	if !completed {
		t.result.StatusOverride = "upstream_error"
	}
	return t.writeRelay(status, rewriteCodexJSON(body, t.req.AdvertisedModel, t.req.FilterPlanTools, t.req.BackendModel), "")
}
