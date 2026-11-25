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
	"testing"

	gocmp "github.com/google/go-cmp/cmp"
)

func TestEndpointDecisions(t *testing.T) {
	const (
		invalidURL = "not a valid URL"
		noHost     = "the URL has no host"
		schemeRule = "must use https (plain http only for a loopback host)"
		noUserInfo = "must not contain a user name or password"
		originOnly = "must be a scheme, a host and a port only, without a path such as /v1"
	)
	tests := map[string]struct {
		value string
		otlp  string
		lm    LMOrigin
	}{
		"success: blank is unset": {},
		"success: trim LM whitespace, normalize scheme host and default port": {
			value: " HTTPS://LOCALHOST:0443/ ", otlp: "https://127.0.0.1/", lm: LMOrigin{Origin: "https://localhost"},
		},
		"success: nondefault port and root path": {
			value: "http://127.0.0.2:1234", otlp: "http://127.0.0.2:1234/", lm: LMOrigin{Origin: "http://127.0.0.2:1234"},
		},
		"success: decimal short IPv4": {
			value: "http://127.1:80", otlp: "http://127.0.0.1/", lm: LMOrigin{Origin: "http://127.0.0.1"},
		},
		"success: octal and hexadecimal IPv4 parts": {
			value: "http:0177.0x1", otlp: "http://127.0.0.1/", lm: LMOrigin{Origin: "http://127.0.0.1"},
		},
		"success: canonicalized IPv6 host and numeric port": {
			value: "http://[0:0:0:0:0:0:0:1]:0081", otlp: "http://[::1]:81/", lm: LMOrigin{Origin: "http://[::1]:81"},
		},
		"success: dotted localhost accepted only for LM Studio": {
			value: "http://LOCALHOST.:80/", lm: LMOrigin{Origin: "http://localhost."},
		},
		"success: remote HTTPS origin accepted only for LM Studio": {
			value: "https://Example.test:9443/", lm: LMOrigin{Origin: "https://example.test:9443"},
		},
		"success: normalize encoded dots and preserve remaining escape spelling": {
			value: "http://127.0.0.1/a/%2e./b%2fc/?discard=1#discard", otlp: "http://127.0.0.1/b%2fc/", lm: LMOrigin{Reason: originOnly},
		},
		"success: normalize path to root before deciding LM origin": {
			value: "http://localhost/a/..", otlp: "http://127.0.0.1/", lm: LMOrigin{Origin: "http://localhost"},
		},
		"error: malformed URL":                                    {value: "not a URL", lm: LMOrigin{Reason: invalidURL}},
		"error: empty host":                                       {value: "mailto:example.test", lm: LMOrigin{Reason: noHost}},
		"error: unsupported scheme":                               {value: "ftp://localhost/", lm: LMOrigin{Reason: schemeRule}},
		"error: plain HTTP external host":                         {value: "http://example.test", lm: LMOrigin{Reason: schemeRule}},
		"error: IPv4-mapped IPv6 is not loopback":                 {value: "http://[::ffff:127.0.0.1]", lm: LMOrigin{Reason: schemeRule}},
		"error: port out of range":                                {value: "http://localhost:65536/", lm: LMOrigin{Reason: invalidURL}},
		"error: invalid numeric address":                          {value: "http://127.256.0.1/", lm: LMOrigin{Reason: invalidURL}},
		"error: oversized hexadecimal host is not a DNS fallback": {value: "https://0x100000000/", lm: LMOrigin{Reason: invalidURL}},
		"error: non-root LM path":                                 {value: "https://127.0.0.1/v1", otlp: "https://127.0.0.1/v1", lm: LMOrigin{Reason: originOnly}},
		"error: empty query still disallowed for LM":              {value: "https://127.0.0.1/?", otlp: "https://127.0.0.1/", lm: LMOrigin{Reason: originOnly}},
		"error: empty fragment still disallowed for LM":           {value: "https://127.0.0.1/#", otlp: "https://127.0.0.1/", lm: LMOrigin{Reason: originOnly}},
		"error: conservative reject credentials":                  {value: "http://user:pass@127.0.0.1/", lm: LMOrigin{Reason: noUserInfo}},
		"error: conservative reject empty userinfo":               {value: "http://@127.0.0.1/", lm: LMOrigin{Reason: noUserInfo}},
		"error: conservative reject escaped host":                 {value: "http://%6cocalhost/", lm: LMOrigin{Reason: invalidURL}},
		"error: conservative reject non-ASCII hostname":           {value: "https://bücher.example/", lm: LMOrigin{Reason: invalidURL}},
		"error: conservative reject punycode hostname":            {value: "https://xn--bcher-kva.example/", lm: LMOrigin{Reason: invalidURL}},
		"error: conservative reject backslash":                    {value: `http:\localhost`, lm: LMOrigin{Reason: invalidURL}},
		"error: conservative reject embedded tab":                 {value: "http://local\thost/", lm: LMOrigin{Reason: invalidURL}},
		"error: conservative reject embedded newline":             {value: "http://local\nhost/", lm: LMOrigin{Reason: invalidURL}},
		"error: conservative reject incomplete percent escape":    {value: "http://localhost/%ab%", lm: LMOrigin{Reason: invalidURL}},
		"error: conservative reject literal spaces in path":       {value: "http://localhost/an endpoint", lm: LMOrigin{Reason: invalidURL}},
	}
	for label, tt := range tests {
		t.Run(label, func(t *testing.T) {
			var otlp string
			if got := normalizeLoopback(tt.value); got != nil {
				otlp = got.String()
			}
			if diff := gocmp.Diff(tt.otlp, otlp); diff != "" {
				t.Errorf("OTLP result (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.lm, parseLMStudioOrigin(jsTrim(tt.value))); diff != "" {
				t.Errorf("LM Studio result (-want +got):\n%s", diff)
			}
		})
	}
}
