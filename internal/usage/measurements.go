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
	json "encoding/json/v2"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/zchee/claude-code-model-gateway/internal/config"
	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

func numeric(v any) (float64, bool) { n := config.JSNumber(v); return n, n >= 0 && !math.IsInf(n, 0) }

func count(m jsonx.WireValue, k string) any {
	if n, ok := wireNumber(field(m, k)); ok {
		return n
	}
	return nil
}

func exactUsage(raw map[string]any) map[string]any { return exactWireUsage(legacyWire(raw)) }
func exactWireUsage(raw jsonx.WireValue) map[string]any {
	cache, details, server := field(raw, "cache_creation"), field(raw, "output_tokens_details"), field(raw, "server_tool_use")
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
	for _, m := range []jsonx.WireValue{raw, details} {
		for _, key := range []string{"thinking_tokens", "reasoning_tokens"} {
			if v := count(m, key); v != nil && a["thinking_tokens"] == nil {
				a["thinking_tokens"] = v
			}
		}
	}
	var sum float64
	add := func(v jsonx.WireValue) {
		if n, ok := wireNumber(v); ok {
			sum += n
			a["server_tool_use_count"] = sum
		}
	}
	for _, m := range server.Members {
		add(m.Value)
	}
	for _, el := range server.Elements {
		add(el)
	}
	for _, k := range []string{"web_search_requests", "web_fetch_requests", "code_execution_requests", "tool_search_requests"} {
		a[k] = count(server, k)
	}
	return a
}

// MergeUsage merges numeric usage maps. Use MergeWireUsage when source member order is known.
func MergeUsage(current, next map[string]any) map[string]any {
	tree := MergeWireUsage(legacyWire(current), legacyWire(next))
	raw, _ := jsonx.EncodeWire(tree)
	result := make(map[string]any)
	_ = json.Unmarshal(raw, &result)
	return result
}

func timestamp(s string) (float64, bool) {
	s = config.JSTrim(s)
	if s == "" {
		return 0, false
	}
	if n, ok := numeric(s); ok {
		if n < 1e12 {
			n *= 1000
		}
		return n, true
	}
	if len(s) > 10 && s[10] == 't' {
		s = s[:10] + "T" + s[11:]
	}
	if before, ok := strings.CutSuffix(s, "z"); ok {
		s = before + "Z"
	}
	for _, layout := range []string{time.RFC3339Nano, time.DateOnly, time.DateTime, "2006-01-02T15:04:05", time.RFC1123Z, time.RFC822Z, http.TimeFormat, "Monday, 02-Jan-06 15:04:05 GMT", time.ANSIC} {
		if t, err := time.Parse(layout, s); err == nil {
			// Date.parse places two-digit years 50..99 in the twentieth century.
			if (layout == time.RFC822Z || layout == "Monday, 02-Jan-06 15:04:05 GMT") && t.Year() >= 2050 {
				t = t.AddDate(-100, 0, 0)
			}
			return float64(t.UnixMilli()), true
		}
	}
	return 0, false
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

func limitEvidence(h http.Header, now time.Time) []recordField {
	var result []recordField
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
				result = append(result, recordField{key, n})
			}
		}
	}
	if raw, ok := header(h, "retry-after"); ok {
		if n, ok := numeric(raw); ok {
			result = append(result, recordField{"retry_after_ms", n * 1000})
		} else if t, ok := timestamp(raw); ok {
			result = append(result, recordField{"retry_after_ms", max(0, t-float64(now.UnixMilli()))})
		}
	}
	throttle := -1.0
	for k, values := range h {
		k = strings.ToLower(k)
		if strings.HasPrefix(k, "x-codex-") && strings.HasSuffix(k, "-used-percent") && len(values) > 0 {
			if n, ok := numeric(strings.TrimSuffix(values[0], "%")); ok {
				throttle = max(throttle, n)
			}
		}
	}
	if throttle >= 0 {
		result = append(result, recordField{"codex_throttle_used_percent", throttle})
	}
	return result
}
