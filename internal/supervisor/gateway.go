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

package supervisor

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
)

const (
	// identityPath and stopPath are the two supervisor control operations served before the relay. They are
	// compared with the raw request target before '?', so an encoded or otherwise different spelling is ordinary
	// traffic.
	identityPath = "/internal/identity"
	stopPath     = "/internal/stop"
	// maxStopBody is the largest stop entity read, inclusive. The client sends {}; the bound also holds for a
	// chunked entity, whose length the head does not announce.
	maxStopBody = 4096
	// controlTokenHeader names the control token field. controlauth keeps its own copy private and compares the
	// token itself; this copy only detects a token offered where none belongs.
	controlTokenHeader = "X-Model-Gateway-Control-Token"
	// stopAcknowledgment acknowledges that worker starts are inhibited; it does not confirm that any process exited.
	stopAcknowledgment = `{"ok":true,"stopping":true}`
)

// Options supplies the bound public listener's relay, lifecycle actions and supervisor identity.
type Options struct {
	// PublicPort is the actual bound loopback TCP port.
	PublicPort int
	// Token is the validated control token of the supervisor's state root.
	Token string
	// Relay handles traffic not dispatched as a supervisor control operation.
	Relay http.Handler
	// Restart validates a candidate and requests a worker restart.
	Restart func(context.Context, []byte) error
	// Stop synchronously inhibits worker starts and requests ordered shutdown without waiting for exit.
	Stop func()
	// StartupGeneration is the generation published in the supervisor's own instance record.
	StartupGeneration string
	// SignIdentity returns the supervisor proof for the given token, nonce and startup generation.
	SignIdentity func(token, nonce, generation string) (string, error)
}

// controlDispatcher answers the identity proof and the stop on the public listener and hands every other request to
// the relay. Nothing it writes or returns names the token, a nonce or a proof outside the proof's own answer.
type controlDispatcher struct {
	o Options
	// host is the only Host a control request may carry: the numeric loopback address with the bound port. The
	// name localhost, accepted for ordinary traffic, is not an authenticated control Host.
	host string

	// stopMu serializes accepted stops, so that a concurrent repeat is acknowledged only after the first call of
	// Stop has returned. stopState records how that one call ended.
	stopMu    sync.Mutex
	stopState stopState
}

// stopState is the outcome of the dispatcher's one call of Stop.
type stopState int

const (
	// stopPending means Stop has not been called yet.
	stopPending stopState = iota
	// stopDone means Stop returned normally: worker starts are inhibited and shutdown has been requested.
	stopDone
	// stopFailed means Stop did not return normally. What it did before that is unknown, so it is never called
	// again and no later stop is acknowledged.
	stopFailed
)

// NewGateway constructs the dispatcher for an already bound supervisor listener.
//
// It refuses options that cannot serve: a port outside the TCP range, an invalid token, an empty or invalid UTF-8
// startup generation, and a missing relay, stop callback or signer. The startup generation is the one value ever
// signed; nothing the client sends chooses it. Restart is not used by this dispatcher. Errors never repeat the token.
func NewGateway(o Options) (http.Handler, error) {
	switch {
	case o.PublicPort <= 0 || o.PublicPort > 65535:
		return nil, errors.New("model-gateway: the supervisor gateway needs its bound public port")
	case !validToken(o.Token):
		return nil, errors.New("model-gateway: the supervisor gateway needs a valid local control token")
	case o.StartupGeneration == "" || !utf8.ValidString(o.StartupGeneration):
		return nil, errors.New("model-gateway: the supervisor gateway needs its startup generation")
	case o.Relay == nil:
		return nil, errors.New("model-gateway: the supervisor gateway needs a relay for ordinary traffic")
	case o.Stop == nil:
		return nil, errors.New("model-gateway: the supervisor gateway needs a stop action")
	case o.SignIdentity == nil:
		return nil, errors.New("model-gateway: the supervisor gateway needs an identity signer")
	}
	return &controlDispatcher{o: o, host: "127.0.0.1:" + strconv.Itoa(o.PublicPort)}, nil
}

// ServeHTTP dispatches on the raw path alone: a request for either control path is answered here whatever its
// method, and is never relayed; every other request goes to the relay unchanged. The path is taken from the request
// target exactly as received (RequestURI) and never rebuilt from the parsed URL, so a request supplied without a
// received target is ordinary traffic.
func (d *controlDispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	pathOnly, _, _ := strings.Cut(r.RequestURI, "?")
	switch pathOnly {
	case identityPath:
		d.serveIdentity(w, r)
	case stopPath:
		d.serveStop(w, r)
	default:
		d.o.Relay.ServeHTTP(w, r)
	}
}

// serveIdentity answers a challenge with the keyed proof over the nonce and this supervisor's startup generation.
// A wrong method or Host, any Origin and any offered control token are refused with an empty 403 before the
// challenge is judged; an absent, repeated or malformed challenge and a nonempty entity get an empty 400; a signer
// failure gets an empty 500, never a partial proof. The signer is called only for an accepted challenge.
func (d *controlDispatcher) serveIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !controlauth.LocalRequestAllowed(r, []string{d.host}) || hasField(r.Header, controlTokenHeader) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	nonce, ok := singleField(r.Header, controlauth.IdentityNonceHeader)
	if !ok || !validNonce(nonce) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	empty, ok := emptyEntity(w, r)
	if !ok {
		return
	}
	if !empty {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	proof, err := d.o.SignIdentity(d.o.Token, nonce, d.o.StartupGeneration)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(struct {
		Generation string `json:"generation"`
		Proof      string `json:"proof"`
	}{d.o.StartupGeneration, proof})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeControlJSON(w, http.StatusOK, body)
}

