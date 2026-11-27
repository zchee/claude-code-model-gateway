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

package relay

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"time"
)

// copyBuffer is the size of the buffer one read of a request or response body fills.
const copyBuffer = 32 << 10

// readUpload reads the whole request body within BodyLimit (lib/commands.js:2297-2323). It reports false when the
// request must not be delivered; it has then answered whatever the client is owed: the fixed 400 for a read failure
// while the client is there, the 413 for an oversized upload, or nothing for a departed client.
func readUpload(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	ctx := r.Context()
	var body []byte
	var over bool
	var err error
	if r.ContentLength > BodyLimit {
		// The declared length already exceeds the bound, so the bytes are counted, not kept. The refusal still waits
		// for the byte past the bound, as the source does, so that the grace starts at the same moment.
		var n int64
		n, err = io.Copy(io.Discard, io.LimitReader(r.Body, BodyLimit+1))
		over = n > BodyLimit
		if err == nil && !over {
			// The body ended before its declared length without saying so; it is incomplete either way.
			err = io.ErrUnexpectedEOF
		}
	} else {
		body, err = readBounded(r.Body, r.ContentLength)
		over = len(body) > BodyLimit
		if err == nil && int64(len(body)) < r.ContentLength {
			// A body that ends before its declared length is an early end, whichever reader supplied it.
			err = io.ErrUnexpectedEOF
		}
	}
	switch {
	case err != nil:
		if !departed(ctx, err) {
			writeUnreadable(ctx, w)
		}
		return nil, false
	case over:
		if ctx.Err() == nil {
			refuseOversize(w, r)
		}
		return nil, false
	}
	return body, true
}

// readBounded reads body to its end or to one byte past BodyLimit. The buffer grows with the bytes that actually
// arrive, never to more than the declared length plus the byte that shows the end, nor past BodyLimit plus the byte
// that shows an excess, so that an advertised length alone allocates nothing.
func readBounded(body io.Reader, length int64) ([]byte, error) {
	ceiling := int64(BodyLimit + 1)
	if length >= 0 {
		ceiling = min(ceiling, length+1)
	}
	buf := make([]byte, 0, min(ceiling, 512))
	limited := io.LimitReader(body, BodyLimit+1)
	for {
		if len(buf) == cap(buf) {
			if int64(len(buf)) > BodyLimit {
				// The byte past the bound has arrived; the caller refuses the upload.
				return buf, nil
			}
			// A body longer than it declared still reaches the bound: past the ceiling the buffer doubles as usual.
			grown := max(int64(2*cap(buf)), 512)
			if int64(cap(buf)) < ceiling {
				grown = min(grown, ceiling)
			}
			buf = append(make([]byte, 0, min(grown, BodyLimit+1)), buf...)
		}
		n, err := limited.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}

// departed reports whether a body read failed because the client went away rather than because its upload is
// unreadable: the request context has ended, or the read ended in a reset, an early end, a cancellation or a closed
// connection, the Go forms of the source's ECONNRESET, ERR_STREAM_PREMATURE_CLOSE and ABORT_ERR
// (lib/commands.js:2316-2321). A broken pipe, and the EPROTOTYPE that macOS reports for a socket torn down under an
// operation, are the same departure seen from a connection's other direction.
func departed(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.EPROTOTYPE) ||
		errors.Is(err, net.ErrClosed)
}

// writeUnreadable answers an upload that could not be read while its client is there, without the failure's own text
// (lib/commands.js:2273-2283).
func writeUnreadable(ctx context.Context, w http.ResponseWriter) {
	h := w.Header()
	h.Set("Connection", "close")
	h.Set("X-Should-Retry", "false")
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(unreadableBody)))
	w.WriteHeader(http.StatusBadRequest)
	// A client that left while the head was committed gets nothing further.
	abortIfDeparted(ctx)
	_, _ = io.WriteString(w, unreadableBody)
}

// refuseOversize answers an upload above BodyLimit (lib/commands.js:2285-2295). The 413 head is flushed at once, and
// the upload is then read on until it ends, so that the client can read the answer instead of a reset, but for no
// longer than OversizeGrace: an upload that ends within the grace finishes the 413 normally, and one that is still
// running when it ends breaks the connection by http.ErrAbortHandler, without any linger of its own.
//
// The grace is a read deadline on the client connection, set through http.ResponseController, and the drain reads in
// this goroutine, so no read of the body is in progress once the handler ends: the public listener forbids reading the
// body after that. A response that cannot carry a read deadline gets the flushed 413 and the abort at once, since its
// drain could neither be bounded nor be joined.
func refuseOversize(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Connection", "close")
	h.Set("Content-Length", "0")
	h.Set("X-Should-Retry", "false")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		// The head did not reach the client; there is nothing left to read the upload for.
		panic(http.ErrAbortHandler)
	}
	if err := rc.SetReadDeadline(time.Now().Add(OversizeGrace)); err != nil {
		panic(http.ErrAbortHandler)
	}
	buf := make([]byte, copyBuffer)
	for {
		_, err := r.Body.Read(buf)
		if errors.Is(err, io.EOF) {
			// The grace no longer applies once the upload has ended; a deadline left armed would cut what the
			// listener still does on this connection.
			_ = rc.SetReadDeadline(time.Time{})
			return
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}
