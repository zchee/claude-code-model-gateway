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

// Package upstream filters headers and executes HTTP/1.1 requests to Anthropic, proxy, and Grok upstreams.
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTimeout reports that an upstream connection exceeded its one-hour socket-inactivity deadline.
var ErrTimeout = errors.New("upstream timeout")

// Target selects which upstream header policy and request rules apply.
type Target string

const (
	// TargetAnthropic forwards requests to the Anthropic upstream, stripping hop-by-hop,
	// control-token, and trace headers while keeping client authentication headers.
	TargetAnthropic Target = "anthropic"
	// TargetProxy forwards requests to the local proxy, additionally stripping client
	// authentication headers.
	TargetProxy Target = "proxy"
	// TargetGrok posts a translated body to the configured Grok endpoint using only
	// GrokHeaders and forwarding no client headers.
	TargetGrok Target = "grok"
)

// Options configures an upstream HTTP/1.1 client or standalone transport.
type Options struct {
	// DialContext overrides outbound connection dialing when non-nil.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSConfig optionally overrides TLS settings for HTTPS upstreams.
	// HTTP/1.1-only negotiation is enforced regardless of this setting.
	TLSConfig *tls.Config
}

// Request describes one outbound HTTP request to an upstream provider.
type Request struct {
	// Target selects the header policy and URL/method rules.
	Target Target
	// BaseURL is the upstream origin for Anthropic and proxy requests, or the full endpoint URL for Grok.
	BaseURL string
	// Method is the client HTTP method; ignored for TargetGrok, which always sends POST.
	Method string
	// RequestURI is the client request path and optional query; ignored for TargetGrok.
	RequestURI string
	// ClientHeaders holds incoming client headers filtered according to Target.
	ClientHeaders http.Header
	// GrokHeaders holds the explicit headers sent for TargetGrok requests.
	GrokHeaders http.Header
	// Body holds the exact request body bytes to send; nil or empty sends an empty body directly.
	Body []byte
}

// Response wraps an upstream HTTP response with separate parsed capture and normalized relay header views.
type Response struct {
	// StatusCode is the HTTP status code returned by the upstream.
	StatusCode int
	// Header holds the parsed capture copy of the upstream response headers returned by
	// net/http, with no additional trimming or joining, preserving repeated values in
	// order for rate-limit state and usage capture.
	Header http.Header
	// RelayHeader holds the normalized, independently cloned copy of the response headers
	// that may be written to the client: Transfer-Encoding, Connection, and Keep-Alive are
	// removed, single-value headers keep their first value, Cookie is joined with "; ",
	// Set-Cookie stays separate, other repeated headers are joined with ", ", and declared
	// chunked Trailer header names are preserved. Mutating RelayHeader never affects Header.
	RelayHeader http.Header
	// Body streams the upstream response bytes without automatic decompression.
	// Body is owned by exactly one consumer at a time and must be closed by its current
	// owner; a consumer that hands the open stream on (such as a reply handler transferring
	// a stream to a relay) must not close it.
	Body io.ReadCloser
}

// Send executes an outbound upstream request for an immutable Request bound once by the
// worker or branch caller to a Client. Requests carrying a body perform a single outbound
// attempt with no automatic replay; bodyless idempotent requests on a reused keep-alive
// connection may be retried by net/http if the connection fails before any response byte.
type Send func(context.Context) (*Response, error)

// errClientClosed reports a request made on, or ended by, a closed Client.
var errClientClosed = errors.New("upstream client closed")

// errResponseFailed replaces an error from the response side of an exchange. net/http quotes the
// upstream's bytes in some of those errors (a malformed status line, header line or framing
// value), and the error reaches the client in a gateway error body.
var errResponseFailed = errors.New("upstream response failed")

// tlsError reports a failed TLS handshake, certificate verification included, or a TLS failure
// after the handshake with fixed text. TLS errors quote values the upstream sent, such as the names
// in its certificate or the version of a record, and a verification hook of the caller can return
// an error of any shape; the error reaches the client in a gateway error body. Unwrap keeps the
// original error reachable for errors.Is and errors.As.
type tlsError struct {
	err error
}

