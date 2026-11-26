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

package controlauth_test

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/controlauth"
)

func TestMissingStoredTokenNeverAuthorizes(t *testing.T) {
	const header = "X-Model-Gateway-Control-Token"
	const lower = "x-model-gateway-control-token"
	fixture := strings.Repeat("a", 64)
	// An absent file yields no stored token, and the server-side format check
	// cannot pass regardless of the supplied header (lib/control-auth.js:12-21,45-53).
	tests := map[string]struct{ headers http.Header }{
		"error: absent":                    {},
		"error: empty value list":          {http.Header{header: nil}},
		"error: empty value":               {http.Header{header: {""}}},
		"error: malformed value":           {http.Header{header: {"malformed"}}},
		"error: well-formed value":         {http.Header{header: {fixture}}},
		"error: noncanonical spelling":     {http.Header{lower: {fixture}}},
		"error: repeated value":            {http.Header{header: {fixture, fixture}}},
		"error: duplicate header spelling": {http.Header{header: {fixture}, lower: {fixture}}},
		"error: comma-joined value":        {http.Header{header: {fixture + ", " + fixture}}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			token, err := controlauth.ReadToken(filepath.Join(t.TempDir(), "control-token"))
			if err != nil {
				t.Fatal(err)
			}
			if token != "" {
				t.Fatal("a missing token file returned a nonempty token")
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/restart", nil)
			req.Host = "127.0.0.1:12345"
			maps.Copy(req.Header, tt.headers)
			req.Header.Set("Content-Type", "application/json")
			if diff := gocmp.Diff(false, controlauth.Authenticated(req, token, []int{12345}, false)); diff != "" {
				t.Fatalf("authorization without stored token (-want +got):\n%s", diff)
			}
		})
	}
}

func TestControlHeaderSpellings(t *testing.T) {
	const header = "X-Model-Gateway-Control-Token"
	fixture := strings.Repeat("a", 64)
	// Node normalizes header names, preserves Origin presence, and joins
	// duplicate custom headers (lib/control-auth.js:41-53).
	tests := map[string]struct {
		headers           http.Header
		local, authorized bool
	}{
		"success: raw lowercase keys":       {http.Header{"content-type": {"application/json"}, "x-model-gateway-control-token": {fixture}}, true, true},
		"error: empty raw lowercase origin": {http.Header{"Content-Type": {"application/json"}, header: {fixture}, "origin": {""}}, false, false},
		"error: mixed-case origin":          {http.Header{"Content-Type": {"application/json"}, header: {fixture}, "oRiGiN": {"", "null"}}, false, false},
		"error: repeated token spellings":   {http.Header{"Content-Type": {"application/json"}, header: {fixture}, "x-model-gateway-control-token": {fixture}}, true, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/restart", nil)
			req.Host = "127.0.0.1:12345"
			req.Header = tt.headers
			if diff := gocmp.Diff(tt.local, controlauth.LocalRequestAllowed(req, []string{req.Host})); diff != "" {
				t.Errorf("local predicate (-want +got):\n%s", diff)
			}
			if diff := gocmp.Diff(tt.authorized, controlauth.Authenticated(req, fixture, []int{12345}, false)); diff != "" {
				t.Errorf("authorization (-want +got):\n%s", diff)
			}
		})
	}
}
