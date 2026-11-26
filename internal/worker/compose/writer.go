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
	"errors"
	"net/http"
	"strconv"
)

// clientWriter is the only writer the branches of one request reach the client through: request
// assembly's local refusals, the sentry's 413, reply, the relays, the compaction guard and the Grok
// branch. Each WriteHeader, Write and flush checks, without blocking, that the client is still there
// immediately before it runs; for a client that has left it does nothing, Header returns a map of its
// own and Write reports the bytes as written. What a branch records about the request does not depend
// on it. It deliberately has no Unwrap, so a ResponseController cannot reach past the check.
//
// It also observes delivery: whether the response head was committed and with which declared
// Content-Length, how many body bytes the client writer accepted, whether a write or a flush to the
// client failed, and whether an output operation was dropped because the client had left. Like the
// http.ResponseWriter it wraps, it is used by one goroutine at a time.
type clientWriter struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	done <-chan struct{}

	committed bool  // a status, a body byte or a flush reached the client
	declared  int64 // the Content-Length the committed head declared, or -1 for none
	written   int64 // body bytes the client writer accepted
	failed    bool  // a write or a flush to the client failed
	dropped   bool  // an output operation was dropped because the client had left
	discard   http.Header
}

// newClientWriter gates w on done, the client context's done channel.
func newClientWriter(w http.ResponseWriter, done <-chan struct{}) *clientWriter {
	return &clientWriter{w: w, rc: http.NewResponseController(w), done: done, declared: -1}
}

// gone reports whether the client has left; an output operation that finds it gone is dropped.
func (c *clientWriter) gone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// commit records the head being committed by the operation about to run, with the Content-Length
// its header map declares at that moment.
func (c *clientWriter) commit() {
	if c.committed {
		return
	}
	c.committed = true
	if n, err := strconv.ParseInt(c.w.Header().Get("Content-Length"), 10, 64); err == nil && n >= 0 {
		c.declared = n
	}
}

// Header returns the client's header map while the client is there, and a private map after.
func (c *clientWriter) Header() http.Header {
	if c.gone() {
		if c.discard == nil {
			c.discard = make(http.Header)
		}
		return c.discard
	}
	return c.w.Header()
}

// WriteHeader sends the status line unless the client has left. An informational status does not
// commit the response.
func (c *clientWriter) WriteHeader(code int) {
	if c.gone() {
		c.dropped = true
		return
	}
	if code >= http.StatusOK {
		c.commit()
	}
	c.w.WriteHeader(code)
}

// Write sends b unless the client has left. A write refused because the status allows no body is not
// a delivery failure.
func (c *clientWriter) Write(b []byte) (int, error) {
	if c.gone() {
		c.dropped = true
		return len(b), nil
	}
	c.commit()
	n, err := c.w.Write(b)
	c.written += int64(n)
	if err != nil && !errors.Is(err, http.ErrBodyNotAllowed) {
		c.failed = true
	}
	return n, err
}

// FlushError pushes written bytes to the client unless it has left. A client writer that cannot
// flush reports http.ErrNotSupported, which is not a delivery failure.
func (c *clientWriter) FlushError() error {
	if c.gone() {
		c.dropped = true
		return nil
	}
	// A flush commits a head that was not written yet.
	c.commit()
	err := c.rc.Flush()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.failed = true
	}
	return err
}

// Flush is FlushError for callers that assert http.Flusher.
func (c *clientWriter) Flush() { _ = c.FlushError() }

// delivered reports whether the branch handed its whole response to the client, the Go reading of
// the source's response 'finish' event (lib/request-worker.js:1824-1825): the head was committed,
// no write or flush failed and no output operation was dropped for a departed client, and either
//   - the head declared a Content-Length and exactly that many body bytes were accepted (a declared
//     length of zero is complete at the head), so a client that leaves afterwards does not undo the
//     delivery, as the source finishes on 'finish' once res.end(body) was flushed; or
//   - the response declared no length (a stream) and the client had not left when the branch
//     returned, as the source emits no 'finish' when end() meets a destroyed socket. A relay that
//     stops writing because its client left writes nothing more, so its client is gone by then.
//
// No final flush is made here: the span and the usage record are queued before net/http completes
// the response, and a flush would hand the client the end of the response first. A failure of the
// bytes net/http still buffers after the handler returns is therefore not observed.
func (c *clientWriter) delivered() bool {
	if !c.committed || c.failed || c.dropped {
		return false
	}
	if c.declared >= 0 && c.written == c.declared {
		return true
	}
	return c.declared < 0 && !c.gone()
}
