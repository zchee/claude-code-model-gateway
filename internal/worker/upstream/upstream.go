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
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
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

// Options configures an upstream HTTP/1.1 client.
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
	// Header holds the parsed upstream response headers returned by net/http without
	// additional trimming or joining, preserving repeated values in order for capture
	// and upstream-state tracking.
	Header http.Header
	// RelayHeader holds the normalized outbound relay headers with Transfer-Encoding,
	// Connection, and Keep-Alive removed, single-value headers reduced to their first value,
	// Cookie joined with "; ", Set-Cookie kept separate, other repeated headers joined
	// with ", ", and declared chunked Trailer header names preserved.
	RelayHeader http.Header
	// Body streams the upstream response bytes without automatic decompression.
	Body io.ReadCloser
}

// Client executes pooled HTTP/1.1 requests (up to 64 connections per host, 1-hour socket-inactivity
// timeout) to upstream providers without environment proxy lookup, automatic decompression,
// redirect following, or automatic replay of requests carrying a body.
type Client struct{}

// NewClient constructs a pooled HTTP/1.1 upstream client with the given options.
func NewClient(opts Options) *Client {
	_ = opts
	return &Client{}
}

// Do sends req over the upstream transport and returns the upstream response.
func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	_, _, _ = c, ctx, req
	return nil, errors.New("not implemented")
}

// Close cancels in-flight upstream requests and closes idle connections for service shutdown.
// It is safe to call more than once.
func (c *Client) Close() error {
	_ = c
	return errors.New("not implemented")
}

// RequestHeaders returns a fresh normalized copy of incoming client headers filtered for target.
// For TargetAnthropic, Host, Connection, Content-Length, Keep-Alive,
// X-Model-Gateway-Control-Token, Traceparent, Tracestate, and Baggage are removed.
// For TargetProxy, Authorization, Proxy-Authorization, X-Api-Key, and Cookie are also removed.
// For TargetGrok or any unknown target, all client headers are removed.
// Remaining headers are normalized to match the source runtime's parsed header object:
// differently cased occurrences of the same name collapse into one field, single-value
// headers keep their first value, Cookie values are joined with "; ", Set-Cookie values
// remain separate, and other repeated headers are joined with ", ".
// The input map is never mutated.
func RequestHeaders(in http.Header, target Target) http.Header {
	_, _ = in, target
	return nil
}

// ResponseHeaders returns a fresh normalized relay copy of upstream response headers with only
// Transfer-Encoding, Connection, and Keep-Alive removed case-insensitively.
// Differently cased occurrences of the same name collapse into one field, single-value
// headers keep their first value, Cookie values are joined with "; ", Set-Cookie values
// remain separate, and other repeated headers are joined with ", ".
// Values are not additionally trimmed, and in is never mutated.
func ResponseHeaders(in http.Header) http.Header {
	_ = in
	return nil
}
