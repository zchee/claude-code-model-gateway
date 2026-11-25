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

package proxyconfig_test

import (
	"os"
	"path/filepath"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/proxyconfig"
	"github.com/zchee/claude-code-model-gateway/internal/venue"
)

func TestMain(m *testing.M) {
	venue.Require()
	os.Exit(m.Run())
}

func TestStudioSettings(t *testing.T) {
	const baseURL = "CCP_LMSTUDIO_BASE_URL"
	const key = "CCP_LMSTUDIO_API_KEY"
	const token = "LM_API_TOKEN"
	const enabled = "CCP_LMSTUDIO_ENABLED"
	const defaultURL = "http://127.0.0.1:1234"
	const fileURL = "http://localhost:4321"
	const fromFile = `{"lmstudio":{"baseUrl":"http://localhost:4321/","apiKey":"from-file"}}`
	const off = `{"lmstudio":{"enabled":false}}`
	const scheme = "must use https (plain http only for a loopback host)"
	const path = "must be a scheme, a host and a port only, without a path such as /v1"
	valid := func(url, key string) proxyconfig.StudioSettings {
		return proxyconfig.StudioSettings{Enabled: true, BaseURL: url, APIKey: key}
	}
	invalid := func(setting, reason string) proxyconfig.StudioSettings {
		return proxyconfig.StudioSettings{Enabled: true, Invalid: &proxyconfig.InvalidSetting{Setting: setting, Reason: reason}}
	}
	tests := map[string]struct {
		env  map[string]string
		file string
		want proxyconfig.StudioSettings
	}{
		"success: default server":                         {want: valid(defaultURL, "")},
		"success: file origin and key":                    {file: fromFile, want: valid(fileURL, "from-file")},
		"success: environment overrides file":             {map[string]string{baseURL: "http://[::1]:1235", key: "from-env"}, fromFile, valid("http://[::1]:1235", "from-env")},
		"success: proxy key precedes provider key":        {env: map[string]string{key: "ccp", token: "lm"}, want: valid(defaultURL, "ccp")},
		"success: provider key precedes file":             {map[string]string{token: "lm"}, fromFile, valid(fileURL, "lm")},
		"success: blank overrides ignored":                {map[string]string{baseURL: "  ", key: ""}, fromFile, valid(fileURL, "from-file")},
		"success: trim origin":                            {env: map[string]string{baseURL: " http://127.0.0.1:9999 "}, want: valid("http://127.0.0.1:9999", "")},
		"success: fully qualified localhost":              {env: map[string]string{baseURL: "http://LOCALHOST.:1234"}, want: valid("http://localhost.:1234", "")},
		"success: loopback range":                         {env: map[string]string{baseURL: "http://127.8.9.10:1234/"}, want: valid("http://127.8.9.10:1234", "")},
		"success: remote HTTPS":                           {env: map[string]string{baseURL: "https://lmstudio.example:8443"}, want: valid("https://lmstudio.example:8443", "")},
		"success: unknown enabled value":                  {env: map[string]string{enabled: "maybe"}, want: valid(defaultURL, "")},
		"success: override disabled file":                 {map[string]string{enabled: " ON "}, off, valid(defaultURL, "")},
		"success: disabled in file":                       {file: off},
		"success: disabled by zero":                       {env: map[string]string{enabled: "0"}},
		"success: disabled by false":                      {env: map[string]string{enabled: "False"}},
		"success: disabled by no":                         {env: map[string]string{enabled: "no"}},
		"success: disabled by off":                        {env: map[string]string{enabled: "off"}},
		"error: cleartext remote":                         {env: map[string]string{baseURL: "http://lmstudio.example:1234"}, want: invalid(baseURL, scheme)},
		"error: non-root path":                            {env: map[string]string{baseURL: defaultURL + "/v1"}, want: invalid(baseURL, path)},
		"error: query":                                    {env: map[string]string{baseURL: defaultURL + "/?x=1"}, want: invalid(baseURL, path)},
		"error: fragment":                                 {env: map[string]string{baseURL: defaultURL + "/#x"}, want: invalid(baseURL, path)},
		"error: user information":                         {env: map[string]string{baseURL: "http://user:pass@127.0.0.1:1234"}, want: invalid(baseURL, "must not contain a user name or password")},
		"error: missing host":                             {env: map[string]string{baseURL: "localhost:1234"}, want: invalid(baseURL, "the URL has no host")},
		"error: malformed URL":                            {env: map[string]string{baseURL: "lm studio"}, want: invalid(baseURL, "not a valid URL")},
		"error: mistyped field invalidates whole section": {file: `{"lmstudio":{"enabled":"false","baseUrl":"http://127.0.0.1:4321","apiKey":"from-file"}}`, want: valid(defaultURL, "")},
		"error: section is array":                         {file: `{"lmstudio":["http://127.0.0.1:4321"]}`, want: valid(defaultURL, "")},
		"success: null fields ignored":                    {file: `{"lmstudio":{"enabled":null,"baseUrl":null,"apiKey":"from-file"}}`, want: valid(defaultURL, "from-file")},
		"error: origin from file named":                   {file: `{"lmstudio":{"baseUrl":"ftp://127.0.0.1:1234"}}`, want: invalid("lmstudio.baseUrl in config.json", scheme)},
		"error: malformed file ignored":                   {file: `{not json`, want: valid(defaultURL, "")},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.file != "" {
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tt.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			lookup := func(k string) (string, bool) { s, ok := tt.env[k]; return s, ok }
			if diff := gocmp.Diff(tt.want, proxyconfig.ReadStudio(lookup, dir)); diff != "" {
				t.Errorf("ReadStudio (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLlamaPort(t *testing.T) {
	tests := map[string]struct {
		raw  *string
		want config.LlamaPort
	}{
		"success: decimal":      {new("8080"), config.LlamaPort{Configured: true, Port: 8080}},
		"success: leading zero": {new("08080"), config.LlamaPort{Configured: true, Port: 8080}},
		"success: leading plus": {new("+8080"), config.LlamaPort{Configured: true, Port: 8080}},
		"success: highest port": {new("65535"), config.LlamaPort{Configured: true, Port: 65535}},
		"success: unset":        {},
		"error: empty":          {new(""), config.LlamaPort{Configured: true, Invalid: ""}},
		"error: zero":           {new("0"), config.LlamaPort{Configured: true, Invalid: "0"}},
		"error: overflow":       {new("65536"), config.LlamaPort{Configured: true, Invalid: "65536"}},
		"error: whitespace":     {new(" 8080"), config.LlamaPort{Configured: true, Invalid: " 8080"}},
		"error: not a number":   {new("llama"), config.LlamaPort{Configured: true, Invalid: "llama"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			lookup := func(k string) (string, bool) {
				if k != "CCP_LLAMA_PORT" || tt.raw == nil {
					return "", false
				}
				return *tt.raw, true
			}
			if diff := gocmp.Diff(tt.want, config.LlamaServerPort.Get(lookup)); diff != "" {
				t.Errorf("llama port (-want +got):\n%s", diff)
			}
		})
	}
}
