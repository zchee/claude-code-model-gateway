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

import "github.com/zchee/claude-code-model-gateway/internal/config"

// Settings holds the compaction stream guard configuration loaded from an injected environment.
type Settings struct {
	StreamGuard      bool    `json:"streamGuard"`
	Retries          float64 `json:"retries"`
	MaxBufferedBytes float64 `json:"maxBufferedBytes"`
	RetryDelayMillis float64 `json:"-"`
}

// LoadSettings reads the compaction guard settings from lookup.
func LoadSettings(lookup config.LookupEnv) Settings {
	return Settings{}
}

// IsCompactionRequest reports whether body is a streaming JSON request whose system prompt contains the compaction prompt fragment.
func IsCompactionRequest(body []byte) bool {
	return false
}

// ShouldGuard reports whether a request should be buffered and retried by the compaction stream guard.
func ShouldGuard(settings Settings, proxyProvider, pathOnly string, body []byte) bool {
	return false
}
