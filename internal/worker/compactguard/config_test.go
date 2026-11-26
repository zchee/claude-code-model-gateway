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

package compactguard_test

import (
	json "encoding/json/v2"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/worker/compactguard"
)

func mapLookup(values map[string]string) config.LookupEnv {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestLoadSettings(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		env      map[string]string
		expected compactguard.Settings
		wantJSON string
	}{
		"success: defaults": {
			env: nil,
			expected: compactguard.Settings{
				StreamGuard:      true,
				Retries:          2,
				MaxBufferedBytes: 16 * 1024 * 1024,
				RetryDelayMillis: 250,
			},
			wantJSON: `{"streamGuard":true,"retries":2,"maxBufferedBytes":16777216}`,
		},
		"success: guard disabled on exact zero": {
			env: map[string]string{
				"CODEX_GATEWAY_COMPACT_STREAM_GUARD": "0",
			},
			expected: compactguard.Settings{
				StreamGuard:      false,
				Retries:          2,
				MaxBufferedBytes: 16 * 1024 * 1024,
				RetryDelayMillis: 250,
			},
			wantJSON: `{"streamGuard":false,"retries":2,"maxBufferedBytes":16777216}`,
		},
		"success: non-zero guard string stays enabled": {
			env: map[string]string{
				"CODEX_GATEWAY_COMPACT_STREAM_GUARD": "false",
			},
			expected: compactguard.Settings{
				StreamGuard:      true,
				Retries:          2,
				MaxBufferedBytes: 16 * 1024 * 1024,
				RetryDelayMillis: 250,
			},
			wantJSON: `{"streamGuard":true,"retries":2,"maxBufferedBytes":16777216}`,
		},
		"success: explicit retries zero and zero delay": {
			env: map[string]string{
				"CODEX_GATEWAY_COMPACT_STREAM_RETRIES":        "0",
				"CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS": "0",
				"CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES":      "4096",
			},
			expected: compactguard.Settings{
				StreamGuard:      true,
				Retries:          0,
				MaxBufferedBytes: 4096,
				RetryDelayMillis: 0,
			},
			wantJSON: `{"streamGuard":true,"retries":0,"maxBufferedBytes":4096}`,
		},
		"success: empty numeric strings coerce to zero": {
			env: map[string]string{
				"CODEX_GATEWAY_COMPACT_STREAM_RETRIES":        "",
				"CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS": "",
				"CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES":      "",
			},
			expected: compactguard.Settings{
				StreamGuard:      true,
				Retries:          0,
				MaxBufferedBytes: 16 * 1024 * 1024,
				RetryDelayMillis: 0,
			},
			wantJSON: `{"streamGuard":true,"retries":0,"maxBufferedBytes":16777216}`,
		},
		"success: explicit retries four": {
			env: map[string]string{
				"CODEX_GATEWAY_COMPACT_STREAM_RETRIES":        "4",
				"CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS": "12.5",
			},
			expected: compactguard.Settings{
				StreamGuard:      true,
				Retries:          4,
				MaxBufferedBytes: 16 * 1024 * 1024,
				RetryDelayMillis: 12.5,
			},
			wantJSON: `{"streamGuard":true,"retries":4,"maxBufferedBytes":16777216}`,
		},
		"success: numeric configuration can exceed machine counters and timer range": {
			env:      map[string]string{"CODEX_GATEWAY_COMPACT_STREAM_RETRIES": "1e30", "CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS": "1e100", "CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES": "4096.5"},
			expected: compactguard.Settings{StreamGuard: true, Retries: 1e30, RetryDelayMillis: 1e100, MaxBufferedBytes: 4096.5},
			wantJSON: `{"streamGuard":true,"retries":1e+30,"maxBufferedBytes":4096.5}`,
		},
		"error: invalid numeric overrides fall back to defaults": {
			env: map[string]string{
				"CODEX_GATEWAY_COMPACT_STREAM_RETRIES":        "1.5",
				"CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS": "-1",
				"CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES":      "0",
			},
			expected: compactguard.Settings{
				StreamGuard:      true,
				Retries:          2,
				MaxBufferedBytes: 16 * 1024 * 1024,
				RetryDelayMillis: 250,
			},
			wantJSON: `{"streamGuard":true,"retries":2,"maxBufferedBytes":16777216}`,
		},
		"error: negative retries and non-finite max bytes fall back to defaults": {
			env: map[string]string{
				"CODEX_GATEWAY_COMPACT_STREAM_RETRIES":        "-2",
				"CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS": "Infinity",
				"CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES":      "Infinity",
			},
			expected: compactguard.Settings{
				StreamGuard:      true,
				Retries:          2,
				MaxBufferedBytes: 16 * 1024 * 1024,
				RetryDelayMillis: 250,
			},
			wantJSON: `{"streamGuard":true,"retries":2,"maxBufferedBytes":16777216}`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := compactguard.LoadSettings(mapLookup(tt.env))
			if diff := gocmp.Diff(tt.expected, got); diff != "" {
				t.Fatalf("LoadSettings mismatch (-want +got):\n%s", diff)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal(Settings): %v", err)
			}
			if diff := gocmp.Diff(tt.wantJSON, string(encoded)); diff != "" {
				t.Fatalf("healthz compaction JSON mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestIsCompactionRequest(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body     string
		expected bool
	}{
		"success: string system prompt with stream true": {
			body:     `{"stream":true,"system":"You are a helpful AI assistant tasked with summarizing conversations."}`,
			expected: true,
		},
		"success: array system prompt with stream true": {
			body:     `{"stream":true,"system":[{"type":"text","text":"You are a helpful AI assistant tasked with summarizing conversations."}]}`,
			expected: true,
		},
		"success: embedded prompt in later text block": {
			body:     `{"stream":true,"system":[null,{"type":"text","text":123},{"type":"text","text":"Prefix. You are a helpful AI assistant tasked with summarizing conversations. Suffix."}]}`,
			expected: true,
		},
		"success: duplicate stream keys last true wins": {
			body:     `{"stream":false,"stream":true,"system":"You are a helpful AI assistant tasked with summarizing conversations."}`,
			expected: true,
		},
		"error: duplicate stream keys last false wins": {
			body:     `{"stream":true,"stream":false,"system":"You are a helpful AI assistant tasked with summarizing conversations."}`,
			expected: false,
		},
		"error: stream false": {
			body:     `{"stream":false,"system":"You are a helpful AI assistant tasked with summarizing conversations."}`,
			expected: false,
		},
		"error: stream missing": {
			body:     `{"system":"You are a helpful AI assistant tasked with summarizing conversations."}`,
			expected: false,
		},
		"error: stream string true is not boolean true": {
			body:     `{"stream":"true","system":"You are a helpful AI assistant tasked with summarizing conversations."}`,
			expected: false,
		},
		"error: unrelated system prompt": {
			body:     `{"stream":true,"system":[{"type":"text","text":"You are Claude Code."}]}`,
			expected: false,
		},
		"error: object system prompt": {
			body:     `{"stream":true,"system":{"text":"You are a helpful AI assistant tasked with summarizing conversations."}}`,
			expected: false,
		},
		"error: malformed JSON": {
			body:     `{"stream":true`,
			expected: false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := compactguard.IsCompactionRequest([]byte(tt.body))
			if diff := gocmp.Diff(tt.expected, got); diff != "" {
				t.Fatalf("IsCompactionRequest mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestShouldGuard(t *testing.T) {
	t.Parallel()

	on := compactguard.Settings{StreamGuard: true, Retries: 2, MaxBufferedBytes: 16 * 1024 * 1024}
	off := compactguard.Settings{StreamGuard: false, Retries: 2, MaxBufferedBytes: 16 * 1024 * 1024}
	compact := []byte(`{"stream":true,"system":[{"type":"text","text":"You are a helpful AI assistant tasked with summarizing conversations."}]}`)
	normal := []byte(`{"stream":true,"system":[{"type":"text","text":"You are Claude Code."}]}`)

	tests := map[string]struct {
		settings      compactguard.Settings
		proxyProvider string
		pathOnly      string
		body          []byte
		expected      bool
	}{
		"success: proxy compaction on messages path with guard enabled": {
			settings:      on,
			proxyProvider: "codex",
			pathOnly:      "/v1/messages",
			body:          compact,
			expected:      true,
		},
		"success: lmstudio compaction on messages path with guard enabled": {
			settings:      on,
			proxyProvider: "lmstudio",
			pathOnly:      "/v1/messages",
			body:          compact,
			expected:      true,
		},
		"error: native Anthropic empty provider is never guarded": {
			settings:      on,
			proxyProvider: "",
			pathOnly:      "/v1/messages",
			body:          compact,
			expected:      false,
		},
		"error: guard disabled": {
			settings:      off,
			proxyProvider: "codex",
			pathOnly:      "/v1/messages",
			body:          compact,
			expected:      false,
		},
		"error: non-compaction turn": {
			settings:      on,
			proxyProvider: "codex",
			pathOnly:      "/v1/messages",
			body:          normal,
			expected:      false,
		},
		"error: count_tokens subpath is not guarded": {
			settings:      on,
			proxyProvider: "codex",
			pathOnly:      "/v1/messages/count_tokens",
			body:          compact,
			expected:      false,
		},
		"error: path with unstripped query string is not exact messages path": {
			settings:      on,
			proxyProvider: "codex",
			pathOnly:      "/v1/messages?beta=true",
			body:          compact,
			expected:      false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := compactguard.ShouldGuard(tt.settings, tt.proxyProvider, tt.pathOnly, tt.body)
			if diff := gocmp.Diff(tt.expected, got); diff != "" {
				t.Fatalf("ShouldGuard mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
