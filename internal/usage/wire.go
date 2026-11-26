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
	json "encoding/json/v2"
	"maps"
	"math"
	"slices"
	"strconv"
	"unicode/utf16"

	"github.com/zchee/claude-code-model-gateway/internal/jsonx"
)

// A map supplied by an older caller has already discarded insertion order. Sort
// its non-index names deterministically. Response capture should supply WireUsage.
func legacyWire(v any) jsonx.WireValue {
	switch x := v.(type) {
	case map[string]any:
		result := jsonx.WireValue{Kind: '{'}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			result.Members = append(result.Members, jsonx.Member{Name: utf16.Encode([]rune(k)), Value: legacyWire(x[k])})
		}
		sortIndices(result.Members)
		return result
	case []any:
		result := jsonx.WireValue{Kind: '['}
		for _, item := range x {
			result.Elements = append(result.Elements, legacyWire(item))
		}
		return result
	case float64:
		return jsonx.WireValue{Kind: '0', Number: x}
	default:
		data, err := json.Marshal(x)
		if err != nil {
			return jsonx.WireValue{}
		}
		value, _ := jsonx.DecodeWire(data)
		return value
	}
}

func indexOf(s string) uint64 {
	n, e := strconv.ParseUint(s, 10, 32)
	if e != nil || strconv.FormatUint(n, 10) != s {
		return math.MaxUint32
	}
	return n
}

func sortIndices(members []jsonx.Member) {
	slices.SortStableFunc(members, func(a, b jsonx.Member) int {
		return cmp.Compare(indexOf(string(utf16.Decode(a.Name))), indexOf(string(utf16.Decode(b.Name))))
	})
}

func wireNumber(v jsonx.WireValue) (float64, bool) {
	switch v.Kind {
	case '0':
		return numeric(v.Number)
	case 'n', 'f':
		return 0, true
	case 't':
		return 1, true
	case '"':
		return numeric(string(utf16.Decode(v.Text)))
	case '[':
		switch len(v.Elements) {
		case 0:
			return 0, true
		case 1:
			item := v.Elements[0]
			if item.Kind != 't' && item.Kind != 'f' {
				return wireNumber(item)
			}
		}
	}
	return 0, false
}

var toStringName = utf16.Encode([]rune("toString"))

// numericConversionFails reports whether the source's numeric conversion throws while merging
// usage, which drops the whole JSON observation there. The source converts every member value
// of usage and of its nested objects down to the merge's depth limit, including a "__proto__"
// member that the merge then skips; usage itself is not converted.
func numericConversionFails(usage jsonx.WireValue) bool { return treeConversionFails(usage, 0) }

func treeConversionFails(v jsonx.WireValue, depth int) bool {
	if depth > 6 || v.Kind != '{' {
		return false
	}
	for _, m := range v.Members {
		if conversionFails(m.Value) || m.Value.Kind == '{' && treeConversionFails(m.Value, depth+1) {
			return true
		}
	}
	return false
}

// conversionFails reports whether converting v to a number throws in the source. An object with
// an own member named "toString" has no callable conversion method, because its inherited
// valueOf returns the object itself; an array converts each element to a string first, at any
// depth, and an object element converts the same way.
func conversionFails(v jsonx.WireValue) bool {
	switch v.Kind {
	case '{':
		return slices.ContainsFunc(v.Members, func(m jsonx.Member) bool { return slices.Equal(m.Name, toStringName) })
	case '[':
		return slices.ContainsFunc(v.Elements, conversionFails)
	}
	return false
}

// MergeWireUsage combines numeric observations without discarding the source's
// object-property order. The inputs are not modified, and the result shares no storage
// with next: changing next afterwards, including a member name, does not change it.
func MergeWireUsage(current, next jsonx.WireValue) jsonx.WireValue {
	return mergeWire(current, next, 0)
}

func mergeWire(current, next jsonx.WireValue, depth int) jsonx.WireValue {
	result := jsonx.WireValue{Kind: '{', Members: slices.Clone(current.Members)}
	if next.Kind != '{' {
		return result
	}
	positions := make(map[string]int, len(result.Members))
	keyOf := func(units []uint16) string {
		raw, _ := jsonx.EncodeWire(jsonx.WireValue{Kind: '"', Text: units})
		return string(raw)
	}
	for i, m := range result.Members {
		positions[keyOf(m.Name)] = i
	}
	for _, m := range next.Members {
		key := keyOf(m.Name)
		if key == `"__proto__"` { // The source assignment creates no own property; never copy a prototype.
			continue
		}
		i, exists := positions[key]
		if n, ok := wireNumber(m.Value); ok {
			m.Value = jsonx.WireValue{Kind: '0', Number: n}
		} else if m.Value.Kind == '{' && depth < 6 {
			var prior jsonx.WireValue
			if exists {
				prior = result.Members[i].Value
			}
			m.Value = mergeWire(prior, m.Value, depth+1)
			if len(m.Value.Members) == 0 {
				continue
			}
		} else {
			continue
		}
		m.Name = slices.Clone(m.Name)
		if exists {
			result.Members[i] = m
		} else {
			positions[key] = len(result.Members)
			result.Members = append(result.Members, m)
		}
	}
	sortIndices(result.Members)
	return result
}