// serveStop inhibits worker starts and acknowledges it. The request is authenticated from its head alone, by
// controlauth's constant-time token comparison with the bound port; a refusal is an empty 403 written before the
// entity is read, so an unauthenticated client cannot make the supervisor read its body. An authenticated stop that
// announces more than maxStopBody bytes is an empty 413 at once, before any body byte is read and so without a
// 100 Continue. Otherwise the entity, chunked or not, is read into one fixed buffer of maxStopBody+1 bytes: filling
// it is an empty 413, and anything other than one empty JSON object is an empty 400. If Stop failed on an earlier
// request, the answer is an empty 500.
func (d *controlDispatcher) serveStop(w http.ResponseWriter, r *http.Request) {
	if !controlauth.Authenticated(r, d.o.Token, []int{d.o.PublicPort}, false) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.ContentLength > maxStopBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	var buf [maxStopBody + 1]byte
	n, err := readStopBody(r.Body, &buf)
	switch {
	case n > maxStopBody:
		// The bytes read decide before the error that came with them: the entity is too large either way.
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	case err != nil && err != io.EOF: //nolint:errorlint // Only the bare io.EOF is a graceful end; one that wraps or joins it is a failure.
		if !clientDeparted(r.Context(), err) {
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	case !emptyJSONObject(buf[:n]):
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !d.stop() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeControlJSON(w, http.StatusAccepted, []byte(stopAcknowledgment))
}

// readStopBody reads the stop entity into buf until buf is full or a read returns an error, and returns the number of
// bytes read with that error exactly as the body returned it (io.EOF at a normal end, nil when buf filled). The caller
// judges it: only the bare io.EOF is a graceful end. No read asks for a byte past the end of buf, so at most len(buf)
// bytes are ever taken from the body.
func readStopBody(body io.Reader, buf *[maxStopBody + 1]byte) (int, error) {
	if body == nil {
		return 0, nil
	}
	n := 0
	for n < len(buf) {
		m, err := body.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// stop calls Stop at most once for the life of the dispatcher and reports whether that call returned normally.
// Concurrent and later accepted stops wait for that call instead of repeating the shutdown request, so every
// acknowledgment follows the inhibition. A Stop that does not return normally (it panics) is recorded as failed by a
// deferred assignment before the panic leaves the dispatcher; the panic then propagates to the server unchanged. That
// Stop may already have set the inhibition or requested shutdown, so it is never called again, and every later
// accepted stop is answered with an empty 500 instead of an acknowledgment.
func (d *controlDispatcher) stop() bool {
	d.stopMu.Lock()
	defer d.stopMu.Unlock()
	if d.stopState != stopPending {
		return d.stopState == stopDone
	}
	returned := false
	defer func() {
		if !returned {
			d.stopState = stopFailed
		}
	}()
	d.o.Stop()
	returned = true
	d.stopState = stopDone
	return true
}

// writeControlJSON writes a complete, self-delimited control answer that no cache may keep.
func writeControlJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// emptyEntity reports whether the request carries no entity. An announced length is judged from the head; a chunked
// entity is read until its first byte or its end, so that an empty one ends before the answer and the connection
// can carry the next request. ok is false when the read failed; a read failure other than the client's departure has
// already been answered with an empty 400.
func emptyEntity(w http.ResponseWriter, r *http.Request) (empty, ok bool) {
	switch {
	case r.ContentLength > 0:
		return false, true
	case r.ContentLength == 0 || r.Body == nil:
		return true, true
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil {
		if !clientDeparted(r.Context(), err) {
			w.WriteHeader(http.StatusBadRequest)
		}
		return false, false
	}
	return len(data) == 0, true
}

// hasField reports whether any field of the given name is present, even with an empty value. The name is compared
// without regard to case, because a header that was not built by a parser need not be canonical.
func hasField(header http.Header, name string) bool {
	for key := range header {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// singleField returns the value of the given field when the header holds exactly one occurrence of it, counted
// across every spelling of the name.
func singleField(header http.Header, name string) (string, bool) {
	var found []string
	for key, values := range header {
		if strings.EqualFold(key, name) {
			found = append(found, values...)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// validNonce reports whether s is a canonical challenge: exactly the challenge's bytes spelled in lowercase
// hexadecimal. It is checked here, before the signer, so that a malformed challenge never reaches it.
func validNonce(s string) bool {
	if len(s) != 2*controlauth.IdentityNonceBytes {
		return false
	}
	for i := range len(s) {
		if b := s[i]; (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// emptyJSONObject reports whether data is exactly one empty JSON object, with JSON white space allowed around and
// inside it. A member, any other value or a trailing value is refused.
func emptyJSONObject(data []byte) bool {
	inner, ok := bytes.CutPrefix(bytes.Trim(data, jsonSpace), []byte("{"))
	if !ok {
		return false
	}
	inner, ok = bytes.CutSuffix(inner, []byte("}"))
	return ok && len(bytes.Trim(inner, jsonSpace)) == 0
}

// jsonSpace is the white space JSON allows between tokens (RFC 8259, section 2).
const jsonSpace = " \t\r\n"
