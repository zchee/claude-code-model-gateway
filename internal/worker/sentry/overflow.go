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

package sentry

import (
	"math"
	"net/http"
	"regexp"
	"strings"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// Counts preserves whether each engine count was available, independently of overflow recognition.
type Counts struct{ Tokens, Limit *float64 }

// The upstream pattern uses ECMAScript whitespace, including no-break spaces and the BOM.
const space = `[\t\n\v\f\r\p{Zs}\x{2028}\x{2029}\x{FEFF}]`

var (
	upstreamCounts = regexp.MustCompile(`(\d+)` + space + `+tokens` + space + `*>` + space + `*(\d+)` + space + `+tokens`)
	wordedCounts   = regexp.MustCompile(`request \((\d+) tokens\) exceeds the available context size \((\d+) tokens\)`)
	legacyPhrase   = regexp.MustCompile(`context window|context length|input exceeds|prompt token count|too many tokens`)
)

// For ASCII literals, JavaScript's non-Unicode /i flag folds only ASCII letters.
func lowerASCII(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// ParseOverflow recognizes genuine and legacy error responses and reports their upstream token counts.
func ParseOverflow(status int, body []byte) (Counts, bool) {
	if status < http.StatusBadRequest {
		return Counts{}, false
	}
	v, _ := jsonx.DecodeWire(body)
	e := member(v, "error")
	if status == http.StatusRequestEntityTooLarge {
		message := member(e, "message")
		if text(member(e, "type")) != "request_too_large" || message.Kind != '"' {
			return Counts{}, false
		}
		m := upstreamCounts.FindStringSubmatch(lowerASCII(text(message)))
		if m == nil {
			return Counts{}, true
		}
		return Counts{Tokens: new(config.JSNumber(m[1])), Limit: new(config.JSNumber(m[2]))}, true
	}
	if c, ok := EngineOverflow(e); ok {
		return c, true
	}
	return Counts{}, legacyPhrase.MatchString(lowerASCII(string(body)))
}

// EngineOverflow recognizes an engine error object from a buffered reply or a first stream event.
// The marker suffices even when neither engine count is usable.
func EngineOverflow(e jsonx.WireValue) (Counts, bool) {
	const marker = "exceed_context_size_error"
	if text(member(e, "type")) != marker {
		message := text(member(e, "message"))
		if !strings.Contains(message, marker) {
			return Counts{}, false
		}
		if i := strings.IndexByte(message, '{'); i >= 0 {
			v, _ := jsonx.DecodeWire([]byte(message[i:]))
			e = member(v, "error")
		}
		if text(member(e, "type")) != marker {
			m := wordedCounts.FindStringSubmatch(message)
			if m == nil {
				return Counts{}, true
			}
			return Counts{safeCount(config.JSNumber(m[1])), safeCount(config.JSNumber(m[2]))}, true
		}
	}
	// Only numeric wire values populate Number: no object or string is coerced.
	return Counts{safeCount(member(e, "n_prompt_tokens").Number), safeCount(member(e, "n_ctx").Number)}, true
}

func safeCount(n float64) *float64 {
	if n > 0 && n < 1<<53 && n == math.Trunc(n) {
		return &n
	}
	return nil
}
