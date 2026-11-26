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
	"cmp"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/cases"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

func field(v jsonx.WireValue, name string) jsonx.WireValue {
	for _, m := range v.Members {
		if len(m.Name) == len(name) && slices.EqualFunc(m.Name, []byte(name), func(unit uint16, b byte) bool { return unit == uint16(b) }) {
			return m.Value
		}
	}
	return jsonx.WireValue{}
}

func text(v jsonx.WireValue, name string) string { return string(utf16.Decode(field(v, name).Text)) }

func byteSize(v jsonx.WireValue) int64 {
	raw, _ := jsonx.EncodeWire(v)
	return int64(len(raw))
}

func isResult(kind string) bool {
	return kind == "tool_result" || strings.HasSuffix(kind, "_tool_result")
}

func resultBytes(v jsonx.WireValue) int64 {
	if isResult(text(v, "type")) {
		return byteSize(v)
	}
	var n int64
	for _, item := range v.Elements {
		n += resultBytes(item)
	}
	for _, m := range v.Members {
		n += resultBytes(m.Value)
	}
	return n
}

type serverMeasure struct {
	name   string
	bytes  int64
	tokens float64
}

var serverPrefix = regexp.MustCompile(`^mcp__([^\r\n\x{2028}\x{2029}]+?)__`)

