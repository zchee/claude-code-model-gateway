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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The public listener is served by this connection loop rather than by net/http's server. net/http replaces
// Request.Host with the authority of an absolute-form request target and deletes the Host header before any handler
// runs, while the source judges the Host header as the client sent it (lib/commands.js:2801-2813,
// lib/control-auth.js:41-43). Each request head is parsed exactly once, by net/textproto (the parser net/http uses for
// the same purpose), and Request.Host is the Host field of that parse. A connection serves its requests strictly one
// after another: the next head is read only after the previous response is complete and the previous body has been read
// to its framed end, trailers included; otherwise the connection is closed. Nothing is answered without the handler
// except a refusal of a request that cannot be parsed or framed.

const (
	// headTimeout and idleTimeout are the source's server settings; a request has no overall time limit
	// (lib/commands.js:2867-2871).
	headTimeout = 120 * time.Second
	idleTimeout = 75 * time.Second
	// maxHeadBytes bounds the bytes read from the connection while one request head or trailer section is parsed,
	// as net/http bounds them by its default header limit plus its read slack.
	maxHeadBytes = http.DefaultMaxHeaderBytes + 4096
	// maxDiscard bounds the unread request body read away so that the connection can serve its next request.
	maxDiscard = 256 << 10
	// lingerTimeout bounds the wait for the client to close its end of a connection that is being closed, so that
	// unread request bytes do not turn the close into a reset that destroys the response.
	lingerTimeout = 500 * time.Millisecond
)

var errHeadTooLarge = errors.New("model-gateway: request head too large")

type server struct {
	handler http.Handler
	base    context.Context
	wg      sync.WaitGroup

	mu       sync.Mutex
	conns    map[*conn]struct{}
	stopping bool
}

// Serve dispatches requests received on the bound public listener until cancellation or listener failure.
// The handler receives each request with Host set to the Host header exactly as the client sent it.
// It returns nil after cancellation, once in-flight requests have finished or the shutdown grace has passed.
func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	s := &server{handler: handler, base: context.WithoutCancel(ctx), conns: make(map[*conn]struct{})}
	accepted := make(chan error, 1)
	go func() { accepted <- s.accept(listener) }()
	var err error
	select {
	case err = <-accepted:
	case <-ctx.Done():
		_ = listener.Close()
		<-accepted
	}
	if ctx.Err() == nil {
		s.closeAll()
		s.wg.Wait()
		return err
	}
	s.shutdown()
	return nil
}

