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

// Package supervisor serves the public listener in front of the request worker.
package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

const (
	// maxRelayBody is the largest request body the supervisor buffers for the worker (lib/commands.js:2290).
	maxRelayBody = 64 << 20
	// relayAttempts and relayWait bound the wait for a worker that has not reported its port or refused a connection
	// (lib/commands.js:2366-2368,2393).
	relayAttempts = 80
	relayWait     = 50 * time.Millisecond
	// shutdownGrace bounds how long a cancelled Serve waits for in-flight requests before closing their connections.
	shutdownGrace = 5 * time.Second
	// maxInterim is the number of interim responses tolerated before the worker's final answer; the next one ends the
	// exchange, as in net/http's client.
	maxInterim = 5
	// maxHealthBody bounds the worker's health document the supervisor reads to add its own identity.
	maxHealthBody = 1 << 20
)

type handler struct {
	publicPort int
	workerPort func() int
	token      string
	version    string
	pid        int
}

// NewHandler constructs a handler for an already bound public port. workerPort returns the port of the worker that is
// currently eligible to receive requests, or 0 while none is.
//
// The handler judges req.Host as the Host header the client sent. Serve supplies exactly that; a caller that dispatches
// requests some other way must do the same.
func NewHandler(publicPort int, workerPort func() int, token, version string, pid int) (http.Handler, error) {
	if !validToken(token) {
		return nil, errors.New("model-gateway: the supervisor needs a valid local control token before serving")
	}
	if publicPort <= 0 || publicPort > 65535 || workerPort == nil {
		return nil, errors.New("model-gateway: the supervisor needs its public port and a worker port source")
	}
	return &handler{publicPort: publicPort, workerPort: workerPort, token: token, version: version, pid: pid}, nil
}

