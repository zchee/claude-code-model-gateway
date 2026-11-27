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
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// healthLimit bounds the worker's health document the relay reads to add the supervisor's identity, so that a health
// probe never allocates in proportion to an unbounded worker answer.
const healthLimit = 1 << 20

// relayAnswer relays the worker's final response (lib/commands.js:2393-2418). The head is written and flushed at once,
// with Connection: close, because the measured source ends the client connection after a relayed answer by copying
// the worker's own; each piece of the body is written and flushed as it arrives. Once the head is out the answer can
// no longer become an error: a worker failure, a failed client write or flush, or a departure breaks the client
// connection by http.ErrAbortHandler, without terminal framing, a rewritten status, another attempt or another read
// from the worker. A client that departed before the head gets nothing. Redirects are relayed, never followed.
//
// The request context is consulted before every commit, write and flush, and before the normal return at the end of
// the body: a clean end of the worker's answer that coincides with a departure still aborts, so that the public
// listener does not write terminal framing to a client that has gone.
//
// The body is never closed through resp.Body: for an unfinished answer that would read the worker leg to its end. The
// caller closes the leg instead.
func (h *handler) relayAnswer(ctx context.Context, w http.ResponseWriter, method, target string, resp *http.Response) {
	pathOnly, _, _ := strings.Cut(target, "?")
	if pathOnly == "/healthz" && bodyAllowed(method, resp.StatusCode) {
		h.relayHealth(ctx, w, resp)
		return
	}
	if ctx.Err() != nil {
		return
	}
	copyHeader(w.Header(), resp.Header)
	w.Header().Set("Connection", "close")
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	flushOrAbort(ctx, rc)
	buf := make([]byte, copyBuffer)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			// Bytes that arrive together with a failure are passed on before the connection breaks.
			abortIfDeparted(ctx)
			if _, werr := w.Write(buf[:n]); werr != nil {
				panic(http.ErrAbortHandler)
			}
			flushOrAbort(ctx, rc)
		}
		if errors.Is(err, io.EOF) {
			abortIfDeparted(ctx)
			return
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

// abortIfDeparted breaks the client connection by http.ErrAbortHandler when the request context has ended after the
// response head was committed.
func abortIfDeparted(ctx context.Context) {
	if ctx.Err() != nil {
		panic(http.ErrAbortHandler)
	}
}

// flushOrAbort sends what the response holds. A departure, or a flush that fails, breaks the client connection by
// http.ErrAbortHandler; a response that cannot flush at all is not a failure, its bytes go out when it completes.
func flushOrAbort(ctx context.Context, rc *http.ResponseController) {
	abortIfDeparted(ctx)
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		panic(http.ErrAbortHandler)
	}
}

// bodyAllowed reports whether a response to method with status carries a body at all.
func bodyAllowed(method string, status int) bool {
	return method != http.MethodHead && status != http.StatusNoContent && status != http.StatusNotModified
}

// relayHealth adds the supervisor's identity to the worker's health document (lib/commands.js:2402-2414): its version,
// its process identifier and proxyRecovery, assigned as JavaScript assigns properties, so that a member the worker
// already sent keeps its place. Every other member, compat included, stays as the worker sent it: the supervisor
// neither adds nor overwrites compat, which belongs to the remote control feature. The worker's status and fields are
// kept, with the length of the new document. A document that is not a JSON object, or is larger than healthLimit, is
// answered 502; a worker failure while it is read breaks the client connection. The request context is consulted
// again after decoding, after the head is committed and immediately before the body is written, and before the
// normal return, as relayAnswer does.
func (h *handler) relayHealth(ctx context.Context, w http.ResponseWriter, resp *http.Response) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, healthLimit+1))
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		panic(http.ErrAbortHandler)
	}
	out, ok := []byte(nil), len(data) <= healthLimit
	if ok {
		out, ok = h.augmentHealth(data)
	}
	if ctx.Err() != nil {
		return
	}
	if !ok {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	copyHeader(w.Header(), resp.Header)
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	// A client that left while the head was committed gets nothing further.
	abortIfDeparted(ctx)
	if _, err := w.Write(out); err != nil {
		panic(http.ErrAbortHandler)
	}
	abortIfDeparted(ctx)
}

// augmentHealth returns the worker's health document with the supervisor's identity, or false when data is not a JSON
// object.
func (h *handler) augmentHealth(data []byte) ([]byte, bool) {
	health, err := jsonx.DecodeWire(data)
	if err != nil || health.Kind != '{' {
		return nil, false
	}
	setMember(&health, "supervisorVersion", wireString(h.version))
	setMember(&health, "supervisorPid", jsonx.WireValue{Kind: '0', Number: float64(h.pid)})
	setMember(&health, "proxyRecovery", jsonx.WireValue{Kind: 't'})
	out, err := jsonx.EncodeWire(health)
	if err != nil {
		return nil, false
	}
	return out, true
}

// copyHeader copies every field of src into dst.
func copyHeader(dst, src http.Header) {
	for key, values := range src {
		dst[key] = slices.Clone(values)
	}
}

// wireString is s as a JSON string of UTF-16 units.
func wireString(s string) jsonx.WireValue {
	return jsonx.WireValue{Kind: '"', Text: utf16Units(s)}
}

// utf16Units returns the UTF-16 units of s.
func utf16Units(s string) []uint16 {
	units := make([]uint16, 0, len(s))
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			units = append(units, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
			continue
		}
		units = append(units, uint16(r))
	}
	return units
}

// setMember assigns a property as JavaScript does: an existing member keeps its position.
func setMember(object *jsonx.WireValue, name string, value jsonx.WireValue) {
	key := utf16Units(name)
	for i := range object.Members {
		if slices.Equal(object.Members[i].Name, key) {
			object.Members[i].Value = value
			return
		}
	}
	object.Members = append(object.Members, jsonx.Member{Name: key, Value: value})
}