func (s *server) accept(listener net.Listener) error {
	var delay time.Duration
	for {
		rwc, err := listener.Accept()
		if err != nil {
			if !temporary(err) {
				return err
			}
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			time.Sleep(delay)
			continue
		}
		delay = 0
		c := newConn(s, rwc)
		s.mu.Lock()
		if s.stopping {
			s.mu.Unlock()
			_ = rwc.Close()
			continue
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go c.serve()
	}
}

func temporary(err error) bool {
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

// shutdown closes idle connections at once, lets active requests finish within the grace, then closes the rest.
func (s *server) shutdown() {
	s.mu.Lock()
	s.stopping = true
	for c := range s.conns {
		if !c.active {
			_ = c.rwc.Close()
		}
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	timer := time.NewTimer(shutdownGrace)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	s.closeAll()
	<-done
}

// closeAll closes every connection and ends the context of every request in progress.
func (s *server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping = true
	for c := range s.conns {
		_ = c.rwc.Close()
		if c.cancel != nil {
			c.cancel()
		}
	}
}

func (s *server) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// budgetReader limits the bytes read from the connection while a head or trailer section is parsed. The budget counts
// the bytes actually read, so a head is judged the same however the network splits it.
type budgetReader struct {
	r      io.Reader
	remain int64 // Negative when no parse is in progress.
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.remain < 0 {
		return b.r.Read(p)
	}
	if b.remain == 0 {
		return 0, errHeadTooLarge
	}
	if int64(len(p)) > b.remain {
		p = p[:b.remain]
	}
	n, err := b.r.Read(p)
	b.remain -= int64(n)
	return n, err
}

type conn struct {
	srv    *server
	rwc    net.Conn
	budget *budgetReader
	br     *bufio.Reader
	wmu    sync.Mutex // Serializes writes of an interim 100 Continue with the response.
	bw     *bufio.Writer
	// answered records, under wmu, that the response head is out; a 100 Continue after it would be out of order.
	answered bool

	// Guarded by srv.mu.
	active bool
	cancel context.CancelFunc

	watch    chan struct{}
	stopped  atomic.Bool
	departed atomic.Bool
}

func newConn(s *server, rwc net.Conn) *conn {
	c := &conn{srv: s, rwc: rwc, budget: &budgetReader{r: rwc, remain: -1}}
	c.br = bufio.NewReader(c.budget)
	c.bw = bufio.NewWriter(rwc)
	return c
}

func (c *conn) serve() {
	defer c.srv.wg.Done()
	defer func() {
		c.srv.mu.Lock()
		delete(c.srv.conns, c)
		c.srv.mu.Unlock()
		_ = c.rwc.Close()
	}()
	for first := true; ; first = false {
		if !c.awaitHead(first) {
			return
		}
		if !c.serveOne() {
			c.linger()
			return
		}
	}
}

// awaitHead waits for the first byte of the next request and marks the connection active, unless the server stops.
func (c *conn) awaitHead(first bool) bool {
	wait := idleTimeout
	if first {
		wait = headTimeout
	}
	c.srv.mu.Lock()
	c.active = false
	stopping := c.srv.stopping
	c.srv.mu.Unlock()
	if stopping {
		return false
	}
	_ = c.rwc.SetReadDeadline(time.Now().Add(wait))
	if _, err := c.br.Peek(1); err != nil {
		return false
	}
	c.srv.mu.Lock()
	defer c.srv.mu.Unlock()
	if c.srv.stopping {
		return false
	}
	c.active = true
	return true
}

// serveOne reads, dispatches and completes one request. It reports whether the connection can carry another one.
func (c *conn) serveOne() bool {
	_ = c.rwc.SetReadDeadline(time.Now().Add(headTimeout))
	c.budget.remain = maxHeadBytes
	parsed, status := c.readRequest()
	c.budget.remain = -1
	if parsed == nil {
		if status != 0 {
			c.writeRefusal(status)
		}
		return false
	}
	_ = c.rwc.SetReadDeadline(time.Time{})

	ctx, cancel := context.WithCancel(context.WithValue(c.srv.base, http.LocalAddrContextKey, c.rwc.LocalAddr()))
	defer cancel()
	c.srv.mu.Lock()
	c.cancel = cancel
	c.srv.mu.Unlock()
	defer func() {
		c.srv.mu.Lock()
		c.cancel = nil
		c.srv.mu.Unlock()
	}()

	body := parsed.body
	req := parsed.req.WithContext(ctx)
	body.done = parsed.req.ContentLength == 0
	body.atEnd = func() { c.watchDeparture(cancel) }
	if body.done {
		c.watchDeparture(cancel)
	}
	c.answered = false
	w := &response{c: c, req: req, header: make(http.Header), length: -1}
	aborted := c.dispatch(w, req)
	c.stopWatch()
	cancel()
	if aborted {
		return false
	}
	w.finish()
	if w.close || w.failed != nil || c.departed.Load() || c.srv.isStopping() {
		return false
	}
	return body.discard()
}

func (c *conn) dispatch(w *response, req *http.Request) (aborted bool) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		aborted = true
		if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			log.Printf("model-gateway: supervisor request handler panicked (%T); closing the connection", v)
		}
	}()
	c.srv.handler.ServeHTTP(w, req)
	return false
}

// watchDeparture cancels the request context when the client closes its connection. It starts only once the request
// body has been read to its end, so it never reads bytes that belong to the body.
func (c *conn) watchDeparture(cancel context.CancelFunc) {
	if c.watch != nil {
		return
	}
	c.stopped.Store(false)
	watch := make(chan struct{})
	c.watch = watch
	go func() {
		defer close(watch)
		// A pipelined next request stays buffered; only an error ends the wait.
		if _, err := c.br.Peek(1); err != nil && !c.stopped.Load() {
			c.departed.Store(true)
			cancel()
		}
	}()
}

