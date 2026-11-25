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

package config

import (
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Endpoint distinguishes an absent OTLP override from a present but disabled or invalid one.
type Endpoint struct {
	URL *url.URL
	Set bool
}

const (
	traceDefault = "http://127.0.0.1:4318/v1/traces"
	usageDefault = "http://127.0.0.1:4318/v1/logs"
	urlLimits    = " The port is stricter than the source's URL parser: after setting-specific trimming, it rejects backslashes, ASCII controls (including embedded tabs and newlines), credentials (even empty userinfo), and ambiguous numeric hosts. DNS labels must contain only ASCII letters, digits and hyphens, with no empty labels or punycode. Paths must use ASCII URL characters or complete percent escapes."
)

var (
	TraceEndpoint      = endpointKey("CODEX_GATEWAY_TELEMETRY_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, then OTEL_EXPORTER_OTLP_ENDPOINT, then "+traceDefault, "First OTLP route trace endpoint, when tracing is enabled.", "lib/commands.js:2103–2127; lib/request-worker.js:933–954", true)
	OTelTracesEndpoint = endpointKey("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT, then "+traceDefault, "Second OTLP route trace endpoint.", "lib/commands.js:2107; lib/request-worker.js:937", false)
	OTelLogsEndpoint   = endpointKey("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT, then "+usageDefault, "Second OTLP usage log endpoint.", "lib/usage-observability.js:624–626", false)
	OTelEndpoint       = endpointKey("OTEL_EXPORTER_OTLP_ENDPOINT", traceDefault+" for traces; "+usageDefault+" for usage logs", "Generic OTLP base, with /v1/traces or /v1/logs appended.", "lib/commands.js:2109–2125; lib/request-worker.js:939–951; lib/usage-observability.js:605–629", false)
	UsageEndpoint      = endpointKey("CODEX_GATEWAY_USAGE_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT, then OTEL_EXPORTER_OTLP_ENDPOINT, then "+usageDefault, "First OTLP usage log endpoint.", "lib/usage-observability.js:605–630", true)
)

func endpointKey(name, fallback, meaning, readers string, disable bool) Key[Endpoint] {
	rule := "Empty is unset. Require a loopback HTTP(S) URL, rewrite localhost to 127.0.0.1 and remove query and fragment. Invalid nonempty input, including a Unicode or punycode host, disables the selected signal without falling back."
	if disable {
		rule += ` The exact string "0" also disables this signal.`
	}
	return key(name, fallback, meaning, rule+urlLimits, readers, func(s string, _ bool) Endpoint {
		return Endpoint{URL: normalizeLoopback(s), Set: s != ""}
	})
}

// TraceURL returns the loopback OTLP route trace endpoint, or nil if tracing is disabled.
func TraceURL(lookup LookupEnv) *url.URL {
	if !Traceparent.Get(lookup) {
		return nil
	}
	return selectEndpoint(lookup, TraceEndpoint, OTelTracesEndpoint, "/v1/traces", traceDefault)
}

// UsageURL returns the loopback OTLP usage log endpoint, or nil if the selected endpoint is disabled.
func UsageURL(lookup LookupEnv) *url.URL {
	return selectEndpoint(lookup, UsageEndpoint, OTelLogsEndpoint, "/v1/logs", usageDefault)
}

func selectEndpoint(lookup LookupEnv, first, second Key[Endpoint], suffix, fallback string) *url.URL {
	for _, k := range []Key[Endpoint]{first, second, OTelEndpoint} {
		if ep := k.Get(lookup); ep.Set {
			if k.variable.Name == OTelEndpoint.variable.Name && ep.URL != nil {
				escaped := strings.TrimSuffix(ep.URL.EscapedPath(), "/") + suffix
				ep.URL.Path, _ = url.PathUnescape(escaped)
				ep.URL.RawPath = escaped
			}
			return ep.URL
		}
	}
	return normalizeLoopback(fallback)
}

func normalizeLoopback(raw string) *url.URL {
	u := parseEndpoint(raw)
	if u == nil || u.User != nil || u.Scheme != "http" && u.Scheme != "https" || !loopback(u.Hostname()) {
		return nil
	}
	if u.Hostname() == "localhost" {
		u.Host = "127.0.0.1" + strings.TrimPrefix(u.Host, "localhost")
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
	return u
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && !addr.Is4In6() && addr.IsLoopback()
}

// LMOrigin carries either a normalized LM Studio origin or the validation reason; both are empty when unset.
type LMOrigin struct {
	Origin string
	Reason string
}

func parseLMStudioOrigin(raw string) LMOrigin {
	if raw == "" {
		return LMOrigin{}
	}
	u := parseEndpoint(raw)
	if u == nil {
		return LMOrigin{Reason: "not a valid URL"}
	}
	host := u.Hostname()
	validScheme := u.Scheme == "https" || u.Scheme == "http" && (loopback(host) || host == "localhost.")
	switch {
	case host == "":
		return LMOrigin{Reason: "the URL has no host"}
	case !validScheme:
		return LMOrigin{Reason: "must use https (plain http only for a loopback host)"}
	case u.User != nil:
		return LMOrigin{Reason: "must not contain a user name or password"}
	case u.EscapedPath() != "/" || strings.ContainsAny(raw, "?#"):
		return LMOrigin{Reason: "must be a scheme, a host and a port only, without a path such as /v1"}
	default:
		return LMOrigin{Origin: u.Scheme + "://" + u.Host}
	}
}

// Parse only spellings for which the source's host, port and path are known.
func parseEndpoint(raw string) *url.URL {
	if strings.ContainsRune(raw, '\\') || strings.IndexFunc(raw, func(r rune) bool { return r < ' ' || r == 127 }) >= 0 {
		return nil
	}
	s := strings.Trim(raw, " ")
	scheme, rest, found := strings.Cut(s, ":")
	if !found {
		return nil
	}
	switch scheme = strings.ToLower(scheme); scheme {
	case "http", "https":
	default:
		// This URL can supply a more specific LM Studio diagnostic but is never accepted.
		u, err := url.Parse(s)
		if err != nil {
			return nil
		}
		return u
	}

	main, _, _ := strings.Cut(rest, "#")
	main, _, _ = strings.Cut(main, "?")
	authority, path, _ := strings.Cut(strings.TrimLeft(main, "/"), "/")
	if strings.IndexFunc(path, func(r rune) bool {
		return !asciiLetter(r) && (r < '0' || r > '9') && !strings.ContainsRune("/-._~!$&'()*+,;=:@%", r)
	}) >= 0 {
		return nil
	}
	userinfo, hostport, found := strings.CutLast(authority, "@")
	if !found {
		hostport = authority
	}
	if !strings.HasPrefix(hostport, "[") {
		host, port, hasPort := strings.Cut(hostport, ":")
		if !plainASCIIHost(host) {
			return nil
		}
		host, ok := whatwgIPv4(strings.ToLower(host))
		if !ok {
			return nil
		}
		hostport = host
		if hasPort {
			hostport += ":" + port
		}
	}
	if found {
		hostport = userinfo + "@" + hostport
	}
	u, err := url.Parse(scheme + "://" + hostport + "/" + path)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	host, port := u.Hostname(), u.Port()
	if strings.Contains(host, ":") || strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return nil
		}
		host = "[" + canonicalIPv6(addr) + "]"
	}
	u.Host = host
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return nil
		}
		if scheme != "http" || n != 80 {
			if scheme != "https" || n != 443 {
				u.Host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.FormatUint(n, 10))
			}
		}
	}
	normalizeEndpointPath(u)
	return u
}

func asciiLetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }

func plainASCIIHost(host string) bool {
	for label := range strings.SplitSeq(strings.TrimSuffix(host, "."), ".") {
		if label == "" || strings.HasPrefix(strings.ToLower(label), "xn--") {
			return false
		}
		for _, r := range label {
			if r != '-' && !asciiLetter(r) && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

func canonicalIPv6(addr netip.Addr) string {
	if !addr.Is4In6() {
		return addr.String()
	}
	// WHATWG serializes the last two groups of an IPv4-mapped address as hex.
	v := addr.As4()
	return "::ffff:" + strconv.FormatUint(uint64(v[0])<<8|uint64(v[1]), 16) + ":" + strconv.FormatUint(uint64(v[2])<<8|uint64(v[3]), 16)
}

func normalizeEndpointPath(u *url.URL) {
	var segments []string
	escaped := strings.TrimPrefix(u.EscapedPath(), "/")
	for remaining := escaped; ; {
		part, rest, more := strings.Cut(remaining, "/")
		switch strings.ReplaceAll(strings.ToLower(part), "%2e", ".") {
		case "..":
			if len(segments) != 0 {
				segments = segments[:len(segments)-1]
			}
			if !more {
				segments = append(segments, "")
			}
		case ".":
			if !more {
				segments = append(segments, "")
			}
		default:
			segments = append(segments, part)
		}
		if !more {
			break
		}
		remaining = rest
	}
	u.RawPath = "/" + strings.Join(segments, "/")
	u.Path, _ = url.PathUnescape(u.RawPath)
}

func whatwgIPv4(host string) (string, bool) {
	parts := strings.Split(strings.TrimSuffix(host, "."), ".")
	last := parts[len(parts)-1]
	_, numeric := ipv4Number(last)
	if !numeric && strings.Trim(last, "0123456789") != "" {
		// Invalid or oversized hexadecimal candidates must not be accepted as DNS names.
		return host, !strings.HasPrefix(last, "0x")
	}
	if len(parts) > 4 {
		return "", false
	}
	var address uint32
	for i, part := range parts {
		n, ok := ipv4Number(part)
		if !ok || i < len(parts)-1 && n > 255 || i == len(parts)-1 && uint64(n) >= 1<<uint(8*(5-len(parts))) {
			return "", false
		}
		if i < len(parts)-1 {
			address |= n << uint(8*(3-i))
		} else {
			address |= n
		}
	}
	return netip.AddrFrom4([4]byte{byte(address >> 24), byte(address >> 16), byte(address >> 8), byte(address)}).String(), true
}

func ipv4Number(part string) (uint32, bool) {
	if part == "" {
		return 0, false
	}
	base := 10
	if rest, ok := strings.CutPrefix(part, "0x"); ok {
		part, base = rest, 16
	} else if len(part) > 1 && part[0] == '0' {
		part, base = part[1:], 8
	}
	if part == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(part, base, 32)
	return uint32(n), err == nil
}