func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	for i := range len(token) {
		if b := token[i]; (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func requestTarget(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

func refuse(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	port := strconv.Itoa(h.publicPort)
	if !controlauth.LocalRequestAllowed(r, []string{"127.0.0.1:" + port, "localhost:" + port}) {
		refuse(w)
		return
	}
	pathOnly, _, _ := strings.Cut(requestTarget(r), "?")
	if pathOnly == "/restart" || pathOnly == "/drain" {
		if !controlauth.Authenticated(r, h.token, []int{h.publicPort}, false) {
			refuse(w)
			return
		}
	}
	if pathOnly == "/restart" {
		// Replacing the worker on request needs the worker process owner, which this handler does not hold.
		w.WriteHeader(http.StatusNotImplemented)
		return
	}
	h.relay(w, r)
}

// relay forwards one buffered request to the current worker and streams its answer back.
func (h *handler) relay(w http.ResponseWriter, r *http.Request) {
	target := requestTarget(r)
	if !wireSafe(r.Method) || !wireSafe(target) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	resp, err := h.deliver(r, target, body)
	if err != nil {
		if r.Context().Err() != nil {
			// The client is gone; there is no one to answer.
			return
		}
		writeWorkerFailure(w, errors.Is(err, errBodyTaken))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	pathOnly, _, _ := strings.Cut(target, "?")
	if pathOnly == "/healthz" && bodyAllowed(r.Method, resp.StatusCode) {
		h.relayHealth(w, resp)
		return
	}
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if err := streamBody(w, resp.Body); err != nil {
		// Headers are out: the only honest signal left is a broken connection (lib/commands.js:2398-2406).
		panic(http.ErrAbortHandler)
	}
}

// bodyAllowed reports whether a response to method with status carries a body at all.
func bodyAllowed(method string, status int) bool {
	return method != http.MethodHead && status != http.StatusNoContent && status != http.StatusNotModified
}

// wireSafe reports whether s can be written into a request line unchanged.
func wireSafe(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if b := s[i]; b <= ' ' || b == 0x7f {
			return false
		}
	}
	return true
}

// clientDeparted reports whether a body read failed because the client went away, as the source distinguishes it
// (lib/commands.js:2311-2323).
func clientDeparted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRelayBody+1))
	switch {
	case err != nil:
		if !clientDeparted(r.Context(), err) {
			w.Header().Set("Connection", "close")
			w.Header().Set("X-Should-Retry", "false")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"model-gateway could not read the request body"}}`)
		}
		return nil, false
	case len(data) > maxRelayBody:
		w.Header().Set("Connection", "close")
		w.Header().Set("Content-Length", "0")
		w.Header().Set("X-Should-Retry", "false")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return nil, false
	}
	return data, true
}

var errBodyTaken = errors.New("model-gateway: the worker took the request body and never answered")

// deliver sends the body to the worker. A body that reached a worker is never sent to the same worker again: it may
// already have been spent upstream. Only a worker on a different port, a replacement, receives it again.
// It returns the context's error once the client has gone.
func (h *handler) deliver(r *http.Request, target string, body []byte) (*http.Response, error) {
	ctx := r.Context()
	var takenBy int
	var lastErr error
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if attempt > relayAttempts {
				if takenBy != 0 {
					return nil, errBodyTaken
				}
				return nil, errors.Join(errors.New("model-gateway: could not deliver the request to the worker"), lastErr)
			}
			timer := time.NewTimer(relayWait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		port := h.workerPort()
		if port <= 0 || port > 65535 || port == takenBy {
			continue
		}
		resp, connected, err := roundTrip(ctx, r, target, body, port)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		if connected {
			takenBy = port
		}
	}
}

// relayExcluded are the request header fields the relay writes itself or that describe only the client's connection.
var relayExcluded = []string{"Host", "Content-Length", "Transfer-Encoding", "Trailer", "Connection", "Keep-Alive", "Expect"}

// roundTrip sends one request on a connection of its own (lib/commands.js:2467-2472) and reads the response head.
// connected reports whether the worker accepted the connection, after which it may have taken the body. The request
// line carries target byte for byte. The returned body closes the connection.
//
// The Connection: close on this leg concerns the worker connection only. http.ReadResponse removes a Connection: close
// field from the worker's answer, so the relayed response does not carry it and the client's connection stays open
// unless the client or the public listener ends it.
func roundTrip(ctx context.Context, r *http.Request, target string, body []byte, port int) (*http.Response, bool, error) {
	authority := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp4", authority)
	if err != nil {
		return nil, false, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	release := func() {
		stop()
		_ = conn.Close()
	}
	header := r.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	for _, name := range relayExcluded {
		header.Del(name)
	}
	bw := bufio.NewWriter(conn)
	_, _ = fmt.Fprintf(bw, "%s %s HTTP/1.1\r\nHost: %s\r\n", r.Method, target, authority)
	_ = header.Write(bw)
	if len(body) > 0 || r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
		_, _ = fmt.Fprintf(bw, "Content-Length: %d\r\n", len(body))
	}
	_, _ = bw.WriteString("Connection: close\r\n\r\n")
	_, _ = bw.Write(body)
	if err := bw.Flush(); err != nil {
		release()
		return nil, true, err
	}
	br := bufio.NewReader(conn)
	for interim := 0; ; interim++ {
		resp, err := http.ReadResponse(br, &http.Request{Method: r.Method})
		if err != nil {
			release()
			return nil, true, err
		}
		if resp.StatusCode >= http.StatusOK {
			resp.Body = &relayBody{ReadCloser: resp.Body, release: release}
			return resp, true, nil
		}
		_ = resp.Body.Close()
		// The worker took the connection, so each of these is answered as a worker that never answered.
		switch {
		case resp.StatusCode < http.StatusContinue:
			release()
			return nil, true, fmt.Errorf("model-gateway: the worker answered with status %d", resp.StatusCode)
		case resp.StatusCode == http.StatusSwitchingProtocols:
			release()
			return nil, true, errors.New("model-gateway: the worker switched protocols on a relayed request")
		case interim+1 > maxInterim:
			release()
			return nil, true, errors.New("model-gateway: the worker sent too many interim responses")
		}
	}
}

// relayBody closes the worker connection with the response body.
type relayBody struct {
	io.ReadCloser
	release func()
}

func (b *relayBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		dst[key] = slices.Clone(values)
	}
}

func streamBody(w http.ResponseWriter, body io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// relayHealth adds the supervisor's identity to the worker's health document (lib/commands.js:2408-2419).
func (h *handler) relayHealth(w http.ResponseWriter, resp *http.Response) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBody+1))
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	if len(data) > maxHealthBody {
		// Too large to be the worker's health document.
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	health, err := jsonx.DecodeWire(data)
	if err != nil || health.Kind != '{' {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	setMember(&health, "supervisorVersion", wireString(h.version))
	setMember(&health, "supervisorPid", jsonx.WireValue{Kind: '0', Number: float64(h.pid)})
	setMember(&health, "proxyRecovery", jsonx.WireValue{Kind: 't'})
	out, err := jsonx.EncodeWire(health)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	copyHeader(w.Header(), resp.Header)
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

func wireString(s string) jsonx.WireValue {
	return jsonx.WireValue{Kind: '"', Text: utf16Units(s)}
}

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

func writeWorkerFailure(w http.ResponseWriter, bodyTaken bool) {
	// Claude Code resends a retryable status silently; once a worker took the body a resend buys a second inference
	// (lib/commands.js:2423-2437).
	message := "model-gateway could not deliver this request to the shim worker; retry it shortly"
	if bodyTaken {
		w.Header().Set("X-Should-Retry", "false")
		message = "model-gateway lost the shim worker connection after this request reached the model; that answer cannot be recovered, and retrying automatically would pay for a second inference, so send it again yourself if you still want one"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	out, err := jsonx.EncodeWire(jsonx.WireValue{Kind: '{', Members: []jsonx.Member{
		{Name: utf16Units("type"), Value: wireString("error")},
		{Name: utf16Units("error"), Value: jsonx.WireValue{Kind: '{', Members: []jsonx.Member{
			{Name: utf16Units("type"), Value: wireString("api_error")},
			{Name: utf16Units("message"), Value: wireString(message)},
		}}},
	}})
	if err == nil {
		_, _ = w.Write(out)
	}
}

// FixtureShutdown consumes and closes the inherited fixture channel. Without a test guard it closes it unread.
// It reports whether the channel carried the fixture's shutdown message before its end.
func FixtureShutdown(channel io.ReadCloser, guarded bool) (bool, error) {
	defer func() { _ = channel.Close() }()
	if !guarded {
		return false, nil
	}
	reader := bufio.NewReader(channel)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 && fixtureStop(line) {
			return true, nil
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// fixtureStop applies message?.type === 'fixture-shutdown' to one channel message (lib/commands.js:2900-2904).
func fixtureStop(line []byte) bool {
	message, err := jsonx.DecodeWire(line)
	if err != nil || message.Kind != '{' {
		return false
	}
	for _, m := range slices.Backward(message.Members) {
		if slices.Equal(m.Name, utf16Units("type")) {
			return m.Value.Kind == '"' && slices.Equal(m.Value.Text, utf16Units("fixture-shutdown"))
		}
	}
	return false
}