func (c *conn) stopWatch() {
	if c.watch == nil {
		return
	}
	c.stopped.Store(true)
	_ = c.rwc.SetReadDeadline(time.Unix(1, 0))
	<-c.watch
	c.watch = nil
	_ = c.rwc.SetReadDeadline(time.Time{})
}

// linger flushes and half-closes the connection, then reads what the client still sends for a short while, so that the
// client receives the response before the connection ends.
func (c *conn) linger() {
	c.wmu.Lock()
	_ = c.bw.Flush()
	c.wmu.Unlock()
	if cw, ok := c.rwc.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	_ = c.rwc.SetReadDeadline(time.Now().Add(lingerTimeout))
	_, _ = io.Copy(io.Discard, io.LimitReader(c.rwc, maxDiscard))
}

func (c *conn) writeRefusal(status int) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, _ = fmt.Fprintf(c.bw, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
	_ = c.bw.Flush()
}

func (c *conn) writeContinue() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.answered {
		return nil
	}
	if _, err := c.bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
		return err
	}
	return c.bw.Flush()
}

type parsedRequest struct {
	req  *http.Request
	body *requestBody
}

// readRequest parses one request head. A nil result means the connection must close, after a refusal with the
// returned status when it is not zero.
func (c *conn) readRequest() (*parsedRequest, int) {
	tp := textproto.NewReader(c.br)
	var line string
	for line == "" {
		l, err := tp.ReadLine()
		if err != nil {
			return nil, readFailure(err)
		}
		line = l
	}
	method, rest, ok1 := strings.Cut(line, " ")
	target, proto, ok2 := strings.Cut(rest, " ")
	if !ok1 || !ok2 || !isToken(method) || target == "" {
		return nil, http.StatusBadRequest
	}
	major, minor, ok := http.ParseHTTPVersion(proto)
	if !ok {
		return nil, http.StatusBadRequest
	}
	if major != 1 || minor > 1 {
		return nil, http.StatusHTTPVersionNotSupported
	}
	if method == http.MethodConnect {
		// The source has no CONNECT listener, so the connection is closed without an answer.
		return nil, 0
	}
	u, err := url.ParseRequestURI(target)
	if err != nil {
		return nil, http.StatusBadRequest
	}
	mime, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, readFailure(err)
	}
	header := http.Header(mime)
	for _, values := range header {
		for _, v := range values {
			if !validFieldValue(v) {
				return nil, http.StatusBadRequest
			}
		}
	}
	hosts := header["Host"]
	if len(hosts) > 1 || len(hosts) == 0 && minor == 1 {
		return nil, http.StatusBadRequest
	}
	host := ""
	if len(hosts) == 1 {
		host = hosts[0]
	}
	delete(header, "Host")

	body := &requestBody{c: c}
	if expect, ok := header["Expect"]; ok {
		if minor != 1 || len(expect) != 1 || !strings.EqualFold(expect[0], "100-continue") {
			return nil, http.StatusExpectationFailed
		}
		body.expect = true
	}
	length, status := framing(header, minor)
	if status != 0 {
		return nil, status
	}
	body.chunked = length < 0
	body.remain = max(length, 0)
	req := &http.Request{
		Method: method, URL: u, Proto: proto, ProtoMajor: major, ProtoMinor: minor, Header: header, Host: host,
		RequestURI: target, RemoteAddr: c.rwc.RemoteAddr().String(), ContentLength: length, Body: body,
		Close: minor == 0 || headerHasToken(header["Connection"], "close"),
	}
	if body.chunked {
		req.TransferEncoding = []string{"chunked"}
		body.reader = httputil.NewChunkedReader(c.br)
	}
	return &parsedRequest{req: req, body: body}, 0
}

