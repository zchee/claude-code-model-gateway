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

// Package compactguard buffers proxy compaction SSE streams so incomplete
// generations can be retried while the client remains writable.
package compactguard

import (
	"math"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/worker/sentry"
)

const (
	defaultRetries          = 2
	defaultRetryDelayMillis = 250
	defaultMaxBufferedBytes = 16 * 1024 * 1024

	// messagesPath is the only request path whose compaction turns are guarded.
	messagesPath = "/v1/messages"
)

// Settings holds the compaction stream guard configuration loaded from an injected environment.
type Settings struct {
	StreamGuard      bool    `json:"streamGuard"`
	Retries          float64 `json:"retries"`
	MaxBufferedBytes float64 `json:"maxBufferedBytes"`
	RetryDelayMillis float64 `json:"-"`
}

// LoadSettings reads the compaction guard settings from lookup.
//
// The guard is disabled only by the exact value "0". Each numeric setting is converted with
// JavaScript's Number semantics (an unset variable is NaN, an empty one is zero) and keeps its
// default unless it is valid: retries must be a non-negative integer, the retry delay a finite
// non-negative number, and the buffer ceiling a finite positive number. Valid values are kept
// as numbers, not narrowed to machine counters or durations.
func LoadSettings(lookup config.LookupEnv) Settings {
	guard, ok := lookup("CODEX_GATEWAY_COMPACT_STREAM_GUARD")
	s := Settings{
		StreamGuard:      !ok || guard != "0",
		Retries:          defaultRetries,
		MaxBufferedBytes: defaultMaxBufferedBytes,
		RetryDelayMillis: defaultRetryDelayMillis,
	}
	// Adding zero turns a negative zero into the zero JSON.stringify prints.
	if n := envNumber(lookup, "CODEX_GATEWAY_COMPACT_STREAM_RETRIES"); finite(n) && n == math.Trunc(n) && n >= 0 {
		s.Retries = n + 0
	}
	if n := envNumber(lookup, "CODEX_GATEWAY_COMPACT_STREAM_RETRY_DELAY_MS"); finite(n) && n >= 0 {
		s.RetryDelayMillis = n + 0
	}
	if n := envNumber(lookup, "CODEX_GATEWAY_COMPACT_STREAM_MAX_BYTES"); finite(n) && n > 0 {
		s.MaxBufferedBytes = n
	}
	return s
}

// envNumber is Number(process.env[key]): NaN when the variable is unset.
func envNumber(lookup config.LookupEnv, key string) float64 {
	v, ok := lookup(key)
	if !ok {
		return math.NaN()
	}
	return config.JSNumber(v)
}

func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }

// IsCompactionRequest reports whether body is a streaming JSON request whose system prompt contains the compaction prompt fragment.
func IsCompactionRequest(body []byte) bool {
	return sentry.RequestPhase(body) == "compaction"
}

// ShouldGuard reports whether a request should be buffered and retried by the compaction stream guard.
// Only proxy traffic is guarded: an empty provider is native Anthropic passthrough.
func ShouldGuard(settings Settings, proxyProvider, pathOnly string, body []byte) bool {
	return settings.StreamGuard && proxyProvider != "" && pathOnly == messagesPath && IsCompactionRequest(body)
}