// Error returns the fixed text of a TLS failure.
func (e *tlsError) Error() string {
	return "upstream TLS connection failed"
}

// Unwrap returns the error the TLS layer reported.
func (e *tlsError) Unwrap() error {
	return e.err
}

// isTLSError reports whether err wraps one of the exported error types the TLS layer returns.
func isTLSError(err error) bool {
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return true
	}
	if _, ok := errors.AsType[tls.AlertError](err); ok {
		return true
	}
	_, ok := errors.AsType[*tls.CertificateVerificationError](err)
	return ok
}

// idlePoolTimeout is how long an unused pooled connection waits for its next request. It is shorter
// than socketInactivity so that net/http evicts an idle connection under its own pool lock before
// the inactivity deadline closes it, which could otherwise happen while a request carrying a body,
// which net/http never retries, is taking the connection.
const idlePoolTimeout = 55 * time.Minute

// Client executes pooled HTTP/1.1 requests (up to 64 connections per host, 1-hour socket-inactivity
// timeout) to upstream providers without environment proxy lookup, automatic decompression,
// redirect following, or automatic replay of requests carrying a body.
type Client struct {
	transport *http.Transport
	lifetime  context.Context
	cancel    context.CancelFunc
}

// NewTransport constructs a standalone pooled HTTP/1.1 transport with the same safe policy
// used by NewClient: no environment proxy lookup, no automatic decompression, HTTP/1.1 only,
// at most 64 concurrent connections per host, and a one-hour per-socket inactivity deadline
// reporting ErrTimeout. Using RoundTrip directly follows no redirects.
// It takes no Target and builds no inference headers, allowing callers such as Grok OIDC
// credential refresh to hold a distinct transport instance isolated from an inference Client's
// lifetime.
//
// For HTTPS the TLS settings and the dial function come from opts, captured at construction: the
// returned transport's TLSClientConfig, TLSHandshakeTimeout and DialContext fields are not consulted
// for HTTPS after construction, so changing them does not change how HTTPS connections are made.
func NewTransport(opts Options) *http.Transport {
	return newTransport(opts, nil)
}

// newTransport builds the shared transport policy. A non-nil lifetime ends outstanding dials and
// closes every connection of the transport when it is done.
func newTransport(opts Options, lifetime context.Context) *http.Transport {
	// Clone so the caller's configuration is never changed, and offer only HTTP/1.1 in ALPN.
	tlsConfig := opts.TLSConfig.Clone()
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}
	tlsConfig.NextProtos = []string{"http/1.1"}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	// Start from a zero Transport: no environment proxy, and no timer other than the socket
	// inactivity deadline and the idle-pool timeout below it (the source sets no separate connect
	// or handshake timeout).
	return &http.Transport{
		DialContext: inactivityDialer(opts.DialContext, lifetime),
		// The transport's own handshake would hand back a failed handshake's error unmarked, so
		// the dial of an HTTPS connection performs it and marks a failure where it happens.
		DialTLSContext:      tlsDialer(opts.DialContext, tlsConfig, lifetime),
		TLSClientConfig:     tlsConfig,
		DisableCompression:  true,
		MaxConnsPerHost:     64,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     idlePoolTimeout,
		Protocols:           protocols,
	}
}

// NewClient constructs a pooled HTTP/1.1 upstream client using NewTransport with the given options.
func NewClient(opts Options) *Client {
	lifetime, cancel := context.WithCancel(context.Background())
	return &Client{
		transport: newTransport(opts, lifetime),
		lifetime:  lifetime,
		cancel:    cancel,
	}
}