// framing returns the body length, -1 for a chunked body, or a refusal status. Ambiguous framing is refused rather
// than guessed, so the next head can never start inside this request's body.
func framing(header http.Header, minor int) (int64, int) {
	encodings, lengths := header["Transfer-Encoding"], header["Content-Length"]
	if len(encodings) > 0 {
		if minor == 0 || len(lengths) > 0 {
			return 0, http.StatusBadRequest
		}
		if len(encodings) != 1 || !strings.EqualFold(strings.TrimSpace(encodings[0]), "chunked") {
			return 0, http.StatusNotImplemented
		}
		delete(header, "Transfer-Encoding")
		return -1, 0
	}
	if len(lengths) == 0 {
		return 0, 0
	}
	first := strings.TrimSpace(lengths[0])
	for _, v := range lengths[1:] {
		if strings.TrimSpace(v) != first {
			return 0, http.StatusBadRequest
		}
	}
	if first == "" || strings.Trim(first, "0123456789") != "" {
		return 0, http.StatusBadRequest
	}
	n, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, http.StatusBadRequest
	}
	return n, 0
}

// readFailure maps a head read error: an oversized head is refused, a malformed one gets 400, and a connection that
// failed or closed is closed without an answer.
func readFailure(err error) int {
	var protocol textproto.ProtocolError
	switch {
	case errors.Is(err, errHeadTooLarge):
		return http.StatusRequestHeaderFieldsTooLarge
	case errors.As(err, &protocol):
		return http.StatusBadRequest
	default:
		return 0
	}
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		b := s[i]
		if b <= ' ' || b >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, b) >= 0 {
			return false
		}
	}
	return true
}

// validFieldValue rejects control characters other than horizontal tab, as net/http's server does.
func validFieldValue(v string) bool {
	for i := range len(v) {
		if b := v[i]; b < ' ' && b != '\t' || b == 0x7f {
			return false
		}
	}
	return true
}

func headerHasToken(values []string, token string) bool {
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// requestBody reads one request body to its framed end. A chunked body's trailer section is read and discarded, so
// that the connection is positioned at the next head.
type requestBody struct {
	c       *conn
	reader  io.Reader // The chunked decoder; nil for a body with a length.
	chunked bool
	remain  int64
	expect  bool // The client waits for 100 Continue before sending the body.
	atEnd   func()

	mu     sync.Mutex
	done   bool
	err    error
	closed bool
}

func (b *requestBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, http.ErrBodyReadAfterClose
	}
	return b.read(p)
}

func (b *requestBody) read(p []byte) (int, error) {
	switch {
	case b.done:
		return 0, io.EOF
	case b.err != nil:
		return 0, b.err
	}
	if b.expect {
		b.expect = false
		if err := b.c.writeContinue(); err != nil {
			b.err = err
			return 0, err
		}
	}
	var n int
	var err error
	if b.chunked {
		n, err = b.reader.Read(p)
		if errors.Is(err, io.EOF) {
			err = b.readTrailer()
		}
	} else {
		if int64(len(p)) > b.remain {
			p = p[:b.remain]
		}
		n, err = b.c.br.Read(p)
		b.remain -= int64(n)
		switch {
		case b.remain == 0:
			err = io.EOF
		case errors.Is(err, io.EOF):
			err = io.ErrUnexpectedEOF
		}
	}
	if errors.Is(err, io.EOF) {
		b.done = true
		if b.atEnd != nil {
			b.atEnd()
		}
		return n, io.EOF
	}
	if err != nil {
		b.err = err
	}
	return n, err
}