// sanitizeLabel replaces individual UTF-16 units, as a JavaScript regexp without the Unicode flag does.
func sanitizeLabel(s string, server bool) string {
	var b strings.Builder
	for _, c := range utf16.Encode([]rune(s)) {
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || !server && strings.ContainsRune(".:@-", rune(c)) {
			b.WriteByte(byte(c))
		} else if server {
			b.WriteByte('_')
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func inputComposition(body jsonx.WireValue, bodyBytes int64) map[string]int64 {
	counts, _ := measureInput(body, bodyBytes)
	return counts
}

func measureInput(body jsonx.WireValue, bodyBytes int64) (map[string]int64, []serverMeasure) {
	if bodyBytes < 0 {
		bodyBytes = byteSize(body)
	}
	messages, tools := field(body, "messages"), field(body, "tools")
	if messages.Kind != '[' {
		messages = jsonx.WireValue{Kind: '['}
	}
	counts := map[string]int64{
		"request_body_bytes":        bodyBytes,
		"input_system_bytes":        byteSize(field(body, "system")),
		"input_tools_bytes":         byteSize(tools),
		"input_messages_bytes":      byteSize(messages),
		"input_tool_results_bytes":  resultBytes(messages),
		"input_mcp_tools_bytes":     0,
		"input_native_tools_bytes":  0,
		"input_history_bytes":       0,
		"input_first_message_bytes": 0,
	}
	for i, m := range messages.Elements {
		if i == 0 {
			counts["input_first_message_bytes"] = byteSize(m)
		} else {
			counts["input_history_bytes"] += byteSize(m)
		}
	}
	var servers []serverMeasure
	positions := make(map[string]int)
	lower := cases.Lower(language.Und)
	for _, tool := range tools.Elements {
		name := text(tool, "name")
		n := byteSize(tool)
		if !strings.HasPrefix(name, "mcp__") {
			counts["input_native_tools_bytes"] += n
			continue
		}
		counts["input_mcp_tools_bytes"] += n
		if m := serverPrefix.FindStringSubmatch(name); m != nil {
			server := sanitizeLabel(lower.String(m[1]), true)
			i, ok := positions[server]
			if !ok {
				i = len(servers)
				positions[server] = i
				servers = append(servers, serverMeasure{name: server})
			}
			servers[i].bytes += n
		}
	}
	// Object.entries visits array-index property names before the other server names.
	slices.SortStableFunc(servers, func(a, b serverMeasure) int { return cmp.Compare(indexOf(a.name), indexOf(b.name)) })
	return counts, servers
}

func allocateLargestRemainder(total int64, weights []int64) []int64 {
	result := shares(float64(total), weights)
	out := make([]int64, len(result))
	for i, n := range result {
		out[i] = int64(n)
	}
	return out
}

const maxSafeInteger = 1<<53 - 1

func shares(total float64, weights []int64) []float64 {
	// Unit increments beyond the exact integer range are not meaningful. Cap finite
	// totals before distribution so an untrusted count cannot overflow an allocation.
	if !math.IsInf(total, 0) {
		total = min(total, maxSafeInteger)
	}
	total = math.Floor(total)
	out := make([]float64, len(weights))
	var sum float64
	for _, w := range weights {
		sum += float64(max(0, w))
	}
	if total <= 0 || math.IsInf(total, 0) || math.IsNaN(total) || sum == 0 {
		return out
	}
	type fraction struct {
		i int
		f float64
	}
	fractions := make([]fraction, len(weights))
	var used float64
	for i, w := range weights {
		exact := total * float64(max(0, w)) / sum
		out[i] = math.Floor(exact)
		used += out[i]
		fractions[i] = fraction{i, exact - out[i]}
	}
	slices.SortStableFunc(fractions, func(a, b fraction) int { return cmp.Compare(b.f, a.f) })
	// A floating-point total can exceed the exact-integer range. Distribute full rounds without iterating once per token.
	if left := total - used; left > 0 {
		rounds := math.Floor(left / float64(len(out)))
		extra := int(math.Mod(left, float64(len(out))))
		for rank, f := range fractions {
			out[f.i] += rounds
			if rank < extra {
				out[f.i]++
			}
		}
	}
	return out
}

func attribute(a map[string]any, c map[string]int64, servers []serverMeasure) []serverMeasure {
	var total float64
	var measured bool
	for _, key := range []string{"input_tokens", "cache_read_tokens", "cache_creation_tokens"} {
		if n, ok := a[key].(float64); ok {
			total += n
			measured = true
		}
	}
	sections := [...]string{"tools", "system", "first_message", "history"}
	weights := make([]int64, len(sections))
	for i, s := range sections {
		weights[i] = c["input_"+s+"_bytes"]
	}
	sectionTokens := shares(total, weights)
	a["context_tokens"] = nil
	if measured {
		a["context_tokens"] = total
	} else {
		for i, w := range weights {
			sectionTokens[i] = math.Ceil(float64(w) / 4)
		}
	}
	read, _ := a["cache_read_tokens"].(float64)
	read = math.Floor(read)
	for i, s := range sections {
		n := sectionTokens[i]
		cached := min(n, read)
		read -= cached
		a["input_"+s+"_tokens"] = n
		a["cache_read_"+s+"_tokens"] = cached
		a["fresh_"+s+"_tokens"] = n - cached
	}
	kinds := shares(sectionTokens[0], []int64{c["input_native_tools_bytes"], c["input_mcp_tools_bytes"]})
	a["input_native_tools_tokens"] = kinds[0]
	a["input_mcp_tools_tokens"] = kinds[1]
	weights = make([]int64, len(servers))
	for i, s := range servers {
		weights[i] = s.bytes
	}
	alloc := shares(kinds[1], weights)
	for i := range servers {
		servers[i].tokens = alloc[i]
	}
	col := collate.New(language.Und)
	slices.SortStableFunc(servers, func(a, b serverMeasure) int {
		if n := cmp.Compare(b.bytes, a.bytes); n != 0 {
			return n
		}
		return col.CompareString(a.name, b.name)
	})
	servers = servers[:min(20, len(servers))]
	for _, s := range servers {
		a["input_mcp_tools_"+s.name+"_tokens"] = s.tokens
	}
	messageTokens := sectionTokens[2] + sectionTokens[3]
	a["input_tool_results_tokens"] = min(messageTokens, math.Round(messageTokens*float64(c["input_tool_results_bytes"])/float64(c["input_messages_bytes"])))
	return servers
}