// Do sends req over the upstream transport and returns the upstream response.
// ctx bounds the whole exchange, including reads of the returned Body; Close on the Client ends it
// too. The caller owns the returned Body and must close it. The error reports what ended the
// request: ErrTimeout as it is; when ctx ended first, the transport's error unchanged if it is
// ctx's error or cause; when Close ended it, the closed-client error. Otherwise a failed TLS
// handshake, and a TLS failure or any other failure after the connection was obtained, are each
// reported as one fixed error, so no upstream bytes reach the error text; the fixed error of a TLS
// failure unwraps to the TLS layer's error. A failed dial keeps its own error.
//
// ctx's cause is recognised only when its value can be compared. A cause whose value cannot be
// compared (a struct whose interface field holds a slice, for example) is never matched: Do then
// returns the transport's error as it is when no connection was obtained, and otherwise classifies
// the failure like any other after the connection was obtained, which for most failures is the
// fixed response error, or the closed-client error once Close was called. Such a cause never
// makes Do panic.
//
// When ctx ended and Close was called both before the exchange failed, Do reports one of the two,
// ctx's error or cause or the closed-client error; which one is not specified. A Close that starts
// after the exchange already failed with ctx's error or cause does not replace it.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	if c == nil || c.transport == nil {
		return nil, errors.New("upstream client not constructed by NewClient")
	}
	if c.lifetime.Err() != nil {
		return nil, errClientClosed
	}
	outbound, err := buildRequest(req)
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(c.lifetime, func() { cancel(errClientClosed) })
	release := func() {
		stop()
		cancel(nil)
	}
	// connected reports that the current attempt obtained a connection; net/http starts every
	// attempt, including each retry, with GetConn.
	var connected atomic.Bool
	traced := httptrace.WithClientTrace(reqCtx, &httptrace.ClientTrace{
		GetConn: func(string) { connected.Store(false) },
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	resp, err := c.transport.RoundTrip(outbound.WithContext(traced)) //nolint:bodyclose // The body is handed to the caller inside Response, which owns closing it.
	if err != nil {
		// The cause names what ended the request: the caller's context, which passes its own cause
		// on, or the client's closing. It is read before release ends the context itself.
		cause := context.Cause(reqCtx)
		release()
		return nil, c.exchangeError(ctx, cause, err, connected.Load())
	}

	relay := ResponseHeaders(resp.Header)
	// net/http moves the Trailer declaration of a chunked response out of the header map.
	if _, declared := relay["Trailer"]; !declared && len(resp.Trailer) > 0 {
		relay["Trailer"] = []string{strings.Join(slices.Sorted(maps.Keys(resp.Trailer)), ", ")}
	}
	return &Response{
		StatusCode:  resp.StatusCode,
		Header:      resp.Header,
		RelayHeader: relay,
		Body:        &responseBody{body: resp.Body, release: release},
	}, nil
}

// Close cancels in-flight upstream requests and closes idle connections for service shutdown.
// It is safe to call more than once. A request whose context had also ended before its exchange
// failed reports either its context's error or cause or the closed-client error, which one is not
// specified; a Close that starts after an exchange already failed with its context's error or cause
// does not replace that error.
func (c *Client) Close() error {
	if c == nil || c.cancel == nil {
		return nil
	}
	c.cancel()
	c.transport.CloseIdleConnections()
	return nil
}

// exchangeError returns the error Do reports for err, the transport's error of a request whose
// context ended with cause (nil when nothing ended it) and whose last attempt obtained a
// connection when connected. The class is decided by what ended the request, by where the error
// arose and by its type, never by its text.
func (c *Client) exchangeError(ctx context.Context, cause, err error, connected bool) error {
	if errors.Is(err, ErrTimeout) {
		return err
	}
	if cause != nil && !errors.Is(cause, errClientClosed) && (errors.Is(err, ctx.Err()) || isCause(err, cause)) {
		return err
	}
	if c.lifetime.Err() != nil {
		// Closing the client also closes its connections, and the transport can report the
		// broken connection before it sees the cancelled request.
		return errClientClosed
	}
	if tlsErr, ok := err.(*tlsError); ok { //nolint:errorlint // Only the marker the handshake step returned itself counts; a custom dial's error that wraps one keeps its own identity.
		// The dial marked a failed handshake. net/http hands the TLS dial's error back as it is, so
		// the marker is the error itself; one found deeper in a dial error's chain is not renamed.
		return tlsErr
	}
	if !connected {
		// A failed dial keeps its error, whatever its type: the dial is the caller's or the
		// system's, and the upstream sent nothing yet.
		return err
	}
	if isTLSError(err) {
		return &tlsError{err: err}
	}
	return errResponseFailed
}

// isCause reports whether err is, or wraps, cause. A caller's cause can be any value, and errors.Is
// compares with == whenever the cause's type is comparable, which panics when its value is not (a
// struct whose interface field holds a slice, for example). A cause whose value is not comparable
// is therefore never matched.
func isCause(err, cause error) bool {
	return reflect.ValueOf(cause).Comparable() && errors.Is(err, cause)
}

// responseBody owns a live upstream response body and the request context bound to it.
type responseBody struct {
	body    io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

// Read reads the upstream response bytes as received. An error of an exported TLS type is reported
// as a tlsError, since its text can quote bytes the upstream sent; every other error, io.EOF
// included, is returned as it is.
func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if err != nil && isTLSError(err) {
		return n, &tlsError{err: err}
	}
	return n, err
}

// Close closes the upstream body and releases its request context; later calls return the
// first call's result.
func (b *responseBody) Close() error {
	b.once.Do(func() {
		b.err = b.body.Close()
		b.release()
	})
	return b.err
}

// buildRequest turns req into an outbound HTTP/1.1 request without a context.
func buildRequest(req Request) (*http.Request, error) {
	var (
		method string
		target *url.URL
		header http.Header
		err    error
	)
	switch req.Target {
	case TargetAnthropic, TargetProxy:
		method = req.Method
		if target, err = resolveTarget(req.BaseURL, req.RequestURI); err != nil {
			return nil, err
		}
		header = RequestHeaders(req.ClientHeaders, req.Target)
	case TargetGrok:
		method = http.MethodPost
		if target, err = parseBaseURL(req.BaseURL); err != nil {
			return nil, err
		}
		header = make(http.Header, len(req.GrokHeaders))
		// Sorted, so that names differing only in case merge in the same order every time.
		for _, name := range slices.Sorted(maps.Keys(req.GrokHeaders)) {
			for _, value := range req.GrokHeaders[name] {
				header.Add(name, value)
			}
		}
	default:
		return nil, errors.New("upstream: unknown target")
	}
	// An empty User-Agent entry stops net/http from sending its own; the source adds none.
	if _, ok := header["User-Agent"]; !ok {
		header["User-Agent"] = []string{""}
	}

	outbound := &http.Request{
		Method:     method,
		URL:        target,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     header,
		Host:       target.Host,
		Body:       http.NoBody,
	}
	if len(req.Body) > 0 {
		// No GetBody: net/http may replay only a request it can rewind, so a request with a body
		// is never sent twice by the transport.
		outbound.Body = io.NopCloser(bytes.NewReader(req.Body))
		outbound.ContentLength = int64(len(req.Body))
	}
	return outbound, nil
}

// parseBaseURL parses an absolute http or https upstream URL.
func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("upstream: invalid base URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("upstream: base URL scheme must be http or https")
	}
	if u.Host == "" {
		return nil, errors.New("upstream: base URL has no host")
	}
	return u, nil
}

// resolveTarget resolves the client request target against the upstream origin. A target that
// names its own scheme or authority is refused, so client credentials never leave for an origin
// other than the configured upstream.
func resolveTarget(baseURL, requestURI string) (*url.URL, error) {
	base, err := parseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(requestURI)
	if err != nil {
		return nil, errors.New("upstream: invalid request target")
	}
	if ref.Scheme != "" || ref.Host != "" || ref.User != nil || ref.Opaque != "" || strings.HasPrefix(requestURI, "//") {
		return nil, errors.New("upstream: request target names its own origin")
	}
	return base.ResolveReference(ref), nil
}