func (b *requestBody) readTrailer() error {
	b.c.budget.remain = maxHeadBytes
	defer func() { b.c.budget.remain = -1 }()
	if _, err := textproto.NewReader(b.c.br).ReadMIMEHeader(); err != nil {
		if errors.Is(err, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return io.EOF
}

func (b *requestBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

// discard reads the unread rest of the body so that the connection can serve another request. It reports false when
// the rest is too large, the client still waits for 100 Continue, or the read fails.
func (b *requestBody) discard() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return true
	}
	if b.expect || b.err != nil {
		return false
	}
	b.atEnd = nil
	_ = b.c.rwc.SetReadDeadline(time.Now().Add(headTimeout))
	defer func() { _ = b.c.rwc.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, 32<<10)
	for total := 0; total <= maxDiscard; {
		n, err := b.read(buf)
		total += n
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
	return false
}

// response writes one HTTP/1.1 response. The head is sent with the first body bytes, a flush or the handler's return,
// so a response without a body carries Content-Length: 0. A body of unknown length is chunked for an HTTP/1.1 client
// and delimited by the connection's end otherwise.
type response struct {
	c      *conn
	req    *http.Request
	header http.Header

	status   int
	head     http.Header // The header as it was when the status was set.
	sent     bool
	chunked  bool
	noBody   bool
	length   int64 // -1 when unknown.
	written  int64
	close    bool
	failed   error
	finished bool
}

func (w *response) Header() http.Header { return w.header }

// WriteHeader sets the status. An interim 1xx status cannot be relayed on this connection and aborts it.
func (w *response) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	if code < 200 || code > 999 {
		panic(http.ErrAbortHandler)
	}
	w.status = code
	w.head = w.header.Clone()
}

// sendHead writes the status line and header once; the caller holds c.wmu.
func (w *response) sendHead() {
	if w.sent {
		return
	}
	w.sent = true
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	h := w.head
	w.noBody = w.req.Method == http.MethodHead || w.status == http.StatusNoContent || w.status == http.StatusNotModified
	if v := h.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			w.length = n
		} else {
			h.Del("Content-Length")
		}
	}
	h.Del("Transfer-Encoding")
	switch {
	case w.noBody, w.length >= 0:
	case w.finished:
		w.length = 0
		h.Set("Content-Length", "0")
	case w.req.ProtoAtLeast(1, 1):
		w.chunked = true
		h.Set("Transfer-Encoding", "chunked")
	default:
		w.close = true
	}
	if w.close || w.req.Close || headerHasToken(h["Connection"], "close") || w.c.srv.isStopping() {
		w.close = true
		h.Set("Connection", "close")
	}
	if _, ok := h["Date"]; !ok {
		h.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	}
	w.c.answered = true
	text := http.StatusText(w.status)
	if text == "" {
		text = "status code " + strconv.Itoa(w.status)
	}
	if _, err := fmt.Fprintf(w.c.bw, "HTTP/1.1 %03d %s\r\n", w.status, text); err != nil {
		w.fail(err)
		return
	}
	if err := h.Write(w.c.bw); err != nil {
		w.fail(err)
		return
	}
	if _, err := w.c.bw.WriteString("\r\n"); err != nil {
		w.fail(err)
	}
}

func (w *response) fail(err error) {
	if w.failed == nil {
		w.failed = err
	}
	w.close = true
}

func (w *response) Write(p []byte) (int, error) {
	w.c.wmu.Lock()
	defer w.c.wmu.Unlock()
	if len(p) == 0 {
		return 0, w.failed
	}
	w.sendHead()
	switch {
	case w.failed != nil:
		return 0, w.failed
	case w.noBody:
		return 0, http.ErrBodyNotAllowed
	case w.length >= 0 && w.written+int64(len(p)) > w.length:
		w.close = true
		return 0, http.ErrContentLength
	}
	if w.chunked {
		if _, err := fmt.Fprintf(w.c.bw, "%x\r\n", len(p)); err != nil {
			w.fail(err)
			return 0, err
		}
	}
	n, err := w.c.bw.Write(p)
	w.written += int64(n)
	if err == nil && w.chunked {
		_, err = w.c.bw.WriteString("\r\n")
	}
	if err != nil {
		w.fail(err)
	}
	return n, err
}

// Flush sends the response head and the buffered body bytes to the client.
func (w *response) Flush() {
	w.c.wmu.Lock()
	defer w.c.wmu.Unlock()
	w.sendHead()
	if w.failed != nil {
		return
	}
	if err := w.c.bw.Flush(); err != nil {
		w.fail(err)
	}
}

// finish completes a response whose handler returned normally.
func (w *response) finish() {
	w.c.wmu.Lock()
	defer w.c.wmu.Unlock()
	w.finished = true
	w.sendHead()
	if w.failed != nil {
		return
	}
	if !w.noBody && w.length >= 0 && w.written < w.length {
		// The client must see the connection end rather than wait for bytes that never come.
		w.close = true
	}
	if w.chunked {
		if _, err := w.c.bw.WriteString("0\r\n\r\n"); err != nil {
			w.fail(err)
			return
		}
	}
	if err := w.c.bw.Flush(); err != nil {
		w.fail(err)
	}
}
