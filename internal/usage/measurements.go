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

package usage

import (
	"maps"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/zchee/claude-code-model-gateway/internal/config"
)

func numeric(v any) (float64, bool) { n := config.JSNumber(v); return n, n >= 0 && !math.IsInf(n, 0) }

func count(m map[string]any, k string) any {
	if v, ok := m[k]; ok {
		if n, valid := numeric(v); valid {
			return n
		}
	}
	return nil
}

func exactUsage(raw map[string]any) map[string]any {
	cache, _ := raw["cache_creation"].(map[string]any)
	details, _ := raw["output_tokens_details"].(map[string]any)
	server, _ := raw["server_tool_use"].(map[string]any)
	a := map[string]any{
		"input_tokens":             count(raw, "input_tokens"),
		"output_tokens":            count(raw, "output_tokens"),
		"cache_read_tokens":        count(raw, "cache_read_input_tokens"),
		"cache_creation_tokens":    count(raw, "cache_creation_input_tokens"),
		"cache_creation_5m_tokens": count(cache, "ephemeral_5m_input_tokens"),
		"cache_creation_1h_tokens": count(cache, "ephemeral_1h_input_tokens"),
		"thinking_tokens":          nil,
		"server_tool_use_count":    nil,
	}
	if a["cache_creation_tokens"] != nil && a["cache_creation_5m_tokens"] == nil && a["cache_creation_1h_tokens"] == nil {
		a["cache_creation_5m_tokens"] = a["cache_creation_tokens"]
		a["cache_creation_1h_tokens"] = float64(0)
	}
	for _, m := range []map[string]any{raw, details} {
		for _, key := range []string{"thinking_tokens", "reasoning_tokens"} {
			if v := count(m, key); v != nil && a["thinking_tokens"] == nil {
				a["thinking_tokens"] = v
			}
		}
	}
	var sum float64
	add := func(v any) {
		if n, ok := numeric(v); ok {
			sum += n
			a["server_tool_use_count"] = sum
		}
	}
	for _, v := range server {
		add(v)
	}
	if values, ok := raw["server_tool_use"].([]any); ok {
		for _, v := range values {
			add(v)
		}
	}
	for _, k := range []string{"web_search_requests", "web_fetch_requests", "code_execution_requests", "tool_search_requests"} {
		a[k] = count(server, k)
	}
	return a
}

// MergeUsage merges numeric usage trees, giving later observations precedence.
func MergeUsage(current, next map[string]any) map[string]any { return mergeCounts(current, next, 0) }

func mergeCounts(current, next map[string]any, depth int) map[string]any {
	result := maps.Clone(current)
	if result == nil {
		result = make(map[string]any)
	}
	for k, v := range next {
		if n, ok := numeric(v); ok {
			result[k] = n
		} else if m, ok := v.(map[string]any); ok && depth < 6 {
			old, _ := result[k].(map[string]any)
			if merged := mergeCounts(old, m, depth+1); len(merged) > 0 {
				result[k] = merged
			}
		}
	}
	return result
}

func timestamp(s string) (float64, bool) {
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.Is(unicode.Zs, r) || r == 0x2028 || r == 0x2029 || r == 0xfeff || strings.ContainsRune("\t\n\v\f\r", r)
	})
	if s == "" {
		return 0, false
	}
	if n, ok := numeric(s); ok {
		if n < 1e12 {
			n *= 1000
		}
		return n, true
	}
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, time.DateOnly, time.DateTime, "2006-01-02T15:04:05", time.RFC1123Z, time.RFC822Z} {
		if t, e := time.Parse(layout, s); e == nil {
			return float64(t.UnixMilli()), true
		}
	}
	t, e := http.ParseTime(s)
	return float64(t.UnixMilli()), e == nil
}

func header(h http.Header, key string) (string, bool) {
	if values := h.Values(key); len(values) > 0 {
		return values[0], true
	}
	if values := h[key]; len(values) > 0 {
		return values[0], true
	}
	return "", false
}

func limitEvidence(h http.Header, now time.Time) map[string]any {
	result := make(map[string]any)
	for _, group := range []string{"requests", "input-tokens", "output-tokens", "tokens"} {
		for _, field := range []string{"limit", "remaining", "reset"} {
			raw, ok := header(h, "anthropic-ratelimit-"+group+"-"+field)
			if !ok {
				continue
			}
			key := "rate_limit_" + strings.ReplaceAll(group, "-", "_") + "_" + field
			var n float64
			if field == "reset" {
				n, ok = timestamp(raw)
				key += "_at_ms"
			} else {
				n, ok = numeric(raw)
			}
			if ok {
				result[key] = n
			}
		}
	}
	if raw, ok := header(h, "retry-after"); ok {
		if n, ok := numeric(raw); ok {
			result["retry_after_ms"] = n * 1000
		} else if t, ok := timestamp(raw); ok {
			result["retry_after_ms"] = max(0, t-float64(now.UnixMilli()))
		}
	}
	for k, values := range h {
		k = strings.ToLower(k)
		if !strings.HasPrefix(k, "x-codex-") || !strings.HasSuffix(k, "-used-percent") || len(values) == 0 {
			continue
		}
		if n, ok := numeric(strings.TrimSuffix(values[0], "%")); ok {
			prev, _ := result["codex_throttle_used_percent"].(float64)
			result["codex_throttle_used_percent"] = max(prev, n)
		}
	}
	return result
}
