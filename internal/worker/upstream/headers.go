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

package upstream

import (
	"maps"
	"net/http"
	"slices"
	"strings"
)

// requestDrop lists the lowercase client header names never forwarded to the Anthropic or proxy
// upstream: the hop-by-hop set, the recomputed length, the gateway control token and the trace
// context headers.
var requestDrop = map[string]struct{}{
	"host":                          {},
	"connection":                    {},
	"content-length":                {},
	"keep-alive":                    {},
	"x-model-gateway-control-token": {},
	"traceparent":                   {},
	"tracestate":                    {},
	"baggage":                       {},
}

// authDrop lists the lowercase client credential header names that never reach the local proxy.
var authDrop = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"x-api-key":           {},
	"cookie":              {},
}

// responseDrop lists the lowercase upstream response header names never relayed to the client.
var responseDrop = map[string]struct{}{
	"transfer-encoding": {},
	"connection":        {},
	"keep-alive":        {},
}

// singletonFields lists the lowercase header names for which Node's parsed header object keeps
// only the first occurrence and discards the rest.
var singletonFields = map[string]struct{}{
	"age":                 {},
	"authorization":       {},
	"content-length":      {},
	"content-type":        {},
	"etag":                {},
	"expires":             {},
	"from":                {},
	"host":                {},
	"if-modified-since":   {},
	"if-unmodified-since": {},
	"last-modified":       {},
	"location":            {},
	"max-forwards":        {},
	"proxy-authorization": {},
	"referer":             {},
	"retry-after":         {},
	"server":              {},
	"user-agent":          {},
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
	switch target {
	case TargetAnthropic:
		return normalizeHeaders(in, func(name string) bool {
			_, drop := requestDrop[name]
			return drop
		})
	case TargetProxy:
		return normalizeHeaders(in, func(name string) bool {
			_, drop := requestDrop[name]
			_, auth := authDrop[name]
			return drop || auth
		})
	default:
		return http.Header{}
	}
}

// ResponseHeaders returns a fresh normalized relay copy of upstream response headers with only
// Transfer-Encoding, Connection, and Keep-Alive removed case-insensitively.
// Differently cased occurrences of the same name collapse into one field, single-value
// headers keep their first value, Cookie values are joined with "; ", Set-Cookie values
// remain separate, and other repeated headers are joined with ", ".
// Values are not additionally trimmed, and in is never mutated.
func ResponseHeaders(in http.Header) http.Header {
	return normalizeHeaders(in, func(name string) bool {
		_, drop := responseDrop[name]
		return drop
	})
}

// normalizeHeaders folds in into a new header map the way Node builds a parsed header object,
// skipping every field whose lowercase name drop reports. Keys are visited in sorted order so the
// merge of differently cased occurrences is deterministic, and every value slice is newly
// allocated so the result never aliases in.
func normalizeHeaders(in http.Header, drop func(lowerName string) bool) http.Header {
	out := make(http.Header, len(in))
	for _, key := range slices.Sorted(maps.Keys(in)) {
		lower := strings.ToLower(key)
		if drop(lower) {
			continue
		}
		// Canonicalizing the lowercase spelling gives every case variant the same key, including
		// names that are not valid tokens, which CanonicalHeaderKey returns unchanged.
		name := http.CanonicalHeaderKey(lower)
		for _, value := range in[key] {
			addNodeValue(out, name, lower, value)
		}
	}
	return out
}

// addNodeValue adds one occurrence of a header field to out following Node's rules for
// repeated fields.
func addNodeValue(out http.Header, name, lower, value string) {
	prev, seen := out[name]
	switch {
	case lower == "set-cookie":
		out[name] = append(prev, value)
	case !seen:
		out[name] = []string{value}
	case lower == "cookie":
		prev[0] += "; " + value
	default:
		if _, single := singletonFields[lower]; !single {
			prev[0] += ", " + value
		}
	}
}
