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
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
)

type handler struct {
	publicPort int
	workerPort func() int
	token      string
	version    string
	pid        int
	transport  *http.Transport
}

// NewHandler constructs a handler for an already bound public port. workerPort returns the current worker's port.
func NewHandler(publicPort int, workerPort func() int, token, version string, pid int) (http.Handler, error) {
	if !validToken(token) {
		return nil, errors.New("model-gateway: the supervisor needs a valid local control token before serving")
	}
	if publicPort <= 0 || publicPort > 65535 || workerPort == nil {
		return nil, errors.New("model-gateway: the supervisor needs its public port and a worker port source")
	}
	// One connection per relayed request, no environment proxy, no compression and no automatic replay: a pooled
	// socket the worker closed at the same moment would be indistinguishable from a worker that took the body
	// (lib/commands.js:2467-2472).
	transport := &http.Transport{
		Proxy:              nil,
		DisableKeepAlives:  true,
		DisableCompression: true,
		ForceAttemptHTTP2:  false,
	}
	return &handler{publicPort: publicPort, workerPort: workerPort, token: token, version: version, pid: pid, transport: transport}, nil
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
	host, ok := wireHost(r.Context(), r.Host)
	if !ok {
		refuse(w)
		return
	}
	// The controlauth checks read the Host header as sent from req.Host.
	r.Host = host
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
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	resp, err := h.deliver(r, body)
	if err != nil {
		writeWorkerFailure(w, errors.Is(err, errBodyTaken))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	pathOnly, _, _ := strings.Cut(requestTarget(r), "?")
	if pathOnly == "/healthz" {
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

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRelayBody+1))
	switch {
	case err != nil:
		if r.Context().Err() == nil {
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
func (h *handler) deliver(r *http.Request, body []byte) (*http.Response, error) {
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
			case <-r.Context().Done():
				timer.Stop()
				return nil, r.Context().Err()
			case <-timer.C:
			}
		}
		port := h.workerPort()
		if port <= 0 || port > 65535 || port == takenBy {
			continue
		}
		var connected bool
		resp, err := h.roundTrip(r, body, port, &connected)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if connected {
			takenBy = port
		}
	}
}

func (h *handler) roundTrip(r *http.Request, body []byte, port int, connected *bool) (*http.Response, error) {
	authority := "127.0.0.1:" + strconv.Itoa(port)
	header := r.Header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	// The relayed connection is closed after this request; the client's own connection headers do not apply to it.
	header.Del("Connection")
	header.Del("Keep-Alive")
	if _, ok := header["User-Agent"]; !ok {
		// An explicitly empty value keeps net/http from adding its own User-Agent.
		header["User-Agent"] = []string{""}
	}
	ctx := r.Context()
	out := (&http.Request{
		Method:        r.Method,
		URL:           relayURL(authority, requestTarget(r)),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Host:          authority,
		ContentLength: int64(len(body)),
	}).WithContext(ctx)
	if len(body) > 0 {
		out.Body = io.NopCloser(bytes.NewReader(body))
	} else {
		out.Body = http.NoBody
	}
	transport := h.transport.Clone()
	defer transport.CloseIdleConnections()
	dialer := &net.Dialer{}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if err == nil {
			*connected = true
		}
		return conn, err
	}
	return transport.RoundTrip(out)
}

// relayURL keeps the request target byte for byte, including an absolute-form target and its query.
func relayURL(authority, target string) *url.URL {
	return &url.URL{Scheme: "http", Host: authority, Opaque: target}
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
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
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(http.ErrAbortHandler)
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
	w.Header().Del("Content-Length")
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

// Serve dispatches requests received on the bound public listener until cancellation or listener failure.
// It returns nil after cancellation, once in-flight requests have finished or the shutdown grace has passed.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 120 * time.Second,
		IdleTimeout:       75 * time.Second,
		ConnContext:       withHeadParser,
		ErrorLog:          nil,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(headListener{listener}) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
