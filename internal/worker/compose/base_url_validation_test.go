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

package compose_test

import (
	"strings"
	"testing"

	"github.com/zchee/claude-code-model-gateway/internal/worker/compose"
	"github.com/zchee/claude-code-model-gateway/internal/worker/upstream"
)

// TestBaseURLValidation decides each base URL on its own: the other one keeps the valid value minimalOptions gives it.
// An unusable value is refused with an error that names the setting and repeats no part of the value, which can
// carry credentials; an absolute http or https URL is accepted with or without a port or a path.
func TestBaseURLValidation(t *testing.T) {
	// The texts restate the errors validate returns in internal/worker/compose/compose.go.
	const (
		anthropicError = "compose: AnthropicBaseURL must be an absolute http or https URL"
		proxyError     = "compose: ProxyBaseURL must be an absolute http or https URL"
	)
	// Each refused value carries its own user, password, host, port, path, query key and query value. An http value
	// also forbids its scheme prefix with the next character of the value, because "http" alone is in the error text.
	invalid := map[string]struct {
		value     string
		forbidden []string
	}{
		"a value that does not parse": {
			value:     "http://fixture-user:fixture-pass@[fixture-host:28417/fixture-path?fixture-query=fixture-value",
			forbidden: []string{"http://f", "fixture-user", "fixture-pass", "fixture-host", "28417", "fixture-path", "fixture-query", "fixture-value"},
		},
		"an http value with an empty host": {
			value:     "http:fixture-user:fixture-pass@fixture-host:28417/fixture-path?fixture-query=fixture-value",
			forbidden: []string{"http:f", "fixture-user", "fixture-pass", "fixture-host", "28417", "fixture-path", "fixture-query", "fixture-value"},
		},
		"a value with a host and no scheme": {
			value:     "//fixture-user:fixture-pass@fixture-host:28417/fixture-path?fixture-query=fixture-value",
			forbidden: []string{"fixture-user", "fixture-pass", "fixture-host", "28417", "fixture-path", "fixture-query", "fixture-value"},
		},
		"a value with a non-http scheme": {
			value:     "ftp://fixture-user:fixture-pass@fixture-host:28417/fixture-path?fixture-query=fixture-value",
			forbidden: []string{"ftp:", "fixture-user", "fixture-pass", "fixture-host", "28417", "fixture-path", "fixture-query", "fixture-value"},
		},
	}
	valid := map[string]string{
		"an http origin with a port":  "http://127.0.0.1:18766",
		"an https origin with a port": "https://upstream.invalid:8443",
		"an http URL with a path":     "http://127.0.0.1:18766/fixture-prefix",
		"an https URL with a path":    "https://upstream.invalid/v1/fixture-prefix",
	}
	settings := map[string]struct {
		set     func(*compose.Options, string)
		wantErr string
	}{
		"AnthropicBaseURL": {set: func(o *compose.Options, v string) { o.AnthropicBaseURL = v }, wantErr: anthropicError},
		"ProxyBaseURL":     {set: func(o *compose.Options, v string) { o.ProxyBaseURL = v }, wantErr: proxyError},
	}

	type testCase struct {
		set       func(*compose.Options, string)
		value     string
		wantErr   string
		forbidden []string
	}
	tests := map[string]testCase{}
	for setting, s := range settings {
		for kind, tc := range invalid {
			tests["error: "+setting+" refuses "+kind] = testCase{set: s.set, value: tc.value, wantErr: s.wantErr, forbidden: tc.forbidden}
		}
		for kind, value := range valid {
			tests["success: "+setting+" accepts "+kind] = testCase{set: s.set, value: value}
		}
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			client := upstream.NewClient(upstream.Options{})
			t.Cleanup(func() { _ = client.Close() })
			opts := minimalOptions(t, t.Context(), client)
			tt.set(&opts, tt.value)
			_, err := compose.New(opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("compose.New refused an accepted base URL: got error %v, want nil", err)
				}
				return
			}
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			for _, fragment := range tt.forbidden {
				if strings.Contains(gotErr, fragment) {
					t.Errorf("compose.New error repeats a part of the refused value: %q", fragment)
				}
			}
			mustDiff(t, "compose.New error", tt.wantErr, gotErr)
		})
	}
}
