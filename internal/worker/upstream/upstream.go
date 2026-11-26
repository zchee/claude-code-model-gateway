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
		DialContext:         inactivityDialer(opts.DialContext, lifetime),
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
// too. The caller owns the returned Body and must close it. A failure after the connection was
// obtained is reported as one fixed error unless it is ErrTimeout, the context's cancellation or
// deadline, or the client's closing, so no upstream bytes reach the error text.
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
	// connected tells a failure of the attempt's connection setup (dial, TLS) from one after a
	// connection was obtained; net/http starts a new attempt with GetConn when it retries.
	var connected atomic.Bool
	traced := httptrace.WithClientTrace(reqCtx, &httptrace.ClientTrace{
		GetConn: func(string) { connected.Store(false) },
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	resp, err := c.transport.RoundTrip(outbound.WithContext(traced)) //nolint:bodyclose // The body is handed to the caller inside Response, which owns closing it.
	if err != nil {
		release()
		if c.lifetime.Err() != nil {
			// Closing the client also closes its connections, and the transport can report the
			// broken connection before it sees the cancelled request.
			return nil, errClientClosed
		}
		return nil, exchangeError(err, connected.Load())
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
// It is safe to call more than once.
func (c *Client) Close() error {
	if c == nil || c.cancel == nil {
		return nil
	}
	c.cancel()
	c.transport.CloseIdleConnections()
	return nil
}

// exchangeError keeps the identity of an error that cannot carry upstream bytes: a failure before
// the attempt had a connection (dial, TLS handshake), the inactivity timeout, the request
// context's cancellation or deadline, and the client's own closing. Every other error is replaced
// by errResponseFailed, without inspecting its text.
func exchangeError(err error, connected bool) error {
	if !connected ||
		errors.Is(err, ErrTimeout) ||
		errors.Is(err, errClientClosed) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errResponseFailed
}

// responseBody owns a live upstream response body and the request context bound to it.
type responseBody struct {
	body    io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

// Read reads the upstream response bytes as received.
func (b *responseBody) Read(p []byte) (int, error) {
	return b.body.Read(p)
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
