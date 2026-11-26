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

// errDeparted reports that the client left while an upstream response was being received; Handle
// turns it into a result with nothing written.
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

// readBody buffers the whole upstream body and closes it. With note, every read is counted as
// response bytes as it arrives, so a failed read still accounts for what was received. The read
// is not interrupted when the client leaves; errDeparted reports, once it has finished, that the
// client is gone and nothing more may be answered or updated for it.
func (t *turn) readBody(note bool) ([]byte, error) {
	body, err := t.readAll(note)
	if t.ctx.Err() != nil {
		return nil, errDeparted
	}
	return body, err
}

func (t *turn) readAll(note bool) ([]byte, error) {
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

// writeRelay answers with the upstream's relay headers, an optional content type and the length of body.
func (t *turn) writeRelay(status int, body []byte, contentType string) {
	h := t.w.Header()
	maps.Copy(h, t.res.RelayHeader)
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	t.w.WriteHeader(status)
	// A client that has gone away cannot be answered; the facts stand either way.
	_, _ = t.w.Write(body)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// transportFailure answers an attempt that produced no response.
func transportFailure(w http.ResponseWriter, req Request, cause error, result outcome.Result) (Result, error) {
	if req.Compaction {
		req.Usage.SetResponseComplete(false)
		result.Route.Compaction = compactionFailure(http.StatusBadGateway)
	}
	result.Status, result.StatusOverride = http.StatusBadGateway, "upstream_error"
	if req.HeadWritten {
		return Result{Result: result}, errAborted
	}
	body, err := errorDocument("api_error", units("model-gateway shim: upstream "+req.Origin+" failed: "+cause.Error()))
	if err != nil {
		return Result{Result: result}, err
	}
	writeJSON(w, http.StatusBadGateway, body)
	return Result{Result: result}, nil
}

// respond handles one upstream response. retry reports a rejected WebSocket upgrade that the
// caller may send again; its body is already closed and nothing has been written.
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
			// Streams, redirects and native Anthropic replies belong to the relay, unread and open.
			return Result{Result: t.result, Stream: t.res}, false, nil
		}
	}
	return Result{Result: t.result}, false, err
}

// committed reports a failed compaction retry inside the stream whose status line was already
// sent: one in-band error event, never a second header.
func (t *turn) committed() error {
	status := t.res.StatusCode
	t.req.Usage.SetResponse(status, t.res.Header)
	body, err := t.readBody(true)
	if errors.Is(err, errDeparted) {
		return err
	}
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
	_, _ = t.w.Write(frame)
	return nil
}

// forbidden handles a proxy 403: a rejected WebSocket upgrade is retried, any other 403 is answered once.
func (t *turn) forbidden(attempt, retries int) (Result, bool, error) {
	status := t.res.StatusCode
	body, err := t.readBody(true)
	if errors.Is(err, errDeparted) {
		return Result{}, false, err
	}
	if err != nil {
		t.fail(status, err)
		return Result{Result: t.result}, false, errAborted
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
	t.writeRelay(http.StatusServiceUnavailable, exhausted, "")
	return Result{Result: t.result}, false, nil
}

// genuineOverflow answers a proxy's own 413 with the gateway's diagnostics appended.
func (t *turn) genuineOverflow() error {
	body, err := t.readBody(true)
	if errors.Is(err, errDeparted) {
		return err
	}
	if err != nil {
		t.fail(http.StatusRequestEntityTooLarge, err)
		writeJSON(t.w, http.StatusBadGateway, []byte(earlyEnd))
		return nil
	}
	normalized, overflow := normalizeGenuine(body, t.req)
	t.result.Status, t.result.Route.Overflow = http.StatusRequestEntityTooLarge, overflow
	t.writeRelay(http.StatusRequestEntityTooLarge, rewriteCodexJSON(normalized, t.req.AdvertisedModel, false, t.req.BackendModel), "")
	return nil
}

// proxyFailure handles a proxy error other than 413, in the source's order: the empty Codex
// completion, rejection evidence, an engine or legacy context error, then the error as sent.
func (t *turn) proxyFailure() error {
	status := t.res.StatusCode
	raw, err := t.readBody(true)
	if errors.Is(err, errDeparted) {
		return err
	}
	if err != nil {
		t.fail(status, err)
		writeJSON(t.w, http.StatusBadGateway, []byte(earlyEnd))
		return nil
	}
	body := rewriteCodexJSON(raw, t.req.AdvertisedModel, false, t.req.BackendModel)
	codex := t.req.Provider == "codex"
	if codex && isEmptyCompletion(status, body) {
		contentType, answer, err := emptyEndTurn(t.req.AdvertisedModel, newMessageID(t.req.NewMessageID), requestedStream(t.req.Body))
		if err != nil {
			return err
		}
		diagnose(t.req.Diagnostics, emptyCompletionLine)
		t.result.Status = http.StatusOK
		t.w.Header().Set("Content-Type", contentType)
		t.w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
		t.w.WriteHeader(http.StatusOK)
		_, _ = t.w.Write(answer)
		return nil
	}
	if codex {
		noteCodexRejection(t.req, status, t.res.Header, body)
	}
	if counts, ok := sentry.ParseOverflow(status, body); ok {
		overflow, err := engineOverflow(t.w, t.req, status, counts)
		if err != nil {
			return err
		}
		t.result.Status, t.result.Route.Overflow = http.StatusRequestEntityTooLarge, overflow
		return nil
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
			t.writeRelay(status, doc, "application/json")
			return nil
		}
	}
	t.writeRelay(status, body, "")
	return nil
}

// passFailure relays a proxy error the earlier branches left alone (a 413 with the sentry off)
// with only the model rewritten.
func (t *turn) passFailure() error {
	status := t.res.StatusCode
	body, err := t.readBody(true)
	if errors.Is(err, errDeparted) {
		return err
	}
	if err != nil {
		t.fail(status, err)
		return errAborted
	}
	t.result.Status = status
	t.writeRelay(status, rewriteCodexJSON(body, t.req.AdvertisedModel, false, t.req.BackendModel), "")
	return nil
}

// successJSON delivers a successful proxy JSON reply. LM Studio's usage is corrected before
// usage capture and the sentry read it, and before the model rewrite and plan-tool filter.
func (t *turn) successJSON() error {
	status := t.res.StatusCode
	body, err := t.readBody(false)
	if errors.Is(err, errDeparted) {
		return err
	}
	if err != nil {
		t.fail(status, err)
		return errAborted
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
	t.writeRelay(status, rewriteCodexJSON(body, t.req.AdvertisedModel, t.req.FilterPlanTools, t.req.BackendModel), "")
	return nil
}
